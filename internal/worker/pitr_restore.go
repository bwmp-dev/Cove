package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

func pitrCopyDataDirectory(major int) string {
	if major >= 18 {
		return "/var/lib/postgresql"
	}
	return "/var/lib/postgresql/data"
}

func (e *Engine) PlanPITRCopy(ctx context.Context, r Request) (*RestoreCopy, error) {
	if r.PITRRestore == nil || !workloadIdentifier.MatchString(r.Workload) || !workloadIdentifier.MatchString(r.PITRRestore.Source) || r.Workload == r.PITRRestore.Source {
		return nil, errors.New("invalid PITR copy source")
	}
	state, err := e.loadPITR(r.PITRRestore.Source)
	if err != nil {
		return nil, err
	}
	status, err := e.PITRStatus(ctx, r.PITRRestore.Source)
	if err != nil {
		return nil, err
	}
	valid := false
	for _, base := range status.Bases {
		if base.ID == r.PITRRestore.Base && base.AvailableTo != nil && !r.PITRRestore.Timestamp.Before(base.CreatedAt) && !r.PITRRestore.Timestamp.After(*base.AvailableTo) {
			valid = true
		}
	}
	if !valid {
		return nil, errors.New("select a retained PITR base and a timestamp inside its observed continuous recovery window")
	}
	volume := "cove-pitr-copy-" + r.Workload
	dataDirectory := pitrCopyDataDirectory(state.Config.Database.Major)
	config := map[string]any{
		"services": map[string]any{"postgres": map[string]any{"image": state.Source.Image, "restart": "unless-stopped", "environment": map[string]string{"POSTGRES_USER": "${COVE_PITR_USER}", "POSTGRES_PASSWORD": "${COVE_PITR_PASSWORD}", "PGDATA": dataDirectory}, "volumes": []string{volume + ":" + dataDirectory}}},
		"volumes":  map[string]any{volume: map[string]any{"external": true, "name": volume}},
		"networks": map[string]any{"default": map[string]any{"internal": true}},
	}
	data, err := json.Marshal(config)
	if err != nil {
		return nil, err
	}
	return &RestoreCopy{Source: r.PITRRestore.Source, Compose: string(data), Environment: map[string]string{"COVE_PITR_USER": state.Source.Database.User, "COVE_PITR_PASSWORD": r.Environment["COVE_PITR_PASSWORD"]}, Volumes: []string{volume}}, nil
}

func (e *Engine) pitrOwnedVolume(ctx context.Context, name, owner string) error {
	exists, err := e.pitrVolumeExists(ctx, name)
	if err != nil {
		return err
	}
	if exists {
		actual, err := e.output(ctx, "docker", "volume", "inspect", "--format", `{{index .Labels "dev.cove.restore"}}`, name)
		if err != nil {
			return err
		}
		if actual != owner {
			return errors.New("refusing existing unowned PITR recovery volume")
		}
		return nil
	}
	return e.Run(ctx, e.Root, []string{"docker", "volume", "create", "--label", "dev.cove.restore=" + owner, "--label", "com.docker.compose.project=cove-" + owner, name}, nil, io.Discard)
}

func (e *Engine) performPITRCopy(ctx context.Context, r Request, out io.Writer) (result error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	e.backupMu.Lock()
	defer e.backupMu.Unlock()
	if err := e.recoverPITRCopies(ctx); err != nil {
		return err
	}
	sourceID := r.PITRRestore.Source
	e.mu.Lock()
	if e.busy(sourceID) {
		e.mu.Unlock()
		return errors.New("source resource has an active operation")
	}
	if e.active == nil {
		e.active = map[string]bool{}
	}
	e.active[sourceID] = true
	e.mu.Unlock()
	defer func() { e.mu.Lock(); delete(e.active, sourceID); e.mu.Unlock() }()
	unlock, err := lockPITRArchive(ctx, e.pitrDirectory(sourceID))
	if err != nil {
		return errors.New("PITR base backup or retention is active; retry the copy")
	}
	defer unlock()
	plan, err := e.PlanPITRCopy(ctx, r)
	if err != nil {
		return err
	}
	if plan.Compose != r.Compose {
		return errors.New("PITR recovery plan changed; create a new copy")
	}
	state, err := e.loadPITR(sourceID)
	if err != nil {
		return err
	}
	dataDirectory := pitrCopyDataDirectory(state.Config.Database.Major)
	dir := filepath.Join(e.Root, "workloads", r.Workload)
	if _, err = os.Lstat(filepath.Join(dir, "current.json")); err == nil {
		return errors.New("PITR requires a new undeployed resource")
	} else if !os.IsNotExist(err) {
		return err
	}
	containers, err := e.Containers(ctx, r.Workload)
	if err != nil || len(containers) != 0 {
		return errors.New("PITR target must have no containers")
	}
	marker := filepath.Join(dir, "copy-recovery.json")
	if err = writeJSON(marker, map[string]string{"operation": r.ID, "source": sourceID}); err != nil {
		return err
	}
	volume, wals, verify := plan.Volumes[0], "cove-pitr-wal-"+r.ID, "cove-pitr-verify-"+r.ID
	if err = os.MkdirAll(filepath.Join(e.Root, "pitr-recovery"), 0700); err != nil {
		return err
	}
	if err = writeJSON(filepath.Join(e.Root, "pitr-recovery", r.ID+".json"), pitrCopyRecovery{Workload: r.Workload, Owner: state.Config.Owner}); err != nil {
		return err
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
		defer cancel()
		result = errors.Join(result, e.cleanupPITRCopy(cleanup, r.ID))
		if result == nil {
			result = os.Remove(marker)
		} else {
			fmt.Fprintln(out, "PITR copy failed. Source storage is unchanged; the new resource is blocked and its data volume is retained for inspection.")
		}
	}()
	for _, name := range []string{volume, wals, verify} {
		if err = e.pitrOwnedVolume(ctx, name, r.Workload); err != nil {
			return err
		}
	}
	image, err := e.preparePITRTools(ctx)
	if err != nil {
		return err
	}
	binary, err := e.prepareSnapshotHostBinary()
	if err != nil {
		return err
	}
	helper, replay := "cove-pitr-fetch-"+r.ID, "cove-pitr-replay-"+r.ID
	args := []string{"docker", "run", "--name", helper, "--label", "dev.cove.pitr.owner=" + state.Config.Owner, "--read-only", "--tmpfs", "/tmp:rw,noexec,nosuid,size=128m", "--tmpfs", "/run/cove:rw,exec,nosuid,size=64m,mode=700", "--memory", "1g", "--cpus", "1", "--pids-limit", "128", "--log-driver", "none", "-i", "--mount", "type=bind,source=" + e.pitrDirectory(sourceID) + ",target=/state,readonly", "--mount", "type=bind,source=" + binary + ",target=/worker,readonly", "--volume", volume + ":/restore", "--volume", wals + ":/wals"}
	if state.Config.Storage == nil {
		args = append(args, "--volume", "cove-pitr-archive-"+sourceID+":/archive")
	}
	args = append(args, "--entrypoint", "/bin/sh", image, "-ec", "cp /worker /run/cove/worker; chmod 700 /run/cove/worker; exec /run/cove/worker pitr-fetch")
	input, err := json.Marshal(r.PITRRestore)
	if err != nil {
		return err
	}
	if err = runInput(ctx, e.Root, args, nil, strings.NewReader(string(input)), io.Discard); err != nil {
		return errors.New("PITR encrypted data could not be fetched or verified; source was not changed")
	}
	setup := `set -eu
test -f /restore/PG_VERSION
test ! -L /restore/postgresql.auto.conf
printf '\n%s\n' "unix_socket_directories='/var/run/postgresql'" "restore_command='cp /recovery-wal/%f %p && chmod 600 %p'" "recovery_target=''" "recovery_target_name=''" "recovery_target_lsn=''" "recovery_target_xid=''" "recovery_target_time='$1'" "recovery_target_inclusive='on'" "recovery_target_action='promote'" "max_slot_wal_keep_size='512MB'" >> /restore/postgresql.auto.conf
touch /restore/recovery.signal
chown -R "$2:$3" /restore
chmod 700 /restore
chown "$2:$3" /verify
chmod 700 /verify`
	if err = runInput(ctx, e.Root, []string{"docker", "run", "--rm", "--network", "none", "--volume", volume + ":/restore", "--volume", verify + ":/verify", "busybox:1.37", "sh", "-ec", setup, "cove", r.PITRRestore.Timestamp.UTC().Format("2006-01-02 15:04:05.999999999-07:00"), state.Source.UID, state.Source.GID}, nil, nil, io.Discard); err != nil {
		return errors.New("PITR isolated recovery configuration failed")
	}
	args = []string{"docker", "run", "-d", "--name", replay, "--label", "dev.cove.pitr.owner=" + state.Config.Owner, "--network", "none", "--memory", "2g", "--cpus", "1", "--pids-limit", "256", "--log-driver", "none", "-e", "PGDATA=" + dataDirectory, "--volume", volume + ":" + dataDirectory, "--volume", wals + ":/recovery-wal:ro", state.Source.Image}
	if err = e.Run(ctx, e.Root, args, nil, io.Discard); err != nil {
		return errors.New("PITR isolated PostgreSQL replay could not start")
	}
	ready := false
	deadline := time.Now().Add(5 * time.Minute)
	for time.Now().Before(deadline) {
		state, inspectErr := e.output(ctx, "docker", "inspect", "--format", "{{.State.Status}}", replay)
		if inspectErr != nil || state != "running" {
			return errors.New("PITR isolated replay stopped before reaching the requested timestamp")
		}
		err = e.Run(ctx, e.Root, []string{"docker", "exec", replay, "sh", "-ec", `pg_isready -h /var/run/postgresql -p 5432 >/dev/null && test ! -f "$1/recovery.signal"`, "cove", dataDirectory}, nil, io.Discard)
		if err == nil {
			ready = true
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
	if !ready {
		return errors.New("PITR timestamp was not reached and promoted; the new copy remains blocked")
	}
	if err = e.Run(ctx, e.Root, []string{"docker", "stop", "--time", "30", replay}, nil, io.Discard); err != nil {
		return err
	}
	if err = e.Run(ctx, e.Root, []string{"docker", "rm", replay}, nil, io.Discard); err != nil {
		return err
	}
	user := plan.Environment["COVE_PITR_USER"]
	password := plan.Environment["COVE_PITR_PASSWORD"]
	if len(password) < 32 || len(password) > 256 {
		return errors.New("PITR copy requires new protected administrator credentials")
	}
	role := pgx.Identifier{user}.Sanitize()
	body := "BEGIN IF pg_is_in_recovery() THEN RAISE EXCEPTION 'Recovery unfinished'; END IF; IF NOT EXISTS(SELECT 1 FROM pg_roles WHERE rolname=" + pitrSQLLiteral(user) + ") THEN CREATE ROLE " + role + "; END IF; END;"
	sql := "SET password_encryption='scram-sha-256';\nDO " + pitrSQLLiteral(body) + ";\nALTER ROLE " + role + " WITH LOGIN SUPERUSER PASSWORD " + pitrSQLLiteral(password) + ";\nCOPY (SELECT json_build_object('recovered',NOT pg_is_in_recovery(),'replay_at',pg_last_xact_replay_timestamp())) TO '/pitr-verify/result.json';\n"
	args = []string{"docker", "run", "--rm", "-i", "--network", "none", "--user", state.Source.UID + ":" + state.Source.GID, "--volume", volume + ":" + dataDirectory, "--volume", verify + ":/pitr-verify", "--entrypoint", "postgres", state.Source.Image, "--single", "-D", dataDirectory, "-c", "log_statement=none", "-c", "log_min_error_statement=panic", "postgres"}
	if err = runInput(ctx, e.Root, args, nil, strings.NewReader(sql), io.Discard); err != nil {
		return errors.New("PITR recovery verification and administrator bootstrap failed")
	}
	data, err := e.output(ctx, "docker", "run", "--rm", "--network", "none", "--volume", verify+":/verify:ro", "busybox:1.37", "cat", "/verify/result.json")
	if err != nil {
		return errors.New("PITR recovery receipt unavailable")
	}
	var receipt struct {
		Recovered bool       `json:"recovered"`
		ReplayAt  *time.Time `json:"replay_at"`
	}
	if json.Unmarshal([]byte(data), &receipt) != nil || !receipt.Recovered || receipt.ReplayAt != nil && receipt.ReplayAt.After(r.PITRRestore.Timestamp) {
		return errors.New("PITR recovery receipt did not verify the requested timestamp")
	}
	clear := `set -eu
printf '\n%s\n' "restore_command=''" "recovery_target_time=''" "unix_socket_directories='/var/run/postgresql'" >> /restore/postgresql.auto.conf
rm -f /restore/recovery.signal /restore/standby.signal
if [ -d /restore/.cove-pitr-socket ]; then
 test ! -L /restore/.cove-pitr-socket
 test -f /restore/.cove-pitr-socket/owner && test ! -L /restore/.cove-pitr-socket/owner
 test "$(cat /restore/.cove-pitr-socket/owner)" = "$1"
 rm /restore/.cove-pitr-socket/owner
 if [ -e /restore/.cove-pitr-socket/.s.PGSQL.5432.lock ]; then
  test -f /restore/.cove-pitr-socket/.s.PGSQL.5432.lock && test ! -L /restore/.cove-pitr-socket/.s.PGSQL.5432.lock
  rm /restore/.cove-pitr-socket/.s.PGSQL.5432.lock
 fi
 rmdir /restore/.cove-pitr-socket
fi`
	if err = runInput(ctx, e.Root, []string{"docker", "run", "--rm", "--network", "none", "--volume", volume + ":/restore", "busybox:1.37", "sh", "-ec", clear, "cove", pitrArchiveOwner(state.Config)}, nil, nil, io.Discard); err != nil {
		return errors.New("PITR recovery settings cleanup failed")
	}
	args = []string{"docker", "run", "-d", "--name", replay, "--label", "dev.cove.pitr.owner=" + state.Config.Owner, "--network", "none", "--memory", "2g", "--cpus", "1", "--pids-limit", "256", "--log-driver", "none", "-e", "PGDATA=" + dataDirectory, "--volume", volume + ":" + dataDirectory, state.Source.Image}
	if err = e.Run(ctx, e.Root, args, nil, io.Discard); err != nil {
		return errors.New("PITR copy could not start after recovery settings were removed")
	}
	verified := false
	deadline = time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		output := &limitedBuffer{Limit: 4096}
		query := "SELECT NOT pg_is_in_recovery() AND (SELECT system_identifier::text FROM pg_control_system())=" + pitrSQLLiteral(state.Source.Database.SystemID) + ";"
		args = []string{"docker", "exec", "-i", "-e", "PGPASSWORD", replay, "psql", "-X", "-h", "/var/run/postgresql", "-p", "5432", "-U", user, "-d", "postgres", "-At", "-v", "ON_ERROR_STOP=1"}
		if err = runInput(ctx, e.Root, args, []string{"PGPASSWORD=" + password}, strings.NewReader(query+"\n"), output); err == nil && strings.TrimSpace(output.data.String()) == "t" {
			verified = true
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
	if !verified {
		return errors.New("PITR copy did not pass a normal administrator login with its unchanged authentication rules")
	}
	if err = e.Run(ctx, e.Root, []string{"docker", "stop", "--time", "30", replay}, nil, io.Discard); err != nil {
		return err
	}
	if err = e.Run(ctx, e.Root, []string{"docker", "rm", replay}, nil, io.Discard); err != nil {
		return err
	}
	holder := state
	holder.Config.Workload = r.Workload
	if err = e.holdPITRImage(ctx, holder); err != nil {
		return err
	}
	if err = writeJSON(filepath.Join(dir, "pitr-image.json"), map[string]string{"operation": r.ID, "image": state.Source.Image}); err != nil {
		return err
	}
	fmt.Fprintln(out, "Timestamp verified in separate storage. Source was unchanged. The new PostgreSQL resource remains offline, with no routes or published ports and new protected administrator credentials; review it before deploying.")
	return nil
}

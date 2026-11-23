package worker

import (
	"context"
	"errors"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
)

type pitrState struct {
	Config      pitrRunnerConfig  `json:"config"`
	Source      pitrSource        `json:"source"`
	Operation   string            `json:"operation"`
	Configuring bool              `json:"configuring"`
	Bases       map[string]string `json:"bases"`
}

func (e *Engine) pitrDirectory(workload string) string {
	return filepath.Join(e.Root, "pitr", workload)
}

func (e *Engine) loadPITR(workload string) (pitrState, error) {
	var state pitrState
	if !workloadIdentifier.MatchString(workload) {
		return state, errors.New("invalid PITR resource")
	}
	err := readSnapshotMetadata(filepath.Join(e.pitrDirectory(workload), "state.json"), &state)
	if err != nil {
		return state, err
	}
	if state.Config.Workload != workload || validatePITRRunner(state.Config) != nil || !identifier.MatchString(state.Operation) || len(state.Bases) > 32 {
		return state, errors.New("invalid PITR state")
	}
	source := state.Source
	if source.Database != state.Config.Database || !imageDigest.MatchString(source.Image) || !fileContainerID.MatchString(source.Container) || source.Mount.Type != "volume" || !fileVolumeName.MatchString(source.Mount.Name) || !path.IsAbs(source.Data) || path.Clean(source.Data) != source.Data || !snapshotRootPath.MatchString(source.Data) || !path.IsAbs(source.Mount.Destination) || path.Clean(source.Mount.Destination) != source.Mount.Destination || !(source.Data == source.Mount.Destination || strings.HasPrefix(source.Data, source.Mount.Destination+"/")) || source.Relative != strings.TrimPrefix(strings.TrimPrefix(source.Data, source.Mount.Destination), "/") || source.Database.Host != path.Join("/source", source.Relative, ".cove-pitr-socket") || source.UID == "0" || !pitrSystemID.MatchString(source.UID) || !pitrSystemID.MatchString(source.GID) {
		return state, errors.New("invalid PITR PostgreSQL storage identity")
	}
	for name, operation := range state.Bases {
		if !pitrBaseName.MatchString(name) || !identifier.MatchString(operation) {
			return state, errors.New("invalid retained PITR base reference")
		}
	}
	return state, nil
}

func (e *Engine) savePITR(state pitrState) error {
	directory := e.pitrDirectory(state.Config.Workload)
	if err := writeJSON(filepath.Join(directory, "state.json"), state); err != nil {
		return err
	}
	return writeJSON(filepath.Join(e.Root, "workloads", state.Config.Workload, "pitr-bases.json"), map[string]any{"operation": state.Operation, "bases": state.Bases})
}

func (e *Engine) pitrContainer(ctx context.Context, workload string) (string, error) {
	owner, err := e.backupHelperOwner(false)
	if err != nil || owner == "" {
		return "", errors.New("PITR ownership unavailable")
	}
	name := "cove-pitr-" + workload
	id, err := e.pitrContainerID(ctx, name)
	if err != nil || id == "" {
		return "", err
	}
	data, err := e.output(ctx, "docker", "inspect", "--format", `{{index .Config.Labels "dev.cove.pitr.owner"}}`, name)
	if err != nil {
		return "", err
	}
	if data != owner {
		return "", errors.New("PITR helper name is occupied by another owner")
	}
	return name, nil
}

func (e *Engine) stopPITR(ctx context.Context, workload string) error {
	name, err := e.pitrContainer(ctx, workload)
	if err != nil || name == "" {
		return err
	}
	return e.Run(ctx, e.Root, []string{"docker", "rm", "-f", name}, nil, io.Discard)
}

func (e *Engine) startPITR(ctx context.Context, state pitrState) error {
	image, err := e.preparePITRTools(ctx)
	if err != nil {
		return err
	}
	binary, err := e.prepareSnapshotHostBinary()
	if err != nil {
		return err
	}
	directory := e.pitrDirectory(state.Config.Workload)
	if err := writeJSON(filepath.Join(directory, "config.json"), state.Config); err != nil {
		return err
	}
	if state.Config.Storage != nil {
		if _, err := storageMount(directory, state.Config.Storage); err != nil {
			return err
		}
	}
	if err := e.stopPITR(ctx, state.Config.Workload); err != nil {
		return err
	}
	// Create the shared archive mutex as the worker before the root helper can create it.
	unlock, err := lockWorker(directory)
	if err != nil {
		return err
	}
	unlock()
	if err = os.Remove(filepath.Join(directory, "status.json")); err != nil && !os.IsNotExist(err) {
		return err
	}
	arguments := []string{"docker", "run", "-d", "--name", "cove-pitr-" + state.Config.Workload, "--label", "dev.cove.pitr.owner=" + state.Config.Owner, "--label", "dev.cove.workload=" + state.Config.Workload, "--restart", "unless-stopped", "--read-only", "--tmpfs", "/tmp:rw,noexec,nosuid,size=128m", "--tmpfs", "/run/cove:rw,exec,nosuid,size=64m,mode=700", "--memory", "1g", "--cpus", "1", "--pids-limit", "128", "--log-driver", "none", "--mount", "type=bind,source=" + directory + ",target=/state", "--mount", "type=bind,source=" + binary + ",target=/worker,readonly", "--mount", "type=volume,source=" + state.Source.Mount.Name + ",target=/source,readonly"}
	if state.Config.Storage == nil {
		volume := "cove-pitr-archive-" + state.Config.Workload
		exists, err := e.pitrVolumeExists(ctx, volume)
		if err != nil {
			return err
		}
		if exists {
			existing, err := e.output(ctx, "docker", "volume", "inspect", "--format", `{{index .Labels "dev.cove.pitr.owner"}}`, volume)
			if err != nil {
				return err
			}
			if existing != state.Config.Owner {
				return errors.New("local PITR archive belongs to another owner")
			}
		} else if err = e.Run(ctx, e.Root, []string{"docker", "volume", "create", "--label", "dev.cove.pitr.owner=" + state.Config.Owner, "--label", "com.docker.compose.project=cove-" + state.Config.Workload, volume}, nil, io.Discard); err != nil {
			return err
		}
		arguments = append(arguments, "--mount", "type=volume,source="+volume+",target=/archive")
	}
	arguments = append(arguments, "--entrypoint", "/bin/sh", image, "-ec", "cp /worker /run/cove/worker; chmod 700 /run/cove/worker; exec /run/cove/worker pitr-runner")
	if err := e.Run(ctx, e.Root, arguments, nil, io.Discard); err != nil {
		return errors.New("PITR capture helper could not start")
	}
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		var status PITRStatus
		if err := readSnapshotMetadata(filepath.Join(directory, "status.json"), &status); err == nil && status.CheckedAt.After(time.Now().Add(-10*time.Second)) {
			if status.LastError != "" {
				return errors.New(status.LastError)
			}
			if state.Config.Policy.Settings.Enabled && status.Receiver || !state.Config.Policy.Settings.Enabled && status.State == "disabled" {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
	return errors.New("PITR helper did not become ready; inspect its capture status")
}

func (e *Engine) PITRStatus(ctx context.Context, workload string) (PITRStatus, error) {
	status := PITRStatus{State: "not_configured", Bases: []PITRBase{}}
	state, err := e.loadPITR(workload)
	if os.IsNotExist(err) {
		return status, nil
	}
	if err != nil {
		return status, err
	}
	if err := readSnapshotMetadata(filepath.Join(e.pitrDirectory(workload), "status.json"), &status); err != nil {
		if !os.IsNotExist(err) {
			return status, err
		}
		status.State = "starting"
	}
	status.Enabled = state.Config.Policy.Settings.Enabled
	if state.Configuring {
		status.State = "configuring"
	}
	if len(status.Bases) > 32 {
		return PITRStatus{}, errors.New("PITR base inventory exceeds its limit")
	}
	for i := range status.Bases {
		if !strings.HasPrefix(status.Bases[i].ID, state.Config.Policy.Archive+"_") || !pitrBaseName.MatchString(strings.TrimPrefix(status.Bases[i].ID, state.Config.Policy.Archive+"_")) {
			return PITRStatus{}, errors.New("invalid PITR base identity")
		}
	}
	return status, nil
}

func (e *Engine) stagePITRSocket(ctx context.Context, state pitrState) error {
	socket := path.Join(state.Source.Data, ".cove-pitr-socket")
	marker := pitrArchiveOwner(state.Config)
	script := `set -eu
directory=$1; owner=$2; uid=$3; gid=$4
if [ -L "$directory" ]; then exit 1; fi
if [ -d "$directory" ]; then
 test -f "$directory/owner" && test ! -L "$directory/owner" && test "$(cat "$directory/owner")" = "$owner"
else
 mkdir -m 700 "$directory"
 printf '%s' "$owner" > "$directory/owner"
 chmod 600 "$directory/owner"
 chown "$uid:$gid" "$directory" "$directory/owner"
fi`
	return runInput(ctx, e.Root, []string{"docker", "exec", "--user", "0", state.Source.Container, "sh", "-ec", script, "cove", socket, marker, state.Source.UID, state.Source.GID}, nil, nil, io.Discard)
}

func (e *Engine) configurePITR(ctx context.Context, r Request) error {
	if r.PITR == nil {
		return errors.New("PITR configuration required")
	}
	if r.Backup.Remote == nil && r.Backup.Repository != "local" {
		return errors.New("PITR requires local storage or a configured remote storage location")
	}
	directory := e.pitrDirectory(r.Workload)
	if err := os.MkdirAll(directory, 0700); err != nil {
		return err
	}
	entries, err := os.ReadDir(filepath.Dir(directory))
	if err != nil {
		return err
	}
	if len(entries) > pitrPolicyLimit {
		return errors.New("a worker supports at most 32 PITR resources")
	}
	state, err := e.loadPITR(r.Workload)
	if os.IsNotExist(err) {
		if !r.PITR.Settings.Enabled {
			entries, readErr := os.ReadDir(directory)
			if readErr != nil {
				return readErr
			}
			if len(entries) > 0 {
				return errors.New("PITR state is missing but archive artifacts remain; repair the private worker metadata before disabling")
			}
			return nil
		}
		owner, err := e.backupHelperOwner(true)
		if err != nil {
			return err
		}
		source, err := e.inspectPITRSource(ctx, r.Workload, r.PITR.Settings.Service, "cove_"+r.PITR.Archive)
		if err != nil {
			return err
		}
		state = pitrState{Config: pitrRunnerConfig{Workload: r.Workload, Owner: owner, Policy: *r.PITR, Database: source.Database, Storage: r.Backup.Remote}, Source: source, Bases: map[string]string{}}
	} else if err != nil {
		return err
	} else {
		if state.Config.Policy.Archive != r.PITR.Archive || state.Config.Policy.Key != r.PITR.Key || state.Config.Policy.Settings.Service != r.PITR.Settings.Service {
			return errors.New("PITR archive, key and PostgreSQL service cannot change while history is retained")
		}
		before, after := state.Config.Storage, r.Backup.Remote
		if (before == nil) != (after == nil) || before != nil && (before.Protocol != after.Protocol || before.Endpoint != after.Endpoint || before.Path != after.Path) {
			return errors.New("PITR storage cannot change while history is retained")
		}
		state.Config.Storage = r.Backup.Remote
		state.Config.RequireArchive = state.Config.RequireArchive || len(state.Bases) > 0
		state.Config.Policy.Settings = r.PITR.Settings
	}
	fresh, err := e.inspectPITRSource(ctx, r.Workload, r.PITR.Settings.Service, state.Source.Database.Slot)
	if err != nil {
		return err
	}
	if fresh.Database.SystemID != state.Source.Database.SystemID || fresh.Mount.Name != state.Source.Mount.Name || state.Config.Policy.Settings.Enabled && fresh.Image != state.Source.Image {
		return errors.New("PostgreSQL cluster, storage or image changed; disable and delete the old PITR history before beginning a new archive")
	}
	if state.Config.Policy.Settings.Enabled && fresh.Bytes > r.PITR.Settings.MaxBytes {
		return errors.New("PostgreSQL database size exceeds the configured PITR byte limit")
	}
	state.Source.Container = fresh.Container
	if err = validatePITRRunner(state.Config); err != nil {
		return err
	}
	state.Operation, state.Configuring = r.ID, true
	if err := e.savePITR(state); err != nil {
		return err
	}
	if err := e.stopPITR(ctx, r.Workload); err != nil {
		return err
	}
	if state.Config.Policy.Settings.Enabled {
		if _, err := e.preparePITRTools(ctx); err != nil {
			return err
		}
		probe := state
		probe.Config.Policy.Settings.Enabled = false
		if err := e.startPITR(ctx, probe); err != nil {
			return err
		}
		if err := e.stopPITR(ctx, r.Workload); err != nil {
			return err
		}
		if err := e.stagePITRSocket(ctx, state); err != nil {
			return errors.New("private PITR socket directory ownership could not be established")
		}
		sockets, err := pitrSocketSetting(state.Source.OriginalSockets, path.Join(state.Source.Data, ".cove-pitr-socket"))
		if err != nil {
			return err
		}
		current, err := pitrSocketSetting(fresh.OriginalSockets, "")
		if err != nil {
			return err
		}
		if current != sockets || fresh.OriginalSlotLimit != "512MB" {
			original, err := pitrSocketSetting(state.Source.OriginalSockets, "")
			if err != nil {
				return err
			}
			if current != original && current != sockets || fresh.OriginalSlotLimit != state.Source.OriginalSlotLimit && fresh.OriginalSlotLimit != "512MB" {
				return errors.New("PostgreSQL socket or WAL settings changed externally; reconcile them before enabling PITR")
			}
			if _, err = e.pitrSQL(ctx, state.Source.Container, "ALTER SYSTEM SET unix_socket_directories="+sockets+";\nALTER SYSTEM SET max_slot_wal_keep_size='512MB';"); err != nil {
				return err
			}
			if err = e.Run(ctx, e.Root, []string{"docker", "restart", state.Source.Container}, nil, io.Discard); err != nil {
				return err
			}
			if err = e.waitPITRSource(ctx, state.Source); err != nil {
				return err
			}
		}
		if err = e.createPITRSlot(ctx, state.Source); err != nil {
			return err
		}
	} else {
		if _, err = e.pitrSQL(ctx, state.Source.Container, "SELECT pg_drop_replication_slot(slot_name) FROM pg_replication_slots WHERE slot_name="+pitrSQLLiteral(state.Source.Database.Slot)+" AND NOT active;\nCHECKPOINT;"); err != nil {
			return err
		}
		configured, err := pitrSocketSetting(state.Source.OriginalSockets, path.Join(state.Source.Data, ".cove-pitr-socket"))
		if err != nil {
			return err
		}
		current, err := pitrSocketSetting(fresh.OriginalSockets, "")
		if err != nil {
			return err
		}
		original, err := pitrSocketSetting(state.Source.OriginalSockets, "")
		if err != nil {
			return err
		}
		if current != original || fresh.OriginalSlotLimit != state.Source.OriginalSlotLimit {
			if current != original && configured != current || fresh.OriginalSlotLimit != state.Source.OriginalSlotLimit && fresh.OriginalSlotLimit != "512MB" {
				return errors.New("PostgreSQL PITR settings changed externally; reconcile the recorded settings before retrying disable")
			}
			if _, err = e.pitrSQL(ctx, state.Source.Container, "ALTER SYSTEM SET unix_socket_directories="+original+";\nALTER SYSTEM SET max_slot_wal_keep_size="+pitrSQLLiteral(state.Source.OriginalSlotLimit)+";"); err != nil {
				return err
			}
			if err = e.Run(ctx, e.Root, []string{"docker", "restart", state.Source.Container}, nil, io.Discard); err != nil {
				return err
			}
			if err = e.waitPITRSource(ctx, state.Source); err != nil {
				return err
			}
		}
	}
	if !state.Config.Policy.Settings.Enabled {
		state.Configuring = false
		if err = e.savePITR(state); err != nil {
			return err
		}
	}
	if err = e.startPITR(ctx, state); err != nil {
		return err
	}
	state.Configuring = false
	return e.savePITR(state)
}

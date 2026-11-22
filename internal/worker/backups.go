package worker

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type BackupConfig struct {
	VolumeSnapshot     *VolumeSnapshotConfig `json:"volume_snapshot,omitempty"`
	KeepDaily          int                   `json:"keep_daily,omitempty"`
	KeepWeekly         int                   `json:"keep_weekly,omitempty"`
	KeepMonthly        int                   `json:"keep_monthly,omitempty"`
	DrillIntervalHours int                   `json:"drill_interval_hours,omitempty"`
	Database           *DatabaseBackup       `json:"database,omitempty"`
	StorageID          string                `json:"storage_id,omitempty"`
	Remote             *StorageLocation      `json:"remote,omitempty"`
	CACertificate      string                `json:"ca_certificate,omitempty"`
	Repository         string                `json:"repository"`
	Password           string                `json:"password,omitempty"`
	AccessKey          string                `json:"access_key,omitempty"`
	SecretKey          string                `json:"secret_key,omitempty"`
	SessionToken       string                `json:"session_token,omitempty"`
	KeepLast           int                   `json:"keep_last"`
	IntervalHours      int                   `json:"interval_hours"`
}
type backupMount struct {
	Type        string
	Source      string
	Name        string
	Destination string
	RW          bool
}
type BackupRecord struct {
	VolumeMethod   string          `json:"volume_method,omitempty"`
	FallbackReason string          `json:"fallback_reason,omitempty"`
	DataSnapshot   string          `json:"data_snapshot,omitempty"`
	VerifiedAt     *time.Time      `json:"verified_at,omitempty"`
	Verification   string          `json:"verification,omitempty"`
	Database       *DatabaseBackup `json:"database,omitempty"`
	ID             string          `json:"id"`
	Workload       string          `json:"workload"`
	Snapshot       string          `json:"snapshot"`
	Created        time.Time       `json:"created_at"`
	Repository     string          `json:"repository"`
	Mounts         []backupMount   `json:"-"`
}
type backupManifest struct {
	Record BackupRecord  `json:"record"`
	Mounts []backupMount `json:"mounts"`
	State  deployedState `json:"state"`
}
type storageRecovery struct {
	DatabaseUser       string   `json:"database_user,omitempty"`
	DatabaseVerify     string   `json:"database_verify,omitempty"`
	DatabaseEngine     string   `json:"database_engine,omitempty"`
	DatabaseAppendOnly bool     `json:"database_appendonly,omitempty"`
	DatabaseContainer  string   `json:"database_container,omitempty"`
	Containers         []string `json:"containers"`
	Restoring          bool     `json:"restoring"`
	Helper             string   `json:"helper"`
}

func ValidateBackup(b *BackupConfig) error {
	if b == nil {
		return nil
	}
	if err := validateDatabaseBackup(b.Database); err != nil {
		return err
	}
	if err := validateVolumeSnapshot(b.VolumeSnapshot); err != nil {
		return err
	}
	if b.Database != nil && b.VolumeSnapshot != nil {
		return errors.New("filesystem snapshots apply only to volume backups")
	}
	if b.CACertificate != "" {
		roots := x509.NewCertPool()
		if len(b.CACertificate) > 65536 || !roots.AppendCertsFromPEM([]byte(b.CACertificate)) {
			return errors.New("invalid backup CA certificate")
		}
	}
	if b.DrillIntervalHours < 0 || b.DrillIntervalHours > 8760 {
		return errors.New("restore drill interval must be 0-8760 hours")
	}
	if b.KeepLast < 1 || b.KeepLast > 1000 || b.IntervalHours < 0 || b.IntervalHours > 8760 {
		return errors.New("backup retention must be 1-1000 and interval 0-8760 hours")
	}
	if b.KeepDaily < 0 || b.KeepDaily > 366 || b.KeepWeekly < 0 || b.KeepWeekly > 520 || b.KeepMonthly < 0 || b.KeepMonthly > 120 {
		return errors.New("daily retention must be 0-366, weekly 0-520 and monthly 0-120")
	}
	if len(b.Password) < 16 || len(b.Password) > 4096 {
		return errors.New("backup encryption password must contain 16-4096 bytes")
	}
	if b.Remote != nil {
		if err := ValidateStorage(b.Remote); err != nil {
			return err
		}
		if !strings.HasPrefix(b.Repository, "rclone:cove:") {
			return errors.New("invalid remote repository")
		}
	} else if b.StorageID != "" {
		if !workloadIdentifier.MatchString(b.StorageID) {
			return errors.New("invalid storage location")
		}
	} else if b.Repository != "local" {
		if !strings.HasPrefix(b.Repository, "s3:") {
			return errors.New("backup repository must be local or s3:https://endpoint/bucket/path")
		}
		u, err := url.Parse(strings.TrimPrefix(b.Repository, "s3:"))
		if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || len(u.Path) < 2 {
			return errors.New("invalid S3 repository URL")
		}
	}
	for _, value := range []string{b.Repository, b.Password, b.AccessKey, b.SecretKey, b.SessionToken} {
		if strings.ContainsRune(value, 0) || len(value) > 8192 {
			return errors.New("invalid backup credential")
		}
	}
	return nil
}

func (e *Engine) backupMounts(ctx context.Context, workload string) ([]backupMount, []string, error) {
	containers, err := e.Containers(ctx, workload)
	if err != nil {
		return nil, nil, err
	}
	var mounts []backupMount
	var running []string
	owned := map[string]bool{}
	for _, container := range containers {
		owned[container.ID] = true
	}
	seen := map[string]bool{}
	for _, container := range containers {
		if container.State == "paused" {
			return nil, nil, errors.New("unpause workload containers before backing up")
		}
		if container.State == "running" {
			running = append(running, container.ID)
		}
		data, err := e.output(ctx, "docker", "inspect", "--format", "{{json .Mounts}}", container.ID)
		if err != nil {
			return nil, nil, err
		}
		var list []backupMount
		if err = json.Unmarshal([]byte(data), &list); err != nil {
			return nil, nil, err
		}
		for _, mount := range list {
			if !mount.RW {
				continue
			}
			if mount.Type != "volume" && mount.Type != "bind" {
				return nil, nil, errors.New("backup supports persistent volumes and bind mounts only")
			}
			source := mount.Source
			if mount.Type == "volume" {
				source = mount.Name
			}
			if source == "" || strings.ContainsAny(source, ",\r\n") {
				return nil, nil, errors.New("unsupported backup mount path")
			}
			if mount.Type == "bind" {
				clean := filepath.Clean(source)
				if clean == "/var/run/docker.sock" || clean == "/run/docker.sock" {
					info, err := os.Stat(clean)
					if err != nil || info.Mode()&os.ModeSocket == 0 {
						return nil, nil, errors.New("Docker socket mount is not a socket")
					}
					continue
				}
				imported := filepath.Join(e.Root, "imported-data")
				importedData := strings.HasPrefix(clean, imported+string(filepath.Separator))
				if importedData {
					resolved, err := filepath.EvalSymlinks(clean)
					if err != nil || resolved != clean {
						return nil, nil, errors.New("imported backup mount must exist without symbolic links")
					}
				}
				for _, forbidden := range []string{"/", "/etc", "/usr", "/var", "/home", "/root", "/bin", "/sbin", "/boot", "/proc", "/sys", "/dev", "/run", "/var/lib/docker", "/var/lib/containerd", e.Root} {
					if forbidden == e.Root && importedData {
						continue
					}
					if clean == forbidden || (forbidden != "/" && (forbidden == e.Root || forbidden == "/proc" || forbidden == "/sys" || forbidden == "/dev" || forbidden == "/run" || forbidden == "/var/lib/docker" || forbidden == "/var/lib/containerd") && strings.HasPrefix(clean, forbidden+string(filepath.Separator))) {
						return nil, nil, errors.New("refusing to back up a system or worker-state mount")
					}
				}
			}
			key := mount.Type + ":" + source
			if !seen[key] {
				seen[key] = true
				mount.Destination = ""
				mounts = append(mounts, mount)
			}
		}
	}
	sort.Slice(mounts, func(i, j int) bool {
		return mounts[i].Type+mounts[i].Source+mounts[i].Name < mounts[j].Type+mounts[j].Source+mounts[j].Name
	})
	if len(mounts) == 0 {
		return nil, nil, errors.New("workload has no writable persistent mounts to back up")
	}
	for _, mount := range mounts {
		source := mount.Source
		if mount.Type == "volume" {
			source = mount.Name
		}
		ids, err := e.output(ctx, "docker", "ps", "-q", "--filter", "volume="+source)
		if err != nil {
			return nil, nil, err
		}
		for _, id := range strings.Fields(ids) {
			if !owned[id] {
				return nil, nil, errors.New("persistent storage is shared with another running workload; stop it before backup or restore")
			}
		}
	}
	return mounts, running, nil
}

func (e *Engine) restic(ctx context.Context, r Request, mounts []backupMount, writable bool, out io.Writer, command ...string) error {
	return e.resticInput(ctx, r, mounts, writable, nil, out, command...)
}

func (e *Engine) resticInput(ctx context.Context, r Request, mounts []backupMount, writable bool, input io.Reader, out io.Writer, command ...string) error {
	owner, ownerErr := e.backupHelperOwner(e.Root != "")
	if ownerErr != nil {
		return ownerErr
	}
	b := r.Backup
	repository := b.Repository
	env := []string{"RESTIC_PASSWORD=" + b.Password, "AWS_ACCESS_KEY_ID=" + b.AccessKey, "AWS_SECRET_ACCESS_KEY=" + b.SecretKey, "AWS_SESSION_TOKEN=" + b.SessionToken}
	args := []string{"docker", "run", "--rm", "--log-driver", "none", "--name", "cove-backup-" + r.ID, "--label", "dev.cove.operation=" + r.ID, "--network", "host", "--read-only", "--tmpfs", "/tmp", "--tmpfs", "/data", "-e", "RESTIC_PASSWORD", "-e", "RESTIC_REPOSITORY", "-e", "AWS_ACCESS_KEY_ID", "-e", "AWS_SECRET_ACCESS_KEY", "-e", "AWS_SESSION_TOKEN"}
	if owner != "" {
		args = append(args, "--label", "dev.cove.backup-owner="+owner)
	}
	if b.CACertificate != "" {
		path := filepath.Join(e.Root, "workloads", r.Workload, "backup-ca.pem")
		if err := os.WriteFile(path, []byte(b.CACertificate), 0600); err != nil {
			return err
		}
		args = append(args, "--mount", "type=bind,source="+path+",target=/backup-ca.pem,readonly", "-e", "RESTIC_CACERT=/backup-ca.pem")
	}
	if repository == "local" {
		repository = "/repository"
		volume := "cove-backups-" + r.Workload
		label, err := e.output(ctx, "docker", "volume", "inspect", "--format", `{{index .Labels "cove.backup"}}`, volume)
		if err != nil {
			if err = e.Run(ctx, e.Root, []string{"docker", "volume", "create", "--label", "cove.backup=" + r.Workload, volume}, nil, out); err != nil {
				return err
			}
		} else if label != r.Workload {
			return errors.New("backup repository volume is not owned by this workload")
		}
		args = append(args, "--mount", "type=volume,source="+volume+",target=/repository")
	}
	env = append(env, "RESTIC_REPOSITORY="+repository)
	for i, mount := range mounts {
		source := mount.Source
		if mount.Type == "volume" {
			source = mount.Name
		}
		spec := fmt.Sprintf("type=%s,source=%s,target=/data/%03d", mount.Type, source, i)
		if !writable {
			spec += ",readonly"
		}
		args = append(args, "--mount", spec)
	}
	if len(command) > 0 && command[0] == "backup" && input == nil {
		metadata := filepath.Join(e.Root, "workloads", r.Workload, "backups", r.ID+".source.json")
		args = append(args, "--mount", "type=bind,source="+metadata+",target=/cove-manifest.json,readonly")
	}
	image := "restic/restic:0.19.1"
	if b.Remote != nil {
		image = remoteBackupImage
		remoteEnv, err := storageEnvironment(b.Remote)
		if err != nil {
			return err
		}
		env = append(env, remoteEnv...)
		for _, value := range remoteEnv {
			key, _, _ := strings.Cut(value, "=")
			args = append(args, "-e", key)
		}
		dir, err := os.MkdirTemp(e.Root, "remote-backup-")
		if err != nil {
			return err
		}
		defer os.RemoveAll(dir)
		mounts, err := storageMount(dir, b.Remote)
		if err != nil {
			return err
		}
		args = append(args, mounts...)
	}
	args = append(args, image, "--no-cache")
	if input != nil {
		args = append([]string{"docker", "run", "--interactive"}, args[2:]...)
	}
	args = append(args, command...)
	var err error
	if input == nil {
		err = e.Run(ctx, e.Root, args, env, out)
	} else {
		err = runInput(ctx, e.Root, args, env, input, out)
	}
	if ctx.Err() != nil || (input != nil && err != nil) {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		if owner != "" {
			return errors.Join(err, e.removeBackupHelper(cleanup, owner, "cove-backup-"+r.ID, r.ID))
		}
		if removeErr := e.Run(cleanup, e.Root, []string{"docker", "rm", "-f", "cove-backup-" + r.ID}, nil, io.Discard); removeErr != nil {
			return errors.Join(err, removeErr)
		}
	}
	return err
}

func (e *Engine) Backups(workload string) ([]BackupRecord, error) {
	if !workloadIdentifier.MatchString(workload) {
		return nil, errors.New("invalid workload")
	}
	entries, err := os.ReadDir(filepath.Join(e.Root, "workloads", workload, "backups"))
	if os.IsNotExist(err) {
		return []BackupRecord{}, nil
	}
	if err != nil {
		return nil, err
	}
	records := []BackupRecord{}
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".json") || strings.HasSuffix(entry.Name(), ".source.json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(e.Root, "workloads", workload, "backups", entry.Name()))
		if err != nil {
			return nil, err
		}
		var manifest backupManifest
		if err = json.Unmarshal(data, &manifest); err != nil {
			return nil, err
		}
		records = append(records, manifest.Record)
	}
	sort.Slice(records, func(i, j int) bool { return records[i].Created.After(records[j].Created) })
	return records, nil
}

func (e *Engine) makeBackup(ctx context.Context, r Request, mounts []backupMount, state deployedState, out io.Writer) (backupManifest, error) {
	return e.makeBackupWithData(ctx, r, mounts, state, out, "")
}

func (e *Engine) makeBackupWithData(ctx context.Context, r Request, mounts []backupMount, state deployedState, out io.Writer, dataSnapshot string) (backupManifest, error) {
	dir := filepath.Join(e.Root, "workloads", r.Workload, "backups")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return backupManifest{}, err
	}
	manifest := backupManifest{Record: BackupRecord{ID: r.ID, Workload: r.Workload, Repository: r.Backup.Repository, Created: time.Now().UTC(), Database: r.Backup.Database}, Mounts: mounts, State: state}
	manifest.Record.DataSnapshot = dataSnapshot
	manifest.Record.VolumeMethod, manifest.Record.FallbackReason = r.volumeMethod, r.snapshotFallback
	metadata := filepath.Join(dir, r.ID+".source.json")
	if err := writeJSON(metadata, manifest); err != nil {
		return manifest, err
	}
	backupMounts := mounts
	if r.snapshotMounts != nil {
		backupMounts = r.snapshotMounts
	}
	if dataSnapshot != "" {
		backupMounts = nil
	}
	if err := e.restic(ctx, r, backupMounts, false, out, "backup", "/data", "/cove-manifest.json", "--tag", "cove-workload:"+r.Workload, "--tag", "cove-operation:"+r.ID); err != nil {
		return backupManifest{}, err
	}
	if r.snapshotCheck != nil {
		if err := r.snapshotCheck(ctx); err != nil {
			return backupManifest{}, err
		}
	}
	result := &limitedBuffer{Limit: 2 * 1024 * 1024}
	if err := e.restic(ctx, r, nil, false, result, "snapshots", "--json", "--tag", "cove-operation:"+r.ID); err != nil {
		return backupManifest{}, err
	}
	var snapshots []struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(result.data.Bytes(), &snapshots); err != nil {
		return backupManifest{}, err
	}
	if len(snapshots) != 1 || len(snapshots[0].ID) != 64 {
		return backupManifest{}, errors.New("backup snapshot could not be identified")
	}
	manifest.Record.Snapshot = snapshots[0].ID
	return manifest, writeJSON(filepath.Join(dir, r.ID+".json"), manifest)
}

func (e *Engine) performBackup(ctx context.Context, r Request, out io.Writer) (result error) {
	e.backupMu.Lock()
	defer e.backupMu.Unlock()
	if r.Backup.Database != nil {
		if extendedDatabase(r.Backup.Database.Engine) {
			return e.performExtendedDatabaseBackup(ctx, r, out)
		}
		return e.performDatabaseBackup(ctx, r, out)
	}
	dir := filepath.Join(e.Root, "workloads", r.Workload)
	data, err := os.ReadFile(filepath.Join(dir, "current.json"))
	if err != nil {
		return err
	}
	var current deployedState
	if err = json.Unmarshal(data, &current); err != nil {
		return err
	}
	mounts, running, err := e.backupMounts(ctx, r.Workload)
	if err != nil {
		return err
	}
	var restore backupManifest
	if r.Action == "restore" {
		restore, err = e.loadBackupManifest(ctx, r, out)
		if err != nil {
			return err
		}
		if restore.Record.Database != nil {
			return errors.New("select the matching logical database backup policy before restoring this snapshot")
		}
		if restore.Record.Workload != r.Workload || restore.Record.Repository != r.Backup.Repository {
			return errors.New("backup belongs to another workload or repository")
		}
		old, _ := json.Marshal(portableBackupMounts(restore.Mounts))
		now, _ := json.Marshal(portableBackupMounts(mounts))
		if string(old) != string(now) {
			return errors.New("storage layout changed since backup; restore to the original layout before retrying")
		}
		if err = e.restic(ctx, r, nil, false, out, "check", "--read-data"); err != nil {
			return err
		}
	}
	recoveryPath := filepath.Join(dir, "storage-recovery.json")
	interruptedRestore := false
	if prior, readErr := os.ReadFile(recoveryPath); readErr == nil {
		var old storageRecovery
		if err = json.Unmarshal(prior, &old); err != nil {
			return err
		}
		if old.Restoring && r.Action == "restore" && len(running) == 0 {
			running = old.Containers
		}
		interruptedRestore = old.Restoring
	} else if !os.IsNotExist(readErr) {
		return readErr
	}
	if r.Backup.Remote != nil {
		if err = e.remoteBackupHelper(ctx, out); err != nil {
			return err
		}
	} else if _, err = e.output(ctx, "docker", "image", "inspect", "restic/restic:0.19.1"); err != nil {
		if err = e.Run(ctx, dir, []string{"docker", "pull", "restic/restic:0.19.1"}, nil, out); err != nil {
			return err
		}
	}
	if r.Action == "backup" {
		probeCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		probe := &limitedBuffer{Limit: 2 * 1024 * 1024}
		err = e.restic(probeCtx, r, nil, false, probe, "snapshots", "--json")
		timedOut := probeCtx.Err() != nil
		cancel()
		if timedOut {
			return errors.New("backup repository did not respond; check the endpoint, bucket and credentials")
		}
		if err != nil {
			if err = e.restic(ctx, r, nil, false, out, "init"); err != nil {
				return err
			}
		}
	}
	if r.Action == "backup" && r.Backup.VolumeSnapshot != nil {
		handled, reason, snapshotErr := e.performFilesystemBackup(ctx, r, mounts, running, current, out)
		if handled || snapshotErr != nil {
			return snapshotErr
		}
		r.volumeMethod, r.snapshotFallback = "stopped", reason
		fmt.Fprintln(out, "Filesystem snapshot unavailable; using a stopped backup:", reason)
	}
	recovery := storageRecovery{Containers: running, Restoring: interruptedRestore, Helper: "cove-backup-" + r.ID}
	if err = writeJSON(recoveryPath, recovery); err != nil {
		return err
	}
	restart := !interruptedRestore
	defer func() {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
		defer cancel()
		if restart && len(running) > 0 {
			if err := e.startBackupContainers(cleanup, running, out); err != nil {
				result = errors.Join(result, err)
				return
			}
		}
		if restart {
			if err := os.Remove(recoveryPath); err != nil && !os.IsNotExist(err) {
				result = errors.Join(result, err)
			}
		}
	}()
	if len(running) > 0 {
		if err = e.Run(ctx, dir, append([]string{"docker", "stop", "--time", "60"}, running...), nil, out); err != nil {
			return err
		}
	}
	if r.Action == "backup" {
		_, err = e.makeBackup(ctx, r, mounts, current, out)
		return err
	}
	safety := r
	safety.ID = r.ID + "-before"
	recovery.Helper = "cove-backup-" + safety.ID
	if err = writeJSON(recoveryPath, recovery); err != nil {
		return err
	}
	if _, err = e.makeBackup(ctx, safety, mounts, current, out); err != nil {
		return err
	}
	recovery.Restoring = true
	recovery.Helper = "cove-backup-" + r.ID
	if err = writeJSON(recoveryPath, recovery); err != nil {
		return err
	}
	restart = false
	if err = e.restic(ctx, r, mounts, true, out, "restore", restore.Record.Snapshot, "--target", "/", "--include", "/data", "--delete", "--verify"); err != nil {
		fmt.Fprintln(out, "Restore failed. Containers remain stopped; the pre-restore safety backup is retained. Retry a restore after resolving the error.")
		return err
	}
	restart = true
	return nil
}

func (e *Engine) pruneBackups(ctx context.Context, r Request, out io.Writer) error {
	e.backupMu.Lock()
	defer e.backupMu.Unlock()
	return e.pruneBackupsLocked(ctx, r, out)
}

func (e *Engine) pruneBackupsLocked(ctx context.Context, r Request, out io.Writer) error {
	records, err := e.Backups(r.Workload)
	if err != nil {
		return err
	}
	retained := retainedBackups(records, r.Backup)
	pruned := false
	for _, record := range records {
		if record.Repository != r.Backup.Repository {
			continue
		}
		if retained[record.ID] {
			continue
		}
		if record.DataSnapshot != "" {
			if err = e.forgetDatabaseSnapshots(ctx, r, record); err != nil {
				return err
			}
		} else if err = e.restic(ctx, r, nil, false, out, "forget", record.Snapshot); err != nil {
			return err
		}
		pruned = true
		for _, suffix := range []string{".json", ".source.json"} {
			if err = os.Remove(filepath.Join(e.Root, "workloads", r.Workload, "backups", record.ID+suffix)); err != nil && !os.IsNotExist(err) {
				return err
			}
		}
	}
	if pruned {
		return e.restic(ctx, r, nil, false, out, "prune")
	}
	return nil
}

func (e *Engine) recoverStorage(ctx context.Context) error {
	entries, err := os.ReadDir(filepath.Join(e.Root, "workloads"))
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		path := filepath.Join(e.Root, "workloads", entry.Name(), "storage-recovery.json")
		data, err := os.ReadFile(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		var state storageRecovery
		if err = json.Unmarshal(data, &state); err != nil {
			return err
		}
		exists, err := e.output(ctx, "docker", "ps", "-aq", "--filter", "name=^/"+state.Helper+"$")
		if err != nil {
			return err
		}
		if exists != "" {
			if err = e.Run(ctx, e.Root, []string{"docker", "rm", "-f", state.Helper}, nil, os.Stderr); err != nil {
				return err
			}
		}
		if state.Restoring {
			if state.DatabaseVerify != "" {
				if !identifier.MatchString(state.DatabaseVerify) {
					return errors.New("invalid database verifier recovery identity")
				}
				if err = e.cleanupDatabaseVerifier(ctx, Request{ID: state.DatabaseVerify, Workload: entry.Name()}); err != nil {
					return err
				}
			}
			if state.DatabaseContainer != "" {
				if err = e.Run(ctx, e.Root, []string{"docker", "stop", "--time", "30", state.DatabaseContainer}, nil, os.Stderr); err != nil {
					return err
				}
			}
			continue
		}
		if len(state.Containers) > 0 {
			if err = e.startBackupContainers(ctx, state.Containers, os.Stderr); err != nil {
				return err
			}
		}
		if err = os.Remove(path); err != nil {
			return err
		}
	}
	return nil
}

func (e *Engine) startBackupContainers(ctx context.Context, containers []string, out io.Writer) error {
	labels := map[string]map[string]string{}
	services := map[string]string{}
	for _, id := range containers {
		raw, err := e.output(ctx, "docker", "inspect", "--format", "{{json .Config.Labels}}", id)
		if err != nil {
			return err
		}
		var values map[string]string
		if err := json.Unmarshal([]byte(raw), &values); err != nil {
			return err
		}
		labels[id] = values
		if project, service := values["com.docker.compose.project"], values["com.docker.compose.service"]; project != "" && service != "" {
			services[project+"/"+service] = id
		}
	}
	dependencies := map[string][]string{}
	healthRequired := map[string]bool{}
	for id, values := range labels {
		for _, dependency := range strings.Split(values["com.docker.compose.depends_on"], ",") {
			parts := strings.Split(dependency, ":")
			if len(parts) < 2 {
				continue
			}
			if target := services[values["com.docker.compose.project"]+"/"+parts[0]]; target != "" {
				dependencies[id] = append(dependencies[id], target)
				if parts[1] == "service_healthy" {
					healthRequired[target] = true
				}
			}
		}
	}
	started := map[string]bool{}
	remaining := append([]string(nil), containers...)
	for len(remaining) > 0 {
		var pending []string
		var failures []error
		var failedOutput bytes.Buffer
		for _, id := range remaining {
			if err := ctx.Err(); err != nil {
				return err
			}
			waiting := false
			for _, dependency := range dependencies[id] {
				waiting = waiting || !started[dependency]
			}
			if waiting {
				pending = append(pending, id)
				continue
			}
			var output bytes.Buffer
			if err := e.Run(ctx, e.Root, []string{"docker", "start", id}, nil, &output); err != nil {
				pending = append(pending, id)
				failures = append(failures, err)
				failedOutput.Write(output.Bytes())
			} else {
				_, _ = out.Write(output.Bytes())
				if healthRequired[id] {
					if err := e.waitBackupHealth(ctx, id); err != nil {
						return err
					}
				}
				started[id] = true
			}
		}
		if len(pending) == len(remaining) {
			_, _ = out.Write(failedOutput.Bytes())
			if len(failures) == 0 {
				return errors.New("backup recovery containers have circular startup dependencies")
			}
			return errors.Join(failures...)
		}
		// Namespace-sharing containers can start only after their owner has started.
		remaining = pending
	}
	return nil
}

func (e *Engine) waitBackupHealth(ctx context.Context, id string) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	for {
		raw, err := e.output(ctx, "docker", "inspect", "--format", "{{json .State}}", id)
		if err != nil {
			return err
		}
		var state struct {
			Running bool
			Health  struct{ Status string }
		}
		if err := json.Unmarshal([]byte(raw), &state); err != nil {
			return err
		}
		if !state.Running || state.Health.Status == "" {
			return errors.New("backup recovery dependency stopped or has no required health check")
		}
		if state.Health.Status == "healthy" {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("backup recovery dependency did not become healthy: %w", ctx.Err())
		case <-time.After(time.Second):
		}
	}
}

func portableBackupMounts(mounts []backupMount) []backupMount {
	result := append([]backupMount(nil), mounts...)
	for i := range result {
		if result[i].Type == "volume" {
			result[i].Source = ""
		}
	}
	return result
}

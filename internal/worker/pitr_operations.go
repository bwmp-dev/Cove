package worker

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
)

func (e *Engine) holdPITRImage(ctx context.Context, state pitrState) error {
	name := "cove-pitr-image-" + state.Config.Workload
	id, err := e.pitrContainerID(ctx, name)
	if err != nil {
		return err
	}
	if id != "" {
		owner, err := e.output(ctx, "docker", "inspect", "--format", `{{index .Config.Labels "dev.cove.pitr.owner"}}`, name)
		if err != nil {
			return err
		}
		image, err := e.output(ctx, "docker", "inspect", "--format", "{{.Image}}", name)
		if err != nil || owner != state.Config.Owner || image != state.Source.Image {
			return errors.New("PITR retained image ownership changed")
		}
		return nil
	}
	raw, err := e.output(ctx, "docker", "image", "inspect", "--format", "{{json .Config.Volumes}}", state.Source.Image)
	if err != nil {
		return err
	}
	var volumes map[string]any
	if json.Unmarshal([]byte(raw), &volumes) != nil || len(volumes) > 32 {
		return errors.New("PITR image declares too many persistent mount points")
	}
	args := []string{"docker", "create", "--name", name, "--label", "dev.cove.pitr.owner=" + state.Config.Owner, "--label", "dev.cove.workload=" + state.Config.Workload, "--network", "none", "--read-only", "--entrypoint", "/bin/true"}
	for directory := range volumes {
		if !path.IsAbs(directory) || path.Clean(directory) != directory || strings.ContainsAny(directory, ",\r\n") {
			return errors.New("PITR image declares an unsupported mount point")
		}
		args = append(args, "--mount", "type=tmpfs,target="+directory)
	}
	return e.Run(ctx, e.Root, append(args, state.Source.Image), nil, io.Discard)
}

func (e *Engine) backupPITR(ctx context.Context, r Request) error {
	e.backupMu.Lock()
	defer e.backupMu.Unlock()
	state, err := e.loadPITR(r.Workload)
	if err != nil {
		return err
	}
	if !state.Config.Policy.Settings.Enabled || state.Configuring || state.Config.Policy.Archive != r.PITR.Archive {
		return errors.New("PITR capture is not ready")
	}
	fresh, err := e.inspectPITRSource(ctx, r.Workload, state.Config.Policy.Settings.Service, state.Source.Database.Slot)
	if err != nil {
		return err
	}
	if fresh.Image != state.Source.Image || fresh.Database.SystemID != state.Source.Database.SystemID || fresh.Mount.Name != state.Source.Mount.Name {
		return errors.New("PostgreSQL image, cluster or storage changed; disable and replace the PITR history before taking a new base backup")
	}
	if fresh.Bytes > state.Config.Policy.Settings.MaxBytes {
		return errors.New("PostgreSQL database size exceeds the configured PITR byte limit")
	}
	state.Source.Container = fresh.Container
	if err = e.holdPITRImage(ctx, state); err != nil {
		return err
	}
	helper, err := e.pitrContainer(ctx, r.Workload)
	if err != nil {
		return err
	}
	status, err := e.PITRStatus(ctx, r.Workload)
	if err != nil {
		return err
	}
	running := false
	if helper != "" {
		value, err := e.output(ctx, "docker", "inspect", "--format", "{{.State.Running}}", helper)
		if err != nil {
			return err
		}
		running = value == "true"
	}
	if helper != "" && (!running || !status.Receiver || status.State == "paused" || status.State == "error") {
		if err = e.stopPITR(ctx, r.Workload); err != nil {
			return err
		}
		helper = ""
	}
	if helper == "" {
		if err = e.createPITRSlot(ctx, state.Source); err != nil {
			return err
		}
		if err = e.startPITR(ctx, state); err != nil {
			return err
		}
		helper = "cove-pitr-" + r.Workload
	}
	if err = e.Run(ctx, e.Root, []string{"docker", "exec", helper, "/run/cove/worker", "pitr-base"}, nil, io.Discard); err != nil {
		return errors.New("PITR base backup failed; inspect capture status before retrying")
	}
	var base pitrStoredBase
	if err = readSnapshotMetadata(filepath.Join(e.pitrDirectory(r.Workload), "base-result.json"), &base); err != nil {
		return err
	}
	if !pitrBaseName.MatchString(base.Name) {
		return errors.New("invalid PITR base backup result")
	}
	if state.Bases == nil {
		state.Bases = map[string]string{}
	}
	state.Bases[base.Name] = r.ID
	state.Config.RequireArchive = true
	status, err = e.PITRStatus(ctx, r.Workload)
	if err != nil {
		return err
	}
	retained := map[string]bool{base.Name: true}
	for _, item := range status.Bases {
		retained[strings.TrimPrefix(item.ID, state.Config.Policy.Archive+"_")] = true
	}
	for name := range state.Bases {
		if !retained[name] {
			delete(state.Bases, name)
		}
	}
	if len(state.Bases) > 32 {
		return errors.New("PITR retention must complete before another base backup")
	}
	return e.savePITR(state)
}

func RunPITRForget(ctx context.Context) error {
	var config pitrRunnerConfig
	if err := readSnapshotMetadata("/state/config.json", &config); err != nil {
		return err
	}
	if err := validatePITRRunner(config); err != nil {
		return err
	}
	if config.Policy.Settings.Enabled {
		return errors.New("disable PITR before deleting its history")
	}
	unlock, err := lockPITRArchive(ctx, "/state")
	if err != nil {
		return err
	}
	defer unlock()
	root, environment, err := pitrStorageEnvironment(config)
	if err != nil {
		return err
	}
	environment, err = obscurePITRStoragePassword(ctx, environment)
	if err != nil {
		return err
	}
	if err = ensurePITRArchive(ctx, config, root, environment); err != nil {
		return err
	}
	remote := strings.TrimSuffix(root, "/") + "/" + config.Policy.Archive
	if _, err = pitrCommandOutput(ctx, []string{"rclone", "delete", "--exclude", "/.cove-pitr-owner", remote}, environment, 65536); err != nil {
		return err
	}
	inventory, err := pitrArchiveInventory(ctx, root, config, environment)
	if err != nil {
		return err
	}
	if inventory.Objects != 1 || inventory.Bytes != 64 {
		return errors.New("PITR history removal is incomplete; its owner marker is retained for retry")
	}
	if err = writePITRMetadata("/state/forget-ready.json", map[string]string{"archive": config.Policy.Archive}); err != nil {
		return err
	}
	if _, err = pitrCommandOutput(ctx, []string{"rclone", "deletefile", remote + "/.cove-pitr-owner"}, environment, 65536); err != nil {
		return err
	}
	_, err = pitrCommandOutput(ctx, []string{"rclone", "rmdirs", "--leave-root", remote}, environment, 65536)
	return err
}

func (e *Engine) forgetPITR(ctx context.Context, r Request) error {
	e.backupMu.Lock()
	defer e.backupMu.Unlock()
	state, err := e.loadPITR(r.Workload)
	if os.IsNotExist(err) {
		entries, readErr := os.ReadDir(e.pitrDirectory(r.Workload))
		if os.IsNotExist(readErr) {
			return nil
		}
		if readErr != nil {
			return readErr
		}
		if len(entries) != 0 {
			return errors.New("PITR state is missing but archive artifacts remain; repair the private worker metadata before deleting")
		}
		return os.Remove(e.pitrDirectory(r.Workload))
	}
	if err != nil {
		return err
	}
	if state.Config.Policy.Settings.Enabled || state.Configuring {
		return errors.New("disable PITR capture before deleting its history")
	}
	if state.Config.Policy.Archive != r.PITR.Archive || state.Config.Policy.Key != r.PITR.Key {
		return errors.New("PITR history identity changed")
	}
	socket := path.Join("/source", state.Source.Relative, ".cove-pitr-socket")
	script := `set -eu
directory=$1; owner=$2
if [ -e "$directory" ]; then
 test -d "$directory" && test ! -L "$directory"
 test -f "$directory/owner" && test ! -L "$directory/owner"
 test "$(cat "$directory/owner")" = "$owner"
 test ! -e "$directory/.s.PGSQL.5432"
 test -z "$(find "$directory" -mindepth 1 -maxdepth 1 ! -name owner ! -name .s.PGSQL.5432.lock -print -quit)"
 if [ -e "$directory/.s.PGSQL.5432.lock" ]; then
  test -f "$directory/.s.PGSQL.5432.lock" && test ! -L "$directory/.s.PGSQL.5432.lock"
  rm "$directory/.s.PGSQL.5432.lock"
 fi
 rm "$directory/owner"
 rmdir "$directory"
fi`
	if err = runInput(ctx, e.Root, []string{"docker", "run", "--rm", "--network", "none", "--read-only", "--volume", state.Source.Mount.Name + ":/source", "busybox:1.37", "sh", "-ec", script, "cove", socket, pitrArchiveOwner(state.Config)}, nil, nil, io.Discard); err != nil {
		return errors.New("PITR socket cleanup is unsafe or still active; save disabled capture settings and retry before deleting history")
	}
	var ready struct {
		Archive string `json:"archive"`
	}
	if err = readSnapshotMetadata(filepath.Join(e.pitrDirectory(r.Workload), "forget-ready.json"), &ready); err == nil {
		if ready.Archive != state.Config.Policy.Archive {
			return errors.New("PITR deletion receipt belongs to another archive")
		}
		state.Config.RequireArchive = false
	} else if !os.IsNotExist(err) {
		return err
	}
	if err = e.startPITR(ctx, state); err != nil {
		return err
	}
	if err = e.Run(ctx, e.Root, []string{"docker", "exec", "cove-pitr-" + r.Workload, "/run/cove/worker", "pitr-forget"}, nil, io.Discard); err != nil {
		return errors.New("PITR history deletion did not finish; retry while storage is reachable")
	}
	if err = e.stopPITR(ctx, r.Workload); err != nil {
		return err
	}
	name := "cove-pitr-image-" + r.Workload
	id, err := e.pitrContainerID(ctx, name)
	if err != nil {
		return err
	}
	if id != "" {
		owner, err := e.output(ctx, "docker", "inspect", "--format", `{{index .Config.Labels "dev.cove.pitr.owner"}}`, name)
		if err != nil {
			return err
		}
		if owner != state.Config.Owner {
			return errors.New("PITR retained image ownership changed")
		}
		if err = e.Run(ctx, e.Root, []string{"docker", "rm", "--volumes", name}, nil, io.Discard); err != nil {
			return err
		}
	}
	if state.Config.Storage == nil {
		volume := "cove-pitr-archive-" + r.Workload
		exists, err := e.pitrVolumeExists(ctx, volume)
		if err != nil {
			return err
		}
		if exists {
			owner, err := e.output(ctx, "docker", "volume", "inspect", "--format", `{{index .Labels "dev.cove.pitr.owner"}}`, volume)
			if err != nil {
				return err
			}
			if owner != state.Config.Owner {
				return errors.New("PITR archive volume ownership changed")
			}
			if err = e.Run(ctx, e.Root, []string{"docker", "volume", "rm", volume}, nil, io.Discard); err != nil {
				return err
			}
		}
	}
	if err = os.Remove(filepath.Join(e.Root, "workloads", r.Workload, "pitr-bases.json")); err != nil && !os.IsNotExist(err) {
		return err
	}
	return os.RemoveAll(e.pitrDirectory(r.Workload))
}

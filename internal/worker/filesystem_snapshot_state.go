package worker

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path"
	"strings"
	"time"
)

func snapshotIndex(root, owner string) (snapshotHostIndex, error) {
	index := snapshotHostIndex{Owner: owner, Workloads: []string{}}
	err := readSnapshotMetadata(path.Join(snapshotScope(root), "index.json"), &index)
	if os.IsNotExist(err) {
		return index, nil
	}
	if err != nil {
		return index, err
	}
	if index.Owner != owner || len(index.Workloads) > snapshotSessionLimit {
		return index, errors.New("invalid filesystem snapshot index")
	}
	seen := map[string]bool{}
	for _, workload := range index.Workloads {
		if !workloadIdentifier.MatchString(workload) || seen[workload] {
			return index, errors.New("invalid snapshot workload reference")
		}
		seen[workload] = true
	}
	return index, nil
}

func readSnapshotState(root, owner, workload string) (snapshotHostState, error) {
	var state snapshotHostState
	if err := readSnapshotMetadata(snapshotStateFile(root, workload), &state); err != nil {
		return state, err
	}
	if state.Version != 1 || state.Owner != owner || state.Root != root || state.Workload != workload || !identifier.MatchString(state.Operation) || len(state.Resources) > snapshotMountLimit || len(state.Resources) == 0 || len(state.Containers) > 128 || state.CreatedAt.IsZero() || state.ExpiresAt.After(state.CreatedAt.Add(snapshotLease)) || !state.ExpiresAt.After(state.CreatedAt) {
		return state, errors.New("invalid filesystem snapshot recovery state")
	}
	for _, id := range state.Containers {
		if !snapshotContainerID.MatchString(id) {
			return state, errors.New("invalid recovery container identity")
		}
	}
	for i, r := range state.Resources {
		if r.Name != snapshotResourceName(state, i) || r.Mount != path.Join(snapshotScope(root), workload, r.Name) || !snapshotRelative(r.Plan.DataPath) || !r.Plan.Available {
			return state, errors.New("invalid recovery snapshot path")
		}
		p := r.Plan
		if (p.Method == "btrfs" && (!filesystemUUID.MatchString(p.UUID) || !snapshotRelative(p.Subvolume))) || (p.Method == "zfs" && (!filesystemName.MatchString(p.Dataset) || !path.IsAbs(p.DatasetMount))) || (p.Method == "lvm" && (!logicalVolumeName.MatchString(p.VG) || !logicalVolumeName.MatchString(p.LV) || !logicalVolumeUUID.MatchString(p.LVUUID))) {
			return state, errors.New("invalid recovery filesystem identity")
		}
		if p.Method != "btrfs" && p.Method != "zfs" && p.Method != "lvm" {
			return state, errors.New("invalid recovery snapshot method")
		}
	}
	return state, nil
}

func snapshotContainer(ctx context.Context, id, workload string, run snapshotCommand) (bool, error) {
	raw, err := run(ctx, "docker", "inspect", "--format", "{{json .}}", id)
	if err != nil {
		return false, err
	}
	var container struct {
		ID     string `json:"Id"`
		Config struct{ Labels map[string]string }
		State  struct{ Running, Paused bool }
	}
	if json.Unmarshal([]byte(raw), &container) != nil || !strings.HasPrefix(container.ID, id) || container.Config.Labels["com.docker.compose.project"] != "cove-"+workload {
		return false, errors.New("snapshot container ownership changed")
	}
	return container.State.Running && container.State.Paused, nil
}

func unpauseSnapshotContainers(ctx context.Context, state *snapshotHostState, run snapshotCommand) error {
	if !state.Paused {
		return nil
	}
	var result error
	for _, id := range state.Containers {
		paused, err := snapshotContainer(ctx, id, state.Workload, run)
		if err == nil && paused {
			_, err = run(ctx, "docker", "unpause", id)
		}
		result = errors.Join(result, err)
	}
	if result != nil {
		return result
	}
	state.Paused = false
	return writeJSON(snapshotStateFile(state.Root, state.Workload), state)
}

func cleanupSnapshotState(ctx context.Context, state snapshotHostState, run snapshotCommand) error {
	// Stop the exact owned reader before releasing a mount it may still use.
	raw, err := run(ctx, "docker", "ps", "-aq", "--filter", "label=dev.cove.backup-owner="+state.Owner, "--filter", "label=dev.cove.operation="+state.Operation, "--filter", "name=^/cove-backup-"+state.Operation+"$")
	if err != nil {
		return err
	}
	ids := strings.Fields(raw)
	if len(ids) > 1 {
		return errors.New("ambiguous snapshot reader identity")
	}
	if len(ids) == 1 {
		if !snapshotContainerID.MatchString(ids[0]) {
			return errors.New("invalid snapshot reader identity")
		}
		if _, err := run(ctx, "docker", "rm", "-f", ids[0]); err != nil {
			return err
		}
	}
	var result error
	for i := len(state.Resources) - 1; i >= 0; i-- {
		result = errors.Join(result, cleanupSnapshotResource(ctx, state, state.Resources[i], run))
	}
	result = errors.Join(result, unpauseSnapshotContainers(ctx, &state, run))
	if result != nil {
		return result
	}
	if err := os.Remove(snapshotStateFile(state.Root, state.Workload)); err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := os.Remove(path.Join(snapshotScope(state.Root), state.Workload)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func runSnapshotHost(ctx context.Context, request snapshotHostRequest, run snapshotCommand) (response snapshotHostResponse, result error) {
	if request.Action == "recover" {
		if request.Workload != "recovery" || request.Operation != "recovery" {
			return response, errors.New("invalid startup snapshot recovery request")
		}
		return response, RunFilesystemSnapshotReaper(ctx, request.Root, true)
	}
	if request.Action == "pending" {
		index, err := snapshotIndex(request.Root, request.Owner)
		if err != nil {
			return response, err
		}
		for _, workload := range index.Workloads {
			response.Pending = response.Pending || workload == request.Workload
		}
		return response, nil
	}
	if request.Action == "inspect" {
		return inspectSnapshotHost(ctx, request, run)
	}
	if request.Action != "create" && request.Action != "validate" && request.Action != "cleanup" {
		return response, errors.New("unsupported host filesystem action")
	}
	if err := os.MkdirAll(snapshotScope(request.Root), 0700); err != nil {
		return response, err
	}
	unlock, err := lockWorker(snapshotScope(request.Root))
	if err != nil {
		return response, errors.New("filesystem snapshot recovery is busy")
	}
	defer unlock()
	index, err := snapshotIndex(request.Root, request.Owner)
	if err != nil {
		return response, err
	}
	state, err := readSnapshotState(request.Root, request.Owner, request.Workload)
	if err == nil {
		if state.Operation != request.Operation {
			return response, errors.New("an unfinished filesystem snapshot must be recovered first")
		}
		if request.Action == "validate" {
			if !time.Now().Before(state.ExpiresAt) {
				return response, errors.New("filesystem snapshot lease expired")
			}
			for _, resource := range state.Resources {
				if err := validateSnapshotResource(ctx, state, resource, run); err != nil {
					return response, err
				}
			}
			response.Available = true
			return response, nil
		}
		if request.Action == "create" {
			return response, errors.New("filesystem snapshot already exists; recover before retrying")
		}
		if err := cleanupSnapshotState(ctx, state, run); err != nil {
			return response, err
		}
	} else if !os.IsNotExist(err) {
		return response, err
	} else if request.Action == "validate" {
		return response, errors.New("filesystem snapshot recovery state is missing")
	} else if request.Action == "create" {
		if len(index.Workloads) >= snapshotSessionLimit {
			return response, errors.New("filesystem snapshot session limit reached")
		}
		response, err = inspectSnapshotHost(ctx, request, run)
		if err != nil || !response.Available {
			return response, err
		}
		for _, id := range request.Containers {
			paused, err := snapshotContainer(ctx, id, request.Workload, run)
			if err != nil {
				return response, err
			}
			if paused {
				return response, errors.New("a source container was already paused")
			}
		}
		indexed := false
		for _, workload := range index.Workloads {
			indexed = indexed || workload == request.Workload
		}
		if !indexed {
			index.Workloads = append(index.Workloads, request.Workload)
		}
		if err := writeJSON(path.Join(snapshotScope(request.Root), "index.json"), index); err != nil {
			return response, err
		}
		if err := installSnapshotUnits(ctx, request.Root, request.Owner, run); err != nil {
			return response, err
		}
		now := time.Now().UTC()
		state = snapshotHostState{Version: 1, Root: request.Root, Owner: request.Owner, Workload: request.Workload, Operation: request.Operation, CreatedAt: now, ExpiresAt: now.Add(snapshotLease), Containers: request.Containers, Resources: []snapshotHostResource{}}
		for i, plan := range response.Plans {
			name := snapshotResourceName(state, i)
			state.Resources = append(state.Resources, snapshotHostResource{Plan: plan, Name: name, Mount: path.Join(snapshotScope(request.Root), request.Workload, name)})
		}
		if err := writeJSON(snapshotStateFile(request.Root, request.Workload), state); err != nil {
			return response, err
		}
		if len(request.Containers) > 0 {
			state.Paused = true
			if err := writeJSON(snapshotStateFile(request.Root, request.Workload), state); err != nil {
				return response, err
			}
			if _, err := run(ctx, append([]string{"docker", "pause"}, request.Containers...)...); err != nil {
				return response, err
			}
		}
		for i := range state.Resources {
			resource, err := createSnapshotResource(ctx, state, i, snapshotBytes(&VolumeSnapshotConfig{MaxBytes: request.MaxBytes}), run)
			if err != nil {
				return response, err
			}
			state.Resources[i] = resource
			if err := writeJSON(snapshotStateFile(request.Root, request.Workload), state); err != nil {
				return response, err
			}
			response.Sources = append(response.Sources, resource.Source)
		}
		if err := unpauseSnapshotContainers(ctx, &state, run); err != nil {
			return response, err
		}
		return response, nil
	}
	kept := index.Workloads[:0]
	for _, w := range index.Workloads {
		if w != request.Workload {
			kept = append(kept, w)
		}
	}
	index.Workloads = kept
	if err := writeJSON(path.Join(snapshotScope(request.Root), "index.json"), index); err != nil {
		return response, err
	}
	if len(kept) == 0 {
		if err := removeSnapshotUnits(ctx, request.Root, request.Owner, run); err != nil {
			return response, err
		}
	}
	response.Available = true
	return response, nil
}

func RunFilesystemSnapshotReaper(ctx context.Context, root string, force bool) error {
	engine := &Engine{Root: root}
	owner, err := engine.backupHelperOwner(false)
	if err != nil {
		return err
	}
	request := snapshotHostRequest{Root: root, Owner: owner, Workload: "recovery", Operation: "recovery"}
	if err := validateSnapshotHostRequest(request); err != nil {
		return err
	}
	index, err := snapshotIndex(root, owner)
	if err != nil {
		return err
	}
	var result error
	for _, workload := range index.Workloads {
		state, err := readSnapshotState(root, owner, workload)
		if os.IsNotExist(err) {
			state = snapshotHostState{Operation: "recovery"}
		} else if err != nil {
			result = errors.Join(result, err)
			continue
		} else if !force && time.Now().Before(state.ExpiresAt) {
			continue
		}
		request.Workload, request.Operation, request.Action = workload, state.Operation, "cleanup"
		_, err = runSnapshotHost(ctx, request, snapshotHostCommand)
		result = errors.Join(result, err)
	}
	return result
}

func (e *Engine) recoverFilesystemSnapshots(ctx context.Context) error {
	if _, err := os.Lstat(snapshotScope(e.Root)); os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return err
	}
	_, err := e.callSnapshotHost(ctx, snapshotHostRequest{Action: "recover", Workload: "recovery", Operation: "recovery"})
	return err
}

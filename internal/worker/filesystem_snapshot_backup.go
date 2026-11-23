package worker

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"time"
)

func snapshotRequest(r Request, mounts []backupMount, running []string) snapshotHostRequest {
	request := snapshotHostRequest{Workload: r.Workload, Operation: r.ID, MaxBytes: snapshotBytes(r.Backup.VolumeSnapshot), Containers: running}
	for _, mount := range mounts {
		label := mount.Name
		if label == "" {
			label = mount.Source
		}
		request.Sources = append(request.Sources, snapshotSource{Source: mount.Source, Label: label})
	}
	return request
}

func (e *Engine) SnapshotPreview(ctx context.Context, workload string, maxBytes int64) (SnapshotPreview, error) {
	preview := SnapshotPreview{Mounts: []SnapshotCapability{}}
	if !workloadIdentifier.MatchString(workload) {
		return preview, errors.New("invalid workload")
	}
	if err := validateVolumeSnapshot(&VolumeSnapshotConfig{MaxBytes: maxBytes}); err != nil {
		return preview, err
	}
	e.mu.Lock()
	busy := e.busy(workload) || e.draining || len(e.active) >= 4
	for _, op := range e.pending {
		busy = busy || op.Workload == workload
	}
	if busy {
		e.mu.Unlock()
		return preview, errors.New("wait for active operations before snapshot inspection")
	}
	if e.active == nil {
		e.active = map[string]bool{}
	}
	e.active[workload] = true
	e.mu.Unlock()
	defer func() { e.mu.Lock(); delete(e.active, workload); e.mu.Unlock() }()
	mounts, _, err := e.backupMounts(ctx, workload)
	if err != nil {
		return preview, err
	}
	request := snapshotHostRequest{Action: "inspect", Workload: workload, Operation: "preview", MaxBytes: maxBytes}
	for _, mount := range mounts {
		label := mount.Name
		if label == "" {
			label = mount.Source
		}
		request.Sources = append(request.Sources, snapshotSource{Source: mount.Source, Label: label})
	}
	response, err := e.callSnapshotHost(ctx, request)
	if err != nil {
		return preview, err
	}
	preview.Available, preview.Reason = response.Available, response.Reason
	for _, plan := range response.Plans {
		preview.Mounts = append(preview.Mounts, SnapshotCapability{Mount: plan.Label, Filesystem: plan.Filesystem, Method: plan.Method, Available: plan.Available, Reason: plan.Reason})
	}
	return preview, nil
}

func (e *Engine) performFilesystemBackup(ctx context.Context, r Request, mounts []backupMount, running []string, current deployedState, out io.Writer) (handled bool, reason string, result error) {
	request := snapshotRequest(r, mounts, running)
	request.Action = "inspect"
	owner, err := e.backupHelperOwner(false)
	if err != nil {
		return true, "", err
	}
	index, err := snapshotIndex(e.Root, owner)
	if err != nil {
		return true, "", err
	}
	for _, workload := range index.Workloads {
		if workload == r.Workload {
			return true, "", errors.New("restart the worker to recover the pending filesystem snapshot before another backup")
		}
	}
	preview, err := e.callSnapshotHost(ctx, request)
	if err != nil {
		return false, "Host snapshot inspection is unavailable; check host tools and privileges", nil
	}
	if !preview.Available {
		return false, preview.Reason, nil
	}
	if err := e.settleDatabaseSnapshot(ctx, r); err != nil {
		return true, "", err
	}
	journal := filepath.Join(e.Root, "workloads", r.Workload, "database-snapshot.json")
	pending := databaseSnapshotJournal{Operation: r.ID, Repository: r.Backup.Repository}
	if err := writeJSON(journal, pending); err != nil {
		return true, "", err
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Minute)
		defer cancel()
		request.Action = "cleanup"
		_, err := e.callSnapshotHost(cleanup, request)
		result = errors.Join(result, err, e.settleDatabaseSnapshot(cleanup, r))
	}()
	request.Action = "create"
	response, err := e.callSnapshotHost(ctx, request)
	if err != nil {
		return true, "", err
	}
	if !response.Available || len(response.Sources) != len(mounts) {
		return true, "", errors.New("filesystem snapshot source count changed")
	}
	methods := []string{}
	seen := map[string]bool{}
	r.snapshotMounts = append([]backupMount{}, mounts...)
	for i, source := range response.Sources {
		if !filepath.IsAbs(source) {
			return true, "", errors.New("invalid snapshot source")
		}
		r.snapshotMounts[i].Source = source
		r.snapshotMounts[i].Type, r.snapshotMounts[i].Name = "bind", ""
		method := preview.Plans[i].Method
		if !seen[method] {
			methods = append(methods, method)
			seen[method] = true
		}
	}
	r.volumeMethod = strings.Join(methods, "+")
	r.snapshotCheck = func(c context.Context) error {
		request.Action = "validate"
		_, err := e.callSnapshotHost(c, request)
		return err
	}
	if _, err := e.makeBackup(ctx, r, mounts, current, out); err != nil {
		return true, "", err
	}
	pending.Complete = true
	return true, "", writeJSON(journal, pending)
}

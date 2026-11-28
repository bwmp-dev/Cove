package worker

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

type JournalCandidate struct {
	ID       string `json:"id"`
	Workload string `json:"workload"`
}
type JournalPruneRequest struct {
	Candidates []JournalCandidate `json:"candidates"`
}
type JournalPruneResult struct {
	Pruned    []string `json:"pruned"`
	Protected int      `json:"protected"`
}

func (e *Engine) PruneJournals(ctx context.Context, in JournalPruneRequest) (*JournalPruneResult, error) {
	if len(in.Candidates) > 500 {
		return nil, errors.New("journal pruning is limited to 500 records")
	}
	candidates := map[string]bool{}
	for _, c := range in.Candidates {
		if !workloadIdentifier.MatchString(c.ID) || !workloadIdentifier.MatchString(c.Workload) {
			return nil, errors.New("invalid journal candidate")
		}
		candidates[c.ID] = true
	}
	e.mu.Lock()
	if e.draining || e.hasMoveFence() || len(e.active) > 0 || len(e.pending) > 0 || len(e.recovering) > 0 {
		e.mu.Unlock()
		return nil, errors.New("worker operations or recovery prevent journal pruning")
	}
	e.active[housekeepingWorkload] = true
	e.mu.Unlock()
	defer func() { e.mu.Lock(); delete(e.active, housekeepingWorkload); e.mu.Unlock() }()
	ownedSnapshots := map[string]string{}
	snapshotNames := map[string]string{}
	for _, candidate := range in.Candidates {
		for _, name := range operationComposeSnapshotNames(candidate.ID) {
			ownedSnapshots[filepath.Join(candidate.Workload, name)] = candidate.ID
			snapshotNames[name] = candidate.ID
		}
	}
	protected := map[string]bool{}
	var inspectValue func(any, string)
	inspectValue = func(value any, ownOperation string) {
		switch value := value.(type) {
		case string:
			for _, part := range strings.FieldsFunc(value, func(r rune) bool { return r == '/' || r == '\\' }) {
				if candidates[part] && part != ownOperation {
					protected[part] = true
				}
			}
			base := filepath.Base(value)
			if id := snapshotNames[base]; id != "" && id != ownOperation {
				protected[id] = true
			}
			for id := range candidates {
				if id != ownOperation && (value == id || base == id || strings.HasPrefix(base, id+".") || base == id+"-links.yaml" || base == id+"-private.yaml") {
					protected[id] = true
				}
			}
		case map[string]any:
			for _, v := range value {
				inspectValue(v, ownOperation)
			}
		case []any:
			for _, v := range value {
				inspectValue(v, ownOperation)
			}
		}
	}
	count, bytes := 0, int64(0)
	root := filepath.Join(e.Root, "workloads")
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		parts := strings.Split(relative, string(filepath.Separator))
		if entry.Type()&os.ModeSymlink != 0 {
			return errors.New("unexpected symlink in worker metadata")
		}
		if entry.IsDir() {
			if len(parts) > 2 {
				return filepath.SkipDir
			}
			if len(parts) > 1 && relative != "." && parts[1] != "release-states" && parts[1] != "backups" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(entry.Name(), ".json") {
			return nil
		}
		count++
		if count > 200000 {
			return errors.New("worker reference inventory exceeds pruning limit")
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		bytes += info.Size()
		if info.Size() > 4*1024*1024 || bytes > 512*1024*1024 {
			return errors.New("worker reference inventory exceeds pruning byte limit")
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		var value any
		if json.Unmarshal(data, &value) != nil {
			return errors.New("invalid worker reference metadata prevents pruning")
		}
		if len(parts) > 2 && (parts[1] == "release-states" || parts[1] == "backups") {
			inspectValue(strings.TrimSuffix(entry.Name(), ".json"), "")
		}
		// Operation-owned snapshots can contain their own asset paths without being live references.
		ownOperation := ownedSnapshots[relative]
		inspectValue(value, ownOperation)
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	entries, err := os.ReadDir(filepath.Join(e.Root, "operations"))
	if err != nil {
		return nil, err
	}
	if len(entries) > 400000 {
		return nil, errors.New("journal inventory exceeds pruning limit")
	}
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		j, err := e.read(strings.TrimSuffix(entry.Name(), ".json"))
		if err != nil {
			return nil, err
		}
		if j.Operation.FinishedAt == nil || j.RouteChange != nil && !j.RouteChange.Committed || j.Request.DatabaseMaintenance != nil && j.Operation.State != "succeeded" {
			protected[j.Operation.ID] = true
			data, _ := json.Marshal(j)
			var value any
			if json.Unmarshal(data, &value) != nil {
				return nil, errors.New("invalid recovery journal")
			}
			inspectValue(value, "")
		}
	}
	result := &JournalPruneResult{Pruned: []string{}}
	for _, c := range in.Candidates {
		if ctx.Err() != nil {
			return result, ctx.Err()
		}
		if protected[c.ID] {
			result.Protected++
			continue
		}
		j, err := e.read(c.ID)
		if err == nil {
			if j.Operation.Workload != c.Workload || j.Operation.FinishedAt == nil || (j.Operation.State != "succeeded" && j.Operation.State != "failed" && j.Operation.State != "interrupted" && j.Operation.State != "cancelled") {
				result.Protected++
				continue
			}
		} else if !os.IsNotExist(err) {
			return result, err
		}
		if err = e.pruneOperationComposeBundle(ctx, c); err != nil {
			return result, err
		}
		if err = os.Remove(e.path(c.ID)); err != nil && !os.IsNotExist(err) {
			return result, err
		}
		if err = os.Remove(filepath.Join(e.Root, "operations", c.ID+".log")); err != nil && !os.IsNotExist(err) {
			return result, err
		}
		result.Pruned = append(result.Pruned, c.ID)
	}
	return result, nil
}

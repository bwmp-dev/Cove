package worker

import (
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

func (e *Engine) pruneIdleOperations() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	entries, err := os.ReadDir(filepath.Join(e.Root, "operations"))
	if err != nil {
		return err
	}
	groups := map[string][]Operation{}
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		id := strings.TrimSuffix(entry.Name(), ".json")
		if !strings.HasPrefix(id, "idle-") && !strings.HasPrefix(id, "wake-idle-") {
			continue
		}
		j, err := e.read(id)
		if err != nil {
			return err
		}
		if j.Operation.Action != "idle_sleep" && j.Operation.Action != "idle_wake" || j.Operation.FinishedAt == nil || j.Operation.State == "running" || j.Operation.State == "queued" {
			continue
		}
		if _, recovering := e.recovering[id]; recovering {
			continue
		}
		groups[j.Operation.Workload] = append(groups[j.Operation.Workload], j.Operation)
	}
	for workload, operations := range groups {
		sort.Slice(operations, func(i, j int) bool { return operations[i].CreatedAt.After(operations[j].CreatedAt) })
		suspension, err := e.readSuspension(workload)
		if err != nil {
			return err
		}
		keep := map[string]bool{}
		if suspension != nil {
			keep[suspension.Operation], keep[suspension.idleEpisode()], keep["wake-"+suspension.idleEpisode()] = true, true, true
			if suspension.BeforeIdle != nil {
				keep[suspension.BeforeIdle.Operation], keep["wake-"+suspension.BeforeIdle.Operation] = true, true
			}
		}
		for index, operation := range operations {
			if index < 100 || keep[operation.ID] {
				continue
			}
			if err := os.Remove(filepath.Join(e.Root, "operations", operation.ID+".log")); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
			if err := os.Remove(e.path(operation.ID)); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
	}
	return syncDirectory(filepath.Join(e.Root, "operations"))
}

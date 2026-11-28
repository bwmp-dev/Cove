package worker

import (
	"os"
	"path/filepath"
)

type Maintenance struct {
	Draining bool `json:"draining"`
	Active   int  `json:"active"`
	Queued   int  `json:"queued"`
}

// The same mutex guards dispatch so zero active operations is a safe stop boundary.
func (e *Engine) Maintenance(drain bool) (Maintenance, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	path := filepath.Join(e.Root, "draining.json")
	if drain {
		if err := writeJSON(path, true); err != nil {
			return Maintenance{}, err
		}
	} else {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return Maintenance{}, err
		}
		if err := syncDirectory(e.Root); err != nil {
			return Maintenance{}, err
		}
	}
	e.draining = drain
	active := len(e.active) + e.recoveringWorkloadCount()
	if e.proxyRunning || e.proxyUnsettled || e.customRunning || e.customUnsettled || e.hostChanging {
		active++
	}
	if err := e.pendingNodeMoves(); err != nil {
		active++
	}
	if _, err := os.Lstat(filepath.Join(e.Root, "node-changes")); !os.IsNotExist(err) {
		unlock, err := nodeChangeLease(e.Root, false)
		if err != nil {
			active++
		} else {
			unlock()
		}
	}
	return Maintenance{Draining: drain, Active: active, Queued: len(e.pending)}, nil
}

func (e *Engine) recoveringWorkloadCount() int {
	count := 0
	for _, recovery := range e.recovering {
		if recovery.activeSlot {
			count++
		}
	}
	return count
}

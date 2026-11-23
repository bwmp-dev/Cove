package worker

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"
)

func terminalMove(phase string) bool {
	return slices.Contains([]string{"committed", "rolled_back", "departed", "discarded"}, phase)
}

func (e *Engine) moveReceipts() ([]moveJournal, error) {
	entries, err := os.ReadDir(filepath.Join(e.Root, "moves"))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if len(entries) > 1024 {
		return nil, errors.New("move staging inventory exceeds its bound; inspect worker storage")
	}
	result := []moveJournal{}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		workload := strings.TrimSuffix(entry.Name(), ".json")
		j, err := e.readMove(workload)
		if err != nil {
			return nil, err
		}
		result = append(result, j)
	}
	return result, nil
}

func (e *Engine) hasMoveFence() bool {
	records, err := e.moveReceipts()
	if err != nil {
		return true
	}
	for _, j := range records {
		if !terminalMove(j.Phase) {
			return true
		}
	}
	return false
}

func (e *Engine) admitMoveReceipt() error {
	records, err := e.moveReceipts()
	if err != nil {
		return err
	}
	active := 0
	sort.Slice(records, func(i, j int) bool { return records[i].UpdatedAt.After(records[j].UpdatedAt) })
	retained := 0
	for _, j := range records {
		if !terminalMove(j.Phase) {
			active++
			continue
		}
		retained++
		if retained <= 256 && time.Since(j.UpdatedAt) < 30*24*time.Hour {
			continue
		}
		if err = os.RemoveAll(e.moveDirectory(j.ID)); err != nil {
			return err
		}
		if err = os.Remove(e.movePath(j.Workload)); err != nil {
			return err
		}
	}
	if active >= 2 {
		return errors.New("at most two unfinished moves can be held by one worker")
	}
	return nil
}

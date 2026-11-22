package control

import (
	"cove/internal/worker"
	"encoding/json"
)

func pushMatchesWatchPaths(body []byte, patterns []string) bool {
	if len(patterns) == 0 {
		return true
	}
	var event struct {
		Forced  bool `json:"forced"`
		Created bool `json:"created"`
		Size    *int `json:"size"`
		Commits []struct {
			Added    *[]string `json:"added"`
			Modified *[]string `json:"modified"`
			Removed  *[]string `json:"removed"`
		} `json:"commits"`
	}
	// GitHub caps push payloads at 2048 commits. Missing file lists are unknown,
	// not evidence that nothing changed.
	if json.Unmarshal(body, &event) != nil || event.Forced || event.Created || len(event.Commits) == 0 || len(event.Commits) >= 2048 || event.Size != nil && *event.Size != len(event.Commits) {
		return true
	}
	var files []string
	for _, commit := range event.Commits {
		if commit.Added == nil || commit.Modified == nil || commit.Removed == nil {
			return true
		}
		files = append(files, (*commit.Added)...)
		files = append(files, (*commit.Modified)...)
		files = append(files, (*commit.Removed)...)
	}
	return worker.WatchPathsMatch(patterns, files)
}

func filterWebhookSequencePaths(requests []worker.Request, predecessors map[string]string, matched map[string]bool) []worker.Request {
	// An ordered sequence is a single deployment unit: when any member changes,
	// retain its migration and prerequisite steps even if their own paths did not.
	for changed := true; changed; {
		changed = false
		for child, parent := range predecessors {
			if matched[child] != matched[parent] {
				matched[child], matched[parent] = true, true
				changed = true
			}
		}
	}
	out := make([]worker.Request, 0, len(requests))
	for _, request := range requests {
		if matched[request.Workload] {
			out = append(out, request)
		}
	}
	return out
}

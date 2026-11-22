package control

import (
	"cove/internal/worker"
	"testing"
)

func TestPushWatchPaths(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		want       bool
	}{
		{"changed", `{"commits":[{"added":["src/app.go"],"modified":[],"removed":[]}]}`, true},
		{"removed", `{"commits":[{"added":[],"modified":[],"removed":["src/old.go"]}]}`, true},
		{"excluded", `{"commits":[{"added":["src/app_test.go"],"modified":[],"removed":[]}]}`, false},
		{"unrelated", `{"commits":[{"added":["README.md"],"modified":[],"removed":[]}]}`, false},
		{"missing files", `{"commits":[{"added":["README.md"]}]}`, true},
		{"missing commits", `{}`, true},
		{"truncated", `{"size":2,"commits":[{"added":[],"modified":[],"removed":[]}]}`, true},
		{"forced", `{"forced":true,"commits":[{"added":[],"modified":[],"removed":[]}]}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := pushMatchesWatchPaths([]byte(tc.body), []string{"src/**", "!**/*_test.go"}); got != tc.want {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestWatchPathsKeepWholeSequence(t *testing.T) {
	requests := []worker.Request{{Workload: "migration"}, {Workload: "app"}, {Workload: "warmup"}, {Workload: "other"}}
	got := filterWebhookSequencePaths(requests, map[string]string{"app": "migration", "warmup": "app"}, map[string]bool{"app": true})
	if len(got) != 3 {
		t.Fatalf("sequence lost members: %v", got)
	}
	got = filterWebhookSequencePaths(requests, map[string]string{"app": "migration", "warmup": "app"}, map[string]bool{})
	if len(got) != 0 {
		t.Fatal("unchanged sequence deployed")
	}
}

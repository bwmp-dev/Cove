package worker

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestPITRCopyCleanupRetainsJournalWhenDockerIsUnavailableOrForeign(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("private Unix metadata permissions require Linux")
	}
	for _, mode := range []string{"unavailable", "foreign"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			if err := os.Mkdir(filepath.Join(root, "pitr-recovery"), 0700); err != nil {
				t.Fatal(err)
			}
			journal := filepath.Join(root, "pitr-recovery", "copy.json")
			if err := writeJSON(journal, pitrCopyRecovery{Workload: "copy-resource", Owner: strings.Repeat("a", 32)}); err != nil {
				t.Fatal(err)
			}
			mutated := false
			e := &Engine{Root: root, Run: func(_ context.Context, _ string, args, _ []string, out io.Writer) error {
				if len(args) > 2 && args[1] == "ps" {
					if mode == "unavailable" {
						return errors.New("fixture disconnect")
					}
					io.WriteString(out, strings.Repeat("b", 12))
					return nil
				}
				if len(args) > 2 && args[1] == "inspect" {
					io.WriteString(out, strings.Repeat("c", 32))
					return nil
				}
				mutated = true
				return nil
			}}
			if e.cleanupPITRCopy(context.Background(), "copy") == nil {
				t.Fatal("unsafe cleanup succeeded")
			}
			if mutated {
				t.Fatal("unsafe cleanup attempted a Docker mutation")
			}
			if _, err := os.Stat(journal); err != nil {
				t.Fatal("recovery reference was discarded", err)
			}
		})
	}
}

func TestPITRStorageIdentityRejectsChangedMountBeforeDockerAccess(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("private Unix metadata permissions require Linux")
	}
	root := t.TempDir()
	workload := "postgres"
	directory := filepath.Join(root, "pitr", workload)
	if err := os.MkdirAll(directory, 0700); err != nil {
		t.Fatal(err)
	}
	database := pitrDatabase{Host: "/source/.cove-pitr-socket", Port: 5432, User: "postgres", Password: "synthetic-password", SystemID: "12345", Slot: "cove_test", Major: 17}
	config := pitrRunnerConfig{Workload: workload, Owner: strings.Repeat("a", 32), Policy: PITRPolicy{Key: strings.Repeat("b", 64), Archive: strings.Repeat("c", 32), Settings: PITRSettings{Service: "postgres", IntervalHours: 24, KeepLast: 2, RetentionDays: 7, MaxBytes: 1 << 30}}, Database: database}
	source := pitrSource{Container: strings.Repeat("d", 12), Image: "sha256:" + strings.Repeat("e", 64), Mount: backupMount{Type: "volume", Name: "data", Destination: "/var/lib/postgresql/data"}, Data: "/var/lib/postgresql/data", UID: "70", GID: "70", Database: database}
	state := pitrState{Config: config, Source: source, Operation: "configure", Bases: map[string]string{}}
	filename := filepath.Join(directory, "state.json")
	if err := writeJSON(filename, state); err != nil {
		t.Fatal(err)
	}
	e := &Engine{Root: root}
	if _, err := e.loadPITR(workload); err != nil {
		t.Fatal("valid storage identity refused", err)
	}
	state.Source.Relative = "../../another-resource"
	if err := writeJSON(filename, state); err != nil {
		t.Fatal(err)
	}
	if _, err := e.loadPITR(workload); err == nil {
		t.Fatal("changed storage identity accepted")
	}
}

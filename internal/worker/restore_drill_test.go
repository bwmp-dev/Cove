package worker

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestVolumeRestoreDrillNeverMountsProductionStorage(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "verified", true: "restore-failed"}[fail], func(t *testing.T) {
			root := t.TempDir()
			dir := filepath.Join(root, "workloads", "test", "backups")
			if err := os.MkdirAll(dir, 0700); err != nil {
				t.Fatal(err)
			}
			manifest := backupManifest{Record: BackupRecord{ID: "backup", Workload: "test", Repository: "local", Snapshot: strings.Repeat("a", 64), Created: time.Now()}, Mounts: []backupMount{{Type: "volume", Name: "production-data", Source: "/production/data"}}}
			if err := writeJSON(filepath.Join(dir, "backup.json"), manifest); err != nil {
				t.Fatal(err)
			}
			restored := false
			e := &Engine{Root: root, Run: func(ctx context.Context, dir string, args, env []string, out io.Writer) error {
				joined := strings.Join(args, " ")
				if strings.Contains(joined, "production-data") || strings.Contains(joined, "/production/data") {
					t.Fatal("production storage mounted", args)
				}
				switch {
				case strings.HasPrefix(joined, "docker volume inspect"):
					_, err := io.WriteString(out, "test")
					return err
				case strings.HasPrefix(joined, "docker ps -aq --filter label=dev.cove."):
					return nil
				case strings.Contains(joined, "restic/restic:0.19.1 --no-cache check --read-data"):
					return nil
				case strings.Contains(joined, "restic/restic:0.19.1 --no-cache restore "):
					restored = true
					if !strings.Contains(joined, filepath.Join(root, "drills", "drill", "000")) || !strings.Contains(joined, "--verify") {
						t.Fatal("restore did not use isolated verified storage", args)
					}
					if fail {
						return errors.New("fixture restore failure")
					}
					return nil
				default:
					t.Fatalf("unexpected command %v", args)
					return nil
				}
			}}
			err := e.performDrill(context.Background(), Request{ID: "drill", Workload: "test", Action: "drill", BackupID: "backup", Backup: &BackupConfig{Repository: "local", Password: "fixture-password-long", KeepLast: 3}}, io.Discard)
			if (err != nil) != fail || !restored {
				t.Fatal(err, restored)
			}
			records, err := e.Backups("test")
			if err != nil || len(records) != 1 || (records[0].VerifiedAt != nil) == fail {
				t.Fatal(records, err)
			}
			entries, err := os.ReadDir(filepath.Join(root, "drills"))
			if err != nil || len(entries) != 0 {
				t.Fatal("drill was not cleaned", entries, err)
			}
		})
	}
}

func TestDrillRecoveryRetainsJournalUntilCleanupSucceeds(t *testing.T) {
	root := t.TempDir()
	stage := filepath.Join(root, "drills", "interrupted")
	if err := os.MkdirAll(stage, 0700); err != nil {
		t.Fatal(err)
	}
	journal := filepath.Join(root, "drills", "interrupted.json")
	if err := writeJSON(journal, map[string]string{"workload": "test"}); err != nil {
		t.Fatal(err)
	}
	broken := true
	removed := false
	e := &Engine{Root: root, Run: func(ctx context.Context, dir string, args, env []string, out io.Writer) error {
		if broken {
			return errors.New("Docker unavailable")
		}
		joined := strings.Join(args, " ")
		if strings.HasPrefix(joined, "docker ps -aq") {
			if strings.Contains(joined, "dev.cove.drill=") {
				_, err := io.WriteString(out, "owned-container")
				return err
			}
			return nil
		}
		if joined == "docker rm --force --volumes owned-container" {
			removed = true
			return nil
		}
		t.Fatalf("unexpected cleanup %v", args)
		return nil
	}}
	if e.recoverDrills(context.Background()) == nil {
		t.Fatal("ignored cleanup failure")
	}
	if _, err := os.Stat(journal); err != nil {
		t.Fatal("lost recovery journal", err)
	}
	broken = false
	if err := e.recoverDrills(context.Background()); err != nil || !removed {
		t.Fatal(err, removed)
	}
	if _, err := os.Stat(stage); !os.IsNotExist(err) {
		t.Fatal("temporary data retained", err)
	}
}

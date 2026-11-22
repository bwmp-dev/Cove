package worker

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestImportedBackupMounts(t *testing.T) {
	e, err := NewEngine(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	imported := filepath.Join(e.Root, "imported-data", "resource", "data")
	if err := os.MkdirAll(imported, 0700); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(imported, "settings.json")
	if err := os.WriteFile(file, []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name, source string
		allowed      bool
	}{
		{"imported directory", imported, true},
		{"imported file", file, true},
		{"missing imported data", filepath.Join(imported, "missing"), false},
		{"worker root", e.Root, false},
		{"worker secrets", filepath.Join(e.Root, "workloads", "other", "current.json"), false},
		{"import root", filepath.Join(e.Root, "imported-data"), false},
		{"similar prefix", filepath.Join(e.Root, "imported-data-other", "data"), false},
	}
	link := filepath.Join(imported, "escape")
	if err := os.Symlink(e.Root, link); err == nil {
		cases = append(cases, struct {
			name, source string
			allowed      bool
		}{"symlink escape", link, false})
	} else {
		t.Logf("symlink creation unavailable: %v", err)
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e.Run = func(_ context.Context, _ string, args, _ []string, out io.Writer) error {
				if args[1] == "inspect" {
					return json.NewEncoder(out).Encode([]backupMount{{Type: "bind", Source: tc.source, RW: true}})
				}
				if args[1] == "ps" && args[2] == "-q" {
					_, err := io.WriteString(out, "container\n")
					return err
				}
				_, err := io.WriteString(out, `{"ID":"container","State":"running"}`)
				return err
			}
			mounts, running, err := e.backupMounts(context.Background(), "resource")
			if (err == nil) != tc.allowed {
				t.Fatalf("allowed=%v err=%v", tc.allowed, err)
			}
			if tc.allowed && (len(mounts) != 1 || mounts[0].Source != tc.source || len(running) != 1) {
				t.Fatalf("mounts=%v running=%v", mounts, running)
			}
		})
	}
}

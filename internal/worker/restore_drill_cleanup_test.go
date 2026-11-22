package worker

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"testing"
)

func TestDrillCleanupUsesCachedBackupImage(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("requires Unix permissions and a non-root user")
	}
	for _, remote := range []bool{false, true} {
		name := "local"
		if remote {
			name = "remote"
		}
		t.Run(name, func(t *testing.T) {
			e, err := NewEngine(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			stage := filepath.Join(e.Root, "drills", "fixture")
			data := filepath.Join(stage, "000")
			if err := os.MkdirAll(data, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(data, "file"), []byte("fixture"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(data, 0500); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chmod(data, 0700) })
			journal := filepath.Join(e.Root, "drills", "fixture.json")
			if err := os.WriteFile(journal, []byte("{}"), 0600); err != nil {
				t.Fatal(err)
			}
			used := false
			e.Run = func(_ context.Context, _ string, args, _ []string, _ io.Writer) error {
				switch args[1] {
				case "ps":
					return nil
				case "image":
					if !remote {
						return errors.New("remote helper absent")
					}
					return nil
				case "run":
					image := "restic/restic:0.19.1"
					if remote {
						image = remoteBackupImage
					}
					if !slices.Contains(args, image) || !slices.Contains(args, "type=bind,source="+stage+",target=/drill") {
						t.Fatalf("unexpected cleanup image or mount: %v", args)
					}
					used = true
					return os.Chmod(data, 0700)
				default:
					t.Fatalf("unexpected command: %v", args)
					return nil
				}
			}
			if err := e.cleanupDrill(context.Background(), "fixture"); err != nil {
				t.Fatal(err)
			}
			if !used {
				t.Fatal("permission cleanup was not exercised")
			}
			for _, path := range []string{stage, journal} {
				if _, err := os.Stat(path); !os.IsNotExist(err) {
					t.Fatalf("cleanup left %s: %v", path, err)
				}
			}
		})
	}
}

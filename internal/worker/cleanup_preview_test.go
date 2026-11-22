package worker

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCleanupPreviewProtectsRecentAndUsedImages(t *testing.T) {
	root := t.TempDir()
	owner := strings.Repeat("a", 32)
	if err := writeJSON(filepath.Join(root, "build-owner.json"), owner); err != nil {
		t.Fatal(err)
	}
	old, used, recent := "sha256:"+strings.Repeat("1", 64), "sha256:"+strings.Repeat("2", 64), "sha256:"+strings.Repeat("3", 64)
	e := &Engine{Root: root, Run: func(ctx context.Context, dir string, args, env []string, out io.Writer) error {
		command := strings.Join(args, " ")
		switch {
		case strings.HasPrefix(command, "docker image ls"):
			if !strings.Contains(command, "dangling=true") || !strings.Contains(command, "label=dev.cove.build-owner="+owner) {
				t.Fatal(command)
			}
			fmt.Fprintln(out, old, used, recent, old)
		case strings.HasPrefix(command, "docker image inspect"):
			id := args[len(args)-1]
			created := time.Now().Add(-200 * time.Hour)
			if id == recent {
				created = time.Now()
			}
			fmt.Fprintf(out, "%s %s 1024", id, created.Format(time.RFC3339Nano))
		case strings.HasPrefix(command, "docker ps -aq"):
			if strings.HasSuffix(command, used) {
				fmt.Fprint(out, "container")
			}
		default:
			t.Fatal("preview mutated Docker", command)
		}
		return nil
	}}
	report, err := e.PreviewCleanup(context.Background())
	if err != nil || len(report.Images) != 1 || report.Images[0].ID != old || report.Bytes != 1024 {
		t.Fatal(report, err)
	}
	e.Root = t.TempDir()
	report, err = e.PreviewCleanup(context.Background())
	if err != nil || report.Managed || len(report.Images) != 0 {
		t.Fatal(report, err)
	}
}

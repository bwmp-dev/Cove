package worker

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSourceFilesReadPinnedSelectionWithoutDockerOrRetainedCheckout(t *testing.T) {
	engine, err := NewEngine(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	commit := strings.Repeat("a", 40)
	request := Request{ID: "read-source", Workload: "app", Action: "deploy", Source: &GitSource{Repository: "https://fixture.invalid/app.git", Branch: "main", ComposePath: "cove.yaml", Revision: commit, Token: "fixture-private-token"}, ComposeBundle: &ComposeBundle{Paths: []string{"production.yaml"}}}
	fetched, pinned := false, false
	engine.Run = func(_ context.Context, directory string, args, env []string, output io.Writer) error {
		if args[0] != "git" || strings.Contains(strings.Join(args, " "), request.Source.Token) {
			t.Fatal("source inspection invoked Docker or exposed a clone credential")
		}
		if strings.Contains(strings.Join(args, " "), " clone ") {
			destination := args[len(args)-1]
			if err := os.MkdirAll(destination, 0700); err != nil {
				return err
			}
			for name, content := range map[string]string{"cove.yaml": "api_version: cove/v1\nproject: App\nenvironments: []\nresources: []\n", "production.yaml": "services: {}\n"} {
				if err := os.WriteFile(filepath.Join(destination, name), []byte(content), 0600); err != nil {
					return err
				}
			}
		}
		if strings.Contains(strings.Join(args, " "), " fetch ") {
			fetched = args[len(args)-1] == commit
		}
		if args[1] == "checkout" {
			pinned = args[len(args)-1] == commit
		}
		if args[1] == "rev-parse" {
			_, err := io.WriteString(output, commit+"\n")
			return err
		}
		return nil
	}
	files, err := engine.ReadSourceFiles(context.Background(), request)
	if err != nil || !fetched || !pinned || files.Revision != commit || len(files.Files) != 2 || !strings.Contains(files.Files["cove.yaml"], "cove/v1") {
		t.Fatal("source selection was not pinned", err)
	}
	entries, err := os.ReadDir(engine.Root)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "source-files-") {
			t.Fatal("source inspection retained checkout")
		}
	}
}

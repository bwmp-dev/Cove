package control

import (
	"context"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func runWorkloadMoveBrowser(t *testing.T, f *accountFixture, resource, target string) {
	t.Helper()
	script, dist := os.Getenv("COVE_MOVE_BROWSER_SCRIPT"), os.Getenv("COVE_MOVE_BROWSER_DIST")
	if script == "" || dist == "" {
		t.Fatal("real move browser requires a frozen production build and the Opus UI script")
	}
	if _, err := os.Stat(filepath.Join(dist, "index.html")); err != nil {
		t.Fatal(err)
	}
	f.s.FrontendDir = dist
	server := httptest.NewTLSServer(f.handler)
	defer server.Close()
	previous := f.s.PublicURL
	f.s.PublicURL = server.URL
	defer func() { f.s.PublicURL = previous }()
	cookie := f.user("move-browser-owner", "move-browser-owner@example.com")
	f.sql(`INSERT INTO team_members(team_id,user_id,role) VALUES('default','move-browser-owner','owner')`)
	shots := os.Getenv("COVE_MOVE_BROWSER_SHOTS")
	if shots == "" {
		var err error
		shots, err = os.MkdirTemp("", "cove-move-browser-")
		if err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		for {
			if err := f.s.dispatchOperations(ctx); err != nil && ctx.Err() == nil {
				done <- err
				cancel()
				return
			}
			select {
			case <-ctx.Done():
				done <- nil
				return
			case <-time.After(200 * time.Millisecond):
			}
		}
	}()
	command := exec.CommandContext(ctx, "node", script)
	command.Env = append(os.Environ(), "COVE_MOVE_BROWSER_URL="+server.URL, "COVE_MOVE_BROWSER_COOKIE="+cookie, "COVE_MOVE_BROWSER_RESOURCE="+resource, "COVE_MOVE_BROWSER_TARGET="+target, "COVE_MOVE_BROWSER_SHOTS="+shots)
	output, err := command.CombinedOutput()
	cancel()
	dispatchErr := <-done
	t.Log(string(output))
	t.Log("move browser artifacts", shots)
	if dispatchErr != nil {
		t.Fatal("move dispatcher failed during browser acceptance", dispatchErr)
	}
	if err != nil {
		t.Fatal("real move browser acceptance failed", err)
	}
}

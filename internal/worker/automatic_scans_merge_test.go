package worker

import (
	"context"
	"cove/internal/outbound"
	"errors"
	"io"
	"slices"
	"strings"
	"sync"
	"testing"
)

func TestAutomaticScansMergeRefreshUsesRPCProxy(t *testing.T) {
	t.Setenv("COVE_IMAGE_SCANNING", "1")
	e, err := NewEngine(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	proxy := &outbound.Config{HTTP: "http://fixture:opaque@proxy.example.invalid:8080", HTTPS: "http://fixture:opaque@proxy.example.invalid:8080", NoProxy: "localhost"}
	started := false
	e.Run = func(_ context.Context, _ string, args, env []string, _ io.Writer) error {
		if len(args) > 2 && args[1] == "image" && args[2] == "inspect" || args[1] == "ps" {
			return nil
		}
		if args[1] != "run" {
			t.Fatal("unexpected Docker action", args[1])
		}
		started = true
		if !slices.Equal(env, proxy.Env()) || strings.Contains(strings.Join(args, " "), "opaque") {
			t.Fatal("refresh lost the RPC proxy or placed its credential in arguments")
		}
		return errors.New("fixture stopped before refresh")
	}
	response := e.RPC(t.Context(), RPCRequest{Method: "image_scan_database", RefreshScanDatabase: true, Proxy: proxy})
	if response.Error != "" || response.ScanDatabase == nil || !response.ScanDatabase.Refreshing {
		t.Fatal("refresh request did not persist", response.Error)
	}
	var jobs sync.WaitGroup
	if err := e.dispatchScanDatabase(t.Context(), &jobs); err != nil {
		t.Fatal(err)
	}
	jobs.Wait()
	if !started {
		t.Fatal("the background dispatcher did not reach the refresh container")
	}
	state, err := e.readScanRefresh()
	if err != nil || state.ID != "" || state.Message == "" {
		t.Fatal("failed refresh did not record its bounded cooldown", state, err)
	}
}

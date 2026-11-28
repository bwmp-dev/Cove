package worker

import (
	"context"
	"cove/internal/outbound"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestManagedRegistryMergeVerificationHonorsSelectedProxy(t *testing.T) {
	connections := 0
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		connections++
		if r.Method != "CONNECT" || r.Host != "registry.example.invalid:443" || r.Header.Get("Authorization") != "" {
			t.Error("unexpected proxy request")
		}
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer proxy.Close()
	registry := &Registry{Server: "registry.example.invalid", Username: "fixture", Password: "synthetic-password"}
	if err := VerifyManagedRegistry(t.Context(), registry, "", &outbound.Config{HTTPS: proxy.URL}); err == nil || connections != 1 {
		t.Fatal("registry bypassed the selected proxy", err, connections)
	}
}

func TestManagedRegistryMergeMaintenanceAndRecoveryInterlockNodeChanges(t *testing.T) {
	e, err := NewEngine(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	unlock, err := e.lockNodeChange(false)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	e.Run = func(context.Context, string, []string, []string, io.Writer) error {
		t.Fatal("registry touched Docker while node change was held")
		return nil
	}
	r := Request{ID: "registry-operation", Workload: strings.Repeat("a", 32), Action: "registry_maintenance", Kind: "service", ManagedRegistry: &ManagedRegistryOperation{Mode: "gc", Policy: ManagedRegistryRetention{KeepPerRepository: 10, MinimumAgeDays: 30}}}
	if err = e.performManagedRegistry(t.Context(), r, io.Discard); err == nil {
		t.Fatal("maintenance bypassed node-change lease")
	}
	if err = e.recoverManagedRegistry(t.Context(), journal{Request: r, RegistryCleanup: &managedRegistryCleanup{}}); err == nil {
		t.Fatal("recovery bypassed node-change lease")
	}
}

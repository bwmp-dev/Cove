package worker

import (
	"context"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestManagedRegistryVerificationRequiresTrustedAuthenticatedHTTPS(t *testing.T) {
	redirect := false
	native := true
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if redirect {
			http.Redirect(w, r, "https://foreign.example.invalid/v2/", 302)
			return
		}
		user, password, ok := r.BasicAuth()
		if !ok || user != "fixture" || password != "generated-fixture-password" {
			w.WriteHeader(401)
			return
		}
		if native {
			w.Header().Set("Docker-Distribution-Api-Version", "registry/2.0")
		}
		_, _ = w.Write([]byte("{}"))
	}))
	defer server.Close()
	registry := &Registry{Server: strings.TrimPrefix(server.URL, "https://"), Username: "fixture", Password: "generated-fixture-password"}
	certificate := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}))
	if err := VerifyManagedRegistry(context.Background(), registry, ""); err == nil {
		t.Fatal("untrusted TLS certificate activated a registry")
	}
	if err := VerifyManagedRegistry(context.Background(), registry, certificate); err != nil {
		t.Fatal(err)
	}
	registry.Password = "wrong"
	if err := VerifyManagedRegistry(context.Background(), registry, certificate); err == nil {
		t.Fatal("rejected credentials accepted")
	}
	registry.Password = "generated-fixture-password"
	redirect = true
	if err := VerifyManagedRegistry(context.Background(), registry, certificate); err == nil {
		t.Fatal("registry redirect followed")
	}
	redirect = false
	native = false
	if err := VerifyManagedRegistry(context.Background(), registry, certificate); err == nil {
		t.Fatal("arbitrary HTTPS endpoint accepted as registry")
	}
	if err := VerifyManagedRegistry(context.Background(), nil, certificate); err == nil {
		t.Fatal("missing credentials accepted")
	}
}

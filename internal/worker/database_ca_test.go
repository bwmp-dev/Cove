package worker

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDatabaseTrustFilesAreMountedWithoutMutatingTheRetainedRequest(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	certificate := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}))
	server.Close()
	name := "cove_ca_" + strings.Repeat("a", 32)
	engine, err := NewEngine(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	engine.Run = func(_ context.Context, _ string, _, _ []string, out io.Writer) error {
		_, err := io.WriteString(out, `{"services":{"app":{"image":"busybox:1.37"}}}`)
		return err
	}
	request := Request{ID: "trust", Workload: "consumer", Environment: map[string]string{"MODE": "test"}, DatabaseCAs: map[string]string{name: certificate}}
	if err := ValidateRequest(Request{ID: request.ID, Workload: request.Workload, Action: "deploy", Compose: "services:\n  app:\n    image: busybox:1.37\n", DatabaseCAs: request.DatabaseCAs}); err != nil {
		t.Fatal(err)
	}
	path, err := engine.prepareFileSecrets(context.Background(), request, engine.Root, []string{"docker", "compose"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var compose map[string]any
	if err = json.Unmarshal(data, &compose); err != nil {
		t.Fatal(err)
	}
	secret := compose["secrets"].(map[string]any)[name].(map[string]any)
	written, err := os.ReadFile(secret["file"].(string))
	if err != nil || string(written) != certificate {
		t.Fatal("delivered trust differs from selected source", err)
	}
	service := compose["services"].(map[string]any)["app"].(map[string]any)
	if len(service["secrets"].([]any)) != 1 || filepath.Base(secret["file"].(string)) != name {
		t.Fatal("trust file was not attached to the selected workload service")
	}
	if len(request.Environment) != 1 || len(request.FileSecrets) != 0 {
		t.Fatal("trust preparation changed immutable request data")
	}
	request.DatabaseCAs = map[string]string{"../outside": certificate}
	if validateDatabaseCAs(request.DatabaseCAs) == nil {
		t.Fatal("unsafe trust path accepted")
	}
}

package worker

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRouteCertificateACMEProjectionPreservesUnrelatedAccountsAndRecovers(t *testing.T) {
	certificate, key := selfSignedCertificate(t, "app.example.test")
	victim, victimKey := selfSignedCertificate(t, "victim.example.test")
	entry := func(c, k []byte) map[string]any {
		return map[string]any{"domain": map[string]string{"main": "kept.example.test"}, "certificate": base64.StdEncoding.EncodeToString(c), "key": base64.StdEncoding.EncodeToString(k), "Store": "default"}
	}
	original, err := json.Marshal(map[string]any{"cove": map[string]any{"Account": map[string]string{"PrivateKey": "fixture-account-key"}, "Certificates": []any{entry(certificate, key), entry(victim, victimKey)}, "future_field": "preserved"}})
	if err != nil {
		t.Fatal(err)
	}
	uploaded := []Route{{Domain: "app.example.test"}}
	projected, err := projectACMECertificates(original, nil, uploaded)
	if err != nil || !projected.Changed || len(projected.Archive) != 1 {
		t.Fatal("owned cached certificate was not archived", err)
	}
	raw, _ := json.Marshal(projected.Document)
	if !bytes.Contains(raw, []byte("fixture-account-key")) || !bytes.Contains(raw, []byte("future_field")) || bytes.Contains(raw, []byte(base64.StdEncoding.EncodeToString(certificate))) || !bytes.Contains(raw, []byte(base64.StdEncoding.EncodeToString(victim))) {
		t.Fatal("projection changed unrelated material")
	}
	repeated, err := projectACMECertificates(raw, projected.Archive, uploaded)
	if err != nil || repeated.Changed || len(repeated.Archive) != 1 {
		t.Fatal("projection was not stable", err)
	}
	recovered, err := projectACMECertificates(raw, projected.Archive, nil)
	if err != nil || !recovered.Changed {
		t.Fatal("rollback did not restore archived certificate", err)
	}
	restored, _ := json.Marshal(recovered.Document)
	for _, value := range []string{base64.StdEncoding.EncodeToString(certificate), base64.StdEncoding.EncodeToString(victim), "fixture-account-key", "future_field"} {
		if !bytes.Contains(restored, []byte(value)) {
			t.Fatal("rollback lost previously stored material")
		}
	}
	again, err := projectACMECertificates(restored, recovered.Archive, nil)
	if err != nil || again.Changed {
		t.Fatal("restoration was not stable", err)
	}
	if bytes.Contains(restored, []byte("cove_resolver")) {
		t.Fatal("archive metadata leaked into provider certificate")
	}
}

func TestRouteCertificateACMEProjectionRejectsUnreplacedNames(t *testing.T) {
	certificate, key := selfSignedCertificate(t, "*.example.test")
	document, _ := json.Marshal(map[string]any{"cove": map[string]any{"Certificates": []any{map[string]any{"certificate": base64.StdEncoding.EncodeToString(certificate), "key": base64.StdEncoding.EncodeToString(key)}}}})
	if _, err := projectACMECertificates(document, nil, []Route{{Domain: "app.example.test"}}); err == nil || !strings.Contains(err.Error(), "all its names") {
		t.Fatal("shared wildcard certificate was removed without replacements", err)
	}
	for _, bad := range [][]byte{[]byte("unreadable"), []byte(`{"cove":{"Certificates":[{"certificate":""}]}}`)} {
		if _, err := projectACMECertificates(bad, nil, nil); err == nil {
			t.Fatal("corrupt ACME data accepted")
		}
	}
}

func TestRouteCertificateReloadIntentIsBoundedAndRecognizedWithoutRoutes(t *testing.T) {
	e, err := NewEngine(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if pending, err := e.certificateReloadPending(); pending || err != nil {
		t.Fatal(pending, err)
	}
	path := filepath.Join(e.Root, "certificate-acme-reload.json")
	if err := writeJSON(path, map[string]int{"version": 1}); err != nil {
		t.Fatal(err)
	}
	if pending, err := e.certificateReloadPending(); !pending || err != nil {
		t.Fatal(pending, err)
	}
	for _, value := range []string{`{"version":2}`, "invalid", strings.Repeat("x", 1025)} {
		if err := os.WriteFile(path, []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := e.certificateReloadPending(); err == nil {
			t.Fatal("invalid reload intent accepted")
		}
	}
}

func TestRouteCertificateACMEEmptyAccountDoesNotRestartProxy(t *testing.T) {
	for _, document := range []string{`{}`, `{"cove":{"Account":{"PrivateKey":"fixture"},"Certificates":null}}`, `{"cove":{"Certificates":[]}}`} {
		projected, err := projectACMECertificates([]byte(document), nil, []Route{{Domain: "app.example.test"}})
		if err != nil || projected.Changed || len(projected.Archive) != 0 {
			t.Fatal("empty account caused a needless proxy restart", err)
		}
	}
}

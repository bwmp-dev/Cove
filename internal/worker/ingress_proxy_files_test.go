package worker

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestIngressProxyFilesOnlyRemoveOwnedPrivateValidationStaging(t *testing.T) {
	for _, owned := range []bool{true, false} {
		t.Run(map[bool]string{true: "owned", false: "foreign"}[owned], func(t *testing.T) {
			e := &Engine{Root: t.TempDir()}
			id := strings.Repeat("e", 32)
			change := &ingressProxyChange{ID: id, Staging: ".ingress-listener-check-" + id}
			directory := filepath.Join(e.Root, change.Staging)
			if err := os.Mkdir(directory, 0700); err != nil {
				t.Fatal(err)
			}
			owner := id
			if !owned {
				owner = strings.Repeat("f", 32)
			}
			if err := writeJSON(filepath.Join(directory, ".owner"), map[string]string{"change_id": owner}); err != nil {
				t.Fatal(err)
			}
			file := filepath.Join(directory, "private-cache")
			if err := os.WriteFile(file, []byte("fixture-private-cache"), 0600); err != nil {
				t.Fatal(err)
			}
			err := e.cleanupIngressExposureStaging(change)
			if owned {
				if err != nil {
					t.Fatal(err)
				}
				if _, err := os.Stat(directory); !os.IsNotExist(err) {
					t.Fatal("owned staging remained")
				}
			} else {
				if err == nil {
					t.Fatal("foreign staging was accepted for cleanup")
				}
				data, err := os.ReadFile(file)
				if err != nil || string(data) != "fixture-private-cache" {
					t.Fatal("foreign files were changed", err)
				}
			}
		})
	}
}

func TestIngressProxyFilesKeepUnprovedOrEscapingStagingUntouched(t *testing.T) {
	e := &Engine{Root: t.TempDir()}
	id := strings.Repeat("e", 32)
	change := &ingressProxyChange{ID: id, Staging: ".ingress-listener-check-" + id}
	directory := filepath.Join(e.Root, change.Staging)
	if err := os.Mkdir(directory, 0700); err != nil {
		t.Fatal(err)
	}
	if err := e.cleanupIngressExposureStaging(change); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(directory); err != nil {
		t.Fatal("unproved empty directory was removed", err)
	}
	file := filepath.Join(directory, "unrelated")
	if err := os.WriteFile(file, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := e.cleanupIngressExposureStaging(change); err == nil {
		t.Fatal("unproved nonempty staging was removed")
	}
	change.Staging = "../outside"
	if err := e.cleanupIngressExposureStaging(change); err == nil {
		t.Fatal("escaping staging identity was accepted")
	}
	if data, err := os.ReadFile(file); err != nil || string(data) != "keep" {
		t.Fatal("unproved files were changed", err)
	}
}

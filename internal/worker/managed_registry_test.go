package worker

import (
	"encoding/json"
	"strings"
	"testing"

	"golang.org/x/crypto/bcrypt"
)

func TestManagedRegistryDeploymentAuthenticationAndIsolation(t *testing.T) {
	hash, err := bcrypt.GenerateFromPassword([]byte("generated-fixture-password"), 10)
	if err != nil {
		t.Fatal(err)
	}
	c := ManagedRegistryConfig{ID: strings.Repeat("a", 32), Hostname: "registry.example.test", Username: "cove_registry", Hash: string(hash)}
	r, err := c.Deployment("provision")
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Routes) != 1 || r.Routes[0].Auth != "" || r.Routes[0].Domain != c.Hostname || len(r.FileSecrets) != 1 || r.FileSecrets[0].Services[0] != "registry" {
		t.Fatalf("incorrect route or auth file mapping: %+v", r.FileSecrets)
	}
	if strings.Contains(r.Compose, c.Hash) || strings.Contains(r.Compose, "generated-fixture-password") {
		t.Fatal("credential in Compose")
	}
	if r.Environment["COVE_REGISTRY_AUTH"] != c.Username+":"+c.Hash+"\n" {
		t.Fatal("auth file content lost")
	}
	var compose struct {
		Services map[string]struct {
			Image       string            `json:"image"`
			Ports       []any             `json:"ports"`
			ReadOnly    bool              `json:"read_only"`
			CapDrop     []string          `json:"cap_drop"`
			Environment map[string]string `json:"environment"`
			Volumes     []string          `json:"volumes"`
		} `json:"services"`
	}
	if err = json.Unmarshal([]byte(r.Compose), &compose); err != nil {
		t.Fatal(err)
	}
	s := compose.Services["registry"]
	if s.Image != ManagedRegistryImage || len(s.Ports) != 0 || !s.ReadOnly || len(s.CapDrop) != 1 || s.CapDrop[0] != "ALL" || s.Environment["REGISTRY_AUTH"] != "htpasswd" || s.Environment["REGISTRY_STORAGE_DELETE_ENABLED"] != "false" || len(s.Volumes) != 1 || s.Volumes[0] != "data:/var/lib/registry" {
		t.Fatal("unsafe registry deployment")
	}
	if support := ManagedRegistryStorageSupport(); support.HardCapVerified || !strings.Contains(support.Reason, "No hard cap") {
		t.Fatal("unverified quota advertised")
	}
	for _, change := range []func(*ManagedRegistryConfig){
		func(c *ManagedRegistryConfig) { c.Hostname = "*.example.test" },
		func(c *ManagedRegistryConfig) { c.Hostname = "registry.example.test/path" },
		func(c *ManagedRegistryConfig) { c.Username = "user:other" },
		func(c *ManagedRegistryConfig) { c.Hash = "plaintext" },
		func(c *ManagedRegistryConfig) { c.Hash = "$2a$10$" + strings.Repeat("!", 53) },
		func(c *ManagedRegistryConfig) { c.ID = "../foreign" },
		func(c *ManagedRegistryConfig) { c.Hostname = "127.0.0.1" },
	} {
		bad := c
		change(&bad)
		if _, err = bad.Deployment("provision"); err == nil {
			t.Fatal("invalid registry configuration accepted")
		}
	}
}

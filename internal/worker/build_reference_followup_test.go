package worker

import (
	"strings"
	"testing"
)

func TestBuildRepositoryRefusesQualifiedRegistryReferences(t *testing.T) {
	for _, scenario := range []struct {
		server, repository string
		valid              bool
	}{
		{"registry.example.test", "team/app", true},
		{"registry.example.test", "app", true},
		{"registry.example.test", "team/app.image", true},
		{"registry.example.test", "registry.example.test/team/app", false},
		{"registry.example.test", "other.example.test/team/app", false},
		{"registry.example.test", "localhost/team/app", false},
		{"registry", "registry/team/app", false},
		{"registry:5000", "registry:5000/team/app", false},
	} {
		t.Run(scenario.server+"/"+scenario.repository, func(t *testing.T) {
			request := Request{ID: "build", Workload: "app", Action: "build", Kind: "service", Compose: "services: {}", Build: &BuildOptions{Repository: scenario.repository}, Registry: &Registry{Server: scenario.server, Username: "fixture", Password: "fixture-password"}}
			err := ValidateRequest(request)
			if scenario.valid && err != nil {
				t.Fatal(err)
			}
			if !scenario.valid && (err == nil || !strings.Contains(err.Error(), "without a registry host")) {
				t.Fatal("qualified repository was not clearly refused", err)
			}
		})
	}
}

package worker

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestGuardedHooksStopBeforeReplacement(t *testing.T) {
	for _, failure := range []string{"", "volume", "network", "database", "receipt", "compatibility"} {
		t.Run(failure, func(t *testing.T) {
			id := strings.Repeat("a", 64)
			image := "sha256:" + strings.Repeat("b", 64)
			hooks := &DeploymentHooks{Assertions: &DeploymentAssertions{
				Volumes:  []VolumeAssertion{{Name: "sealed-data", Labels: map[string]string{"owner": "fixture"}}},
				Networks: []NetworkAssertion{{Name: "private-db", Internal: true}},
				Services: []ServiceAssertion{{Service: "database", Image: image, Networks: []string{"private-db"}, Mounts: []MountAssertion{{Volume: "sealed-data", Target: "/data"}}}},
			}, BeforeSteps: []DeploymentHook{{Service: "receipt", Command: "verify"}, {Service: "compatibility", Command: "check"}}}
			if err := validateHooks(hooks, "service"); err != nil {
				t.Fatal(err)
			}
			var steps []string
			e := &Engine{Root: t.TempDir()}
			e.Run = func(_ context.Context, _ string, args, env []string, out io.Writer) error {
				command := strings.Join(args, " ")
				switch {
				case strings.HasSuffix(command, "config --format json"):
					_, err := io.WriteString(out, `{"services":{"database":{"image":"`+image+`","networks":{"db":{}},"volumes":[{"type":"volume","source":"data","target":"/data","read_only":false}]}},"networks":{"db":{"name":"private-db","external":true}},"volumes":{"data":{"name":"sealed-data","external":true}}}`)
					return err
				case command == "docker volume inspect sealed-data":
					labels := map[string]string{"owner": "fixture"}
					if failure == "volume" {
						labels["owner"] = "foreign"
					}
					return json.NewEncoder(out).Encode([]any{map[string]any{"Name": "sealed-data", "Driver": "local", "Labels": labels}})
				case command == "docker network inspect private-db":
					return json.NewEncoder(out).Encode([]any{map[string]any{"Name": "private-db", "Driver": "bridge", "Internal": failure != "network"}})
				case strings.HasSuffix(command, "ps --all --quiet database"):
					_, err := io.WriteString(out, id)
					return err
				case command == "docker inspect "+id:
					return json.NewEncoder(out).Encode([]any{map[string]any{"Image": image, "State": map[string]any{"Running": true, "Health": map[string]string{"Status": "healthy"}}, "HostConfig": map[string]any{"PortBindings": map[string]any{}}, "NetworkSettings": map[string]any{"Networks": map[string]any{"private-db": map[string]any{}}}, "Mounts": []any{map[string]any{"Type": "volume", "Name": "sealed-data", "Destination": "/data", "RW": failure != "database"}}}})
				case strings.Contains(command, "run --rm"):
					step := "receipt"
					if strings.Contains(command, "sh compatibility") {
						step = "compatibility"
					}
					steps = append(steps, step)
					if failure == step {
						return errors.New("check refused")
					}
					return nil
				case strings.Contains(command, "ps -aq"):
					return nil
				default:
					t.Fatalf("unexpected Docker mutation: %s", command)
					return nil
				}
			}
			err := e.runBeforeHooks(context.Background(), e.Root, "guard-test", []string{"docker", "compose", "--project-name", "cove-fixture"}, nil, hooks, io.Discard)
			if (err != nil) != (failure != "") {
				t.Fatal(err)
			}
			want := 0
			if failure == "receipt" {
				want = 1
			}
			if failure == "" || failure == "compatibility" {
				want = 2
			}
			if len(steps) != want {
				t.Fatalf("checks after refusal: %v", steps)
			}
		})
	}
}

func TestDeploymentAssertionValidation(t *testing.T) {
	for _, a := range []*DeploymentAssertions{
		{Volumes: []VolumeAssertion{{Name: "--format"}}},
		{Networks: []NetworkAssertion{{Name: "../host"}}},
		{Services: []ServiceAssertion{{Service: "database", Image: "latest"}}},
	} {
		if validateDeploymentAssertions(a) == nil {
			t.Fatal("unsafe assertion accepted")
		}
	}
	if validateHooks(&DeploymentHooks{BeforeSteps: []DeploymentHook{{Service: "check", Command: "true"}}}, "job") == nil {
		t.Fatal("job accepted guarded hooks")
	}
}

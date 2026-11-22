package worker

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSharedNetworkCompose(t *testing.T) {
	id := strings.Repeat("a", 32)
	connections := []NetworkAttachment{{ID: id, Service: "db", Alias: "database"}, {ID: id, Service: "app", Alias: "backend"}}
	if err := ValidateNetworks(connections); err != nil {
		t.Fatal(err)
	}
	var config map[string]any
	if err := json.Unmarshal([]byte(`{"services":{"db":{"networks":{"default":null}},"app":{"networks":{"default":null}}},"networks":{"default":{}}}`), &config); err != nil {
		t.Fatal(err)
	}
	if err := addSharedNetworks(config, connections); err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(config)
	if !strings.Contains(string(data), `"aliases":["database"]`) || !strings.Contains(string(data), `"default":null`) {
		t.Fatal(string(data))
	}
	if err := addSharedNetworks(config, connections); err == nil {
		t.Fatal("reserved network accepted")
	}
	for _, n := range []NetworkAttachment{{ID: "../host", Service: "db", Alias: "db"}, {ID: id, Service: "missing", Alias: "bad alias"}} {
		if ValidateNetworks([]NetworkAttachment{n}) == nil {
			t.Fatal("invalid attachment accepted")
		}
	}
	if ValidateNetworks(append(connections, connections[0])) == nil {
		t.Fatal("duplicate accepted")
	}
	if err := addSharedNetworks(map[string]any{"services": map[string]any{"db": map[string]any{"network_mode": "host"}}}, connections[:1]); err == nil {
		t.Fatal("host networking accepted")
	}
}

func TestSharedNetworkDockerCommunication(t *testing.T) {
	if os.Getenv("COVE_E2E") != "1" {
		t.Skip("COVE_E2E=1 required; creates disposable Docker resources")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	bytes := make([]byte, 16)
	if _, err := rand.Read(bytes); err != nil {
		t.Fatal(err)
	}
	id := hex.EncodeToString(bytes)
	e, err := NewEngine(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	name := "cove-shared-" + id
	t.Cleanup(func() {
		if err := RunCommand(context.Background(), e.Root, []string{"docker", "network", "rm", name}, nil, io.Discard); err != nil {
			t.Error(err)
		}
	})
	configs := []string{
		"services:\n  server:\n    image: busybox:1.37\n    command: [sh, -c, 'mkdir -p /www; echo private-ok > /www/index.html; httpd -f -p 8080 -h /www']\n",
		"services:\n  client:\n    image: busybox:1.37\n    command: [sleep, '300']\n",
	}
	var clientArgs []string
	for i, compose := range configs {
		project := "cove-net-test-" + id + string(rune('a'+i))
		dir := filepath.Join(e.Root, project)
		if err = os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, "source.yaml")
		if err = os.WriteFile(path, []byte(compose), 0600); err != nil {
			t.Fatal(err)
		}
		args := []string{"docker", "compose", "-p", project, "-f", path}
		service, alias := "server", "database"
		if i == 1 {
			service, alias = "client", "application"
		}
		path, err = e.prepareNetworks(ctx, Request{ID: project, Networks: []NetworkAttachment{{ID: id, Service: service, Alias: alias}}}, dir, args, nil, io.Discard)
		if err != nil {
			t.Fatal(err)
		}
		args = []string{"docker", "compose", "-p", project, "-f", path}
		t.Cleanup(func() {
			if err := RunCommand(context.Background(), dir, append(append([]string{}, args...), "down", "--volumes"), nil, io.Discard); err != nil {
				t.Error(err)
			}
		})
		if err = RunCommand(ctx, dir, append(append([]string{}, args...), "up", "-d", "--wait"), nil, io.Discard); err != nil {
			t.Fatal(err)
		}
		if i == 1 {
			clientArgs = args
		}
	}
	var output strings.Builder
	if err = RunCommand(ctx, e.Root, append(clientArgs, "exec", "-T", "client", "wget", "-qO-", "http://database:8080"), nil, &output); err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(output.String()) != "private-ok" {
		t.Fatal("private DNS communication failed")
	}
}

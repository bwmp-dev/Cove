package worker

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"
)

func TestIngressProxySnapshotDockerReplay(t *testing.T) {
	if os.Getenv("COVE_TEST_PROXY_SNAPSHOT_REPLAY") != "1" {
		t.Skip("requires owned local Docker proxy replay resources")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	e, err := NewEngine(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte(e.Root))
	owner := fmt.Sprintf("cove-proxy-replay-%x", digest[:6])
	label := "cove.proxy-replay=" + owner
	resources := [][2]string{}
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), time.Minute)
		defer stop()
		for i := len(resources) - 1; i >= 0; i-- {
			resource := resources[i]
			format := `{{index .Labels "cove.proxy-replay"}}`
			if resource[0] == "container" {
				format = `{{index .Config.Labels "cove.proxy-replay"}}`
			}
			actual, err := e.output(cleanup, "docker", resource[0], "inspect", "--format", format, resource[1])
			if err != nil || actual != owner {
				t.Errorf("could not confirm owned replay %s for cleanup", resource[0])
				continue
			}
			args := []string{"docker", resource[0], "rm", resource[1]}
			if resource[0] == "container" {
				args = []string{"docker", "rm", "-f", resource[1]}
			}
			if err := e.Run(cleanup, e.Root, args, nil, io.Discard); err != nil {
				t.Errorf("could not clean owned replay %s", resource[0])
			}
		}
	})
	run := func(args ...string) {
		t.Helper()
		if err := e.Run(ctx, e.Root, args, nil, io.Discard); err != nil {
			t.Fatalf("owned proxy replay command failed: %v", err)
		}
	}
	if _, err := e.output(ctx, "docker", "image", "inspect", ingressImage); err != nil {
		t.Fatal("proxy replay requires the cached pinned Traefik image")
	}
	front, back, volume := owner+"-front", owner+"-back", owner+"-data"
	for _, name := range []string{front, back} {
		args := []string{"docker", "network", "create", "--label", label}
		if name == back {
			args = append(args, "--internal")
		}
		run(append(args, name)...)
		resources = append(resources, [2]string{"network", name})
	}
	run("docker", "volume", "create", "--label", label, volume)
	resources = append(resources, [2]string{"volume", volume})
	old, candidate := owner+"-old", owner+"-candidate"
	run("docker", "create", "--name", old, "--label", label, "--label", "cove.managed=ingress", "--network", front, "--restart", "unless-stopped", "--read-only", "--security-opt", "no-new-privileges:true", "-v", volume+":/data", "--entrypoint", "sh", ingressImage, "-ec", `mkdir -p /data/dynamic; exec traefik "$@"`, "traefik", "--entrypoints.web.address=:8080", "--providers.file.directory=/data/dynamic")
	resources = append(resources, [2]string{"container", old})
	run("docker", "network", "connect", "--alias", "trusted-helper-peer", back, old)
	run("docker", "start", old)
	raw, err := e.output(ctx, "docker", "inspect", old)
	if err != nil {
		t.Fatal(err)
	}
	var objects []map[string]json.RawMessage
	if json.Unmarshal([]byte(raw), &objects) != nil || len(objects) != 1 {
		t.Fatal("owned replay proxy inspection unavailable")
	}
	objects[0]["Name"] = json.RawMessage(`"/cove-ingress"`)
	normalized, _ := json.Marshal(objects)
	snapshot, err := inspectIngressProxy(normalized)
	if err != nil {
		t.Fatal(err)
	}
	run("docker", "update", "--restart=no", old)
	run("docker", "stop", "--timeout", "1", old)
	for name := range snapshot.Networks {
		run("docker", "network", "disconnect", name, old)
	}
	body, err := ingressProxyCreateBody(snapshot, strings.Repeat("e", 32))
	if err != nil {
		t.Fatal(err)
	}
	client := e.ingressDockerClient()
	defer client.CloseIdleConnections()
	var created struct {
		ID string `json:"Id"`
	}
	resources = append(resources, [2]string{"container", candidate})
	if err := ingressDockerJSON(ctx, client, "POST", "/containers/create?name="+candidate, body, &created); err != nil {
		t.Fatal(err)
	}
	if err := ingressDockerJSON(ctx, client, "POST", "/containers/"+created.ID+"/start", nil, nil); err != nil {
		t.Fatal(err)
	}
	var actual struct {
		Image      string
		State      struct{ Running bool }
		HostConfig struct {
			ReadonlyRootfs bool
			RestartPolicy  struct{ Name string }
			SecurityOpt    []string
		}
		NetworkSettings struct {
			Networks map[string]ingressProxyEndpoint
		}
	}
	if err := ingressDockerJSON(ctx, client, "GET", "/containers/"+created.ID+"/json", nil, &actual); err != nil {
		t.Fatal(err)
	}
	if !actual.State.Running || actual.Image != snapshot.Image || !actual.HostConfig.ReadonlyRootfs || actual.HostConfig.RestartPolicy.Name != "unless-stopped" || len(actual.HostConfig.SecurityOpt) == 0 || len(actual.NetworkSettings.Networks) != 2 {
		t.Fatal("proxy replay lost its original runtime/security definition")
	}
	for name, expected := range snapshot.Networks {
		got := actual.NetworkSettings.Networks[name]
		if got.NetworkID != expected.NetworkID || got.IPAddress != expected.IPAddress || got.MacAddress != expected.MacAddress {
			t.Fatal("proxy replay changed a pinned helper peer")
		}
	}
	if err := ingressDockerJSON(ctx, client, "DELETE", "/containers/"+created.ID+"?force=1", nil, nil); err != nil {
		t.Fatal(err)
	}
	resources = resources[:len(resources)-1]
	for _, endpoint := range snapshot.Networks {
		connection := map[string]any{"Container": snapshot.ID, "EndpointConfig": ingressProxyEndpointConfig(endpoint)}
		if err := ingressDockerJSON(ctx, client, "POST", "/networks/"+endpoint.NetworkID+"/connect", connection, nil); err != nil {
			t.Fatal(err)
		}
	}
	var originalHost map[string]json.RawMessage
	if json.Unmarshal(snapshot.HostConfig, &originalHost) != nil {
		t.Fatal("original restart policy unavailable")
	}
	if err := ingressDockerJSON(ctx, client, "POST", "/containers/"+snapshot.ID+"/update", map[string]json.RawMessage{"RestartPolicy": originalHost["RestartPolicy"]}, nil); err != nil {
		t.Fatal(err)
	}
	if err := ingressDockerJSON(ctx, client, "POST", "/containers/"+snapshot.ID+"/start", nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := ingressDockerJSON(ctx, client, "GET", "/containers/"+snapshot.ID+"/json", nil, &actual); err != nil {
		t.Fatal(err)
	}
	if !actual.State.Running || actual.HostConfig.RestartPolicy.Name != "unless-stopped" {
		t.Fatal("original proxy did not resume with its saved restart policy")
	}
	for name, expected := range snapshot.Networks {
		if actual.NetworkSettings.Networks[name].IPAddress != expected.IPAddress {
			t.Fatal("original proxy could not reclaim its pinned peer after replacement removal")
		}
	}
}

func TestIngressProxySnapshotDockerControlReadOnly(t *testing.T) {
	if os.Getenv("COVE_TEST_PROXY_SNAPSHOT_DOCKER") != "1" {
		t.Skip("requires an explicitly selected local Docker metadata check")
	}
	e := &Engine{Root: t.TempDir()}
	client := e.ingressDockerClient()
	defer client.CloseIdleConnections()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	var version struct {
		API string `json:"ApiVersion"`
	}
	if err := ingressDockerJSON(ctx, client, "GET", "/version", nil, &version); err != nil || version.API == "" {
		t.Fatal("Docker CLI transport could not read engine metadata", err)
	}
	cancel()
	if err := ingressDockerJSON(ctx, client, "GET", "/version", nil, &version); !errors.Is(err, context.Canceled) {
		t.Fatal("cancelled Docker control request continued")
	}
}

func ingressSnapshotFixture() map[string]any {
	return map[string]any{
		"Id": strings.Repeat("a", 64), "Name": "/cove-ingress", "Image": "sha256:" + strings.Repeat("b", 64),
		"State": map[string]any{"Running": true},
		"Config": map[string]any{
			"Image": "traefik:v3.7.19", "Labels": map[string]string{"cove.managed": "ingress", "cove.browser-fixture": "fixture-owner"},
			"Entrypoint": []string{"sh"}, "Cmd": []string{"-ec", `mkdir -p /data/dynamic; exec traefik "$@"`, "traefik", "--entrypoints.websecure.address=:443", "--providers.file.directory=/data/dynamic"},
			"Env": []string{"SECRET=snapshot-sensitive-fixture", "COVE_DOMAIN=panel.example.test"}, "User": "1000:1000", "WorkingDir": "/data",
		},
		"HostConfig": map[string]any{"NetworkMode": "cove-ingress", "RestartPolicy": map[string]any{"Name": "unless-stopped", "MaximumRetryCount": 0}, "ReadonlyRootfs": true, "SecurityOpt": []string{"no-new-privileges:true"}, "Binds": []string{"cove-ingress:/data"}},
		"Mounts":     []map[string]string{{"Type": "volume", "Name": "cove-ingress", "Source": "/var/lib/docker/volumes/cove-ingress/_data", "Destination": "/data"}},
		"NetworkSettings": map[string]any{"Networks": map[string]any{
			"cove-ingress": map[string]any{"NetworkID": strings.Repeat("c", 64), "IPAddress": "172.18.0.2", "Aliases": []string{"cove-ingress", "proxy"}, "MacAddress": "02:42:ac:12:00:02"},
			"idle-front":   map[string]any{"NetworkID": strings.Repeat("d", 64), "IPAddress": "172.19.0.3", "GlobalIPv6Address": "fd00:1234::3", "Aliases": []string{"trusted-proxy"}, "IPAMConfig": map[string]string{"IPv4Address": "172.19.0.3", "IPv6Address": "fd00:1234::3"}},
		}},
	}
}

func TestIngressProxySnapshotKeepsWorkerAndPanelLaunchState(t *testing.T) {
	for _, panel := range []bool{false, true} {
		name := "worker"
		if panel {
			name = "panel"
		}
		t.Run(name, func(t *testing.T) {
			input := ingressSnapshotFixture()
			if panel {
				config := input["Config"].(map[string]any)
				config["Entrypoint"] = []string{"sh", "-ec"}
				config["Cmd"] = []string{"mkdir -p /data/dynamic\ncp /etc/traefik/panel.yaml /data/dynamic/panel.yaml\nexec traefik --configFile=/etc/traefik/traefik.yaml\n"}
			}
			raw, err := json.Marshal([]any{input})
			if err != nil {
				t.Fatal(err)
			}
			snapshot, err := inspectIngressProxy(raw)
			if err != nil {
				t.Fatal(err)
			}
			original, _ := json.Marshal(input["Config"])
			if string(snapshot.Config) != string(original) {
				t.Fatal("private original container configuration changed")
			}
			originalHost, _ := json.Marshal(input["HostConfig"])
			if string(snapshot.HostConfig) != string(originalHost) || !snapshot.Running || len(snapshot.Networks) != 2 || snapshot.Networks["idle-front"].GlobalIPv6Address != "fd00:1234::3" || snapshot.Networks["idle-front"].IPAMConfig.IPv4Address != "172.19.0.3" {
				t.Fatal("restart/security settings or pinned helper peers were lost")
			}
			if panel && (snapshot.Launch.Kind != "file" || snapshot.Launch.StaticFile != "/etc/traefik/traefik.yaml") || !panel && snapshot.Launch.Kind != "cli" {
				t.Fatal("proxy launch shape changed")
			}
		})
	}
}

func TestIngressProxySnapshotAllowsPrivateTmpfs(t *testing.T) {
	input := ingressSnapshotFixture()
	input["Mounts"] = []map[string]string{{"Type": "tmpfs", "Destination": "/tmp"}}
	raw, _ := json.Marshal([]any{input})
	if _, err := inspectIngressProxy(raw); err != nil {
		t.Fatal(err)
	}
}

func TestIngressProxySnapshotRecreatesImmutableImageAndPinnedPeers(t *testing.T) {
	raw, _ := json.Marshal([]any{ingressSnapshotFixture()})
	snapshot, err := inspectIngressProxy(raw)
	if err != nil {
		t.Fatal(err)
	}
	body, err := ingressProxyCreateBody(snapshot, strings.Repeat("e", 32))
	if err != nil {
		t.Fatal(err)
	}
	var image string
	if json.Unmarshal(body["Image"], &image) != nil || image != snapshot.Image || string(body["HostConfig"]) != string(snapshot.HostConfig) {
		t.Fatal("immutable image or original host settings were lost")
	}
	var networks struct {
		Endpoints map[string]ingressProxyEndpoint `json:"EndpointsConfig"`
	}
	if json.Unmarshal(body["NetworkingConfig"], &networks) != nil || len(networks.Endpoints) != 2 || networks.Endpoints["cove-ingress"].IPAMConfig.IPv4Address != "172.18.0.2" || networks.Endpoints["idle-front"].IPAMConfig.IPv6Address != "fd00:1234::3" {
		t.Fatal("effective proxy addresses were not pinned for replacement")
	}
	if strings.Contains(string(body["NetworkingConfig"]), "GlobalIPv6Address") || strings.Contains(string(body["NetworkingConfig"]), "\"IPAddress\"") {
		t.Fatal("read-only Docker network fields were copied into a create request")
	}
	var original, candidate map[string]string
	var originalConfig map[string]json.RawMessage
	json.Unmarshal(snapshot.Config, &originalConfig)
	json.Unmarshal(originalConfig["Labels"], &original)
	json.Unmarshal(body["Labels"], &candidate)
	if original["cove.ingress-revision"] != "" || candidate["cove.ingress-revision"] != strings.Repeat("e", 32) || candidate["cove.browser-fixture"] != "fixture-owner" {
		t.Fatal("snapshot was changed or replacement ownership was lost")
	}
}

func TestIngressProxySnapshotRejectsUnsafeOrUnknownLaunchWithoutSecrets(t *testing.T) {
	for name, mutate := range map[string]func(map[string]any){
		"foreign name":          func(v map[string]any) { v["Name"] = "/foreign" },
		"unconfirmed image":     func(v map[string]any) { v["Image"] = "traefik:latest" },
		"unknown running":       func(v map[string]any) { v["State"] = map[string]any{} },
		"host network":          func(v map[string]any) { v["HostConfig"].(map[string]any)["NetworkMode"] = "host" },
		"privileged":            func(v map[string]any) { v["HostConfig"].(map[string]any)["Privileged"] = true },
		"automatic removal":     func(v map[string]any) { v["HostConfig"].(map[string]any)["AutoRemove"] = true },
		"implicit publications": func(v map[string]any) { v["HostConfig"].(map[string]any)["PublishAllPorts"] = true },
		"dependent mounts":      func(v map[string]any) { v["HostConfig"].(map[string]any)["VolumesFrom"] = []string{"foreign"} },
		"anonymous volume":      func(v map[string]any) { v["HostConfig"].(map[string]any)["Binds"] = []string{"/data"} },
		"ephemeral publication": func(v map[string]any) {
			v["HostConfig"].(map[string]any)["PortBindings"] = map[string]any{"443/tcp": []map[string]string{{"HostIP": "127.0.0.1", "HostPort": ""}}}
		},
		"foreign ownership": func(v map[string]any) {
			v["Config"].(map[string]any)["Labels"] = map[string]string{"cove.managed": "foreign"}
		},
		"Docker provider": func(v map[string]any) {
			v["Config"].(map[string]any)["Env"] = []string{"TRAEFIK_PROVIDERS_DOCKER=true"}
		},
		"Docker socket": func(v map[string]any) {
			v["Mounts"] = []map[string]string{{"Source": "/var/run/docker.sock", "Destination": "/engine"}}
		},
		"runtime directory": func(v map[string]any) {
			v["Mounts"] = []map[string]string{{"Source": "/run", "Destination": "/runtime"}}
		},
		"root directory": func(v map[string]any) { v["Mounts"] = []map[string]string{{"Source": "/", "Destination": "/host"}} },
		"unfamiliar script": func(v map[string]any) {
			v["Config"].(map[string]any)["Cmd"] = []string{"-ec", "curl example.test; exec traefik"}
		},
		"missing networking": func(v map[string]any) { v["NetworkSettings"] = map[string]any{} },
		"wrong IP family": func(v map[string]any) {
			v["NetworkSettings"].(map[string]any)["Networks"].(map[string]any)["cove-ingress"].(map[string]any)["IPAddress"] = "fd00::1"
		},
	} {
		t.Run(name, func(t *testing.T) {
			input := ingressSnapshotFixture()
			mutate(input)
			raw, _ := json.Marshal([]any{input})
			if _, err := inspectIngressProxy(raw); err == nil || strings.Contains(err.Error(), "snapshot-sensitive-fixture") {
				t.Fatal("unsafe launch accepted or private configuration exposed")
			}
		})
	}
	for _, raw := range [][]byte{[]byte("[]"), []byte("[{},{}]"), []byte(strings.Repeat(" ", (2<<20)+1))} {
		if _, err := inspectIngressProxy(raw); err == nil {
			t.Fatal("ambiguous or oversized inspection accepted")
		}
	}
}

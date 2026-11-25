package worker

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestFirewallPublishedPortsOnOwnedDocker(t *testing.T) {
	if runtime.GOOS != "linux" || os.Getenv("COVE_TEST_FIREWALL_PORTS_DOCKER") != "1" {
		t.Skip("explicit owned Linux Docker fixture required")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	e, err := NewEngine(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	command := func(args ...string) string {
		t.Helper()
		out, err := e.output(ctx, append([]string{"docker"}, args...)...)
		if err != nil {
			t.Fatal(args, out, err)
		}
		return out
	}
	for _, name := range []string{"cove-ingress", linkGatewayName} {
		if _, err := e.output(ctx, "docker", "inspect", name); err == nil {
			t.Fatal("refusing a daemon with an existing shared Cove container", name)
		}
	}
	owner, err := firewallChangeID()
	if err != nil {
		t.Fatal(err)
	}
	workload := "inventory-" + owner
	if err = os.MkdirAll(filepath.Join(e.Root, "workloads", workload), 0700); err != nil {
		t.Fatal(err)
	}
	created := map[string]string{}
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		for name, id := range created {
			actual, err := e.output(cleanup, "docker", "inspect", "--format", `{{.Id}} {{index .Config.Labels "cove.firewall-inventory-owner"}}`, name)
			if err != nil || strings.TrimSpace(actual) != id+" "+owner {
				t.Error("refusing unconfirmed inventory fixture cleanup", name, actual, err)
				continue
			}
			if err := e.Run(cleanup, e.Root, []string{"docker", "rm", "-f", "-v", id}, nil, io.Discard); err != nil {
				t.Error(err)
			}
		}
		remaining, err := e.output(cleanup, "docker", "ps", "-aq", "--filter", "label=cove.firewall-inventory-owner="+owner)
		if err != nil || strings.TrimSpace(remaining) != "" {
			t.Error("inventory fixture containers remain", remaining, err)
		}
	})
	start := func(name string, args ...string) {
		t.Helper()
		commandArgs := []string{"run", "-d", "--pull", "never", "--name", name, "--label", "cove.firewall-inventory-owner=" + owner}
		commandArgs = append(commandArgs, args...)
		commandArgs = append(commandArgs, "busybox:1.37", "sleep", "180")
		id := strings.TrimSpace(command(commandArgs...))
		created[name] = id
	}
	local, public, foreign := "cove-fwi-"+owner+"-local", "cove-fwi-"+owner+"-public", "cove-fwi-"+owner+"-foreign"
	start(local, "--label", "com.docker.compose.project=cove-"+workload, "-p", "127.0.0.1::8080", "-p", "[::1]::8081")
	start(public, "--label", "com.docker.compose.project=cove-"+workload, "-p", "8080", "-p", "8081/udp")
	start(linkGatewayName, "--label", "cove.managed=link", "--label", "cove.link-port=47800", "--label", "cove.link-version="+linkGatewayVersion, "-p", "47800:47800")
	start(foreign, "--label", "cove.managed=link", "-p", "8080")
	wanted := map[FirewallPort]bool{}
	for _, name := range []string{public, linkGatewayName} {
		var bindings map[string][]struct{ HostPort string }
		if err := json.Unmarshal([]byte(command("inspect", "--format", "{{json .NetworkSettings.Ports}}", name)), &bindings); err != nil {
			t.Fatal(err)
		}
		for port, hosts := range bindings {
			_, protocol, _ := strings.Cut(port, "/")
			for _, host := range hosts {
				number, err := strconv.ParseUint(host.HostPort, 10, 16)
				if err != nil {
					t.Fatal(err)
				}
				wanted[FirewallPort{Protocol: protocol, Port: uint16(number)}] = true
			}
		}
	}
	ports, err := e.firewallPorts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	got := map[FirewallPort]bool{}
	for _, port := range ports {
		got[port] = true
	}
	if !reflect.DeepEqual(got, wanted) || !slices.Contains(ports, FirewallPort{Protocol: "tcp", Port: 47800}) {
		t.Fatal("real Docker loopback, wildcard, protocol or gateway inventory differed", ports, wanted)
	}
	t.Log("Real IPv4/IPv6 loopback-only bindings excluded; public TCP/UDP and the labelled private-link gateway included; foreign helper excluded.")
}

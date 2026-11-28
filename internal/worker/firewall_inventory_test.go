package worker

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func TestFirewallInventoryExcludesLoopbackAndIncludesOwnedGateways(t *testing.T) {
	e := &Engine{Root: t.TempDir()}
	if err := os.MkdirAll(filepath.Join(e.Root, "workloads", "inventory-app"), 0700); err != nil {
		t.Fatal(err)
	}
	binding := func(address, port string) map[string]string {
		return map[string]string{"HostIP": address, "HostPort": port}
	}
	items := []map[string]any{
		{"name": "/inventory-app", "labels": map[string]string{"com.docker.compose.project": "cove-inventory-app"}, "ports": map[string]any{
			"80/tcp": []map[string]string{binding("127.0.0.1", "5055"), binding("127.0.0.2", "5056"), binding("::1", "5057"), binding("::ffff:127.0.0.1", "5058")},
			"81/tcp": []map[string]string{binding("192.0.2.10", "8080"), binding("", "8081"), binding("2001:db8::10", "8082"), binding("0.0.0.0", "8083")},
			"53/udp": []map[string]string{binding("127.0.0.1", "1053"), binding("::", "1053")},
		}},
		{"name": "/cove-ingress", "labels": map[string]string{"cove.managed": "ingress"}, "ports": map[string]any{"443/tcp": []map[string]string{binding("0.0.0.0", "9443")}}},
		{"name": "/cove-link", "labels": map[string]string{"cove.managed": "link"}, "ports": map[string]any{"47800/tcp": []map[string]string{binding("0.0.0.0", "47800"), binding("::", "47800")}}},
		{"name": "/foreign-link", "labels": map[string]string{"cove.managed": "link"}, "ports": map[string]any{"80/tcp": []map[string]string{binding("0.0.0.0", "6666")}}},
		{"name": "/cove-link", "labels": map[string]string{"cove.managed": "foreign"}, "ports": map[string]any{"80/tcp": []map[string]string{binding("0.0.0.0", "6667")}}},
		{"name": "/foreign-resource", "labels": map[string]string{"com.docker.compose.project": "cove-unowned"}, "ports": map[string]any{"80/tcp": []map[string]string{binding("0.0.0.0", "6668")}}},
	}
	e.Run = func(_ context.Context, _ string, args []string, _ []string, output io.Writer) error {
		if args[1] == "ps" {
			io.WriteString(output, strings.Repeat("a", 64))
			return nil
		}
		if args[1] != "inspect" {
			t.Fatal(args)
		}
		for _, item := range items {
			if err := json.NewEncoder(output).Encode(item); err != nil {
				t.Fatal(err)
			}
		}
		return nil
	}
	got, err := e.firewallPorts(t.Context())
	want := []FirewallPort{{"tcp", 8080}, {"tcp", 8081}, {"tcp", 8082}, {"tcp", 8083}, {"tcp", 9443}, {"tcp", 47800}, {"udp", 1053}}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatal("incorrect externally published Cove port inventory", got, err)
	}
}

func TestFirewallInventoryRefusesMalformedBindingAddress(t *testing.T) {
	e := &Engine{Root: t.TempDir()}
	e.Run = func(_ context.Context, _ string, args []string, _ []string, output io.Writer) error {
		if args[1] == "ps" {
			io.WriteString(output, strings.Repeat("a", 64))
			return nil
		}
		io.WriteString(output, `{"name":"/cove-ingress","labels":{"cove.managed":"ingress"},"ports":{"80/tcp":[{"HostIP":"not-an-address","HostPort":"8080"}]}}`)
		return nil
	}
	if _, err := e.firewallPorts(t.Context()); err == nil {
		t.Fatal("malformed host binding was offered as an openable port")
	}
}

func TestFirewallSSHProtectionUsesExactProcessNames(t *testing.T) {
	ports, err := inspectHostSSHPorts(t.Context(), func(_ context.Context, args ...string) (string, error) {
		switch args[0] {
		case "/usr/bin/ss":
			return `LISTEN 0 4096 127.0.0.53:53 0.0.0.0:* users:(("systemd-resolve",pid=80,fd=14))
LISTEN 0 4096 0.0.0.0:8443 0.0.0.0:* users:(("sshd-helper",pid=81,fd=15))
LISTEN 0 4096 0.0.0.0:2222 0.0.0.0:* users:(("sshd",pid=82,fd=16))
LISTEN 0 4096 [::]:2223 [::]:* users:(("systemd",pid=1,fd=17))
LISTEN 0 4096 0.0.0.0:2224 0.0.0.0:* users:(("sshd",pid=82,fd=18),("systemd",pid=1,fd=19))`, nil
		case "/usr/sbin/sshd":
			return "port 2200\nport 2201\n", nil
		default:
			return "", errors.New("unexpected command")
		}
	})
	slices.Sort(ports)
	ports = slices.Compact(ports)
	if err != nil || !reflect.DeepEqual(ports, []uint16{22, 2200, 2201, 2222, 2223, 2224}) {
		t.Fatal("SSH inventory included a process-name prefix or lost a real listener/configured port", ports, err)
	}
}

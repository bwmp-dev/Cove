package worker

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestHostFirewallPolicySyntaxOnOwnedLinuxVM(t *testing.T) {
	if os.Getenv("COVE_TEST_OWNED_HOST_INSPECTION") != "1" || os.Geteuid() != 0 {
		t.Skip("explicit disposable root Linux VM required")
	}
	owner := strings.Repeat("a7", 16)
	policy, err := renderFirewallRules(owner, []FirewallRule{{Protocol: "tcp", Port: 18080, Sources: []string{"192.0.2.0/24", "2001:db8::/64"}}, {Protocol: "udp", Port: 19000}}, false)
	if err != nil {
		t.Fatal(err)
	}
	filename := filepath.Join(t.TempDir(), "policy.nft")
	if err = os.WriteFile(filename, []byte(policy), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if _, err = snapshotHostCommand(ctx, "/usr/sbin/nft", "--check", "--file", filename); err != nil {
		t.Fatal("real nft parser refused generated policy", policy, err)
	}
	if _, err = snapshotHostCommand(ctx, "/usr/sbin/nft", "list", "table", "inet", firewallTable(owner)); err == nil {
		t.Fatal("validation unexpectedly installed firewall policy")
	}
}

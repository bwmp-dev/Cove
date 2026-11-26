package worker

import (
	"os"
	"strings"
	"testing"
)

func TestFirewallRemovalPreviewIsDistinctAndDeletesOnlyOwnTable(t *testing.T) {
	owner := strings.Repeat("a", 32)
	normal := FirewallControl{Rules: []FirewallRule{}, Available: []FirewallPort{{Protocol: "tcp", Port: 8080}}, Reserved: []uint16{22, 443}}
	removal := normal
	removal.Remove = true
	if firewallRevision(owner, "current", normal) == firewallRevision(owner, "current", removal) {
		t.Fatal("a normal empty-policy preview authorizes removing management")
	}
	for _, present := range []bool{true, false} {
		file, err := writeFirewallRemovalBatch(t.TempDir(), owner, present)
		if err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		want := "# Cove firewall removal\n"
		if present {
			want += "delete table inet " + firewallTable(owner) + "\n"
		}
		if string(data) != want {
			t.Fatal("removal touches something beyond the owned table", string(data))
		}
	}
}

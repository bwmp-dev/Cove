package worker

import (
	"strings"
	"testing"
)

func TestFirewallPolicyRefusesManagementAndForeignPorts(t *testing.T) {
	available := []FirewallPort{{Protocol: "tcp", Port: 8080}, {Protocol: "udp", Port: 9000}, {Protocol: "tcp", Port: 2222}}
	for _, rules := range [][]FirewallRule{
		{{Protocol: "tcp", Port: 2222}},
		{{Protocol: "tcp", Port: 22}},
		{{Protocol: "udp", Port: 8080}},
		{{Protocol: "tcp", Port: 8080, Sources: []string{"$(id)"}}},
		{{Protocol: "tcp", Port: 8080, Sources: []string{"::ffff:192.0.2.1/128"}}},
		{{Protocol: "tcp", Port: 8080}, {Protocol: "tcp", Port: 8080}},
	} {
		if _, err := normalizeFirewallRules(rules, available, []uint16{22, 2222, 443}); err == nil {
			t.Fatal("unsafe firewall policy accepted", rules)
		}
	}
	rules, err := normalizeFirewallRules([]FirewallRule{{Protocol: "tcp", Port: 8080, Sources: []string{"192.0.2.123/24", "192.0.2.0/24", "2001:db8::1/64"}}}, available, []uint16{22, 443})
	if err != nil || len(rules) != 1 || len(rules[0].Sources) != 2 {
		t.Fatal("CIDRs were not normalized", rules, err)
	}
	text, err := renderFirewallRules(strings.Repeat("a1", 16), rules, true)
	if err != nil || strings.Contains(text, "flush ruleset") || !strings.Contains(text, "policy accept") || !strings.Contains(text, "ct state established,related accept") || !strings.Contains(text, "tcp dport 8080 ip saddr 192.0.2.0/24 accept") || !strings.Contains(text, "tcp dport 8080 drop") || strings.Contains(text, "dport 22") {
		t.Fatal("policy touched unrelated management traffic", text, err)
	}
}

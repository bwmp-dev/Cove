package worker

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"sort"
	"strings"
)

type FirewallRule struct {
	Protocol string   `json:"protocol"`
	Port     uint16   `json:"port"`
	Sources  []string `json:"sources"`
}

type FirewallPort struct {
	Protocol string `json:"protocol"`
	Port     uint16 `json:"port"`
}

func firewallTable(owner string) string {
	digest := sha256.Sum256([]byte("cove firewall v1:" + owner))
	return "cove_guard_" + hex.EncodeToString(digest[:12])
}

func normalizeFirewallRules(rules []FirewallRule, available []FirewallPort, reserved []uint16) ([]FirewallRule, error) {
	if len(rules) > 32 || len(available) > 1024 || len(reserved) > 65535 {
		return nil, errors.New("firewall port inventory or policy exceeds its bounded size")
	}
	ports := map[FirewallPort]bool{}
	for _, port := range available {
		if port.Port == 0 || port.Protocol != "tcp" && port.Protocol != "udp" {
			return nil, errors.New("invalid published application port inventory")
		}
		ports[port] = true
	}
	protected := map[uint16]bool{}
	for _, port := range reserved {
		if port == 0 {
			return nil, errors.New("invalid reserved management port")
		}
		protected[port] = true
	}
	out := make([]FirewallRule, 0, len(rules))
	seen := map[FirewallPort]bool{}
	for _, rule := range rules {
		port := FirewallPort{Protocol: rule.Protocol, Port: rule.Port}
		if !ports[port] || protected[rule.Port] || seen[port] || len(rule.Sources) > 32 {
			return nil, errors.New("select each published Cove application port once; management ports cannot be restricted")
		}
		seen[port] = true
		rule.Sources = slices.Clone(rule.Sources)
		for i, value := range rule.Sources {
			if len(value) > 64 {
				return nil, errors.New("firewall source prefixes exceed their size limit")
			}
			prefix, err := netip.ParsePrefix(value)
			if err != nil || prefix.Addr().Is4In6() || prefix.Addr().Zone() != "" {
				return nil, errors.New("firewall sources must be IPv4 or IPv6 CIDR prefixes")
			}
			rule.Sources[i] = prefix.Masked().String()
		}
		sort.Strings(rule.Sources)
		rule.Sources = slices.Compact(rule.Sources)
		out = append(out, rule)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Protocol == out[j].Protocol {
			return out[i].Port < out[j].Port
		}
		return out[i].Protocol < out[j].Protocol
	})
	return out, nil
}

func renderFirewallRules(owner string, rules []FirewallRule, replace bool) (string, error) {
	if !snapshotOwnerID.MatchString(owner) {
		return "", errors.New("invalid firewall owner")
	}
	available := make([]FirewallPort, len(rules))
	for i, rule := range rules {
		available[i] = FirewallPort{Protocol: rule.Protocol, Port: rule.Port}
	}
	rules, err := normalizeFirewallRules(rules, available, nil)
	if err != nil {
		return "", err
	}
	var text strings.Builder
	table := firewallTable(owner)
	if replace {
		fmt.Fprintf(&text, "delete table inet %s\n", table)
	}
	fmt.Fprintf(&text, "table inet %s {\n comment \"Cove owner %s\"\n chain ingress {\n type filter hook prerouting priority -110; policy accept;\n ct state established,related accept\n", table, owner)
	for _, rule := range rules {
		for _, source := range rule.Sources {
			prefix, _ := netip.ParsePrefix(source)
			family := "ip6"
			if prefix.Addr().Is4() {
				family = "ip"
			}
			fmt.Fprintf(&text, " fib daddr type local meta l4proto %s %s dport %d %s saddr %s accept\n", rule.Protocol, rule.Protocol, rule.Port, family, source)
		}
		fmt.Fprintf(&text, " fib daddr type local meta l4proto %s %s dport %d drop\n", rule.Protocol, rule.Protocol, rule.Port)
	}
	text.WriteString(" }\n}\n")
	return text.String(), nil
}

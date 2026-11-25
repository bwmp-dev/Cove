package worker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

type FirewallControl struct {
	Action    string         `json:"action"`
	Rules     []FirewallRule `json:"rules"`
	Reserved  []uint16       `json:"reserved"`
	Available []FirewallPort `json:"available"`
	Revision  string         `json:"revision"`
	Intent    string         `json:"intent"`
}

type FirewallPending struct {
	ID       string    `json:"id"`
	Deadline time.Time `json:"deadline"`
}

type FirewallResult struct {
	Supported bool             `json:"supported"`
	Active    bool             `json:"active"`
	Rules     []FirewallRule   `json:"rules"`
	Available []FirewallPort   `json:"available"`
	Reserved  []uint16         `json:"reserved"`
	Revision  string           `json:"revision"`
	Pending   *FirewallPending `json:"pending,omitempty"`
	UFW       string           `json:"ufw"`
	NFTables  string           `json:"nftables"`
	Limited   bool             `json:"limited"`
	Notice    string           `json:"notice"`
	LastError string           `json:"last_error"`
}

func firewallUnit(owner string) string { return "cove-firewall-revert-" + firewallTable(owner) }

func inspectHostSSHPorts(ctx context.Context, run snapshotCommand) ([]uint16, error) {
	output, err := run(ctx, "/usr/bin/ss", "-H", "-ltnp")
	if err != nil || len(output) > 65536 {
		return nil, errors.New("host SSH listener inspection is unavailable")
	}
	ports := []uint16{22}
	for _, line := range strings.Split(output, "\n") {
		// systemd can own a socket-activated SSH listener; other systemd-* processes are not SSH.
		if !strings.Contains(line, `("sshd",`) && !strings.Contains(line, `("systemd",`) {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 5 {
			return nil, errors.New("invalid SSH listener inventory")
		}
		_, port, err := net.SplitHostPort(fields[3])
		if err != nil {
			index := strings.LastIndexByte(fields[3], ':')
			if index < 0 {
				return nil, errors.New("invalid SSH listener port")
			}
			port = fields[3][index+1:]
		}
		number, err := strconv.ParseUint(port, 10, 16)
		if err != nil || number == 0 {
			return nil, errors.New("invalid SSH listener port")
		}
		ports = append(ports, uint16(number))
	}
	if output, err := run(ctx, "/usr/sbin/sshd", "-T"); err == nil && len(output) <= 65536 {
		for _, line := range strings.Split(output, "\n") {
			fields := strings.Fields(line)
			if len(fields) == 2 && fields[0] == "port" {
				number, err := strconv.ParseUint(fields[1], 10, 16)
				if err != nil || number == 0 {
					return nil, errors.New("invalid configured SSH port")
				}
				ports = append(ports, uint16(number))
			}
		}
	}
	return ports, nil
}

func firewallRevision(owner, digest string, control FirewallControl) string {
	data, _ := json.Marshal(struct {
		Owner, Digest string
		Rules         []FirewallRule
		Available     []FirewallPort
		Reserved      []uint16
	}{owner, digest, control.Rules, control.Available, control.Reserved})
	hash := sha256.Sum256(data)
	return hex.EncodeToString(hash[:])
}

func stopFirewallTimer(ctx context.Context, owner string, run snapshotCommand) error {
	unit := firewallUnit(owner) + ".timer"
	load, err := run(ctx, "/usr/bin/systemctl", "list-units", "--all", "--plain", "--no-legend", "--full", unit)
	if err != nil || strings.TrimSpace(load) == "" {
		return err
	}
	description, err := run(ctx, "/usr/bin/systemctl", "show", unit, "--property=Description", "--value")
	if err != nil || strings.TrimSpace(description) != "Cove firewall recovery "+owner {
		return errors.New("refusing an unowned firewall recovery timer")
	}
	_, err = run(ctx, "/usr/bin/systemctl", "stop", unit)
	return err
}

func runHostFirewall(ctx context.Context, root, owner string, control FirewallControl, run snapshotCommand) (FirewallResult, error) {
	result := FirewallResult{Rules: []FirewallRule{}, Available: control.Available, Reserved: control.Reserved}
	if control.Action != "recover" && control.Action != "inspect" && control.Action != "preview" && control.Action != "apply" && control.Action != "confirm" && control.Action != "revert" {
		return result, errors.New("invalid firewall action")
	}
	if control.Action == "revert" {
		if err := revertFirewall(ctx, root, owner, "", run); err != nil {
			return result, err
		}
		if err := stopFirewallTimer(ctx, owner, run); err != nil {
			return result, err
		}
	}
	scope, err := hostPolicyScope(root, owner)
	if err != nil {
		return result, err
	}
	unlock, err := lockWorker(scope)
	if err != nil {
		return result, errors.New("another host maintenance change is active")
	}
	defer unlock()
	if err := cleanupFirewallArming(ctx, scope, owner, run); err != nil {
		return result, errors.New("firewall arming cleanup is pending; inspect host recovery before changing the firewall")
	}
	if control.Action == "recover" {
		result.Supported = true
		return result, nil
	}
	state, err := readFirewallJournal(scope, owner)
	if err != nil {
		return result, err
	}
	result.Rules, result.LastError = state.Rules, state.LastError
	present, comment, digest, err := inspectFirewallTable(ctx, owner, run)
	if err != nil {
		result.Notice = "Install nftables and verify host namespace privileges before changing the firewall."
		return result, nil
	}
	result.Active = present
	if present && comment != firewallGenerationComment(owner, state.Generation) && (state.Pending == nil || comment != firewallGenerationComment(owner, state.Pending.ID)) {
		return result, errors.New("Cove firewall table ownership differs from its protected journal")
	}
	if present && state.Pending == nil && state.Digest != digest {
		return result, errors.New("Cove firewall rules changed outside the protected policy; reconcile through a trusted console")
	}
	if raw, err := run(ctx, "/usr/sbin/ufw", "status", "verbose"); err == nil && len(raw) <= 65536 {
		result.UFW = raw
	} else {
		result.UFW = "UFW status unavailable."
	}
	if raw, err := run(ctx, "/usr/sbin/nft", "--stateless", "list", "ruleset"); err == nil {
		if len(raw) > 65536 {
			raw, result.Limited = raw[:65536], true
		}
		result.NFTables = raw
	} else {
		result.NFTables = "External nftables rules could not be read within the inspection limit."
		result.Limited = true
	}
	sshPorts, err := inspectHostSSHPorts(ctx, run)
	if err != nil {
		result.Notice = "Host SSH listener inspection is unavailable; firewall changes are refused."
		return result, nil
	}
	control.Reserved = append(slices.Clone(control.Reserved), sshPorts...)
	slices.Sort(control.Reserved)
	control.Reserved = slices.Compact(control.Reserved)
	result.Reserved = control.Reserved
	if _, err = os.Stat("/run/systemd/system"); err != nil {
		result.Notice = "A running host systemd manager is required for independent timed firewall recovery."
		return result, nil
	}
	result.Supported = true
	if pending := state.Pending; pending != nil {
		result.Pending = &FirewallPending{ID: pending.ID, Deadline: pending.Deadline}
		if present && comment == firewallGenerationComment(owner, pending.ID) {
			result.Rules = pending.Desired
		}
		if control.Action != "confirm" && control.Action != "inspect" {
			return result, errors.New("confirm or revert the pending firewall change first")
		}
	}
	if control.Action == "confirm" {
		if state.Pending == nil || control.Intent != state.Pending.ID || !time.Now().Before(state.Pending.Deadline) || !present || comment != firewallGenerationComment(owner, control.Intent) || state.Pending.Digest == "" || state.Pending.Digest != digest {
			return result, errors.New("firewall confirmation is stale or its owned table changed; inspect recovery status")
		}
		state.Reserved = state.Pending.Reserved
		state.Rules, state.Generation, state.Digest, state.Pending, state.LastError = state.Pending.Desired, state.Pending.ID, digest, nil, ""
		if err = writeJSON(filepath.Join(scope, "firewall.json"), state); err != nil {
			return result, err
		}
		if err = stopFirewallTimer(ctx, owner, run); err != nil {
			return result, err
		}
		result.Rules, result.Pending, result.LastError = state.Rules, nil, ""
		return result, nil
	}
	if control.Action == "inspect" || control.Action == "revert" {
		return result, nil
	}
	control.Rules, err = normalizeFirewallRules(control.Rules, control.Available, control.Reserved)
	if err != nil {
		return result, err
	}
	sort.Slice(control.Available, func(i, j int) bool {
		if control.Available[i].Protocol == control.Available[j].Protocol {
			return control.Available[i].Port < control.Available[j].Port
		}
		return control.Available[i].Protocol < control.Available[j].Protocol
	})
	result.Revision = firewallRevision(owner, digest, control)
	if control.Action == "apply" && control.Revision != result.Revision {
		return result, errors.New("firewall preview is stale; preview the current ports and rules again")
	}
	id, err := firewallChangeID()
	if err != nil {
		return result, err
	}
	filename, err := writeFirewallBatch(scope, owner, id, control.Rules, present)
	if err != nil {
		return result, err
	}
	if _, err = run(ctx, "/usr/sbin/nft", "--check", "--file", filename); err != nil {
		return result, errors.New("native nftables validation refused the proposed application-port policy")
	}
	result.Rules = control.Rules
	if control.Action == "preview" {
		return result, nil
	}
	binary, err := prepareFirewallReverter(scope)
	if err != nil {
		return result, err
	}
	state.Pending = &firewallIntent{ID: id, Previous: state.Rules, PreviousPresent: present, Desired: control.Rules, Reserved: control.Reserved, AttemptBound: true}
	if err = armFirewallRecovery(ctx, root, owner, scope, binary, state, run); err != nil {
		return result, err
	}
	if _, err = run(ctx, "/usr/sbin/nft", "--file", filename); err != nil {
		return result, errors.New("firewall apply could not be confirmed; independent recovery remains armed")
	}
	_, appliedComment, appliedDigest, err := inspectFirewallTable(ctx, owner, run)
	if err != nil || appliedComment != firewallGenerationComment(owner, id) {
		return result, errors.New("firewall apply receipt could not be verified; independent recovery remains armed")
	}
	state.Pending.Digest = appliedDigest
	if err = writeJSON(filepath.Join(scope, "firewall.json"), state); err != nil {
		return result, err
	}
	result.Active, result.Pending = true, &FirewallPending{ID: id, Deadline: state.Pending.Deadline}
	return result, nil
}

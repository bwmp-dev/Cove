package worker

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
)

func firewallBootUnit(owner string) string {
	return "cove-firewall-policy-" + firewallTable(owner) + ".service"
}

func installFirewallBootPolicy(ctx context.Context, root, owner, scope, binary string, run snapshotCommand) error {
	marker := "# Managed by Cove firewall " + owner + "\n"
	unit := firewallBootUnit(owner)
	data := marker + "[Unit]\nDescription=Cove application-port firewall recovery\nAfter=local-fs.target network.target ssh.service sshd.service ssh.socket\nBefore=docker.service\n\n[Service]\nType=oneshot\nExecStart=" + binary + " firewall-restore " + root + " " + owner + "\nTimeoutStartSec=30s\nStandardOutput=null\nStandardError=journal\nLogRateLimitIntervalSec=30s\nLogRateLimitBurst=10\n\n[Install]\nWantedBy=multi-user.target\n"
	if err := writeHostPolicyFile(filepath.Join("/etc/systemd/system", unit), scope, marker, data); err != nil {
		return err
	}
	if _, err := run(ctx, "/usr/bin/systemctl", "daemon-reload"); err != nil {
		return errors.New("firewall boot recovery could not be loaded; no new firewall policy was applied")
	}
	if _, err := run(ctx, "/usr/bin/systemctl", "enable", unit); err != nil {
		return errors.New("firewall boot recovery could not be enabled; no new firewall policy was applied")
	}
	return nil
}

func firewallRecoveryRules(ctx context.Context, rules []FirewallRule, reserved []uint16, run snapshotCommand) ([]FirewallRule, []uint16, bool, error) {
	ports, err := inspectHostSSHPorts(ctx, run)
	if err != nil {
		return nil, nil, false, err
	}
	reserved = append(slices.Clone(reserved), ports...)
	slices.Sort(reserved)
	reserved = slices.Compact(reserved)
	protected := make(map[uint16]bool, len(reserved))
	for _, port := range reserved {
		protected[port] = true
	}
	kept := make([]FirewallRule, 0, len(rules))
	for _, rule := range rules {
		if !protected[rule.Port] {
			kept = append(kept, rule)
		}
	}
	return kept, reserved, len(kept) != len(rules), nil
}

func RunFirewallRestore(ctx context.Context, root, owner string) error {
	if err := validateHostMaintenance(HostMaintenanceRequest{Action: "inspect", Root: root, Owner: owner}); err != nil {
		return err
	}
	scope, err := hostPolicyScope(root, owner)
	if err != nil {
		return err
	}
	cleanupUnlock, err := lockWorker(scope)
	if err != nil {
		return err
	}
	err = cleanupFirewallArming(ctx, scope, owner, snapshotHostCommand)
	if err == nil {
		state, stateErr := readFirewallJournal(scope, owner)
		err = stateErr
		if err == nil && state.Removed {
			err = cleanupFirewallManagement(ctx, scope, owner, snapshotHostCommand)
		}
	}
	cleanupUnlock()
	if err != nil {
		return err
	}
	state, err := readFirewallJournal(scope, owner)
	if err != nil {
		return err
	}
	if state.Pending != nil {
		return revertFirewall(ctx, root, owner, "", snapshotHostCommand)
	}
	unlock, err := lockWorker(scope)
	if err != nil {
		return err
	}
	defer unlock()
	state, err = readFirewallJournal(scope, owner)
	if err != nil || state.Pending != nil {
		return errors.Join(err, errors.New("firewall state changed while boot recovery was starting"))
	}
	if state.Generation == "" {
		return nil
	}
	present, comment, digest, err := inspectFirewallTable(ctx, owner, snapshotHostCommand)
	if err != nil {
		return err
	}
	if present && (comment != firewallGenerationComment(owner, state.Generation) || digest != state.Digest) {
		return errors.New("boot recovery refused externally changed firewall rules")
	}
	rules, reserved, removed, err := firewallRecoveryRules(ctx, state.Rules, state.Reserved, snapshotHostCommand)
	if err != nil {
		return err
	}
	filename, err := writeFirewallBatch(scope, owner, state.Generation, rules, present)
	if err != nil {
		return err
	}
	if _, err = snapshotHostCommand(ctx, "/usr/sbin/nft", "--file", filename); err != nil {
		return errors.New("boot recovery could not restore the owned application-port policy")
	}
	_, _, state.Digest, err = inspectFirewallTable(ctx, owner, snapshotHostCommand)
	if err != nil {
		return err
	}
	state.Rules, state.Reserved = rules, reserved
	if removed {
		state.LastError = "Rules conflicting with current management listeners were removed during recovery."
	}
	return writeJSON(filepath.Join(scope, "firewall.json"), state)
}

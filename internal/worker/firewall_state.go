package worker

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type firewallIntent struct {
	Remove          bool           `json:"remove,omitempty"`
	ID              string         `json:"id"`
	AttemptBound    bool           `json:"attempt_bound,omitempty"`
	Previous        []FirewallRule `json:"previous"`
	PreviousPresent bool           `json:"previous_present"`
	Desired         []FirewallRule `json:"desired"`
	Digest          string         `json:"digest"`
	Deadline        time.Time      `json:"deadline"`
	Reserved        []uint16       `json:"reserved"`
}

type firewallJournal struct {
	Removed    bool            `json:"removed,omitempty"`
	Owner      string          `json:"owner"`
	Rules      []FirewallRule  `json:"rules"`
	Generation string          `json:"generation"`
	Digest     string          `json:"digest"`
	Pending    *firewallIntent `json:"pending,omitempty"`
	LastError  string          `json:"last_error"`
	Reserved   []uint16        `json:"reserved"`
}

func readFirewallJournal(scope, owner string) (firewallJournal, error) {
	state := firewallJournal{Owner: owner, Rules: []FirewallRule{}}
	if err := readSnapshotMetadata(filepath.Join(scope, "firewall.json"), &state); os.IsNotExist(err) {
		return state, nil
	} else if err != nil {
		return state, err
	}
	if state.Removed && (state.Pending != nil || state.Generation != "" || state.Digest != "" || len(state.Rules) != 0) || state.Owner != owner || state.Generation != "" && !snapshotOwnerID.MatchString(state.Generation) || state.Digest != "" && !snapshotIdentifier.MatchString(state.Digest) || len(state.LastError) > 1024 || len(state.Reserved) > 65535 {
		return state, errors.New("invalid host firewall ownership state")
	}
	if _, err := renderFirewallRules(owner, state.Rules, false); err != nil {
		return state, err
	}
	if pending := state.Pending; pending != nil {
		if !snapshotOwnerID.MatchString(pending.ID) || pending.Digest != "" && !snapshotIdentifier.MatchString(pending.Digest) || pending.Deadline.IsZero() || pending.Deadline.After(time.Now().Add(3*time.Minute)) || len(pending.Reserved) > 65535 {
			return state, errors.New("invalid host firewall recovery lease")
		}
		if _, err := renderFirewallRules(owner, pending.Previous, false); err != nil {
			return state, err
		}
		if _, err := renderFirewallRules(owner, pending.Desired, false); err != nil {
			return state, err
		}
	}
	return state, nil
}

func firewallGenerationComment(owner, generation string) string {
	return "Cove owner " + owner + " generation " + generation
}

func inspectFirewallTable(ctx context.Context, owner string, run snapshotCommand) (bool, string, string, error) {
	raw, err := run(ctx, "/usr/sbin/nft", "--json", "list", "tables")
	if err != nil || len(raw) > 256<<10 {
		return false, "", "", errors.New("bounded nftables inspection is unavailable")
	}
	var report struct {
		NFTables []json.RawMessage `json:"nftables"`
	}
	if json.Unmarshal([]byte(raw), &report) != nil || len(report.NFTables) > 1024 {
		return false, "", "", errors.New("invalid nftables inventory")
	}
	present := false
	for _, value := range report.NFTables {
		var item struct{ Table struct{ Family, Name string } }
		if json.Unmarshal(value, &item) != nil {
			return false, "", "", errors.New("invalid nftables table inventory")
		}
		present = present || item.Table.Family == "inet" && item.Table.Name == firewallTable(owner)
	}
	if !present {
		return false, "", "", nil
	}
	raw, err = run(ctx, "/usr/sbin/nft", "--json", "list", "table", "inet", firewallTable(owner))
	if err != nil || len(raw) > 256<<10 || json.Unmarshal([]byte(raw), &report) != nil || len(report.NFTables) > 4096 {
		return false, "", "", errors.New("bounded Cove firewall inspection is unavailable")
	}
	comment := ""
	for _, value := range report.NFTables {
		var item struct {
			Table struct{ Family, Name, Comment string }
		}
		if json.Unmarshal(value, &item) != nil {
			return false, "", "", errors.New("invalid Cove firewall table")
		}
		if item.Table.Name == firewallTable(owner) && item.Table.Family == "inet" {
			comment = item.Table.Comment
		}
	}
	digest := sha256.Sum256([]byte(raw))
	return true, comment, hex.EncodeToString(digest[:]), nil
}

func writeFirewallBatch(scope, owner, generation string, rules []FirewallRule, present bool) (string, error) {
	if !snapshotOwnerID.MatchString(generation) {
		return "", errors.New("invalid firewall generation")
	}
	text, err := renderFirewallRules(owner, rules, present)
	if err != nil {
		return "", err
	}
	text = strings.Replace(text, "Cove owner "+owner, firewallGenerationComment(owner, generation), 1)
	return writeFirewallChangeFile(scope, text)
}

func writeFirewallRemovalBatch(scope, owner string, present bool) (string, error) {
	text := "# Cove firewall removal\n"
	if present {
		text += "delete table inet " + firewallTable(owner) + "\n"
	}
	return writeFirewallChangeFile(scope, text)
}

func writeFirewallChangeFile(scope, text string) (string, error) {
	filename := filepath.Join(scope, "firewall.nft")
	if info, err := os.Lstat(filename); err == nil && (!info.Mode().IsRegular() || info.Size() > 256<<10) {
		return "", errors.New("host firewall staging path is unsafe")
	} else if err != nil && !os.IsNotExist(err) {
		return "", err
	}
	if err := os.WriteFile(filename, []byte(text), 0600); err != nil {
		return "", err
	}
	return filename, nil
}

func prepareFirewallReverter(scope string) (string, error) {
	source, err := os.Executable()
	if err != nil {
		return "", err
	}
	input, err := os.Open(source)
	if err != nil {
		return "", err
	}
	defer input.Close()
	info, err := input.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > 64<<20 {
		return "", errors.New("trusted firewall reverter binary exceeds its limit")
	}
	destination := filepath.Join(scope, "firewall-reverter")
	stage := destination + ".next"
	for _, path := range []string{destination, stage} {
		if existing, err := os.Lstat(path); err == nil {
			uid, _ := fileOwner(existing)
			if !existing.Mode().IsRegular() || existing.Size() > 64<<20 || uid != 0 || existing.Mode().Perm()&0022 != 0 {
				return "", errors.New("firewall reverter is not protected")
			}
		} else if !os.IsNotExist(err) {
			return "", err
		}
	}
	file, err := os.OpenFile(stage, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0700)
	if err != nil {
		return "", err
	}
	written, err := io.Copy(file, io.LimitReader(input, 64<<20+1))
	if written > 64<<20 {
		err = errors.New("trusted firewall reverter binary changed beyond its size limit")
	}
	if err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err = errors.Join(err, closeErr); err != nil {
		return "", err
	}
	if err = os.Rename(stage, destination); err != nil {
		return "", err
	}
	return destination, syncDirectory(scope)
}

func revertFirewall(ctx context.Context, root, owner, attempt string, run snapshotCommand) error {
	scope, err := hostPolicyScope(root, owner)
	if err != nil {
		return err
	}
	unlock, err := lockWorker(scope)
	if err != nil {
		return err
	}
	defer unlock()
	return revertFirewallLocked(ctx, scope, owner, attempt, run)
}

func revertFirewallLocked(ctx context.Context, scope, owner, attempt string, run snapshotCommand) error {
	if _, err := os.Lstat(filepath.Join(scope, "firewall-arming.json")); err == nil {
		fmt.Fprintln(os.Stderr, "Cove firewall recovery: arming was not committed; cleanup is pending, no firewall policy was applied")
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	state, err := readFirewallJournal(scope, owner)
	if err != nil {
		return err
	}
	if state.Removed {
		return cleanupFirewallManagement(ctx, scope, owner, run)
	}
	if state.Pending == nil || attempt != "" && (attempt == "legacy" && state.Pending.AttemptBound || attempt != "legacy" && attempt != state.Pending.ID) {
		fmt.Fprintln(os.Stderr, "Cove firewall recovery: stale attempt ignored; no matching pending revision")
		return nil
	}
	present, comment, digest, err := inspectFirewallTable(ctx, owner, run)
	if err != nil {
		return err
	}
	pending := state.Pending
	if present && comment != firewallGenerationComment(owner, pending.ID) && comment != firewallGenerationComment(owner, state.Generation) {
		return errors.New("firewall recovery refused a table with changed ownership")
	}
	if present && (comment == firewallGenerationComment(owner, pending.ID) && pending.Digest != "" && digest != pending.Digest || comment == firewallGenerationComment(owner, state.Generation) && state.Digest != "" && digest != state.Digest) {
		return errors.New("firewall recovery refused externally changed rules in its owned table")
	}
	rules, reserved, removed, err := firewallRecoveryRules(ctx, pending.Previous, append(state.Reserved, pending.Reserved...), run)
	if err != nil {
		return err
	}
	if pending.PreviousPresent {
		filename, err := writeFirewallBatch(scope, owner, state.Generation, rules, present)
		if err != nil {
			return err
		}
		if _, err = run(ctx, "/usr/sbin/nft", "--file", filename); err != nil {
			return errors.New("Cove firewall recovery failed; inspect the owned table through a trusted console")
		}
	} else if present {
		if _, err = run(ctx, "/usr/sbin/nft", "delete", "table", "inet", firewallTable(owner)); err != nil {
			return errors.New("temporary Cove firewall table could not be reverted")
		}
	}
	_, _, state.Digest, err = inspectFirewallTable(ctx, owner, run)
	if err != nil {
		return err
	}
	if !pending.PreviousPresent && state.Generation == "" {
		return commitFirewallRemoval(ctx, scope, owner, run)
	}
	state.Rules, state.Reserved, state.Pending, state.LastError = rules, reserved, nil, "The unconfirmed firewall change was reverted."
	if removed {
		state.LastError += " Rules conflicting with current management listeners were removed."
	}
	return writeJSON(filepath.Join(scope, "firewall.json"), state)
}

func firewallChangeID() (string, error) {
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(nonce[:]), nil
}

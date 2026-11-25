package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

type firewallHostRequest struct {
	Root    string          `json:"root"`
	Owner   string          `json:"owner"`
	Control FirewallControl `json:"control"`
}

func RunHostFirewall(ctx context.Context, input io.Reader, output io.Writer) error {
	decoder := json.NewDecoder(io.LimitReader(input, 1<<20+1))
	decoder.DisallowUnknownFields()
	var request firewallHostRequest
	if decoder.Decode(&request) != nil {
		return errors.New("invalid host firewall request")
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		return errors.New("unexpected host firewall data")
	}
	if err := validateHostMaintenance(HostMaintenanceRequest{Action: "inspect", Root: request.Root, Owner: request.Owner}); err != nil {
		return err
	}
	result, err := runHostFirewall(ctx, request.Root, request.Owner, request.Control, snapshotHostCommand)
	response := struct {
		State FirewallResult `json:"state"`
		Error string         `json:"error,omitempty"`
	}{State: result}
	if err != nil {
		response.Error = err.Error()
	}
	return json.NewEncoder(output).Encode(response)
}

func RunFirewallRevert(ctx context.Context, root, owner string) error {
	if err := validateHostMaintenance(HostMaintenanceRequest{Action: "inspect", Root: root, Owner: owner}); err != nil {
		return err
	}
	return revertFirewall(ctx, root, owner, "legacy", snapshotHostCommand)
}

func RunFirewallRevertAttempt(ctx context.Context, root, owner, attempt string) error {
	if err := validateHostMaintenance(HostMaintenanceRequest{Action: "inspect", Root: root, Owner: owner}); err != nil {
		return err
	}
	if !snapshotOwnerID.MatchString(attempt) {
		return errors.New("invalid firewall recovery attempt")
	}
	return revertFirewall(ctx, root, owner, attempt, snapshotHostCommand)
}

func (e *Engine) firewallPorts(ctx context.Context) ([]FirewallPort, error) {
	raw, err := e.output(ctx, "docker", "ps", "-q", "--no-trunc", "--filter", "label=com.docker.compose.project")
	if err != nil {
		return nil, err
	}
	ids := strings.Fields(raw)
	if len(ids) > 512 {
		return nil, errors.New("firewall inspection supports at most 512 running Compose containers")
	}
	for _, managed := range []string{"ingress", "link"} {
		shared, err := e.output(ctx, "docker", "ps", "-q", "--no-trunc", "--filter", "label=cove.managed="+managed)
		if err != nil {
			return nil, err
		}
		for _, id := range strings.Fields(shared) {
			if !slices.Contains(ids, id) {
				ids = append(ids, id)
			}
		}
	}
	if len(ids) == 0 {
		return []FirewallPort{}, nil
	}
	if len(ids) > 514 {
		return nil, errors.New("firewall container inventory exceeds its limit")
	}
	for _, id := range ids {
		if !fileContainerID.MatchString(id) {
			return nil, errors.New("invalid container port inventory")
		}
	}
	format := `{"name":{{json .Name}},"labels":{{json .Config.Labels}},"ports":{{json .NetworkSettings.Ports}}}`
	raw, err = e.output(ctx, append([]string{"docker", "inspect", "--format", format}, ids...)...)
	if err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(strings.NewReader(raw))
	ports := map[FirewallPort]bool{}
	for {
		var item struct {
			Name   string
			Labels map[string]string
			Ports  map[string][]struct{ HostIP, HostPort string }
		}
		if err := decoder.Decode(&item); err == io.EOF {
			break
		} else if err != nil {
			return nil, err
		}
		sharedInfrastructure := item.Name == "/cove-ingress" && item.Labels["cove.managed"] == "ingress" || item.Name == "/"+linkGatewayName && item.Labels["cove.managed"] == "link"
		workload := strings.TrimPrefix(item.Labels["com.docker.compose.project"], "cove-")
		if !sharedInfrastructure && (workload == item.Labels["com.docker.compose.project"] || !workloadIdentifier.MatchString(workload)) {
			continue
		}
		if !sharedInfrastructure {
			info, err := os.Lstat(filepath.Join(e.Root, "workloads", workload))
			if os.IsNotExist(err) {
				continue
			}
			if err != nil || !info.IsDir() {
				return nil, errors.New("resource ownership inventory is unavailable")
			}
		}
		for destination, bindings := range item.Ports {
			_, protocol, ok := strings.Cut(destination, "/")
			if !ok || protocol != "tcp" && protocol != "udp" {
				return nil, errors.New("unsupported published port protocol")
			}
			for _, binding := range bindings {
				number, err := strconv.ParseUint(binding.HostPort, 10, 16)
				if err != nil || number == 0 {
					return nil, errors.New("invalid published application port")
				}
				if binding.HostIP != "" {
					address, err := netip.ParseAddr(binding.HostIP)
					if err != nil {
						return nil, errors.New("invalid published application address")
					}
					if address.Unmap().IsLoopback() {
						continue
					}
				}
				ports[FirewallPort{Protocol: protocol, Port: uint16(number)}] = true
				if len(ports) > 1024 {
					return nil, errors.New("firewall published-port inventory exceeds its limit")
				}
			}
		}
	}
	out := make([]FirewallPort, 0, len(ports))
	for port := range ports {
		out = append(out, port)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Protocol == out[j].Protocol {
			return out[i].Port < out[j].Port
		}
		return out[i].Protocol < out[j].Protocol
	})
	return out, nil
}

func (e *Engine) ConfigureFirewall(ctx context.Context, control FirewallControl) (state *FirewallResult, result error) {
	unlock, err := e.lockNodeChange(false)
	if err != nil {
		return nil, err
	}
	defer unlock()
	if len(control.Available) != 0 || len(control.Reserved) == 0 || len(control.Reserved) > 65535 || len(control.Revision) > 64 || len(control.Intent) > 32 {
		return nil, errors.New("firewall requests require a bounded panel management-port inventory")
	}
	if !e.ingressMu.TryLock() {
		return nil, errors.New("wait for the current proxy or firewall change")
	}
	defer e.ingressMu.Unlock()
	e.mu.Lock()
	if len(e.active) != 0 || e.draining || e.customRunning || e.customUnsettled || e.hostChanging || e.proxyUnsettled || e.proxyRunning {
		e.mu.Unlock()
		return nil, errors.New("wait for worker operations and recovery before inspecting or changing the firewall")
	}
	e.hostChanging = true
	e.mu.Unlock()
	defer func() { e.mu.Lock(); e.hostChanging = false; e.mu.Unlock() }()
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	available, err := e.firewallPorts(ctx)
	if err != nil {
		return nil, err
	}
	control.Available = available
	return e.runFirewallHost(ctx, control)
}

func (e *Engine) runFirewallHost(ctx context.Context, control FirewallControl) (state *FirewallResult, result error) {
	owner, err := e.backupHelperOwner(true)
	if err != nil {
		return nil, err
	}
	binary, err := e.prepareSnapshotHostBinary()
	if err != nil {
		return nil, err
	}
	payload, err := json.Marshal(firewallHostRequest{Root: e.Root, Owner: owner, Control: control})
	if err != nil || len(payload) > 1<<20 {
		return nil, errors.New("firewall request exceeds its size limit")
	}
	name := "cove-firewall-" + owner
	args := []string{"docker", "run", "--rm", "--interactive", "--name", name, "--privileged", "--pid", "host", "--network", "none", "--read-only", "--log-driver", "none", "--memory", "256m", "--cpus", "1", "--pids-limit", "128", "--label", "dev.cove.backup-owner=" + owner, "--label", "dev.cove.operation=firewall", "--entrypoint", "nsenter", "busybox:1.37", "-t", "1", "-m", "-n", "-r", "-w", "--", binary, "host-firewall"}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		result = errors.Join(result, e.removeBackupHelper(cleanup, owner, name, "firewall"))
	}()
	out := &limitedBuffer{Limit: 1 << 20}
	if err = runInput(ctx, e.Root, args, nil, bytes.NewReader(payload), out); err != nil {
		return nil, errors.New("host firewall request could not be confirmed; inspect the node and recovery timer")
	}
	var response struct {
		State FirewallResult `json:"state"`
		Error string         `json:"error"`
	}
	if json.Unmarshal(out.data.Bytes(), &response) != nil {
		return nil, errors.New("invalid firewall helper response")
	}
	if response.Error != "" {
		return &response.State, errors.New(response.Error)
	}
	return &response.State, nil
}

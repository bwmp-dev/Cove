package worker

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type ProxyTrafficControl struct {
	Action   string           `json:"action"`
	Settings *TrafficSettings `json:"settings,omitempty"`
	Revision string           `json:"revision,omitempty"`
}
type ProxyTrafficState struct {
	Settings   TrafficSettings `json:"settings"`
	Configured bool            `json:"configured"`
	Pending    bool            `json:"pending"`
	Notice     string          `json:"notice"`
}
type ProxyTrafficPlan struct {
	Revision string          `json:"revision"`
	Settings TrafficSettings `json:"settings"`
}
type proxyTrafficJournal struct {
	OldID      string          `json:"old_id"`
	NewID      string          `json:"new_id"`
	BackupName string          `json:"backup_name"`
	Nonce      string          `json:"nonce"`
	Previous   TrafficSettings `json:"previous"`
	Committed  bool            `json:"committed"`
}

func (e *Engine) trafficRecoveryPending() bool {
	_, err := os.Stat(filepath.Join(e.Root, "proxy-traffic-change.json"))
	return !os.IsNotExist(err)
}

func (e *Engine) recoverTrafficOnStartup(ctx context.Context) error {
	if !e.trafficRecoveryPending() {
		return nil
	}
	_, _, err := e.ConfigureTraffic(ctx, ProxyTrafficControl{Action: "recover"})
	return err
}

type dockerStatusError struct{ status int }

func (e dockerStatusError) Error() string {
	return fmt.Sprintf("Docker request rejected (%d)", e.status)
}
func dockerNotFound(err error) bool {
	var status dockerStatusError
	return errors.As(err, &status) && status.status == http.StatusNotFound
}

func (e *Engine) dockerSocketClient(ctx context.Context) (*http.Client, error) {
	host := os.Getenv("DOCKER_HOST")
	if host == "" {
		var err error
		host, err = e.output(ctx, "docker", "context", "inspect", "--format", "{{.Endpoints.docker.Host}}")
		if err != nil {
			return nil, errors.New("Docker context unavailable")
		}
	}
	u, err := url.Parse(host)
	if err != nil || u.Scheme != "unix" || u.Path == "" || u.Host != "" {
		return nil, errors.New("proxy reconfiguration requires the worker's local Unix Docker socket")
	}
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "unix", u.Path)
	}}
	return &http.Client{Transport: transport, Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, nil
}

func dockerRequest(ctx context.Context, client *http.Client, method, path string, body any, out any) error {
	var data []byte
	var err error
	if body != nil {
		data, err = json.Marshal(body)
		if err != nil {
			return err
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://docker"+path, bytes.NewReader(data))
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	response, err := client.Do(req)
	if err != nil {
		return errors.New("Docker request unavailable")
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 && response.StatusCode != 304 {
		return dockerStatusError{status: response.StatusCode}
	}
	if out != nil && response.StatusCode != 304 {
		data, err := io.ReadAll(io.LimitReader(response.Body, 2*1024*1024+1))
		if err != nil || len(data) > 2*1024*1024 {
			return errors.New("Docker response exceeds proxy configuration limit")
		}
		if json.Unmarshal(data, out) != nil {
			return errors.New("invalid Docker response")
		}
	}
	return nil
}

func trafficCommand(cmd []any, settings TrafficSettings) ([]any, error) {
	if len(cmd) == 0 {
		return nil, errors.New("proxy command unavailable")
	}
	if cmd[0] == "-ec" {
		if len(cmd) < 3 || cmd[2] != "traefik" {
			return nil, errors.New("unsupported managed proxy command")
		}
		script, ok := cmd[1].(string)
		if !ok || !strings.Contains(script, `exec traefik "$@"`) {
			return nil, errors.New("unsupported managed proxy entrypoint")
		}
	}
	out := []any{}
	for _, value := range cmd {
		arg, ok := value.(string)
		if !ok {
			return nil, errors.New("invalid managed proxy command")
		}
		lower := strings.ToLower(arg)
		for _, entrypoint := range []string{"web", "websecure"} {
			for _, kind := range []string{"forwardedheaders", "proxyprotocol"} {
				key := "--entrypoints." + entrypoint + "." + kind + ".insecure"
				if lower == key || lower == key+"=true" {
					return nil, errors.New("disable insecure forwarded-header and proxy-protocol trust before configuring traffic")
				}
			}
		}
		if strings.HasPrefix(lower, "--accesslog") || strings.HasPrefix(lower, "--entrypoints.web.forwardedheaders.trustedips=") || strings.HasPrefix(lower, "--entrypoints.websecure.forwardedheaders.trustedips=") {
			continue
		}
		out = append(out, arg)
	}
	if settings.Enabled {
		out = append(out, "--accesslog=true", "--accesslog.format=json", "--accesslog.bufferingsize=0", "--accesslog.fields.defaultmode=drop", "--accesslog.fields.headers.defaultmode=drop", "--accesslog.fields.headers.names.X-Forwarded-For=keep")
		for _, field := range []string{"RouterName", "ClientHost", "Duration", "DownstreamStatus", "DownstreamContentSize"} {
			out = append(out, "--accesslog.fields.names."+field+"=keep")
		}
	}
	if len(settings.TrustedProxies) > 0 {
		trusted := strings.Join(settings.TrustedProxies, ",")
		out = append(out, "--entrypoints.web.forwardedheaders.trustedips="+trusted, "--entrypoints.websecure.forwardedheaders.trustedips="+trusted)
	}
	return out, nil
}

func cloneTrafficProxy(inspect map[string]any, settings TrafficSettings, nonce string) (map[string]any, error) {
	config, ok := inspect["Config"].(map[string]any)
	if !ok {
		return nil, errors.New("proxy configuration unavailable")
	}
	labels, _ := config["Labels"].(map[string]any)
	if labels["cove.managed"] != "ingress" {
		return nil, errors.New("proxy is not owned by Cove")
	}
	image, _ := inspect["Image"].(string)
	if !imageDigest.MatchString(image) {
		return nil, errors.New("proxy immutable image unavailable")
	}
	cmd, _ := config["Cmd"].([]any)
	command, err := trafficCommand(cmd, settings)
	if err != nil {
		return nil, err
	}
	config["Cmd"] = command
	config["Image"] = image
	labels["cove.traffic-change"] = nonce
	config["Labels"] = labels
	host, ok := inspect["HostConfig"].(map[string]any)
	if !ok {
		return nil, errors.New("proxy host configuration unavailable")
	}
	if host["AutoRemove"] == true {
		return nil, errors.New("auto-remove proxies cannot be safely reconfigured")
	}
	// Image-declared anonymous volumes must stay bound to the original data during replacement.
	mounts, _ := inspect["Mounts"].([]any)
	binds, _ := host["Binds"].([]any)
	configuredMounts, _ := host["Mounts"].([]any)
	for _, value := range mounts {
		mount, _ := value.(map[string]any)
		if mount["Type"] != "volume" {
			continue
		}
		name, _ := mount["Name"].(string)
		target, _ := mount["Destination"].(string)
		if !fileVolumeName.MatchString(name) || !strings.HasPrefix(target, "/") || strings.Contains(target, ":") {
			return nil, errors.New("proxy volume cannot be safely rebound")
		}
		bound := false
		for _, item := range configuredMounts {
			m, _ := item.(map[string]any)
			if m["Target"] == target {
				m["Source"] = name
				bound = true
			}
		}
		for _, item := range binds {
			binding, _ := item.(string)
			parts := strings.Split(binding, ":")
			if len(parts) > 1 && parts[1] == target {
				bound = true
			}
		}
		if !bound {
			mode := "rw"
			if mount["RW"] != true {
				mode = "ro"
			}
			binds = append(binds, name+":"+target+":"+mode)
		}
	}
	host["Binds"] = binds
	host["Mounts"] = configuredMounts
	if mode, _ := host["NetworkMode"].(string); mode == "host" || strings.HasPrefix(mode, "container:") {
		return nil, errors.New("host/shared-network-namespace proxy recreation needs manual migration")
	}
	host["LogConfig"] = map[string]any{"Type": "json-file", "Config": map[string]string{"max-size": "4m", "max-file": "3"}}
	config["HostConfig"] = host
	networks, _ := inspect["NetworkSettings"].(map[string]any)
	endpoints, _ := networks["Networks"].(map[string]any)
	connections := map[string]any{}
	oldID, _ := inspect["Id"].(string)
	for name, value := range endpoints {
		endpoint, _ := value.(map[string]any)
		ipam, _ := endpoint["IPAMConfig"].(map[string]any)
		if len(ipam) > 0 {
			return nil, errors.New("static proxy IP assignments need a separately planned recreation")
		}
		aliases := []any{}
		list, _ := endpoint["Aliases"].([]any)
		for _, alias := range list {
			if alias != oldID && alias != oldID[:min(12, len(oldID))] {
				aliases = append(aliases, alias)
			}
		}
		connections[name] = map[string]any{"Aliases": aliases, "Links": endpoint["Links"], "DriverOpts": endpoint["DriverOpts"]}
	}
	config["NetworkingConfig"] = map[string]any{"EndpointsConfig": connections}
	return config, nil
}

func (e *Engine) proxyRoutesReady(ctx context.Context, id string, expected map[string]bool) error {
	for attempt := 0; attempt < 50; attempt++ {
		ping, err := e.output(ctx, "docker", "exec", id, "wget", "-qO-", "http://127.0.0.1:8080/ping")
		if err == nil && strings.TrimSpace(ping) == "OK" {
			raw, err := e.output(ctx, "docker", "exec", id, "wget", "-qO-", "http://127.0.0.1:8080/api/http/routers")
			var routers []struct{ Name, Status string }
			if err == nil && json.Unmarshal([]byte(raw), &routers) == nil {
				loaded := map[string]bool{}
				for _, router := range routers {
					if router.Status == "enabled" {
						loaded[router.Name] = true
					}
				}
				ready := true
				for name := range expected {
					if !loaded[name] {
						ready = false
					}
				}
				if ready {
					return nil
				}
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
	return errors.New("replacement proxy did not become ready")
}

func (e *Engine) recoverProxyTraffic(ctx context.Context, client *http.Client) error {
	path := filepath.Join(e.Root, "proxy-traffic-change.json")
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var j proxyTrafficJournal
	if len(data) > 16384 || json.Unmarshal(data, &j) != nil || len(j.OldID) != 64 || !fileContainerID.MatchString(j.OldID) || j.BackupName != "cove-ingress-previous-"+j.Nonce || len(j.Nonce) != 32 || !validTrafficSettings(j.Previous) {
		return errors.New("invalid proxy recovery journal")
	}
	if _, err := hex.DecodeString(j.Nonce); err != nil {
		return errors.New("invalid proxy recovery identity")
	}
	var original map[string]any
	err = dockerRequest(ctx, client, http.MethodGet, "/containers/"+j.OldID+"/json", nil, &original)
	if err != nil && !(j.Committed && dockerNotFound(err)) {
		return err
	}
	if err == nil {
		config, _ := original["Config"].(map[string]any)
		labels, _ := config["Labels"].(map[string]any)
		if labels["cove.managed"] != "ingress" || (original["Name"] != "/cove-ingress" && original["Name"] != "/"+j.BackupName) {
			return errors.New("original proxy identity no longer matches recovery")
		}
	}
	if j.Committed {
		if err == nil && original["Name"] != "/"+j.BackupName {
			return errors.New("committed proxy backup identity is unexpected")
		}
		if err == nil {
			rollback := func() error {
				j.Committed = false
				if err := writeJSON(path, j); err != nil {
					return err
				}
				return e.recoverProxyTraffic(ctx, client)
			}
			var replacement map[string]any
			if len(j.NewID) != 64 || !fileContainerID.MatchString(j.NewID) {
				return errors.New("committed replacement identity is unavailable")
			}
			if err = dockerRequest(ctx, client, http.MethodGet, "/containers/"+j.NewID+"/json", nil, &replacement); err != nil {
				if dockerNotFound(err) {
					return rollback()
				}
				return errors.New("replacement proxy unavailable; retained the previous proxy for recovery")
			}
			configuration, _ := replacement["Config"].(map[string]any)
			labels, _ := configuration["Labels"].(map[string]any)
			if replacement["Name"] != "/cove-ingress" || labels["cove.traffic-change"] != j.Nonce || labels["cove.managed"] != "ingress" {
				return errors.New("replacement identity changed; retained the previous proxy")
			}
			if err = e.proxyRoutesReady(ctx, j.NewID, nil); err != nil {
				return rollback()
			}
		}
		if err = dockerRequest(ctx, client, http.MethodDelete, "/containers/"+j.OldID, nil, nil); err != nil && !dockerNotFound(err) {
			return err
		}
	} else {
		var current map[string]any
		currentErr := dockerRequest(ctx, client, http.MethodGet, "/containers/cove-ingress/json", nil, &current)
		if currentErr != nil && !dockerNotFound(currentErr) {
			return currentErr
		}
		if currentErr == nil {
			id, _ := current["Id"].(string)
			config, _ := current["Config"].(map[string]any)
			labels, _ := config["Labels"].(map[string]any)
			if id != j.OldID {
				if labels["cove.managed"] != "ingress" || labels["cove.traffic-change"] != j.Nonce || len(id) != 64 || !fileContainerID.MatchString(id) {
					return errors.New("proxy name is occupied by another container; recovery refused")
				}
				if err = dockerRequest(ctx, client, http.MethodPost, "/containers/"+id+"/stop?t=10", nil, nil); err != nil {
					return err
				}
				if err = dockerRequest(ctx, client, http.MethodDelete, "/containers/"+id, nil, nil); err != nil {
					return err
				}
			}
		}
		if original["Name"] != "/cove-ingress" {
			if err = dockerRequest(ctx, client, http.MethodPost, "/containers/"+j.OldID+"/rename?name=cove-ingress", nil, nil); err != nil {
				return err
			}
		}
		if err = dockerRequest(ctx, client, http.MethodPost, "/containers/"+j.OldID+"/start", nil, nil); err != nil {
			return err
		}
		if err = e.proxyRoutesReady(ctx, j.OldID, nil); err != nil {
			return err
		}
		if err = writeJSON(filepath.Join(e.Root, "traffic-settings.json"), j.Previous); err != nil {
			return err
		}
	}
	if err = os.Remove(path); err != nil {
		return err
	}
	return syncDirectory(e.Root)
}

func (e *Engine) ConfigureTraffic(ctx context.Context, in ProxyTrafficControl) (*ProxyTrafficState, *ProxyTrafficPlan, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	e.ingressMu.Lock()
	defer e.ingressMu.Unlock()
	settings, err := e.trafficSettings()
	if err != nil {
		return nil, nil, err
	}
	state := &ProxyTrafficState{Settings: settings}
	journalPath := filepath.Join(e.Root, "proxy-traffic-change.json")
	if _, err := os.Stat(journalPath); err == nil {
		state.Pending = true
	} else if !os.IsNotExist(err) {
		return state, nil, err
	}
	client, err := e.dockerSocketClient(ctx)
	if err != nil {
		state.Notice = err.Error()
		return state, nil, err
	}
	defer client.CloseIdleConnections()
	if state.Pending && in.Action != "recover" {
		state.Notice = "A previous proxy change needs recovery before another change."
		if in.Action == "get" {
			return state, nil, nil
		}
		return state, nil, errors.New(state.Notice)
	}
	if in.Action == "recover" {
		e.mu.Lock()
		if len(e.active) > 0 {
			e.mu.Unlock()
			return state, nil, errors.New("wait for active worker operations before proxy recovery")
		}
		if e.active == nil {
			e.active = map[string]bool{}
		}
		e.active[housekeepingWorkload] = true
		e.mu.Unlock()
		defer func() { e.mu.Lock(); delete(e.active, housekeepingWorkload); e.mu.Unlock() }()
		recovery, cancel := context.WithTimeout(context.WithoutCancel(ctx), 90*time.Second)
		defer cancel()
		err := e.recoverProxyTraffic(recovery, client)
		if err == nil {
			state.Pending = false
			state.Settings, err = e.trafficSettings()
		}
		return state, nil, err
	}
	var inspect map[string]any
	if err = dockerRequest(ctx, client, http.MethodGet, "/containers/cove-ingress/json", nil, &inspect); err != nil {
		return state, nil, errors.New("managed proxy unavailable")
	}
	config, _ := inspect["Config"].(map[string]any)
	labels, _ := config["Labels"].(map[string]any)
	if labels["cove.managed"] != "ingress" {
		return state, nil, errors.New("existing proxy is not managed by Cove")
	}
	for _, arg := range func() []any { v, _ := config["Cmd"].([]any); return v }() {
		if arg == "--accesslog=true" {
			state.Configured = true
		}
	}
	if in.Action == "get" {
		return state, nil, nil
	}
	if in.Settings == nil || !validTrafficSettings(*in.Settings) {
		return state, nil, errors.New("use at most 32 trusted proxy CIDRs, never an all-address range")
	}
	oldID, _ := inspect["Id"].(string)
	if len(oldID) != 64 || !fileContainerID.MatchString(oldID) {
		return state, nil, errors.New("invalid managed proxy identity")
	}
	oldState, _ := inspect["State"].(map[string]any)
	if oldState["Running"] != true {
		return state, nil, errors.New("start the existing proxy before reconfiguring traffic logging")
	}
	copyData, _ := json.Marshal(inspect)
	var cloned map[string]any
	if json.Unmarshal(copyData, &cloned) != nil {
		return state, nil, errors.New("invalid proxy configuration")
	}
	create, err := cloneTrafficProxy(cloned, *in.Settings, "preview")
	if err != nil {
		return state, nil, err
	}
	data, _ := json.Marshal(struct {
		ID       string
		Config   map[string]any
		Settings TrafficSettings
	}{oldID, create, *in.Settings})
	sum := sha256.Sum256(data)
	plan := &ProxyTrafficPlan{Revision: hex.EncodeToString(sum[:]), Settings: *in.Settings}
	if in.Action == "preview" {
		return state, plan, nil
	}
	if in.Action != "apply" || in.Revision != plan.Revision {
		return state, plan, errors.New("proxy preview changed; refresh before applying")
	}
	e.mu.Lock()
	if e.draining || len(e.active) > 0 || len(e.pending) > 0 || len(e.recovering) > 0 || e.customRunning || e.customUnsettled {
		e.mu.Unlock()
		return state, plan, errors.New("wait for worker operations and proxy recovery")
	}
	if e.active == nil {
		e.active = map[string]bool{}
	}
	e.active[housekeepingWorkload] = true
	e.mu.Unlock()
	defer func() { e.mu.Lock(); delete(e.active, housekeepingWorkload); e.mu.Unlock() }()
	operationContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), 90*time.Second)
	defer cancel()
	if err = e.proxyRoutesReady(operationContext, oldID, nil); err != nil {
		return state, plan, errors.New("existing proxy is not healthy; reconfiguration refused")
	}
	raw, err := e.output(operationContext, "docker", "exec", oldID, "wget", "-qO-", "http://127.0.0.1:8080/api/http/routers")
	if err != nil {
		return state, plan, err
	}
	var routers []struct{ Name, Status string }
	if json.Unmarshal([]byte(raw), &routers) != nil {
		return state, plan, errors.New("existing proxy route report unavailable")
	}
	expected := map[string]bool{}
	for _, router := range routers {
		if router.Status == "enabled" {
			expected[router.Name] = true
		}
	}
	nonceSum := sha256.Sum256([]byte(fmt.Sprintf("%s-%d", oldID, time.Now().UnixNano())))
	nonce := hex.EncodeToString(nonceSum[:16])
	create, err = cloneTrafficProxy(inspect, *in.Settings, nonce)
	if err != nil {
		return state, plan, err
	}
	j := proxyTrafficJournal{OldID: oldID, BackupName: "cove-ingress-previous-" + nonce, Nonce: nonce, Previous: settings}
	if err = writeJSON(journalPath, j); err != nil {
		return state, plan, err
	}
	applyErr := func() error {
		if err := dockerRequest(operationContext, client, http.MethodPost, "/containers/"+oldID+"/stop?t=10", nil, nil); err != nil {
			return err
		}
		if err := dockerRequest(operationContext, client, http.MethodPost, "/containers/"+oldID+"/rename?name="+j.BackupName, nil, nil); err != nil {
			return err
		}
		var result struct{ ID string }
		if err := dockerRequest(operationContext, client, http.MethodPost, "/containers/create?name=cove-ingress", create, &result); err != nil {
			return err
		}
		if len(result.ID) != 64 || !fileContainerID.MatchString(result.ID) {
			return errors.New("invalid replacement proxy identity")
		}
		j.NewID = result.ID
		if err := writeJSON(journalPath, j); err != nil {
			return err
		}
		if err := dockerRequest(operationContext, client, http.MethodPost, "/containers/"+j.NewID+"/start", nil, nil); err != nil {
			return err
		}
		if err := e.proxyRoutesReady(operationContext, j.NewID, expected); err != nil {
			return err
		}
		if err := writeJSON(filepath.Join(e.Root, "traffic-settings.json"), *in.Settings); err != nil {
			return err
		}
		j.Committed = true
		return writeJSON(journalPath, j)
	}()
	cleanupContext, cleanupCancel := context.WithTimeout(context.WithoutCancel(ctx), 90*time.Second)
	defer cleanupCancel()
	if err = e.recoverProxyTraffic(cleanupContext, client); err != nil {
		state.Pending = true
		state.Notice = "Proxy recovery remains pending; restore it before further changes."
		return state, plan, err
	}
	if applyErr != nil {
		return state, plan, errors.New("traffic logging change failed; the previous proxy was restored")
	}
	current, readErr := e.trafficSettings()
	if readErr != nil {
		return state, plan, readErr
	}
	if current.Enabled != in.Settings.Enabled || strings.Join(current.TrustedProxies, ",") != strings.Join(in.Settings.TrustedProxies, ",") {
		return state, plan, errors.New("replacement proxy failed its final check; the previous settings were restored")
	}
	state.Settings = *in.Settings
	state.Configured = in.Settings.Enabled
	return state, plan, nil
}

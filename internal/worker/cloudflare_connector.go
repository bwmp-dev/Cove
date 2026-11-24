package worker

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"time"
)

type CloudflarePublishingStatus struct {
	ID        string            `json:"id,omitempty"`
	TunnelID  string            `json:"tunnel_id,omitempty"`
	Image     string            `json:"image"`
	Hostnames []string          `json:"hostnames"`
	Phase     string            `json:"phase"`
	Running   bool              `json:"running"`
	Connected bool              `json:"connected"`
	LastError string            `json:"last_error,omitempty"`
	Withheld  map[string]string `json:"withheld"`
}

type cloudflareGeneration struct {
	Revision      string `json:"revision"`
	Relay         string `json:"relay"`
	Connector     string `json:"connector"`
	Front         string `json:"front"`
	Back          string `json:"back"`
	RelayRevision string `json:"relay_revision,omitempty"`
}

type cloudflarePublishingState struct {
	Desired   *CloudflarePublishing  `json:"desired,omitempty"`
	Active    *cloudflareGeneration  `json:"active,omitempty"`
	Candidate *cloudflareGeneration  `json:"candidate,omitempty"`
	Retired   []cloudflareGeneration `json:"retired,omitempty"`
	Phase     string                 `json:"phase"`
	LastError string                 `json:"last_error,omitempty"`
	Withheld  map[string]string      `json:"withheld,omitempty"`
}

func (e *Engine) cloudflarePublishingPath(parts ...string) string {
	return filepath.Join(append([]string{e.Root, "cloudflare-publishing"}, parts...)...)
}

func (e *Engine) readCloudflarePublishing() (cloudflarePublishingState, error) {
	var state cloudflarePublishingState
	data, err := os.ReadFile(e.cloudflarePublishingPath("state.json"))
	if os.IsNotExist(err) {
		state.Phase = "disabled"
		return state, nil
	}
	if err != nil {
		return state, err
	}
	if len(data) > 128*1024 || json.Unmarshal(data, &state) != nil {
		return state, errors.New("stored tunnel deployment is unreadable")
	}
	if state.Desired != nil {
		if err := ValidateCloudflarePublishing(*state.Desired); err != nil {
			return state, errors.New("stored tunnel deployment is invalid")
		}
	}
	for _, g := range append(append([]cloudflareGeneration{}, state.Retired...), cloudflareGenerationValues(state.Active, state.Candidate)...) {
		if len(g.Revision) != 64 {
			return state, errors.New("stored tunnel generation is invalid")
		}
		if _, err := hex.DecodeString(g.Revision); err != nil {
			return state, errors.New("stored tunnel generation is invalid")
		}
	}
	return state, nil
}

func cloudflareGenerationValues(generations ...*cloudflareGeneration) []cloudflareGeneration {
	out := []cloudflareGeneration{}
	for _, g := range generations {
		if g != nil {
			out = append(out, *g)
		}
	}
	return out
}

func (e *Engine) writeCloudflarePublishing(state cloudflarePublishingState) error {
	if err := os.MkdirAll(e.cloudflarePublishingPath(), 0700); err != nil {
		return err
	}
	return writeJSON(e.cloudflarePublishingPath("state.json"), state)
}

func cloudflareDeploymentStatus(state cloudflarePublishingState) CloudflarePublishingStatus {
	status := CloudflarePublishingStatus{Image: DefaultCloudflaredImage, Phase: state.Phase, Hostnames: []string{}, LastError: state.LastError, Withheld: state.Withheld}
	if state.Desired != nil {
		status.ID, status.TunnelID = state.Desired.ID, state.Desired.TunnelID
		status.Hostnames = slices.Clone(state.Desired.Hostnames)
		if state.Desired.Image != "" {
			status.Image = state.Desired.Image
		}
	}
	return status
}

func (e *Engine) cloudflareContainer(ctx context.Context, name, revision string) (string, bool, error) {
	owner, err := e.buildOwner()
	if err != nil {
		return "", false, err
	}
	ids, err := e.output(ctx, "docker", "ps", "-aq", "--no-trunc", "--filter", "name=^/"+name+"$")
	if err != nil || ids == "" {
		return "", false, err
	}
	if !containerIdentity.MatchString(ids) {
		return "", false, errors.New("tunnel container identity is invalid")
	}
	var detail struct {
		ID      string
		Labels  map[string]string
		Running bool
	}
	if err := e.imageJSON(ctx, nil, &detail, "docker", "inspect", "--format", `{"id":{{json .Id}},"labels":{{json .Config.Labels}},"running":{{json .State.Running}}}`, ids); err != nil {
		return "", false, err
	}
	if detail.Labels["cove.managed"] != "cloudflare-tunnel" || detail.Labels["cove.cloudflare-tunnel-owner"] != owner || detail.Labels["cove.cloudflare-tunnel-revision"] != revision || detail.ID != ids {
		return "", false, errors.New("tunnel container has unexpected ownership")
	}
	return ids, detail.Running, nil
}

func (e *Engine) ensureCloudflareContainer(ctx context.Context, name, revision, image, network, address, dir string, command []string) error {
	id, _, err := e.cloudflareContainer(ctx, name, revision)
	if err != nil {
		return err
	}
	if id == "" {
		owner, err := e.buildOwner()
		if err != nil {
			return err
		}
		args := []string{"docker", "create", "--pull", "never", "--name", name, "--label", "cove.managed=cloudflare-tunnel", "--label", "cove.cloudflare-tunnel-owner=" + owner, "--label", "cove.cloudflare-tunnel-revision=" + revision, "--restart", "unless-stopped", "--network", network, "--user", fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()), "--read-only", "--cap-drop", "ALL", "--security-opt", "no-new-privileges:true", "--memory", "192m", "--pids-limit", "64", "--log-driver", "none", "--mount", "type=bind,source=" + dir + ",target=/config,readonly"}
		if address != "" {
			args = append(args, "--ip", address)
		}
		args = append(args, image)
		args = append(args, command...)
		if err := e.Run(ctx, e.Root, args, nil, io.Discard); err != nil {
			return errors.New("could not create the owned tunnel container")
		}
	}
	return nil
}

func (e *Engine) cloudflareProbe(ctx context.Context, name, path, revision string) error {
	image, err := e.linkGatewayImage(ctx)
	if err != nil {
		return err
	}
	owner, err := e.buildOwner()
	if err != nil {
		return err
	}
	args := []string{"docker", "run", "--rm", "--label", "cove.managed=cloudflare-probe", "--label", "cove.cloudflare-tunnel-owner=" + owner, "--read-only", "--cap-drop", "ALL", "--security-opt", "no-new-privileges:true", "--network", "container:" + name, image, "cloudflare-probe", path}
	if revision != "" {
		args = append(args, revision)
	}
	return e.Run(ctx, e.Root, args, nil, io.Discard)
}

func RunCloudflareProbe(ctx context.Context, address, revision string) error {
	if address != "http://127.0.0.1:8081/" && address != "http://127.0.0.1:20241/ready" {
		return errors.New("invalid tunnel readiness endpoint")
	}
	request, err := http.NewRequestWithContext(ctx, "GET", address, nil)
	if err != nil {
		return err
	}
	response, err := (&http.Client{Timeout: 3 * time.Second, Transport: &http.Transport{Proxy: nil}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}).Do(request)
	if err != nil {
		return errors.New("tunnel readiness unavailable")
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, 4097))
	if err != nil || response.StatusCode != 200 || len(data) > 4096 || revision != "" && string(data) != revision {
		return errors.New("tunnel has not confirmed readiness")
	}
	return nil
}

func (e *Engine) cloudflareGenerationNames(revision string) (cloudflareGeneration, error) {
	owner, err := e.buildOwner()
	if err != nil {
		return cloudflareGeneration{}, err
	}
	base := "cove-cft-" + owner[:8] + "-" + revision[:16]
	return cloudflareGeneration{Revision: revision, Relay: base + "-origin", Connector: base + "-edge", Front: base + "-front", Back: base + "-back"}, nil
}

func cloudflareRevision(value any) string {
	data, _ := json.Marshal(value)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func (e *Engine) ensureCloudflareNetwork(ctx context.Context, name, revision, role string) (*linkNetworkInfo, error) {
	owner, err := e.buildOwner()
	if err != nil {
		return nil, err
	}
	ids, err := e.output(ctx, "docker", "network", "ls", "-q", "--filter", "name=^"+name+"$")
	if err != nil {
		return nil, err
	}
	if ids == "" {
		if err := e.Run(ctx, e.Root, []string{"docker", "network", "create", "--internal", "--label", "cove.managed=cloudflare-tunnel", "--label", "cove.cloudflare-tunnel-owner=" + owner, "--label", "cove.cloudflare-tunnel-revision=" + revision, "--label", "cove.cloudflare-tunnel-role=" + role, name}, nil, io.Discard); err != nil {
			return nil, errors.New("could not create the owned tunnel network")
		}
	}
	var network dockerNetwork
	if err := e.imageJSON(ctx, nil, &network, "docker", "network", "inspect", "--format", "{{json .}}", name); err != nil {
		return nil, err
	}
	if network.Labels["cove.managed"] != "cloudflare-tunnel" || network.Labels["cove.cloudflare-tunnel-owner"] != owner || network.Labels["cove.cloudflare-tunnel-revision"] != revision || network.Labels["cove.cloudflare-tunnel-role"] != role || len(network.IPAM.Config) != 1 {
		return nil, errors.New("tunnel network has unexpected ownership or addressing")
	}
	return &linkNetworkInfo{Name: name, Subnet: network.IPAM.Config[0].Subnet, Labels: network.Labels}, nil
}

func (e *Engine) prepareCloudflareGeneration(ctx context.Context, value CloudflarePublishing, g *cloudflareGeneration, roots map[string]string, helperImage string) error {
	front, err := e.ensureCloudflareNetwork(ctx, g.Front, g.Revision, "front")
	if err != nil {
		return err
	}
	back, err := e.ensureCloudflareNetwork(ctx, g.Back, g.Revision, "back")
	if err != nil {
		return err
	}
	peer, err := idleAddress(front.Subnet, 2)
	if err != nil {
		return err
	}
	origin, err := idleAddress(front.Subnet, 3)
	if err != nil {
		return err
	}
	backOrigin, err := idleAddress(back.Subnet, 2)
	if err != nil {
		return err
	}
	proxy, err := idleAddress(back.Subnet, 3)
	if err != nil {
		return err
	}
	identity, err := e.ensureCloudflareOriginIdentity()
	if err != nil {
		return err
	}
	connector, credentials, err := cloudflareConnectorFiles(value, origin)
	if err != nil {
		return err
	}
	systemRoots, err := routeAuxRoots()
	if err != nil {
		return err
	}
	systemRoots, err = cloudflarePublicCABundle(systemRoots)
	if err != nil {
		return err
	}
	config := cloudflareOriginConfig{Peer: peer, Proxy: proxy, Roots: roots, CAPEM: systemRoots}
	if _, closeIdle, err := newCloudflareOriginHandler(config); err != nil {
		return err
	} else {
		closeIdle()
	}
	base := e.cloudflarePublishingPath("generations", g.Revision)
	for _, dir := range []string{filepath.Join(base, "origin"), filepath.Join(base, "connector")} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			return err
		}
	}
	for _, file := range []struct {
		path string
		data []byte
	}{
		{filepath.Join(base, "origin", "relay.crt"), []byte(identity.CertificatePEM)},
		{filepath.Join(base, "origin", "relay.key"), []byte(identity.PrivateKeyPEM)},
		{filepath.Join(base, "connector", "config.json"), connector},
		{filepath.Join(base, "connector", "credentials.json"), credentials},
		{filepath.Join(base, "connector", "origin.pem"), []byte(identity.CertificatePEM)},
	} {
		if err := writeFileAtomic(file.path, file.data, 0600); err != nil {
			return err
		}
	}
	if err := writeJSON(filepath.Join(base, "origin", "relay.json"), config); err != nil {
		return err
	}
	g.RelayRevision = cloudflareRevision(config)
	if err := e.idleAttach(ctx, back, "cove-ingress", proxy); err != nil {
		return err
	}
	if err := e.ensureCloudflareContainer(ctx, g.Relay, g.Revision, helperImage, front.Name, origin, filepath.Join(base, "origin"), []string{"cloudflare-origin"}); err != nil {
		return err
	}
	if err := e.idleAttach(ctx, back, g.Relay, backOrigin); err != nil {
		return err
	}
	if err := e.Run(ctx, e.Root, []string{"docker", "start", g.Relay}, nil, io.Discard); err != nil {
		return err
	}
	ready := false
	for attempt := 0; attempt < 20; attempt++ {
		if e.cloudflareProbe(ctx, g.Relay, "http://127.0.0.1:8081/", g.RelayRevision) == nil {
			ready = true
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	if !ready {
		return errors.New("the verified origin did not become ready")
	}
	image := value.Image
	if image == "" {
		image = DefaultCloudflaredImage
	}
	if _, err := e.output(ctx, "docker", "image", "inspect", "--format", "{{.Id}}", image); err != nil {
		client := e.ingressDockerClient()
		defer client.CloseIdleConnections()
		client.Timeout = 2 * time.Minute
		if err := pullCloudflareConnectorImage(ctx, client, image); err != nil {
			return err
		}
	}
	if err := e.ensureCloudflareContainer(ctx, g.Connector, g.Revision, image, "bridge", "", filepath.Join(base, "connector"), []string{"tunnel", "--config", "/config/config.json", "run"}); err != nil {
		return err
	}
	if err := e.idleAttach(ctx, front, g.Connector, peer); err != nil {
		return err
	}
	return e.Run(ctx, e.Root, []string{"docker", "start", g.Connector}, nil, io.Discard)
}

func (e *Engine) confirmCloudflareGeneration(ctx context.Context, g cloudflareGeneration) error {
	if err := e.cloudflareProbe(ctx, g.Relay, "http://127.0.0.1:8081/", g.RelayRevision); err != nil {
		return errors.New("tunnel origin has not confirmed its configuration")
	}
	if err := e.Run(ctx, e.Root, []string{"docker", "exec", g.Relay, "/cove-worker", "cloudflare-origin-probe"}, nil, io.Discard); err != nil {
		return errors.New("the origin has not verified the managed proxy certificate")
	}
	image, err := e.linkGatewayImage(ctx)
	if err != nil {
		return err
	}
	base := e.cloudflarePublishingPath("generations", g.Revision, "connector")
	args := []string{"docker", "run", "--rm", "--read-only", "--cap-drop", "ALL", "--security-opt", "no-new-privileges:true", "--user", fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()), "--network", "container:" + g.Connector, "--mount", "type=bind,source=" + filepath.Join(base, "config.json") + ",target=/config/config.json,readonly", "--mount", "type=bind,source=" + filepath.Join(base, "origin.pem") + ",target=/config/origin.pem,readonly", image, "cloudflare-connector-origin-probe"}
	if err := e.Run(ctx, e.Root, args, nil, io.Discard); err != nil {
		return errors.New("the connector has not verified its local origin TLS identity")
	}
	if err := e.cloudflareProbe(ctx, g.Connector, "http://127.0.0.1:20241/ready", ""); err != nil {
		return errors.New("connector has not authenticated a connection to the edge")
	}
	return nil
}

func (e *Engine) removeCloudflareGeneration(ctx context.Context, g cloudflareGeneration) error {
	for _, name := range []string{g.Connector, g.Relay} {
		id, _, err := e.cloudflareContainer(ctx, name, g.Revision)
		if err != nil {
			return err
		}
		if id != "" {
			if err := e.Run(ctx, e.Root, []string{"docker", "rm", "-f", id}, nil, io.Discard); err != nil {
				return err
			}
		}
	}
	owner, err := e.buildOwner()
	if err != nil {
		return err
	}
	for _, name := range []string{g.Front, g.Back} {
		ids, err := e.output(ctx, "docker", "network", "ls", "-q", "--filter", "name=^"+name+"$")
		if err != nil {
			return err
		}
		if ids == "" {
			continue
		}
		var network dockerNetwork
		if err := e.imageJSON(ctx, nil, &network, "docker", "network", "inspect", "--format", "{{json .}}", name); err != nil {
			return err
		}
		if network.Labels["cove.managed"] != "cloudflare-tunnel" || network.Labels["cove.cloudflare-tunnel-owner"] != owner || network.Labels["cove.cloudflare-tunnel-revision"] != g.Revision {
			return errors.New("tunnel network ownership changed before cleanup")
		}
		for id, container := range network.Containers {
			if name != g.Back || container.Name != "cove-ingress" {
				return errors.New("an unexpected endpoint prevents tunnel network cleanup")
			}
			var labels map[string]string
			if err := e.imageJSON(ctx, nil, &labels, "docker", "inspect", "--format", "{{json .Config.Labels}}", id); err != nil {
				return err
			}
			if labels["cove.managed"] != "ingress" {
				return errors.New("proxy ownership changed before tunnel cleanup")
			}
			if err := e.Run(ctx, e.Root, []string{"docker", "network", "disconnect", name, id}, nil, io.Discard); err != nil {
				return err
			}
		}
		if err := e.Run(ctx, e.Root, []string{"docker", "network", "rm", name}, nil, io.Discard); err != nil {
			return err
		}
	}
	return os.RemoveAll(e.cloudflarePublishingPath("generations", g.Revision))
}

func (e *Engine) ApplyCloudflarePublishing(ctx context.Context, value *CloudflarePublishing) (CloudflarePublishingStatus, error) {
	if value != nil {
		config, err := readOutboundConfig(e.Root)
		if err != nil {
			return CloudflarePublishingStatus{}, err
		}
		if config != nil && (config.HTTP != "" || config.HTTPS != "") {
			return CloudflarePublishingStatus{}, errors.New("Cloudflare Tunnel edge transport cannot use Cove's HTTP outbound proxy; disable publishing before enabling that proxy")
		}
		e.ingressMu.Lock()
		state, err := e.readCloudflarePublishing()
		unchanged := err == nil && state.Desired != nil && state.Active != nil && state.Phase == "active" && state.LastError == "" && state.Candidate == nil && len(state.Retired) == 0 && cloudflareRevision(*state.Desired) == cloudflareRevision(*value)
		if unchanged {
			status, err := e.cloudflarePublishingStatusLocked(ctx)
			e.ingressMu.Unlock()
			return status, err
		}
		e.ingressMu.Unlock()
		if err != nil {
			return CloudflarePublishingStatus{}, err
		}
	}
	unlock, err := e.lockNodeChange(false)
	if err != nil {
		return CloudflarePublishingStatus{}, err
	}
	defer unlock()
	ctx = context.WithValue(ctx, nodeLeaseContextKey{}, e)
	if value != nil {
		config, err := readOutboundConfig(e.Root)
		if err != nil {
			return CloudflarePublishingStatus{}, err
		}
		if config != nil && (config.HTTP != "" || config.HTTPS != "") {
			return CloudflarePublishingStatus{}, errors.New("Cloudflare Tunnel edge transport cannot use Cove's HTTP outbound proxy; disable publishing before enabling that proxy")
		}
		if err := e.requireDockerProxy(ctx, config); err != nil {
			return CloudflarePublishingStatus{}, err
		}
		if err := e.requireInterlockedHostSchedule(); err != nil {
			return CloudflarePublishingStatus{}, err
		}
		if err := e.ingressExposureReady(); err != nil {
			return CloudflarePublishingStatus{}, err
		}
		if e.trafficRecoveryPending() {
			return CloudflarePublishingStatus{}, errors.New("finish proxy traffic recovery before changing tunnel publishing")
		}
	}
	e.ingressMu.Lock()
	defer e.ingressMu.Unlock()
	state, err := e.readCloudflarePublishing()
	if err != nil {
		return CloudflarePublishingStatus{}, err
	}
	if value != nil {
		if err := ValidateCloudflarePublishing(*value); err != nil {
			return cloudflareDeploymentStatus(state), err
		}
		if state.Desired != nil && (state.Desired.ID != value.ID || state.Desired.TunnelID != value.TunnelID || state.Desired.AccountID != value.AccountID || state.Desired.TunnelSecret != value.TunnelSecret) {
			return cloudflareDeploymentStatus(state), errors.New("disable and clean up the current tunnel before replacing its identity")
		}
	}
	state.Desired = value
	if value == nil {
		state.Phase = "disabling"
	} else {
		state.Phase = "preparing"
	}
	state.LastError = ""
	if err := e.writeCloudflarePublishing(state); err != nil {
		return cloudflareDeploymentStatus(state), err
	}
	return e.reconcileCloudflarePublishing(ctx, &state)
}

func (e *Engine) reconcileCloudflarePublishing(ctx context.Context, state *cloudflarePublishingState) (CloudflarePublishingStatus, error) {
	fail := func(message string) (CloudflarePublishingStatus, error) {
		state.LastError = message
		if err := e.writeCloudflarePublishing(*state); err != nil {
			return cloudflareDeploymentStatus(*state), err
		}
		return cloudflareDeploymentStatus(*state), nil
	}
	if state.Desired == nil {
		all := append(slices.Clone(state.Retired), cloudflareGenerationValues(state.Active, state.Candidate)...)
		for _, g := range all {
			if err := e.removeCloudflareGeneration(ctx, g); err != nil {
				return fail("owned connector cleanup is incomplete; it will retry")
			}
		}
		state.Active, state.Candidate, state.Retired = nil, nil, nil
		state.Phase = "disabled"
		state.LastError = ""
		if err := e.writeCloudflarePublishing(*state); err != nil {
			return cloudflareDeploymentStatus(*state), err
		}
		routes, err := e.loadIngress()
		if err != nil {
			return cloudflareDeploymentStatus(*state), err
		}
		if err := e.writeManagedIngress(routes); err != nil {
			return cloudflareDeploymentStatus(*state), err
		}
		return cloudflareDeploymentStatus(*state), nil
	}
	routes, err := e.loadIngress()
	if err != nil {
		return fail("managed routes are unreadable")
	}
	roots := map[string]string{}
	state.Withheld = map[string]string{}
	if !state.Desired.ProveOnly {
		for _, host := range state.Desired.Hostnames {
			selected, err := e.cloudflareOriginRoots([]string{host}, routes)
			if err != nil {
				state.Withheld[host] = "The current route is missing, private, source restricted or lacks a valid origin identity."
				continue
			}
			roots[host] = selected[host]
		}
	}
	if err := e.writeManagedIngress(routes); err != nil {
		return fail("could not protect the saved routing inventory before starting the connector")
	}
	client := e.ingressDockerClient()
	defer client.CloseIdleConnections()
	proxy, err := captureIngressProxy(ctx, client)
	if err != nil || !proxy.Running {
		return fail("a running owned managed proxy is required")
	}
	helper, err := e.linkGatewayImage(ctx)
	if err != nil {
		return fail("the owned origin helper image is unavailable")
	}
	identity, err := e.ensureCloudflareOriginIdentity()
	if err != nil {
		return fail("the persisted local origin identity is unavailable")
	}
	revision := cloudflareRevision(struct {
		Spec             CloudflarePublishing
		Roots            map[string]string
		Helper, Identity string
	}{*state.Desired, roots, helper, identity.CertificatePEM})
	selectCloudflareGeneration(state, revision)
	if (state.Active == nil || state.Active.Revision != revision) && state.Candidate == nil {
		g, err := e.cloudflareGenerationNames(revision)
		if err != nil {
			return cloudflareDeploymentStatus(*state), err
		}
		state.Candidate = &g
		state.Phase = "connecting"
	}
	if err := e.writeCloudflarePublishing(*state); err != nil {
		return cloudflareDeploymentStatus(*state), err
	}
	if state.Active == nil || state.Active.Revision != revision {
		if err := e.prepareCloudflareGeneration(ctx, *state.Desired, state.Candidate, roots, helper); err != nil {
			return fail("could not prepare the owned connector and verified TLS origin")
		}
		if err := e.writeCloudflarePublishing(*state); err != nil {
			return cloudflareDeploymentStatus(*state), err
		}
		if err := e.confirmCloudflareGeneration(ctx, *state.Candidate); err != nil {
			return fail(err.Error())
		}
		if state.Active != nil {
			id, _, err := e.cloudflareContainer(ctx, state.Active.Connector, state.Active.Revision)
			if err != nil {
				return fail("could not verify the previous connector")
			}
			if id != "" {
				if err := e.Run(ctx, e.Root, []string{"docker", "stop", "--time", "20", id}, nil, io.Discard); err != nil {
					return fail("could not stop the previous connector")
				}
			}
			state.Retired = append(state.Retired, *state.Active)
		}
		state.Active, state.Candidate = state.Candidate, nil
		state.Phase = "active"
		state.LastError = ""
		if err := e.writeCloudflarePublishing(*state); err != nil {
			return cloudflareDeploymentStatus(*state), err
		}
	} else {
		if err := e.prepareCloudflareGeneration(ctx, *state.Desired, state.Active, roots, helper); err != nil {
			return fail("could not reconcile the owned connector")
		}
	}
	for len(state.Retired) > 0 {
		if state.Active != nil && state.Active.Revision == state.Retired[0].Revision || state.Candidate != nil && state.Candidate.Revision == state.Retired[0].Revision {
			return fail("tunnel generation ownership overlaps; cleanup is withheld")
		}
		if err := e.removeCloudflareGeneration(ctx, state.Retired[0]); err != nil {
			return fail("previous connector cleanup is incomplete; it will retry")
		}
		state.Retired = state.Retired[1:]
		if err := e.writeCloudflarePublishing(*state); err != nil {
			return cloudflareDeploymentStatus(*state), err
		}
	}
	state.Phase = "active"
	if err := e.writeCloudflarePublishing(*state); err != nil {
		return cloudflareDeploymentStatus(*state), err
	}
	status := cloudflareDeploymentStatus(*state)
	_, status.Running, err = e.cloudflareContainer(ctx, state.Active.Connector, state.Active.Revision)
	if err != nil {
		return status, err
	}
	status.Connected = status.Running && e.confirmCloudflareGeneration(ctx, *state.Active) == nil
	return status, nil
}

func (e *Engine) restrictCloudflareOrigins(ctx context.Context, routes map[string][]Route) error {
	state, err := e.readCloudflarePublishing()
	if err != nil {
		return err
	}
	generations := append(slices.Clone(state.Retired), cloudflareGenerationValues(state.Active, state.Candidate)...)
	if len(generations) == 0 {
		return nil
	}
	roots := map[string]string{}
	state.Withheld = map[string]string{}
	if state.Desired != nil && !state.Desired.ProveOnly {
		for _, host := range state.Desired.Hostnames {
			selected, err := e.cloudflareOriginRoots([]string{host}, routes)
			if err != nil {
				state.Withheld[host] = "The current route is missing, private, source restricted or lacks a valid origin identity."
				continue
			}
			roots[host] = selected[host]
		}
	}
	for _, g := range generations {
		path := e.cloudflarePublishingPath("generations", g.Revision, "origin", "relay.json")
		_, running, err := e.cloudflareContainer(ctx, g.Relay, g.Revision)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(path)
		var config cloudflareOriginConfig
		if err != nil || json.Unmarshal(data, &config) != nil {
			if !running {
				continue
			}
			if err := e.stopCloudflareConnector(ctx, g); err != nil {
				return err
			}
			continue
		}
		allowed := map[string]string{}
		for host, root := range roots {
			allowed[host], err = cloudflareTrustUnion(root, config.Roots[host])
			if err != nil {
				return err
			}
		}
		config.Roots = allowed
		if err := writeJSON(path, config); err != nil {
			if err := e.stopCloudflareConnector(ctx, g); err != nil {
				return err
			}
			continue
		}
		if !running {
			continue
		}
		revision := cloudflareRevision(config)
		confirmed := false
		for attempt := 0; attempt < 20; attempt++ {
			if e.cloudflareProbe(ctx, g.Relay, "http://127.0.0.1:8081/", revision) == nil {
				confirmed = true
				break
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(100 * time.Millisecond):
			}
		}
		if !confirmed {
			if err := e.stopCloudflareConnector(ctx, g); err != nil {
				return err
			}
		}
		for _, saved := range []*cloudflareGeneration{state.Active, state.Candidate} {
			if saved != nil && saved.Revision == g.Revision {
				saved.RelayRevision = revision
			}
		}
	}
	return e.writeCloudflarePublishing(state)
}

func (e *Engine) stopCloudflareConnector(ctx context.Context, g cloudflareGeneration) error {
	id, running, err := e.cloudflareContainer(ctx, g.Connector, g.Revision)
	if err != nil {
		return err
	}
	if id == "" || !running {
		return nil
	}
	if err := e.Run(ctx, e.Root, []string{"docker", "stop", "--time", "10", id}, nil, io.Discard); err != nil {
		return errors.New("could not withhold the tunnel before changing route security")
	}
	_, running, err = e.cloudflareContainer(ctx, g.Connector, g.Revision)
	if err != nil || running {
		return errors.New("connector stop was not confirmed before changing route security")
	}
	return nil
}

func (e *Engine) CloudflarePublishingStatus(ctx context.Context) (CloudflarePublishingStatus, error) {
	e.ingressMu.Lock()
	defer e.ingressMu.Unlock()
	return e.cloudflarePublishingStatusLocked(ctx)
}

func (e *Engine) cloudflarePublishingStatusLocked(ctx context.Context) (CloudflarePublishingStatus, error) {
	state, err := e.readCloudflarePublishing()
	if err != nil {
		return CloudflarePublishingStatus{}, err
	}
	status := cloudflareDeploymentStatus(state)
	if state.Active != nil {
		_, status.Running, err = e.cloudflareContainer(ctx, state.Active.Connector, state.Active.Revision)
		if err != nil {
			return status, err
		}
		status.Connected = status.Running && e.confirmCloudflareGeneration(ctx, *state.Active) == nil
	}
	return status, nil
}

func (e *Engine) cloudflarePublishingLoop(ctx context.Context) {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		err := e.recoverCloudflarePublishing(ctx)
		if err != nil && ctx.Err() == nil {
			fmt.Fprintln(os.Stderr, "Cloudflare Tunnel recovery is incomplete; saved ownership is retained.")
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (e *Engine) recoverCloudflarePublishing(ctx context.Context) error {
	state, err := e.readCloudflarePublishing()
	if err != nil {
		return err
	}
	if state.Desired == nil && state.Active == nil && state.Candidate == nil && len(state.Retired) == 0 {
		return nil
	}
	unlock, err := e.lockNodeChange(false)
	if err != nil {
		return err
	}
	defer unlock()
	ctx = context.WithValue(ctx, nodeLeaseContextKey{}, e)
	if err := e.requireInterlockedHostSchedule(); err != nil {
		return err
	}
	if err := e.ingressExposureReady(); err != nil {
		return err
	}
	if e.trafficRecoveryPending() {
		return errors.New("finish proxy traffic recovery before recovering tunnel publishing")
	}
	e.ingressMu.Lock()
	state, err = e.readCloudflarePublishing()
	if err == nil && (state.Desired != nil || state.Active != nil || state.Candidate != nil || len(state.Retired) > 0) {
		routes, readErr := e.loadIngress()
		if readErr != nil {
			routes = map[string][]Route{}
		}
		if err = e.restrictCloudflareOrigins(ctx, routes); err == nil && readErr == nil {
			state, err = e.readCloudflarePublishing()
			if err == nil {
				_, err = e.reconcileCloudflarePublishing(ctx, &state)
			}
		} else if readErr != nil {
			err = readErr
		}
	}
	e.ingressMu.Unlock()
	return err
}

func cloudflareTrustUnion(current, previous string) (string, error) {
	seen := map[string]bool{}
	var result []byte
	for _, data := range []string{current, previous} {
		remaining := []byte(data)
		for len(bytes.TrimSpace(remaining)) > 0 {
			block, rest := pem.Decode(remaining)
			if block == nil || block.Type != "CERTIFICATE" || len(block.Headers) > 0 {
				return "", errors.New("invalid public origin identity")
			}
			key := string(block.Bytes)
			if !seen[key] {
				result = append(result, pem.EncodeToMemory(block)...)
				seen[key] = true
			}
			remaining = rest
		}
	}
	if len(result) > 262144 {
		return "", errors.New("tunnel origin identity set exceeds the hostname budget")
	}
	return string(result), nil
}

func selectCloudflareGeneration(state *cloudflarePublishingState, revision string) {
	if state.Candidate != nil && (state.Candidate.Revision != revision || state.Active != nil && state.Active.Revision == revision) {
		state.Retired = append(state.Retired, *state.Candidate)
		state.Candidate = nil
	}
	kept := []cloudflareGeneration{}
	seen := map[string]bool{}
	for _, g := range state.Retired {
		if g.Revision == revision || state.Active != nil && state.Active.Revision == g.Revision || seen[g.Revision] {
			continue
		}
		seen[g.Revision] = true
		kept = append(kept, g)
	}
	state.Retired = kept
}

func (e *Engine) writeManagedIngress(routes map[string][]Route) error {
	state, err := e.readCloudflarePublishing()
	if err != nil {
		return err
	}
	path := filepath.Join(e.Root, "ingress.json")
	if state.Desired == nil && state.Active == nil && state.Candidate == nil && len(state.Retired) == 0 {
		return writeJSON(path, routes)
	}
	data, err := json.Marshal(routes)
	if err != nil {
		return err
	}
	var document map[string][]map[string]any
	if json.Unmarshal(data, &document) != nil {
		return errors.New("managed routes could not be protected")
	}
	for _, list := range document {
		for _, route := range list {
			policy, _ := route["policy"].(map[string]any)
			if policy == nil {
				policy = map[string]any{}
				route["policy"] = policy
			}
			source, _ := policy["certificate_source"].(string)
			policy["certificate_source"] = "cove-protected-tunnel-v1"
			policy["cove_certificate_source"] = source
			policy["cove_route_protocol"] = 3
		}
	}
	return writeJSON(path, document)
}

func pullCloudflareConnectorImage(ctx context.Context, client *http.Client, image string) error {
	if err := ValidateCloudflaredImage(image); err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, "POST", "http://docker/images/create?fromImage="+url.QueryEscape(image), nil)
	if err != nil {
		return err
	}
	response, err := client.Do(request)
	if err != nil {
		return errors.New("could not fetch the pinned official connector image")
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		return errors.New("the Docker daemon refused the anonymous pinned connector pull")
	}
	limited := &io.LimitedReader{R: response.Body, N: 8*1024*1024 + 1}
	decoder := json.NewDecoder(limited)
	for {
		var message struct {
			Error string `json:"error"`
		}
		err := decoder.Decode(&message)
		if err == io.EOF && limited.N > 0 {
			return nil
		}
		if err != nil || message.Error != "" {
			return errors.New("the anonymous pinned connector pull did not complete")
		}
	}
}

func cloudflarePublicCABundle(bundle string) (string, error) {
	if len(bundle) > 1024*1024 {
		return "", errors.New("the system CA bundle exceeds the tunnel limit")
	}
	var public bytes.Buffer
	remaining := []byte(bundle)
	for len(remaining) != 0 {
		block, rest := pem.Decode(remaining)
		if block == nil {
			break
		}
		if block.Type != "CERTIFICATE" || len(block.Headers) != 0 {
			return "", errors.New("the system CA bundle must contain only public certificates")
		}
		if _, err := x509.ParseCertificate(block.Bytes); err != nil {
			return "", errors.New("the system CA bundle contains an invalid certificate")
		}
		if err := pem.Encode(&public, block); err != nil {
			return "", err
		}
		remaining = rest
	}
	if public.Len() == 0 {
		return "", errors.New("the system CA bundle contains no public certificates")
	}
	return public.String(), nil
}

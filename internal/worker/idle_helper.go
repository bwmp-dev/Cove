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
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

func idleNetwork(workload, role string) string { return "cove-idle-" + role + "-" + linkHash(workload) }
func idleContainer(workload string) string     { return "cove-idle-" + linkHash(workload) }

func idleSocketAddress(path string) (string, func(), error) {
	if runtime.GOOS != "linux" || len(path) < 108 {
		return path, func() {}, nil
	}
	// A directory descriptor keeps long worker roots within Linux's sockaddr_un limit.
	directory, err := os.Open(filepath.Dir(path))
	if err != nil {
		return "", nil, err
	}
	return filepath.Join("/proc/self/fd", strconv.Itoa(int(directory.Fd())), filepath.Base(path)), func() { directory.Close() }, nil
}

func idleAddress(prefix string, offset int) (string, error) {
	p, err := netip.ParsePrefix(prefix)
	if err != nil || !p.Addr().Is4() {
		return "", errors.New("invalid idle network subnet")
	}
	address := p.Masked().Addr()
	for range offset {
		address = address.Next()
	}
	if !p.Contains(address) {
		return "", errors.New("idle network is too small")
	}
	return address.String(), nil
}

func (e *Engine) idleControl(ctx context.Context, workload, method, path string, input, output any) (int, error) {
	var body bytes.Buffer
	if input != nil {
		if err := json.NewEncoder(&body).Encode(input); err != nil {
			return 0, err
		}
	}
	transport := &http.Transport{Proxy: nil, DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		address, closeAddress, err := idleSocketAddress(e.idleDir(workload, "state", "control.sock"))
		if err != nil {
			return nil, err
		}
		defer closeAddress()
		return (&net.Dialer{Timeout: time.Second}).DialContext(ctx, "unix", address)
	}}
	defer transport.CloseIdleConnections()
	request, err := http.NewRequestWithContext(ctx, method, "http://gateway"+path, &body)
	if err != nil {
		return 0, err
	}
	response, err := (&http.Client{Transport: transport, Timeout: 10 * time.Second}).Do(request)
	if err != nil {
		return 0, err
	}
	defer response.Body.Close()
	if output != nil && response.StatusCode == 200 {
		err = json.NewDecoder(io.LimitReader(response.Body, 1024*1024)).Decode(output)
	}
	return response.StatusCode, err
}

func (e *Engine) writeIdle(workload string, state *idleWorkloadState) error {
	path := filepath.Join(e.Root, "workloads", workload, "idle.json")
	if state == nil {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
		return syncDirectory(filepath.Dir(path))
	}
	return writeJSON(path, state)
}

func (e *Engine) idleAttach(ctx context.Context, network *linkNetworkInfo, container, address string) error {
	var attachment struct {
		IPAddress  string
		IPAMConfig *struct{ IPv4Address string }
	}
	format := `{{json (index .NetworkSettings.Networks ` + strconv.Quote(network.Name) + `)}}`
	if err := e.imageJSON(ctx, nil, &attachment, "docker", "inspect", "--format", format, container); err != nil {
		return err
	}
	if attachment.IPAddress != "" || attachment.IPAMConfig != nil {
		actual := attachment.IPAddress
		if actual == "" && attachment.IPAMConfig != nil {
			actual = attachment.IPAMConfig.IPv4Address
		}
		if address != "" && actual != address {
			return errors.New("idle endpoint has an unexpected address")
		}
		return nil
	}
	args := []string{"docker", "network", "connect"}
	if address != "" {
		args = append(args, "--ip", address)
	}
	return e.Run(ctx, e.Root, append(args, network.Name, container), nil, io.Discard)
}

func (e *Engine) prepareIdleGateway(ctx context.Context, workload string, state *idleWorkloadState, routes []Route) error {
	networks, err := e.helperNetworks(ctx, "idle")
	if err != nil {
		return err
	}
	front, err := e.ensureHelperNetwork(ctx, networks, idleNetwork(workload, "front"), "idle", "front", workload, 29)
	if err != nil {
		return err
	}
	back, err := e.ensureHelperNetwork(ctx, networks, idleNetwork(workload, "back"), "idle", "back", workload, 24)
	if err != nil {
		return err
	}
	helperIP, err := idleAddress(front.Subnet, 2)
	if err != nil {
		return err
	}
	proxyIP, err := idleAddress(front.Subnet, 3)
	if err != nil {
		return err
	}
	backIP, err := idleAddress(back.Subnet, 2)
	if err != nil {
		return err
	}
	state.Config.Listen = net.JoinHostPort(helperIP, "8080")
	state.Config.Peer, state.Config.FrontSubnet, state.Config.BackendSubnet = proxyIP, front.Subnet, back.Subnet
	state.Config.Workload = workload
	if state.Config.Token == "" {
		state.Config.Token, err = idleToken()
		if err != nil {
			return err
		}
	}
	if state.Config.Mode == "" {
		state.Config.Mode = "blocked"
	}
	if state.Config.Targets == nil {
		state.Config.Targets = map[string]string{}
	}
	state.Config.Revision = state.Config.digest()
	for _, dir := range []string{e.idleDir(workload, "config"), e.idleDir(workload, "state"), filepath.Join(e.Root, "idle-wake")} {
		if err = os.MkdirAll(dir, 0700); err != nil {
			return err
		}
	}
	if err = writeJSON(e.idleDir(workload, "config", "config.json"), state.Config); err != nil {
		return err
	}
	name := idleContainer(workload)
	existing, err := e.output(ctx, "docker", "ps", "-aq", "--no-trunc", "--filter", "name=^"+name+"$")
	if err != nil {
		return err
	}
	if existing != "" {
		var detail struct {
			ID     string
			Labels map[string]string
		}
		if err = e.imageJSON(ctx, nil, &detail, "docker", "inspect", "--format", `{"id":{{json .Id}},"labels":{{json .Config.Labels}}}`, existing); err != nil {
			return err
		}
		if !containerIdentity.MatchString(detail.ID) || detail.Labels["cove.managed"] != "idle" || detail.Labels["cove.idle-workload"] != workload || detail.Labels["cove.idle-version"] != "1" {
			return errors.New("idle gateway has unexpected ownership or version")
		}
		existing = detail.ID
	} else {
		image, err := e.linkGatewayImage(ctx)
		if err != nil {
			return err
		}
		args := []string{"docker", "create", "--name", name, "--label", "cove.managed=idle", "--label", "cove.idle-workload=" + workload, "--label", "cove.idle-version=1", "--restart", "unless-stopped", "--network", front.Name, "--ip", helperIP, "--user", fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()), "--read-only", "--cap-drop", "ALL", "--security-opt", "no-new-privileges:true", "--sysctl", "net.ipv4.ip_forward=0", "--memory", "128m", "--pids-limit", "64", "--log-driver", "json-file", "--log-opt", "max-size=1m", "--log-opt", "max-file=2", "--mount", "type=bind,source=" + e.idleDir(workload, "config") + ",target=/config,readonly", "--mount", "type=bind,source=" + e.idleDir(workload, "state") + ",target=/state", "--mount", "type=bind,source=" + filepath.Join(e.Root, "idle-wake") + ",target=/wake,readonly", image, "idle-gateway"}
		if err = e.Run(ctx, e.Root, args, nil, io.Discard); err != nil {
			return err
		}
		existing, err = e.output(ctx, "docker", "inspect", "--format", "{{.Id}}", name)
		if err != nil {
			return err
		}
	}
	if err = e.idleAttach(ctx, back, existing, backIP); err != nil {
		return err
	}
	if err = e.idleAttach(ctx, front, "cove-ingress", proxyIP); err != nil {
		return err
	}
	if err = e.Run(ctx, e.Root, []string{"docker", "start", existing}, nil, io.Discard); err != nil {
		return err
	}
	// Old routers can still use their container-scoped keys until the proxy confirms and drains them.
	endpoints := append([]idleEndpoint{}, state.Endpoints...)
	traffic, err := e.loadTraffic(workload)
	if err != nil {
		return err
	}
	for _, target := range routeTargets(routes) {
		selected := traffic[target.Service]
		if len(selected) == 0 {
			selected = []trafficTarget{{Service: target.Service, Weight: 100}}
		}
		for _, generation := range selected {
			ids, err := e.output(ctx, "docker", "ps", "-q", "--no-trunc", "--filter", "label=com.docker.compose.project="+e.composeProject(workload), "--filter", "label=com.docker.compose.service="+generation.Service)
			if err != nil {
				return err
			}
			for _, id := range strings.Fields(ids) {
				if !containerIdentity.MatchString(id) {
					return errors.New("invalid idle container identity")
				}
				sum := sha256.Sum256([]byte(id + "/" + strconv.Itoa(target.Port)))
				endpoints = append(endpoints, idleEndpoint{Service: generation.Service, Port: target.Port, Container: id, Key: hex.EncodeToString(sum[:16])})
			}
		}
	}
	if state.Config.Mode == "awake" {
		targets := map[string]string{}
		next := []idleEndpoint{}
		seen := map[string]bool{}
		live, err := e.HealthContainers(ctx, workload)
		if err != nil {
			return err
		}
		for _, endpoint := range endpoints {
			if seen[endpoint.Key] {
				continue
			}
			seen[endpoint.Key] = true
			present := false
			for _, container := range live {
				if container.State == "running" && strings.HasPrefix(endpoint.Container, container.ID) {
					present = true
					break
				}
			}
			if !present {
				continue
			}
			var owner struct{ Project, Service string }
			if err = e.imageJSON(ctx, nil, &owner, "docker", "inspect", "--format", `{"project":{{json (index .Config.Labels "com.docker.compose.project")}},"service":{{json (index .Config.Labels "com.docker.compose.service")}}}`, endpoint.Container); err != nil {
				return err
			}
			if owner.Project != e.composeProject(workload) || owner.Service != endpoint.Service {
				return errors.New("idle endpoint ownership changed")
			}
			if err = e.idleAttach(ctx, back, endpoint.Container, ""); err != nil {
				return err
			}
			address, err := e.output(ctx, "docker", "inspect", "--format", `{{with index .NetworkSettings.Networks `+strconv.Quote(back.Name)+`}}{{.IPAddress}}{{end}}`, endpoint.Container)
			if err != nil {
				return err
			}
			ip, err := netip.ParseAddr(address)
			if err != nil || !netip.MustParsePrefix(back.Subnet).Contains(ip) {
				return errors.New("idle endpoint address is outside its network")
			}
			targets[endpoint.Key] = "http://" + net.JoinHostPort(address, strconv.Itoa(endpoint.Port))
			next = append(next, endpoint)
		}
		state.Endpoints, state.Config.Targets = next, targets
	}
	state.Config.Revision = state.Config.digest()
	if err = state.Config.validate(); err != nil {
		return err
	}
	if err = writeJSON(e.idleDir(workload, "config", "config.json"), state.Config); err != nil {
		return err
	}
	wait, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	for {
		code, err := e.idleControl(wait, workload, "POST", "/reload", map[string]string{"revision": state.Config.Revision}, nil)
		if err == nil && code == 204 {
			return nil
		}
		select {
		case <-wait.Done():
			return errors.New("idle gateway did not confirm its configuration")
		case <-time.After(100 * time.Millisecond):
		}
	}
}

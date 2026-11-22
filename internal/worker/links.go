package worker

import (
	"archive/tar"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Private links give one source Compose service TCP access to one target service port. Each node runs
// one gateway container: sources reach it on a dedicated internal network per (source service, alias),
// and it reaches targets on a dedicated internal network per target service. Cross-node traffic uses
// mutual TLS between gateways, pinned to each node's key fingerprint.
const (
	linkGatewayVersion = "2"
	linkGatewayName    = "cove-link"
	linkMeshNetwork    = "cove-link-mesh"
	DefaultLinkPort    = 47800
	maxLinkEntries     = 512
	defaultLinkPool    = "10.250.0.0/16"
)

type LinkPeer struct {
	Address     string `json:"address"`
	Port        int    `json:"port"`
	Fingerprint string `json:"fingerprint"`
}

type LinkSource struct {
	ID       string    `json:"id"`
	Version  int64     `json:"version"`
	Workload string    `json:"workload"`
	Service  string    `json:"service"`
	Alias    string    `json:"alias"`
	Port     int       `json:"port"`
	Peer     *LinkPeer `json:"peer,omitempty"`
	// TargetWorkload and TargetService name the target on this node when Peer is nil.
	TargetWorkload string `json:"target_workload,omitempty"`
	TargetService  string `json:"target_service,omitempty"`
}

type LinkTarget struct {
	ID       string `json:"id"`
	Version  int64  `json:"version"`
	Workload string `json:"workload"`
	Service  string `json:"service"`
	Port     int    `json:"port"`
	// Peer is the source node's key fingerprint, or empty when the source runs on this node.
	Peer string `json:"peer,omitempty"`
}

// LinkConfig is the complete desired private-link state of one node. It replaces the previous state.
type LinkConfig struct {
	Digest  string       `json:"digest"`
	Port    int          `json:"port"`
	Sources []LinkSource `json:"sources,omitempty"`
	Targets []LinkTarget `json:"targets,omitempty"`
}

type LinkResult struct {
	ID      string `json:"id"`
	Version int64  `json:"version"`
	Role    string `json:"role"`
	State   string `json:"state"`
	Detail  string `json:"detail,omitempty"`
}

type LinkStatus struct {
	Digest      string       `json:"digest,omitempty"`
	Fingerprint string       `json:"fingerprint"`
	Results     []LinkResult `json:"results,omitempty"`
}

var linkAliasPattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?(\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)*$`)
var fingerprintPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)
var hostnamePattern = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9.-]{0,251}[A-Za-z0-9])?$`)

// ValidLinkAlias reports whether a source may use alias as a private endpoint name.
func ValidLinkAlias(alias string) bool {
	return len(alias) <= 253 && linkAliasPattern.MatchString(alias) && alias != "localhost" && !strings.HasPrefix(alias, "cove-")
}

// ValidLinkFingerprint reports whether f is a link key fingerprint as workers report it.
func ValidLinkFingerprint(f string) bool { return fingerprintPattern.MatchString(f) }

func ValidLinkAddress(address string) bool {
	if _, err := netip.ParseAddr(address); err == nil {
		return true
	}
	return hostnamePattern.MatchString(address) && !strings.Contains(address, "..")
}

// LinkDigest identifies the content of a configuration independently of its Digest field.
func LinkDigest(c LinkConfig) string {
	c.Digest = ""
	data, _ := json.Marshal(c)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func ValidateLinkConfig(c LinkConfig) error {
	if c.Port < 1 || c.Port > 65535 {
		return errors.New("invalid private link port")
	}
	if len(c.Sources)+len(c.Targets) > maxLinkEntries {
		return fmt.Errorf("at most %d private link endpoints are supported per node", maxLinkEntries)
	}
	if c.Digest != LinkDigest(c) {
		return errors.New("private link configuration digest does not match its content")
	}
	targets := map[string]LinkTarget{}
	for _, t := range c.Targets {
		if !networkID.MatchString(t.ID) || t.Version < 1 || !workloadIdentifier.MatchString(t.Workload) || !serviceIdentifier.MatchString(t.Service) || t.Port < 1 || t.Port > 65535 || (t.Peer != "" && !fingerprintPattern.MatchString(t.Peer)) {
			return errors.New("invalid private link target")
		}
		if _, duplicate := targets[t.ID]; duplicate {
			return errors.New("duplicate private link target")
		}
		targets[t.ID] = t
	}
	sources := map[string]bool{}
	endpoints := map[string]bool{}
	for _, s := range c.Sources {
		if !networkID.MatchString(s.ID) || s.Version < 1 || !workloadIdentifier.MatchString(s.Workload) || !serviceIdentifier.MatchString(s.Service) || !ValidLinkAlias(s.Alias) || s.Port < 1 || s.Port > 65535 {
			return errors.New("invalid private link source")
		}
		if sources[s.ID] {
			return errors.New("duplicate private link source")
		}
		sources[s.ID] = true
		endpoint := s.Workload + "/" + s.Service + "/" + s.Alias + ":" + strconv.Itoa(s.Port)
		if endpoints[endpoint] {
			return errors.New("a source service can use each private endpoint and port only once")
		}
		endpoints[endpoint] = true
		if s.Peer != nil {
			if s.TargetWorkload != "" || s.TargetService != "" || !ValidLinkAddress(s.Peer.Address) || s.Peer.Port < 1 || s.Peer.Port > 65535 || !fingerprintPattern.MatchString(s.Peer.Fingerprint) {
				return errors.New("invalid private link peer")
			}
			continue
		}
		t, ok := targets[s.ID]
		if !ok || t.Peer != "" || t.Workload != s.TargetWorkload || t.Service != s.TargetService || t.Port != s.Port || t.Version != s.Version {
			return errors.New("a same-node private link needs its matching target")
		}
	}
	return nil
}

func linkHash(parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(sum[:8])
}

func linkSourceNetwork(workload, service, alias string) string {
	return "cove-link-s-" + linkHash(workload, service, alias)
}
func linkTargetNetwork(workload, service string) string {
	return "cove-link-t-" + linkHash(workload, service)
}
func linkTargetAlias(workload, service string) string {
	return "cove-target-" + linkHash(workload, service)
}
func linkSourceKey(s LinkSource) string {
	return "source/" + s.ID + "/" + strconv.FormatInt(s.Version, 10)
}
func linkTargetKey(t LinkTarget) string {
	return "target/" + t.ID + "/" + strconv.FormatInt(t.Version, 10)
}

func (e *Engine) linksDir(parts ...string) string {
	return filepath.Join(append([]string{e.Root, "links"}, parts...)...)
}

// LinkIdentity returns this node's link key fingerprint, creating the key on first use. The private key
// never leaves the node.
func (e *Engine) LinkIdentity() (string, error) {
	e.linkMu.Lock()
	defer e.linkMu.Unlock()
	return e.ensureLinkIdentity()
}

func (e *Engine) ensureLinkIdentity() (string, error) {
	dir := e.linksDir("identity")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", err
	}
	certPath, keyPath := filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	if data, err := os.ReadFile(certPath); err == nil {
		if _, keyErr := os.Stat(keyPath); keyErr != nil {
			return "", errors.New("private link key is missing; remove the identity directory to create a new one")
		}
		block, _ := pem.Decode(data)
		if block == nil {
			return "", errors.New("invalid private link certificate")
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return "", err
		}
		return certificateFingerprint(cert), nil
	} else if !os.IsNotExist(err) {
		return "", err
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return "", err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 120))
	if err != nil {
		return "", err
	}
	template := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: "cove-link"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().AddDate(30, 0, 0), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth}, DNSNames: []string{"cove-link"}}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return "", err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return "", err
	}
	if err = writeFileAtomic(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0600); err != nil {
		return "", err
	}
	if err = writeFileAtomic(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0600); err != nil {
		return "", err
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return "", err
	}
	return certificateFingerprint(cert), nil
}

func certificateFingerprint(cert *x509.Certificate) string {
	sum := sha256.Sum256(cert.RawSubjectPublicKeyInfo)
	return hex.EncodeToString(sum[:])
}

func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".write-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err = tmp.Chmod(mode); err == nil {
		_, err = tmp.Write(data)
	}
	if err == nil {
		err = tmp.Sync()
	}
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err = os.Rename(name, path); err != nil {
		return err
	}
	return syncDirectory(filepath.Dir(path))
}

func (e *Engine) loadLinkConfig() (LinkConfig, error) {
	var c LinkConfig
	data, err := os.ReadFile(e.linksDir("desired.json"))
	if os.IsNotExist(err) {
		return c, nil
	}
	if err != nil {
		return c, err
	}
	err = json.Unmarshal(data, &c)
	return c, err
}

// ApplyLinks durably replaces the node's private-link state and reconciles it before replying.
// Repeating a call with the same configuration is safe.
func (e *Engine) ApplyLinks(ctx context.Context, c LinkConfig) (LinkStatus, error) {
	if err := ValidateLinkConfig(c); err != nil {
		return LinkStatus{}, err
	}
	e.linkMu.Lock()
	defer e.linkMu.Unlock()
	if err := os.MkdirAll(e.linksDir(), 0700); err != nil {
		return LinkStatus{}, err
	}
	if err := writeJSON(e.linksDir("desired.json"), c); err != nil {
		return LinkStatus{}, err
	}
	return e.reconcileLinksLocked(ctx, c)
}

func (e *Engine) kickLinks() {
	select {
	case e.linkKick <- struct{}{}:
	default:
	}
}

// linkLoop reapplies the persisted state after restarts, operations and Docker changes, so links do
// not depend on the control plane being reachable.
func (e *Engine) linkLoop(ctx context.Context) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		e.linkMu.Lock()
		// Nodes that never received a link configuration have nothing to reconcile.
		_, err := os.Stat(e.linksDir("desired.json"))
		var c LinkConfig
		if err == nil {
			c, err = e.loadLinkConfig()
		} else if os.IsNotExist(err) {
			err = nil
		}
		if err == nil && c.Port > 0 {
			reconcile, cancel := context.WithTimeout(ctx, 3*time.Minute)
			_, err = e.reconcileLinksLocked(reconcile, c)
			cancel()
		}
		e.linkMu.Unlock()
		if err != nil && ctx.Err() == nil {
			fmt.Fprintln(os.Stderr, "Private link reconciliation failed:", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-e.linkKick:
		}
	}
}

func (e *Engine) linkGatewayPresent(ctx context.Context) bool {
	out, err := e.output(ctx, "docker", "ps", "-aq", "--filter", "name=^"+linkGatewayName+"$", "--filter", "label=cove.managed=link")
	return err == nil && out != ""
}

func (e *Engine) linkGatewayRunning(ctx context.Context) bool {
	out, err := e.output(ctx, "docker", "ps", "-q", "--filter", "name=^"+linkGatewayName+"$", "--filter", "label=cove.managed=link", "--filter", "status=running")
	return err == nil && out != ""
}

type linkNetworkInfo struct {
	Name       string
	Subnet     string
	Labels     map[string]string
	Containers map[string]bool
}

type dockerNetwork struct {
	Name       string
	Labels     map[string]string
	IPAM       struct{ Config []struct{ Subnet string } }
	Containers map[string]struct {
		Name string
	}
}

func (e *Engine) inspectNetworks(ctx context.Context, filter ...string) ([]dockerNetwork, error) {
	ids, err := e.output(ctx, append([]string{"docker", "network", "ls", "-q"}, filter...)...)
	if err != nil {
		return nil, err
	}
	if ids == "" {
		return nil, nil
	}
	b := &limitedBuffer{Limit: 16 << 20}
	if err = e.Run(ctx, e.Root, append([]string{"docker", "network", "inspect"}, strings.Fields(ids)...), nil, b); err != nil {
		return nil, err
	}
	var networks []dockerNetwork
	err = json.Unmarshal(b.data.Bytes(), &networks)
	return networks, err
}

func (e *Engine) linkNetworks(ctx context.Context) (map[string]*linkNetworkInfo, error) {
	networks, err := e.inspectNetworks(ctx, "--filter", "label=cove.managed=link")
	if err != nil {
		return nil, err
	}
	out := map[string]*linkNetworkInfo{}
	for _, n := range networks {
		info := &linkNetworkInfo{Name: n.Name, Labels: n.Labels, Containers: map[string]bool{}}
		if len(n.IPAM.Config) > 0 {
			info.Subnet = n.IPAM.Config[0].Subnet
		}
		for id := range n.Containers {
			info.Containers[id] = true
		}
		out[n.Name] = info
	}
	return out, nil
}

func linkPool() (netip.Prefix, error) {
	value := os.Getenv("COVE_LINK_POOL")
	if value == "" {
		value = defaultLinkPool
	}
	pool, err := netip.ParsePrefix(value)
	if err != nil || !pool.Addr().Is4() || pool.Bits() > 27 {
		return netip.Prefix{}, errors.New("COVE_LINK_POOL must be an IPv4 prefix of /27 or larger")
	}
	return pool.Masked(), nil
}

// ensureLinkNetwork creates an internal bridge network from Cove's dedicated pool. Docker's default
// pools hold only a few dozen networks, so link networks use small /27 subnets instead.
func (e *Engine) ensureLinkNetwork(ctx context.Context, networks map[string]*linkNetworkInfo, name, role, workload string) (*linkNetworkInfo, error) {
	if n, ok := networks[name]; ok {
		if n.Labels["cove.link-workload"] != workload || n.Labels["cove.link-role"] != role {
			return nil, errors.New("existing private link network has unexpected ownership")
		}
		return n, nil
	}
	if existing, _ := e.output(ctx, "docker", "network", "ls", "-q", "--filter", "name=^"+name+"$"); existing != "" {
		return nil, errors.New("a network named " + name + " exists that Cove does not manage")
	}
	pool, err := linkPool()
	if err != nil {
		return nil, err
	}
	all, err := e.inspectNetworks(ctx)
	if err != nil {
		return nil, err
	}
	var used []netip.Prefix
	for _, n := range all {
		for _, config := range n.IPAM.Config {
			if prefix, err := netip.ParsePrefix(config.Subnet); err == nil {
				used = append(used, prefix)
			}
		}
	}
	start := pool.Addr().As4()
	base := uint32(start[0])<<24 | uint32(start[1])<<16 | uint32(start[2])<<8 | uint32(start[3])
	count := uint32(1) << (27 - pool.Bits())
	var lastErr error
	for i := uint32(0); i < count; i++ {
		value := base + i*32
		candidate := netip.PrefixFrom(netip.AddrFrom4([4]byte{byte(value >> 24), byte(value >> 16), byte(value >> 8), byte(value)}), 27)
		overlaps := false
		for _, prefix := range used {
			if prefix.Overlaps(candidate) {
				overlaps = true
				break
			}
		}
		if overlaps {
			continue
		}
		lastErr = e.Run(ctx, e.Root, []string{"docker", "network", "create", "--driver", "bridge", "--internal", "--subnet", candidate.String(), "--label", "cove.managed=link", "--label", "cove.link-role=" + role, "--label", "cove.link-workload=" + workload, name}, nil, io.Discard)
		if lastErr == nil {
			n := &linkNetworkInfo{Name: name, Subnet: candidate.String(), Labels: map[string]string{"cove.link-role": role, "cove.link-workload": workload}, Containers: map[string]bool{}}
			networks[name] = n
			return n, nil
		}
		used = append(used, candidate)
	}
	if lastErr != nil {
		return nil, fmt.Errorf("could not create private link network: %w", lastErr)
	}
	return nil, errors.New("the private link address pool is exhausted; set COVE_LINK_POOL to a larger unused range")
}

func (e *Engine) serviceContainers(ctx context.Context, workload, service string) ([]string, bool, error) {
	names, err := e.serviceNames(workload, service)
	if err != nil {
		return nil, false, err
	}
	var output strings.Builder
	for _, name := range names {
		part, err := e.output(ctx, "docker", "ps", "-a", "--no-trunc", "--filter", "label=com.docker.compose.project=cove-"+workload, "--filter", "label=com.docker.compose.service="+name, "--filter", "label=com.docker.compose.oneoff=False", "--format", "{{.ID}} {{.State}}")
		if err != nil {
			return nil, false, err
		}
		output.WriteString(part)
		output.WriteByte('\n')
	}
	out := output.String()
	var ids []string
	running := false
	for _, line := range strings.Split(out, "\n") {
		id, state, ok := strings.Cut(strings.TrimSpace(line), " ")
		if !ok {
			continue
		}
		if !regexp.MustCompile(`^[a-f0-9]{64}$`).MatchString(id) {
			return nil, false, errors.New("invalid container identity")
		}
		ids = append(ids, id)
		running = running || state == "running"
	}
	return ids, running, nil
}

// attach connects containers to a link network without starting or restarting them.
func (e *Engine) attach(ctx context.Context, n *linkNetworkInfo, container string, aliases ...string) error {
	if n.Containers[container] {
		return nil
	}
	args := []string{"docker", "network", "connect"}
	for _, alias := range aliases {
		args = append(args, "--alias", alias)
	}
	if err := e.Run(ctx, e.Root, append(args, n.Name, container), nil, io.Discard); err != nil {
		return err
	}
	n.Containers[container] = true
	return nil
}

func (e *Engine) linkGatewayImage(ctx context.Context) (string, error) {
	executable, err := os.Executable()
	if err != nil {
		return "", err
	}
	binary, err := os.ReadFile(executable)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(binary)
	tag := "cove-link:" + linkGatewayVersion + "-" + hex.EncodeToString(sum[:6])
	if _, err = e.output(ctx, "docker", "image", "inspect", "--format", "{{.Id}}", tag); err == nil {
		return tag, nil
	}
	dir := e.linksDir("build")
	if err = os.MkdirAll(dir, 0700); err != nil {
		return "", err
	}
	archive := filepath.Join(dir, "rootfs.tar")
	defer os.Remove(archive)
	f, err := os.OpenFile(archive, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if err != nil {
		return "", err
	}
	w := tar.NewWriter(f)
	for _, directory := range []string{"config/", "identity/", "state/"} {
		if err == nil {
			err = w.WriteHeader(&tar.Header{Name: directory, Mode: 0755, Typeflag: tar.TypeDir})
		}
	}
	if err == nil {
		err = w.WriteHeader(&tar.Header{Name: "cove-worker", Mode: 0755, Size: int64(len(binary)), Typeflag: tar.TypeReg})
	}
	if err == nil {
		_, err = w.Write(binary)
	}
	if err == nil {
		err = w.Close()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return "", err
	}
	if err = e.Run(ctx, e.Root, []string{"docker", "image", "import", "--change", `ENTRYPOINT ["/cove-worker"]`, "--change", `CMD ["link-gateway"]`, "--change", "LABEL cove.managed=link", archive, tag}, nil, io.Discard); err != nil {
		return "", fmt.Errorf("could not prepare the private link gateway image: %w", err)
	}
	return tag, nil
}

type gatewayContainer struct {
	ID      string
	Running bool
}

func (e *Engine) ensureLinkGateway(ctx context.Context, port int) (gatewayContainer, error) {
	var gw gatewayContainer
	if out, err := e.output(ctx, "docker", "network", "inspect", "--format", `{{index .Labels "cove.managed"}}`, linkMeshNetwork); err != nil {
		if err = e.Run(ctx, e.Root, []string{"docker", "network", "create", "--driver", "bridge", "--label", "cove.managed=link-mesh", linkMeshNetwork}, nil, io.Discard); err != nil {
			return gw, fmt.Errorf("could not create the private link mesh network: %w", err)
		}
	} else if out != "link-mesh" {
		return gw, errors.New("a network named " + linkMeshNetwork + " exists that Cove does not manage")
	}
	info, err := e.output(ctx, "docker", "inspect", "--format", `{{.Id}} {{.State.Running}} {{index .Config.Labels "cove.managed"}} {{index .Config.Labels "cove.link-version"}} {{index .Config.Labels "cove.link-port"}}`, linkGatewayName)
	if err == nil {
		fields := strings.Fields(info)
		if len(fields) < 3 || fields[2] != "link" {
			return gw, errors.New("a container named " + linkGatewayName + " exists that Cove does not manage")
		}
		if len(fields) == 5 && fields[3] == linkGatewayVersion && fields[4] == strconv.Itoa(port) {
			gw.ID = fields[0]
			if fields[1] != "true" {
				if err = e.Run(ctx, e.Root, []string{"docker", "start", linkGatewayName}, nil, io.Discard); err != nil {
					return gw, fmt.Errorf("could not start the private link gateway: %w", err)
				}
			}
			gw.Running = true
			return gw, nil
		}
		// The gateway protocol or published port changed. Recreating it briefly interrupts this node's links.
		if err = e.Run(ctx, e.Root, []string{"docker", "rm", "-f", linkGatewayName}, nil, io.Discard); err != nil {
			return gw, err
		}
	}
	image, err := e.linkGatewayImage(ctx)
	if err != nil {
		return gw, err
	}
	for _, dir := range []string{"identity", "gateway", "state"} {
		if err = os.MkdirAll(e.linksDir(dir), 0700); err != nil {
			return gw, err
		}
	}
	// A previous gateway's status must not count as confirmation from the new one.
	if err = os.Remove(e.linksDir("state", "status.json")); err != nil && !os.IsNotExist(err) {
		return gw, err
	}
	args := []string{"docker", "run", "--detach", "--name", linkGatewayName,
		"--label", "cove.managed=link", "--label", "cove.link-version=" + linkGatewayVersion, "--label", "cove.link-port=" + strconv.Itoa(port),
		"--restart", "unless-stopped", "--network", linkMeshNetwork, "--publish", fmt.Sprintf("%d:%d/tcp", port, port),
		"--user", fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()), "--read-only", "--cap-drop", "ALL", "--security-opt", "no-new-privileges:true",
		"--sysctl", "net.ipv4.ip_unprivileged_port_start=0", "--sysctl", "net.ipv4.ip_forward=0",
		"--pids-limit", "512", "--log-driver", "json-file", "--log-opt", "max-size=1m", "--log-opt", "max-file=2",
		"--mount", "type=bind,source=" + e.linksDir("identity") + ",target=/identity,readonly",
		"--mount", "type=bind,source=" + e.linksDir("gateway") + ",target=/config,readonly",
		"--mount", "type=bind,source=" + e.linksDir("state") + ",target=/state",
		image}
	if err = e.Run(ctx, e.Root, args, nil, io.Discard); err != nil {
		return gw, fmt.Errorf("could not start the private link gateway; check that port %d is free: %w", port, err)
	}
	id, err := e.output(ctx, "docker", "inspect", "--format", "{{.Id}}", linkGatewayName)
	if err != nil {
		return gw, err
	}
	// Images from earlier worker versions are no longer used once the gateway is recreated.
	if tags, listErr := e.output(ctx, "docker", "image", "ls", "--filter", "reference=cove-link:*", "--format", "{{.Repository}}:{{.Tag}}"); listErr == nil {
		for _, tag := range strings.Fields(tags) {
			if tag != image {
				_ = e.Run(ctx, e.Root, []string{"docker", "image", "rm", tag}, nil, io.Discard)
			}
		}
	}
	return gatewayContainer{ID: id, Running: true}, nil
}

func (e *Engine) readGatewayConfig() gatewayConfig {
	var c gatewayConfig
	data, err := os.ReadFile(e.linksDir("gateway", "config.json"))
	if err == nil && json.Unmarshal(data, &c) == nil {
		return c
	}
	return gatewayConfig{}
}

// pushGatewayConfig replaces the gateway configuration and waits until the gateway reports it. The
// returned map holds listener and grant errors keyed like the configuration entries.
func (e *Engine) pushGatewayConfig(ctx context.Context, c gatewayConfig) (map[string]string, error) {
	c.Digest = gatewayDigest(c)
	if err := os.MkdirAll(e.linksDir("gateway"), 0700); err != nil {
		return nil, err
	}
	if err := writeJSON(e.linksDir("gateway", "config.json"), c); err != nil {
		return nil, err
	}
	_ = e.Run(ctx, e.Root, []string{"docker", "kill", "--signal", "HUP", linkGatewayName}, nil, io.Discard)
	deadline := time.Now().Add(20 * time.Second)
	for {
		var status gatewayStatus
		data, err := os.ReadFile(e.linksDir("state", "status.json"))
		if err == nil && json.Unmarshal(data, &status) == nil && status.Digest == c.Digest {
			return status.Errors, nil
		}
		if time.Now().After(deadline) {
			return nil, errors.New("the private link gateway did not confirm its configuration")
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
}

func sameListener(a, b gatewayListener) bool {
	return a.Key == b.Key && a.Subnet == b.Subnet && a.Port == b.Port && a.Dial == b.Dial && a.DialSubnet == b.DialSubnet && ((a.Peer == nil && b.Peer == nil) || (a.Peer != nil && b.Peer != nil && *a.Peer == *b.Peer))
}

// reconcileLinksLocked applies c to Docker and the gateway. Removals take effect before additions,
// and a gateway that cannot confirm a removal is stopped, so failures never leave revoked access open.
func (e *Engine) reconcileLinksLocked(ctx context.Context, c LinkConfig) (LinkStatus, error) {
	status := LinkStatus{Digest: c.Digest}
	results := map[string]*LinkResult{}
	var order []string
	for _, s := range c.Sources {
		key := linkSourceKey(s)
		results[key] = &LinkResult{ID: s.ID, Version: s.Version, Role: "source", State: "applied"}
		order = append(order, key)
	}
	for _, t := range c.Targets {
		key := linkTargetKey(t)
		results[key] = &LinkResult{ID: t.ID, Version: t.Version, Role: "target", State: "applied"}
		order = append(order, key)
	}
	fail := func(key, detail string) {
		if r := results[key]; r != nil && r.State != "error" {
			r.State, r.Detail = "error", detail
		}
	}
	finish := func() (LinkStatus, error) {
		for _, key := range order {
			status.Results = append(status.Results, *results[key])
		}
		return status, writeJSON(e.linksDir("results.json"), status)
	}
	fingerprint, err := e.ensureLinkIdentity()
	if err != nil {
		return status, err
	}
	status.Fingerprint = fingerprint
	if len(c.Sources) == 0 && len(c.Targets) == 0 {
		if e.linkGatewayPresent(ctx) {
			if err = e.Run(ctx, e.Root, []string{"docker", "rm", "-f", linkGatewayName}, nil, io.Discard); err != nil {
				return status, fmt.Errorf("could not remove the private link gateway: %w", err)
			}
		}
		_ = os.Remove(e.linksDir("gateway", "config.json"))
		return finish()
	}

	previous := e.readGatewayConfig()
	wanted := map[string]bool{}
	for key := range results {
		wanted[key] = true
	}
	if len(previous.Listeners) > 0 || len(previous.Grants) > 0 {
		shrunk := gatewayConfig{MeshPort: previous.MeshPort, MeshSubnet: previous.MeshSubnet}
		for _, l := range previous.Listeners {
			if wanted[l.Key] {
				shrunk.Listeners = append(shrunk.Listeners, l)
			}
		}
		for _, g := range previous.Grants {
			if wanted[g.Key] {
				shrunk.Grants = append(shrunk.Grants, g)
			}
		}
		if len(shrunk.Listeners) != len(previous.Listeners) || len(shrunk.Grants) != len(previous.Grants) {
			if !e.linkGatewayRunning(ctx) {
				// A stopped or missing gateway loads this file when it starts, so it must never start
				// with revoked entries.
				shrunk.Digest = gatewayDigest(shrunk)
				if err = writeJSON(e.linksDir("gateway", "config.json"), shrunk); err != nil {
					return status, err
				}
			} else if _, err = e.pushGatewayConfig(ctx, shrunk); err != nil {
				if stopErr := e.Run(context.WithoutCancel(ctx), e.Root, []string{"docker", "stop", "--time", "5", linkGatewayName}, nil, io.Discard); stopErr != nil {
					return status, fmt.Errorf("could not revoke private links or stop the gateway: %w", errors.Join(err, stopErr))
				}
				for key := range results {
					fail(key, "The gateway did not confirm a revocation, so it was stopped; all private links on this node are down until it recovers.")
				}
				return finish()
			}
		}
	}

	gw, err := e.ensureLinkGateway(ctx, c.Port)
	if err != nil {
		for key := range results {
			fail(key, err.Error())
		}
		return finish()
	}
	networks, err := e.linkNetworks(ctx)
	if err != nil {
		return status, err
	}
	mesh, err := e.inspectNetworks(ctx, "--filter", "name=^"+linkMeshNetwork+"$")
	if err != nil || len(mesh) != 1 || len(mesh[0].IPAM.Config) == 0 {
		return status, errors.New("could not inspect the private link mesh network")
	}
	if _, attached := mesh[0].Containers[gw.ID]; !attached {
		if err = e.Run(ctx, e.Root, []string{"docker", "network", "connect", linkMeshNetwork, gw.ID}, nil, io.Discard); err != nil {
			return status, fmt.Errorf("could not attach the private link gateway to its mesh network: %w", err)
		}
	}
	full := gatewayConfig{MeshPort: c.Port, MeshSubnet: mesh[0].IPAM.Config[0].Subnet}
	desiredNetworks := map[string]bool{}
	e.mu.Lock()
	busy := map[string]bool{}
	for _, s := range c.Sources {
		busy[s.Workload] = e.busy(s.Workload)
	}
	for _, t := range c.Targets {
		busy[t.Workload] = e.busy(t.Workload)
	}
	e.mu.Unlock()

	targetReady := map[string]bool{}
	targetSubnets := map[string]string{}
	for _, t := range c.Targets {
		key := linkTargetKey(t)
		name := linkTargetNetwork(t.Workload, t.Service)
		desiredNetworks[name] = true
		n, err := e.ensureLinkNetwork(ctx, networks, name, "target", t.Workload)
		if err == nil {
			err = e.attach(ctx, n, gw.ID)
		}
		var containers []string
		var active bool
		if err == nil && !busy[t.Workload] {
			containers, active, err = e.serviceContainers(ctx, t.Workload, t.Service)
			for _, container := range containers {
				if err == nil {
					err = e.attach(ctx, n, container, linkTargetAlias(t.Workload, t.Service))
				}
			}
		}
		if err != nil {
			fail(key, "Could not prepare the target network: "+err.Error())
			continue
		}
		targetReady[t.ID] = true
		targetSubnets[t.ID] = n.Subnet
		if busy[t.Workload] {
			results[key].Detail = "The target has an operation in progress; its containers are attached when it finishes."
		} else if len(containers) == 0 {
			results[key].Detail = "The target has no containers yet. Connections are refused until it is deployed."
		} else if !active {
			results[key].Detail = "The target is stopped. Connections are refused until it starts."
		}
		if t.Peer != "" {
			full.Grants = append(full.Grants, gatewayGrant{Key: key, ID: t.ID, Peer: t.Peer, Dial: net.JoinHostPort(linkTargetAlias(t.Workload, t.Service), strconv.Itoa(t.Port)), Subnet: n.Subnet})
		}
	}
	for _, s := range c.Sources {
		key := linkSourceKey(s)
		if s.Peer == nil && !targetReady[s.ID] {
			fail(key, "The target on this node is not ready.")
			continue
		}
		name := linkSourceNetwork(s.Workload, s.Service, s.Alias)
		desiredNetworks[name] = true
		n, err := e.ensureLinkNetwork(ctx, networks, name, "source", s.Workload)
		if err == nil {
			err = e.attach(ctx, n, gw.ID, s.Alias)
		}
		var containers []string
		if err == nil && !busy[s.Workload] {
			containers, _, err = e.serviceContainers(ctx, s.Workload, s.Service)
			for _, container := range containers {
				if err == nil {
					err = e.attach(ctx, n, container)
				}
			}
		}
		if err != nil {
			fail(key, "Could not prepare the source network: "+err.Error())
			continue
		}
		if !busy[s.Workload] && len(containers) == 0 {
			results[key].Detail = "The source has no containers yet; it joins on its next deploy."
		}
		listener := gatewayListener{Key: key, ID: s.ID, Subnet: n.Subnet, Port: s.Port, Peer: s.Peer}
		if s.Peer == nil {
			listener.Dial = net.JoinHostPort(linkTargetAlias(s.TargetWorkload, s.TargetService), strconv.Itoa(s.Port))
			listener.DialSubnet = targetSubnets[s.ID]
		}
		full.Listeners = append(full.Listeners, listener)
	}
	sort.Slice(full.Listeners, func(i, j int) bool { return full.Listeners[i].Key < full.Listeners[j].Key })
	sort.Slice(full.Grants, func(i, j int) bool { return full.Grants[i].Key < full.Grants[j].Key })
	errorsByKey, err := e.pushGatewayConfig(ctx, full)
	if err != nil {
		for key := range results {
			fail(key, "The gateway did not confirm this connection; it stays unavailable until the gateway recovers.")
		}
	}
	for key, message := range errorsByKey {
		fail(key, "The gateway could not serve this connection: "+message)
	}
	// Leave stale networks in place: releases kept for rollback may still reference them. Detaching
	// the gateway removes all access through them.
	for name, n := range networks {
		if !desiredNetworks[name] && n.Containers[gw.ID] {
			if err := e.Run(ctx, e.Root, []string{"docker", "network", "disconnect", "--force", name, gw.ID}, nil, io.Discard); err != nil {
				fmt.Fprintln(os.Stderr, "Could not detach the private link gateway from", name+":", err)
			}
		}
	}
	return finish()
}

// linkComposeNetworks returns the link networks each service of a workload joins at deploy time, so
// recreated containers start attached.
func (e *Engine) linkComposeNetworks(ctx context.Context, workload string) (map[string]map[string][]string, error) {
	e.linkMu.Lock()
	defer e.linkMu.Unlock()
	c, err := e.loadLinkConfig()
	if err != nil {
		return nil, err
	}
	out := map[string]map[string][]string{}
	add := func(service, network string, aliases ...string) {
		if out[service] == nil {
			out[service] = map[string][]string{}
		}
		out[service][network] = aliases
	}
	var networks map[string]*linkNetworkInfo
	ensure := func(name, role string) error {
		if networks == nil {
			if networks, err = e.linkNetworks(ctx); err != nil {
				return err
			}
		}
		_, err := e.ensureLinkNetwork(ctx, networks, name, role, workload)
		return err
	}
	for _, s := range c.Sources {
		if s.Workload == workload {
			name := linkSourceNetwork(s.Workload, s.Service, s.Alias)
			if err = ensure(name, "source"); err != nil {
				return nil, err
			}
			add(s.Service, name)
		}
	}
	for _, t := range c.Targets {
		if t.Workload == workload {
			name := linkTargetNetwork(t.Workload, t.Service)
			if err = ensure(name, "target"); err != nil {
				return nil, err
			}
			add(t.Service, name, linkTargetAlias(t.Workload, t.Service))
		}
	}
	return out, nil
}

func addLinkNetworks(config map[string]any, attachments map[string]map[string][]string) error {
	services, ok := config["services"].(map[string]any)
	if !ok {
		return errors.New("Compose has no services")
	}
	networks, ok := config["networks"].(map[string]any)
	if !ok {
		networks = map[string]any{}
		config["networks"] = networks
	}
	for key, value := range networks {
		// Resolved Compose names every network, so a renamed key cannot join another resource's link network.
		network, _ := value.(map[string]any)
		name, _ := network["name"].(string)
		if strings.HasPrefix(key, "cove-link-") || strings.HasPrefix(name, "cove-link-") {
			return errors.New("Compose uses a reserved private link network name")
		}
	}
	names := make([]string, 0, len(attachments))
	for name := range attachments {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		service, ok := services[name].(map[string]any)
		if !ok {
			// A connection may name a service that this release removed; it applies again once it returns.
			continue
		}
		if _, ok = service["network_mode"]; ok {
			return errors.New("private connections cannot be used with network_mode on service " + name)
		}
		existing, ok := service["networks"].(map[string]any)
		if !ok {
			return errors.New("could not resolve service networks")
		}
		for network, aliases := range attachments[name] {
			networks[network] = map[string]any{"external": true, "name": network}
			entry := map[string]any{}
			if len(aliases) > 0 {
				entry["aliases"] = aliases
			}
			existing[network] = entry
		}
	}
	return nil
}

func (e *Engine) prepareLinks(ctx context.Context, r Request, dir string, args, env []string) (string, bool, error) {
	attachments, err := e.linkComposeNetworks(ctx, r.Workload)
	if err != nil || len(attachments) == 0 {
		return "", false, err
	}
	resolved := &limitedBuffer{Limit: 2 << 20}
	if err = e.Run(ctx, dir, append(append([]string{}, args...), "config", "--format", "json"), env, resolved); err != nil {
		return "", false, errors.New("could not resolve Compose for private connections")
	}
	var config map[string]any
	if err = decodeResolvedCompose(resolved.data.Bytes(), &config); err != nil {
		return "", false, errors.New("invalid resolved Compose configuration")
	}
	if err = addLinkNetworks(config, attachments); err != nil {
		return "", false, err
	}
	path := filepath.Join(dir, r.ID+"-links.yaml")
	return path, true, writeJSON(path, literalCompose(config))
}

// removeWorkloadLinks revokes a removed workload's links locally and deletes its link networks.
func (e *Engine) removeWorkloadLinks(ctx context.Context, workload string) error {
	e.linkMu.Lock()
	defer e.linkMu.Unlock()
	if _, err := os.Stat(e.linksDir("desired.json")); os.IsNotExist(err) {
		return nil
	}
	c, err := e.loadLinkConfig()
	if err != nil {
		return err
	}
	kept := LinkConfig{Port: c.Port}
	for _, s := range c.Sources {
		if s.Workload != workload && s.TargetWorkload != workload {
			kept.Sources = append(kept.Sources, s)
		}
	}
	removedTargets := map[string]bool{}
	for _, t := range c.Targets {
		if t.Workload != workload {
			kept.Targets = append(kept.Targets, t)
		} else {
			removedTargets[t.ID] = true
		}
	}
	if len(kept.Sources) != len(c.Sources) || len(kept.Targets) != len(c.Targets) {
		var sources []LinkSource
		for _, s := range kept.Sources {
			if s.Peer != nil || !removedTargets[s.ID] {
				sources = append(sources, s)
			}
		}
		kept.Sources = sources
		kept.Digest = LinkDigest(kept)
		if err = writeJSON(e.linksDir("desired.json"), kept); err != nil {
			return err
		}
		if _, err = e.reconcileLinksLocked(ctx, kept); err != nil {
			return err
		}
	}
	networks, err := e.linkNetworks(ctx)
	if err != nil {
		return err
	}
	for name, n := range networks {
		if n.Labels["cove.link-workload"] != workload {
			continue
		}
		for container := range n.Containers {
			if err = e.Run(ctx, e.Root, []string{"docker", "network", "disconnect", "--force", name, container}, nil, io.Discard); err != nil {
				return err
			}
		}
		if err = e.Run(ctx, e.Root, []string{"docker", "network", "rm", name}, nil, io.Discard); err != nil {
			return err
		}
	}
	return nil
}

// ValidServiceName reports whether name can identify a Compose service.
func ValidServiceName(name string) bool { return serviceIdentifier.MatchString(name) }

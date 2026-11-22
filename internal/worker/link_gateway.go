package worker

import (
	"bufio"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

type gatewayListener struct {
	Key    string `json:"key"`
	ID     string `json:"id"`
	Subnet string `json:"subnet"`
	Port   int    `json:"port"`
	Dial   string `json:"dial,omitempty"`
	// DialSubnet is the target network of a same-node listener; Dial only connects to addresses in it.
	DialSubnet string    `json:"dial_subnet,omitempty"`
	Peer       *LinkPeer `json:"peer,omitempty"`
}

type gatewayGrant struct {
	Key  string `json:"key"`
	ID   string `json:"id"`
	Peer string `json:"peer"`
	Dial string `json:"dial"`
	// Subnet is the target network; Dial only connects to addresses in it.
	Subnet string `json:"subnet,omitempty"`
}

type gatewayConfig struct {
	Digest     string            `json:"digest"`
	MeshPort   int               `json:"mesh_port"`
	MeshSubnet string            `json:"mesh_subnet"`
	Listeners  []gatewayListener `json:"listeners,omitempty"`
	Grants     []gatewayGrant    `json:"grants,omitempty"`
}

type gatewayStatus struct {
	Digest string            `json:"digest"`
	Errors map[string]string `json:"errors,omitempty"`
}

const linkPreamble = "COVE-LINK/1 "

// tracker owns the live connections of one listener or grant so revoking it closes them.
type tracker struct {
	mu     sync.Mutex
	closed bool
	conns  map[net.Conn]struct{}
}

func newTracker() *tracker { return &tracker{conns: map[net.Conn]struct{}{}} }

func (t *tracker) add(conns ...net.Conn) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return false
	}
	for _, c := range conns {
		t.conns[c] = struct{}{}
	}
	return true
}

func (t *tracker) remove(conns ...net.Conn) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, c := range conns {
		delete(t.conns, c)
	}
}

func (t *tracker) close() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.closed = true
	for c := range t.conns {
		c.Close()
	}
	t.conns = map[net.Conn]struct{}{}
}

type activeListener struct {
	config  gatewayListener
	subnet  *net.IPNet
	ln      net.Listener
	tracker *tracker
}

type activeGrant struct {
	config  gatewayGrant
	tracker *tracker
}

// LinkGateway serves one node's private links. It accepts source traffic only from each listener's
// own network and accepts peer traffic only over mutual TLS from the pinned source node.
type LinkGateway struct {
	ConfigPath string
	StatusPath string
	CertPath   string
	KeyPath    string
	// BindAddress finds this gateway's address on a network; tests replace it.
	BindAddress func(*net.IPNet) (net.IP, error)
	// DialTimeout bounds upstream connection attempts.
	DialTimeout time.Duration

	mu        sync.Mutex
	certs     []tls.Certificate
	listeners map[string]*activeListener
	grants    map[string]*activeGrant
	mesh      net.Listener
	meshAddr  string
	// Peer connections arrive on a public port, so they get their own slots and cannot starve sources.
	sourceSlots chan struct{}
	meshSlots   chan struct{}
	status      string
}

// RunLinkGateway runs the gateway inside its container until ctx ends. reload requests an immediate
// configuration check; the configuration is also polled.
func RunLinkGateway(ctx context.Context, reload <-chan os.Signal) error {
	g := &LinkGateway{ConfigPath: "/config/config.json", StatusPath: "/state/status.json", CertPath: "/identity/cert.pem", KeyPath: "/identity/key.pem"}
	return g.Run(ctx, reload)
}

func (g *LinkGateway) Run(ctx context.Context, reload <-chan os.Signal) error {
	cert, err := tls.LoadX509KeyPair(g.CertPath, g.KeyPath)
	if err != nil {
		return fmt.Errorf("could not load the private link identity: %w", err)
	}
	g.mu.Lock()
	g.certs = []tls.Certificate{cert}
	g.listeners = map[string]*activeListener{}
	g.grants = map[string]*activeGrant{}
	g.sourceSlots = make(chan struct{}, 4096)
	g.meshSlots = make(chan struct{}, 1024)
	if g.BindAddress == nil {
		g.BindAddress = interfaceAddress
	}
	if g.DialTimeout == 0 {
		g.DialTimeout = 10 * time.Second
	}
	g.mu.Unlock()
	defer g.shutdown()
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		if err := g.Apply(); err != nil {
			fmt.Fprintln(os.Stderr, "private link configuration:", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-reload:
		case <-ticker.C:
		}
	}
}

func interfaceAddress(subnet *net.IPNet) (net.IP, error) {
	addresses, err := net.InterfaceAddrs()
	if err != nil {
		return nil, err
	}
	for _, address := range addresses {
		if ipNet, ok := address.(*net.IPNet); ok && subnet.Contains(ipNet.IP) {
			return ipNet.IP, nil
		}
	}
	return nil, errors.New("the gateway is not attached to network " + subnet.String() + " yet")
}

func (g *LinkGateway) shutdown() {
	g.mu.Lock()
	defer g.mu.Unlock()
	for key, l := range g.listeners {
		l.ln.Close()
		l.tracker.close()
		delete(g.listeners, key)
	}
	for key, grant := range g.grants {
		grant.tracker.close()
		delete(g.grants, key)
	}
	if g.mesh != nil {
		g.mesh.Close()
		g.mesh = nil
	}
}

// Apply reads the configuration and reconciles listeners and grants. Unchanged entries keep their
// connections; removed or changed entries are closed with all of their connections.
func (g *LinkGateway) Apply() error {
	var c gatewayConfig
	data, err := os.ReadFile(g.ConfigPath)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if err == nil {
		if err = json.Unmarshal(data, &c); err != nil {
			// Fail closed: an unreadable configuration serves nothing.
			c = gatewayConfig{Digest: "invalid"}
		}
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	errs := map[string]string{}
	listeners := map[string]gatewayListener{}
	for _, l := range c.Listeners {
		listeners[l.Key] = l
	}
	for key, active := range g.listeners {
		if next, ok := listeners[key]; !ok || !sameListener(active.config, next) {
			active.ln.Close()
			active.tracker.close()
			delete(g.listeners, key)
		}
	}
	for key, l := range listeners {
		if _, ok := g.listeners[key]; ok {
			continue
		}
		_, subnet, err := net.ParseCIDR(l.Subnet)
		if err != nil || l.Port < 1 || l.Port > 65535 || (l.Dial == "") == (l.Peer == nil) {
			errs[key] = "invalid listener"
			continue
		}
		ip, err := g.BindAddress(subnet)
		if err != nil {
			errs[key] = err.Error()
			continue
		}
		ln, err := net.Listen("tcp", net.JoinHostPort(ip.String(), strconv.Itoa(l.Port)))
		if err != nil {
			errs[key] = err.Error()
			continue
		}
		active := &activeListener{config: l, subnet: subnet, ln: ln, tracker: newTracker()}
		g.listeners[key] = active
		go g.acceptSource(active)
	}
	grants := map[string]gatewayGrant{}
	for _, grant := range c.Grants {
		grants[grant.ID] = grant
	}
	for id, active := range g.grants {
		if next, ok := grants[id]; !ok || next != active.config {
			active.tracker.close()
			delete(g.grants, id)
		}
	}
	for id, grant := range grants {
		if _, ok := g.grants[id]; !ok {
			g.grants[id] = &activeGrant{config: grant, tracker: newTracker()}
		}
	}
	if len(g.grants) == 0 || c.MeshPort < 1 {
		if g.mesh != nil {
			g.mesh.Close()
			g.mesh = nil
		}
	} else {
		_, subnet, err := net.ParseCIDR(c.MeshSubnet)
		var ip net.IP
		if err == nil {
			ip, err = g.BindAddress(subnet)
		}
		address := ""
		if err == nil {
			address = net.JoinHostPort(ip.String(), strconv.Itoa(c.MeshPort))
		}
		if g.mesh != nil && g.meshAddr != address {
			g.mesh.Close()
			g.mesh = nil
		}
		if err == nil && g.mesh == nil {
			var ln net.Listener
			if ln, err = net.Listen("tcp", address); err == nil {
				g.mesh, g.meshAddr = ln, address
				go g.acceptMesh(ln)
			}
		}
		if err != nil {
			for _, grant := range c.Grants {
				errs[grant.Key] = "peer listener unavailable: " + err.Error()
			}
		}
	}
	return g.writeStatus(gatewayStatus{Digest: c.Digest, Errors: errs})
}

func (g *LinkGateway) writeStatus(s gatewayStatus) error {
	data, err := json.Marshal(s)
	if err != nil || g.StatusPath == "" || string(data) == g.status {
		return err
	}
	if err = writeFileAtomic(g.StatusPath, data, 0600); err != nil {
		return err
	}
	g.status = string(data)
	return nil
}

func acquire(slots chan struct{}) bool {
	select {
	case slots <- struct{}{}:
		return true
	default:
		return false
	}
}

// dialTarget connects to a target name, resolving it only to addresses on the target's own network so
// a container on another network that registers the same name cannot receive the traffic.
func (g *LinkGateway) dialTarget(address, subnet string) (net.Conn, error) {
	dialer := &net.Dialer{Timeout: g.DialTimeout, KeepAlive: 30 * time.Second}
	if subnet == "" {
		return dialer.Dial("tcp", address)
	}
	_, allowed, err := net.ParseCIDR(subnet)
	if err != nil {
		return nil, err
	}
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), g.DialTimeout)
	defer cancel()
	addresses, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, err
	}
	err = errors.New("the target has no address on its private link network")
	for _, a := range addresses {
		if !allowed.Contains(a.IP) {
			continue
		}
		var conn net.Conn
		if conn, err = dialer.DialContext(ctx, "tcp", net.JoinHostPort(a.IP.String(), port)); err == nil {
			return conn, nil
		}
	}
	return nil, err
}

func (g *LinkGateway) acceptSource(l *activeListener) {
	for {
		conn, err := l.ln.Accept()
		if err != nil {
			if !errors.Is(err, net.ErrClosed) {
				time.Sleep(100 * time.Millisecond)
				continue
			}
			return
		}
		go g.serveSource(l, conn)
	}
}

func (g *LinkGateway) serveSource(l *activeListener, conn net.Conn) {
	remote, ok := conn.RemoteAddr().(*net.TCPAddr)
	if !ok || !l.subnet.Contains(remote.IP) || !acquire(g.sourceSlots) {
		conn.Close()
		return
	}
	defer func() { <-g.sourceSlots }()
	if !l.tracker.add(conn) {
		conn.Close()
		return
	}
	defer l.tracker.remove(conn)
	var upstream net.Conn
	var reader io.Reader
	var err error
	if l.config.Peer != nil {
		upstream, reader, err = g.dialPeer(*l.config.Peer, l.config.ID)
	} else {
		upstream, err = g.dialTarget(l.config.Dial, l.config.DialSubnet)
		reader = upstream
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "connection", l.config.ID, "upstream unavailable:", err)
		conn.Close()
		return
	}
	if !l.tracker.add(upstream) {
		conn.Close()
		upstream.Close()
		return
	}
	defer l.tracker.remove(upstream)
	pipe(conn, conn, upstream, reader)
}

func pinnedPeer(fingerprints func(string) bool) func([][]byte, [][]*x509.Certificate) error {
	return func(raw [][]byte, _ [][]*x509.Certificate) error {
		if len(raw) == 0 {
			return errors.New("peer certificate required")
		}
		cert, err := x509.ParseCertificate(raw[0])
		if err != nil {
			return err
		}
		if !fingerprints(certificateFingerprint(cert)) {
			return errors.New("peer is not authorized for private links")
		}
		return nil
	}
}

func (g *LinkGateway) dialPeer(peer LinkPeer, id string) (net.Conn, io.Reader, error) {
	g.mu.Lock()
	certs := g.certs
	g.mu.Unlock()
	config := &tls.Config{Certificates: certs, MinVersion: tls.VersionTLS13, ServerName: "cove-link", InsecureSkipVerify: true,
		VerifyPeerCertificate: pinnedPeer(func(fingerprint string) bool { return fingerprint == peer.Fingerprint })}
	dialer := &net.Dialer{Timeout: g.DialTimeout, KeepAlive: 30 * time.Second}
	conn, err := tls.DialWithDialer(dialer, "tcp", net.JoinHostPort(peer.Address, strconv.Itoa(peer.Port)), config)
	if err != nil {
		return nil, nil, err
	}
	_ = conn.SetDeadline(time.Now().Add(g.DialTimeout))
	reader := bufio.NewReaderSize(conn, 4096)
	if _, err = io.WriteString(conn, linkPreamble+id+"\n"); err == nil {
		var line string
		line, err = readLine(reader)
		if err == nil && line != "OK" {
			err = errors.New("peer refused the connection")
		}
	}
	if err != nil {
		conn.Close()
		return nil, nil, err
	}
	_ = conn.SetDeadline(time.Time{})
	return conn, reader, nil
}

func readLine(r *bufio.Reader) (string, error) {
	var line []byte
	for len(line) <= 128 {
		b, err := r.ReadByte()
		if err != nil {
			return "", err
		}
		if b == '\n' {
			return string(line), nil
		}
		line = append(line, b)
	}
	return "", errors.New("line too long")
}

func (g *LinkGateway) acceptMesh(ln net.Listener) {
	g.mu.Lock()
	certs := g.certs
	g.mu.Unlock()
	config := &tls.Config{Certificates: certs, MinVersion: tls.VersionTLS13, ClientAuth: tls.RequireAnyClientCert,
		VerifyPeerCertificate: pinnedPeer(func(fingerprint string) bool {
			g.mu.Lock()
			defer g.mu.Unlock()
			for _, grant := range g.grants {
				if grant.config.Peer == fingerprint {
					return true
				}
			}
			return false
		})}
	for {
		conn, err := ln.Accept()
		if err != nil {
			if !errors.Is(err, net.ErrClosed) {
				time.Sleep(100 * time.Millisecond)
				continue
			}
			return
		}
		go g.serveMesh(tls.Server(conn, config))
	}
}

func (g *LinkGateway) serveMesh(conn *tls.Conn) {
	if !acquire(g.meshSlots) {
		conn.Close()
		return
	}
	defer func() { <-g.meshSlots }()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	if err := conn.Handshake(); err != nil {
		conn.Close()
		return
	}
	reader := bufio.NewReaderSize(conn, 4096)
	line, err := readLine(reader)
	id, ok := strings.CutPrefix(line, linkPreamble)
	if err != nil || !ok {
		conn.Close()
		return
	}
	state := conn.ConnectionState()
	peer := certificateFingerprint(state.PeerCertificates[0])
	g.mu.Lock()
	grant := g.grants[id]
	g.mu.Unlock()
	if grant == nil || grant.config.Peer != peer {
		fmt.Fprintln(os.Stderr, "refused peer connection for", id)
		_, _ = io.WriteString(conn, "NO\n")
		conn.Close()
		return
	}
	if !grant.tracker.add(conn) {
		conn.Close()
		return
	}
	defer grant.tracker.remove(conn)
	upstream, err := g.dialTarget(grant.config.Dial, grant.config.Subnet)
	if err != nil {
		fmt.Fprintln(os.Stderr, "connection", id, "target unavailable:", err)
		_, _ = io.WriteString(conn, "NO\n")
		conn.Close()
		return
	}
	if !grant.tracker.add(upstream) {
		conn.Close()
		upstream.Close()
		return
	}
	defer grant.tracker.remove(upstream)
	if _, err = io.WriteString(conn, "OK\n"); err != nil {
		conn.Close()
		upstream.Close()
		return
	}
	_ = conn.SetDeadline(time.Time{})
	pipe(conn, reader, upstream, upstream)
}

// pipe copies both directions, forwarding half-closes, until both directions finish.
func pipe(a net.Conn, aReader io.Reader, b net.Conn, bReader io.Reader) {
	done := make(chan struct{}, 2)
	copyHalf := func(dst net.Conn, src io.Reader) {
		_, _ = io.Copy(dst, src)
		if half, ok := dst.(interface{ CloseWrite() error }); ok {
			_ = half.CloseWrite()
		} else {
			dst.Close()
		}
		done <- struct{}{}
	}
	go copyHalf(b, aReader)
	go copyHalf(a, bReader)
	<-done
	<-done
	a.Close()
	b.Close()
}

func gatewayDigest(c gatewayConfig) string {
	c.Digest = ""
	data, _ := json.Marshal(c)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

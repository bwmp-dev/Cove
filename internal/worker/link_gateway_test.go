package worker

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

type testGateway struct {
	*LinkGateway
	fingerprint string
	reload      chan os.Signal
}

func startTestGateway(t *testing.T) *testGateway {
	t.Helper()
	e := &Engine{Root: t.TempDir()}
	fingerprint, err := e.LinkIdentity()
	if err != nil {
		t.Fatal(err)
	}
	g := &testGateway{LinkGateway: &LinkGateway{
		ConfigPath: filepath.Join(e.Root, "config.json"), StatusPath: filepath.Join(e.Root, "status.json"),
		CertPath: e.linksDir("identity", "cert.pem"), KeyPath: e.linksDir("identity", "key.pem"), DialTimeout: 2 * time.Second,
		// Each test network is a /30: the gateway takes the first host address.
		BindAddress: func(subnet *net.IPNet) (net.IP, error) {
			ip := subnet.IP.To4()
			return net.IPv4(ip[0], ip[1], ip[2], ip[3]+1), nil
		},
	}, fingerprint: fingerprint, reload: make(chan os.Signal, 1)}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- g.Run(ctx, g.reload) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	})
	return g
}

func (g *testGateway) configure(t *testing.T, c gatewayConfig) map[string]string {
	t.Helper()
	c.Digest = gatewayDigest(c)
	data, _ := json.Marshal(c)
	if err := os.WriteFile(g.ConfigPath, data, 0600); err != nil {
		t.Fatal(err)
	}
	g.reload <- os.Interrupt
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var status gatewayStatus
		if data, err := os.ReadFile(g.StatusPath); err == nil && json.Unmarshal(data, &status) == nil && status.Digest == c.Digest {
			return status.Errors
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("gateway did not apply configuration")
	return nil
}

func echoServer(t *testing.T, name string) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				reader := bufio.NewReader(conn)
				for {
					line, err := reader.ReadString('\n')
					if err != nil {
						return
					}
					if _, err = io.WriteString(conn, name+":"+line); err != nil {
						return
					}
				}
			}()
		}
	}()
	return ln.Addr().String()
}

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

type linkClient struct {
	conn   net.Conn
	reader *bufio.Reader
}

// connect dials from a source address, as a container on that network would.
func connect(from, to string, port int) (*linkClient, error) {
	dialer := net.Dialer{Timeout: 2 * time.Second, LocalAddr: &net.TCPAddr{IP: net.ParseIP(from)}}
	conn, err := dialer.Dial("tcp", net.JoinHostPort(to, strconv.Itoa(port)))
	if err != nil {
		return nil, err
	}
	return &linkClient{conn: conn, reader: bufio.NewReader(conn)}, nil
}

func (c *linkClient) ask(message string) (string, error) {
	_ = c.conn.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err := io.WriteString(c.conn, message+"\n"); err != nil {
		return "", err
	}
	line, err := c.reader.ReadString('\n')
	return strings.TrimSpace(line), err
}

func reach(from, to string, port int) (string, error) {
	c, err := connect(from, to, port)
	if err != nil {
		return "", err
	}
	defer c.conn.Close()
	return c.ask("ping")
}

func expectReach(t *testing.T, label, from, to string, port int, want string) {
	t.Helper()
	got, err := reach(from, to, port)
	if err != nil || got != want {
		t.Fatalf("%s: got %q, %v; want %q", label, got, err, want)
	}
}

func expectDenied(t *testing.T, label, from, to string, port int) {
	t.Helper()
	if got, err := reach(from, to, port); err == nil {
		t.Fatalf("%s: unexpectedly reached %q", label, got)
	}
}

func TestLinkGatewayDirectionalMatrix(t *testing.T) {
	g := startTestGateway(t)
	db, c := echoServer(t, "db"), echoServer(t, "c")
	dbPort, cPort := freePort(t), freePort(t)
	// Networks: A 127.0.1.0/30, B 127.0.1.4/30, D 127.0.1.8/30. The gateway is .1/.5/.9 and the source .2/.6/.10.
	errs := g.configure(t, gatewayConfig{Listeners: []gatewayListener{
		{Key: "source/a-db/1", ID: "a-db", Subnet: "127.0.1.0/30", Port: dbPort, Dial: db},
		{Key: "source/b-db/1", ID: "b-db", Subnet: "127.0.1.4/30", Port: dbPort, Dial: db},
		{Key: "source/b-c/1", ID: "b-c", Subnet: "127.0.1.4/30", Port: cPort, Dial: c},
		{Key: "source/d-c/1", ID: "d-c", Subnet: "127.0.1.8/30", Port: cPort, Dial: c},
	}})
	if len(errs) > 0 {
		t.Fatal(errs)
	}
	expectReach(t, "A->DB", "127.0.1.2", "127.0.1.1", dbPort, "db:ping")
	expectReach(t, "B->DB", "127.0.1.6", "127.0.1.5", dbPort, "db:ping")
	expectReach(t, "B->C", "127.0.1.6", "127.0.1.5", cPort, "c:ping")
	expectReach(t, "D->C", "127.0.1.10", "127.0.1.9", cPort, "c:ping")
	expectDenied(t, "A->C has no grant", "127.0.1.2", "127.0.1.1", cPort)
	expectDenied(t, "D->DB has no grant", "127.0.1.10", "127.0.1.9", dbPort)
	expectDenied(t, "A using B's endpoint", "127.0.1.2", "127.0.1.5", dbPort)
	expectDenied(t, "D using B's endpoint", "127.0.1.10", "127.0.1.5", dbPort)
	expectDenied(t, "C is on no source network", "127.0.1.13", "127.0.1.9", cPort)
	expectDenied(t, "wrong port", "127.0.1.2", "127.0.1.1", dbPort+1)
}

func TestLinkGatewayRevokesExistingConnections(t *testing.T) {
	g := startTestGateway(t)
	db := echoServer(t, "db")
	port := freePort(t)
	listener := gatewayListener{Key: "source/a-db/1", ID: "a-db", Subnet: "127.0.1.0/30", Port: port, Dial: db}
	other := gatewayListener{Key: "source/b-db/1", ID: "b-db", Subnet: "127.0.1.4/30", Port: port, Dial: db}
	g.configure(t, gatewayConfig{Listeners: []gatewayListener{listener, other}})
	kept, err := connect("127.0.1.6", "127.0.1.5", port)
	if err != nil {
		t.Fatal(err)
	}
	defer kept.conn.Close()
	revoked, err := connect("127.0.1.2", "127.0.1.1", port)
	if err != nil {
		t.Fatal(err)
	}
	defer revoked.conn.Close()
	if got, err := revoked.ask("before"); err != nil || got != "db:before" {
		t.Fatal(got, err)
	}
	g.configure(t, gatewayConfig{Listeners: []gatewayListener{other}})
	if got, err := revoked.ask("after"); err == nil {
		t.Fatal("revoked connection still works", got)
	}
	expectDenied(t, "new connection after revocation", "127.0.1.2", "127.0.1.1", port)
	if got, err := kept.ask("unaffected"); err != nil || got != "db:unaffected" {
		t.Fatal("unrelated connection was interrupted", got, err)
	}
	// Changing a connection's port replaces it: the old port stops listening.
	moved := other
	moved.Key, moved.Port = "source/b-db/2", freePort(t)
	g.configure(t, gatewayConfig{Listeners: []gatewayListener{moved}})
	expectDenied(t, "old port after edit", "127.0.1.6", "127.0.1.5", port)
	expectReach(t, "new port after edit", "127.0.1.6", "127.0.1.5", moved.Port, "db:ping")
}

func TestLinkGatewayFailsClosedOnInvalidConfiguration(t *testing.T) {
	g := startTestGateway(t)
	db := echoServer(t, "db")
	port := freePort(t)
	g.configure(t, gatewayConfig{Listeners: []gatewayListener{{Key: "source/a/1", ID: "a", Subnet: "127.0.1.0/30", Port: port, Dial: db}}})
	expectReach(t, "before", "127.0.1.2", "127.0.1.1", port, "db:ping")
	if err := os.WriteFile(g.ConfigPath, []byte("{not json"), 0600); err != nil {
		t.Fatal(err)
	}
	g.reload <- os.Interrupt
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := reach("127.0.1.2", "127.0.1.1", port); err != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("invalid configuration kept serving")
		}
		time.Sleep(20 * time.Millisecond)
	}
	errs := g.configure(t, gatewayConfig{Listeners: []gatewayListener{{Key: "source/bad/1", ID: "bad", Subnet: "not-a-subnet", Port: port, Dial: db}}})
	if errs["source/bad/1"] == "" {
		t.Fatal("invalid listener was not reported")
	}
}

func TestLinkGatewayCrossNode(t *testing.T) {
	source, target, intruder := startTestGateway(t), startTestGateway(t), startTestGateway(t)
	db := echoServer(t, "db")
	mesh, port := freePort(t), freePort(t)
	// The target gateway's mesh address is 127.0.2.1; its source peers must present pinned keys.
	targetConfig := gatewayConfig{MeshPort: mesh, MeshSubnet: "127.0.2.0/30", Grants: []gatewayGrant{{Key: "target/x/1", ID: "x", Peer: source.fingerprint, Dial: db}}}
	if errs := target.configure(t, targetConfig); len(errs) > 0 {
		t.Fatal(errs)
	}
	peer := &LinkPeer{Address: "127.0.2.1", Port: mesh, Fingerprint: target.fingerprint}
	sourceConfig := gatewayConfig{Listeners: []gatewayListener{
		{Key: "source/x/1", ID: "x", Subnet: "127.0.1.0/30", Port: port, Peer: peer},
		{Key: "source/y/1", ID: "y", Subnet: "127.0.1.4/30", Port: port, Peer: peer},
		{Key: "source/z/1", ID: "x", Subnet: "127.0.1.8/30", Port: port, Peer: &LinkPeer{Address: "127.0.2.1", Port: mesh, Fingerprint: intruder.fingerprint}},
	}}
	if errs := source.configure(t, sourceConfig); len(errs) > 0 {
		t.Fatal(errs)
	}
	expectReach(t, "granted cross-node connection", "127.0.1.2", "127.0.1.1", port, "db:ping")
	expectDenied(t, "connection ID without a grant", "127.0.1.6", "127.0.1.5", port)
	expectDenied(t, "target key does not match the pin", "127.0.1.10", "127.0.1.9", port)
	// Another node that copies a valid connection ID is rejected because its key is not the grant's peer.
	intruder.configure(t, gatewayConfig{Listeners: []gatewayListener{{Key: "source/x/1", ID: "x", Subnet: "127.0.1.12/30", Port: port, Peer: peer}}})
	expectDenied(t, "unpinned source node", "127.0.1.14", "127.0.1.13", port)

	open, err := connect("127.0.1.2", "127.0.1.1", port)
	if err != nil {
		t.Fatal(err)
	}
	defer open.conn.Close()
	if got, err := open.ask("live"); err != nil || got != "db:live" {
		t.Fatal(got, err)
	}
	// Revocation on the target node alone cuts existing and new connections.
	targetConfig.Grants = nil
	target.configure(t, targetConfig)
	if got, err := open.ask("after"); err == nil {
		t.Fatal("revoked cross-node connection still works", got)
	}
	expectDenied(t, "new cross-node connection after revocation", "127.0.1.2", "127.0.1.1", port)
}

func TestValidateLinkConfig(t *testing.T) {
	id, other := strings.Repeat("a", 32), strings.Repeat("b", 32)
	fingerprint := strings.Repeat("c", 64)
	valid := LinkConfig{Port: DefaultLinkPort,
		Sources: []LinkSource{{ID: id, Version: 1, Workload: "app", Service: "web", Alias: "db.internal", Port: 5432, TargetWorkload: "db", TargetService: "postgres"},
			{ID: other, Version: 2, Workload: "app", Service: "web", Alias: "cache.internal", Port: 6379, Peer: &LinkPeer{Address: "10.0.0.2", Port: DefaultLinkPort, Fingerprint: fingerprint}}},
		Targets: []LinkTarget{{ID: id, Version: 1, Workload: "db", Service: "postgres", Port: 5432}}}
	valid.Digest = LinkDigest(valid)
	if err := ValidateLinkConfig(valid); err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(*LinkConfig){
		"stale digest":          func(c *LinkConfig) { c.Sources[0].Port = 5433 },
		"local without target":  func(c *LinkConfig) { c.Targets = nil },
		"mismatched local port": func(c *LinkConfig) { c.Targets[0].Port = 5433 },
		"reserved alias":        func(c *LinkConfig) { c.Sources[0].Alias = "cove-link" },
		"uppercase alias":       func(c *LinkConfig) { c.Sources[0].Alias = "DB" },
		"bad fingerprint":       func(c *LinkConfig) { c.Sources[1].Peer.Fingerprint = "short" },
		"bad peer address":      func(c *LinkConfig) { c.Sources[1].Peer.Address = "host name" },
		"duplicate endpoint":    func(c *LinkConfig) { c.Sources[1].Alias, c.Sources[1].Port = "db.internal", 5432 },
	} {
		c := valid
		c.Sources = append([]LinkSource{}, valid.Sources...)
		c.Targets = append([]LinkTarget{}, valid.Targets...)
		peer := *valid.Sources[1].Peer
		c.Sources[1].Peer = &peer
		change(&c)
		if name != "stale digest" {
			c.Digest = LinkDigest(c)
		}
		if err := ValidateLinkConfig(c); err == nil {
			t.Fatal(name, "accepted")
		}
	}
}

func TestAddLinkNetworks(t *testing.T) {
	config := map[string]any{"services": map[string]any{
		"web": map[string]any{"networks": map[string]any{"default": nil}},
		"db":  map[string]any{"networks": map[string]any{"default": nil}},
	}}
	err := addLinkNetworks(config, map[string]map[string][]string{
		"web":     {"cove-link-s-1": nil},
		"db":      {"cove-link-t-1": {"cove-target-1"}},
		"removed": {"cove-link-s-2": nil},
	})
	if err != nil {
		t.Fatal(err)
	}
	services := config["services"].(map[string]any)
	web := services["web"].(map[string]any)["networks"].(map[string]any)
	db := services["db"].(map[string]any)["networks"].(map[string]any)
	if _, ok := web["default"]; !ok || web["cove-link-s-1"] == nil || db["cove-link-t-1"].(map[string]any)["aliases"].([]string)[0] != "cove-target-1" {
		t.Fatal("link networks were not added beside existing networks", config)
	}
	networks := config["networks"].(map[string]any)
	if networks["cove-link-s-1"].(map[string]any)["external"] != true || networks["cove-link-s-2"] != nil {
		t.Fatal("external networks wrong", networks)
	}
	if err := addLinkNetworks(map[string]any{"services": map[string]any{"web": map[string]any{"network_mode": "host"}}}, map[string]map[string][]string{"web": {"cove-link-s-1": nil}}); err == nil {
		t.Fatal("network_mode accepted")
	}
	if err := addLinkNetworks(map[string]any{"services": map[string]any{}, "networks": map[string]any{"cove-link-x": nil}}, nil); err == nil {
		t.Fatal("reserved network accepted")
	}
	renamed := map[string]any{"services": map[string]any{"web": map[string]any{"networks": map[string]any{"other": nil}}},
		"networks": map[string]any{"other": map[string]any{"external": true, "name": "cove-link-t-1"}}}
	if err := addLinkNetworks(renamed, map[string]map[string][]string{"web": {"cove-link-s-1": nil}}); err == nil {
		t.Fatal("another resource's link network was accepted under a different key")
	}
}

func TestLinkGatewayDialsTargetsOnlyOnTheirNetwork(t *testing.T) {
	g := startTestGateway(t)
	_, dbPort, _ := net.SplitHostPort(echoServer(t, "db"))
	port := freePort(t)
	byName := net.JoinHostPort("localhost", dbPort)
	errs := g.configure(t, gatewayConfig{Listeners: []gatewayListener{
		{Key: "source/a/1", ID: "a", Subnet: "127.0.1.0/30", Port: port, Dial: byName, DialSubnet: "127.0.0.0/8"},
		{Key: "source/b/1", ID: "b", Subnet: "127.0.1.4/30", Port: port, Dial: byName, DialSubnet: "10.0.0.0/30"},
	}})
	if len(errs) > 0 {
		t.Fatal(errs)
	}
	expectReach(t, "name on the target network", "127.0.1.2", "127.0.1.1", port, "db:ping")
	expectDenied(t, "name resolving outside the target network", "127.0.1.6", "127.0.1.5", port)
}

func TestReconcileLinksNeverStartsGatewayWithRevokedEntries(t *testing.T) {
	e := &Engine{Root: t.TempDir()}
	source, err := e.LinkIdentity()
	if err != nil {
		t.Fatal(err)
	}
	kept, revoked := strings.Repeat("a", 32), strings.Repeat("b", 32)
	if err = os.MkdirAll(e.linksDir("gateway"), 0700); err != nil {
		t.Fatal(err)
	}
	previous := gatewayConfig{MeshPort: DefaultLinkPort, MeshSubnet: "172.30.0.0/24", Grants: []gatewayGrant{
		{Key: "target/" + kept + "/1", ID: kept, Peer: source, Dial: "cove-target-1:5432"},
		{Key: "target/" + revoked + "/1", ID: revoked, Peer: source, Dial: "cove-target-1:6379"},
	}}
	if err = writeJSON(e.linksDir("gateway", "config.json"), previous); err != nil {
		t.Fatal(err)
	}
	var started *gatewayConfig
	// No gateway container exists, so nothing can confirm the revocation; the file must shrink before docker run.
	e.Run = func(_ context.Context, _ string, args []string, _ []string, _ io.Writer) error {
		command := strings.Join(args, " ")
		switch {
		case strings.HasPrefix(command, "docker inspect"), strings.HasPrefix(command, "docker network inspect"):
			return errors.New("not found")
		case strings.HasPrefix(command, "docker run"):
			loaded := e.readGatewayConfig()
			started = &loaded
			return errors.New("stop after start")
		case strings.HasPrefix(command, "docker kill"):
			t.Fatal("signalled a gateway that is not running")
		}
		return nil
	}
	c := LinkConfig{Port: DefaultLinkPort, Targets: []LinkTarget{{ID: kept, Version: 1, Workload: "db", Service: "postgres", Port: 5432, Peer: source}}}
	c.Digest = LinkDigest(c)
	if _, err = e.reconcileLinksLocked(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	if started == nil || len(started.Grants) != 1 || started.Grants[0].ID != kept {
		t.Fatalf("gateway started with %+v", started)
	}
}

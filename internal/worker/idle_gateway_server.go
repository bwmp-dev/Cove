package worker

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"time"
)

type idleGatewayConfig struct {
	ActivityEpoch string            `json:"activity_epoch,omitempty"`
	Revision      string            `json:"revision"`
	Listen        string            `json:"listen"`
	Peer          string            `json:"peer"`
	FrontSubnet   string            `json:"front_subnet"`
	BackendSubnet string            `json:"backend_subnet"`
	Workload      string            `json:"workload"`
	Token         string            `json:"token"`
	Mode          string            `json:"mode"`
	Episode       string            `json:"episode,omitempty"`
	Targets       map[string]string `json:"targets"`
}

type idleGatewaySaved struct {
	Revision string           `json:"revision"`
	State    idleGatewayState `json:"state"`
}

func (c idleGatewayConfig) digest() string {
	c.Revision = ""
	data, _ := json.Marshal(c)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func (c idleGatewayConfig) validate() error {
	listen, err := netip.ParseAddrPort(c.Listen)
	if err != nil || listen.Port() == 0 {
		return errors.New("invalid idle listener")
	}
	peer, err := netip.ParseAddr(c.Peer)
	if err != nil {
		return errors.New("invalid idle peer")
	}
	front, err := netip.ParsePrefix(c.FrontSubnet)
	if err != nil || !front.Contains(listen.Addr()) || !front.Contains(peer) {
		return errors.New("idle listener or peer is outside its front network")
	}
	backend, err := netip.ParsePrefix(c.BackendSubnet)
	if err != nil || front.Overlaps(backend) {
		return errors.New("idle gateway networks overlap or are invalid")
	}
	if !workloadIdentifier.MatchString(c.Workload) || len(c.Token) != 64 {
		return errors.New("invalid idle wake identity")
	}
	if _, err = hex.DecodeString(c.Token); err != nil {
		return errors.New("invalid idle wake credential")
	}
	if c.Mode != "awake" && c.Mode != "blocked" && c.Mode != "asleep" {
		return errors.New("invalid idle gateway mode")
	}
	if (c.Mode == "asleep" || c.Episode != "") && !identifier.MatchString(c.Episode) {
		return errors.New("invalid idle episode")
	}
	if c.ActivityEpoch != "" && !identifier.MatchString(c.ActivityEpoch) {
		return errors.New("invalid idle activity epoch")
	}
	if c.Revision != c.digest() {
		return errors.New("idle gateway revision does not match its inputs")
	}
	_, err = idleGatewayTargets(c.Targets, backend)
	return err
}

func loadIdleGatewayConfig(path string) (idleGatewayConfig, error) {
	var config idleGatewayConfig
	f, err := os.Open(path)
	if err != nil {
		return config, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, 1024*1024+1))
	if err != nil {
		return config, err
	}
	if len(data) > 1024*1024 {
		return config, errors.New("idle gateway configuration is too large")
	}
	if err = json.Unmarshal(data, &config); err != nil {
		return config, errors.New("invalid idle gateway configuration")
	}
	return config, config.validate()
}

func idleWake(ctx context.Context, config idleGatewayConfig, socket, episode string) error {
	data, err := json.Marshal(map[string]string{"workload": config.Workload, "episode": episode})
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, "POST", "http://worker/wake", bytes.NewReader(data))
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+config.Token)
	request.Header.Set("Content-Type", "application/json")
	transport := &http.Transport{Proxy: nil, DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "unix", socket)
	}}
	defer transport.CloseIdleConnections()
	response, err := (&http.Client{Transport: transport, Timeout: 3 * time.Minute}).Do(request)
	if err != nil {
		return errors.New("local wake service is unavailable")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		return errors.New("local wake service did not confirm readiness")
	}
	return nil
}

func RunIdleGateway(ctx context.Context) error {
	return serveIdleGateway(ctx, "/config/config.json", "/state/state.json", "/state/control.sock", "/wake/worker.sock")
}

func serveIdleGateway(ctx context.Context, configPath, statePath, controlPath, wakePath string) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	config, err := loadIdleGatewayConfig(configPath)
	if err != nil {
		return err
	}
	current := config
	persist := func(state idleGatewayState) error {
		return writeJSON(statePath, idleGatewaySaved{Revision: current.Revision, State: state})
	}
	var gateway *idleGateway
	wake := func(ctx context.Context, episode string) error {
		gateway.mu.Lock()
		config := current
		gateway.mu.Unlock()
		return idleWake(ctx, config, wakePath, episode)
	}
	gateway, err = newIdleGateway(ctx, netip.MustParseAddr(config.Peer), config.Targets, netip.MustParsePrefix(config.BackendSubnet), wake, persist)
	if err != nil {
		return err
	}
	state := idleGatewayState{Mode: config.Mode, Episode: config.Episode, LastActivity: time.Now().UTC()}
	if data, readErr := os.ReadFile(statePath); readErr == nil {
		var saved idleGatewaySaved
		if err = json.Unmarshal(data, &saved); err != nil {
			return errors.New("idle gateway state is unreadable")
		}
		if saved.Revision == config.Revision {
			if saved.State.Mode != "awake" && saved.State.Mode != "asleep" && saved.State.Mode != "quiescing" && saved.State.Mode != "blocked" {
				return errors.New("invalid saved idle mode")
			}
			if (saved.State.Mode == "asleep" || saved.State.Mode == "quiescing") && !identifier.MatchString(saved.State.Episode) {
				return errors.New("invalid saved idle episode")
			}
			state = saved.State
			state.Active = 0
			if state.Mode == "awake" {
				state.LastActivity = time.Now().UTC()
			}
		}
	} else if !os.IsNotExist(readErr) {
		return readErr
	}
	if err = persist(state); err != nil {
		return err
	}
	gateway.state = state
	listener, err := net.Listen("tcp", config.Listen)
	if err != nil {
		return err
	}
	defer listener.Close()
	if err = os.MkdirAll(filepath.Dir(controlPath), 0700); err != nil {
		return err
	}
	if _, err := os.Lstat(controlPath); err == nil {
		if err = os.Remove(controlPath); err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	control, err := net.Listen("unix", controlPath)
	if err != nil {
		return err
	}
	defer control.Close()
	if err = os.Chmod(controlPath, 0600); err != nil {
		return err
	}
	controlHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" && r.URL.Path == "/status" {
			w.Header().Set("Content-Type", "application/json")
			gateway.mu.Lock()
			status := struct {
				idleGatewayState
				Revision string `json:"revision"`
			}{gateway.state, current.Revision}
			gateway.mu.Unlock()
			json.NewEncoder(w).Encode(status)
			return
		}
		if r.Method != "POST" {
			http.NotFound(w, r)
			return
		}
		switch r.URL.Path {
		case "/quiesce":
			var input struct {
				Seconds int    `json:"seconds"`
				Episode string `json:"episode"`
			}
			decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
			decoder.DisallowUnknownFields()
			if decoder.Decode(&input) != nil || input.Seconds < 1 || input.Seconds > 86400 {
				http.Error(w, "Invalid quiesce request", 400)
				return
			}
			ready, err := gateway.quiesce(time.Duration(input.Seconds)*time.Second, input.Episode)
			if err != nil {
				http.Error(w, "Could not persist quiesce state", 500)
				return
			}
			if !ready {
				http.Error(w, "Resource is active or already held", 409)
				return
			}
			w.WriteHeader(204)
		case "/reload":
			var input struct {
				Revision string `json:"revision"`
			}
			decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
			decoder.DisallowUnknownFields()
			if decoder.Decode(&input) != nil {
				http.Error(w, "Configuration revision required", 400)
				return
			}
			next, err := loadIdleGatewayConfig(configPath)
			if err != nil || next.Revision != input.Revision || next.Listen != config.Listen || next.Workload != config.Workload {
				http.Error(w, "Invalid idle gateway configuration", 409)
				return
			}
			targets, err := idleGatewayTargets(next.Targets, netip.MustParsePrefix(next.BackendSubnet))
			if err != nil {
				http.Error(w, "Invalid idle gateway targets", 409)
				return
			}
			gateway.mu.Lock()
			state := gateway.state
			state.Mode, state.Episode = next.Mode, next.Episode
			if state.Mode == "awake" && (gateway.state.Mode != "awake" || next.Revision != current.Revision) {
				state.LastActivity = time.Now().UTC()
			}
			err = writeJSON(statePath, idleGatewaySaved{Revision: next.Revision, State: state})
			if err == nil {
				current = next
				gateway.state = state
				gateway.peer = netip.MustParseAddr(next.Peer).Unmap()
				gateway.backend = netip.MustParsePrefix(next.BackendSubnet)
				gateway.targets = targets
				close(gateway.changed)
				gateway.changed = make(chan struct{})
			}
			gateway.mu.Unlock()
			if err != nil {
				http.Error(w, "Could not persist idle gateway configuration", 500)
				return
			}
			w.WriteHeader(204)
		default:
			http.NotFound(w, r)
		}
	})
	public := &http.Server{Handler: gateway, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 90 * time.Second, MaxHeaderBytes: 64 * 1024}
	private := &http.Server{Handler: controlHandler, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 10 * time.Second, IdleTimeout: 30 * time.Second}
	finished := make(chan error, 2)
	go func() { finished <- public.Serve(listener) }()
	go func() { finished <- private.Serve(control) }()
	var result error
	select {
	case <-ctx.Done():
	case result = <-finished:
	}
	cancel()
	public.Close()
	private.Close()
	if result != nil && result != http.ErrServerClosed {
		return result
	}
	return nil
}

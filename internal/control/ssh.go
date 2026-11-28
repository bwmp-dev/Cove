package control

import (
	"bytes"
	"context"
	"cove/internal/worker"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"golang.org/x/crypto/ssh"
	"io"
	"net"
	"slices"
	"strconv"
)

type cappedWriter struct{ data []byte }
type RemoteError struct{ Message string }

func (e *RemoteError) Error() string { return e.Message }

func (w *cappedWriter) Write(b []byte) (int, error) {
	if len(w.data)+len(b) > 2*1024*1024 {
		return 0, errors.New("worker response too large")
	}
	w.data = append(w.data, b...)
	return len(b), nil
}

func callSSH(ctx context.Context, address string, port int, hostKey string, seed []byte, request worker.RPCRequest) (worker.RPCResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, worker.RPCTimeout(request.Method))
	defer cancel()
	client, err := openSSH(ctx, address, port, hostKey, seed)
	if err != nil {
		return worker.RPCResponse{}, err
	}
	defer client.Close()
	return callSSHClient(client, request)
}

func callSSHClient(client *ssh.Client, request worker.RPCRequest) (worker.RPCResponse, error) {
	var p worker.RPCResponse
	session, err := client.NewSession()
	if err != nil {
		return p, err
	}
	defer session.Close()
	out := &cappedWriter{}
	session.Stdout = out
	session.Stderr = io.Discard
	payload, err := json.Marshal(request)
	if err != nil {
		return p, err
	}
	session.Stdin = bytes.NewReader(payload)
	if err = session.Run("rpc"); err != nil {
		return p, err
	}
	if err = json.Unmarshal(out.data, &p); err != nil {
		return p, err
	}
	if p.Error != "" {
		return p, &RemoteError{Message: p.Error}
	}
	return p, nil
}

func probeSSH(ctx context.Context, address string, port int, hostKey string, seed []byte) (worker.Probe, error) {
	response, err := callSSH(ctx, address, port, hostKey, seed, worker.RPCRequest{Method: "probe"})
	if err != nil {
		return worker.Probe{}, err
	}
	if response.Probe == nil || response.Probe.Protocol != 1 {
		return worker.Probe{}, fmt.Errorf("unsupported worker protocol")
	}
	return *response.Probe, nil
}

func openSSH(ctx context.Context, address string, port int, hostKey string, seed []byte) (*ssh.Client, error) {
	return openSSHUser(ctx, address, port, "cove", hostKey, seed, nil)
}

type HostKeyChanged struct {
	Hop      string
	Expected string
	Observed string
}

func (e *HostKeyChanged) Error() string { return "pinned SSH host key changed" }

func openSSHUser(ctx context.Context, address string, port int, user, hostKey string, seed []byte, forward func(context.Context, string, string) (net.Conn, error)) (*ssh.Client, error) {
	if len(seed) != ed25519.SeedSize {
		return nil, errors.New("invalid identity")
	}
	signer, err := ssh.NewSignerFromKey(ed25519.NewKeyFromSeed(seed))
	if err != nil {
		return nil, err
	}
	host, _, _, _, err := ssh.ParseAuthorizedKey([]byte(hostKey))
	if err != nil {
		return nil, err
	}
	endpoint := net.JoinHostPort(address, strconv.Itoa(port))
	dial := (&net.Dialer{}).DialContext
	if forward != nil {
		dial = forward
	}
	conn, err := dial(ctx, "tcp", endpoint)
	if err != nil {
		return nil, err
	}
	accepted := false
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer func() {
		if !accepted {
			stop()
			conn.Close()
		}
	}()
	deadline, _ := ctx.Deadline()
	if forward == nil {
		if err = conn.SetDeadline(deadline); err != nil {
			return nil, err
		}
	}
	algorithms := []string{host.Type()}
	if host.Type() == ssh.KeyAlgoRSA {
		algorithms = []string{ssh.KeyAlgoRSASHA512, ssh.KeyAlgoRSASHA256}
	}
	// A changed key type must reach the pin check rather than disappear into negotiation failure.
	for _, algorithm := range ssh.SupportedAlgorithms().HostKeys {
		if !slices.Contains(algorithms, algorithm) {
			algorithms = append(algorithms, algorithm)
		}
	}
	verify := func(_ string, _ net.Addr, observed ssh.PublicKey) error {
		if !bytes.Equal(host.Marshal(), observed.Marshal()) {
			return &HostKeyChanged{Expected: ssh.FingerprintSHA256(host), Observed: ssh.FingerprintSHA256(observed)}
		}
		return nil
	}
	c, ch, req, err := ssh.NewClientConn(conn, endpoint, &ssh.ClientConfig{User: user, Auth: []ssh.AuthMethod{ssh.PublicKeys(signer)}, HostKeyCallback: verify, HostKeyAlgorithms: algorithms})
	if err != nil {
		return nil, err
	}
	client := ssh.NewClient(c, ch, req)
	accepted = true
	return client, nil
}

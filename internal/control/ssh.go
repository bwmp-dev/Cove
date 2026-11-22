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
	var p worker.RPCResponse
	ctx, cancel := context.WithTimeout(ctx, worker.RPCTimeout(request.Method))
	defer cancel()
	client, err := openSSH(ctx, address, port, hostKey, seed)
	if err != nil {
		return p, err
	}
	defer client.Close()
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
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", endpoint)
	if err != nil {
		return nil, err
	}
	accepted := false
	defer func() {
		if !accepted {
			conn.Close()
		}
	}()
	deadline, _ := ctx.Deadline()
	if err = conn.SetDeadline(deadline); err != nil {
		return nil, err
	}
	algorithms := []string{host.Type()}
	if host.Type() == ssh.KeyAlgoRSA {
		algorithms = []string{ssh.KeyAlgoRSASHA512, ssh.KeyAlgoRSASHA256}
	}
	c, ch, req, err := ssh.NewClientConn(conn, endpoint, &ssh.ClientConfig{User: "cove", Auth: []ssh.AuthMethod{ssh.PublicKeys(signer)}, HostKeyCallback: ssh.FixedHostKey(host), HostKeyAlgorithms: algorithms})
	if err != nil {
		return nil, err
	}
	client := ssh.NewClient(c, ch, req)
	accepted = true
	context.AfterFunc(ctx, func() { client.Close() })
	return client, nil
}

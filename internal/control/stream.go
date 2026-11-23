package control

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"time"

	"cove/internal/worker"
)

func (s *Server) nodeStream(ctx context.Context, node string, request worker.RPCRequest, input io.Reader, consume func(worker.StreamResponse, io.Reader) error) error {
	proxy, err := s.outboundConfig(ctx)
	if err != nil {
		return err
	}
	if s.propagateOutbound(proxy) {
		request.Proxy = &proxy
		if request.Stream != nil && request.Stream.Request != nil {
			request.Stream.Request.OutboundProxy = &proxy
		}
	}
	if s.workerStream != nil {
		return s.workerStream(ctx, node, request, input, consume)
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	payload, err := json.Marshal(request)
	if err != nil || len(payload) > 2<<20 {
		return errors.New("invalid stream request")
	}
	if input == nil {
		input = bytes.NewReader(nil)
	}
	if closer, ok := input.(io.Closer); ok {
		stop := context.AfterFunc(ctx, func() { closer.Close() })
		defer stop()
	}
	body := io.MultiReader(bytes.NewReader(append(payload, '\n')), input)
	var address, mode string
	var proxySupported bool
	var host *string
	var port int
	var encrypted []byte
	if err = s.DB.QueryRow(ctx, "SELECT address,port,host_key,private_key,mode,COALESCE((capabilities->>'outbound_proxy')::boolean,false) FROM nodes WHERE id=$1", node).Scan(&address, &port, &host, &encrypted, &mode, &proxySupported); err != nil {
		return err
	}
	if request.Proxy != nil && !proxySupported {
		return errOutboundWorkerUnsupported
	}
	if mode == "local" {
		if s.LocalSocket == "" {
			return errors.New("local worker is not configured")
		}
		reader, writer := io.Pipe()
		finished := make(chan error, 1)
		go func() {
			err := worker.Proxy(ctx, s.LocalSocket, body, writer)
			writer.CloseWithError(err)
			finished <- err
		}()
		response, stream, err := worker.ReadStreamResponse(reader)
		if err == nil {
			err = consume(response, stream)
		}
		reader.Close()
		if err != nil {
			cancel()
		}
		return errors.Join(err, <-finished)
	}
	if host == nil {
		return errors.New("worker is not enrolled")
	}
	seed, err := unseal(s.Key, encrypted)
	if err != nil {
		return err
	}
	client, err := openSSH(ctx, address, port, *host, seed)
	if err != nil {
		return err
	}
	defer client.Close()
	session, err := client.NewSession()
	if err != nil {
		return err
	}
	defer session.Close()
	session.Stdin, session.Stderr = body, io.Discard
	out, err := session.StdoutPipe()
	if err != nil {
		return err
	}
	if err = session.Start("rpc"); err != nil {
		return err
	}
	response, stream, err := worker.ReadStreamResponse(out)
	if err == nil {
		err = consume(response, stream)
	}
	if err != nil {
		cancel()
		session.Close()
	}
	return errors.Join(err, session.Wait())
}

package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	if len(os.Args) != 5 {
		return fmt.Errorf("usage: ingress-client URL UDP-address root-file expected-status")
	}
	root, err := os.ReadFile(os.Args[3])
	if err != nil {
		return err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(root) {
		return fmt.Errorf("fixture root could not be loaded")
	}
	expected, err := strconv.Atoi(os.Args[4])
	if err != nil {
		return err
	}
	transport := &http3.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS13}, Dial: func(ctx context.Context, _ string, tlsConfig *tls.Config, config *quic.Config) (*quic.Conn, error) {
		return quic.DialAddrEarly(ctx, os.Args[2], tlsConfig, config)
	}}
	defer transport.Close()
	client := &http.Client{Transport: transport, Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Get(os.Args[1])
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.ProtoMajor != 3 || response.StatusCode != expected {
		return fmt.Errorf("unexpected response: %s %d", response.Proto, response.StatusCode)
	}
	if _, err := io.Copy(io.Discard, io.LimitReader(response.Body, 1<<20)); err != nil {
		return err
	}
	fmt.Println(response.Proto, response.StatusCode)
	return nil
}

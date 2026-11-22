package client

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

type Client struct {
	URL, Token, Team string
	HTTP             *http.Client
}
type Error struct {
	Status  int
	Message string
}

func (e *Error) Error() string { return fmt.Sprintf("Cove API returned %d: %s", e.Status, e.Message) }
func New(address, token, team string) (*Client, error) {
	u, err := url.Parse(address)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.Path != "" && u.Path != "/" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || token == "" || strings.ContainsAny(token+team, "\r\n\x00") {
		return nil, errors.New("an HTTPS panel origin, API token and valid team are required")
	}
	httpClient := &http.Client{Timeout: 45 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	if file := os.Getenv("COVE_CA_FILE"); file != "" {
		raw, err := os.ReadFile(file)
		if err != nil || len(raw) > 1024*1024 {
			return nil, errors.New("could not read COVE_CA_FILE")
		}
		roots, err := x509.SystemCertPool()
		if err != nil {
			return nil, errors.New("could not load system trust roots")
		}
		if !roots.AppendCertsFromPEM(raw) {
			return nil, errors.New("COVE_CA_FILE contains no trusted PEM certificates")
		}
		transport := http.DefaultTransport.(*http.Transport).Clone()
		transport.TLSClientConfig = &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}
		httpClient.Transport = transport
	}
	return &Client{URL: strings.TrimRight(address, "/"), Token: token, Team: team, HTTP: httpClient}, nil
}
func (c *Client) Do(ctx context.Context, method, path string, in, out any) error {
	if !strings.HasPrefix(path, "/") || strings.HasPrefix(path, "//") || strings.ContainsAny(path, "\r\n\x00") {
		return errors.New("invalid API path")
	}
	var body io.Reader
	if in != nil {
		raw, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.URL+"/api/v1"+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("X-Cove-Team", c.Team)
	req.Header.Set("Accept", "application/json")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	response, err := c.HTTP.Do(req)
	if err != nil {
		return errors.New("could not reach Cove; check the panel URL, network and trusted TLS certificate")
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, 4*1024*1024+1))
	if err != nil || len(raw) > 4*1024*1024 {
		return errors.New("Cove response exceeds limit or could not be read")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		message := strings.TrimSpace(string(raw))
		if len(message) > 1024 {
			message = "request failed"
		}
		return &Error{Status: response.StatusCode, Message: strings.ReplaceAll(message, c.Token, "[redacted]")}
	}
	if out != nil && json.Unmarshal(raw, out) != nil {
		return errors.New("Cove returned an invalid JSON response")
	}
	return nil
}

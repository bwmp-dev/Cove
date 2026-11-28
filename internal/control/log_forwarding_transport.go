package control

import (
	"bytes"
	"context"
	"cove/internal/outbound"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

type logForwarder struct {
	Name     string `json:"name"`
	Kind     string `json:"kind"`
	Endpoint string `json:"endpoint"`
	Token    string `json:"token"`
	Username string `json:"username"`
	Enabled  bool   `json:"enabled"`
}
type forwardedLog struct {
	Workload  string    `json:"workload"`
	Container string    `json:"container"`
	Timestamp time.Time `json:"timestamp"`
	Message   string    `json:"message"`
}

func validLogForwarder(c logForwarder) bool {
	if len(c.Name) < 1 || len(c.Name) > 120 || len(c.Endpoint) > 4096 || len(c.Token) > 4096 || len(c.Username) > 256 || strings.ContainsAny(c.Endpoint+c.Token+c.Username, "\r\n") {
		return false
	}
	u, err := url.Parse(c.Endpoint)
	if err != nil || u.Hostname() == "" || u.User != nil || u.Fragment != "" {
		return false
	}
	if c.Kind == "syslog" {
		port, err := strconv.Atoi(u.Port())
		return err == nil && port > 0 && port <= 65535 && u.Scheme == "tls" && u.Path == "" && u.RawQuery == "" && c.Token == "" && c.Username == ""
	}
	if strings.Contains(c.Username, ":") || (c.Kind == "axiom" || c.Kind == "datadog") && c.Username != "" {
		return false
	}
	return u.Scheme == "https" && (c.Kind == "loki" || c.Kind == "otlp" || c.Kind == "http" || c.Kind == "axiom" || c.Kind == "datadog")
}

func forwardPayload(c logForwarder, logs []forwardedLog) (any, http.Header) {
	headers := http.Header{"Content-Type": []string{"application/json"}, "User-Agent": []string{"Cove"}}
	if c.Token != "" {
		headers.Set("Authorization", "Bearer "+c.Token)
	}
	sort.SliceStable(logs, func(i, j int) bool { return logs[i].Timestamp.Before(logs[j].Timestamp) })
	switch c.Kind {
	case "loki":
		type stream struct {
			Stream map[string]string `json:"stream"`
			Values [][2]string       `json:"values"`
		}
		streams := []stream{}
		indexes := map[string]int{}
		for _, entry := range logs {
			key := entry.Workload + "\x00" + entry.Container
			index, ok := indexes[key]
			if !ok {
				index = len(streams)
				indexes[key] = index
				streams = append(streams, stream{Stream: map[string]string{"job": "cove", "workload": entry.Workload, "container": entry.Container}, Values: [][2]string{}})
			}
			streams[index].Values = append(streams[index].Values, [2]string{strconv.FormatInt(entry.Timestamp.UnixNano(), 10), entry.Message})
		}
		return map[string]any{"streams": streams}, headers
	case "otlp":
		attr := func(key, value string) any {
			return map[string]any{"key": key, "value": map[string]string{"stringValue": value}}
		}
		records := []any{}
		for _, entry := range logs {
			records = append(records, map[string]any{"timeUnixNano": strconv.FormatInt(entry.Timestamp.UnixNano(), 10), "body": map[string]string{"stringValue": entry.Message}, "attributes": []any{attr("cove.workload", entry.Workload), attr("container.name", entry.Container)}})
		}
		return map[string]any{"resourceLogs": []any{map[string]any{"resource": map[string]any{"attributes": []any{attr("service.name", "cove")}}, "scopeLogs": []any{map[string]any{"scope": map[string]string{"name": "cove"}, "logRecords": records}}}}}, headers
	case "datadog":
		headers.Del("Authorization")
		if c.Token != "" {
			headers.Set("DD-API-KEY", c.Token)
		}
		records := []any{}
		for _, entry := range logs {
			records = append(records, map[string]any{"message": entry.Message, "service": entry.Workload, "ddsource": "cove", "container_name": entry.Container, "timestamp": entry.Timestamp.UnixMilli()})
		}
		return records, headers
	case "axiom":
		return logs, headers
	default:
		return map[string]any{"logs": logs}, headers
	}
}

func syslogFrame(entry forwardedLog) []byte {
	data, _ := json.Marshal(map[string]string{"workload": entry.Workload, "container": entry.Container, "message": entry.Message})
	message := fmt.Sprintf("<134>1 %s - cove - - - %s", entry.Timestamp.UTC().Format(time.RFC3339Nano), data)
	return []byte(strconv.Itoa(len([]byte(message))) + " " + message)
}

func sendSyslogBatch(ctx context.Context, c logForwarder, logs []forwardedLog, proxy outbound.Config) error {
	u, err := url.Parse(c.Endpoint)
	if err != nil {
		return errors.New("invalid syslog endpoint")
	}
	ips, err := net.DefaultResolver.LookupIPAddr(ctx, u.Hostname())
	if err != nil || len(ips) == 0 {
		return errors.New("syslog host unavailable")
	}
	for _, ip := range ips {
		if !publicWebhookIP(ip.IP) {
			return errors.New("syslog must resolve to public addresses")
		}
	}
	endpoint, err := proxy.For("https", u.Host)
	if err != nil {
		return errors.New("syslog proxy settings unavailable")
	}
	raw, err := outbound.Tunnel(ctx, endpoint, "tcp", net.JoinHostPort(ips[0].IP.String(), u.Port()))
	if err != nil {
		return errors.New("syslog TLS connection failed")
	}
	connection := tls.Client(raw, &tls.Config{MinVersion: tls.VersionTLS12, ServerName: u.Hostname()})
	defer connection.Close()
	err = connection.HandshakeContext(ctx)
	if err != nil {
		return errors.New("syslog TLS connection failed")
	}
	defer connection.Close()
	if err = connection.SetWriteDeadline(time.Now().Add(5 * time.Second)); err != nil {
		return errors.New("syslog write deadline failed")
	}
	for _, entry := range logs {
		data := syslogFrame(entry)
		for len(data) > 0 {
			n, err := connection.Write(data)
			if err != nil {
				return errors.New("syslog delivery failed")
			}
			if n == 0 {
				return errors.New("syslog delivery stalled")
			}
			data = data[n:]
		}
	}
	return nil
}

func (s *Server) sendLogBatch(ctx context.Context, c logForwarder, logs []forwardedLog) error {
	if !validLogForwarder(c) || len(logs) > 100 {
		return errors.New("invalid log forwarding request")
	}
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	proxy, err := s.outboundConfig(ctx)
	if err != nil {
		return errors.New("outbound configuration unavailable")
	}
	if c.Kind == "syslog" {
		return sendSyslogBatch(ctx, c, logs, proxy)
	}
	payload, headers := forwardPayload(c, logs)
	data, err := json.Marshal(payload)
	if err != nil || len(data) > 1024*1024 {
		return errors.New("forwarded batch exceeds limits")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.Endpoint, bytes.NewReader(data))
	if err != nil {
		return errors.New("invalid forwarding endpoint")
	}
	req.Header = headers
	sum := sha256.Sum256(data)
	req.Header.Set("X-Cove-Delivery", hex.EncodeToString(sum[:]))
	if c.Username != "" {
		req.SetBasicAuth(c.Username, c.Token)
	}
	transport := publicWebhookTransport(proxy)
	defer transport.CloseIdleConnections()
	var rt http.RoundTripper = transport
	if s.NotificationTransport != nil {
		rt = s.NotificationTransport
	}
	client := &http.Client{Transport: rt, Timeout: 8 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(req)
	if err != nil {
		return errors.New("log forwarding request failed")
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 || response.StatusCode == 260 {
		return errors.New("log receiver rejected the batch")
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, 8193))
	if err != nil || len(body) > 8192 {
		return errors.New("log receiver response unavailable")
	}
	if c.Kind == "otlp" && len(bytes.TrimSpace(body)) > 0 {
		var result struct {
			PartialSuccess struct {
				RejectedLogRecords json.Number `json:"rejectedLogRecords"`
			} `json:"partialSuccess"`
		}
		if json.Unmarshal(body, &result) != nil {
			return errors.New("invalid OTLP response")
		}
		if result.PartialSuccess.RejectedLogRecords != "" && result.PartialSuccess.RejectedLogRecords != "0" {
			return errors.New("OTLP receiver rejected log records")
		}
	}
	return nil
}

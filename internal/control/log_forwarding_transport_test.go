package control

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestLogForwardingPayloadsAndFraming(t *testing.T) {
	stamp := time.Unix(1700000000, 123).UTC()
	logs := []forwardedLog{{Workload: "resource", Container: "app", Timestamp: stamp, Message: "hello\nworld"}}
	for _, kind := range []string{"loki", "otlp", "http", "axiom", "datadog"} {
		c := logForwarder{Name: "Logs", Kind: kind, Endpoint: "https://example.com/ingest", Token: "sealed-token"}
		if !validLogForwarder(c) {
			t.Fatal("valid receiver rejected", kind)
		}
		payload, headers := forwardPayload(c, logs)
		data, err := json.Marshal(payload)
		if err != nil || strings.Contains(string(data), "sealed-token") || !strings.Contains(string(data), "hello") {
			t.Fatal(kind, string(data), err)
		}
		if kind == "datadog" {
			if headers.Get("DD-API-KEY") != "sealed-token" || headers.Get("Authorization") != "" {
				t.Fatal(headers)
			}
		} else if headers.Get("Authorization") != "Bearer sealed-token" {
			t.Fatal(headers)
		}
		if kind == "loki" && !strings.Contains(string(data), `"1700000000000000123"`) {
			t.Fatal("Loki timestamp is not nanosecond string", string(data))
		}
		if kind == "otlp" && (!strings.Contains(string(data), `"resourceLogs"`) || !strings.Contains(string(data), `"timeUnixNano":"1700000000000000123"`)) {
			t.Fatal("invalid OTLP shape", string(data))
		}
	}
	frame := string(syslogFrame(logs[0]))
	prefix, body, ok := strings.Cut(frame, " ")
	size, err := strconv.Atoi(prefix)
	if !ok || err != nil || size != len([]byte(body)) || !strings.HasPrefix(body, "<134>1 ") || strings.Contains(body, "hello\nworld") {
		t.Fatal("invalid octet-counted syslog frame", frame)
	}
	for _, endpoint := range []string{"http://example.com", "https://user:secret@example.com", "tls://example.com:99999", "tls://example.com:6514/path"} {
		kind := "http"
		if strings.HasPrefix(endpoint, "tls") {
			kind = "syslog"
		}
		if validLogForwarder(logForwarder{Name: "Bad", Kind: kind, Endpoint: endpoint}) {
			t.Fatal("unsafe endpoint accepted", endpoint)
		}
	}
}

func TestLogForwardingDeliveryAndOTLPRejections(t *testing.T) {
	s := &Server{}
	requests := 0
	s.NotificationTransport = notificationRoundTrip(func(r *http.Request) (*http.Response, error) {
		requests++
		if r.Header.Get("X-Cove-Delivery") == "" || r.Header.Get("Authorization") != "Bearer token" {
			t.Fatal("missing delivery/auth headers")
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"partialSuccess":{"rejectedLogRecords":"1"}}`)), Header: http.Header{}}, nil
	})
	c := logForwarder{Name: "Logs", Kind: "otlp", Endpoint: "https://example.com/v1/logs", Token: "token"}
	if err := s.sendLogBatch(context.Background(), c, []forwardedLog{{Timestamp: time.Now(), Message: "redacted"}}); err == nil || requests != 1 {
		t.Fatal("OTLP partial rejection lost", err)
	}
}

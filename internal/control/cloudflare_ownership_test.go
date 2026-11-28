package control

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestCloudflareAddressOwnershipProof(t *testing.T) {
	s := &Server{Key: bytes.Repeat([]byte{1}, 32)}
	zone := cloudflareZone{ZoneID: strings.Repeat("a", 32), Domain: "example.test"}
	record := cloudflareRecord{ID: strings.Repeat("b", 32), Type: "A", Name: "app.example.test", Content: "8.8.8.8", TTL: 1}
	record.Comment = s.dnsOwnershipComment(zone, record)
	if len(record.Comment) != 73 || !strings.HasPrefix(record.Comment, dnsOwnershipPrefix) || strings.Contains(record.Comment, "example") || !s.ownsDNSRecord(zone, record) {
		t.Fatal("missing or identifying ownership marker", len(record.Comment))
	}
	record.Settings = map[string]json.RawMessage{"ipv4_only": json.RawMessage("false"), "ipv6_only": json.RawMessage("false")}
	if !s.ownsDNSRecord(zone, record) {
		t.Fatal("provider default address settings lost ownership")
	}
	v6 := cloudflareRecord{Type: "AAAA", Name: "v6.example.test", Content: "2001:db8::1", TTL: 1}
	v6.Comment = s.dnsOwnershipComment(zone, v6)
	if !s.ownsDNSRecord(zone, v6) {
		t.Fatal("IPv6 address proof was not supported")
	}
	for _, change := range []func(*cloudflareRecord){
		func(r *cloudflareRecord) { r.Comment = "Operator note" },
		func(r *cloudflareRecord) { r.Content = "8.8.4.4" },
		func(r *cloudflareRecord) { r.Name = "other.example.test" },
		func(r *cloudflareRecord) { r.Type = "AAAA" },
		func(r *cloudflareRecord) { r.Proxied = true },
		func(r *cloudflareRecord) { r.TTL = 300 },
		func(r *cloudflareRecord) { r.Tags = []string{"operator:keep"} },
		func(r *cloudflareRecord) {
			r.Settings = map[string]json.RawMessage{"operator_setting": json.RawMessage("false")}
		},
		func(r *cloudflareRecord) {
			r.Settings = map[string]json.RawMessage{"ipv4_only": json.RawMessage("true")}
		},
	} {
		changed := record
		change(&changed)
		if s.ownsDNSRecord(zone, changed) {
			t.Fatal("operator-changed record retained deletion authority")
		}
	}
	zone.ZoneID = strings.Repeat("c", 32)
	if s.ownsDNSRecord(zone, record) {
		t.Fatal("proof crossed zone boundary")
	}
	zone.ZoneID = strings.Repeat("a", 32)
	s.Key = bytes.Repeat([]byte{2}, 32)
	if s.ownsDNSRecord(zone, record) {
		t.Fatal("old-key record retained deletion authority")
	}
}

func TestCloudflareUnmarkedAddressChangeKeepsMainBehavior(t *testing.T) {
	// f53333c reconciles the sole existing address in place and preserves its
	// proxy choice. Ownership must not change that pre-feature contract.
	patches := 0
	s := &Server{Key: bytes.Repeat([]byte{1}, 32)}
	s.CloudflareTransport = dnsTransport(func(r *http.Request) (*http.Response, error) {
		var result any = []map[string]any{{"id": strings.Repeat("b", 32), "type": "A", "name": "app.example.test", "content": "8.8.4.4", "proxied": true, "ttl": 1}}
		if r.Method != "GET" {
			patches++
			if r.Method != "PATCH" || !strings.HasSuffix(r.URL.Path, "/"+strings.Repeat("b", 32)) {
				t.Fatal("legacy address was replaced or deleted", r.Method)
			}
			var body map[string]any
			if json.NewDecoder(r.Body).Decode(&body) != nil || body["content"] != "8.8.8.8" || body["proxied"] != true || body["ttl"] != float64(1) {
				t.Fatal("legacy reconciliation changed", body)
			}
			if _, stamped := body["comment"]; stamped {
				t.Fatal("adopted an unmarked existing record")
			}
			result = map[string]any{}
		}
		data, _ := json.Marshal(map[string]any{"success": true, "result": result})
		return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(data))}, nil
	})
	err := s.reconcileDNSRecord(t.Context(), cloudflareZone{ZoneID: strings.Repeat("a", 32), Domain: "example.test", PreserveProxy: true}, "app.example.test", "8.8.8.8")
	if err != nil || patches != 1 {
		t.Fatal("legacy unmarked address reconciliation failed", patches, err)
	}
}

func TestCloudflareOwnershipCommentRefusalAndLostCreate(t *testing.T) {
	for _, outcome := range []string{"comments-refused", "comments-forbidden", "comments-unsupported", "lost-reply", "rate-limited", "server-failed"} {
		t.Run(outcome, func(t *testing.T) {
			s := &Server{Key: bytes.Repeat([]byte{1}, 32)}
			posts := 0
			s.CloudflareTransport = dnsTransport(func(r *http.Request) (*http.Response, error) {
				status := 200
				var result any = []cloudflareRecord{}
				if r.Method == "POST" {
					posts++
					var body map[string]any
					if json.NewDecoder(r.Body).Decode(&body) != nil {
						t.Fatal("unreadable create")
					}
					if posts == 1 {
						if comment, _ := body["comment"].(string); len(comment) != 73 {
							t.Fatal("new address lacks bounded opaque marker")
						}
						switch outcome {
						case "comments-refused":
							status = 400
						case "comments-forbidden":
							status = 403
						case "comments-unsupported":
							status = 422
						case "lost-reply":
							return nil, errors.New("injected lost create acknowledgement")
						case "rate-limited":
							status = 429
						case "server-failed":
							status = 503
						}
					} else if _, stamped := body["comment"]; stamped {
						t.Fatal("comment fallback still sent the unsupported field")
					}
					result = map[string]any{}
				}
				data, _ := json.Marshal(map[string]any{"success": status == 200, "result": result})
				return &http.Response{StatusCode: status, Body: io.NopCloser(bytes.NewReader(data))}, nil
			})
			err := s.reconcileDNSRecord(context.Background(), cloudflareZone{ZoneID: strings.Repeat("a", 32), Domain: "example.test"}, "app.example.test", "8.8.8.8")
			if strings.HasPrefix(outcome, "comments-") {
				if err != nil || posts != 2 {
					t.Fatal("comment refusal prevented an unowned create", posts, err)
				}
			} else if err == nil || posts != 1 {
				t.Fatal("uncertain or retryable create was duplicated", posts, err)
			}
		})
	}
}

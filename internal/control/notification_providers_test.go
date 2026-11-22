package control

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestIncidentRecoveryWaitsForTrigger(t *testing.T) {
	f := newAccountFixture(t)
	ctx := context.Background()
	n := connectionConfig{Name: "Incidents", Notification: &notificationConfig{Provider: "pagerduty", Token: "test-routing-key", Enabled: true, Events: []string{"node_unreachable.alert", "node_unreachable.recovered"}}}
	f.call("root", "default", "POST", "/api/notifications", n, 201)
	if err := f.s.recordHealth(ctx, "default", "node_unreachable", "node", "Node unavailable", true); err != nil {
		t.Fatal(err)
	}
	f.sql("UPDATE health_alerts SET first_seen=now()-interval '3 minutes'")
	if err := f.s.recordHealth(ctx, "default", "node_unreachable", "node", "Node unavailable", true); err != nil {
		t.Fatal(err)
	}
	if err := f.s.recordHealth(ctx, "default", "node_unreachable", "node", "Node unavailable", false); err != nil {
		t.Fatal(err)
	}
	var actions, keys []string
	f.s.NotificationTransport = notificationRoundTrip(func(r *http.Request) (*http.Response, error) {
		var payload struct {
			Action string `json:"event_action"`
			Key    string `json:"dedup_key"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		actions = append(actions, payload.Action)
		keys = append(keys, payload.Key)
		status := 202
		if len(actions) == 1 {
			status = 503
		}
		return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(""))}, nil
	})
	for i := 0; i < 4; i++ {
		f.sql("UPDATE health_deliveries SET next_attempt=now()")
		if err := f.s.dispatchHealthNotifications(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if strings.Join(actions, ",") != "trigger,trigger,resolve" || keys[0] != keys[1] || keys[1] != keys[2] {
		t.Fatal(actions, keys)
	}
}

func TestNotificationProviderContracts(t *testing.T) {
	for _, provider := range []string{"telegram", "ntfy", "gotify", "pushover", "matrix", "teams", "pagerduty", "opsgenie"} {
		t.Run(provider, func(t *testing.T) {
			n := &notificationConfig{Provider: provider, URL: "https://example.com", Token: "token", Target: "target", UserKey: "user", Enabled: true, Events: []string{"node_unreachable.alert", "node_unreachable.recovered"}}
			if err := validateConnection("notification", connectionConfig{Name: "Channel", Notification: n}); err != nil {
				t.Fatal(err)
			}
			endpoint, method, payload, headers := notificationRequest(n, map[string]string{"event": "node_unreachable.alert", "alert_key": "team:node:subject", "message": "Cove alert", "delivery_id": "delivery"})
			data, err := json.Marshal(payload)
			if err != nil || !strings.HasPrefix(endpoint, "https://") || !strings.Contains(string(data), "Cove") {
				t.Fatal(endpoint, payload, err)
			}
			switch provider {
			case "telegram":
				if endpoint != "https://api.telegram.org/bottoken/sendMessage" || !strings.Contains(string(data), `"chat_id":"target"`) {
					t.Fatal(endpoint, string(data))
				}
			case "gotify":
				if endpoint != "https://example.com/message" || headers.Get("X-Gotify-Key") != "token" {
					t.Fatal(endpoint, headers)
				}
			case "ntfy":
				if headers.Get("Authorization") != "Bearer token" || !strings.Contains(string(data), `"topic":"target"`) {
					t.Fatal(headers, string(data))
				}
			case "matrix":
				if method != http.MethodPut || endpoint != "https://example.com/_matrix/client/v3/rooms/target/send/m.room.message/delivery" || headers.Get("Authorization") != "Bearer token" {
					t.Fatal(endpoint, method, headers)
				}
			case "pushover":
				if endpoint != "https://api.pushover.net/1/messages.json" || !strings.Contains(string(data), `"user":"user"`) {
					t.Fatal(endpoint, string(data))
				}
			case "teams":
				if !strings.Contains(string(data), `"type":"AdaptiveCard"`) {
					t.Fatal(string(data))
				}
			case "pagerduty", "opsgenie":
				_, _, resolved, _ := notificationRequest(n, map[string]string{"event": "node_unreachable.recovered", "alert_key": "team:node:subject", "message": "Cove recovered", "delivery_id": "different"})
				trigger := payload.(map[string]any)
				if provider == "pagerduty" {
					recovery := resolved.(map[string]any)
					if trigger["dedup_key"] != recovery["dedup_key"] || recovery["event_action"] != "resolve" {
						t.Fatal(trigger, recovery)
					}
				} else {
					url, _, _, _ := notificationRequest(n, map[string]string{"event": "node_unreachable.recovered", "alert_key": "team:node:subject"})
					if url != "https://example.com/v2/alerts/"+trigger["alias"].(string)+"/close?identifierType=alias" {
						t.Fatal(url, trigger)
					}
				}
			}
			redacted := redactConnection(connectionConfig{Notification: n}).Notification
			if redacted.Token != "" || redacted.UserKey != "" || !redacted.TokenSet || !redacted.UserKeySet {
				t.Fatal("credentials exposed")
			}
		})
	}
}

func TestNotificationProviderValidation(t *testing.T) {
	for _, n := range []notificationConfig{
		{Provider: "pagerduty", Token: "key", Enabled: true, Events: []string{"node_unreachable.alert"}},
		{Provider: "opsgenie", URL: "https://api.opsgenie.com", Token: "key", Enabled: true, Failure: true},
		{Provider: "telegram", Token: "../unsafe", Target: "chat", Failure: true},
		{Provider: "telegram", Token: "token", Failure: true},
		{Provider: "matrix", URL: "https://example.com?token=secret", Token: "token", Target: "room", Failure: true},
		{Provider: "gotify", URL: "https://example.com", Token: "secret\r\nInjected: header", Failure: true},
		{Provider: "pushover", Token: "app", Failure: true},
		{Provider: "ntfy", URL: "http://example.com", Target: "topic", Failure: true},
		{Provider: "email", Email: "invalid", Failure: true},
	} {
		if validateConnection("notification", connectionConfig{Name: "Invalid", Notification: &n}) == nil {
			t.Fatalf("invalid %s channel accepted", n.Provider)
		}
	}
}

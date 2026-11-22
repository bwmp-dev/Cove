package control

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestSignedWebhookRouting(t *testing.T) {
	secret := strings.Repeat("a", 32)
	n := &notificationConfig{Provider: "webhook", URL: "https://example.com/hook", SigningSecret: secret, Enabled: true, Events: []string{"backup.failed"}}
	if err := validateConnection("notification", connectionConfig{Name: "Backup", Notification: n}); err != nil {
		t.Fatal(err)
	}
	if !notificationMatches(n, operationEvent("backup", "failed")) || notificationMatches(n, "operation.failed") || notificationMatches(n, "backup.succeeded") {
		t.Fatal("incorrect event routing")
	}
	s := &Server{NotificationTransport: notificationRoundTrip(func(r *http.Request) (*http.Response, error) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		mac := hmac.New(sha256.New, []byte(secret))
		mac.Write([]byte(r.Header.Get("X-Cove-Timestamp") + "."))
		mac.Write(body)
		if r.Header.Get("X-Cove-Timestamp") == "" || r.Header.Get("X-Cove-Signature") != "sha256="+hex.EncodeToString(mac.Sum(nil)) {
			t.Fatal("invalid signature")
		}
		return &http.Response{StatusCode: 204, Body: io.NopCloser(strings.NewReader(""))}, nil
	})}
	if err := s.sendNotification(context.Background(), n, map[string]string{"event": "backup.failed", "message": "backup failed"}); err != nil {
		t.Fatal(err)
	}
	redacted := redactConnection(connectionConfig{Notification: n}).Notification
	if redacted.SigningSecret != "" || !redacted.SigningSecretSet || redacted.URL != "" {
		t.Fatal("secret exposed")
	}
	for _, events := range [][]string{{"invented.event"}, {"backup.failed", "backup.failed"}} {
		n.Events = events
		if validateNotificationRouting(n) == nil {
			t.Fatal("invalid routing accepted")
		}
	}
}

func TestNotificationEmailAndCatalogue(t *testing.T) {
	f := newAccountFixture(t)
	ctx := context.Background()
	secret := strings.Repeat("s", 32)
	config := connectionConfig{Name: "Signed", Notification: &notificationConfig{Provider: "webhook", URL: "https://example.com/hook", SigningSecret: secret, Enabled: true, Events: []string{"drill.failed"}}}
	id := jsonString(t, f.call("root", "default", "POST", "/api/notifications", config, 201), "id")
	config.Notification.URL = ""
	config.Notification.SigningSecret = ""
	f.call("root", "default", "PUT", "/api/notifications/"+id, config, 200)
	saved, err := f.s.readConnection(ctx, id, "default", "notification")
	if err != nil || saved.Notification.SigningSecret != secret {
		t.Fatal("secret not preserved", err)
	}
	list := f.call("root", "default", "GET", "/api/notifications", nil, 200).Body.String()
	if strings.Contains(list, secret) || !strings.Contains(list, `"signing_secret_set":true`) {
		t.Fatal("redaction failed")
	}
	f.call("root", "default", "GET", "/api/notifications/events", nil, 200)
	config = connectionConfig{Name: "Mail", Notification: &notificationConfig{Provider: "email", Email: "alerts@example.com", Enabled: true, Events: []string{"backup.succeeded"}}}
	f.call("root", "default", "POST", "/api/notifications", config, 201)
	called := false
	f.s.SendMail = func(_ context.Context, to, subject, body string) error {
		called = to == "alerts@example.com" && body == "Backed up" && subject == "Cove notification"
		return nil
	}
	if err := f.s.sendNotification(ctx, config.Notification, map[string]string{"message": "Backed up"}); err != nil || !called {
		t.Fatal("mail not delivered", err)
	}
	config = connectionConfig{Name: "Push", Notification: &notificationConfig{Provider: "pushover", Token: "private-app-token", UserKey: "private-user-key", Enabled: true, Failure: true}}
	id = jsonString(t, f.call("root", "default", "POST", "/api/notifications", config, 201), "id")
	config.Notification.Token = ""
	config.Notification.UserKey = ""
	f.call("root", "default", "PUT", "/api/notifications/"+id, config, 200)
	saved, err = f.s.readConnection(ctx, id, "default", "notification")
	if err != nil || saved.Notification.Token != "private-app-token" || saved.Notification.UserKey != "private-user-key" {
		t.Fatal("provider credentials not preserved", err)
	}
	list = f.call("root", "default", "GET", "/api/notifications", nil, 200).Body.String()
	if strings.Contains(list, "private-app-token") || strings.Contains(list, "private-user-key") || !strings.Contains(list, `"token_set":true`) || !strings.Contains(list, `"user_key_set":true`) {
		t.Fatal("provider credentials exposed")
	}
}

func TestBackupAndDrillEventRouting(t *testing.T) {
	f := newAccountFixture(t)
	ctx := context.Background()
	config := connectionConfig{Name: "Backup outcomes", Notification: &notificationConfig{Provider: "webhook", URL: "https://example.com/hook", Enabled: true, Events: []string{"backup.succeeded", "drill.failed"}}}
	f.call("root", "default", "POST", "/api/notifications", config, 201)
	f.sql("INSERT INTO nodes(id,name,address,port,public_key,private_key) VALUES('events-node','Node','localhost',22,'',$1)", []byte{})
	f.sql("INSERT INTO projects(id,name,team_id) VALUES('events-project','Project','default')")
	sealed, _ := seal(f.s.Key, []byte(`{}`))
	f.sql("INSERT INTO workloads(id,node_id,name,configuration,project_id,team_id) VALUES('events-resource','events-node','Resource',$1,'events-project','default')", sealed)
	got := map[string]int{}
	f.s.NotificationTransport = notificationRoundTrip(func(r *http.Request) (*http.Response, error) {
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		got[body["event"]]++
		return &http.Response{StatusCode: 204, Body: io.NopCloser(strings.NewReader(""))}, nil
	})
	for _, sample := range []struct{ action, state string }{{"backup", "succeeded"}, {"backup", "failed"}, {"drill", "failed"}, {"deploy", "failed"}} {
		id := resourceID()
		f.sql("INSERT INTO operations(id,workload_id,action,payload) VALUES($1,'events-resource',$2,$3)", id, sample.action, sealed)
		f.sql("UPDATE operations SET state=$2 WHERE id=$1", id, sample.state)
		if err := f.s.dispatchNotifications(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if len(got) != 2 || got["backup.succeeded"] != 1 || got["drill.failed"] != 1 {
		t.Fatal("unexpected deliveries", got)
	}
	viewer := f.user("event-viewer", "event-viewer@example.com")
	f.sql("INSERT INTO team_members(team_id,user_id,role,grants) VALUES('default','event-viewer','member','[]')")
	f.call(viewer, "default", "GET", "/api/notifications/events", nil, 403)
}

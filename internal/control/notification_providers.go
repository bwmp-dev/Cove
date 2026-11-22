package control

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/url"
	"strings"
)

func notificationRequest(c *notificationConfig, event map[string]string) (string, string, any, http.Header) {
	endpoint, method := c.URL, http.MethodPost
	publicEvent := make(map[string]string, len(event))
	for key, value := range event {
		if key != "alert_key" {
			publicEvent[key] = value
		}
	}
	var payload any = publicEvent
	headers := http.Header{}
	message := event["message"]
	switch c.Provider {
	case "discord":
		payload = map[string]any{"content": message, "allowed_mentions": map[string]any{"parse": []string{}}}
	case "slack":
		payload = map[string]any{"text": message, "mrkdwn": false, "unfurl_links": false, "unfurl_media": false}
	case "telegram":
		endpoint = "https://api.telegram.org/bot" + c.Token + "/sendMessage"
		payload = map[string]any{"chat_id": c.Target, "text": message, "link_preview_options": map[string]bool{"is_disabled": true}}
	case "ntfy":
		payload = map[string]any{"topic": c.Target, "title": "Cove", "message": message}
		if c.Token != "" {
			headers.Set("Authorization", "Bearer "+c.Token)
		}
	case "gotify":
		endpoint = strings.TrimRight(c.URL, "/") + "/message"
		payload = map[string]any{"title": "Cove", "message": message, "priority": 5}
		headers.Set("X-Gotify-Key", c.Token)
	case "pushover":
		endpoint = "https://api.pushover.net/1/messages.json"
		payload = map[string]any{"token": c.Token, "user": c.UserKey, "title": "Cove", "message": message}
	case "matrix":
		transaction := event["delivery_id"]
		if transaction == "" {
			transaction = resourceID()
		}
		endpoint = strings.TrimRight(c.URL, "/") + "/_matrix/client/v3/rooms/" + url.PathEscape(c.Target) + "/send/m.room.message/" + url.PathEscape(transaction)
		method = http.MethodPut
		payload = map[string]any{"msgtype": "m.text", "body": message}
		headers.Set("Authorization", "Bearer "+c.Token)
	case "teams":
		payload = map[string]any{"type": "message", "attachments": []any{map[string]any{"contentType": "application/vnd.microsoft.card.adaptive", "content": map[string]any{"$schema": "http://adaptivecards.io/schemas/adaptive-card.json", "type": "AdaptiveCard", "version": "1.2", "body": []any{map[string]any{"type": "TextBlock", "text": message, "wrap": true}}}}}}
	case "pagerduty", "opsgenie":
		key := event["alert_key"]
		if key == "" {
			key = event["delivery_id"]
		}
		if key == "" {
			key = resourceID()
		}
		hash := sha256.Sum256([]byte(key))
		key = hex.EncodeToString(hash[:])
		recovered := strings.HasSuffix(event["event"], ".recovered")
		if c.Provider == "pagerduty" {
			endpoint = "https://events.pagerduty.com/v2/enqueue"
			action := "trigger"
			if recovered {
				action = "resolve"
			}
			payload = map[string]any{"routing_key": c.Token, "event_action": action, "dedup_key": key, "payload": map[string]any{"summary": message, "source": "Cove", "severity": "error"}}
		} else {
			endpoint = strings.TrimRight(c.URL, "/") + "/v2/alerts"
			headers.Set("Authorization", "GenieKey "+c.Token)
			payload = map[string]any{"message": "Cove health alert", "description": message, "alias": key, "source": "Cove"}
			if recovered {
				endpoint += "/" + key + "/close?identifierType=alias"
				payload = map[string]string{"source": "Cove", "note": message}
			}
		}
	}
	return endpoint, method, payload, headers
}

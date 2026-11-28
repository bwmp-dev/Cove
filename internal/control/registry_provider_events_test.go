package control

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestRegistryProviderAuthenticationAndNativePayloads(t *testing.T) {
	now := time.Unix(1795446000, 0)
	secret := []byte(strings.Repeat("x", 43))
	hub := []byte(fmt.Sprintf(`{"push_data":{"pushed_at":%d,"tag":"stable"},"repository":{"repo_name":"team/app"},"callback_url":"http://127.0.0.1/never-fetch"}`, now.Unix()))
	r := httptest.NewRequest("POST", "/hooks/registry/id?secret="+string(secret), nil)
	if !registryWebhookAuthenticated("docker_hub", secret, hub, r) || r.URL.RawQuery != "" {
		t.Fatal("native secret authentication or URL credential removal failed")
	}
	events, err := registryProviderEvents("docker_hub", hub, r, now)
	if err != nil || len(events) != 1 || events[0].Repository != "team/app" || events[0].Tag != "stable" || events[0].SourceKey != events[0].Key {
		t.Fatal(events, err)
	}
	retry, err := registryProviderEvents("docker_hub", hub, r, now)
	if err != nil || retry[0] != events[0] {
		t.Fatal("native retry identity changed")
	}
	if _, err := registryProviderEvents("docker_hub", hub, r, now.Add(8*24*time.Hour)); err == nil {
		t.Fatal("expired native push accepted")
	}
	ghcr := []byte(`{"action":"published","package":{"id":1,"name":"app","ecosystem":"container","owner":{"login":"TEAM"},"package_version":{"id":2,"container_metadata":{"tag":{"name":"stable","digest":"ignored"}}}}}`)
	r = httptest.NewRequest("POST", "/hooks/registry/id", nil)
	r.Header.Set("X-GitHub-Event", "package")
	r.Header.Set("X-GitHub-Delivery", "delivery-one")
	mac := hmac.New(sha256.New, secret)
	mac.Write(ghcr)
	r.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	if !registryWebhookAuthenticated("ghcr", secret, ghcr, r) || registryWebhookAuthenticated("ghcr", secret, append(append([]byte{}, ghcr...), ' '), r) {
		t.Fatal("signature did not bind exact raw bytes")
	}
	first, err := registryProviderEvents("ghcr", ghcr, r, now)
	if err != nil || len(first) != 1 || first[0].Repository != "team/app" {
		t.Fatal(first, err)
	}
	r.Header.Set("X-GitHub-Delivery", "replayed-with-new-header")
	second, err := registryProviderEvents("ghcr", ghcr, r, now)
	if err != nil || second[0].Key == first[0].Key || second[0].SourceKey != first[0].SourceKey {
		t.Fatal("unsigned header bypassed stable signed event identity", second, err)
	}
	untagged := []byte(strings.Replace(string(ghcr), `"name":"stable"`, `"name":""`, 1))
	if events, err := registryProviderEvents("ghcr", untagged, r, now); err != nil || len(events) != 0 {
		t.Fatal("untagged architecture manifest requested deployment", events, err)
	}
	r.Header.Set("X-Hub-Signature-256", "sha1="+hex.EncodeToString(mac.Sum(nil)))
	if registryWebhookAuthenticated("ghcr", secret, ghcr, r) {
		t.Fatal("legacy signature accepted")
	}
	for _, provider := range []string{"distribution", "docker_hub"} {
		r = httptest.NewRequest("POST", "/hooks/registry/id", nil)
		r.Header.Set("Authorization", "Bearer "+strings.Repeat("y", 43))
		if registryWebhookAuthenticated(provider, secret, hub, r) {
			t.Fatal("wrong bearer accepted")
		}
		r.Header.Set("Authorization", "Bearer "+string(secret))
		if !registryWebhookAuthenticated(provider, secret, hub, r) {
			t.Fatal("controlled relay bearer refused")
		}
	}
}

package control

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestConnectedGitSigningProtocols(t *testing.T) {
	body := []byte(`{"ref":"refs/heads/main"}`)
	key := bytes.Repeat([]byte{7}, 32)
	provider := connectedGitProvider{connection: gitConnection{Kind: "gitlab"}}
	secret := []byte("whsec_" + base64.StdEncoding.EncodeToString(key))
	signed := func(timestamp time.Time) http.Header {
		headers := http.Header{}
		headers.Set("webhook-id", "fixture-id")
		headers.Set("webhook-timestamp", strconv.FormatInt(timestamp.Unix(), 10))
		mac := hmac.New(sha256.New, key)
		mac.Write([]byte("fixture-id." + headers.Get("webhook-timestamp") + "."))
		mac.Write(body)
		headers.Set("webhook-signature", "v0,invalid v1,"+base64.StdEncoding.EncodeToString(mac.Sum(nil)))
		return headers
	}
	headers := signed(time.Now())
	if !provider.VerifyWebhook(secret, body, headers) {
		t.Fatal("valid Standard Webhooks signature rejected")
	}
	if provider.VerifyWebhook(secret, []byte(`{"ref":"refs/heads/other"}`), headers) {
		t.Fatal("modified payload accepted")
	}
	if provider.VerifyWebhook(secret, body, signed(time.Now().Add(-6*time.Minute))) {
		t.Fatal("stale signed delivery accepted")
	}
	headers = signed(time.Now())
	headers.Set("webhook-id", "different-id")
	if provider.VerifyWebhook(secret, body, headers) {
		t.Fatal("unsigned delivery identity accepted")
	}
	headers = http.Header{}
	headers.Set("X-Gitlab-Token", string(secret))
	if provider.VerifyWebhook(secret, body, headers) {
		t.Fatal("plain secret token accepted instead of signature")
	}
	for _, kind := range []string{"gitea", "bitbucket"} {
		provider.connection.Kind = kind
		headers = http.Header{}
		signature := "a4771c39fbe90f317c7824e83ddef3caae9cb3d976c214ace1f2937e133263c9"
		header := "X-Gitea-Signature"
		if kind == "bitbucket" {
			header = "X-Hub-Signature"
			signature = "sha256=" + signature
		}
		headers.Set(header, signature)
		if !provider.VerifyWebhook([]byte("It's a Secret to Everybody"), []byte("Hello World!"), headers) {
			t.Fatalf("%s signature vector rejected", kind)
		}
		headers.Set(header, strings.Replace(signature, "a", "b", 1))
		if provider.VerifyWebhook([]byte("It's a Secret to Everybody"), []byte("Hello World!"), headers) {
			t.Fatal("wrong signature accepted")
		}
	}
}

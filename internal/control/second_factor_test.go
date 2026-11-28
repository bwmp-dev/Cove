package control

import (
	"context"
	"encoding/base32"
	"encoding/json"
	"net/url"
	"testing"
	"time"
)

func TestTOTPVectorAndReplay(t *testing.T) {
	secret := []byte("12345678901234567890")
	if got := totpCode(secret, 1); got != "287082" {
		t.Fatal(got)
	}
	if _, ok := matchTOTPStep(secret, "287082", -1, time.Unix(59, 0)); !ok {
		t.Fatal("RFC vector rejected")
	}
	if _, ok := matchTOTPStep(secret, "287082", 1, time.Unix(59, 0)); ok {
		t.Fatal("replayed code accepted")
	}
	for _, code := range []string{"", "12345", "1234567", "abcdef"} {
		if _, ok := matchTOTPStep(secret, code, -1, time.Unix(59, 0)); ok {
			t.Fatal("invalid code accepted")
		}
	}
}

func TestTOTPIntegration(t *testing.T) {
	f := newAccountFixture(t)
	session := f.user("factor-user", "factor@example.com")
	w := f.call(session, "", "POST", "/api/account/totp", map[string]string{}, 201)
	uri, err := url.Parse(jsonString(t, w, "provisioning_uri"))
	if err != nil {
		t.Fatal(err)
	}
	secret, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(uri.Query().Get("secret"))
	if err != nil {
		t.Fatal(err)
	}
	f.call(session, "", "POST", "/api/account/totp/confirm", map[string]string{"code": "bad"}, 400)
	w = f.call(session, "", "POST", "/api/account/totp/confirm", map[string]string{"code": totpCode(secret, time.Now().Unix()/30)}, 200)
	var out struct {
		Codes []string `json:"recovery_codes"`
	}
	if json.Unmarshal(w.Body.Bytes(), &out) != nil || len(out.Codes) != 10 {
		t.Fatal(w.Body.String())
	}
	f.call(session, "", "GET", "/api/session", nil, 200)
	var encrypted []byte
	if err = f.s.DB.QueryRow(context.Background(), "SELECT secret FROM account_totp WHERE user_id='factor-user'").Scan(&encrypted); err != nil {
		t.Fatal(err)
	}
	if string(encrypted) == string(secret) {
		t.Fatal("plaintext secret")
	}
	pending := f.user("other-user", "other@example.com")
	f.sql("UPDATE sessions SET user_id='factor-user',expires_at=now()+interval '12 hours' WHERE token_hash=$1", tokenHash(pending))
	f.call(pending, "", "GET", "/api/session", nil, 401)
	f.call(pending, "", "POST", "/auth/second-factor", map[string]string{"code": totpCode(secret, time.Now().Unix()/30)}, 401)
	w = f.call(pending, "", "POST", "/auth/second-factor", map[string]string{"code": out.Codes[0]}, 200)
	f.call(w.Result().Cookies()[0].Value, "", "GET", "/api/session", nil, 200)
	f.sql("INSERT INTO sessions(token_hash,user_id,expires_at) VALUES($1,'factor-user',now()+interval '12 hours')", tokenHash(pending))
	f.call(pending, "", "POST", "/auth/second-factor", map[string]string{"code": out.Codes[0]}, 401)
	f.call("root", "", "DELETE", "/api/users/factor-user/second-factor", nil, 200)
	f.call(session, "", "GET", "/api/session", nil, 401)
	if err = f.s.Init(context.Background()); err != nil {
		t.Fatal("idempotent migration:", err)
	}
}

func TestSecondFactorPolicyIntegration(t *testing.T) {
	f := newAccountFixture(t)
	session := f.user("policy-user", "policy@example.com")
	f.call("root", "", "PUT", "/api/settings/second-factor", map[string]bool{"require_second_factor": true}, 200)
	f.call(session, "", "GET", "/api/workloads", nil, 403)
	f.call(session, "", "GET", "/api/account/sign-in-methods", nil, 200)
	f.call("root", "", "GET", "/api/workloads", nil, 200)
}

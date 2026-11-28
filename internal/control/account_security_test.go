package control

import (
	"context"
	"encoding/base32"
	"encoding/json"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestAccountSecurityGapClosure(t *testing.T) {
	f := newAccountFixture(t)
	session := f.user("security", "security@example.com")
	hash, _ := passwordHash("correct-password-for-security")
	f.sql("UPDATE users SET password_hash=$1 WHERE id='security'", hash)
	proof := map[string]string{"current_password": "correct-password-for-security"}
	f.call(session, "", "POST", "/api/account/recovery-codes", proof, 409)
	secret, _ := seal(f.s.Key, []byte("12345678901234567890"))
	f.sql("INSERT INTO account_totp(user_id,secret,confirmed) VALUES('security',$1,true)", secret)
	f.sql("UPDATE sessions SET second_factor_verified=true WHERE user_id='security'")
	f.sql("INSERT INTO recovery_codes(user_id,code_hash) VALUES('security',$1)", tokenHash("old-code"))
	f.call(session, "", "POST", "/api/account/recovery-codes", map[string]string{"code": "wrong"}, 403)
	w := f.call(session, "", "POST", "/api/account/recovery-codes", map[string]string{"code": "old-code"}, 200)
	var out struct {
		Codes []string `json:"recovery_codes"`
	}
	if json.Unmarshal(w.Body.Bytes(), &out) != nil || len(out.Codes) != 10 {
		t.Fatal(w.Body.String())
	}
	var old bool
	if err := f.s.DB.QueryRow(context.Background(), "SELECT EXISTS(SELECT 1 FROM recovery_codes WHERE code_hash=$1)", tokenHash("old-code")).Scan(&old); err != nil || old {
		t.Fatal("old recovery set remains", err)
	}
	oldSet := append([]string(nil), out.Codes...)
	w = f.call(session, "", "POST", "/api/account/recovery-codes", map[string]string{"code": oldSet[0]}, 200)
	if json.Unmarshal(w.Body.Bytes(), &out) != nil || len(out.Codes) != 10 {
		t.Fatal(w.Body.String())
	}
	if err := f.s.DB.QueryRow(context.Background(), "SELECT EXISTS(SELECT 1 FROM recovery_codes WHERE code_hash=$1)", tokenHash(oldSet[1])).Scan(&old); err != nil || old {
		t.Fatal("unconsumed old code remains", err)
	}
	f.sql("DELETE FROM login_attempts")
	w = f.call(session, "", "GET", "/api/account/sign-in-methods", nil, 200)
	if !strings.Contains(w.Body.String(), `"recovery_codes_remaining":10`) {
		t.Fatal(w.Body.String())
	}
	f.call(session, "", "DELETE", "/api/account/totp", map[string]string{"current_password": "wrong"}, 403)
	f.sql("INSERT INTO team_members(team_id,user_id,role) VALUES('default','security','member')")
	f.sql("UPDATE teams SET require_second_factor=true WHERE id='default'")
	f.call(session, "", "DELETE", "/api/account/totp", proof, 409)
	f.sql("UPDATE teams SET require_second_factor=false WHERE id='default'")
	f.sql("DELETE FROM login_attempts")
	f.sql("UPDATE control_settings SET require_second_factor=true WHERE id=1")
	f.call(session, "", "DELETE", "/api/account/totp", proof, 409)
	f.sql("UPDATE control_settings SET require_second_factor=false WHERE id=1")
	f.call(session, "", "DELETE", "/api/account/totp", proof, 200)
	f.call(session, "", "GET", "/api/session", nil, 200)
	w = f.call(session, "", "GET", "/api/account/sign-in-methods", nil, 200)
	if !strings.Contains(w.Body.String(), `"recovery_codes_remaining":0`) || !strings.Contains(w.Body.String(), `"totp_enrolled":false`) {
		t.Fatal(w.Body.String())
	}
	w = f.call(session, "", "POST", "/api/account/totp", proof, 201)
	uri, err := url.Parse(jsonString(t, w, "provisioning_uri"))
	if err != nil {
		t.Fatal(err)
	}
	replacement, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(uri.Query().Get("secret"))
	if err != nil {
		t.Fatal(err)
	}
	f.call(session, "", "POST", "/api/account/totp/confirm", map[string]string{"code": totpCode(replacement, time.Now().Unix()/30)}, 200)
	var denied, success int
	err = f.s.DB.QueryRow(context.Background(), "SELECT count(*) FILTER(WHERE status=403),count(*) FILTER(WHERE status=200) FROM audit_events WHERE action LIKE '%/api/account/%' ").Scan(&denied, &success)
	if err != nil || denied == 0 || success == 0 {
		t.Fatal("missing security audits", denied, success, err)
	}
}

func TestIdentityUnlinkGapClosure(t *testing.T) {
	f := newAccountFixture(t)
	session := f.user("linked", "linked@example.com")
	hash, _ := passwordHash("correct-password-for-identities")
	f.sql("UPDATE users SET password_hash=$1,github_id='123' WHERE id='linked'", hash)
	proof := map[string]string{"current_password": "correct-password-for-identities"}
	w := f.call("root", "", "POST", "/api/settings/oidc/providers", map[string]any{"display_name": "Fixture", "issuer": "https://id.example.com", "client_id": "client", "scopes": []string{}}, 200)
	provider := jsonString(t, w, "id")
	if jsonString(t, w, "redirect_uri") != f.s.PublicURL+"/auth/oidc/"+provider+"/callback" {
		t.Fatal(w.Body.String())
	}
	w = f.call("root", "", "GET", "/api/settings/oidc/providers", nil, 200)
	if !strings.Contains(w.Body.String(), `"redirect_uri":"`+f.s.PublicURL+"/auth/oidc/"+provider+"/callback") {
		t.Fatal(w.Body.String())
	}
	f.sql("INSERT INTO oidc_identities(provider_id,subject,user_id) VALUES($1,'subject','linked')", provider)
	w = f.call(session, "", "GET", "/api/account/identities", nil, 200)
	if !strings.Contains(w.Body.String(), `"provider_id":"github"`) || !strings.Contains(w.Body.String(), `"subject":"subject"`) {
		t.Fatal(w.Body.String())
	}
	f.call(session, "", "DELETE", "/api/account/identities/"+provider, map[string]string{"current_password": "wrong"}, 403)
	other := f.user("other-linked", "other-linked@example.com")
	otherHash, _ := passwordHash("other-password-for-identities")
	f.sql("UPDATE users SET password_hash=$1 WHERE id='other-linked'", otherHash)
	f.call(other, "", "DELETE", "/api/account/identities/"+provider, map[string]string{"current_password": "other-password-for-identities"}, 404)
	f.call(session, "", "DELETE", "/api/account/identities/github", proof, 200)
	f.sql("UPDATE control_settings SET password_login_disabled=true WHERE id=1")
	f.call(session, "", "DELETE", "/api/account/identities/"+provider, proof, 409)
	f.sql("UPDATE control_settings SET password_login_disabled=false WHERE id=1")
	f.sql("DELETE FROM login_attempts")
	f.call(session, "", "DELETE", "/api/account/identities/"+provider, proof, 200)
	f.call(session, "", "DELETE", "/api/account/identities/"+provider, proof, 404)
}

func TestPendingFactorDiscoveryGapClosure(t *testing.T) {
	f := newAccountFixture(t)
	session := f.user("pending", "pending@example.com")
	secret, _ := seal(f.s.Key, []byte("12345678901234567890"))
	f.sql("INSERT INTO account_totp(user_id,secret,confirmed) VALUES('pending',$1,true)", secret)
	f.sql("INSERT INTO recovery_codes(user_id,code_hash) VALUES('pending',$1)", tokenHash("recovery"))
	f.sql("INSERT INTO passkeys(id,user_id,name,credential) VALUES($1,'pending','Key','{}')", []byte("key"))
	check := func(token string, pending bool) {
		t.Helper()
		w := f.call(token, "", "GET", "/auth/sign-in-methods", nil, 200)
		var out struct {
			Pending bool     `json:"second_factor_pending"`
			Methods []string `json:"second_factor_methods"`
		}
		if json.Unmarshal(w.Body.Bytes(), &out) != nil || out.Pending != pending || (pending && strings.Join(out.Methods, ",") != "totp,recovery_code,passkey") || (!pending && len(out.Methods) != 0) {
			t.Fatal(w.Body.String())
		}
	}
	check("", false)
	check("invalid", false)
	check(session, false)
	f.sql("UPDATE sessions SET expires_at=$1 WHERE user_id='pending'", time.Now().Add(12*time.Hour))
	check(session, true)
	f.sql("UPDATE users SET disabled=true WHERE id='pending'")
	check(session, false)
	f.sql("UPDATE users SET disabled=false WHERE id='pending'")
	f.sql("UPDATE sessions SET second_factor_verified=true WHERE user_id='pending'")
	check(session, false)
	w := f.call("", "", "POST", "/auth/passkeys/begin", map[string]any{}, 200)
	var options map[string]json.RawMessage
	if json.Unmarshal(w.Body.Bytes(), &options) != nil || options["publicKey"] == nil {
		t.Fatal(w.Body.String())
	}
	if options["mediation"] != nil {
		t.Fatal("unexpected mediation", w.Body.String())
	}
}

func TestDelegatedPermissionsGapClosure(t *testing.T) {
	f := newAccountFixture(t)
	session := f.user("manager", "manager@example.com")
	target := f.user("target", "target@example.com")
	_ = target
	role := f.call("root", "default", "POST", "/api/teams/default/roles", map[string]any{"name": "Members", "permissions": []string{"view", "manage_members"}}, 200)
	id := jsonString(t, role, "id")
	f.sql("INSERT INTO team_members(team_id,user_id,role,grants) VALUES('default','manager','member',$1)", []grant{{Scope: "team", Role: id}})
	f.sql("INSERT INTO team_members(team_id,user_id,role) VALUES('default','target','member')")
	w := f.call(session, "default", "GET", "/api/session", nil, 200)
	if !strings.Contains(w.Body.String(), `"permissions":["view","manage_members"]`) {
		t.Fatal(w.Body.String())
	}
	f.call(session, "default", "GET", "/api/teams/default/roles", nil, 200)
	f.call(session, "default", "POST", "/api/teams/default/roles", map[string]any{"name": "No", "permissions": []string{"view"}}, 403)
	f.call(session, "default", "GET", "/api/teams/default/members", nil, 200)
	f.call(session, "default", "PUT", "/api/teams/default/members/target", map[string]any{"role": "member", "grants": []grant{{Scope: "team", Role: id}}}, 200)
	f.call(session, "default", "PUT", "/api/teams/default/members/target", map[string]any{"role": "admin", "grants": []grant{}}, 403)
	f.sql("UPDATE team_members SET role='admin' WHERE user_id='target'")
	f.call(session, "default", "DELETE", "/api/teams/default/members/target", nil, 403)
	f.sql("UPDATE team_members SET grants='[]' WHERE user_id='manager'")
	f.call(session, "default", "GET", "/api/teams/default/roles", nil, 403)
}

func TestPasswordlessSecurityGapClosure(t *testing.T) {
	f := newAccountFixture(t)
	session := f.user("passwordless", "passwordless@example.com")
	f.sql("UPDATE users SET github_id='987' WHERE id='passwordless'")
	secret, _ := seal(f.s.Key, []byte("12345678901234567890"))
	f.sql("INSERT INTO account_totp(user_id,secret,confirmed) VALUES('passwordless',$1,true)", secret)
	f.sql("UPDATE sessions SET second_factor_verified=true WHERE user_id='passwordless'")
	f.sql("INSERT INTO recovery_codes(user_id,code_hash) VALUES('passwordless',$1)", tokenHash("proof"))
	f.call("", "", "DELETE", "/api/account/totp", map[string]string{"code": "proof"}, 401)
	// Refused unlinking must roll back consumption of a valid factor proof.
	f.call(session, "", "DELETE", "/api/account/identities/github", map[string]string{"code": "proof"}, 409)
	f.sql("INSERT INTO passkeys(id,user_id,name,credential) VALUES($1,'passwordless','Key','{}')", []byte("key"))
	f.sql("UPDATE control_settings SET require_second_factor=true WHERE id=1")
	f.call(session, "", "DELETE", "/api/account/totp", map[string]string{"code": "proof"}, 200)
	f.sql("INSERT INTO recovery_codes(user_id,code_hash) VALUES('passwordless',$1)", tokenHash("passkey-backup"))
	f.call(session, "", "POST", "/api/account/recovery-codes", map[string]string{"code": "passkey-backup"}, 200)
}

func TestRecentFactorReauthenticationGapClosure(t *testing.T) {
	f := newAccountFixture(t)
	session := f.user("fresh-key", "fresh-key@example.com")
	f.sql("INSERT INTO passkeys(id,user_id,name,credential) VALUES($1,'fresh-key','Key','{}')", []byte("key"))
	f.sql("UPDATE sessions SET second_factor_verified=true WHERE user_id='fresh-key'")
	f.call(session, "", "POST", "/api/account/recovery-codes", map[string]string{}, 403)
	f.sql("UPDATE sessions SET expires_at=now()+interval '12 hours' WHERE user_id='fresh-key'")
	f.call(session, "", "POST", "/api/account/recovery-codes", map[string]string{}, 200)
}

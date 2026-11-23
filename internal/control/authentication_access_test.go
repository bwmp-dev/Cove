package control

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"github.com/fxamacker/cbor/v2"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
)

func TestOIDCIntegration(t *testing.T) {
	f := newAccountFixture(t)
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: key}, (&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", "fixture"))
	if err != nil {
		t.Fatal(err)
	}
	challenge, nonce := "", ""
	verified := true
	badNonce := false
	exchanges := 0
	f.s.OAuthClient = &http.Client{Transport: oauthTransport(func(r *http.Request) (*http.Response, error) {
		var out any
		switch r.URL.Path {
		case "/.well-known/openid-configuration":
			out = map[string]any{"issuer": "https://id.example.com", "authorization_endpoint": "https://id.example.com/authorize", "token_endpoint": "https://id.example.com/token", "jwks_uri": "https://id.example.com/keys", "id_token_signing_alg_values_supported": []string{"RS256"}}
		case "/keys":
			out = jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &key.PublicKey, KeyID: "fixture", Algorithm: "RS256", Use: "sig"}}}
		case "/token":
			exchanges++
			if e := r.ParseForm(); e != nil {
				t.Fatal(e)
			}
			hash := sha256.Sum256([]byte(r.Form.Get("code_verifier")))
			if base64.RawURLEncoding.EncodeToString(hash[:]) != challenge {
				return &http.Response{StatusCode: 400, Body: io.NopCloser(strings.NewReader(`{"error":"invalid_grant"}`)), Header: http.Header{"Content-Type": {"application/json"}}}, nil
			}
			n := nonce
			if badNonce {
				n = "wrong"
			}
			claims, _ := json.Marshal(map[string]any{"iss": "https://id.example.com", "aud": "fixture-client", "sub": "fixture-subject", "exp": time.Now().Add(time.Hour).Unix(), "iat": time.Now().Unix(), "nonce": n, "email": "oidc@example.com", "email_verified": verified, "name": "Fixture"})
			signed, e := signer.Sign(claims)
			if e != nil {
				t.Fatal(e)
			}
			raw, e := signed.CompactSerialize()
			if e != nil {
				t.Fatal(e)
			}
			out = map[string]any{"access_token": "fixture-access", "token_type": "Bearer", "id_token": raw}
		default:
			t.Fatalf("unexpected provider path %s", r.URL.Path)
		}
		raw, _ := json.Marshal(out)
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(string(raw))), Header: http.Header{"Content-Type": {"application/json"}}}, nil
	})}
	w := f.call("root", "", "POST", "/api/settings/oidc/providers", map[string]any{"display_name": "Fixture", "issuer": "https://id.example.com", "client_id": "fixture-client", "client_secret": "fixture-secret", "scopes": []string{}}, 200)
	id := jsonString(t, w, "id")
	f.call("root", "", "PUT", "/api/settings/sign-in", map[string]any{"oidc_registration_enabled": true, "password_login_disabled": false, "oidc_auto_redirect": id}, 200)
	start := func() (string, *http.Cookie) {
		w := f.call("", "", "POST", "/auth/oidc/"+id+"/start", map[string]bool{}, 200)
		u, e := url.Parse(jsonString(t, w, "url"))
		if e != nil {
			t.Fatal(e)
		}
		challenge = u.Query().Get("code_challenge")
		nonce = u.Query().Get("nonce")
		if challenge == "" || nonce == "" || u.Query().Get("code_challenge_method") != "S256" {
			t.Fatal("missing binding")
		}
		return u.Query().Get("state"), w.Result().Cookies()[0]
	}
	callback := func(state string, cookie *http.Cookie, want string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", "/auth/oidc/"+id+"/callback?state="+url.QueryEscape(state)+"&code=fixture", nil)
		r.AddCookie(cookie)
		w := httptest.NewRecorder()
		f.handler.ServeHTTP(w, r)
		if w.Code != 303 || w.Header().Get("Location") != want {
			t.Fatalf("callback %d %s want %s", w.Code, w.Header().Get("Location"), want)
		}
		return w
	}
	state, cookie := start()
	callback(state, &http.Cookie{Name: cookie.Name, Value: "wrong"}, "/login?error=oidc_failed")
	if exchanges != 0 {
		t.Fatal("exchanged unbound state")
	}
	state, cookie = start()
	badNonce = true
	callback(state, cookie, "/login?error=oidc_failed")
	badNonce = false
	state, cookie = start()
	verified = false
	callback(state, cookie, "/login?error=verified_email_required")
	verified = true
	state, cookie = start()
	encrypted, e := seal(f.s.Key, []byte("wrong-verifier"))
	if e != nil {
		t.Fatal(e)
	}
	f.sql("UPDATE oidc_states SET verifier=$1 WHERE token_hash=$2", encrypted, tokenHash(state))
	callback(state, cookie, "/login?error=oidc_failed")
	state, cookie = start()
	callback(state, cookie, "/login?authenticated=1")
	callback(state, cookie, "/login?error=oidc_failed")
	f.call("root", "", "GET", "/api/settings/oidc/providers", nil, 200)
}
func TestCustomRolesIntegration(t *testing.T) {
	f := newAccountFixture(t)
	user := f.user("custom-user", "custom@example.com")
	w := f.call("root", "default", "POST", "/api/teams/default/roles", map[string]any{"name": "Configuration only", "permissions": []string{"view", "edit_configuration"}}, 200)
	id := jsonString(t, w, "id")
	f.sql("INSERT INTO team_members(team_id,user_id,role,grants) VALUES('default','custom-user','member',$1)", []grant{{Scope: "team", Role: id}})
	f.call(user, "default", "GET", "/api/workloads", nil, 200)
	f.call(user, "default", "POST", "/api/projects", map[string]string{"name": "Custom role project"}, 201)
	f.call(user, "default", "GET", "/api/users", nil, 403)
	f.call("root", "default", "DELETE", "/api/teams/default/roles/"+id, nil, 409)
	for _, permission := range []string{"read_secrets", "operate", "manage_backups", "manage_members"} {
		var allowed bool
		if f.s.DB.QueryRow(context.Background(), "SELECT cove_permission('custom-user','default','','',$1)", permission).Scan(&allowed) != nil || allowed {
			t.Fatalf("unexpected %s", permission)
		}
	}
	var level int
	if f.s.DB.QueryRow(context.Background(), "SELECT cove_access('custom-user','default','','')").Scan(&level) != nil || level != 1 {
		t.Fatalf("unsafe legacy level %d", level)
	}
	f.call("root", "default", "POST", "/api/teams/default/roles", map[string]any{"name": "Invalid", "permissions": []string{"view", "administrator"}}, 400)
}
func TestAuditExportIntegration(t *testing.T) {
	f := newAccountFixture(t)
	f.call("root", "", "PUT", "/api/settings/audit-export", map[string]any{"webhook_url": "https://audit.example.com/events", "webhook_secret": strings.Repeat("x", 32), "syslog_address": "", "syslog_tls": true, "retention_days": 30}, 200)
	var pending int
	if f.s.DB.QueryRow(context.Background(), "SELECT count(*) FROM audit_deliveries").Scan(&pending) != nil || pending < 1 {
		t.Fatal("audit delivery not enqueued")
	}
	f.s.NotificationTransport = oauthTransport(func(r *http.Request) (*http.Response, error) {
		body, e := io.ReadAll(r.Body)
		if e != nil {
			t.Fatal(e)
		}
		mac := hmac.New(sha256.New, []byte(strings.Repeat("x", 32)))
		mac.Write(body)
		if r.Header.Get("X-Cove-Signature") != "sha256="+hex.EncodeToString(mac.Sum(nil)) || r.Header.Get("X-Cove-Delivery") == "" {
			t.Fatal("unsigned delivery")
		}
		return &http.Response{StatusCode: 503, Body: io.NopCloser(strings.NewReader("failed")), Header: http.Header{}}, nil
	})
	if err := f.s.dispatchAuditExport(context.Background()); err != nil {
		t.Fatal(err)
	}
	var attempts int
	if f.s.DB.QueryRow(context.Background(), "SELECT max(attempts) FROM audit_deliveries").Scan(&attempts) != nil || attempts != 1 {
		t.Fatal("retry not persisted")
	}
	f.sql("UPDATE audit_deliveries SET next_attempt_at=now()")
	f.s.NotificationTransport = oauthTransport(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 204, Body: io.NopCloser(strings.NewReader("")), Header: http.Header{}}, nil
	})
	if err := f.s.dispatchAuditExport(context.Background()); err != nil {
		t.Fatal(err)
	}
	var remaining int
	if f.s.DB.QueryRow(context.Background(), "SELECT count(*) FROM audit_deliveries").Scan(&remaining) != nil || remaining != pending-1 {
		t.Fatal("delivered event not removed")
	}
}
func TestPasskeyChallengeIntegration(t *testing.T) {
	f := newAccountFixture(t)
	user := f.user("passkey-user", "passkey@example.com")
	f.call(user, "", "POST", "/api/account/passkeys/begin", map[string]string{}, 200)
	f.call(user, "", "POST", "/api/account/passkeys/finish", map[string]string{}, 401)
	f.call("", "", "POST", "/auth/passkeys/begin", map[string]string{}, 200)
	f.call("", "", "POST", "/auth/passkeys/finish", map[string]string{}, 401)
	f.call(user, "", "GET", "/api/account/passkeys", nil, 200)
}

func TestPasskeyRegistrationAndSignInIntegration(t *testing.T) {
	f := newAccountFixture(t)
	session := f.user("key-owner", "key@example.com")
	private, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	id := make([]byte, 32)
	rand.Read(id)
	call := func(path string, input any, cookies ...*http.Cookie) *httptest.ResponseRecorder {
		raw, _ := json.Marshal(input)
		r := httptest.NewRequest("POST", path, bytes.NewReader(raw))
		r.Header.Set("Origin", f.s.PublicURL)
		for _, c := range cookies {
			r.AddCookie(c)
		}
		w := httptest.NewRecorder()
		f.handler.ServeHTTP(w, r)
		return w
	}
	w := f.call(session, "", "POST", "/api/account/passkeys/begin", map[string]string{}, 200)
	var options struct {
		PublicKey struct {
			Challenge string `json:"challenge"`
		} `json:"publicKey"`
	}
	if json.Unmarshal(w.Body.Bytes(), &options) != nil || options.PublicKey.Challenge == "" {
		t.Fatal(w.Body.String())
	}
	data, _ := json.Marshal(map[string]any{"type": "webauthn.create", "challenge": options.PublicKey.Challenge, "origin": f.s.PublicURL})
	rpHash := sha256.Sum256([]byte("cove.example.com"))
	auth := append([]byte{}, rpHash[:]...)
	auth = append(auth, 0x45, 0, 0, 0, 0)
	auth = append(auth, make([]byte, 16)...)
	var size [2]byte
	binary.BigEndian.PutUint16(size[:], uint16(len(id)))
	auth = append(auth, size[:]...)
	auth = append(auth, id...)
	cose, _ := cbor.Marshal(map[int]any{1: 2, 3: -7, -1: 1, -2: private.X.FillBytes(make([]byte, 32)), -3: private.Y.FillBytes(make([]byte, 32))})
	auth = append(auth, cose...)
	attestation, _ := cbor.Marshal(map[string]any{"fmt": "none", "authData": auth, "attStmt": map[string]any{}})
	encoded := base64.RawURLEncoding.EncodeToString
	registration := map[string]any{"id": encoded(id), "rawId": encoded(id), "type": "public-key", "response": map[string]any{"clientDataJSON": encoded(data), "attestationObject": encoded(attestation), "transports": []string{"internal"}}, "clientExtensionResults": map[string]any{}}
	w = call("/api/account/passkeys/finish", registration, &http.Cookie{Name: "__Host-cove", Value: session}, w.Result().Cookies()[0])
	if w.Code != 200 {
		t.Fatalf("register: %d %s", w.Code, w.Body.String())
	}
	w = f.call("", "", "POST", "/auth/passkeys/begin", map[string]string{}, 200)
	if json.Unmarshal(w.Body.Bytes(), &options) != nil {
		t.Fatal(w.Body.String())
	}
	browser := w.Result().Cookies()[0]
	data, _ = json.Marshal(map[string]any{"type": "webauthn.get", "challenge": options.PublicKey.Challenge, "origin": f.s.PublicURL})
	auth = append([]byte{}, rpHash[:]...)
	auth = append(auth, 0x05, 0, 0, 0, 1)
	clientHash := sha256.Sum256(data)
	message := append(append([]byte{}, auth...), clientHash[:]...)
	digest := sha256.Sum256(message)
	signature, err := ecdsa.SignASN1(rand.Reader, private, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	assertion := map[string]any{"id": encoded(id), "rawId": encoded(id), "type": "public-key", "response": map[string]any{"clientDataJSON": encoded(data), "authenticatorData": encoded(auth), "signature": encoded(signature), "userHandle": encoded([]byte("key-owner"))}, "clientExtensionResults": map[string]any{}}
	w = call("/auth/passkeys/finish", assertion, browser)
	if w.Code != 200 {
		t.Fatalf("login: %d %s", w.Code, w.Body.String())
	}
	cookies := w.Result().Cookies()
	var signedIn string
	for _, c := range cookies {
		if c.Name == "__Host-cove" {
			signedIn = c.Value
		}
	}
	if signedIn == "" {
		t.Fatal("missing session")
	}
	f.call(signedIn, "", "GET", "/api/session", nil, 200)
	w = call("/auth/passkeys/finish", assertion, browser)
	if w.Code != 401 {
		t.Fatalf("replay: %d", w.Code)
	}
}
func TestLoginLockoutIntegration(t *testing.T) {
	f := newAccountFixture(t)
	f.user("locked-user", "locked@example.com")
	for i := 0; i < 10; i++ {
		f.call("", "", "POST", "/auth/login", map[string]string{"email": "locked@example.com", "password": "wrong"}, 401)
	}
	w := f.call("", "", "POST", "/auth/login", map[string]string{"email": "locked@example.com", "password": "wrong"}, 429)
	if w.Header().Get("Retry-After") != "900" {
		t.Fatal("missing lockout interval")
	}
	var failures int
	if f.s.DB.QueryRow(context.Background(), "SELECT count(*) FROM login_events WHERE user_id='locked-user' AND NOT success").Scan(&failures) != nil || failures != 10 {
		t.Fatalf("failed events: %d", failures)
	}
	f.sql("UPDATE authentication_failures SET locked_until=now()-interval '1 second'")
	f.sql("DELETE FROM login_attempts")
	f.call("", "", "POST", "/auth/login", map[string]string{"email": "locked@example.com", "password": "wrong"}, 401)
}

func TestAuditSyslogFraming(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	received := make(chan string, 1)
	go func() {
		connection, e := listener.Accept()
		if e != nil {
			received <- ""
			return
		}
		defer connection.Close()
		connection.SetDeadline(time.Now().Add(3 * time.Second))
		reader := bufio.NewReader(connection)
		var size int
		if _, e = fmt.Fscanf(reader, "%d ", &size); e != nil {
			received <- ""
			return
		}
		body := make([]byte, size)
		if _, e = io.ReadFull(reader, body); e != nil {
			received <- ""
			return
		}
		received <- string(body)
	}()
	s := &Server{}
	if err = s.deliverAudit(context.Background(), auditExportConfig{Syslog: listener.Addr().String()}, "syslog", 123, []byte(`{"id":1}`)); err != nil {
		t.Fatal(err)
	}
	select {
	case message := <-received:
		if !strings.HasPrefix(message, "<134>1 ") || !strings.Contains(message, `delivery="123"`) || !strings.HasSuffix(message, `{"id":1}`) {
			t.Fatal(message)
		}
	case <-time.After(4 * time.Second):
		t.Fatal("collector timed out")
	}
}

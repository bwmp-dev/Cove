package control

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-ldap/ldap/v3"
	"golang.org/x/crypto/bcrypt"
)

func ldapFixtureConfiguration(t *testing.T, transport string) ldapConfiguration {
	t.Helper()
	port := os.Getenv("COVE_TEST_LDAP_LDAPS_PORT")
	if transport == "starttls" {
		port = os.Getenv("COVE_TEST_LDAP_STARTTLS_PORT")
	}
	if port == "" {
		t.Skip("run python scripts/test-ldap.py for real OpenLDAP acceptance")
	}
	ca, err := os.ReadFile(os.Getenv("COVE_TEST_LDAP_CA"))
	if err != nil {
		t.Fatal(err)
	}
	number, err := strconv.Atoi(port)
	if err != nil {
		t.Fatal(err)
	}
	return ldapConfiguration{Name: "Disposable OpenLDAP", Host: "127.0.0.1", Port: number, Transport: transport, CA: string(ca), BindDN: "cn=service,dc=example,dc=invalid", Password: os.Getenv("COVE_TEST_LDAP_PASSWORD"), Base: "ou=users,dc=example,dc=invalid", Filter: "(&(objectClass=inetOrgPerson)(uid={username}))", Subject: "entryUUID", Email: "mail", NameAttribute: "cn", GroupBase: "ou=groups,dc=example,dc=invalid", GroupFilter: "(&(objectClass=groupOfNames)(member={dn}))", Registration: true}
}

func TestLDAPRejectsUnsafeConfigurationAndTLSHostname(t *testing.T) {
	c := ldapConfiguration{Name: "Directory", Host: "localhost", Port: 636, Transport: "ldaps", BindDN: "cn=service,dc=example", Password: "synthetic-fixture", Base: "ou=users,dc=example", Filter: "(uid={username})", Subject: "entryUUID", Email: "mail", NameAttribute: "cn", GroupBase: "ou=groups,dc=example", GroupFilter: "(member={dn})"}
	if err := validateLDAP(c); err != nil {
		t.Fatal(err)
	}
	for _, edit := range []func(*ldapConfiguration){func(c *ldapConfiguration) { c.Transport = "ldap" }, func(c *ldapConfiguration) { c.Password = "" }, func(c *ldapConfiguration) { c.Subject = "uid" }, func(c *ldapConfiguration) { c.Filter = "(uid=*)" }, func(c *ldapConfiguration) { c.Filter = "(uid={username}" }, func(c *ldapConfiguration) { c.CA = "invalid" }} {
		unsafe := c
		edit(&unsafe)
		if validateLDAP(unsafe) == nil {
			t.Fatal("unsafe directory configuration accepted")
		}
	}
	unsafe := c
	unsafe.Transport = "ldap"
	if connection, err := dialLDAP(t.Context(), unsafe); err == nil {
		connection.Close()
		t.Fatal("plaintext transport accepted")
	}
	t.Setenv("COVE_LDAP_ALLOWED_CIDRS", "127.0.0.1/32,::1/128")
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("LDAP credentials reached HTTP handler") }))
	server.Config.ErrorLog = log.New(io.Discard, "", 0)
	server.StartTLS()
	defer server.Close()
	address, _ := url.Parse(server.URL)
	number, _ := strconv.Atoi(address.Port())
	c.Port = number
	c.CA = string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}))
	if connection, err := dialLDAP(t.Context(), c); err == nil {
		connection.Close()
		t.Fatal("trusted certificate with wrong hostname accepted")
	}
}

func ldapFixtureProvider(t *testing.T, f *accountFixture, c ldapConfiguration) string {
	t.Helper()
	return jsonString(t, f.call("root", "", "POST", "/api/settings/ldap/providers", map[string]any{"configuration": c, "enabled": true, "mappings": []directoryMapping{{Group: "cn=engineering,ou=groups,dc=example,dc=invalid", Team: "default", Role: "editor"}}}, 200), "id")
}

func TestLDAPVerifiedTLSAndDirectoryBoundaries(t *testing.T) {
	for _, transport := range []string{"ldaps", "starttls"} {
		t.Run(transport, func(t *testing.T) {
			c := ldapFixtureConfiguration(t, transport)
			if err := validateLDAP(c); err != nil {
				t.Fatal(err)
			}
			account, err := authenticateLDAP(t.Context(), c, "alice", c.Password)
			if err != nil || account.Email != "alice@example.invalid" || account.Subject == "" || len(account.Groups) != 1 {
				t.Fatal("native bind/search failed", err)
			}
			if _, err = authenticateLDAP(t.Context(), c, "alice", "incorrect"); err != errLDAPCredentials {
				t.Fatal("wrong password accepted", err)
			}
			if _, err = authenticateLDAP(t.Context(), c, `alice)(|(uid=*))`, c.Password); err != errLDAPCredentials {
				t.Fatal("filter injection not rejected", err)
			}
			untrusted := c
			untrusted.CA = ""
			if connection, err := dialLDAP(t.Context(), untrusted); err == nil {
				connection.Close()
				t.Fatal("untrusted certificate accepted")
			}
			mismatch := c
			mismatch.Host = "127.0.0.2"
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			if connection, err := dialLDAP(ctx, mismatch); err == nil {
				connection.Close()
				t.Fatal("disallowed target accepted")
			}
			t.Setenv("COVE_LDAP_ALLOWED_CIDRS", "")
			if connection, err := dialLDAP(t.Context(), c); err == nil {
				connection.Close()
				t.Fatal("missing host policy allowed directory dial")
			}
		})
	}
}

func TestLDAPSignInLinkingMembershipAndRecovery(t *testing.T) {
	c := ldapFixtureConfiguration(t, "ldaps")
	f := newAccountFixture(t)
	id := ldapFixtureProvider(t, f, c)
	f.call("root", "", "POST", "/api/settings/ldap/providers/"+id+"/test", nil, 200)
	var posture struct{ Checks []postureCheck }
	if json.Unmarshal(f.call("root", "", "GET", "/api/settings/security-posture", nil, 200).Body.Bytes(), &posture) != nil {
		t.Fatal("invalid security checklist")
	}
	found := false
	for _, check := range posture.Checks {
		if check.ID == "registration" {
			found = true
			if check.Status != "warn" {
				t.Fatal("LDAP registration missing from checklist")
			}
		}
	}
	if !found {
		t.Fatal("registration checklist missing")
	}
	public := f.call("", "", "GET", "/auth/ldap/providers", nil, 200).Body.String()
	if strings.Contains(public, c.BindDN) || strings.Contains(public, c.Password) || !strings.Contains(public, c.Name) {
		t.Fatal("public directory discovery exposed configuration")
	}
	saved := f.call("root", "", "GET", "/api/settings/ldap/providers", nil, 200).Body.String()
	if strings.Contains(saved, c.Password) {
		t.Fatal("directory bind password exposed")
	}
	var encrypted []byte
	if err := f.s.DB.QueryRow(t.Context(), "SELECT configuration FROM ldap_providers WHERE id=$1", id).Scan(&encrypted); err != nil || strings.Contains(string(encrypted), c.Password) {
		t.Fatal("directory configuration not sealed", err)
	}
	result := f.call("", "", "POST", "/auth/ldap/"+id+"/sign-in", map[string]any{"email": "alice", "password": c.Password}, 200)
	user := jsonString(t, result, "user")
	session := ""
	for _, cookie := range result.Result().Cookies() {
		if cookie.Name == "__Host-cove" {
			session = cookie.Value
		}
	}
	if session == "" {
		t.Fatal("sign-in issued no session")
	}
	f.call(session, "default", "GET", "/api/session", nil, 200)
	var grants []grant
	if err := f.s.DB.QueryRow(t.Context(), "SELECT grants FROM team_members WHERE team_id='default' AND user_id=$1", user).Scan(&grants); err != nil || len(grants) != 1 || grants[0].Role != "editor" {
		t.Fatal("LDAP group mapping failed", grants, err)
	}
	f.call("", "", "POST", "/auth/ldap/"+id+"/sign-in", map[string]any{"email": "alice", "password": ""}, 400)
	collision := f.user("collision-local", "collision@example.invalid")
	hash, _ := bcrypt.GenerateFromPassword([]byte("local-fixture-password"), bcrypt.MinCost)
	f.sql("UPDATE users SET password_hash=$1 WHERE id='collision-local'", hash)
	f.call("", "", "POST", "/auth/ldap/"+id+"/sign-in", map[string]any{"email": "collision", "password": c.Password}, 409)
	f.call(collision, "", "POST", "/auth/ldap/"+id+"/sign-in", map[string]any{"email": "collision", "password": c.Password, "link": true, "current_password": "wrong"}, 403)
	f.call(collision, "", "POST", "/auth/ldap/"+id+"/sign-in", map[string]any{"email": "collision", "password": c.Password, "link": true, "current_password": "local-fixture-password"}, 200)
	f.call(collision, "default", "DELETE", "/api/account/identities/ldap:"+id, map[string]string{"current_password": "local-fixture-password"}, 200)
	var retained bool
	if err := f.s.DB.QueryRow(t.Context(), "SELECT EXISTS(SELECT 1 FROM team_members WHERE team_id='default' AND user_id='collision-local')").Scan(&retained); err != nil || retained {
		t.Fatal("unlink retained directory-only access", err)
	}
	f.call(session, "default", "DELETE", "/api/account/identities/ldap:"+id, map[string]string{}, 403)
	protectedSecret, err := seal(f.s.Key, []byte("alice-unlink-factor"))
	if err != nil {
		t.Fatal(err)
	}
	f.sql("INSERT INTO account_totp(user_id,secret,confirmed) VALUES($1,$2,true)", user, protectedSecret)
	f.sql("UPDATE sessions SET second_factor_verified=true,expires_at=now()+interval '12 hours' WHERE token_hash=$1", tokenHash(session))
	f.call(session, "default", "DELETE", "/api/account/identities/ldap:"+id, map[string]string{}, 409)
	updated := c
	updated.Password = ""
	f.call("root", "", "PUT", "/api/settings/ldap/providers/"+id, map[string]any{"configuration": updated, "enabled": false, "version": 1, "mappings": []directoryMapping{}}, 200)
	f.call("", "", "POST", "/auth/ldap/"+id+"/sign-in", map[string]any{"email": "alice", "password": c.Password}, 404)
	f.call("root", "", "PUT", "/api/settings/ldap/providers/"+id, map[string]any{"configuration": updated, "enabled": true, "version": 2, "mappings": []directoryMapping{}}, 200)
	f.call("", "", "POST", "/auth/ldap/"+id+"/sign-in", map[string]any{"email": "alice", "password": c.Password}, 200)
	f.call("root", "", "DELETE", "/api/settings/ldap/providers/"+id, map[string]any{"version": 3, "confirm": true}, 409)
}

func TestLDAPSecondFactorLockoutAndFailedSearchRetainsMembership(t *testing.T) {
	c := ldapFixtureConfiguration(t, "starttls")
	f := newAccountFixture(t)
	id := ldapFixtureProvider(t, f, c)
	userSession := f.user("factor-local", "factor@example.invalid")
	hash, _ := bcrypt.GenerateFromPassword([]byte("local-fixture-password"), bcrypt.MinCost)
	f.sql("UPDATE users SET password_hash=$1 WHERE id='factor-local'", hash)
	f.call(userSession, "", "POST", "/auth/ldap/"+id+"/sign-in", map[string]any{"email": "factor", "password": c.Password, "link": true, "current_password": "local-fixture-password"}, 200)
	secret := []byte("directory-factor-test")
	encrypted, err := seal(f.s.Key, secret)
	if err != nil {
		t.Fatal(err)
	}
	f.sql("INSERT INTO account_totp(user_id,secret,confirmed) VALUES('factor-local',$1,true)", encrypted)
	response := f.call("", "", "POST", "/auth/ldap/"+id+"/sign-in", map[string]any{"email": "factor", "password": c.Password}, 200)
	var payload map[string]any
	if json.Unmarshal(response.Body.Bytes(), &payload) != nil || payload["second_factor_required"] != true {
		t.Fatal("factor not requested")
	}
	session := ""
	for _, cookie := range response.Result().Cookies() {
		if cookie.Name == "__Host-cove" {
			session = cookie.Value
		}
	}
	f.call(session, "default", "GET", "/api/workloads", nil, 401)
	f.call(session, "default", "POST", "/auth/logout", map[string]any{}, 200)
	f.call(session, "default", "GET", "/api/workloads", nil, 401)
	response = f.call("", "", "POST", "/auth/ldap/"+id+"/sign-in", map[string]any{"email": "factor", "password": c.Password}, 200)
	for _, cookie := range response.Result().Cookies() {
		if cookie.Name == "__Host-cove" {
			session = cookie.Value
		}
	}
	f.call(session, "default", "POST", "/auth/second-factor", map[string]string{"code": totpCode(secret, time.Now().Unix()/30)}, 200)
	f.sql("INSERT INTO authentication_failures(key,failures,locked_until) VALUES('account:factor-local',10,now()+interval '15 minutes')")
	f.call("", "", "POST", "/auth/ldap/"+id+"/sign-in", map[string]any{"email": "factor", "password": c.Password}, 429)
	f.sql("DELETE FROM authentication_failures")
	broken := c
	broken.GroupBase = "ou=missing,dc=example,dc=invalid"
	broken.Password = ""
	f.call("root", "", "PUT", "/api/settings/ldap/providers/"+id, map[string]any{"configuration": broken, "enabled": true, "version": 1, "mappings": []directoryMapping{}}, 200)
	f.call("", "", "POST", "/auth/ldap/"+id+"/sign-in", map[string]any{"email": "factor", "password": c.Password}, 502)
	var exists bool
	if err := f.s.DB.QueryRow(t.Context(), "SELECT EXISTS(SELECT 1 FROM identity_membership_claims WHERE source_kind='ldap' AND source_id=$1 AND user_id='factor-local')", id).Scan(&exists); err != nil || !exists {
		t.Fatal("failed group search removed grants", err)
	}
	connection, err := dialLDAP(t.Context(), c)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	if err = connection.Bind(c.BindDN, c.Password); err != nil {
		t.Fatal(err)
	}
	search, err := connection.Search(ldap.NewSearchRequest(c.Base, ldap.ScopeWholeSubtree, ldap.NeverDerefAliases, 2, 5, false, "(uid=factor)", []string{"entryUUID"}, nil))
	if err != nil || len(search.Entries) != 1 {
		t.Fatal("real directory fixture missing", err)
	}
	change := ldap.NewModifyRequest("cn=engineering,ou=groups,dc=example,dc=invalid", nil)
	change.Delete("member", []string{"uid=factor,ou=users,dc=example,dc=invalid"})
	if err = connection.Modify(change); err != nil {
		t.Fatal(err)
	}
	defer func() {
		restore := ldap.NewModifyRequest(change.DN, nil)
		restore.Add("member", []string{"uid=factor,ou=users,dc=example,dc=invalid"})
		if err := connection.Modify(restore); err != nil {
			t.Error(err)
		}
	}()
	c.Password = ""
	f.call("root", "", "PUT", "/api/settings/ldap/providers/"+id, map[string]any{"configuration": c, "enabled": true, "version": 2, "mappings": []directoryMapping{{Group: "cn=engineering,ou=groups,dc=example,dc=invalid", Team: "default", Role: "editor"}}}, 200)
	f.call("", "", "POST", "/auth/ldap/"+id+"/sign-in", map[string]any{"email": "factor", "password": os.Getenv("COVE_TEST_LDAP_PASSWORD")}, 200)
	if err = f.s.DB.QueryRow(t.Context(), "SELECT EXISTS(SELECT 1 FROM identity_membership_claims WHERE source_kind='ldap' AND source_id=$1 AND user_id='factor-local')", id).Scan(&exists); err != nil || exists {
		t.Fatal("successful complete search retained removed group access", err)
	}
}

func TestLDAPRemainsIdentityProviderWhenLocalPasswordsDisabled(t *testing.T) {
	c := ldapFixtureConfiguration(t, "ldaps")
	f := newAccountFixture(t)
	id := ldapFixtureProvider(t, f, c)
	hash, _ := bcrypt.GenerateFromPassword([]byte("local-break-glass-fixture"), bcrypt.MinCost)
	f.sql("UPDATE users SET password_hash=$1 WHERE id='bootstrap'", hash)
	f.sql("UPDATE control_settings SET password_login_disabled=true WHERE id=1")
	var methods struct {
		Password  bool                `json:"password_enabled"`
		LDAP      bool                `json:"ldap_enabled"`
		Providers []map[string]string `json:"ldap_providers"`
	}
	if json.Unmarshal(f.call("", "", "GET", "/auth/sign-in-methods", nil, 200).Body.Bytes(), &methods) != nil || methods.Password || !methods.LDAP || len(methods.Providers) != 1 || methods.Providers[0]["id"] != id {
		t.Fatal("directory availability was coupled to local passwords")
	}
	f.call("", "", "POST", "/auth/login", map[string]any{"password": "local-break-glass-fixture"}, 403)
	f.call("", "", "POST", "/auth/login", map[string]any{"password": "local-break-glass-fixture", "break_glass": true}, 200)
	f.call("", "", "POST", "/auth/ldap/"+id+"/sign-in", map[string]any{"email": "alice", "password": c.Password}, 200)
	for _, body := range []map[string]any{{"email": "", "password": c.Password}, {"email": "alice", "password": ""}, {"email": "alice", "password": strings.Repeat("x", 4097)}} {
		f.call("", "", "POST", "/auth/ldap/"+id+"/sign-in", body, 400)
	}
	f.call("", "", "POST", "/auth/ldap/"+id+"/sign-in", map[string]any{"email": "unassigned", "password": c.Password}, 401)
}

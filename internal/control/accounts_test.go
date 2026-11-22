package control

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"
)

type accountFixture struct {
	t       *testing.T
	s       *Server
	handler http.Handler
	mail    []string
}

func newAccountFixture(t *testing.T) *accountFixture {
	t.Helper()
	database := os.Getenv("COVE_TEST_DATABASE_URL")
	if database == "" {
		t.Skip("COVE_TEST_DATABASE_URL is not set")
	}
	s := &Server{DB: openTestDatabase(t, database), Key: bytes.Repeat([]byte{4}, 32), AdminToken: randomToken(), PublicURL: "https://cove.example.com"}
	if err := s.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	f := &accountFixture{t: t, s: s}
	s.SendMail = func(_ context.Context, _, _, body string) error { f.mail = append(f.mail, body); return nil }
	f.handler = s.Handler()
	return f
}
func (f *accountFixture) sql(query string, args ...any) {
	f.t.Helper()
	if _, err := f.s.DB.Exec(context.Background(), query, args...); err != nil {
		f.t.Fatal(err)
	}
}
func (f *accountFixture) call(user, team, method, path string, input any, status int) *httptest.ResponseRecorder {
	f.t.Helper()
	body, err := json.Marshal(input)
	if err != nil {
		f.t.Fatal(err)
	}
	r := httptest.NewRequest(method, path, bytes.NewReader(body))
	r.Header.Set("Origin", f.s.PublicURL)
	r.Header.Set("X-Cove-Team", team)
	if user == "root" {
		r.Header.Set("Authorization", "Bearer "+f.s.AdminToken)
	} else if user != "" {
		r.AddCookie(&http.Cookie{Name: "__Host-cove", Value: user})
	}
	w := httptest.NewRecorder()
	f.handler.ServeHTTP(w, r)
	if w.Code != status {
		f.t.Fatalf("%s %s as %s: got %d want %d: %s", method, path, user, w.Code, status, w.Body.String())
	}
	return w
}
func (f *accountFixture) user(id, email string) string {
	f.t.Helper()
	f.sql("INSERT INTO users(id,email,name,email_verified) VALUES($1,$2,$1,true)", id, email)
	token := randomToken()
	f.sql("INSERT INTO sessions(token_hash,user_id,expires_at) VALUES($1,$2,now()+interval '1 hour')", tokenHash(token), id)
	return token
}
func jsonString(t *testing.T, w *httptest.ResponseRecorder, key string) string {
	t.Helper()
	var v map[string]json.RawMessage
	if json.Unmarshal(w.Body.Bytes(), &v) != nil {
		t.Fatal(w.Body.String())
	}
	var s string
	if json.Unmarshal(v[key], &s) != nil {
		t.Fatal(w.Body.String())
	}
	return s
}
func (f *accountFixture) mailToken() string {
	f.t.Helper()
	if len(f.mail) == 0 {
		f.t.Fatal("email was not sent")
	}
	_, token, ok := strings.Cut(f.mail[len(f.mail)-1], "#")
	if !ok {
		f.t.Fatal("email missing token")
	}
	return strings.TrimSpace(token)
}

func TestTeamAuthorizationIntegration(t *testing.T) {
	f := newAccountFixture(t)
	a := f.user("alice", "alice@example.com")
	b := f.user("bob", "bob@example.com")
	v := f.user("viewer", "viewer@example.com")
	f.sql("INSERT INTO teams(id,name) VALUES('a','Team A'),('b','Team B')")
	f.sql("INSERT INTO team_members(team_id,user_id,role,grants) VALUES('a','alice','owner','[]'),('b','bob','owner','[]'),('a','viewer','member','[{\"scope\":\"environment\",\"scope_id\":\"ea\",\"role\":\"viewer\"}]')")
	f.sql("INSERT INTO nodes(id,name,address,port,public_key,private_key,state) VALUES('shared','Shared','private-address',22,'',$1,'ready'),('hidden','Hidden','hidden-address',22,'',$1,'ready')", []byte{})
	f.sql("INSERT INTO team_nodes(team_id,node_id) VALUES('a','shared'),('b','shared')")
	f.sql("INSERT INTO projects(id,name,team_id) VALUES('pa','Same name','a'),('pb','Same name','b')")
	f.sql("INSERT INTO environments(id,project_id,name,team_id) VALUES('ea','pa','Prod','a'),('ea2','pa','Secret staging','a'),('eb','pb','Prod','b')")
	config, _ := seal(f.s.Key, []byte(`{"name":"workload","node_id":"shared","compose":"services:\n  app:\n    image: busybox:1.37\n","kind":"service"}`))
	f.sql("INSERT INTO workloads(id,node_id,name,configuration,environment_id,team_id,project_id) VALUES('wa','shared','Visible',$1,'ea','a','pa'),('wa2','shared','Hidden staging',$1,'ea2','a','pa'),('wb','shared','Other team',$1,'eb','b','pb')", config)
	f.sql("INSERT INTO operations(id,workload_id,action,state,payload) VALUES('oa','wa','deploy','succeeded',$1),('ob','wb','deploy','succeeded',$1)", []byte{})
	for _, target := range []string{"/api/workloads/wb", "/api/workloads/wb/runtime", "/api/workloads/wb/logs", "/api/workloads/wb/backups", "/api/workloads/wb/releases", "/api/workloads/wb/backup-policy", "/api/workloads/wb/webhook", "/api/operations/ob", "/api/operations/ob/logs"} {
		f.call(a, "a", "GET", target, nil, 404)
	}
	for _, target := range []string{"/api/workloads/wb", "/api/projects/pb", "/api/environments/eb"} {
		f.call(a, "a", "DELETE", target, nil, 404)
	}
	f.call(a, "a", "POST", "/api/operations/ob/cancel", nil, 404)
	f.call(a, "a", "POST", "/api/workloads/wb/operations", map[string]string{"action": "stop"}, 404)
	f.call(a, "a", "PUT", "/api/workloads/wa/environment", map[string]string{"environment_id": "eb"}, 404)
	f.call(a, "a", "POST", "/api/workloads", map[string]string{"name": "bad", "node_id": "shared", "environment_id": "eb"}, 404)
	f.call(a, "a", "POST", "/api/workloads", map[string]string{"name": "bad", "node_id": "hidden", "environment_id": "ea"}, 403)
	f.call(a, "a", "POST", "/api/nodes", map[string]string{}, 403)
	f.call(a, "a", "PUT", "/api/teams/a/nodes", map[string]any{"node_ids": []string{"hidden"}}, 403)
	f.call(a, "a", "GET", "/api/users", nil, 403)
	f.call(a, "a", "GET", "/api/teams/b/members", nil, 403)
	f.call(a, "b", "GET", "/api/workloads", nil, 401)
	f.call(b, "b", "GET", "/api/workloads/wb", nil, 200)
	for _, path := range []string{"/api/workloads", "/api/projects", "/api/operations"} {
		w := f.call(v, "a", "GET", path, nil, 200)
		if strings.Contains(w.Body.String(), "wb") || strings.Contains(w.Body.String(), "ea2") || strings.Contains(w.Body.String(), "ob") || strings.Contains(w.Body.String(), "wa2") {
			t.Fatalf("scoped list leaked data: %s", w.Body.String())
		}
	}
	w := f.call(v, "a", "GET", "/api/nodes", nil, 200)
	if strings.Contains(w.Body.String(), "private-address") || strings.Contains(w.Body.String(), "hidden") {
		t.Fatal("node details leaked")
	}
	f.call(v, "a", "GET", "/api/workloads/wa2", nil, 403)
	f.call(v, "a", "GET", "/api/workloads/wa", nil, 200)
	f.call(v, "a", "POST", "/api/workloads/wa/operations", map[string]string{"action": "stop"}, 403)
	f.call(v, "a", "POST", "/api/tokens", map[string]any{"name": "bad", "workload_id": "wa", "permission": "operate", "days": 1}, 403)
	f.call(v, "a", "GET", "/api/audit", nil, 403)
	f.call(v, "a", "POST", "/api/teams/a/invitations", map[string]string{"email": "x@example.com", "role": "admin"}, 403)
	f.call(a, "a", "PUT", "/api/teams/a/members/viewer", memberInput{Role: "member", Grants: []grant{{"environment", "ea", "operator"}}}, 200)
	f.call(v, "a", "POST", "/api/workloads/wa/operations", map[string]string{"action": "stop"}, 202)
	f.call(v, "a", "POST", "/api/workloads/wa/operations", map[string]string{"action": "restore"}, 403)
	f.call(v, "a", "POST", "/api/workloads/wa/operations", map[string]string{"action": "stop", "Action": "restore"}, 400)
	f.call(v, "a", "POST", "/api/workloads/wa/operations", map[string]string{"Action": "restore"}, 403)
	f.call(v, "a", "PUT", "/api/workloads/wa", map[string]string{}, 403)
	f.call(a, "a", "PUT", "/api/teams/a/members/viewer", memberInput{Role: "member", Grants: []grant{{"environment", "eb", "editor"}}}, 400)
	f.call(a, "a", "DELETE", "/api/teams/a/members/alice", nil, 409)
	token := jsonString(t, f.call(a, "a", "POST", "/api/tokens", map[string]any{"name": "deploy", "workload_id": "wa", "permission": "operate", "days": 1}, 201), "token")
	requestToken := func(status int) {
		t.Helper()
		r := httptest.NewRequest("GET", "/api/workloads/wa", nil)
		r.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		f.handler.ServeHTTP(w, r)
		if w.Code != status {
			t.Fatalf("token: got %d want %d", w.Code, status)
		}
	}
	requestToken(200)
	f.sql("DELETE FROM team_members WHERE team_id='a' AND user_id='alice'")
	requestToken(401)
	if _, err := f.s.DB.Exec(context.Background(), "UPDATE workloads SET environment_id='eb' WHERE id='wa'"); err == nil {
		t.Fatal("database allowed cross-team environment")
	}
	f.sql("UPDATE users SET disabled=true WHERE id='viewer'")
	f.call(v, "a", "GET", "/api/session", nil, 401)
}

func TestAccountLifecycleIntegration(t *testing.T) {
	f := newAccountFixture(t)
	password := randomToken()
	f.call("", "", "POST", "/auth/register", map[string]string{"email": "new@example.com", "name": "New", "password": password}, 403)
	f.call("root", "", "PUT", "/api/settings", map[string]bool{"registration_enabled": true}, 200)
	f.call("", "", "POST", "/auth/register", map[string]string{"email": "NEW@example.com", "name": "New", "password": password}, 200)
	token := f.mailToken()
	f.call("", "", "POST", "/auth/login", map[string]string{"email": "new@example.com", "password": password}, 401)
	f.call("", "", "POST", "/auth/verify", map[string]string{"token": token}, 200)
	f.call("", "", "POST", "/auth/verify", map[string]string{"token": token}, 400)
	login := f.call("", "", "POST", "/auth/login", map[string]string{"email": "NEW@example.com", "password": password}, 200)
	session := login.Result().Cookies()[0].Value
	f.call(session, "", "GET", "/api/nodes", nil, 200)
	f.call(session, "", "POST", "/api/projects", map[string]string{"name": "no access"}, 403)
	f.call(session, "", "POST", "/api/teams", map[string]string{"name": "My team"}, 201)
	f.call("", "", "POST", "/auth/forgot-password", map[string]string{"email": "new@example.com"}, 200)
	reset := f.mailToken()
	password2 := randomToken()
	f.call("", "", "POST", "/auth/reset-password", map[string]string{"token": reset, "password": password2}, 200)
	f.call("", "", "POST", "/auth/reset-password", map[string]string{"token": reset, "password": password2}, 400)
	f.call(session, "", "GET", "/api/session", nil, 401)
	f.call("", "", "POST", "/auth/login", map[string]string{"email": "new@example.com", "password": password}, 401)
	f.call("", "", "POST", "/auth/login", map[string]string{"email": "new@example.com", "password": password2}, 200)
	f.call("root", "", "PUT", "/api/settings", map[string]bool{"registration_enabled": false}, 200)
	w := f.call("root", "default", "POST", "/api/teams/default/invitations", map[string]any{"email": "invited@example.com", "role": "member", "grants": []grant{{"team", "", "viewer"}}}, 201)
	u, _ := url.Parse(jsonString(t, w, "url"))
	invite := u.Fragment
	f.call("", "", "POST", "/auth/invitation", map[string]string{"token": invite}, 200)
	f.call("", "", "POST", "/auth/accept-invitation", map[string]string{"token": invite, "name": "Invited", "password": password}, 200)
	f.call("", "", "POST", "/auth/accept-invitation", map[string]string{"token": invite, "name": "Invited", "password": password}, 400)
	w = f.call("root", "default", "POST", "/api/teams/default/invitations", map[string]any{"email": "new@example.com", "role": "member", "grants": []grant{}}, 201)
	u, _ = url.Parse(jsonString(t, w, "url"))
	f.call("", "", "POST", "/auth/accept-invitation", map[string]string{"token": u.Fragment, "name": "Hijack", "password": password}, 401)
	f.call("root", "", "PUT", "/api/users/bootstrap", map[string]bool{"is_admin": false, "disabled": false}, 409)
	f.s.SendMail = nil
	f.call("root", "", "PUT", "/api/settings", map[string]bool{"registration_enabled": true}, 409)
}

func TestLegacyAccountsMigrationIntegration(t *testing.T) {
	database := os.Getenv("COVE_TEST_DATABASE_URL")
	if database == "" {
		t.Skip("COVE_TEST_DATABASE_URL is not set")
	}
	db := openTestDatabase(t, database)
	ctx := context.Background()
	schema, _ := assets.ReadFile("schema.sql")
	legacy, _, _ := strings.Cut(string(schema), "CREATE TABLE IF NOT EXISTS users")
	if _, err := db.Exec(ctx, legacy); err != nil {
		t.Fatal(err)
	}
	password := randomToken()
	hash, _ := bcrypt.GenerateFromPassword([]byte(password), bcrypt.MinCost)
	if _, err := db.Exec(ctx, "INSERT INTO administrators(id,password_hash) VALUES(1,$1)", hash); err != nil {
		t.Fatal(err)
	}
	token := randomToken()
	if _, err := db.Exec(ctx, "INSERT INTO sessions(token_hash,expires_at) VALUES($1,$2)", tokenHash(token), time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	s := &Server{DB: db, Key: bytes.Repeat([]byte{5}, 32), AdminToken: randomToken(), PublicURL: "https://cove.example.com"}
	if err := s.Init(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.Init(ctx); err != nil {
		t.Fatal("migration not idempotent", err)
	}
	if err := s.InitAdministrator(ctx, randomToken()); err != nil {
		t.Fatal(err)
	}
	f := &accountFixture{t: t, s: s, handler: s.Handler()}
	f.call(token, "", "GET", "/api/session", nil, 200)
	f.call("", "", "POST", "/auth/login", map[string]string{"password": password}, 200)
}

func TestAccountRevocationAndPermissionBoundariesIntegration(t *testing.T) {
	f := newAccountFixture(t)
	owner := f.user("owner", "owner@example.com")
	other := f.user("other", "other@example.com")
	f.sql("INSERT INTO teams(id,name) VALUES('team','Team')")
	f.sql("INSERT INTO team_members(team_id,user_id,role) VALUES('team','owner','owner'),('team','other','admin')")
	f.call(other, "team", "PUT", "/api/teams/team/members/owner", memberInput{Role: "member", Grants: []grant{}}, 403)
	f.call(other, "team", "PUT", "/api/teams/team/members/other", memberInput{Role: "owner", Grants: []grant{}}, 403)
	f.call(owner, "team", "PUT", "/api/teams/team/members/other", memberInput{Role: "member", Grants: []grant{}}, 200)
	makeInvite := func(email string) (string, string) {
		t.Helper()
		w := f.call(owner, "team", "POST", "/api/teams/team/invitations", map[string]any{"email": email, "role": "member", "grants": []grant{}}, 201)
		u, _ := url.Parse(jsonString(t, w, "url"))
		return jsonString(t, w, "id"), u.Fragment
	}
	inviteID, token := makeInvite("new@example.com")
	f.sql("INSERT INTO oauth_states(token_hash,browser_hash,verifier,invitation_id,expires_at) VALUES($1,$2,$3,$4,now()+interval '10 minutes')", tokenHash(randomToken()), tokenHash(randomToken()), []byte{}, inviteID)
	f.call(owner, "team", "DELETE", "/api/teams/team/invitations/"+inviteID, nil, 200)
	f.call("", "", "POST", "/auth/invitation", map[string]string{"token": token}, 400)
	inviteID, token = makeInvite("expired@example.com")
	f.sql("UPDATE invitations SET expires_at=now()-interval '1 second' WHERE id=$1", inviteID)
	f.call("", "", "POST", "/auth/accept-invitation", map[string]string{"token": token, "name": "Expired", "password": randomToken()}, 400)
	_, token = makeInvite("other@example.com")
	f.call(owner, "team", "POST", "/auth/accept-invitation", map[string]string{"token": token}, 401)
	f.call(other, "team", "POST", "/auth/accept-invitation", map[string]string{"token": token}, 200)
	_, token = makeInvite("revoked-authority@example.com")
	f.sql("UPDATE team_members SET role='member' WHERE team_id='team' AND user_id='owner'")
	f.call("", "", "POST", "/auth/invitation", map[string]string{"token": token}, 400)
	f.call("", "", "POST", "/auth/accept-invitation", map[string]string{"token": token, "name": "No authority", "password": randomToken()}, 400)
	password := randomToken()
	hash, _ := passwordHash(password)
	f.sql("UPDATE users SET password_hash=$1 WHERE id='owner'", hash)
	f.call(owner, "team", "PUT", "/api/account", map[string]string{"name": "Owner", "email": "changed@example.com", "current_password": password}, 200)
	emailToken := f.mailToken()
	f.call(owner, "team", "POST", "/api/password", map[string]string{"current_password": password, "new_password": randomToken()}, 200)
	f.call("", "", "POST", "/auth/verify", map[string]string{"token": emailToken}, 400)
	f.call(other, "team", "GET", "/api/session", nil, 200)
	f.call(owner, "team", "GET", "/api/session", nil, 401)
	for _, path := range []string{"/auth/register", "/auth/verify", "/auth/forgot-password", "/auth/reset-password", "/auth/invitation", "/auth/accept-invitation", "/auth/github/start"} {
		r := httptest.NewRequest("POST", path, strings.NewReader(`{}`))
		r.Header.Set("Origin", "https://attacker.example")
		w := httptest.NewRecorder()
		f.handler.ServeHTTP(w, r)
		if w.Code != 403 {
			t.Fatalf("cross-origin %s: %d", path, w.Code)
		}
	}
}

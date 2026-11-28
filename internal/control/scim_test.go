package control

import (
	"bytes"
	"encoding/json"
	"golang.org/x/crypto/bcrypt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type scimFixture struct {
	*accountFixture
	client *http.Client
	origin string
	token  string
}

func newSCIMFixture(t *testing.T) *scimFixture {
	f := newAccountFixture(t)
	token := jsonString(t, f.call("root", "", "POST", "/api/settings/scim/token", map[string]any{"confirm": true, "version": 0}, 200), "token")
	f.call("root", "", "PUT", "/api/settings/scim", map[string]any{"enabled": true, "version": 1}, 200)
	server := httptest.NewTLSServer(f.handler)
	t.Cleanup(server.Close)
	return &scimFixture{accountFixture: f, client: server.Client(), origin: server.URL, token: token}
}

func (f *scimFixture) request(method, path string, input any, status int, headers ...map[string]string) (scimDocument, http.Header) {
	f.t.Helper()
	raw, err := json.Marshal(input)
	if err != nil {
		f.t.Fatal(err)
	}
	r, err := http.NewRequest(method, f.origin+path, bytes.NewReader(raw))
	if err != nil {
		f.t.Fatal(err)
	}
	r.Header.Set("Authorization", "Bearer "+f.token)
	r.Header.Set("Content-Type", "application/scim+json")
	for _, values := range headers {
		for key, value := range values {
			r.Header.Set(key, value)
		}
	}
	response, err := f.client.Do(r)
	if err != nil {
		f.t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		f.t.Fatal(err)
	}
	if response.StatusCode != status || !strings.HasPrefix(response.Header.Get("Content-Type"), "application/scim+json") {
		f.t.Fatalf("%s %s: got%d want%d: %s", method, path, response.StatusCode, status, body)
	}
	value := scimDocument{}
	if status != 204 && json.Unmarshal(body, &value) != nil {
		f.t.Fatal("SCIM response is not a JSON object")
	}
	if status >= 400 {
		if value["status"] == nil || value["schemas"] == nil {
			f.t.Fatal("SCIM error envelope missing", value)
		}
	}
	return value, response.Header
}

func scimFixtureUser(email, external string) scimDocument {
	return scimDocument{"schemas": []string{scimUserSchema}, "userName": email, "externalId": external, "active": true, "displayName": "Protocol fixture", "emails": []map[string]any{{"value": email, "primary": true, "type": "work"}}, "name": map[string]string{"givenName": "Protocol", "familyName": "Fixture"}}
}

func scimFixturePatch(path string, value any) scimDocument {
	return scimDocument{"schemas": []string{scimPatchSchema}, "Operations": []map[string]any{{"op": "Replace", "path": path, "value": value}}}
}

func TestSCIMUserLifecycleTokenRotationAndRevocation(t *testing.T) {
	f := newSCIMFixture(t)
	document := scimFixtureUser("person@example.invalid", "fixture-subject")
	document["password"], document["is_admin"], document["roles"] = "ignored-create-placeholder", true, []map[string]string{{"value": "Administrator"}}
	user, headers := f.request("POST", "/scim/v2/Users", document, 201)
	id := user["id"].(string)
	if headers.Get("ETag") != `W/"1"` || !strings.HasSuffix(headers.Get("Location"), "/Users/"+id) || user["password"] != nil || user["is_admin"] != nil {
		t.Fatal("unsafe or incomplete create response")
	}
	var admin bool
	var password []byte
	if err := f.s.DB.QueryRow(t.Context(), "SELECT is_admin,password_hash FROM users WHERE id=$1", id).Scan(&admin, &password); err != nil || admin || len(password) > 0 {
		t.Fatal("provisioning profile elevated privileges or set a password", err)
	}
	filter := url.QueryEscape(`userName eq "PERSON@example.invalid"`)
	listing, _ := f.request("GET", "/scim/v2/Users?filter="+filter+"&startIndex=1&count=100", nil, 200)
	if listing["totalResults"] != float64(1) || len(listing["Resources"].([]any)) != 1 {
		t.Fatal("userName filter failed", listing)
	}
	zero, _ := f.request("GET", "/scim/v2/Users?count=0", nil, 200)
	if zero["totalResults"] != float64(1) || len(zero["Resources"].([]any)) != 0 {
		t.Fatal("count=0 did not preserve total results", zero)
	}
	projected, _ := f.request("GET", "/scim/v2/Users/"+id+"?attributes=userName,name.givenName", nil, 200)
	if projected["id"] != id || projected["emails"] != nil || projected["name"].(map[string]any)["familyName"] != nil {
		t.Fatal("attribute projection failed", projected)
	}
	f.sql("INSERT INTO nodes(id,name,address,port,public_key,private_key) VALUES('token-node','Token fixture','unreachable.invalid',22,'',decode('00','hex'))")
	f.sql("INSERT INTO projects(id,name,team_id) VALUES('token-project','Token fixture','default')")
	f.sql("INSERT INTO workloads(id,node_id,name,configuration,project_id) VALUES('token-workload','token-node','Token fixture',decode('00','hex'),'token-project')")
	session, token := randomToken(), randomToken()
	f.sql("INSERT INTO sessions(token_hash,user_id,expires_at) VALUES($1,$2,now()+interval '1 hour')", tokenHash(session), id)
	f.sql("INSERT INTO api_tokens(id,name,token_hash,workload_id,permission,expires_at,user_id) VALUES('fixture','Fixture',$1,'token-workload','read',now()+interval '1 hour',$2)", tokenHash(token), id)
	inactive, _ := f.request("PATCH", "/scim/v2/Users/"+id, scimFixturePatch("active", "False"), 200, map[string]string{"If-Match": headers.Get("ETag")})
	if inactive["active"] != false {
		t.Fatal("deactivated user did not remain readable", inactive)
	}
	var sessions, tokens int
	if err := f.s.DB.QueryRow(t.Context(), "SELECT (SELECT count(*) FROM sessions WHERE user_id=$1),(SELECT count(*) FROM api_tokens WHERE user_id=$1)", id).Scan(&sessions, &tokens); err != nil || sessions != 0 || tokens != 0 {
		t.Fatal("deprovisioning retained access", sessions, tokens, err)
	}
	f.call(session, "default", "GET", "/api/session", nil, 401)
	f.request("PATCH", "/scim/v2/Users/"+id, scimFixturePatch("active", true), 200)
	f.call("root", "", "PUT", "/api/users/"+id, map[string]bool{"disabled": true}, 200)
	f.request("PATCH", "/scim/v2/Users/"+id, scimFixturePatch("active", true), 409)
	f.call("root", "", "PUT", "/api/users/"+id, map[string]bool{"disabled": false}, 200)
	f.request("DELETE", "/scim/v2/Users/"+id, nil, 204)
	f.request("GET", "/scim/v2/Users/"+id, nil, 404)
	user, _ = f.request("POST", "/scim/v2/Users", document, 201)
	if user["id"] != id || user["active"] != true {
		t.Fatal("same managed identity was not safely reprovisioned", user)
	}
	old := f.token
	f.token = jsonString(t, f.call("root", "", "POST", "/api/settings/scim/token", map[string]any{"version": 2, "confirm": true}, 200), "token")
	f.request("GET", "/scim/v2/Users", nil, 401, map[string]string{"Authorization": "Bearer " + old})
	f.request("GET", "/scim/v2/Users", nil, 200)
	var sealed []byte
	if err := f.s.DB.QueryRow(t.Context(), "SELECT configuration FROM scim_settings WHERE id=1").Scan(&sealed); err != nil || bytes.Contains(sealed, []byte(f.token)) {
		t.Fatal("provisioning token was not sealed", err)
	}
	if strings.Contains(f.call("root", "", "GET", "/api/settings/scim", nil, 200).Body.String(), f.token) {
		t.Fatal("settings exposed a provisioning token")
	}
}

func TestSCIMGroupsMappingsAndOwnerProtection(t *testing.T) {
	f := newSCIMFixture(t)
	u, _ := f.request("POST", "/scim/v2/Users", scimFixtureUser("member@example.invalid", "member-sub"), 201)
	id := u["id"].(string)
	g, _ := f.request("POST", "/scim/v2/Groups", scimDocument{"schemas": []string{scimGroupSchema}, "displayName": "Engineering", "externalId": "group-sub", "members": []any{}}, 201)
	group := g["id"].(string)
	f.call("root", "", "PUT", "/api/settings/scim/groups/"+group+"/mappings", map[string]any{"group_version": 1, "confirm": true, "mappings": []map[string]string{{"team_id": "default", "role": "editor"}}}, 200)
	f.call("root", "", "PUT", "/api/settings/scim/groups/"+group+"/mappings", map[string]any{"group_version": 1, "confirm": true, "mappings": []map[string]string{{"team_id": "default", "role": "owner"}}}, 409)
	patch := scimDocument{"schemas": []string{scimPatchSchema}, "Operations": []any{map[string]any{"op": "Add", "path": "members", "value": []any{map[string]string{"value": id}}}}}
	f.request("PATCH", "/scim/v2/Groups/"+group, patch, 204)
	f.request("PATCH", "/scim/v2/Groups/"+group, patch, 204)
	var grants []grant
	if err := f.s.DB.QueryRow(t.Context(), "SELECT grants FROM team_members WHERE team_id='default' AND user_id=$1", id).Scan(&grants); err != nil || len(grants) != 1 || grants[0].Role != "editor" {
		t.Fatal("SCIM group did not grant mapped access", grants, err)
	}
	g, h := f.request("GET", "/scim/v2/Groups/"+group+"?excludedAttributes=members", nil, 200)
	if g["members"] != nil {
		t.Fatal("excluded members leaked")
	}
	f.request("PUT", "/scim/v2/Groups/"+group, scimDocument{"schemas": []string{scimGroupSchema}, "displayName": "Changed"}, 412, map[string]string{"If-Match": `W/"1"`})
	f.call("root", "", "PUT", "/api/settings/scim/groups/"+group+"/mappings", map[string]any{"group_version": 4, "confirm": true, "mappings": []map[string]string{{"team_id": "default", "role": "owner"}}}, 200)
	_, h = f.request("GET", "/scim/v2/Groups/"+group, nil, 200)
	f.sql("DELETE FROM team_members WHERE team_id='default' AND user_id='bootstrap'")
	f.request("PATCH", "/scim/v2/Users/"+id, scimFixturePatch("active", false), 409)
	remove := scimDocument{"schemas": []string{scimPatchSchema}, "Operations": []any{map[string]any{"op": "Remove", "path": `members[value eq "` + id + `"]`}}}
	f.request("PATCH", "/scim/v2/Groups/"+group, remove, 204, map[string]string{"If-Match": h.Get("ETag")})
	var role string
	if err := f.s.DB.QueryRow(t.Context(), "SELECT role FROM team_members WHERE team_id='default' AND user_id=$1", id).Scan(&role); err != nil || role != "owner" {
		t.Fatal("last owner not retained", role, err)
	}
	f.request("DELETE", "/scim/v2/Groups/"+group, nil, 204)
	f.request("GET", "/scim/v2/Groups/"+group, nil, 404)
	f.sql("UPDATE users SET is_admin=true WHERE id=$1", id)
	f.request("DELETE", "/scim/v2/Users/"+id, nil, 409)
}

func TestSCIMPatchProjectionAndProtocolErrors(t *testing.T) {
	f := newSCIMFixture(t)
	doc := scimFixtureUser("profile@example.invalid", "profile-sub")
	doc["schemas"] = []string{scimUserSchema, scimEnterpriseSchema}
	doc[scimEnterpriseSchema] = map[string]any{"department": "Engineering", "manager": map[string]string{"value": "manager-id", "displayName": "Manager"}}
	doc["emails"] = []any{map[string]any{"value": "profile@example.invalid", "primary": true, "type": "work"}, map[string]any{"value": "other@example.invalid", "primary": false, "type": "home"}}
	u, h := f.request("POST", "/scim/v2/Users", doc, 201)
	id := u["id"].(string)
	u, _ = f.request("PATCH", "/scim/v2/Users/"+id, scimFixturePatch(`emails[type eq "home"].primary`, true), 200)
	if u["emails"].([]any)[0].(map[string]any)["primary"] != false {
		t.Fatal("previous primary retained")
	}
	projection := url.QueryEscape("emails.value,emails.type," + scimEnterpriseSchema + ":manager.value")
	u, _ = f.request("GET", "/scim/v2/Users/"+id+"?attributes="+projection, nil, 200)
	email := u["emails"].([]any)[0].(map[string]any)
	manager := u[scimEnterpriseSchema].(map[string]any)["manager"].(map[string]any)
	if email["value"] == nil || email["type"] == nil || email["primary"] != nil || manager["value"] != "manager-id" || manager["displayName"] != nil {
		t.Fatal("projection mixed or lost fields", u)
	}
	f.request("PATCH", "/scim/v2/Users/"+id, scimFixturePatch("displayName", "Stale"), 412, map[string]string{"If-Match": h.Get("ETag")})
	atomic := scimDocument{"schemas": []string{scimPatchSchema}, "Operations": []any{map[string]any{"op": "Replace", "path": "displayName", "value": "Must roll back"}, map[string]any{"op": "Replace", "path": "password", "value": "not-supported"}}}
	f.request("PATCH", "/scim/v2/Users/"+id, atomic, 400)
	u, _ = f.request("GET", "/scim/v2/Users/"+id, nil, 200)
	if u["displayName"] == "Must roll back" {
		t.Fatal("partial PATCH committed")
	}
	for _, filter := range []string{`userName eq "' OR true --"`, `(userName sw "profile" and active eq true) or externalId eq "absent"`, `emails.value ew "example.invalid"`} {
		f.request("GET", "/scim/v2/Users?filter="+url.QueryEscape(filter), nil, 200)
	}
	unequal, _ := f.request("GET", "/scim/v2/Users?filter="+url.QueryEscape(`emails.value ne "profile@example.invalid"`), nil, 200)
	if unequal["totalResults"] != float64(1) {
		t.Fatal("multivalue filter did not match an unequal value")
	}
	u, _ = f.request("PATCH", "/scim/v2/Users/"+id, scimFixturePatch(`emails[primary eq true].primary`, true), 200)
	if len(u["emails"].([]any)) != 2 {
		t.Fatal("primary selector corrupted the multivalue attribute")
	}
	for _, filter := range []string{`password eq "secret"`, `userName eq`, `active eq "wrong"`, strings.Repeat("(", 12) + `active eq true` + strings.Repeat(")", 12)} {
		f.request("GET", "/scim/v2/Users?filter="+url.QueryEscape(filter), nil, 400)
	}
	f.request("GET", "/scim/v2/Users", nil, 401, map[string]string{"Authorization": "Bearer " + f.s.AdminToken})
	f.request("GET", "/scim/v2/ServiceProviderConfig", nil, 200)
	f.request("GET", "/scim/v2/Schemas", nil, 200)
	f.request("GET", "/scim/v2/ResourceTypes", nil, 200)
	f.request("POST", "/scim/v2/Users", scimDocument{"schemas": []string{scimUserSchema}, "userName": "x", "UserName": "y"}, 400)
}

func TestSCIMDocumentedIdentityProviderRequests(t *testing.T) {
	for _, provider := range []string{"okta", "entra", "authentik"} {
		t.Run(provider, func(t *testing.T) {
			f := newSCIMFixture(t)
			raw, err := os.ReadFile(filepath.Join("testdata", "scim", provider+".json"))
			if err != nil {
				t.Fatal(err)
			}
			var fixture struct {
				Source, Evidence string
				Requests         []struct {
					Method, Path string
					Status       int
					Body         json.RawMessage
				}
			}
			if err = json.Unmarshal(raw, &fixture); err != nil || fixture.Source == "" || fixture.Evidence == "" {
				t.Fatal("request provenance missing", err)
			}
			user, group := "", ""
			for _, request := range fixture.Requests {
				replace := strings.NewReplacer("$user", user, "$group", group)
				var body any
				if len(request.Body) > 0 {
					if err = json.Unmarshal([]byte(replace.Replace(string(request.Body))), &body); err != nil {
						t.Fatal(err)
					}
				}
				result, _ := f.request(request.Method, "/scim/v2"+replace.Replace(request.Path), body, request.Status)
				if request.Method == "POST" && request.Path == "/Users" {
					user = result["id"].(string)
				}
				if request.Method == "POST" && request.Path == "/Groups" {
					group = result["id"].(string)
				}
			}
		})
	}
}

func TestSCIMExplicitAccountAdoptionAndPinnedOIDC(t *testing.T) {
	f := newSCIMFixture(t)
	provider := jsonString(t, f.call("root", "", "POST", "/api/settings/oidc/providers", map[string]any{"display_name": "Provisioning fixture", "issuer": "https://identity.example.invalid", "client_id": "fixture-client", "client_secret": "synthetic-fixture"}, 200), "id")
	f.call("root", "", "PUT", "/api/settings/scim", map[string]any{"enabled": true, "version": 2, "oidc_provider_id": provider}, 200)
	u, _ := f.request("POST", "/scim/v2/Users", scimFixtureUser("provisioned@example.invalid", "stable-subject"), 201)
	id := u["id"].(string)
	var linked bool
	if err := f.s.DB.QueryRow(t.Context(), "SELECT EXISTS(SELECT 1 FROM oidc_identities WHERE provider_id=$1 AND subject='stable-subject' AND user_id=$2)", provider, id).Scan(&linked); err != nil || !linked {
		t.Fatal("explicit provider policy did not pin the subject", err)
	}
	f.request("PATCH", "/scim/v2/Users/"+id, scimFixturePatch("externalId", "different-subject"), 400)
	session := f.user("existing", "existing@example.invalid")
	hash, _ := bcrypt.GenerateFromPassword([]byte("adoption-local-password"), bcrypt.MinCost)
	f.sql("UPDATE users SET password_hash=$1 WHERE id='existing'", hash)
	f.request("POST", "/scim/v2/Users", scimFixtureUser("existing@example.invalid", "existing-subject"), 409)
	f.call("root", "", "POST", "/api/settings/scim/links", map[string]any{"user_id": "existing", "external_id": "existing-subject", "confirm": true}, 200)
	links := f.call("root", "", "GET", "/api/settings/scim/links", nil, 200).Body.String()
	if !strings.Contains(links, "existing-subject") || strings.Contains(links, f.token) {
		t.Fatal("provisioning links omitted provenance or exposed a token")
	}
	f.call(session, "", "GET", "/api/settings/scim/links", nil, 403)
	f.call("root", "", "DELETE", "/api/settings/oidc/providers/"+provider, nil, 409)
	f.call(session, "", "DELETE", "/api/account/identities/"+provider, map[string]string{"current_password": "adoption-local-password"}, 409)
	f.call("root", "", "DELETE", "/api/settings/scim/links/existing", map[string]bool{"confirm": true}, 200)
	f.call(session, "", "DELETE", "/api/account/identities/"+provider, map[string]string{"current_password": "adoption-local-password"}, 200)
}

func TestSCIMRotatedTokenCannotCommitAnInflightMutation(t *testing.T) {
	f := newSCIMFixture(t)
	authenticated, release := make(chan struct{}), make(chan struct{})
	server := httptest.NewTLSServer(f.s.scimAuthenticated(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { close(authenticated); <-release; f.s.scimResources(w, r) })))
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
		server.Close()
	}()
	raw, _ := json.Marshal(scimFixtureUser("inflight@example.invalid", "inflight"))
	request, _ := http.NewRequest("POST", server.URL+"/scim/v2/Users", bytes.NewReader(raw))
	request.Header.Set("Authorization", "Bearer "+f.token)
	request.Header.Set("Content-Type", "application/scim+json")
	result := make(chan int, 1)
	go func() {
		response, err := server.Client().Do(request)
		if err != nil {
			result <- 0
			return
		}
		defer response.Body.Close()
		io.Copy(io.Discard, response.Body)
		result <- response.StatusCode
	}()
	<-authenticated
	f.call("root", "", "POST", "/api/settings/scim/token", map[string]any{"version": 2, "confirm": true}, 200)
	close(release)
	if status := <-result; status != 401 {
		t.Fatal("old authenticated token committed after rotation", status)
	}
	var count int
	if err := f.s.DB.QueryRow(t.Context(), "SELECT count(*) FROM users WHERE email='inflight@example.invalid'").Scan(&count); err != nil || count != 0 {
		t.Fatal("inflight mutation was not rolled back", count, err)
	}
}

func TestSCIMIdenticalGroupReplacementAndDisabledMembership(t *testing.T) {
	f := newSCIMFixture(t)
	u, _ := f.request("POST", "/scim/v2/Users", scimFixtureUser("inactive@example.invalid", "inactive-member"), 201)
	user := u["id"].(string)
	members := []map[string]string{{"value": user}}
	g, _ := f.request("POST", "/scim/v2/Groups", scimDocument{"schemas": []string{scimGroupSchema}, "displayName": "Inactive membership", "members": members}, 201)
	group := g["id"].(string)
	f.call("root", "", "PUT", "/api/settings/scim/groups/"+group+"/mappings", map[string]any{"group_version": 1, "confirm": true, "mappings": []map[string]string{{"team_id": "default", "role": "editor"}}}, 200)
	_, headers := f.request("PATCH", "/scim/v2/Groups/"+group, scimFixturePatch("members", members), 204)
	if headers.Get("ETag") != `W/"3"` {
		t.Fatal("identical Group replacement did not advance the revision")
	}
	f.call("root", "", "PUT", "/api/settings/scim/groups/"+group+"/mappings", map[string]any{"group_version": 2, "confirm": true, "mappings": []directoryMapping{}}, 409)
	f.request("PATCH", "/scim/v2/Users/"+user, scimFixturePatch("active", false), 200)
	if strings.Contains(f.call("root", "default", "GET", "/api/teams/default/members", nil, 200).Body.String(), user) {
		t.Fatal("deactivation retained directory-only team authorization")
	}
	u, _ = f.request("GET", "/scim/v2/Users/"+user, nil, 200)
	if u["active"] != false {
		t.Fatal("deactivated user missing from provisioning")
	}
	g, _ = f.request("GET", "/scim/v2/Groups/"+group, nil, 200)
	if len(g["members"].([]any)) != 1 {
		t.Fatal("deactivation discarded the IdP's group relationship")
	}
}

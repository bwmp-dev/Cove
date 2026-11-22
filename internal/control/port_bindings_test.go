package control

import (
	"strings"
	"testing"
)

func TestRewritePortBindings(t *testing.T) {
	compose := `services:
  app:
    image: nginx
    ports:
      - "8080:80"
      - "127.0.0.1:8081:80"
      - "[::]:8082:80"
      - target: 81
        published: "8083"
      - target: 82
        published: "8084"
        host_ip: 192.168.1.1
`
	got, count, err := rewritePortBindings(compose, "127.0.0.1")
	if err != nil || count != 3 || !strings.Contains(got, "127.0.0.1:8080:80") || !strings.Contains(got, "127.0.0.1:8082:80") || !strings.Contains(got, "host_ip: 192.168.1.1") {
		t.Fatalf("count=%d err=%v compose=%s", count, err, got)
	}
	again, count, err := rewritePortBindings(got, "127.0.0.1")
	if err != nil || count != 0 || again != got {
		t.Fatalf("rewrite not idempotent: count=%d err=%v", count, err)
	}
	for _, address := range []string{"0.0.0.0", "::", "invalid", "224.0.0.1"} {
		if _, _, err := rewritePortBindings(compose, address); err == nil {
			t.Fatal(address)
		}
	}
	if _, _, err := rewritePortBindings("services:\n  app:\n    ports: [\"${PORT}:80\"]", "127.0.0.1"); err == nil {
		t.Fatal("interpolated port accepted")
	}
	if _, _, err := rewritePortBindings("services:\n  app: &app\n    ports: [\"8080:80\"]\n  other: *app", "127.0.0.1"); err == nil {
		t.Fatal("shared alias accepted")
	}
}

func TestSecurityPostureIntegration(t *testing.T) {
	f := newAccountFixture(t)
	member := f.user("member", "member@example.com")
	f.call(member, "", "GET", "/api/settings/security-posture", nil, 403)
	w := f.call("root", "", "GET", "/api/settings/security-posture", nil, 200)
	if !strings.Contains(w.Body.String(), `"id":"admin_second_factor_required","status":"warn"`) {
		t.Fatal(w.Body.String())
	}
	f.sql("UPDATE control_settings SET require_second_factor=true WHERE id=1")
	w = f.call("root", "", "GET", "/api/settings/security-posture", nil, 200)
	if !strings.Contains(w.Body.String(), `"id":"admin_second_factor_required","status":"pass"`) {
		t.Fatal(w.Body.String())
	}
}

func TestPortBindingsEditorIntegration(t *testing.T) {
	f := newAccountFixture(t)
	editor := f.user("editor", "editor@example.com")
	viewer := f.user("viewer", "viewer@example.com")
	f.sql(`INSERT INTO team_members(team_id,user_id,role,grants) VALUES('default','editor','member','[{"scope":"team","role":"editor"}]'),('default','viewer','member','[{"scope":"team","role":"viewer"}]')`)
	f.sql("INSERT INTO nodes(id,name,address,port,public_key,private_key,state) VALUES('ports-node','Node','localhost',22,'',$1,'ready')", []byte{})
	f.sql("INSERT INTO projects(id,name,team_id) VALUES('ports-project','Project','default')")
	c := workloadInput{Name: "Ports", NodeID: "ports-node", ProjectID: "ports-project", Compose: "services:\n  app:\n    image: nginx:alpine\n    ports: [\"8080:80\"]\n"}
	id := jsonString(t, f.call("root", "default", "POST", "/api/workloads", c, 201), "id")
	path := "/api/workloads/" + id + "/port-bindings"
	f.call(viewer, "default", "POST", path, map[string]any{"address": "127.0.0.1"}, 403)
	w := f.call(editor, "default", "POST", path, map[string]any{"address": "127.0.0.1"}, 200)
	revision := jsonString(t, w, "revision")
	f.call(editor, "default", "POST", path, map[string]any{"address": "127.0.0.1", "confirm": true, "revision": "stale"}, 409)
	f.call(editor, "default", "POST", path, map[string]any{"address": "127.0.0.1", "confirm": true, "revision": revision}, 200)
	w = f.call(editor, "default", "GET", "/api/workloads/"+id, nil, 200)
	if !strings.Contains(w.Body.String(), "127.0.0.1:8080:80") {
		t.Fatal(w.Body.String())
	}
}

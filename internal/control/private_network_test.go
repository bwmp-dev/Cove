package control

import (
	"context"
	"cove/internal/worker"
	"encoding/json"
	"strings"
	"testing"
)

func TestPrivateNetworkAuthorizationAndSecrets(t *testing.T) {
	f := newAccountFixture(t)
	editor := f.user("editor", "editor@example.com")
	viewer := f.user("viewer", "viewer@example.com")
	outsider := f.user("outsider", "outsider@example.com")
	f.sql("INSERT INTO teams(id,name) VALUES('a','A'),('b','B')")
	f.sql(`INSERT INTO team_members(team_id,user_id,role,grants) VALUES('a','editor','owner','[]'),('a','viewer','member','[{"scope":"team","role":"viewer"}]'),('b','outsider','owner','[]')`)
	f.sql("INSERT INTO nodes(id,name,address,port,public_key,private_key,state) VALUES('n','N','127.0.0.1',22,'',$1,'ready')", []byte{})
	f.sql("INSERT INTO projects(id,name,team_id) VALUES('p','P','a')")
	raw, _ := json.Marshal(workloadInput{Name: "App", NodeID: "n", Kind: "service", Compose: "services:\n  app:\n    image: busybox:1.37\n"})
	encrypted, err := seal(f.s.Key, raw)
	if err != nil {
		t.Fatal(err)
	}
	f.sql("INSERT INTO workloads(id,node_id,name,configuration,team_id,project_id) VALUES('w','n','App',$1,'a','p')", encrypted)
	path := "/api/workloads/w/private-network"
	value := map[string]any{"private": &worker.PrivateNetwork{Service: "app", Hostname: "private-app", Port: 3000, AuthKey: "tskey-auth-test-secret"}}
	f.call(viewer, "a", "PUT", path, value, 403)
	f.call(outsider, "b", "GET", path, nil, 404)
	f.call(editor, "a", "PUT", path, value, 409)
	f.sql(`UPDATE nodes SET capabilities='{"private_networks":true}' WHERE id='n'`)
	f.call(editor, "a", "PUT", path, value, 200)
	got := f.call(viewer, "a", "GET", path, nil, 200)
	if strings.Contains(got.Body.String(), "tskey") {
		t.Fatal("auth key leaked")
	}
	value["private"] = &worker.PrivateNetwork{Service: "app", Hostname: "private-app", Port: 8080}
	f.call(editor, "a", "PUT", path, value, 200)
	var sealed []byte
	if err := f.s.DB.QueryRow(context.Background(), "SELECT configuration FROM workloads WHERE id='w'").Scan(&sealed); err != nil {
		t.Fatal(err)
	}
	decrypted, err := unseal(f.s.Key, sealed)
	if err != nil {
		t.Fatal(err)
	}
	var config workloadInput
	if err = json.Unmarshal(decrypted, &config); err != nil {
		t.Fatal(err)
	}
	if config.Private.AuthKey != "tskey-auth-test-secret" {
		t.Fatal("blank key did not preserve secret")
	}
	f.call(editor, "a", "PUT", "/api/workloads/w/routes", map[string]any{"routes": []worker.Route{{Domain: "private.example.com", Service: "app", Port: 8080}}}, 409)
	f.sql("INSERT INTO operations(id,workload_id,action,payload) VALUES('active','w','deploy',$1)", []byte{})
	f.call(editor, "a", "PUT", path, map[string]any{"private": nil}, 409)
	f.sql("UPDATE operations SET state='cancelled' WHERE id='active'")
	f.call(editor, "a", "PUT", path, map[string]any{"private": nil}, 200)
}

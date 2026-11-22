package control

import (
	"context"
	"cove/internal/worker"
	"encoding/json"
	"testing"
)

func TestSharedNetworkAuthorization(t *testing.T) {
	f := newAccountFixture(t)
	owner := f.user("net-owner", "owner@example.com")
	editor := f.user("net-editor", "editor@example.com")
	f.sql("INSERT INTO teams(id,name) VALUES('other','Other')")
	f.sql("INSERT INTO team_members(team_id,user_id,role,grants) VALUES('default','net-owner','owner','[]'),('default','net-editor','member','[{\"scope\":\"team\",\"role\":\"editor\"}]')")
	f.sql("INSERT INTO nodes(id,name,address,port,public_key,private_key,state) VALUES('n','One','host',22,'',$1,'ready'),('n2','Two','host',22,'',$1,'ready')", []byte{})
	f.sql("INSERT INTO team_nodes(team_id,node_id) VALUES('default','n'),('default','n2')")
	f.sql("INSERT INTO projects(id,name,team_id) VALUES('p','Project','default')")
	f.sql("INSERT INTO environments(id,project_id,name,team_id) VALUES('e1','p','Production','default'),('e2','p','Staging','default')")
	config, _ := seal(f.s.Key, []byte(`{"name":"App","node_id":"n","kind":"service","compose":"services:\n  app:\n    image: busybox:1.37\n"}`))
	f.sql("INSERT INTO workloads(id,name,node_id,configuration,team_id,environment_id,project_id) VALUES('w1','App','n',$1,'default','e1','p'),('w2','App','n',$1,'default','e2','p'),('w3','App','n2',$1,'default','e2','p')", config)
	body := map[string]string{"name": "Shared", "node_id": "n"}
	f.call(editor, "default", "POST", "/api/networks", body, 403)
	network := jsonString(t, f.call(owner, "default", "POST", "/api/networks", body, 201), "id")
	put := func(user, id, alias string, status int) {
		f.call(user, "default", "PUT", "/api/workloads/"+id+"/networks", map[string]any{"networks": []worker.NetworkAttachment{{ID: network, Service: "app", Alias: alias}}}, status)
	}
	put(editor, "w1", "db", 403)
	put(owner, "w1", "database", 200)
	put(owner, "w2", "application", 200)
	put(owner, "w2", "database", 409)
	put(owner, "w3", "remote", 400)
	f.call("root", "other", "PUT", "/api/workloads/w1/networks", map[string]any{"networks": []any{}}, 404)
	var got []worker.NetworkAttachment
	if err := json.Unmarshal(f.call(owner, "default", "GET", "/api/workloads/w2/networks", nil, 200).Body.Bytes(), &got); err != nil || len(got) != 1 || got[0].Alias != "application" {
		t.Fatal("failed update did not preserve connections", err)
	}
	f.call(owner, "default", "POST", "/api/workloads/w1/operations", map[string]string{"action": "deploy"}, 409)
	f.sql("UPDATE nodes SET capabilities='{\"shared_networks\":true}' WHERE id='n'")
	op := jsonString(t, f.call(owner, "default", "POST", "/api/workloads/w1/operations", map[string]string{"action": "deploy"}, 202), "id")
	put(owner, "w1", "changed", 409)
	var encrypted []byte
	if err := f.s.DB.QueryRow(context.Background(), "SELECT payload FROM operations WHERE id=$1", op).Scan(&encrypted); err != nil {
		t.Fatal(err)
	}
	raw, err := unseal(f.s.Key, encrypted)
	if err != nil {
		t.Fatal(err)
	}
	var request worker.Request
	if err = json.Unmarshal(raw, &request); err != nil || len(request.Networks) != 1 || request.Networks[0].Alias != "database" {
		t.Fatal("queued network configuration lost", err)
	}
	clone := jsonString(t, f.call(owner, "default", "POST", "/api/workloads/w2/clone", map[string]string{"name": "Copy"}, 201), "id")
	if body := f.call(owner, "default", "GET", "/api/workloads/"+clone+"/networks", nil, 200).Body.String(); body != "[]\n" {
		t.Fatal("clone retained connections", body)
	}
	webhookID := resourceID()
	data, err := json.Marshal(worker.Request{ID: webhookID, Workload: "w2", Action: "deploy"})
	if err != nil {
		t.Fatal(err)
	}
	payload, err := seal(f.s.Key, data)
	if err != nil {
		t.Fatal(err)
	}
	f.sql("UPDATE workloads SET webhook_secret=$1 WHERE id='w2'", []byte("enabled"))
	f.sql("INSERT INTO webhook_deliveries(workload_id,delivery,payload) VALUES('w2','network-test',$1)", payload)
	if err = f.s.dispatchWebhook(context.Background(), "w2"); err != nil {
		t.Fatal(err)
	}
	if err = f.s.DB.QueryRow(context.Background(), "SELECT payload FROM operations WHERE id=$1", webhookID).Scan(&encrypted); err != nil {
		t.Fatal(err)
	}
	raw, err = unseal(f.s.Key, encrypted)
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(raw, &request); err != nil || len(request.Networks) != 1 || request.Networks[0].Alias != "application" {
		t.Fatal("webhook lost network connections", err)
	}
}

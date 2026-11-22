package control

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"cove/internal/worker"
	"golang.org/x/crypto/ssh"
)

func TestGitDeployKeysIsolationAndSealing(t *testing.T) {
	f := newAccountFixture(t)
	ctx := context.Background()
	f.sql(`INSERT INTO nodes(id,name,address,port,public_key,private_key,state) VALUES('n','Node','localhost',22,'',$1,'ready')`, []byte{})
	f.sql(`INSERT INTO projects(id,name,team_id) VALUES('p','Project','default')`)
	config := workloadInput{Name: "App", NodeID: "n", ProjectID: "p", Source: &worker.GitSource{Repository: "https://git.example.com/app.git", Branch: "main", ComposePath: "compose.yaml"}}
	id := jsonString(t, f.call("root", "default", "POST", "/api/workloads", config, 201), "id")
	path := "/api/workloads/" + id + "/git-deploy-key"
	f.call("root", "default", "GET", path, nil, 200)
	viewer := f.user("key-viewer", "key-viewer@example.com")
	f.sql(`INSERT INTO team_members(team_id,user_id,role,grants) VALUES('default','key-viewer','member','[{"scope":"team","role":"viewer"}]')`)
	host, _, err := keyPair()
	if err != nil {
		t.Fatal(err)
	}
	input := map[string]string{"repository": "ssh://git@git.example.com/app.git", "host_key": host}
	f.call(viewer, "default", "POST", path, input, 403)
	invalid := map[string]string{"repository": input["repository"], "host_key": ""}
	f.call("root", "default", "POST", path, invalid, 400)
	created := f.call("root", "default", "POST", path, input, 201)
	public := jsonString(t, created, "public_key")
	if _, _, _, _, err = ssh.ParseAuthorizedKey([]byte(public)); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(created.Body.String(), "PRIVATE") || strings.Contains(created.Body.String(), "private_key") {
		t.Fatal("private key returned")
	}
	f.call(viewer, "default", "GET", path, nil, 200)
	var encrypted []byte
	if err = f.s.DB.QueryRow(ctx, `SELECT private_key FROM git_deploy_keys WHERE workload_id=$1`, id).Scan(&encrypted); err != nil {
		t.Fatal(err)
	}
	if len(encrypted) <= 32 {
		t.Fatal("private key not sealed")
	}
	response := f.call("root", "default", "GET", "/api/workloads/"+id, nil, 200)
	var workload struct {
		Source *worker.GitSource `json:"source"`
	}
	if err = json.Unmarshal(response.Body.Bytes(), &workload); err != nil {
		t.Fatal(err)
	}
	request := worker.Request{ID: "ssh-op", Workload: id, Action: "deploy", Source: workload.Source}
	payload, err := f.s.sealPreview(request)
	if err != nil {
		t.Fatal(err)
	}
	f.sql(`INSERT INTO operations(id,workload_id,action,payload) VALUES('ssh-op',$1,'deploy',$2)`, id, payload)
	f.call("root", "default", "POST", path, input, 409)
	f.call("root", "default", "DELETE", path, nil, 409)
	if err = f.s.prepareAppDeployment(ctx, &request); err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.ParsePrivateKey([]byte(request.Source.SSHPrivateKey))
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(ssh.MarshalAuthorizedKey(signer.PublicKey()))) != public {
		t.Fatal("wrong workload key injected")
	}
	saved := request.Source.SSHPrivateKey
	if err = f.s.prepareAppDeployment(ctx, &request); err != nil || request.Source.SSHPrivateKey != saved {
		t.Fatal("retry changed identity")
	}
	masked := f.call("root", "default", "GET", "/api/workloads/"+id, nil, 200).Body.String()
	if strings.Contains(masked, "PRIVATE") || strings.Contains(masked, "ssh_private_key") {
		t.Fatal("source read exposed deployment credentials")
	}
	copy := *request.Source
	copy.SSHPrivateKey = ""
	copy.Repository = "ssh://git@other.example.com/app.git"
	other := request
	other.Source = &copy
	if err = f.s.prepareGitDeployKey(ctx, &other); err == nil {
		t.Fatal("key used for another repository")
	}
	f.sql(`UPDATE operations SET state='succeeded' WHERE id='ssh-op'`)
	rotated := f.call("root", "default", "POST", path, input, 201)
	if jsonString(t, rotated, "public_key") == public {
		t.Fatal("rotation reused key")
	}
	f.call("root", "default", "DELETE", path, nil, 200)
	copy = *workload.Source
	copy.SSHPrivateKey = ""
	request.Source = &copy
	if err = f.s.prepareGitDeployKey(ctx, &request); err == nil {
		t.Fatal("deleted key still available")
	}
}

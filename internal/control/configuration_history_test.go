package control

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestConfigurationHistoryRedactsValuesAndRetainsBoundedRevisions(t *testing.T) {
	f := newAccountFixture(t)
	f.sql("INSERT INTO nodes(id,name,address,port,public_key,private_key,state) VALUES('history-node','Node','localhost',22,'',$1,'ready')", []byte{})
	f.sql("INSERT INTO projects(id,name,team_id) VALUES('history-project','Project','default')")
	config := workloadInput{Name: "History", NodeID: "history-node", ProjectID: "history-project", Compose: "services:\n  app:\n    image: busybox:1.37\n", Environment: map[string]string{"SECRET": "original-secret"}}
	id := jsonString(t, f.call("root", "default", "POST", "/api/workloads", config, 201), "id")
	config.ProjectID = ""
	config.Environment["SECRET"] = "replacement-secret"
	f.call("root", "default", "PUT", "/api/workloads/"+id, config, 200)
	response := f.call("root", "default", "GET", "/api/workloads/"+id+"/configuration-history", nil, 200)
	if strings.Contains(response.Body.String(), "original-secret") || strings.Contains(response.Body.String(), "replacement-secret") || !strings.Contains(response.Body.String(), "environment.SECRET") {
		t.Fatal("incorrect redacted history", response.Body.String())
	}
	outsider := f.user("history-outsider", "history-outsider@example.com")
	f.sql("INSERT INTO teams(id,name) VALUES('history-other','Other')")
	f.sql("INSERT INTO team_members(team_id,user_id,role,grants) VALUES('history-other','history-outsider','owner','[]')")
	f.call(outsider, "history-other", "GET", "/api/workloads/"+id+"/configuration-history", nil, 404)
	for i := 0; i < 102; i++ {
		raw, err := json.Marshal(map[string]any{"name": "History", "sequence": i})
		if err != nil {
			t.Fatal(err)
		}
		encrypted, err := seal(f.s.Key, raw)
		if err != nil {
			t.Fatal(err)
		}
		f.sql("UPDATE workloads SET configuration=$2 WHERE id=$1", id, encrypted)
	}
	var count int
	if err := f.s.DB.QueryRow(context.Background(), "SELECT count(*) FROM configuration_history WHERE workload_id=$1", id).Scan(&count); err != nil || count != 100 {
		t.Fatal(count, err)
	}
}

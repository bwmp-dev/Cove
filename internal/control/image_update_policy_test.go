package control

import (
	"context"
	"cove/internal/worker"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestImageUpdateDeploymentWindowMessage(t *testing.T) {
	f := newAccountFixture(t)
	f.sql(`INSERT INTO nodes(id,name,address,port,public_key,private_key,state) VALUES('window-message-node','Node','localhost',22,'',$1,'ready')`, []byte{})
	f.sql(`INSERT INTO projects(id,name,team_id) VALUES('window-message-project','Project','default')`)
	id := jsonString(t, f.call("root", "default", "POST", "/api/workloads", workloadInput{Name: "App", NodeID: "window-message-node", ProjectID: "window-message-project", Compose: "services: {}"}, 201), "id")
	f.sql(`INSERT INTO operations(id,workload_id,action,payload) VALUES('window-message-op',$1,'deploy',$2)`, id, []byte{})
	now := time.Now().UTC()
	policy := deploymentPolicy{WindowStart: now.Add(time.Hour).Format("15:04"), WindowEnd: now.Add(2 * time.Hour).Format("15:04")}
	f.call("root", "default", "PUT", "/api/projects/window-message-project/deployment-policy", policy, 200)
	tx, err := f.s.DB.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	message, err := deploymentGate(context.Background(), tx, "window-message-op")
	want := "Waiting for an allowed deployment window in UTC."
	if err != nil || message != want {
		t.Fatalf("deployment window message %q, %v; want %q", message, err, want)
	}
}

func TestImageUpdatePolicyAccessAndValidation(t *testing.T) {
	f := newAccountFixture(t)
	f.sql(`INSERT INTO nodes(id,name,address,port,public_key,private_key,state,capabilities) VALUES('updates-node','Node','localhost',22,'',$1,'ready','{"image_updates":true,"image_lifecycle":true}')`, []byte{})
	f.sql(`INSERT INTO projects(id,name,team_id) VALUES('updates-project','Project','default')`)
	id := jsonString(t, f.call("root", "default", "POST", "/api/workloads", workloadInput{Name: "App", NodeID: "updates-node", ProjectID: "updates-project", Kind: "service", Compose: "services:\n  app:\n    image: busybox:1.37.0\n"}, 201), "id")
	path := "/api/workloads/" + id + "/image-update-policy"
	p := worker.ImageUpdatePolicy{Enabled: true, Mode: "notify", Rules: []worker.ImageUpdateRule{{Service: "app", Range: "^1.37.0"}}}
	viewer := f.user("updates-viewer", "updates-viewer@example.com")
	f.sql(`INSERT INTO team_members(team_id,user_id,role,grants) VALUES('default','updates-viewer','member','[{"scope":"project","scope_id":"updates-project","role":"viewer"}]')`)
	f.call(viewer, "default", "GET", path, nil, 200)
	f.call(viewer, "default", "PUT", path, p, 403)
	f.call("root", "default", "PUT", path, p, 200)
	var status struct {
		Configuration *worker.ImageUpdatePolicy `json:"configuration"`
	}
	if err := json.Unmarshal(f.call(viewer, "default", "GET", path, nil, 200).Body.Bytes(), &status); err != nil || status.Configuration == nil || status.Configuration.Rules[0].Range != "^1.37.0" {
		t.Fatal(status, err)
	}
	p.WindowStart = "23:00"
	p.WindowEnd = "02:00"
	f.call("root", "default", "PUT", path, p, 200)
	p.WindowEnd = p.WindowStart
	f.call("root", "default", "PUT", path, p, 400)
	p.WindowEnd = "02:00"
	p.Rules[0].Service = "missing"
	f.call("root", "default", "PUT", path, p, 400)
	p.Rules[0].Service = "app"
	f.sql(`UPDATE nodes SET capabilities='{}' WHERE id='updates-node'`)
	f.call("root", "default", "PUT", path, p, 409)
	p.Enabled = false
	f.call("root", "default", "PUT", path, p, 200)
}

func TestImageUpdateDispatchAndAdmission(t *testing.T) {
	for _, mode := range []string{"notify", "auto"} {
		t.Run(mode, func(t *testing.T) {
			f := newRegistryHookFixture(t, "distribution", "registry.example.test")
			f.sql(`UPDATE nodes SET capabilities=capabilities || '{"image_updates":true}'::jsonb WHERE id='node'`)
			policy := worker.ImageUpdatePolicy{Enabled: true, Mode: mode, Rules: []worker.ImageUpdateRule{{Service: "app"}}}
			f.call("root", "default", "PUT", f.path+"/image-update-policy", policy, 200)
			ctx := context.Background()
			tx, err := f.s.DB.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			request := worker.Request{ID: "update-check", Workload: f.id, Kind: "service", Action: "image_check"}
			if err = f.s.prepareImageUpdateCheck(ctx, tx, &request); err != nil {
				t.Fatal(err)
			}
			if err = tx.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			payload, err := f.s.sealJSON(request)
			if err != nil {
				t.Fatal(err)
			}
			target := "registry.example.test/team/app:stable@sha256:" + strings.Repeat("a", 64)
			result, _ := json.Marshal(worker.Operation{ImageCheck: &worker.ImageCheck{Updates: &worker.ImageUpdateResult{State: "available", Key: strings.Repeat("b", 64), Candidates: []worker.ImageUpdateCandidate{{Service: "app", Current: "registry.example.test/team/app:stable", Target: target}}, Pins: map[string]string{"app": target}}}})
			f.sql(`INSERT INTO operations(id,workload_id,action,state,payload,result) VALUES($1,$2,'image_check','succeeded',$3,$4)`, request.ID, f.id, payload, result)
			if err = f.s.dispatchImageUpdates(ctx); err != nil {
				t.Fatal(err)
			}
			var deploy *string
			if err = f.s.DB.QueryRow(ctx, `SELECT last_operation_id FROM image_update_policies WHERE workload_id=$1`, f.id).Scan(&deploy); err != nil {
				t.Fatal(err)
			}
			var observed struct {
				Updates *worker.ImageUpdateResult `json:"updates"`
			}
			if err = json.Unmarshal(f.call("root", "default", "GET", f.path+"/image-update-policy", nil, 200).Body.Bytes(), &observed); err != nil || observed.Updates == nil || observed.Updates.State != "available" {
				t.Fatal("candidate status missing", observed, err)
			}
			if mode == "notify" {
				if deploy != nil {
					t.Fatal("notify mode deployed")
				}
				var count int
				f.s.DB.QueryRow(ctx, `SELECT count(*) FROM image_update_events`).Scan(&count)
				if count != 1 {
					t.Fatal("missing update event", count)
				}
			} else {
				if deploy == nil {
					t.Fatal("auto mode queued no deployment")
				}
				var encrypted []byte
				if err = f.s.DB.QueryRow(ctx, `SELECT payload FROM operations WHERE id=$1`, *deploy).Scan(&encrypted); err != nil {
					t.Fatal(err)
				}
				plain, _ := unseal(f.s.Key, encrypted)
				var queued worker.Request
				json.Unmarshal(plain, &queued)
				if queued.PullLatest || queued.ImagePins["app"] != target {
					t.Fatal("candidate was not pinned", queued)
				}
				start := time.Now().UTC().Add(time.Hour).Format("15:04")
				end := time.Now().UTC().Add(2 * time.Hour).Format("15:04")
				f.sql(`UPDATE image_update_deployments SET window_start=$2,window_end=$3 WHERE operation_id=$1`, *deploy, start, end)
				admitted, err := f.s.admitDeployment(ctx, *deploy)
				if err != nil || admitted {
					t.Fatal("maintenance window bypassed", admitted, err)
				}
			}
		})
	}
}

func TestImageUpdateDispatchGuards(t *testing.T) {
	for _, guard := range []string{"manual stop", "pause", "configuration changed", "unknown result", "closed window", "already consumed"} {
		t.Run(guard, func(t *testing.T) {
			f := newRegistryHookFixture(t, "distribution", "registry.example.test")
			f.sql(`UPDATE nodes SET capabilities=capabilities || '{"image_updates":true}'::jsonb WHERE id='node'`)
			policy := worker.ImageUpdatePolicy{Enabled: true, Mode: "auto", Rules: []worker.ImageUpdateRule{{Service: "app"}}}
			if guard == "closed window" {
				policy.WindowStart = time.Now().UTC().Add(time.Hour).Format("15:04")
				policy.WindowEnd = time.Now().UTC().Add(2 * time.Hour).Format("15:04")
			}
			f.call("root", "default", "PUT", f.path+"/image-update-policy", policy, 200)
			ctx := context.Background()
			tx, err := f.s.DB.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			request := worker.Request{ID: "guarded-check", Workload: f.id, Kind: "service", Action: "image_check"}
			if err = f.s.prepareImageUpdateCheck(ctx, tx, &request); err != nil {
				t.Fatal(err)
			}
			if err = tx.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			payload, err := f.s.sealJSON(request)
			if err != nil {
				t.Fatal(err)
			}
			update := worker.ImageUpdateResult{State: "available", Key: strings.Repeat("b", 64), Pins: map[string]string{"app": "registry.example.test/team/app:stable@sha256:" + strings.Repeat("a", 64)}}
			if guard == "unknown result" {
				update.State = "unknown"
				update.Message = "Registry unavailable."
			}
			result, _ := json.Marshal(worker.Operation{ImageCheck: &worker.ImageCheck{Updates: &update}})
			f.sql(`INSERT INTO operations(id,workload_id,action,state,payload,result) VALUES($1,$2,'image_check','succeeded',$3,$4)`, request.ID, f.id, payload, result)
			switch guard {
			case "manual stop":
				f.sql(`UPDATE workloads SET manually_stopped=true WHERE id=$1`, f.id)
			case "pause":
				f.sql(`UPDATE environments SET paused=true WHERE id='environment'`)
			case "configuration changed":
				var encrypted []byte
				if err = f.s.DB.QueryRow(ctx, `SELECT configuration FROM workloads WHERE id=$1`, f.id).Scan(&encrypted); err != nil {
					t.Fatal(err)
				}
				plain, _ := unseal(f.s.Key, encrypted)
				var config workloadInput
				json.Unmarshal(plain, &config)
				config.Name = "Changed"
				changed, err := f.s.sealJSON(config)
				if err != nil {
					t.Fatal(err)
				}
				f.sql(`UPDATE workloads SET configuration=$2 WHERE id=$1`, f.id, changed)
			case "already consumed":
				f.sql(`UPDATE image_update_policies SET consumed_candidate=$2 WHERE workload_id=$1`, f.id, update.Key)
			}
			if err = f.s.dispatchImageUpdates(ctx); err != nil {
				t.Fatal(err)
			}
			var count int
			if err = f.s.DB.QueryRow(ctx, `SELECT count(*) FROM operations WHERE workload_id=$1 AND action='deploy'`, f.id).Scan(&count); err != nil || count != 0 {
				t.Fatal("guarded update deployed", count, err)
			}
			if guard == "closed window" {
				status, err := f.s.readImageUpdatePolicy(ctx, f.id)
				if err != nil || status["last_check_id"] == nil || status["last_error"] == "" {
					t.Fatal("window wait lost candidate status", status, err)
				}
			}
		})
	}
}

package control

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"cove/internal/worker"
)

func TestRecoveryMergeManagedPITRCopyKeepsTypeAndRotatableCredentials(t *testing.T) {
	f, id := managedDatabaseFixture(t, "")
	source := readDatabaseConfig(t, f, id)
	f.sql(`UPDATE nodes SET capabilities=capabilities||'{"postgres_pitr":true}'::jsonb WHERE id='managed-node'`)
	policy := pitrConfiguration{Policy: worker.PITRPolicy{Archive: strings.Repeat("a", 32), Key: strings.Repeat("b", 64), Settings: worker.PITRSettings{Service: "postgres", IntervalHours: 24, KeepLast: 2, RetentionDays: 7, MaxBytes: 1 << 30}}, Backup: worker.BackupConfig{Repository: "local", Password: "synthetic-password", KeepLast: 2}}
	sealed, err := f.s.sealJSON(policy)
	if err != nil {
		t.Fatal(err)
	}
	f.sql(`INSERT INTO workload_pitr(workload_id,configuration,enabled) VALUES($1,$2,false)`, id, sealed)
	f.s.workerRPC = func(_ context.Context, _ string, r worker.RPCRequest) (worker.RPCResponse, error) {
		return worker.RPCResponse{RestoreCopy: &worker.RestoreCopy{Source: id, Compose: "services:\n  postgres:\n    image: " + source.Database.Image + "\n    environment:\n      POSTGRES_USER: ${COVE_PITR_USER}\n      POSTGRES_PASSWORD: ${COVE_PITR_PASSWORD}\n    volumes: [recovered:/var/lib/postgresql]\nvolumes:\n  recovered: {external: true, name: cove-pitr-copy-" + r.Request.Workload + "}\n", Environment: map[string]string{"COVE_PITR_USER": "app", "COVE_PITR_PASSWORD": r.Request.Environment["COVE_PITR_PASSWORD"]}}}, nil
	}
	response := f.call("root", "default", "POST", "/api/workloads/"+id+"/pitr/restore-copy", pitrCopyInput{Name: "Recovered database", Base: policy.Policy.Archive + "_base_" + strings.Repeat("C", 24), Timestamp: time.Now().Add(-time.Minute)}, 202)
	target := jsonString(t, response, "resource_id")
	saved := readDatabaseConfig(t, f, target)
	var kind string
	if err = f.s.DB.QueryRow(t.Context(), `SELECT kind FROM workloads WHERE id=$1`, target).Scan(&kind); err != nil {
		t.Fatal(err)
	}
	if kind != "database" || saved.Database == nil || saved.Database.Engine != "postgresql" || saved.Database.Host != databaseHostname(target) || saved.Database.PublicAccess != nil || saved.Database.Pooler {
		t.Fatal("PITR copy lost isolated managed identity", kind, saved.Database)
	}
	exports, err := databaseExports(saved, nil)
	if err != nil || exports["DB_PASSWORD"] == source.Environment["POSTGRES_PASSWORD"] || len(exports["DB_PASSWORD"]) != 64 {
		t.Fatal("fresh managed credentials unavailable", err)
	}
	if strings.Contains(saved.Compose, "${COVE_PITR_PASSWORD}") || !strings.Contains(saved.Compose, "${POSTGRES_PASSWORD}") || !strings.Contains(saved.Compose, "cove-pitr-copy-"+target) {
		t.Fatal("rotation would use a stale password or another data volume")
	}
	saved.Environment["POSTGRES_PASSWORD"] = strings.Repeat("d", 64)
	if err = configureDatabasePooler(&saved, true); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(saved.Compose, "cove-pitr-copy-"+target) {
		t.Fatal("pooler changed recovered volume")
	}
}

func TestRecoveryMergePITRCopyNeedsDestinationCreateAndRespectsPause(t *testing.T) {
	f, id, cookie := publicFixture(t)
	f.sql(`INSERT INTO team_roles(team_id,id,name,permissions) VALUES('default','recovery-reader','Recovery reader',ARRAY['view','manage_backups','read_secrets'])`)
	f.sql(`UPDATE team_members SET role='member',grants=$1::jsonb WHERE user_id='public-owner'`, `[{"scope":"resource","scope_id":"`+id+`","role":"recovery-reader"}]`)
	f.sql(`UPDATE nodes SET capabilities=capabilities||'{"postgres_pitr":true}'::jsonb WHERE id='variable-node'`)
	input := pitrCopyInput{Name: "Copy", Base: strings.Repeat("a", 32) + "_base_" + strings.Repeat("C", 24), Timestamp: time.Now().Add(-time.Minute)}
	f.call(cookie, "default", "POST", "/api/workloads/"+id+"/pitr/restore-copy", input, 403)
	f.sql(`UPDATE environments SET paused=true WHERE id='variable-env'`)
	f.call("root", "default", "POST", "/api/workloads/"+id+"/pitr/restore-copy", input, 409)
}

func TestRecoveryMergeMaintenanceAdmissionFreezesMovesAndPITRConfiguration(t *testing.T) {
	for _, action := range []string{"move", "pitr_configure", "pitr_backup", "pitr_restore"} {
		t.Run(action, func(t *testing.T) {
			f, id, _ := publicFixture(t)
			f.sql(`INSERT INTO deployment_policies(team_id,freeze_from,freeze_until) VALUES('default',now()-interval '1 minute',now()+interval '1 hour')`)
			operation := resourceID()
			f.sql(`INSERT INTO operations(id,workload_id,action,payload) VALUES($1,$2,$3,$4)`, operation, id, action, []byte{})
			allowed, err := f.s.admitDeployment(t.Context(), operation)
			if err != nil || allowed != (action == "pitr_backup" || action == "pitr_restore") {
				t.Fatal("maintenance freeze classification", action, allowed, err)
			}
		})
	}
}

func TestRecoveryMergeProvisioningOwnerRevocationCancelsBeforeCloudCall(t *testing.T) {
	f := newAccountFixture(t)
	owner := f.user("provisioner", "provisioner@example.test")
	_ = owner
	f.sql(`UPDATE users SET is_admin=true WHERE id='provisioner'`)
	config, _ := f.s.sealJSON(nodeProvisionConfig{Name: "Fixture", Region: "r", Plan: "p", Image: "i"})
	f.sql(`INSERT INTO node_provisions(id,provider_kind,provider_name,team_id,cloud_name,configuration,creator_id) VALUES('recovery-owned','hetzner','Fixture','default','cove-recovery-owned',$1,'provisioner')`, config)
	f.sql(`UPDATE users SET disabled=true WHERE id='provisioner'`)
	if err := f.s.processNodeProvision(t.Context(), "recovery-owned"); err != nil {
		t.Fatal(err)
	}
	var phase string
	var cleared bool
	if err := f.s.DB.QueryRow(t.Context(), `SELECT phase,octet_length(configuration)=0 FROM node_provisions WHERE id='recovery-owned'`).Scan(&phase, &cleared); err != nil || phase != "cancelled" || !cleared {
		t.Fatal("revoked queued owner retained create credentials", phase, cleared, err)
	}
}

func TestRecoveryMergePITRScopePermissionsAreExplicit(t *testing.T) {
	f, id, cookie := publicFixture(t)
	_, token := mintPublic(t, f, cookie, "user", "workloads:read", "pitr:backup")
	f.sql(`UPDATE nodes SET capabilities=capabilities||'{"postgres_pitr":true}'::jsonb WHERE id='variable-node'`)
	publicCall(t, f, token, "default", "GET", "/api/v1/workloads/"+id+"/pitr", nil, 200)
	publicCall(t, f, token, "default", "PUT", "/api/v1/workloads/"+id+"/pitr", pitrSettingsInput{}, 403)
	publicCall(t, f, token, "default", "POST", "/api/v1/workloads/"+id+"/pitr/backup", map[string]any{}, 409)
	f.sql(`UPDATE team_members SET role='member',grants='[]' WHERE user_id='public-owner'`)
	publicCall(t, f, token, "default", "GET", "/api/v1/workloads/"+id+"/pitr", nil, 403)
	document, _ := json.Marshal(f.s.openAPIDocument())
	if !strings.Contains(string(document), "/workloads/{id}/pitr/restore-copy") {
		t.Fatal("PITR omitted from OpenAPI")
	}
}

func TestRecoveryMergeInheritedLimitsReserveQuotaBeforeDeploy(t *testing.T) {
	for _, test := range []struct {
		name   string
		cpu    float64
		budget float64
		report bool
		want   bool
	}{
		{"replicas within budget", 1.5, 2, true, true},
		{"inherited caps exceed budget", 2.5, 2, true, false},
		{"old worker without usage", 0, 2, false, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			f, id, _ := publicFixture(t)
			f.sql(`INSERT INTO team_quotas(team_id,cpus,memory_mb) VALUES('default',$1,512)`, test.budget)
			calls := 0
			f.s.workerRPC = func(_ context.Context, _ string, r worker.RPCRequest) (worker.RPCResponse, error) {
				calls++
				if r.Method != "capacity" || r.Request == nil || r.Request.DefaultLimits == nil {
					t.Fatal("quota inspection lost inherited limits")
				}
				report := &worker.PreflightReport{}
				if test.report {
					report.Usage = &worker.ResourceUsage{CPUs: test.cpu, MemoryMB: 384}
				}
				return worker.RPCResponse{Preflight: report}, nil
			}
			request := worker.Request{ID: resourceID(), Workload: id, Action: "deploy", DefaultLimits: &worker.ServiceLimits{CPUs: 0.5, MemoryMB: 128}}
			err := f.s.reserveInheritedLimits(t.Context(), "variable-node", request)
			if (err == nil) != test.want || calls != 1 {
				t.Fatal("inherited quota admission", err, calls)
			}
			var reserved float64
			if err = f.s.DB.QueryRow(t.Context(), `SELECT COALESCE(configured_cpu,0) FROM workloads WHERE id=$1`, id).Scan(&reserved); err != nil {
				t.Fatal(err)
			}
			if test.want && reserved != test.cpu || !test.want && reserved != 0 {
				t.Fatal("quota reservation changed after refusal", reserved)
			}
		})
	}
}

func TestRecoveryMergeMaintenanceSuppressesAlertsOnlyWhileRunning(t *testing.T) {
	f, id, _ := publicFixture(t)
	for _, action := range []string{"move", "pitr_configure"} {
		op := resourceID()
		f.sql(`INSERT INTO operations(id,workload_id,action,payload) VALUES($1,$2,$3,$4)`, op, id, action, []byte{})
		if down, err := f.s.resourceIntentionallyDown(t.Context(), id); err != nil || down {
			t.Fatal("queued maintenance hid a real outage", down, err)
		}
		f.sql(`UPDATE operations SET state='running' WHERE id=$1`, op)
		if down, err := f.s.resourceIntentionallyDown(t.Context(), id); err != nil || !down {
			t.Fatal("running maintenance raised downtime alerts", down, err)
		}
		f.sql(`UPDATE operations SET state='succeeded' WHERE id=$1`, op)
	}
}

func TestRecoveryMergeCloudVerifierFailureIsAuthenticatedDurableAndOneUse(t *testing.T) {
	f := newAccountFixture(t)
	f.sql(`INSERT INTO nodes(id,name,address,port,public_key,private_key,state) VALUES('cloud-missing-verifier','Cloud','pending.invalid',22,'',$1,'pending')`, []byte{})
	token := randomToken()
	f.sql(`INSERT INTO enrollments(token_hash,node_id,expires_at) VALUES($1,'cloud-missing-verifier',now()+interval '15 minutes')`, tokenHash(token))
	config, _ := f.s.sealJSON(nodeProvisionConfig{Name: "Prepared image", Verifier: true})
	f.sql(`INSERT INTO node_provisions(id,provider_kind,provider_name,team_id,node_id,cloud_name,configuration,phase,creator_id) VALUES('verifier-report','hetzner','Fixture','default','cloud-missing-verifier','cove-verifier-report',$1,'creating','bootstrap')`, config)
	report := func(credential string, want int) {
		t.Helper()
		request := httptest.NewRequest("POST", "/bootstrap/register", bytes.NewBufferString(`{"enrollment_error":"trusted_verifier_missing"}`))
		request.Header.Set("Authorization", "Bearer "+credential)
		response := httptest.NewRecorder()
		f.handler.ServeHTTP(response, request)
		if response.Code != want {
			t.Fatal("verifier report", response.Code, response.Body.String())
		}
	}
	report("invalid", 401)
	report(token, 200)
	report(token, 401)
	if err := f.s.processNodeProvision(t.Context(), "verifier-report"); err != nil {
		t.Fatal(err)
	}
	var phase, message string
	var revoked, retained bool
	if err := f.s.DB.QueryRow(t.Context(), `SELECT phase,last_error,EXISTS(SELECT 1 FROM enrollments WHERE node_id=p.node_id AND consumed_at IS NOT NULL),EXISTS(SELECT 1 FROM nodes WHERE id=p.node_id) FROM node_provisions p WHERE id='verifier-report'`).Scan(&phase, &message, &revoked, &retained); err != nil {
		t.Fatal(err)
	}
	if phase != "uncertain" || !strings.Contains(message, "Trusted Cosign v3+ is missing") || !strings.Contains(message, "Creation will not be repeated") || !revoked || !retained {
		t.Fatal("missing verifier failure was hidden or paid server tracking discarded", phase, revoked, retained)
	}
	cloud, err := provisioningUserData("https://panel.example.test/control/cove", token)
	if err != nil {
		t.Fatal(err)
	}
	var body struct {
		Run [][]string `json:"runcmd"`
	}
	if err = json.Unmarshal([]byte(strings.TrimPrefix(cloud, "#cloud-config\n")), &body); err != nil {
		t.Fatal(err)
	}
	command := body.Run[0][2]
	if strings.Index(command, "command -v cosign") > strings.Index(command, "setup=$(mktemp)") || !strings.Contains(command, "https://panel.example.test/control/cove/bootstrap/register") || !strings.Contains(command, "trusted_verifier_missing") {
		t.Fatal("cloud-init omitted pre-install verifier refusal or panel prefix")
	}
}

package control

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"cove/internal/worker"
)

func TestBackupDownloadPermissionsBoundsAndAudit(t *testing.T) {
	f := newAccountFixture(t)
	ctx := context.Background()
	f.sql(`INSERT INTO nodes(id,name,address,port,public_key,private_key,state,capabilities) VALUES('transfer-node','Node','localhost',22,'',$1,'ready','{"backup_transfers":true}')`, []byte{})
	f.sql("INSERT INTO projects(id,name,team_id) VALUES('transfer-project','Project','default')")
	configuration, err := json.Marshal(workloadInput{Name: "Database", Kind: "service", Backup: &worker.BackupConfig{Repository: "local", Password: "synthetic-transfer-password", KeepLast: 2}})
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := seal(f.s.Key, configuration)
	if err != nil {
		t.Fatal(err)
	}
	f.sql("INSERT INTO workloads(id,node_id,name,configuration,project_id,team_id) VALUES('transfer-resource','transfer-node','Database',$1,'transfer-project','default')", sealed)
	owner := f.user("transfer-owner", "transfer-owner@example.com")
	viewer := f.user("transfer-viewer", "transfer-viewer@example.com")
	f.sql(`INSERT INTO team_members(team_id,user_id,role,grants) VALUES('default','transfer-owner','owner','[]'),('default','transfer-viewer','member','[{"scope":"project","scope_id":"transfer-project","role":"viewer"}]')`)
	path := "/api/workloads/transfer-resource/backups/backup/download"
	f.call(viewer, "default", "GET", path, nil, 403)
	f.call(owner, "default", "PUT", "/api/settings/transfer-limits", map[string]int64{"backup_bytes": 1 << 20}, 403)
	f.call("root", "default", "PUT", "/api/settings/transfer-limits", map[string]int64{"backup_bytes": 0}, 400)
	f.call("root", "default", "PUT", "/api/settings/transfer-limits", map[string]int64{"backup_bytes": 4 << 20}, 200)
	payload := strings.Repeat("data\x00\n", 400000)
	digest := sha256.Sum256([]byte(payload))
	calls := 0
	f.s.workerStream = func(_ context.Context, node string, r worker.RPCRequest, _ io.Reader, consume func(worker.StreamResponse, io.Reader) error) error {
		calls++
		if node != "transfer-node" || r.Method != "stream" || r.Stream == nil || r.Stream.Action != "backup_download" || r.Stream.Limit != 4<<20 || r.Stream.Request.Backup.Password != "synthetic-transfer-password" {
			t.Fatal("incorrect backup stream request")
		}
		return consume(worker.StreamResponse{Size: int64(len(payload)), SHA256: hex.EncodeToString(digest[:])}, strings.NewReader(payload))
	}
	response := f.call(owner, "default", "GET", path, nil, 200)
	if response.Body.String() != payload || response.Header().Get("X-Cove-SHA256") != hex.EncodeToString(digest[:]) || response.Header().Get("Cache-Control") != "no-store" || !strings.Contains(response.Header().Get("Content-Disposition"), ".tar") {
		t.Fatal("binary download changed or was cacheable")
	}
	var audited int
	if err := f.s.DB.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE action='GET /api/workloads/{id}/backups/{backup}/download' AND subject='transfer-resource' AND status=200`).Scan(&audited); err != nil || audited != 1 {
		t.Fatal("download not audited", audited, err)
	}
	f.sql(`UPDATE nodes SET capabilities='{}' WHERE id='transfer-node'`)
	f.call(owner, "default", "GET", path, nil, 409)
	f.sql(`UPDATE nodes SET capabilities='{"backup_transfers":true}' WHERE id='transfer-node'`)
	f.sql("INSERT INTO operations(id,workload_id,action,payload) VALUES('transfer-active','transfer-resource','deploy',$1)", sealed)
	f.call(owner, "default", "GET", path, nil, 409)
	if calls != 1 {
		t.Fatal("blocked access reached worker")
	}
}

func TestDumpImportStreamsRawBodiesAndUsesOnlyTeamS3Credentials(t *testing.T) {
	f := newAccountFixture(t)
	f.sql(`INSERT INTO nodes(id,name,address,port,public_key,private_key,state,capabilities) VALUES('import-node','Node','localhost',22,'',$1,'ready','{"backup_transfers":true,"logical_backups":true,"extended_logical_backups":true}')`, []byte{})
	f.sql("INSERT INTO projects(id,name,team_id) VALUES('import-project','Project','default')")
	config := workloadInput{Name: "Import", Kind: "service", Backup: &worker.BackupConfig{Repository: "local", Password: "synthetic-import-password", KeepLast: 3, Database: &worker.DatabaseBackup{Engine: "redis", Service: "db", Name: "all"}}}
	plain, _ := json.Marshal(config)
	sealed, _ := seal(f.s.Key, plain)
	f.sql("INSERT INTO workloads(id,node_id,name,configuration,project_id,team_id) VALUES('import-resource','import-node','Import',$1,'import-project','default')", sealed)
	editor := f.user("import-editor", "import-editor@example.com")
	viewer := f.user("import-viewer", "import-viewer@example.com")
	f.sql(`INSERT INTO team_members(team_id,user_id,role,grants) VALUES('default','import-editor','member','[{"scope":"project","scope_id":"import-project","role":"editor"}]'),('default','import-viewer','member','[{"scope":"project","scope_id":"import-project","role":"viewer"}]')`)
	f.call("root", "default", "PUT", "/api/settings/transfer-limits", map[string]int64{"backup_bytes": 4 << 20}, 200)
	payload := bytes.Repeat([]byte{0, 255, '\n', 'a'}, 600000)
	calls := 0
	f.s.workerStream = func(_ context.Context, node string, r worker.RPCRequest, input io.Reader, consume func(worker.StreamResponse, io.Reader) error) error {
		calls++
		if node != "import-node" || r.Stream == nil || r.Stream.Request.Backup.Password != config.Backup.Password {
			t.Fatal("incorrect sealed import policy")
		}
		body := payload
		if r.Stream.Action == "backup_upload" {
			actual, err := io.ReadAll(input)
			if err != nil || !bytes.Equal(actual, payload) || r.Stream.Size != int64(len(payload)) {
				t.Fatal("raw upload changed", err)
			}
		} else {
			if r.Stream.Action != "backup_import_s3" || r.Stream.SourceStorage == nil || r.Stream.SourceStorage.SecretKey != "synthetic-s3-secret" || r.Stream.ObjectKey != "dump.rdb" || input != nil {
				t.Fatal("incorrect S3 import context")
			}
			body = []byte("S3 fixture dump")
		}
		digest := sha256.Sum256(body)
		return consume(worker.StreamResponse{BackupID: r.Stream.Request.ID, Size: int64(len(body)), SHA256: hex.EncodeToString(digest[:])}, bytes.NewReader(nil))
	}
	upload := func(user string, data []byte, size int64, status int) {
		t.Helper()
		r := httptest.NewRequest("POST", "/api/workloads/import-resource/backups/import", bytes.NewReader(data))
		r.Header.Set("Origin", f.s.PublicURL)
		r.Header.Set("X-Cove-Team", "default")
		r.Header.Set("Content-Type", "application/octet-stream")
		r.Header.Set("X-Cove-Size", strconv.FormatInt(size, 10))
		r.AddCookie(&http.Cookie{Name: "__Host-cove", Value: user})
		w := httptest.NewRecorder()
		f.handler.ServeHTTP(w, r)
		if w.Code != status {
			t.Fatalf("binary import: got%d want%d: %s", w.Code, status, w.Body.String())
		}
		if strings.Contains(w.Body.String(), config.Backup.Password) {
			t.Fatal("import returned credentials")
		}
	}
	upload(viewer, payload, int64(len(payload)), 403)
	upload(editor, payload, int64(len(payload))+1, 400)
	upload(editor, make([]byte, (4<<20)+1), (4<<20)+1, 413)
	upload(editor, payload, int64(len(payload)), 201)
	connection := connectionConfig{Name: "S3", Storage: &worker.StorageLocation{Protocol: "s3", Endpoint: "https://storage.example.com", Path: "bucket/prefix", AccessKey: "synthetic-access", SecretKey: "synthetic-s3-secret"}}
	storage := jsonString(t, f.call("root", "default", "POST", "/api/storage-locations", connection, 201), "id")
	response := f.call(editor, "default", "POST", "/api/workloads/import-resource/backups/import-s3", map[string]string{"storage_id": storage, "object_key": "dump.rdb"}, 201)
	if strings.Contains(response.Body.String(), connection.Storage.SecretKey) {
		t.Fatal("S3 secret returned")
	}
	f.call(editor, "default", "POST", "/api/workloads/import-resource/backups/import-s3", map[string]string{"storage_id": storage, "object_key": "../outside.rdb"}, 400)
	f.call(editor, "default", "POST", "/api/workloads/import-resource/backups/import-s3", map[string]string{"storage_id": storage, "object_key": "dump.rdb", "endpoint": "https://arbitrary.example.com"}, 400)
	f.sql("INSERT INTO teams(id,name) VALUES('foreign-import','Foreign')")
	foreign := jsonString(t, f.call("root", "foreign-import", "POST", "/api/storage-locations", connection, 201), "id")
	f.call(editor, "default", "POST", "/api/workloads/import-resource/backups/import-s3", map[string]string{"storage_id": foreign, "object_key": "dump.rdb"}, 403)
	if calls != 2 {
		t.Fatal("rejected import reached worker", calls)
	}
	var audited int
	if err := f.s.DB.QueryRow(context.Background(), `SELECT count(*) FROM audit_events WHERE subject='import-resource' AND action IN ('POST /api/workloads/{id}/backups/import','POST /api/workloads/{id}/backups/import-s3') AND status=201`).Scan(&audited); err != nil || audited != 2 {
		t.Fatal("imports not audited", audited, err)
	}
}

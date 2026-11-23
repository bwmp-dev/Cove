package control

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"cove/internal/worker"
)

func TestStreamedFilesRequireScopedEditorsAndAudit(t *testing.T) {
	f := newAccountFixture(t)
	f.sql(`INSERT INTO nodes(id,name,address,port,public_key,private_key,state,capabilities) VALUES('file-stream-node','Node','localhost',22,'',$1,'ready','{"file_browser":true,"file_transfers":true}')`, []byte{})
	f.sql(`INSERT INTO projects(id,name,team_id) VALUES('file-stream-project','Project','default'),('file-other-project','Other','default')`)
	sealed, _ := seal(f.s.Key, []byte(`{}`))
	f.sql(`INSERT INTO workloads(id,node_id,name,configuration,project_id,team_id) VALUES('file-stream-resource','file-stream-node','Files',$1,'file-stream-project','default'),('file-other-resource','file-stream-node','Other',$1,'file-other-project','default')`, sealed)
	editor := f.user("file-stream-editor", "file-stream-editor@example.com")
	viewer := f.user("file-stream-viewer", "file-stream-viewer@example.com")
	f.sql(`INSERT INTO team_members(team_id,user_id,role,grants) VALUES('default','file-stream-editor','member','[{"scope":"project","scope_id":"file-stream-project","role":"editor"}]'),('default','file-stream-viewer','member','[{"scope":"project","scope_id":"file-stream-project","role":"viewer"}]')`)
	limitPath := "/api/settings/file-transfer-limits"
	f.call(editor, "default", "PUT", limitPath, map[string]int64{"file_bytes": 4 << 20}, 403)
	f.call("root", "default", "PUT", limitPath, map[string]int64{"file_bytes": 0}, 400)
	f.call("root", "default", "PUT", limitPath, map[string]int64{"file_bytes": 4 << 20}, 200)
	f.call(editor, "default", "GET", "/api/workloads/file-stream-resource/files/limits", nil, 200)
	f.call(viewer, "default", "GET", "/api/workloads/file-stream-resource/files/limits", nil, 403)
	payload := bytes.Repeat([]byte{0, 255, 10, 13, 'x'}, 400000)
	digest := sha256.Sum256(payload)
	revision := hex.EncodeToString(digest[:])
	calls := 0
	f.s.workerStream = func(_ context.Context, node string, request worker.RPCRequest, input io.Reader, consume func(worker.StreamResponse, io.Reader) error) error {
		calls++
		if node != "file-stream-node" || request.Stream == nil || request.Stream.Workload != "file-stream-resource" || request.Stream.Files == nil || request.Stream.Limit != 4<<20 {
			t.Fatal("incorrect scoped stream")
		}
		body := io.Reader(bytes.NewReader(payload))
		if request.Stream.Action == "file_upload" {
			actual, err := io.ReadAll(input)
			if err != nil || !bytes.Equal(actual, payload) || request.Stream.Size != int64(len(payload)) {
				t.Fatal("raw file changed", err)
			}
			body = bytes.NewReader(nil)
		}
		return consume(worker.StreamResponse{Size: int64(len(payload)), SHA256: revision}, body)
	}
	query := "?kind=container&target=aaaaaaaaaaaa&path=/data/large.bin"
	url := "/api/workloads/file-stream-resource/files/download" + query
	f.call(viewer, "default", "GET", url, nil, 403)
	f.call(editor, "default", "GET", "/api/workloads/file-other-resource/files/download"+query, nil, 403)
	response := f.call(editor, "default", "GET", url, nil, 200)
	if !bytes.Equal(response.Body.Bytes(), payload) || response.Header().Get("X-Cove-SHA256") != revision || response.Header().Get("Cache-Control") != "no-store" || !strings.Contains(response.Header().Get("Content-Disposition"), "large.bin") {
		t.Fatal("download metadata or bytes changed")
	}
	native := httptest.NewRequest("GET", url+"&team_id=default", nil)
	native.AddCookie(&http.Cookie{Name: "__Host-cove", Value: editor})
	nativeResponse := httptest.NewRecorder()
	f.handler.ServeHTTP(nativeResponse, native)
	if nativeResponse.Code != 200 || !bytes.Equal(nativeResponse.Body.Bytes(), payload) {
		t.Fatal("native browser download lost team context", nativeResponse.Code)
	}
	f.call(editor, "default", "GET", url+"&team_id=other", nil, 400)
	upload := func(user string, data []byte, size int64, status int) {
		t.Helper()
		r := httptest.NewRequest("POST", "/api/workloads/file-stream-resource/files/upload"+query, bytes.NewReader(data))
		r.Header.Set("Origin", f.s.PublicURL)
		r.Header.Set("X-Cove-Team", "default")
		r.Header.Set("Content-Type", "application/octet-stream")
		r.Header.Set("X-Cove-Size", strconv.FormatInt(size, 10))
		r.AddCookie(&http.Cookie{Name: "__Host-cove", Value: user})
		w := httptest.NewRecorder()
		f.handler.ServeHTTP(w, r)
		if w.Code != status {
			t.Fatalf("upload got %d want %d: %s", w.Code, status, w.Body.String())
		}
	}
	upload(viewer, payload, int64(len(payload)), 403)
	upload(editor, payload, int64(len(payload))+1, 400)
	upload(editor, make([]byte, (4<<20)+1), (4<<20)+1, 413)
	upload(editor, payload, int64(len(payload)), 200)
	if calls != 3 {
		t.Fatal("rejected file access reached worker", calls)
	}
	var audited int
	if err := f.s.DB.QueryRow(context.Background(), `SELECT count(*) FROM audit_events WHERE subject='file-stream-resource' AND action IN ('GET /api/workloads/{id}/files/download','POST /api/workloads/{id}/files/upload') AND status=200`).Scan(&audited); err != nil || audited != 3 {
		t.Fatal("file access not audited", audited, err)
	}
	f.sql(`UPDATE nodes SET capabilities='{"file_browser":true}' WHERE id='file-stream-node'`)
	f.call(editor, "default", "GET", url, nil, 409)
	if calls != 3 {
		t.Fatal("old worker reached stream")
	}
}

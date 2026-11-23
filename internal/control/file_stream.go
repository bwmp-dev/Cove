package control

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"mime"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"

	"cove/internal/worker"
	"github.com/jackc/pgx/v5"
)

var fileTransferSlots = make(chan struct{}, 4)

func (s *Server) nativeFileDownload(w http.ResponseWriter, r *http.Request) {
	if team := r.URL.Query().Get("team_id"); team != "" {
		if len(team) > 64 || strings.ContainsAny(team, "\x00\r\n") || r.Header.Get("X-Cove-Team") != "" && r.Header.Get("X-Cove-Team") != team {
			http.Error(w, "invalid download team context", 400)
			return
		}
		r.Header.Set("X-Cove-Team", team)
	}
	s.admin(http.HandlerFunc(s.workloadFiles)).ServeHTTP(w, r)
}

func (s *Server) fileTransferLimits(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.Method == http.MethodPut {
		var in struct {
			FileBytes int64 `json:"file_bytes"`
		}
		if !decode(w, r, &in) {
			return
		}
		if in.FileBytes < 1<<20 || in.FileBytes > worker.MaxFileTransferBytes {
			http.Error(w, "file transfers must be limited to 1 MiB–1 GiB", 400)
			return
		}
		if _, err := s.DB.Exec(r.Context(), `UPDATE control_settings SET file_transfer_bytes=$1 WHERE id=1`, in.FileBytes); err != nil {
			http.Error(w, "could not save file transfer limit", 503)
			return
		}
	} else if r.PathValue("id") != "" && !s.requireAccess(w, r, "workload", r.PathValue("id"), 3) {
		return
	}
	var limit int64
	if err := s.DB.QueryRow(r.Context(), `SELECT file_transfer_bytes FROM control_settings WHERE id=1`).Scan(&limit); err != nil {
		http.Error(w, "file transfer limit unavailable", 503)
		return
	}
	respond(w, 200, map[string]int64{"file_bytes": limit, "min_file_bytes": 1 << 20, "max_file_bytes": worker.MaxFileTransferBytes, "editor_bytes": worker.MaxFileBytes})
}

func (s *Server) streamWorkloadFile(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if !s.requireAccess(w, r, "workload", r.PathValue("id"), 3) {
		return
	}
	if r.Method == http.MethodGet {
		observed := &statusResponse{ResponseWriter: w}
		w = observed
		defer func() {
			status := observed.status
			if status == 0 {
				status = 200
			}
			s.recordAudit(r, actor(r).ID, status)
		}()
	}
	select {
	case fileTransferSlots <- struct{}{}:
		defer func() { <-fileTransferSlots }()
	default:
		http.Error(w, "file transfers are busy; retry later", 429)
		return
	}
	files := worker.FileRequest{Action: "read", Kind: r.URL.Query().Get("kind"), Target: r.URL.Query().Get("target"), Path: r.URL.Query().Get("path"), Revision: r.Header.Get("X-Cove-Revision")}
	upload := r.Method == http.MethodPost
	var size int64
	if upload {
		media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		var sizeErr error
		size, sizeErr = strconv.ParseInt(r.Header.Get("X-Cove-Size"), 10, 64)
		if err != nil || media != "application/octet-stream" || sizeErr != nil || size < 0 || r.ContentLength >= 0 && r.ContentLength != size {
			http.Error(w, "send a binary file with matching X-Cove-Size and Content-Length when known", 400)
			return
		}
		files.Action = "write"
		empty := []byte{}
		files.Data = &empty
	}
	if err := worker.ValidateFileRequest(files); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	files.Data = nil
	tx, err := s.DB.Begin(r.Context())
	if err != nil {
		http.Error(w, "database unavailable", 503)
		return
	}
	defer tx.Rollback(r.Context())
	var node, state string
	var supported, busy bool
	var limit int64
	err = tx.QueryRow(r.Context(), `SELECT w.node_id,n.state,COALESCE((n.capabilities->>'file_transfers')::boolean,false),EXISTS(SELECT 1 FROM operations WHERE workload_id=w.id AND state IN ('queued','running')),(SELECT file_transfer_bytes FROM control_settings WHERE id=1) FROM workloads w JOIN nodes n ON n.id=w.node_id WHERE w.id=$1 AND w.team_id=$2 FOR UPDATE OF w`, r.PathValue("id"), actor(r).Team).Scan(&node, &state, &supported, &busy, &limit)
	if errors.Is(err, pgx.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, "file transfer configuration unavailable", 503)
		return
	}
	if state != "ready" || !supported || busy {
		http.Error(w, "wait for active operations and verify an upgraded ready worker with streamed file support", 409)
		return
	}
	if size > limit {
		http.Error(w, "file exceeds the configured transfer limit", 413)
		return
	}
	deadline := time.Now().Add(30 * time.Minute)
	controller := http.NewResponseController(w)
	_ = controller.SetReadDeadline(deadline)
	_ = controller.SetWriteDeadline(deadline)
	stream := worker.StreamRequest{Action: "file_download", Workload: r.PathValue("id"), Files: &files, Limit: limit, Size: size}
	var input io.Reader
	digest := sha256.New()
	if upload {
		stream.Action = "file_upload"
		r.Body = http.MaxBytesReader(w, r.Body, size+1)
		defer r.Body.Close()
		input = io.TeeReader(r.Body, digest)
	}
	var receipt worker.StreamResponse
	started := false
	err = s.nodeStream(r.Context(), node, worker.RPCRequest{Method: "stream", Stream: &stream}, input, func(meta worker.StreamResponse, body io.Reader) error {
		if meta.Size < 0 || meta.Size > limit || len(meta.SHA256) != 64 {
			return errors.New("invalid file transfer receipt")
		}
		if upload {
			if meta.Size != size || meta.SHA256 != hex.EncodeToString(digest.Sum(nil)) {
				return errors.New("file upload integrity check failed")
			}
		} else {
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": path.Base(files.Path)}))
			w.Header().Set("X-Cove-SHA256", meta.SHA256)
			started = true
			if _, err := io.CopyN(io.MultiWriter(w, digest), body, meta.Size); err != nil {
				return err
			}
			if meta.SHA256 != hex.EncodeToString(digest.Sum(nil)) {
				return errors.New("file download integrity check failed")
			}
		}
		var extra [1]byte
		if n, err := body.Read(extra[:]); n != 0 || err != io.EOF {
			return errors.New("invalid file transfer trailer")
		}
		receipt = meta
		return nil
	})
	if err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		if started {
			panic(http.ErrAbortHandler)
		}
		http.Error(w, "file transfer failed; check ownership, revision, size, free space and required tools; refresh before retrying changes", 502)
		return
	}
	if upload {
		respond(w, 200, map[string]any{"revision": receipt.SHA256, "size": receipt.Size})
	}
}

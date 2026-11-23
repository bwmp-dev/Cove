package control

import (
	"errors"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"

	"cove/internal/worker"
)

func (s *Server) importBackupDump(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if !s.requirePermission(w, r, "workload", r.PathValue("id"), "edit_configuration") || !s.requirePermission(w, r, "workload", r.PathValue("id"), "manage_backups") || !s.requirePermission(w, r, "workload", r.PathValue("id"), "read_secrets") {
		return
	}
	select {
	case backupTransferSlots <- struct{}{}:
		defer func() { <-backupTransferSlots }()
	default:
		http.Error(w, "backup transfer slots are busy; retry later", 429)
		return
	}
	deadline := time.Now().Add(30 * time.Minute)
	controller := http.NewResponseController(w)
	_ = controller.SetReadDeadline(deadline)
	_ = controller.SetWriteDeadline(deadline)
	fromS3 := strings.HasSuffix(r.URL.Path, "/import-s3")
	var source struct {
		StorageID string `json:"storage_id"`
		ObjectKey string `json:"object_key"`
	}
	var size int64
	if fromS3 {
		if !decode(w, r, &source) {
			return
		}
		if source.StorageID == "" || len(source.StorageID) > 64 || worker.ValidateImportObjectKey(source.ObjectKey) != nil {
			http.Error(w, "choose a configured S3 storage location and a relative object key", 400)
			return
		}
	} else {
		mediaType, _, mediaErr := mime.ParseMediaType(r.Header.Get("Content-Type"))
		var sizeErr error
		size, sizeErr = strconv.ParseInt(r.Header.Get("X-Cove-Size"), 10, 64)
		if mediaErr != nil || sizeErr != nil || mediaType != "application/octet-stream" || size < 1 || (r.ContentLength >= 0 && r.ContentLength != size) {
			http.Error(w, "send a nonempty binary dump with matching X-Cove-Size and Content-Length when known", 400)
			return
		}
	}
	transfer, ok := s.prepareBackupTransfer(w, r)
	if !ok {
		return
	}
	defer transfer.tx.Rollback(r.Context())
	stream := worker.StreamRequest{Action: "backup_upload", Request: &transfer.request, Limit: transfer.limit, Size: size}
	var input io.Reader
	if fromS3 {
		connection, err := s.readConnection(r.Context(), source.StorageID, actor(r).Team, "storage")
		if err != nil || connection.Storage == nil || connection.Storage.Protocol != "s3" {
			http.Error(w, "S3 location not available to this team", 403)
			return
		}
		stream.Action, stream.SourceStorage, stream.ObjectKey = "backup_import_s3", connection.Storage, source.ObjectKey
	} else {
		if size > transfer.limit {
			http.Error(w, "dump exceeds the configured backup transfer limit", 413)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, size)
		defer r.Body.Close()
		input = r.Body
	}
	var imported worker.StreamResponse
	err := s.nodeStream(r.Context(), transfer.node, worker.RPCRequest{Method: "stream", Stream: &stream}, input, func(meta worker.StreamResponse, body io.Reader) error {
		if meta.BackupID != transfer.request.ID || meta.Size < 1 || meta.Size > transfer.limit || (size != 0 && meta.Size != size) {
			return errors.New("invalid import confirmation")
		}
		var extra [1]byte
		if n, err := body.Read(extra[:]); n != 0 || err != io.EOF {
			return errors.New("unexpected import response data")
		}
		imported = meta
		return nil
	})
	if err != nil {
		http.Error(w, "dump import failed validation or could not reach the worker or repository; existing resource data was not restored", 502)
		return
	}
	if err = transfer.tx.Commit(r.Context()); err != nil {
		http.Error(w, "import completed but database confirmation failed; refresh the backup list before retrying", 503)
		return
	}
	respond(w, 201, map[string]any{"backup_id": imported.BackupID, "size": imported.Size, "sha256": imported.SHA256, "verified": true})
}

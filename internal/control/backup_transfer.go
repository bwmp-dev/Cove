package control

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strings"
	"time"

	"cove/internal/worker"
	"github.com/jackc/pgx/v5"
)

var backupTransferSlots = make(chan struct{}, 4)

type backupTransferLimits struct {
	BackupBytes int64 `json:"backup_bytes"`
}

func (s *Server) transferLimits(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPut {
		var in backupTransferLimits
		if !decode(w, r, &in) {
			return
		}
		if in.BackupBytes < 1<<20 || in.BackupBytes > worker.MaxTransferBytes {
			http.Error(w, "backup transfers must be limited to 1 MiB–64 GiB", 400)
			return
		}
		if _, err := s.DB.Exec(r.Context(), "UPDATE control_settings SET backup_transfer_bytes=$1 WHERE id=1", in.BackupBytes); err != nil {
			http.Error(w, "could not save transfer limit", 503)
			return
		}
	}
	var limits backupTransferLimits
	if err := s.DB.QueryRow(r.Context(), "SELECT backup_transfer_bytes FROM control_settings WHERE id=1").Scan(&limits.BackupBytes); err != nil {
		http.Error(w, "transfer limit unavailable", 503)
		return
	}
	respond(w, 200, map[string]int64{"backup_bytes": limits.BackupBytes, "min_backup_bytes": 1 << 20, "max_backup_bytes": worker.MaxTransferBytes})
}

func (s *Server) nativeBackupDownload(w http.ResponseWriter, r *http.Request) {
	if team := r.URL.Query().Get("team_id"); team != "" {
		if len(team) > 64 || strings.ContainsAny(team, "\x00\r\n") || (r.Header.Get("X-Cove-Team") != "" && r.Header.Get("X-Cove-Team") != team) {
			http.Error(w, "invalid download team context", 400)
			return
		}
		r.Header.Set("X-Cove-Team", team)
	}
	s.admin(http.HandlerFunc(s.downloadBackup)).ServeHTTP(w, r)
}

func (s *Server) downloadBackup(w http.ResponseWriter, r *http.Request) {
	observed := &statusResponse{ResponseWriter: w}
	w = observed
	status, started := 0, false
	defer func() {
		if status == 0 {
			status = observed.status
			if status == 0 {
				status = 200
			}
		}
		s.recordAudit(r, actor(r).ID, status)
	}()
	w.Header().Set("Cache-Control", "no-store")
	if !s.requireAccess(w, r, "workload", r.PathValue("id"), 3) {
		return
	}
	select {
	case backupTransferSlots <- struct{}{}:
		defer func() { <-backupTransferSlots }()
	default:
		status = 429
		http.Error(w, "backup transfer readers are busy; retry later", status)
		return
	}
	transfer, ok := s.prepareBackupTransfer(w, r)
	if !ok {
		return
	}
	tx, node, request, limit := transfer.tx, transfer.node, transfer.request, transfer.limit
	defer tx.Rollback(r.Context())
	var err error
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(30 * time.Minute))
	err = s.nodeStream(r.Context(), node, worker.RPCRequest{Method: "stream", Stream: &worker.StreamRequest{Action: "backup_download", Request: &request, Limit: limit}}, nil, func(meta worker.StreamResponse, stream io.Reader) error {
		decoded, err := hex.DecodeString(meta.SHA256)
		if meta.Size < 1 || meta.Size > limit || err != nil || len(decoded) != sha256.Size {
			return errors.New("invalid backup download metadata")
		}
		extension := ".tar"
		if request.Backup.Database != nil {
			extension = ".dump"
			if request.Backup.Database.Engine == "redis" || request.Backup.Database.Engine == "valkey" {
				extension = ".rdb"
			}
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": "cove-backup-" + request.BackupID + extension}))
		w.Header().Set("X-Cove-SHA256", meta.SHA256)
		started = true
		digest := sha256.New()
		if _, err = io.CopyN(io.MultiWriter(w, digest), stream, meta.Size); err != nil {
			return err
		}
		var extra [1]byte
		n, err := stream.Read(extra[:])
		if n != 0 || err != io.EOF || hex.EncodeToString(digest.Sum(nil)) != meta.SHA256 {
			return errors.New("backup download failed integrity validation")
		}
		return nil
	})
	if err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		status = 502
		if started {
			panic(http.ErrAbortHandler)
		}
		http.Error(w, "backup download failed; check the selected snapshot, worker, repository and size limit", status)
	}
}

type backupTransferState struct {
	tx      pgx.Tx
	node    string
	request worker.Request
	limit   int64
}

func (s *Server) prepareBackupTransfer(w http.ResponseWriter, r *http.Request) (*backupTransferState, bool) {
	tx, err := s.DB.Begin(r.Context())
	if err != nil {
		status := 503
		http.Error(w, "database unavailable", status)
		return nil, false
	}
	accepted := false
	defer func() {
		if !accepted {
			tx.Rollback(r.Context())
		}
	}()
	var node, state string
	var configuration []byte
	var supported, busy bool
	var limit int64
	err = tx.QueryRow(r.Context(), `SELECT w.node_id,n.state,w.configuration,COALESCE((n.capabilities->>'backup_transfers')::boolean,false),(SELECT backup_transfer_bytes FROM control_settings WHERE id=1),EXISTS(SELECT 1 FROM operations WHERE workload_id=w.id AND state IN ('queued','running')) FROM workloads w JOIN nodes n ON n.id=w.node_id WHERE w.id=$1 AND w.team_id=$2 FOR UPDATE OF w`, r.PathValue("id"), actor(r).Team).Scan(&node, &state, &configuration, &supported, &limit, &busy)
	if errors.Is(err, pgx.ErrNoRows) {
		http.NotFound(w, r)
		return nil, false
	}
	if err != nil {
		status := 503
		http.Error(w, "backup configuration unavailable", status)
		return nil, false
	}
	if state != "ready" || !supported || busy {
		status := 409
		http.Error(w, "wait for active operations and upgrade and verify a ready worker with backup transfer support", status)
		return nil, false
	}
	plain, err := unseal(s.Key, configuration)
	var config workloadInput
	if err != nil || json.Unmarshal(plain, &config) != nil || config.Backup == nil {
		status := 409
		http.Error(w, "configure the backup's matching repository and database method first", status)
		return nil, false
	}
	action := "backup"
	if r.Method == http.MethodGet {
		action = "restore"
	}
	if action == "backup" && (config.Backup.Database == nil || config.Kind == "job") {
		http.Error(w, "select a logical database backup policy before importing a dump", 409)
		return nil, false
	}
	request := worker.Request{ID: resourceID(), Workload: r.PathValue("id"), Action: action, Backup: config.Backup, BackupID: r.PathValue("backup")}
	if worker.ValidateRequest(request) != nil {
		status := 400
		http.Error(w, "invalid backup transfer request", status)
		return nil, false
	}
	if err = s.prepareInfrastructure(r.Context(), &request); err != nil {
		status := 409
		http.Error(w, "backup storage unavailable; verify its worker support and saved connection", status)
		return nil, false
	}
	accepted = true
	return &backupTransferState{tx: tx, node: node, request: request, limit: limit}, true
}

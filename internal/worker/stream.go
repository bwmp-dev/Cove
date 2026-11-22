package worker

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"time"
)

const MaxTransferBytes int64 = 64 << 30

type StreamRequest struct {
	Action        string           `json:"action"`
	Request       *Request         `json:"request"`
	Limit         int64            `json:"limit"`
	Size          int64            `json:"size,omitempty"`
	SourceStorage *StorageLocation `json:"source_storage,omitempty"`
	ObjectKey     string           `json:"object_key,omitempty"`
}

type StreamResponse struct {
	Size     int64  `json:"size"`
	SHA256   string `json:"sha256"`
	Error    string `json:"error,omitempty"`
	BackupID string `json:"backup_id,omitempty"`
}

func decodeRPCFrame(in io.Reader) (RPCRequest, io.Reader, error) {
	var request RPCRequest
	decoder := json.NewDecoder(io.LimitReader(in, (2<<20)+1))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		return request, nil, errors.New("invalid worker request")
	}
	if decoder.InputOffset() > 2<<20 {
		return request, nil, errors.New("worker request header exceeds limit")
	}
	remaining := io.MultiReader(decoder.Buffered(), in)
	if request.Method == "stream" {
		reader := bufio.NewReader(remaining)
		separator, err := reader.ReadByte()
		if err != nil || separator != '\n' {
			return request, nil, errors.New("stream header requires a newline delimiter")
		}
		return request, reader, nil
	}
	trailing, err := io.ReadAll(io.LimitReader(remaining, (2<<20)-decoder.InputOffset()+1))
	if err != nil || int64(len(trailing))+decoder.InputOffset() > 2<<20 || strings.TrimSpace(string(trailing)) != "" {
		return request, nil, errors.New("invalid trailing worker request data")
	}
	return request, nil, nil
}

func (e *Engine) serveStream(w http.ResponseWriter, httpRequest *http.Request, stream *StreamRequest, input io.Reader) {
	controller := http.NewResponseController(w)
	deadline := time.Now().Add(30 * time.Minute)
	_ = controller.SetReadDeadline(deadline)
	_ = controller.SetWriteDeadline(deadline)
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Cache-Control", "no-store")
	respondError := func() {
		_ = json.NewEncoder(w).Encode(StreamResponse{Error: "backup transfer failed; verify policy, snapshot, worker state and transfer size limit"})
	}
	if stream == nil || stream.Request == nil || (stream.Action != "backup_download" && stream.Action != "backup_upload" && stream.Action != "backup_import_s3") || stream.Limit < 1 || stream.Limit > MaxTransferBytes {
		respondError()
		return
	}
	r := *stream.Request
	if ValidateRequest(r) != nil || (stream.Action == "backup_download" && r.Action != "restore") || (stream.Action != "backup_download" && (r.Action != "backup" || r.Backup == nil || r.Backup.Database == nil)) {
		respondError()
		return
	}
	e.mu.Lock()
	busy := e.busy(r.Workload) || e.draining || len(e.active) >= 4
	for _, op := range e.pending {
		if op.Workload == r.Workload {
			busy = true
		}
	}
	if busy {
		e.mu.Unlock()
		respondError()
		return
	}
	if e.active == nil {
		e.active = map[string]bool{}
	}
	e.active[r.Workload] = true
	e.mu.Unlock()
	defer func() { e.mu.Lock(); delete(e.active, r.Workload); e.mu.Unlock() }()
	if stream.Action != "backup_upload" {
		var extra [1]byte
		if n, err := input.Read(extra[:]); n != 0 || err != io.EOF {
			respondError()
			return
		}
	}
	ctx, cancel := context.WithDeadline(httpRequest.Context(), deadline)
	defer cancel()
	e.backupMu.Lock()
	defer e.backupMu.Unlock()
	if err := e.startBackupTransfer(ctx, r); err != nil {
		respondError()
		return
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
		defer cancel()
		if e.recoverBackupTransfer(cleanup) != nil {
			panic(http.ErrAbortHandler)
		}
	}()
	if stream.Action != "backup_download" {
		response, err := e.receiveDatabaseImport(ctx, r, *stream, input)
		if err != nil {
			respondError()
			return
		}
		_ = json.NewEncoder(w).Encode(response)
		return
	}
	file, response, cleanup, err := e.backupDownload(ctx, r, stream.Limit)
	if err != nil {
		respondError()
		return
	}
	defer func() {
		if err := cleanup(); err != nil {
			panic(http.ErrAbortHandler)
		}
	}()
	defer file.Close()
	if err = json.NewEncoder(w).Encode(response); err != nil {
		return
	}
	if _, err = io.CopyN(w, file, response.Size); err != nil {
		panic(http.ErrAbortHandler)
	}
}

func (e *Engine) backupDownload(ctx context.Context, r Request, limit int64) (file *os.File, response StreamResponse, cleanup func() error, result error) {
	manifest, err := e.loadBackupManifest(ctx, r, io.Discard)
	if err != nil {
		return nil, response, nil, err
	}
	if manifest.Record.Workload != r.Workload || manifest.Record.Repository != r.Backup.Repository || manifest.Record.ID != r.BackupID || !snapshotIdentifier.MatchString(manifest.Record.Snapshot) || !reflect.DeepEqual(manifest.Record.Database, r.Backup.Database) {
		return nil, response, nil, errors.New("backup does not match this resource and policy")
	}
	if r.Backup.Remote != nil {
		if err = e.remoteBackupHelper(ctx, io.Discard); err != nil {
			return nil, response, nil, err
		}
	}
	stage := filepath.Join(e.Root, "backup-transfer")
	err = os.RemoveAll(stage)
	if err == nil {
		err = os.MkdirAll(stage, 0700)
	}
	if err != nil {
		return nil, response, nil, err
	}
	cleanup = func() error { return os.RemoveAll(stage) }
	defer func() {
		if result != nil {
			if file != nil {
				file.Close()
			}
			result = errors.Join(result, cleanup())
		}
	}()
	budget, err := transferDiskBudget(stage, limit)
	if err != nil {
		return nil, response, cleanup, err
	}
	file, err = os.OpenFile(filepath.Join(stage, "payload"), os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		return nil, response, cleanup, err
	}
	command := []string{"dump", manifest.Record.Snapshot, "/data", "--archive", "tar"}
	if manifest.Record.Database != nil {
		command = []string{"dump", databaseDataSnapshot(manifest), "/data/000/database.dump"}
	}
	digest := sha256.New()
	if err = e.resticInput(ctx, r, nil, false, strings.NewReader(""), &sizeBoundWriter{Writer: io.MultiWriter(file, digest), remaining: budget}, command...); err != nil {
		return file, response, cleanup, err
	}
	info, err := file.Stat()
	if err != nil || info.Size() == 0 {
		return file, response, cleanup, errors.New("backup download is empty or unreadable")
	}
	if _, err = file.Seek(0, io.SeekStart); err != nil {
		return file, response, cleanup, err
	}
	response.Size, response.SHA256 = info.Size(), hex.EncodeToString(digest.Sum(nil))
	return file, response, cleanup, nil
}

func ReadStreamResponse(input io.Reader) (StreamResponse, *bufio.Reader, error) {
	reader := bufio.NewReaderSize(input, 8192)
	line, err := reader.ReadSlice('\n')
	var response StreamResponse
	if err != nil || len(line) > 8192 || json.Unmarshal(line, &response) != nil {
		return response, nil, errors.New("invalid worker stream response")
	}
	if response.Error != "" {
		return response, nil, errors.New(response.Error)
	}
	if response.Size < 1 || response.Size > MaxTransferBytes || !snapshotIdentifier.MatchString(response.SHA256) {
		return response, nil, errors.New("invalid worker stream metadata")
	}
	return response, reader, nil
}

func (e *Engine) receiveDatabaseImport(ctx context.Context, r Request, stream StreamRequest, input io.Reader) (response StreamResponse, result error) {
	if stream.Action == "backup_upload" && (stream.Size < 1 || stream.Size > stream.Limit) {
		return response, errors.New("upload size required within transfer limit")
	}
	if stream.Action == "backup_import_s3" && (stream.SourceStorage == nil || stream.SourceStorage.Protocol != "s3" || ValidateStorage(stream.SourceStorage) != nil || ValidateImportObjectKey(stream.ObjectKey) != nil) {
		return response, errors.New("configured S3 object required")
	}
	stage := filepath.Join(e.Root, "backup-transfer")
	if err := os.RemoveAll(stage); err != nil {
		return response, err
	}
	if err := os.MkdirAll(filepath.Join(stage, "input"), 0700); err != nil {
		return response, err
	}
	defer func() { result = errors.Join(result, os.RemoveAll(stage)) }()
	budget, err := transferDiskBudget(stage, stream.Limit)
	if err != nil || (stream.Action == "backup_upload" && stream.Size > budget) {
		return response, errors.New("insufficient free worker disk space for the dump and staging reserve")
	}
	dump := filepath.Join(stage, "input", "database.dump")
	file, err := os.OpenFile(dump, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return response, err
	}
	digest := sha256.New()
	writer := &sizeBoundWriter{Writer: io.MultiWriter(file, digest), remaining: budget}
	var copied int64
	if stream.Action == "backup_import_s3" {
		copied, err = e.readS3Import(ctx, r, *stream.SourceStorage, stream.ObjectKey, stage, writer, budget)
	} else {
		copied, err = io.Copy(writer, io.LimitReader(input, stream.Limit+1))
		if err == nil && copied != stream.Size {
			err = errors.New("uploaded dump length does not match declared size")
		}
	}
	if closeErr := file.Close(); closeErr != nil {
		err = errors.Join(err, closeErr)
	}
	if err != nil || copied < 1 {
		return response, errors.New("dump is empty, truncated, oversized or unreadable")
	}
	if err = e.createImportedDatabaseBackup(ctx, r, dump); err != nil {
		return response, err
	}
	return StreamResponse{Size: copied, SHA256: hex.EncodeToString(digest.Sum(nil)), BackupID: r.ID}, nil
}

func transferDiskBudget(stage string, limit int64) (int64, error) {
	free, err := freeBytes(stage)
	const reserve = 256 << 20
	if err != nil || free <= reserve {
		return 0, errors.New("worker staging requires at least 256 MiB of free reserve")
	}
	return min(limit, free-reserve), nil
}

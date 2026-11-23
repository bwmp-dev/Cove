package worker

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

func (e *Engine) serveMoveStream(w http.ResponseWriter, r *http.Request, stream *StreamRequest, input io.Reader) {
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Cache-Control", "no-store")
	controller := http.NewResponseController(w)
	deadline := time.Now().Add(30 * time.Minute)
	_ = controller.SetReadDeadline(deadline)
	_ = controller.SetWriteDeadline(deadline)
	fail := func(cause ...error) {
		message := "move transfer could not be confirmed"
		if len(cause) > 0 && cause[0] != nil {
			message = cause[0].Error()
		}
		_ = json.NewEncoder(w).Encode(StreamResponse{Error: message})
	}
	if stream == nil || stream.Move == nil || !identifier.MatchString(stream.Move.ID) || !workloadIdentifier.MatchString(stream.Move.Workload) {
		fail()
		return
	}
	request := *stream.Move
	if err := e.verifyMoveDaemon(r.Context(), request.Daemon); err != nil {
		fail(err)
		return
	}
	switch stream.Action {
	case "move_upload":
		state, err := e.receiveMove(r.Context(), request, stream.Size, stream.SHA256, input)
		if err != nil || state == nil {
			fail(err)
			return
		}
		_ = json.NewEncoder(w).Encode(StreamResponse{Size: state.Bytes, BackupID: state.ID, SHA256: state.SHA256})
	case "move_download":
		var extra [1]byte
		if n, err := input.Read(extra[:]); n != 0 || err != io.EOF {
			fail()
			return
		}
		e.moveMu.Lock()
		defer e.moveMu.Unlock()
		j, err := e.readMove(request.Workload)
		if err != nil || j.ID != request.ID || j.Role != "source" || j.Phase != "exported" || j.Bytes < 1 || j.Bytes > moveBundleLimit {
			fail()
			return
		}
		file, err := os.Open(filepath.Join(e.moveDirectory(j.ID), "bundle.tar"))
		if err != nil {
			fail()
			return
		}
		defer file.Close()
		info, err := file.Stat()
		if err != nil || info.Size() != j.Bytes {
			fail()
			return
		}
		if err = json.NewEncoder(w).Encode(StreamResponse{Size: j.Bytes, SHA256: j.SHA256}); err != nil {
			return
		}
		_, _ = io.CopyN(w, file, j.Bytes)
	default:
		fail()
	}
}

func discardMovePayload(input io.Reader, size int64) error {
	if _, err := io.CopyN(io.Discard, input, size); err != nil {
		return err
	}
	var extra [1]byte
	if n, err := input.Read(extra[:]); n != 0 || !errors.Is(err, io.EOF) {
		return errors.New("move payload length mismatch")
	}
	return nil
}

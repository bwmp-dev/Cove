package worker

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

const MaxFileTransferBytes int64 = 1 << 30

type fileTransfer struct {
	input   io.Reader
	limit   int64
	size    int64
	consume func(StreamResponse, io.Reader) error
}

func runFileTransfer(ctx context.Context, command *exec.Cmd, transfer *fileTransfer) error {
	if transfer.input != nil {
		command.Stdin = io.LimitReader(transfer.input, transfer.size+1)
	}
	output, err := command.StdoutPipe()
	if err != nil {
		return err
	}
	command.Stderr = &limitedBuffer{Limit: 8192}
	if err = command.Start(); err != nil {
		return err
	}
	reader := bufio.NewReaderSize(output, 256)
	line, readErr := reader.ReadSlice('\n')
	var meta StreamResponse
	fields := strings.Fields(string(line))
	if readErr == nil && len(fields) == 2 {
		meta.Size, readErr = strconv.ParseInt(fields[0], 10, 64)
		meta.SHA256 = fields[1]
	} else {
		readErr = errors.New("invalid file transfer receipt")
	}
	if readErr == nil && (meta.Size < 0 || meta.Size > transfer.limit || !snapshotIdentifier.MatchString(meta.SHA256) || transfer.input != nil && meta.Size != transfer.size) {
		readErr = errors.New("invalid file transfer size or digest")
	}
	if readErr == nil {
		readErr = transfer.consume(meta, reader)
	}
	if readErr != nil {
		_ = command.Process.Kill()
	}
	return errors.Join(readErr, command.Wait(), ctx.Err())
}

func (e *Engine) serveFileStream(w http.ResponseWriter, request *http.Request, stream *StreamRequest, input io.Reader) {
	deadline := time.Now().Add(30 * time.Minute)
	controller := http.NewResponseController(w)
	_ = controller.SetReadDeadline(deadline)
	_ = controller.SetWriteDeadline(deadline)
	ctx, cancel := context.WithDeadline(request.Context(), deadline)
	defer cancel()
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/octet-stream")
	started := false
	fail := func() {
		if started {
			panic(http.ErrAbortHandler)
		}
		_ = json.NewEncoder(w).Encode(StreamResponse{Error: "file transfer failed; check path, ownership, revision, size and worker state"})
	}
	if stream.Files == nil || stream.Limit < 1 || stream.Limit > MaxFileTransferBytes || !workloadIdentifier.MatchString(stream.Workload) || stream.Files.Data != nil {
		fail()
		return
	}
	files := *stream.Files
	files.Action = "read"
	transfer := &fileTransfer{limit: stream.Limit, consume: func(meta StreamResponse, body io.Reader) error {
		if err := json.NewEncoder(w).Encode(meta); err != nil {
			return err
		}
		started = true
		if stream.Action == "file_download" {
			if _, err := io.CopyN(w, body, meta.Size); err != nil {
				return err
			}
		}
		var extra [1]byte
		if n, err := body.Read(extra[:]); n != 0 || err != io.EOF {
			return errors.New("invalid file transfer trailer")
		}
		return nil
	}}
	if stream.Action == "file_upload" {
		if stream.Size < 0 || stream.Size > stream.Limit {
			fail()
			return
		}
		files.Action = "write"
		transfer.input, transfer.size = input, stream.Size
	} else if stream.Action != "file_download" {
		fail()
		return
	} else {
		var extra [1]byte
		if n, err := input.Read(extra[:]); n != 0 || err != io.EOF {
			fail()
			return
		}
	}
	if _, err := e.fileOperation(ctx, stream.Workload, files, transfer); err != nil {
		fail()
	}
}

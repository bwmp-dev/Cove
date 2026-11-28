package worker

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const snapshotBinaryLimit = 64 << 20

func (e *Engine) prepareSnapshotHostBinary() (string, error) {
	e.storageMu.Lock()
	defer e.storageMu.Unlock()
	if runtime.GOOS != "linux" {
		return "", errors.New("filesystem snapshots require Linux")
	}
	source := e.hostHelperBinary
	if source == "" {
		var err error
		source, err = os.Executable()
		if err != nil {
			return "", err
		}
	}
	info, err := os.Lstat(source)
	if err != nil || !info.Mode().IsRegular() || info.Size() < 1 || info.Size() > snapshotBinaryLimit {
		return "", errors.New("trusted snapshot helper executable unavailable")
	}
	file, err := os.Open(source)
	if err != nil {
		return "", err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, snapshotBinaryLimit+1))
	if err != nil || len(data) > snapshotBinaryLimit {
		return "", errors.New("snapshot helper exceeds its limit")
	}
	target := filepath.Join(e.Root, "host-snapshot-helper")
	if existing, err := os.Lstat(target); err == nil {
		if !existing.Mode().IsRegular() || existing.Size() > snapshotBinaryLimit || existing.Mode().Perm()&0077 != 0 || existing.Mode().Perm()&0100 == 0 {
			return "", errors.New("snapshot helper cache is not a regular bounded executable")
		}
		old, err := os.ReadFile(target)
		if err != nil {
			return "", err
		}
		if sha256.Sum256(old) == sha256.Sum256(data) {
			return target, nil
		}
	} else if !os.IsNotExist(err) {
		return "", err
	}
	temporary := target + ".next"
	if info, err := os.Lstat(temporary); err == nil && !info.Mode().IsRegular() {
		return "", errors.New("snapshot helper staging file is unsafe")
	} else if err != nil && !os.IsNotExist(err) {
		return "", err
	}
	if err := os.WriteFile(temporary, data, 0700); err != nil {
		return "", err
	}
	if err := os.Chmod(temporary, 0700); err != nil {
		return "", err
	}
	if err := os.Rename(temporary, target); err != nil {
		return "", err
	}
	return target, nil
}

func (e *Engine) callSnapshotHost(ctx context.Context, request snapshotHostRequest) (response snapshotHostResponse, result error) {
	binary, err := e.prepareSnapshotHostBinary()
	if err != nil {
		return response, err
	}
	owner, err := e.backupHelperOwner(true)
	if err != nil {
		return response, err
	}
	request.Owner, request.Root = owner, e.Root
	data, err := json.Marshal(request)
	if err != nil || len(data) > snapshotHostInputLimit {
		return response, errors.New("filesystem snapshot request exceeds its limit")
	}
	digest := sha256.Sum256([]byte(request.Workload + "/" + request.Operation + "/" + request.Action))
	name := "cove-fssnap-" + owner + "-" + hex.EncodeToString(digest[:8])
	// LVM udev completion cookies must share the host daemon's IPC namespace.
	args := []string{"docker", "run", "--rm", "--interactive", "--name", name, "--privileged", "--pid", "host", "--ipc", "host", "--network", "none", "--read-only", "--log-driver", "none", "--memory", "256m", "--pids-limit", "128", "--cpus", "1", "--label", "dev.cove.backup-owner=" + owner, "--label", "dev.cove.snapshot-workload=" + request.Workload, "--label", "dev.cove.operation=" + request.Operation, "--entrypoint", "nsenter", "busybox:1.37", "-t", "1", "-m", "-r", "-w", "--", binary, "snapshot-host"}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 20*time.Second)
		defer cancel()
		ids, err := e.output(cleanup, "docker", "ps", "-aq", "--filter", "name=^/"+name+"$")
		if err != nil {
			result = errors.Join(result, err)
			return
		}
		if ids == "" {
			return
		}
		if len(strings.Fields(ids)) != 1 {
			result = errors.Join(result, errors.New("ambiguous filesystem helper identity"))
			return
		}
		raw, err := e.output(cleanup, "docker", "inspect", "--format", "{{json .Config.Labels}}", name)
		var labels map[string]string
		if err != nil || json.Unmarshal([]byte(raw), &labels) != nil || labels["dev.cove.backup-owner"] != owner || labels["dev.cove.snapshot-workload"] != request.Workload || labels["dev.cove.operation"] != request.Operation {
			result = errors.Join(result, errors.New("filesystem helper ownership changed"))
			return
		}
		result = errors.Join(result, e.Run(cleanup, e.Root, []string{"docker", "rm", "-f", name}, nil, io.Discard))
	}()
	output := &limitedBuffer{Limit: snapshotHostInputLimit}
	if err := runInput(ctx, e.Root, args, nil, bytes.NewReader(data), output); err != nil {
		return response, errors.New("host snapshot helper failed; check host tools, privileges and recovery state")
	}
	if json.Unmarshal(output.data.Bytes(), &response) != nil {
		return response, errors.New("invalid host snapshot response")
	}
	if response.Error != "" {
		return response, errors.New(response.Error)
	}
	return response, nil
}

func RunFilesystemSnapshotHost(ctx context.Context, input io.Reader, output io.Writer) error {
	data, err := io.ReadAll(io.LimitReader(input, snapshotHostInputLimit+1))
	if err != nil || len(data) > snapshotHostInputLimit {
		return errors.New("filesystem snapshot request exceeds its limit")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var request snapshotHostRequest
	if decoder.Decode(&request) != nil || validateSnapshotHostRequest(request) != nil {
		return errors.New("invalid host filesystem request")
	}
	var trailing any
	if decoder.Decode(&trailing) != io.EOF {
		return errors.New("unexpected data after filesystem request")
	}
	var response snapshotHostResponse
	response, err = runSnapshotHost(ctx, request, snapshotHostCommand)
	if err != nil {
		response.Error = err.Error()
	}
	return json.NewEncoder(output).Encode(response)
}

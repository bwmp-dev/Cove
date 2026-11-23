package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"time"
)

type HostMaintenanceRequest struct {
	Action  string `json:"action"`
	Enabled bool   `json:"enabled"`
	Root    string `json:"root,omitempty"`
	Owner   string `json:"owner,omitempty"`
}

type hostMaintenanceResponse struct {
	Status *HostPatchStatus `json:"status,omitempty"`
	Error  string           `json:"error,omitempty"`
}

func validateHostMaintenance(request HostMaintenanceRequest) error {
	if runtime.GOOS != "linux" || os.Geteuid() != 0 || request.Action != "inspect" && request.Action != "security_policy" || !snapshotRootPath.MatchString(request.Root) || filepath.Clean(request.Root) != request.Root || request.Root == "/" || !snapshotOwnerID.MatchString(request.Owner) {
		return errors.New("invalid host maintenance request")
	}
	info, err := os.Lstat(request.Root)
	if err != nil || !info.IsDir() || info.Mode().Perm()&0022 != 0 {
		return errors.New("host worker directory is not protected")
	}
	engine := &Engine{Root: request.Root}
	owner, err := engine.backupHelperOwner(false)
	if err != nil || owner != request.Owner {
		return errors.New("host maintenance ownership could not be verified")
	}
	return nil
}

func RunHostMaintenance(ctx context.Context, input io.Reader, output io.Writer) error {
	decoder := json.NewDecoder(io.LimitReader(input, 8193))
	decoder.DisallowUnknownFields()
	var request HostMaintenanceRequest
	if decoder.Decode(&request) != nil {
		return errors.New("invalid host maintenance request")
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		return errors.New("unexpected host maintenance data")
	}
	if err := validateHostMaintenance(request); err != nil {
		return err
	}
	response := hostMaintenanceResponse{}
	if request.Action == "security_policy" {
		if err := configureHostPatchPolicy(ctx, "/", request.Root, request.Owner, request.Enabled, snapshotHostCommand); err != nil {
			response.Error = err.Error()
		}
	}
	status, err := inspectHostPatches(ctx, "/", request.Owner, snapshotHostCommand)
	if err != nil {
		if response.Error != "" {
			response.Error += "; "
		}
		response.Error += err.Error()
	} else {
		response.Status = &status
	}
	return json.NewEncoder(output).Encode(response)
}

func (e *Engine) HostMaintenance(ctx context.Context, request HostMaintenanceRequest) (status *HostPatchStatus, result error) {
	if request.Root != "" || request.Owner != "" || request.Action != "inspect" && request.Action != "security_policy" {
		return nil, errors.New("invalid host maintenance options")
	}
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	if !e.backupMu.TryLock() {
		return nil, errors.New("wait for the worker's current backup or host maintenance request")
	}
	defer e.backupMu.Unlock()
	owner, err := e.backupHelperOwner(true)
	if err != nil {
		return nil, err
	}
	binary, err := e.prepareSnapshotHostBinary()
	if err != nil {
		return nil, err
	}
	request.Root, request.Owner = e.Root, owner
	data, err := json.Marshal(request)
	if err != nil || len(data) > 8192 {
		return nil, errors.New("host maintenance request exceeds its limit")
	}
	name := "cove-host-maintenance-" + owner
	args := []string{"docker", "run", "--rm", "--interactive", "--name", name, "--privileged", "--pid", "host", "--network", "none", "--read-only", "--log-driver", "none", "--memory", "256m", "--cpus", "1", "--pids-limit", "128", "--label", "dev.cove.backup-owner=" + owner, "--label", "dev.cove.operation=host-maintenance", "--entrypoint", "nsenter", "busybox:1.37", "-t", "1", "-m", "-r", "-w", "--", binary, "host-maintenance"}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		// A disconnected Docker client can leave its short-lived privileged helper running.
		result = errors.Join(result, e.removeBackupHelper(cleanup, owner, name, "host-maintenance"))
	}()
	out := &limitedBuffer{Limit: hostMetadataLimit}
	if err = runInput(ctx, e.Root, args, nil, bytes.NewReader(data), out); err != nil {
		return nil, errors.New("host maintenance inspection unavailable; verify the Linux host and worker privileges")
	}
	var response hostMaintenanceResponse
	if json.Unmarshal(out.data.Bytes(), &response) != nil {
		return nil, errors.New("invalid host maintenance response")
	}
	if response.Error != "" {
		return response.Status, errors.New(response.Error)
	}
	return response.Status, nil
}

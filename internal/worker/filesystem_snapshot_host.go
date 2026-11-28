package worker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"
)

const snapshotSessionLimit = 16
const snapshotLease = 35 * time.Minute
const snapshotHostInputLimit = 2 << 20

var snapshotRootPath = regexp.MustCompile(`^/[A-Za-z0-9_./-]+$`)
var snapshotContainerID = regexp.MustCompile(`^[a-f0-9]{12,64}$`)
var snapshotOwnerID = regexp.MustCompile(`^[a-f0-9]{32}$`)

type snapshotHostRequest struct {
	Action     string           `json:"action"`
	Root       string           `json:"root"`
	Owner      string           `json:"owner"`
	Workload   string           `json:"workload"`
	Operation  string           `json:"operation"`
	MaxBytes   int64            `json:"max_bytes"`
	Sources    []snapshotSource `json:"sources,omitempty"`
	Containers []string         `json:"containers,omitempty"`
}

type snapshotHostResource struct {
	Plan    snapshotMountPlan `json:"plan"`
	Name    string            `json:"name"`
	Mount   string            `json:"mount,omitempty"`
	Source  string            `json:"source,omitempty"`
	Created bool              `json:"created"`
}

type snapshotHostState struct {
	Version    int                    `json:"version"`
	Owner      string                 `json:"owner"`
	Root       string                 `json:"root"`
	Workload   string                 `json:"workload"`
	Operation  string                 `json:"operation"`
	CreatedAt  time.Time              `json:"created_at"`
	ExpiresAt  time.Time              `json:"expires_at"`
	Paused     bool                   `json:"paused"`
	Containers []string               `json:"containers"`
	Resources  []snapshotHostResource `json:"resources"`
}

type snapshotHostIndex struct {
	Owner     string   `json:"owner"`
	Workloads []string `json:"workloads"`
}

type snapshotHostResponse struct {
	Pending   bool                `json:"pending,omitempty"`
	Error     string              `json:"error,omitempty"`
	Plans     []snapshotMountPlan `json:"plans,omitempty"`
	Sources   []string            `json:"sources,omitempty"`
	Available bool                `json:"available"`
	Reason    string              `json:"reason,omitempty"`
}

func snapshotHostCommand(ctx context.Context, args ...string) (string, error) {
	if len(args) == 0 {
		return "", errors.New("missing filesystem command")
	}
	command := exec.CommandContext(ctx, args[0], args[1:]...)
	command.Dir = "/"
	output := &limitedBuffer{Limit: 2 << 20}
	command.Stdout = output
	command.Stderr = io.Discard
	if err := command.Run(); err != nil {
		return "", fmt.Errorf("host %s command failed: %w", path.Base(args[0]), err)
	}
	return output.data.String(), nil
}

func snapshotUnitName(owner string) string {
	digest := sha256.Sum256([]byte("cove filesystem snapshots v1:" + owner))
	return "cove-snapshot-" + hex.EncodeToString(digest[:16])
}

func snapshotScope(root string) string { return path.Join(root, "filesystem-snapshots") }
func snapshotStateFile(root, workload string) string {
	return path.Join(root, "workloads", workload, "filesystem-snapshot.json")
}
func snapshotHostBinary(root string) string { return path.Join(root, "host-snapshot-helper") }

func readSnapshotMetadata(filename string, value any) error {
	info, err := os.Lstat(filename)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() > snapshotHostInputLimit || info.Mode().Perm()&0022 != 0 {
		return errors.New("invalid filesystem recovery metadata")
	}
	file, err := os.Open(filename)
	if err != nil {
		return err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, snapshotHostInputLimit+1))
	if err != nil || len(data) > snapshotHostInputLimit {
		return errors.New("filesystem recovery metadata exceeds its limit")
	}
	return json.Unmarshal(data, value)
}

func validateSnapshotHostRequest(request snapshotHostRequest) error {
	if runtime.GOOS != "linux" || os.Geteuid() != 0 {
		return errors.New("filesystem snapshots require a Linux root worker")
	}
	rootInfo, rootErr := os.Lstat(request.Root)
	if rootErr != nil || !rootInfo.IsDir() || rootInfo.Mode()&os.ModeSymlink != 0 || rootInfo.Mode().Perm()&0022 != 0 {
		return errors.New("filesystem snapshot worker root is not protected")
	}
	if !snapshotRootPath.MatchString(request.Root) || path.Clean(request.Root) != request.Root || request.Root == "/" || !snapshotOwnerID.MatchString(request.Owner) || !workloadIdentifier.MatchString(request.Workload) || !identifier.MatchString(request.Operation) || len(request.Sources) > snapshotMountLimit || len(request.Containers) > 128 {
		return errors.New("invalid filesystem snapshot identity or bounds")
	}
	if err := validateVolumeSnapshot(&VolumeSnapshotConfig{MaxBytes: request.MaxBytes}); err != nil {
		return err
	}
	for _, id := range request.Containers {
		if !snapshotContainerID.MatchString(id) {
			return errors.New("invalid snapshot container identity")
		}
	}
	for _, source := range request.Sources {
		if len(source.Label) > 256 || strings.ContainsAny(source.Label, "\x00\r\n") {
			return errors.New("invalid snapshot mount label")
		}
	}
	engine := &Engine{Root: request.Root}
	owner, err := engine.backupHelperOwner(false)
	if err != nil || owner != request.Owner {
		return errors.New("filesystem snapshot owner does not match this worker")
	}
	return nil
}

func inspectSnapshotHost(ctx context.Context, request snapshotHostRequest, run snapshotCommand) (snapshotHostResponse, error) {
	response := snapshotHostResponse{Available: true, Plans: []snapshotMountPlan{}}
	if len(request.Sources) == 0 {
		return response, errors.New("no persistent storage was selected")
	}
	for _, source := range request.Sources {
		resolved, err := filepath.EvalSymlinks(source.Source)
		if err != nil || resolved != source.Source {
			return response, errors.New("snapshot sources must exist without symbolic links")
		}
		info, err := os.Stat(source.Source)
		if err != nil || !info.IsDir() {
			return response, errors.New("filesystem snapshots require directory mounts")
		}
		plan, err := inspectSnapshotMount(ctx, source, request.MaxBytes, run)
		if err != nil {
			return response, err
		}
		response.Plans = append(response.Plans, plan)
		if !plan.Available {
			response.Available = false
		}
	}
	if !response.Available {
		response.Reason = "At least one mount requires a stopped backup"
		return response, nil
	}
	if _, err := run(ctx, "systemctl", "show", "--property=Version", "--value"); err != nil {
		response.Available = false
		response.Reason = "A working host systemd manager is required for crash-independent snapshot cleanup"
	}
	return response, nil
}

func snapshotUnitContents(root, owner string) (string, string) {
	unit := snapshotUnitName(owner)
	service := "[Unit]\nDescription=Cove filesystem snapshot recovery\nAfter=docker.service\n[Service]\nType=oneshot\nUMask=0077\nTimeoutStartSec=3min\nExecStart=" + snapshotHostBinary(root) + " snapshot-reap " + root + "\n"
	timer := "[Unit]\nDescription=Reap expired Cove filesystem snapshots\n[Timer]\nOnBootSec=30s\nOnUnitInactiveSec=30s\nAccuracySec=1s\nUnit=" + unit + ".service\n[Install]\nWantedBy=timers.target\n"
	return service, timer
}

func installSnapshotUnits(ctx context.Context, root, owner string, run snapshotCommand) error {
	service, timer := snapshotUnitContents(root, owner)
	unit := snapshotUnitName(owner)
	for extension, contents := range map[string]string{".service": service, ".timer": timer} {
		filename := path.Join("/etc/systemd/system", unit+extension)
		if info, err := os.Lstat(filename); err == nil {
			if !info.Mode().IsRegular() || info.Size() > 65536 || info.Mode().Perm()&0022 != 0 {
				return errors.New("snapshot recovery unit is not a protected regular file")
			}
			old, err := os.ReadFile(filename)
			if err != nil || len(old) > 65536 || string(old) != contents {
				return errors.New("refusing to replace an unrelated snapshot recovery unit")
			}
		} else if !os.IsNotExist(err) {
			return err
		}
		if err := os.WriteFile(filename, []byte(contents), 0600); err != nil {
			return err
		}
	}
	if _, err := run(ctx, "systemctl", "daemon-reload"); err != nil {
		return err
	}
	_, err := run(ctx, "systemctl", "enable", "--now", unit+".timer")
	return err
}

func removeSnapshotUnits(ctx context.Context, root, owner string, run snapshotCommand) error {
	unit := snapshotUnitName(owner)
	service, timer := snapshotUnitContents(root, owner)
	haveUnits := false
	for extension, contents := range map[string]string{".service": service, ".timer": timer} {
		filename := path.Join("/etc/systemd/system", unit+extension)
		info, err := os.Lstat(filename)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil || !info.Mode().IsRegular() || info.Size() > 65536 {
			return errors.New("snapshot recovery unit ownership is unavailable")
		}
		haveUnits = true
		data, err := os.ReadFile(filename)
		if err != nil || len(data) > 65536 || string(data) != contents {
			return errors.New("snapshot recovery unit ownership changed")
		}
	}
	if !haveUnits {
		return nil
	}
	if _, err := os.Stat(path.Join("/etc/systemd/system", unit+".timer")); err == nil {
		if _, err := run(ctx, "systemctl", "disable", "--now", unit+".timer"); err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	for _, extension := range []string{".timer", ".service"} {
		if err := os.Remove(path.Join("/etc/systemd/system", unit+extension)); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	_, err := run(ctx, "systemctl", "daemon-reload")
	return err
}

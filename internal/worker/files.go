package worker

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const MaxFileBytes = 32768

var fileContainerID = regexp.MustCompile(`^[a-f0-9]{12,64}$`)
var fileVolumeName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,127}$`)

type FileRequest struct {
	Action   string  `json:"action"`
	Kind     string  `json:"kind"`
	Target   string  `json:"target"`
	Path     string  `json:"path"`
	Data     *[]byte `json:"data,omitempty"`
	Revision string  `json:"revision,omitempty"`
}
type FileTarget struct {
	Kind  string `json:"kind"`
	ID    string `json:"id"`
	Name  string `json:"name"`
	State string `json:"state"`
}
type FileEntry struct {
	Name string `json:"name"`
	Kind string `json:"kind"`
	Size int64  `json:"size"`
}
type FileResponse struct {
	Targets  []FileTarget `json:"targets"`
	Entries  []FileEntry  `json:"entries"`
	Data     []byte       `json:"data"`
	Revision string       `json:"revision"`
	Limited  bool         `json:"limited"`
	MaxBytes int          `json:"max_bytes"`
	Size     int64        `json:"size"`
}

func ValidateFileRequest(r FileRequest) error {
	if r.Data != nil && (r.Action != "write" || len(*r.Data) > MaxFileBytes) {
		return errors.New("file data is only accepted for writes up to 32 KiB")
	}
	if r.Action == "targets" {
		return nil
	}
	if r.Action != "list" && r.Action != "read" && r.Action != "write" && r.Action != "delete" && r.Action != "stat" {
		return errors.New("invalid file action")
	}
	if r.Kind != "container" && r.Kind != "volume" || r.Kind == "container" && !fileContainerID.MatchString(r.Target) || r.Kind == "volume" && !fileVolumeName.MatchString(r.Target) {
		return errors.New("select a container or owned volume")
	}
	if len(r.Path) > 1024 || !utf8.ValidString(r.Path) || strings.IndexFunc(r.Path, unicode.IsControl) >= 0 || strings.Contains(r.Path, "\\") || !strings.HasPrefix(r.Path, "/") || path.Clean(r.Path) != r.Path || strings.Contains(r.Path, "//") {
		return errors.New("use a clean absolute path without parent traversal or control characters")
	}
	for _, part := range strings.Split(r.Path, "/") {
		if part == ".." || part == "." {
			return errors.New("parent traversal is not allowed")
		}
	}
	if r.Kind == "container" {
		for _, blocked := range []string{"/proc", "/sys", "/dev", "/run/secrets"} {
			if r.Path == blocked || strings.HasPrefix(r.Path, blocked+"/") {
				return errors.New("special filesystems and secret mounts are not browsable")
			}
		}
	}
	if r.Action != "list" && r.Path == "/" {
		return errors.New("select a regular file or empty directory")
	}
	if r.Action == "write" {
		if r.Data == nil || len(*r.Data) > MaxFileBytes {
			return errors.New("file data is required and must not exceed 32 KiB")
		}
		if r.Revision != "" {
			digest, err := hex.DecodeString(r.Revision)
			if err != nil || len(digest) != 32 {
				return errors.New("invalid file revision")
			}
		}
	}
	return nil
}

func (e *Engine) FileOperation(ctx context.Context, workload string, r FileRequest) (*FileResponse, error) {
	return e.fileOperation(ctx, workload, r, nil)
}

func (e *Engine) fileOperation(ctx context.Context, workload string, r FileRequest, transfer *fileTransfer) (*FileResponse, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !workloadIdentifier.MatchString(workload) {
		return nil, errors.New("invalid workload")
	}
	validation := r
	if transfer != nil && r.Action == "write" {
		empty := []byte{}
		validation.Data = &empty
	}
	if err := ValidateFileRequest(validation); err != nil {
		return nil, err
	}
	e.mu.Lock()
	busy := e.busy(workload) || e.draining || e.hostChanging || len(e.active) >= 4
	for _, op := range e.pending {
		if op.Workload == workload {
			busy = true
		}
	}
	if busy {
		e.mu.Unlock()
		return nil, errors.New("wait for active operations or worker maintenance before file access")
	}
	if e.active == nil {
		e.active = map[string]bool{}
	}
	e.active[workload] = true
	e.mu.Unlock()
	defer func() { e.mu.Lock(); delete(e.active, workload); e.mu.Unlock() }()
	operationContext := ctx
	// Keep accepted mutations under the workload guard even if their caller disconnects.
	if r.Action == "write" || r.Action == "delete" {
		operationContext = context.WithoutCancel(ctx)
	}
	duration := 30 * time.Second
	commandDuration := 20 * time.Second
	limit := int64(MaxFileBytes)
	shellTimeout := "15"
	if transfer != nil || r.Action == "stat" {
		duration, commandDuration = 30*time.Minute, 30*time.Minute
		shellTimeout = "1790"
		if transfer != nil {
			limit = transfer.limit
		}
	}
	ctx, cancel := context.WithTimeout(operationContext, duration)
	defer cancel()
	containers, err := e.Containers(ctx, workload)
	if err != nil {
		return nil, errors.New("could not inspect resource containers")
	}
	if len(containers) > 32 {
		return nil, errors.New("file browsing supports at most 32 containers per resource")
	}
	result := &FileResponse{Targets: []FileTarget{}, Entries: []FileEntry{}, MaxBytes: MaxFileBytes}
	volumes := map[string]bool{}
	ownedContainer := false
	for _, c := range containers {
		result.Targets = append(result.Targets, FileTarget{Kind: "container", ID: c.ID, Name: c.Name, State: c.State})
		if r.Kind == "container" && r.Target == c.ID && c.State == "running" {
			ownedContainer = true
		}
		mountJSON, err := e.output(ctx, "docker", "inspect", "--format", "{{json .Mounts}}", c.ID)
		if err != nil {
			return nil, errors.New("could not inspect resource mounts")
		}
		var mounts []struct{ Type, Name string }
		if json.Unmarshal([]byte(mountJSON), &mounts) != nil {
			return nil, errors.New("invalid mount report")
		}
		for _, mount := range mounts {
			if mount.Type != "volume" || volumes[mount.Name] {
				continue
			}
			if !fileVolumeName.MatchString(mount.Name) {
				result.Limited = true
				continue
			}
			if len(volumes) >= 128 {
				result.Limited = true
				continue
			}
			owner, err := e.output(ctx, "docker", "volume", "inspect", "--format", `{{index .Labels "com.docker.compose.project"}}`, mount.Name)
			if err != nil {
				return nil, errors.New("could not inspect volume ownership")
			}
			if owner == "cove-"+workload {
				volumes[mount.Name] = true
				result.Targets = append(result.Targets, FileTarget{Kind: "volume", ID: mount.Name, Name: mount.Name, State: "available"})
			}
		}
	}
	if r.Action == "targets" {
		return result, nil
	}
	if r.Kind == "container" && !ownedContainer || r.Kind == "volume" && !volumes[r.Target] {
		return nil, errors.New("target is not a running resource container or an attached Cove-owned volume")
	}
	args := []string{"docker", "exec", "--interactive", "--user", "0", r.Target, "timeout", "-k", "2", shellTimeout, "sh", "-ec", fileScript, "cove", r.Action, r.Path, "/", r.Revision}
	helper := ""
	if r.Kind == "volume" {
		if _, inspectErr := e.output(ctx, "docker", "image", "inspect", "restic/restic:0.19.1"); inspectErr != nil {
			if err = e.Run(ctx, e.Root, []string{"docker", "pull", "restic/restic:0.19.1"}, nil, io.Discard); err != nil {
				return nil, errors.New("could not prepare the isolated file helper")
			}
		}
		if r.Action == "write" || r.Action == "delete" {
			users, err := e.output(ctx, "docker", "ps", "-q", "--filter", "volume="+r.Target)
			if err != nil {
				return nil, errors.New("could not inspect volume users")
			}
			if strings.TrimSpace(users) != "" {
				return nil, errors.New("stop all containers using this volume before changing files")
			}
		}
		mount := "type=volume,source=" + r.Target + ",target=/data"
		if r.Action == "read" || r.Action == "list" || r.Action == "stat" {
			mount += ",readonly"
		}
		nonce := make([]byte, 12)
		if _, err = rand.Read(nonce); err != nil {
			return nil, err
		}
		helper = "cove-files-" + hex.EncodeToString(nonce)
		args = []string{"docker", "run", "--rm", "--interactive", "--name", helper, "--label", "cove.managed=files", "--label", "dev.cove.workload=" + workload, "--network", "none", "--read-only", "--cap-drop", "ALL", "--cap-add", "DAC_OVERRIDE", "--cap-add", "FOWNER", "--security-opt", "no-new-privileges", "--mount", mount, "--entrypoint", "timeout", "restic/restic:0.19.1", "-k", "2", shellTimeout, "sh", "-ec", fileScript, "cove", r.Action, r.Path, "/data", r.Revision}
	}
	out := &limitedBuffer{Limit: 256 * 1024}
	expected := int64(0)
	if r.Data != nil {
		expected = int64(len(*r.Data))
	}
	if r.Action == "stat" {
		limit = MaxFileTransferBytes
	}
	streaming := "false"
	if transfer != nil {
		expected, streaming = transfer.size, "true"
	}
	args = append(args, strconv.FormatInt(expected, 10), strconv.FormatInt(limit, 10), streaming)
	commandContext, commandCancel := context.WithTimeout(operationContext, commandDuration)
	defer commandCancel()
	if transfer != nil || r.Action == "write" {
		command := exec.CommandContext(commandContext, args[0], args[1:]...)
		configureCommand(command)
		command.Dir = e.Root
		command.Env = os.Environ()
		if transfer != nil {
			err = runFileTransfer(commandContext, command, transfer)
		} else {
			command.Stdin = bytes.NewReader(*r.Data)
			command.Stdout = out
			command.Stderr = out
			err = command.Run()
		}
	} else {
		err = e.Run(commandContext, e.Root, args, nil, out)
	}
	if err != nil {
		if helper != "" && commandContext.Err() != nil {
			cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()
			if cleanupErr := e.Run(cleanup, e.Root, []string{"docker", "rm", "--force", helper}, nil, &limitedBuffer{Limit: 4096}); cleanupErr != nil {
				return nil, errors.New("file operation failed; helper cleanup could not be confirmed")
			}
		}
		return nil, errors.New("file operation failed; verify the path, revision, permissions and required shell tools")
	}
	if transfer != nil {
		return result, nil
	}
	if r.Action == "read" {
		result.Data, err = base64.StdEncoding.DecodeString(strings.TrimSpace(out.data.String()))
		if err != nil || len(result.Data) > MaxFileBytes {
			return nil, errors.New("invalid or oversized file response")
		}
		hash := sha256.Sum256(result.Data)
		result.Revision = hex.EncodeToString(hash[:])
	}
	if r.Action == "write" {
		hash := sha256.Sum256(*r.Data)
		result.Revision = hex.EncodeToString(hash[:])
	}
	if r.Action == "stat" {
		fields := strings.Fields(out.data.String())
		if len(fields) != 2 || !snapshotIdentifier.MatchString(fields[1]) {
			return nil, errors.New("invalid file metadata")
		}
		result.Size, err = strconv.ParseInt(fields[0], 10, 64)
		if err != nil || result.Size < 0 || result.Size > MaxFileTransferBytes {
			return nil, errors.New("invalid file size")
		}
		result.Revision = fields[1]
	}
	if r.Action == "list" {
		for _, line := range strings.Split(strings.TrimSpace(out.data.String()), "\n") {
			if line == "" {
				continue
			}
			if line == "LIMIT" {
				result.Limited = true
				continue
			}
			fields := strings.Split(line, "\t")
			if len(fields) != 3 {
				return nil, errors.New("invalid file listing")
			}
			name, err := base64.StdEncoding.DecodeString(fields[2])
			size, parseErr := strconv.ParseInt(fields[1], 10, 64)
			if err != nil || parseErr != nil || size < 0 || !utf8.Valid(name) || strings.IndexFunc(string(name), unicode.IsControl) >= 0 || strings.ContainsAny(string(name), "/\\") {
				result.Limited = true
				continue
			}
			result.Entries = append(result.Entries, FileEntry{Name: string(name), Kind: fields[0], Size: size})
		}
	}
	return result, nil
}

const fileScript = `action=$1; relative=$2; root=$3; revision=$4; expected=$5; limit=$6; streaming=$7
target="$root$relative"
if [ "$root" = / ]; then target=$relative; fi
part=$root
rest=${relative#/}
while [ -n "$rest" ]; do
 component=${rest%%/*}
 if [ "$part" = / ]; then part="/$component"; else part="$part/$component"; fi
 [ ! -L "$part" ] || exit 1
 if [ "$rest" = "$component" ]; then break; fi
 rest=${rest#*/}
done
case "$action" in
list)
 [ -d "$target" ] || exit 1
 count=0
 for entry in "$target"/* "$target"/.[!.]* "$target"/..?*; do
  [ -e "$entry" ] || [ -L "$entry" ] || continue
  if [ "$count" -ge 200 ]; then printf 'LIMIT\n'; break; fi
  kind=other; size=0
  if [ -L "$entry" ]; then kind=link; elif [ -d "$entry" ]; then kind=directory; elif [ -f "$entry" ]; then kind=file; size=$(stat -c %s "$entry"); fi
  name=${entry##*/}
  encoded=$(printf %s "$name" | base64 | tr -d '\n')
  printf '%s\t%s\t%s\n' "$kind" "$size" "$encoded"
  count=$((count+1))
 done;;
read|stat)
 [ -f "$target" ] || exit 1
 exec 3< "$target"
 [ -f /proc/self/fd/3 ] || exit 1
 size=$(stat -Lc %s /proc/self/fd/3); [ "$size" -le "$limit" ] || exit 1
 if [ "$streaming" = true ] || [ "$action" = stat ]; then
  actual=$(sha256sum /proc/self/fd/3)
  printf '%s %s\n' "$size" "${actual%% *}"
  if [ "$action" != stat ]; then head -c "$size" <&3; fi
 else head -c "$((limit+1))" <&3 | base64 | tr -d '\n'; fi;;
write)
 parent=${target%/*}; [ -n "$parent" ] || parent=/; [ -d "$parent" ] || exit 1
 if [ -e "$target" ]; then
  [ -f "$target" ] && [ "$(stat -c %s "$target")" -le "$limit" ] && [ -n "$revision" ] || exit 1
  actual=$(sha256sum "$target"); [ "${actual%% *}" = "$revision" ] || exit 1
 else [ -z "$revision" ] || exit 1; fi
 umask 077
 tmp=$(mktemp "$parent/.cove-edit.XXXXXX")
 trap 'rm -f "$tmp"' EXIT HUP INT TERM
 head -c "$((limit+1))" > "$tmp"
 [ "$(stat -c %s "$tmp")" -eq "$expected" ] && [ "$expected" -le "$limit" ] || exit 1
 if [ -n "$revision" ]; then
  [ -f "$target" ] && [ ! -L "$target" ] || exit 1
  # Keep the inode so ownership, ACLs, labels and hard links survive edits.
  exec 3<> "$target"
  [ -f /proc/self/fd/3 ] || exit 1
  actual=$(sha256sum /proc/self/fd/3); [ "${actual%% *}" = "$revision" ] || exit 1
  cat "$tmp" >&3
  truncate -s "$(stat -c %s "$tmp")" /proc/self/fd/3
  exec 3>&-
 else ln "$tmp" "$target"; fi
 if [ "$streaming" = true ]; then
  actual=$(sha256sum "$tmp")
  printf '%s %s\n' "$expected" "${actual%% *}"
 fi;;
delete)
 if [ -d "$target" ]; then rmdir "$target"; else [ -f "$target" ] && rm -f "$target"; fi;;
*) exit 1;;
esac`

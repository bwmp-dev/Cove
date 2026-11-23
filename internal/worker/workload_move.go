package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"
)

const moveBundleLimit int64 = 64 << 30
const moveMetadataLimit int64 = 64 << 20

type MoveRequest struct {
	Daemon   string `json:"daemon,omitempty"`
	ID       string `json:"id"`
	Workload string `json:"workload"`
	Action   string `json:"action"`
}

type MoveVolume struct {
	Name   string            `json:"name"`
	Labels map[string]string `json:"labels"`
}

type MovePlan struct {
	Root         string       `json:"root"`
	Daemon       string       `json:"daemon"`
	Architecture string       `json:"architecture"`
	Volumes      []MoveVolume `json:"volumes"`
	Containers   int          `json:"containers"`
	Running      int          `json:"running"`
	MaxBytes     int64        `json:"max_bytes"`
}

type moveContainer struct {
	ID      string
	Service string
	Running bool
	Restart string
}

type MoveState struct {
	ID        string    `json:"id"`
	Workload  string    `json:"workload"`
	Role      string    `json:"role"`
	Phase     string    `json:"phase"`
	Bytes     int64     `json:"bytes"`
	SHA256    string    `json:"sha256"`
	UpdatedAt time.Time `json:"updated_at"`
	Plan      *MovePlan `json:"plan,omitempty"`
}

type moveJournal struct {
	MoveState
	Containers []moveContainer
	Current    deployedState
}

func (e *Engine) movePath(workload string) string {
	return filepath.Join(e.Root, "moves", workload+".json")
}
func (e *Engine) moveDirectory(id string) string { return filepath.Join(e.Root, "moves", "bundle-"+id) }
func (e *Engine) readMove(workload string) (moveJournal, error) {
	var j moveJournal
	if !workloadIdentifier.MatchString(workload) {
		return j, errors.New("invalid move resource")
	}
	file, err := os.Open(e.movePath(workload))
	if err != nil {
		return j, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, (1<<20)+1))
	if err == nil && (len(data) > 1<<20 || json.Unmarshal(data, &j) != nil) {
		err = errors.New("move journal exceeds its bound or is invalid")
	}
	if err == nil && (!identifier.MatchString(j.ID) || j.Workload != workload || (j.Role != "source" && j.Role != "target")) {
		err = errors.New("move journal identity is invalid")
	}
	return j, err
}
func (e *Engine) moving(workload string) bool {
	j, err := e.readMove(workload)
	if os.IsNotExist(err) {
		return false
	}
	return err != nil || j.Phase != "committed" && j.Phase != "rolled_back" && j.Phase != "departed" && j.Phase != "discarded"
}
func (e *Engine) saveMove(j *moveJournal) error {
	j.UpdatedAt = time.Now().UTC()
	if err := os.MkdirAll(filepath.Join(e.Root, "moves"), 0700); err != nil {
		return err
	}
	return writeJSON(e.movePath(j.Workload), j)
}

func (e *Engine) inspectMove(ctx context.Context, workload string) (*MovePlan, []moveContainer, deployedState, error) {
	var state deployedState
	data, err := os.ReadFile(filepath.Join(e.Root, "workloads", workload, "current.json"))
	if err != nil || len(data) > 2<<20 || json.Unmarshal(data, &state) != nil {
		return nil, nil, state, errors.New("move requires a deployed immutable release")
	}
	if state.Generation != "" || state.Rollout != nil || len(state.DatabaseMetadata) > 0 || e.composeProject(workload) != "cove-"+workload {
		return nil, nil, state, errors.New("node moves require a standard service release; rollout generations, managed databases and adopted projects cannot be moved")
	}
	if suspension, err := e.readSuspension(workload); err != nil || suspension != nil && suspension.Phase != "awake" && suspension.Options.Reason != "manual_stop" {
		return nil, nil, state, errors.New("resume the paused or sleeping resource before moving it")
	}
	prefix := filepath.Join(e.Root, "workloads", workload, "release-states") + string(filepath.Separator)
	if filepath.Clean(state.ComposePath) != state.ComposePath || !strings.HasPrefix(state.ComposePath, prefix) || !strings.HasSuffix(state.ComposePath, ".compose.json") {
		return nil, nil, state, errors.New("move requires a safe immutable release path")
	}
	if state.Private != nil || len(state.Networks) > 0 {
		return nil, nil, state, errors.New("detach private networking and shared networks before moving this resource")
	}
	if _, err = os.Lstat(filepath.Join(e.pitrDirectory(workload), "state.json")); !os.IsNotExist(err) {
		return nil, nil, state, errors.New("disable and delete PITR history before moving this resource")
	}
	for _, file := range []string{"storage-recovery.json", "copy-recovery.json", "filesystem-snapshot.json", "database-snapshot.json", "deploy-guard.json", "hook.json"} {
		if _, err = os.Lstat(filepath.Join(e.Root, "workloads", workload, file)); !os.IsNotExist(err) {
			return nil, nil, state, errors.New("settle resource recovery before moving it")
		}
	}
	identity, err := e.output(ctx, "docker", "info", "--format", "{{.ID}}|{{.Architecture}}")
	if err != nil {
		return nil, nil, state, err
	}
	parts := strings.Split(identity, "|")
	if len(parts) != 2 || len(identity) > 256 {
		return nil, nil, state, errors.New("Docker identity unavailable")
	}
	plan := &MovePlan{Root: e.Root, Daemon: parts[0], Architecture: parts[1], Volumes: []MoveVolume{}, MaxBytes: moveBundleLimit}
	containers, err := e.Containers(ctx, workload)
	if err != nil || len(containers) == 0 || len(containers) > 128 {
		return nil, nil, state, errors.New("move requires 1-128 existing containers")
	}
	owned := map[string]bool{}
	for _, c := range containers {
		owned[c.ID] = true
	}
	volumes := map[string]bool{}
	result := []moveContainer{}
	for _, container := range containers {
		if container.State != "running" && container.State != "exited" && container.State != "created" {
			return nil, nil, state, errors.New("unpause or repair containers before moving")
		}
		if container.Service == "" {
			return nil, nil, state, errors.New("container service identity unavailable")
		}
		raw, err := e.output(ctx, "docker", "inspect", "--format", "{{json .HostConfig}}", container.ID)
		if err != nil {
			return nil, nil, state, err
		}
		var host struct {
			Privileged                    bool
			NetworkMode, PidMode, IpcMode string
			Devices                       []json.RawMessage
			DeviceRequests                []json.RawMessage
			RestartPolicy                 struct {
				Name              string
				MaximumRetryCount int
			}
		}
		if json.Unmarshal([]byte(raw), &host) != nil {
			return nil, nil, state, errors.New("container configuration unavailable")
		}
		if host.Privileged || host.NetworkMode == "host" || (host.PidMode != "" && host.PidMode != "private") || host.IpcMode == "host" || strings.HasPrefix(host.IpcMode, "container:") || len(host.Devices) > 0 || len(host.DeviceRequests) > 0 {
			return nil, nil, state, errors.New("remove host privileges, GPU assignments and host namespaces before moving")
		}
		restart := host.RestartPolicy.Name
		if restart == "on-failure" && host.RestartPolicy.MaximumRetryCount > 0 {
			restart = fmt.Sprintf("on-failure:%d", host.RestartPolicy.MaximumRetryCount)
		}
		if restart == "" {
			restart = "no"
		}
		result = append(result, moveContainer{ID: container.ID, Service: container.Service, Running: container.State == "running", Restart: restart})
		if container.State == "running" {
			plan.Running++
		}
		plan.Containers++
		raw, err = e.output(ctx, "docker", "inspect", "--format", "{{json .Mounts}}", container.ID)
		if err != nil {
			return nil, nil, state, err
		}
		var mounts []backupMount
		if json.Unmarshal([]byte(raw), &mounts) != nil || len(mounts) > 64 {
			return nil, nil, state, errors.New("container mounts unavailable")
		}
		for _, mount := range mounts {
			if mount.Type == "tmpfs" {
				continue
			}
			if mount.Type == "bind" && strings.HasPrefix(filepath.Clean(mount.Source), filepath.Join(e.Root, "file-secrets", workload)+string(filepath.Separator)) && !mount.RW {
				continue
			}
			if mount.Type != "volume" {
				return nil, nil, state, errors.New("host bind mounts cannot be moved; use Cove-owned named volumes")
			}
			volumes[mount.Name] = true
		}
	}
	for _, key := range []string{"com.docker.compose.project=cove-" + workload, "dev.cove.restore=" + workload, "dev.cove.workload=" + workload} {
		names, err := e.output(ctx, "docker", "volume", "ls", "-q", "--filter", "label="+key)
		if err != nil {
			return nil, nil, state, err
		}
		for _, name := range strings.Fields(names) {
			volumes[name] = true
		}
	}
	backupVolume := "cove-backups-" + workload
	if raw, err := e.output(ctx, "docker", "volume", "inspect", "--format", `{{index .Labels "cove.backup"}}`, backupVolume); err == nil {
		if raw != workload {
			return nil, nil, state, errors.New("local backup repository ownership mismatch")
		}
		volumes[backupVolume] = true
	}
	maximumVolumes := 16
	if volumes[backupVolume] {
		maximumVolumes++
	}
	if len(volumes) > maximumVolumes {
		return nil, nil, state, errors.New("move supports at most 16 application volumes and one local backup repository")
	}
	for name := range volumes {
		if !safeDockerVolume(name) {
			return nil, nil, state, errors.New("unsupported volume name")
		}
		raw, err := e.output(ctx, "docker", "volume", "inspect", "--format", "{{json .Labels}}", name)
		if err != nil {
			return nil, nil, state, err
		}
		var labels map[string]string
		if json.Unmarshal([]byte(raw), &labels) != nil {
			return nil, nil, state, errors.New("volume ownership unavailable")
		}
		if labels["com.docker.compose.project"] != "cove-"+workload && labels["dev.cove.restore"] != workload && labels["cove.backup"] != workload && labels["dev.cove.workload"] != workload {
			return nil, nil, state, errors.New("external volumes must be detached before moving")
		}
		users, err := e.output(ctx, "docker", "ps", "-aq", "--filter", "volume="+name)
		if err != nil {
			return nil, nil, state, err
		}
		for _, id := range strings.Fields(users) {
			if !owned[id] {
				return nil, nil, state, errors.New("volume is shared with another resource")
			}
		}
		safeLabels := map[string]string{}
		for _, key := range []string{"com.docker.compose.project", "com.docker.compose.volume", "com.docker.compose.version", "dev.cove.restore", "cove.backup"} {
			if value, ok := labels[key]; ok {
				if len(value) > 256 {
					return nil, nil, state, errors.New("move ownership label exceeds its bound")
				}
				safeLabels[key] = value
			}
		}
		plan.Volumes = append(plan.Volumes, MoveVolume{Name: name, Labels: safeLabels})
	}
	sort.Slice(plan.Volumes, func(i, j int) bool { return plan.Volumes[i].Name < plan.Volumes[j].Name })
	return plan, result, state, nil
}

func safeDockerVolume(value string) bool {
	return len(value) > 0 && len(value) <= 128 && validMoveVolumeName(value)
}
func validMoveVolumeName(value string) bool {
	for i, c := range value {
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || i > 0 && (c == '_' || c == '.' || c == '-') {
			continue
		}
		return false
	}
	return true
}

func (e *Engine) Move(ctx context.Context, r MoveRequest) (*MoveState, error) {
	if !identifier.MatchString(r.ID) || !workloadIdentifier.MatchString(r.Workload) {
		return nil, errors.New("invalid move identity")
	}
	if err := e.verifyMoveDaemon(ctx, r.Daemon); err != nil {
		return nil, err
	}
	e.moveMu.Lock()
	defer e.moveMu.Unlock()
	if r.Action == "node" {
		identity, err := e.output(ctx, "docker", "info", "--format", "{{.ID}}|{{.Architecture}}")
		parts := strings.Split(identity, "|")
		if err != nil || len(parts) != 2 {
			return nil, errors.New("target Docker identity unavailable")
		}
		containers, err := e.Containers(ctx, r.Workload)
		if err != nil || len(containers) > 0 {
			return nil, errors.New("target already contains this resource")
		}
		if _, err = os.Lstat(filepath.Join(e.Root, "workloads", r.Workload)); !os.IsNotExist(err) {
			return nil, errors.New("target resource state already exists")
		}
		return &MoveState{ID: r.ID, Workload: r.Workload, Plan: &MovePlan{Root: e.Root, Daemon: parts[0], Architecture: parts[1], Volumes: []MoveVolume{}, MaxBytes: moveBundleLimit}}, nil
	}
	if r.Action == "plan" {
		plan, _, _, err := e.inspectMove(ctx, r.Workload)
		return &MoveState{ID: r.ID, Workload: r.Workload, Plan: plan}, err
	}
	j, err := e.readMove(r.Workload)
	if r.Action == "status" {
		if os.IsNotExist(err) {
			return &MoveState{ID: r.ID, Workload: r.Workload, Phase: "absent"}, nil
		}
		if err != nil {
			return nil, err
		}
		if j.ID != r.ID {
			if slices.Contains([]string{"committed", "rolled_back", "departed", "discarded"}, j.Phase) {
				return &MoveState{ID: r.ID, Workload: r.Workload, Phase: "absent"}, nil
			}
			return nil, errors.New("move identity changed")
		}
		return &j.MoveState, nil
	}
	if err == nil && j.ID != r.ID && slices.Contains([]string{"committed", "rolled_back", "departed", "discarded"}, j.Phase) {
		if err = os.RemoveAll(e.moveDirectory(j.ID)); err != nil {
			return nil, err
		}
		err = os.ErrNotExist
	}
	if os.IsNotExist(err) && r.Action == "freeze" {
		if err = e.admitMoveReceipt(); err != nil {
			return nil, err
		}
		e.mu.Lock()
		busy := e.busy(r.Workload) || e.draining
		for _, op := range e.pending {
			busy = busy || op.Workload == r.Workload
		}
		if !busy {
			e.active[r.Workload] = true
		}
		e.mu.Unlock()
		if busy {
			return nil, errors.New("resource has unfinished work")
		}
		defer func() { e.mu.Lock(); delete(e.active, r.Workload); e.mu.Unlock() }()
		plan, containers, current, err := e.inspectMove(ctx, r.Workload)
		if err != nil {
			return nil, err
		}
		j = moveJournal{MoveState: MoveState{ID: r.ID, Workload: r.Workload, Role: "source", Phase: "freezing", Plan: plan}, Containers: containers, Current: current}
		if err = e.saveMove(&j); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	}
	if j.ID != r.ID {
		return nil, errors.New("another move owns this resource")
	}
	switch r.Action {
	case "freeze":
		if j.Role != "source" || !slices.Contains([]string{"freezing", "frozen", "exported"}, j.Phase) {
			return nil, errors.New("source move cannot freeze in this state")
		}
		for _, c := range j.Containers {
			if err = e.Run(ctx, e.Root, []string{"docker", "update", "--restart=no", c.ID}, nil, io.Discard); err != nil {
				return nil, err
			}
			if err = e.Run(ctx, e.Root, []string{"docker", "stop", "--time", "30", c.ID}, nil, io.Discard); err != nil {
				return nil, err
			}
		}
		if j.Phase == "freezing" {
			j.Phase = "frozen"
			err = e.saveMove(&j)
		}
	case "export":
		if j.Role != "source" || !slices.Contains([]string{"frozen", "exported"}, j.Phase) {
			return nil, errors.New("source is not frozen")
		}
		err = e.exportMove(ctx, &j)
	case "start":
		if j.Role != "target" || !slices.Contains([]string{"imported", "starting", "started"}, j.Phase) {
			return nil, errors.New("target is not imported")
		}
		err = e.startMove(ctx, &j)
	case "fence":
		if j.Role != "target" || j.Phase == "committed" {
			return nil, errors.New("target move cannot be fenced")
		}
		err = e.fenceMove(ctx, &j)
	case "rollback":
		if j.Role != "source" || !slices.Contains([]string{"freezing", "frozen", "exported", "rolling_back", "rolled_back"}, j.Phase) {
			return nil, errors.New("source cannot roll back")
		}
		j.Phase = "rolling_back"
		if err = e.saveMove(&j); err != nil {
			return nil, err
		}
		for _, c := range j.Containers {
			if err = e.Run(ctx, e.Root, []string{"docker", "update", "--restart=" + c.Restart, c.ID}, nil, io.Discard); err != nil {
				return nil, err
			}
			if c.Running {
				if err = e.Run(ctx, e.Root, []string{"docker", "start", c.ID}, nil, io.Discard); err != nil {
					return nil, err
				}
			}
		}
		if err = e.applyIngress(ctx, r.Workload, j.Current.Routes, io.Discard); err == nil {
			j.Phase = "rolled_back"
			err = e.saveMove(&j)
			if err == nil {
				err = os.RemoveAll(e.moveDirectory(j.ID))
			}
		}
	case "commit":
		if j.Role != "target" || !slices.Contains([]string{"started", "committed"}, j.Phase) {
			return nil, errors.New("destination has not started")
		}
		j.Phase = "committed"
		err = e.saveMove(&j)
		if err == nil {
			err = os.RemoveAll(e.moveDirectory(j.ID))
		}
	case "discard":
		if j.Role != "target" || !slices.Contains([]string{"fenced", "discarding", "discarded"}, j.Phase) {
			return nil, errors.New("target must be fenced before discarding")
		}
		err = e.discardMove(ctx, &j)
	case "depart":
		if j.Role != "source" || !slices.Contains([]string{"exported", "departing", "departed"}, j.Phase) {
			return nil, errors.New("source cannot be removed")
		}
		err = e.departMove(ctx, &j)
	default:
		return nil, errors.New("invalid move action")
	}
	return &j.MoveState, err
}

func (e *Engine) verifyMoveDaemon(ctx context.Context, expected string) error {
	if expected == "" {
		return nil
	}
	actual, err := e.output(ctx, "docker", "info", "--format", "{{.ID}}")
	if err != nil || actual != expected {
		return errors.New("move endpoint Docker identity changed; reconnect the original node before recovering")
	}
	return nil
}

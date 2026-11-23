package worker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"
)

func moveVolumeIndex(index int) string { return fmt.Sprintf("%03d", index) }

func (e *Engine) installMove(ctx context.Context, j *moveJournal) error {
	for _, volume := range j.Plan.Volumes {
		if !safeDockerVolume(volume.Name) {
			return errors.New("move volume name is invalid")
		}
		if raw, err := e.output(ctx, "docker", "volume", "inspect", "--format", `{{index .Labels "dev.cove.move"}}`, volume.Name); err == nil && raw != j.ID {
			return errors.New("target volume already exists outside this move")
		}
	}
	containers, err := e.Containers(ctx, j.Workload)
	if err != nil || len(containers) > 0 {
		return errors.New("target already has resource containers")
	}
	for _, volume := range j.Plan.Volumes {
		args := []string{"docker", "volume", "create", "--label", "dev.cove.move=" + j.ID, "--label", "dev.cove.workload=" + j.Workload}
		for key, value := range volume.Labels {
			if !slices.Contains([]string{"com.docker.compose.project", "com.docker.compose.volume", "com.docker.compose.version", "dev.cove.restore", "cove.backup"}, key) || len(value) > 256 {
				return errors.New("move volume ownership labels are invalid")
			}
			args = append(args, "--label", key+"="+value)
		}
		if err = e.Run(ctx, e.Root, append(args, volume.Name), nil, io.Discard); err != nil {
			return err
		}
		if owner, err := e.output(ctx, "docker", "volume", "inspect", "--format", `{{index .Labels "dev.cove.move"}}`, volume.Name); err != nil || owner != j.ID {
			return errors.New("new target volume ownership could not be confirmed")
		}
	}
	if err = e.moveHelper(ctx, *j, true, []string{"tar", "-C", "/data", "-xpf", "/input.tar"}, io.Discard); err != nil {
		return err
	}
	if err = e.Run(ctx, e.Root, []string{"docker", "image", "load", "--input", filepath.Join(e.moveDirectory(j.ID), "images.tar")}, nil, io.Discard); err != nil {
		return err
	}
	base := filepath.Join(e.moveDirectory(j.ID), "metadata")
	err = filepath.WalkDir(base, func(filename string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil || !info.Mode().IsRegular() {
			return errors.New("move metadata changed type")
		}
		relative, err := filepath.Rel(base, filename)
		if err != nil {
			return err
		}
		destination := filepath.Join(e.Root, relative)
		if strings.HasPrefix(relative, "operations"+string(filepath.Separator)) && strings.HasSuffix(relative, ".json") {
			var record journal
			data, err := os.ReadFile(filename)
			if err != nil || json.Unmarshal(data, &record) != nil || record.Operation.Workload != j.Workload || record.Request.Workload != j.Workload {
				return errors.New("move operation journal crosses resource boundary")
			}
		}
		if err = moveMetadataParent(e.Root, relative); err != nil {
			return err
		}
		if info, err := os.Lstat(destination); err == nil {
			if !info.Mode().IsRegular() {
				return errors.New("target metadata changed type")
			}
			expected, err := fileSum(filename)
			if err != nil {
				return err
			}
			actual, err := fileSum(destination)
			if err != nil || expected != actual {
				return errors.New("target metadata conflicts with move receipt")
			}
			return nil
		} else if !os.IsNotExist(err) {
			return err
		}
		input, err := os.Open(filename)
		if err != nil {
			return err
		}
		defer input.Close()
		file, err := os.CreateTemp(filepath.Dir(destination), ".move-")
		if err != nil {
			return err
		}
		temporary := file.Name()
		defer os.Remove(temporary)
		_, err = io.Copy(file, input)
		err = errors.Join(err, file.Sync(), file.Close())
		if err != nil {
			return err
		}
		if err = os.Link(temporary, destination); err != nil {
			return err
		}
		if err = syncDirectory(filepath.Dir(destination)); err != nil {
			return err
		}
		if strings.HasPrefix(relative, "file-secrets"+string(filepath.Separator)) {
			return os.Chmod(destination, 0444)
		}
		return nil
	})
	if err != nil {
		return err
	}
	if err = e.tagMoveImages(ctx, j.Workload); err != nil {
		return err
	}
	j.Phase = "imported"
	return e.saveMove(j)
}

func moveMetadataParent(root, relative string) error {
	if !validMoveTarName(filepath.ToSlash(relative)) {
		return errors.New("invalid move metadata destination")
	}
	current := root
	for _, part := range strings.Split(filepath.Dir(relative), string(filepath.Separator)) {
		if part == "." {
			continue
		}
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if os.IsNotExist(err) {
			if err = os.Mkdir(current, 0700); err != nil {
				return err
			}
			continue
		}
		if err != nil {
			return err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return errors.New("move metadata parent is not a private directory")
		}
	}
	return nil
}

func (e *Engine) tagMoveImages(ctx context.Context, workload string) error {
	dir := filepath.Join(e.Root, "workloads", workload, "release-states")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".compose.json") {
			continue
		}
		id := strings.TrimSuffix(entry.Name(), ".compose.json")
		if !identifier.MatchString(id) {
			return errors.New("retained move release identifier is invalid")
		}
		data, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			return err
		}
		var config struct {
			Services map[string]struct{ Image string }
		}
		if json.Unmarshal(data, &config) != nil {
			return errors.New("retained release is invalid")
		}
		for name, service := range config.Services {
			hash := sha256.Sum256([]byte(name))
			tag := strings.ToLower("cove-release/" + workload + ":" + id + "-" + hex.EncodeToString(hash[:8]))
			if old, err := e.output(ctx, "docker", "image", "inspect", "--format", "{{.Id}}", tag); err == nil && old != service.Image {
				return errors.New("target retained image tag conflicts with another image")
			}
			if err = e.Run(ctx, e.Root, []string{"docker", "image", "tag", service.Image, tag}, nil, io.Discard); err != nil {
				return err
			}
		}
	}
	return nil
}

func (e *Engine) moveCompose(ctx context.Context, j moveJournal, command ...string) error {
	prefix := filepath.Join(e.Root, "workloads", j.Workload, "release-states") + string(filepath.Separator)
	if !strings.HasPrefix(j.Current.ComposePath, prefix) || !strings.HasSuffix(j.Current.ComposePath, ".compose.json") {
		return errors.New("move current release path is invalid")
	}
	env := []string{}
	for key, value := range j.Current.Environment {
		env = append(env, key+"="+value)
	}
	args := []string{"docker", "compose", "--project-name", "cove-" + j.Workload, "--file", j.Current.ComposePath}
	return e.Run(ctx, e.Root, append(args, command...), env, io.Discard)
}

func (e *Engine) startMove(ctx context.Context, j *moveJournal) error {
	if j.Phase == "started" {
		return e.checkMoveStarted(ctx, *j)
	}
	j.Phase = "starting"
	if err := e.saveMove(j); err != nil {
		return err
	}
	if len(j.Current.Routes) > 0 {
		if err := e.ensureIngress(ctx, io.Discard); err != nil {
			return err
		}
	}
	if err := e.moveCompose(ctx, *j, "create", "--no-build", "--pull", "never"); err != nil {
		return err
	}
	containers, err := e.Containers(ctx, j.Workload)
	if err != nil {
		return err
	}
	counts := map[string]int{}
	for _, c := range j.Containers {
		if c.Running {
			counts[c.Service]++
		}
	}
	sort.Slice(containers, func(i, k int) bool { return containers[i].ID < containers[k].ID })
	for _, c := range containers {
		if counts[c.Service] > 0 {
			counts[c.Service]--
			if err = e.Run(ctx, e.Root, []string{"docker", "start", c.ID}, nil, io.Discard); err != nil {
				return err
			}
		}
	}
	for _, count := range counts {
		if count != 0 {
			return errors.New("target service replicas do not match source")
		}
	}
	if err = e.checkMoveStarted(ctx, *j); err != nil {
		return err
	}
	if err = e.applyIngress(ctx, j.Workload, j.Current.Routes, io.Discard); err != nil {
		return err
	}
	j.Phase = "started"
	return e.saveMove(j)
}

func (e *Engine) checkMoveStarted(ctx context.Context, j moveJournal) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	expected := map[string]int{}
	for _, c := range j.Containers {
		if c.Running {
			expected[c.Service]++
		}
	}
	for {
		containers, err := e.Containers(ctx, j.Workload)
		if err != nil {
			return err
		}
		actual := map[string]int{}
		healthy := true
		for _, c := range containers {
			if c.State != "running" {
				continue
			}
			actual[c.Service]++
			raw, err := e.output(ctx, "docker", "inspect", "--format", "{{json .State}}", c.ID)
			if err != nil {
				return err
			}
			var state struct {
				Restarting bool
				Health     struct{ Status string }
			}
			if json.Unmarshal([]byte(raw), &state) != nil {
				return errors.New("target health unavailable")
			}
			healthy = healthy && !state.Restarting && (state.Health.Status == "" || state.Health.Status == "healthy")
		}
		matches := len(expected) == len(actual)
		for service, count := range expected {
			matches = matches && actual[service] == count
		}
		if matches && healthy {
			return nil
		}
		select {
		case <-ctx.Done():
			return errors.New("target containers did not reach source running state and health")
		case <-time.After(time.Second):
		}
	}
}

func (e *Engine) fenceMove(ctx context.Context, j *moveJournal) error {
	j.Phase = "fencing"
	if err := e.saveMove(j); err != nil {
		return err
	}
	containers, err := e.Containers(ctx, j.Workload)
	if err != nil {
		return err
	}
	for _, c := range containers {
		if err = e.Run(ctx, e.Root, []string{"docker", "update", "--restart=no", c.ID}, nil, io.Discard); err != nil {
			return err
		}
		if err = e.Run(ctx, e.Root, []string{"docker", "stop", "--time", "30", c.ID}, nil, io.Discard); err != nil {
			return err
		}
	}
	containers, err = e.Containers(ctx, j.Workload)
	if err != nil {
		return err
	}
	for _, c := range containers {
		if c.State == "running" || c.State == "restarting" || c.State == "paused" {
			return errors.New("target has not stopped")
		}
	}
	if err = e.applyIngress(ctx, j.Workload, nil, io.Discard); err != nil {
		return err
	}
	j.Phase = "fenced"
	return e.saveMove(j)
}

func (e *Engine) departMove(ctx context.Context, j *moveJournal) error {
	if j.Phase == "departed" {
		return nil
	}
	j.Phase = "departing"
	if err := e.saveMove(j); err != nil {
		return err
	}
	containers, err := e.Containers(ctx, j.Workload)
	if err != nil {
		return err
	}
	for _, c := range containers {
		if c.State == "running" || c.State == "restarting" || c.State == "paused" {
			return errors.New("source restarted; refusing committed source cleanup")
		}
		if err = e.Run(ctx, e.Root, []string{"docker", "rm", c.ID}, nil, io.Discard); err != nil {
			return err
		}
	}
	if err = e.applyIngress(ctx, j.Workload, nil, io.Discard); err != nil {
		return err
	}
	for _, volume := range j.Plan.Volumes {
		names, err := e.output(ctx, "docker", "volume", "ls", "--format", "{{.Name}}", "--filter", "name=^"+volume.Name+"$")
		if err != nil {
			return err
		}
		if names == "" {
			continue
		}
		raw, err := e.output(ctx, "docker", "volume", "inspect", "--format", "{{json .Labels}}", volume.Name)
		if err != nil {
			return err
		}
		var labels map[string]string
		if json.Unmarshal([]byte(raw), &labels) != nil {
			return errors.New("source volume ownership unavailable")
		}
		if labels["com.docker.compose.project"] != "cove-"+j.Workload && labels["dev.cove.restore"] != j.Workload && labels["cove.backup"] != j.Workload && labels["dev.cove.workload"] != j.Workload {
			return errors.New("source volume ownership changed")
		}
		if err = e.Run(ctx, e.Root, []string{"docker", "volume", "rm", volume.Name}, nil, io.Discard); err != nil {
			return err
		}
	}
	for _, dir := range []string{filepath.Join(e.Root, "workloads", j.Workload), filepath.Join(e.Root, "file-secrets", j.Workload), e.moveDirectory(j.ID)} {
		if err = os.RemoveAll(dir); err != nil {
			return err
		}
	}
	j.Phase = "departed"
	return e.saveMove(j)
}

func (e *Engine) discardMove(ctx context.Context, j *moveJournal) error {
	if j.Phase == "discarded" {
		return nil
	}
	j.Phase = "discarding"
	if err := e.saveMove(j); err != nil {
		return err
	}
	containers, err := e.Containers(ctx, j.Workload)
	if err != nil {
		return err
	}
	for _, c := range containers {
		if c.State == "running" || c.State == "restarting" || c.State == "paused" {
			return errors.New("target restarted; refusing rollback cleanup")
		}
		if err = e.Run(ctx, e.Root, []string{"docker", "rm", c.ID}, nil, io.Discard); err != nil {
			return err
		}
	}
	volumes := []MoveVolume{}
	if j.Plan != nil {
		volumes = j.Plan.Volumes
	}
	for _, volume := range volumes {
		names, err := e.output(ctx, "docker", "volume", "ls", "--format", "{{.Name}}", "--filter", "name=^"+volume.Name+"$")
		if err != nil {
			return err
		}
		if names == "" {
			continue
		}
		owner, err := e.output(ctx, "docker", "volume", "inspect", "--format", `{{index .Labels "dev.cove.move"}}`, volume.Name)
		if err != nil || owner != j.ID {
			return errors.New("rollback volume ownership changed")
		}
		if err = e.Run(ctx, e.Root, []string{"docker", "volume", "rm", volume.Name}, nil, io.Discard); err != nil {
			return err
		}
	}
	for _, dir := range []string{filepath.Join(e.Root, "workloads", j.Workload), filepath.Join(e.Root, "file-secrets", j.Workload), e.moveDirectory(j.ID)} {
		if err = os.RemoveAll(dir); err != nil {
			return err
		}
	}
	j.Phase = "discarded"
	return e.saveMove(j)
}

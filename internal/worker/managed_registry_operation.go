package worker

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"time"
)

type ManagedRegistryOperation struct {
	Mode       string                   `json:"mode"`
	Policy     ManagedRegistryRetention `json:"policy"`
	Protected  []string                 `json:"protected"`
	DeleteData bool                     `json:"delete_data"`
}

type ManagedRegistryResult struct {
	Storage      ManagedRegistryStorage `json:"storage"`
	Manifests    int                    `json:"manifests"`
	Deleted      int                    `json:"deleted"`
	Removed      bool                   `json:"removed"`
	DataRetained bool                   `json:"data_retained"`
}

type managedRegistryCleanup struct {
	Container string `json:"container"`
	Restart   string `json:"restart"`
	Retries   int    `json:"retries"`
	Running   bool   `json:"running"`
	Removing  bool   `json:"removing"`
}

func validateManagedRegistryOperation(r Request) error {
	if r.Action != "registry_maintenance" {
		if r.ManagedRegistry != nil {
			return errors.New("registry maintenance inputs require a registry operation")
		}
		return nil
	}
	if r.ManagedRegistry == nil || !certificateIdentifier.MatchString(r.Workload) {
		return errors.New("managed registry maintenance specification required")
	}
	minimal := Request{ID: r.ID, Workload: r.Workload, Action: r.Action, Kind: "service", ManagedRegistry: r.ManagedRegistry}
	if !reflect.DeepEqual(r, minimal) {
		return errors.New("registry maintenance accepts only its owned identity and protected references")
	}
	m := r.ManagedRegistry
	if m.Mode != "gc" && m.Mode != "remove" {
		return errors.New("unsupported registry maintenance mode")
	}
	if err := m.Policy.Validate(); err != nil {
		return err
	}
	if len(m.Protected) > MaxManagedRegistryReferences {
		return errors.New("protected registry inventory exceeds limit")
	}
	for _, ref := range m.Protected {
		repo, digest, ok := strings.Cut(ref, "@")
		if !ok || ValidateRegistryRepository(repo) != nil || !managedManifestDigest.MatchString(digest) {
			return errors.New("invalid protected registry reference")
		}
	}
	if m.DeleteData && (m.Mode != "remove" || len(m.Protected) != 0) {
		return errors.New("registry data deletion requires removal without protected references")
	}
	return nil
}

func registryMaintenanceName(r Request, role string) string {
	return "cove-registry-" + r.ID + "-" + role
}

func (e *Engine) registryMaintenanceContainer(ctx context.Context, r Request, role, image string, args []string, out io.Writer) error {
	name := registryMaintenanceName(r, role)
	command := []string{"docker", "run", "--name", name, "--pull", "never", "--network", "none", "--read-only", "--cap-drop", "ALL", "--security-opt", "no-new-privileges:true", "--cpus", "1", "--memory", "512m", "--pids-limit", "128", "--label", "cove.managed=registry-maintenance", "--label", "cove.registry-operation=" + r.ID, "--label", "cove.registry-workload=" + r.Workload, "--mount", "type=volume,src=cove-" + r.Workload + "_data,dst=/var/lib/registry"}
	if role == "inventory" {
		command = append(command, "--mount", "type=volume,src=cove-"+r.Workload+"_data,dst=/registry", "--mount", "type=bind,src="+filepath.Join(e.Root, "registry-maintenance", r.ID, "request.json")+",dst=/config/registry.json,readonly", "--entrypoint", "/cove-worker", image, "registry-storage")
	} else {
		command = append(command, "--env", "OTEL_TRACES_EXPORTER=none", "--env", "OTEL_METRICS_EXPORTER=none", "--entrypoint", "/bin/registry", image)
		command = append(command, args...)
	}
	return e.Run(ctx, e.Root, command, nil, out)
}

func (e *Engine) ownedRegistryVolume(ctx context.Context, id string) error {
	var volume struct {
		Name    string            `json:"Name"`
		Driver  string            `json:"Driver"`
		Options map[string]string `json:"Options"`
		Labels  map[string]string `json:"Labels"`
	}
	if err := e.imageJSON(ctx, nil, &volume, "docker", "volume", "inspect", "--format", "{{json .}}", "cove-"+id+"_data"); err != nil {
		return err
	}
	if volume.Name != "cove-"+id+"_data" || volume.Driver != "local" || len(volume.Options) != 0 || volume.Labels["dev.cove.managed-registry"] != id || volume.Labels["com.docker.compose.project"] != "cove-"+id || volume.Labels["com.docker.compose.volume"] != "data" {
		return errors.New("registry data volume ownership or storage driver mismatch")
	}
	return nil
}

func (e *Engine) performManagedRegistry(ctx context.Context, r Request, out io.Writer) (result error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	volumes, err := e.output(ctx, "docker", "volume", "ls", "-q", "--filter", "name=^cove-"+r.Workload+"_data$")
	if err != nil {
		return err
	}
	if strings.TrimSpace(volumes) != "" {
		if err = e.ownedRegistryVolume(ctx, r.Workload); err != nil {
			return err
		}
	} else if r.ManagedRegistry.Mode != "remove" {
		return errors.New("managed registry volume is missing")
	}
	containers, err := e.Containers(ctx, r.Workload)
	if err != nil {
		return err
	}
	if len(containers) > 1 || len(containers) == 0 && r.ManagedRegistry.Mode != "remove" {
		return errors.New("managed registry requires exactly one owned registry container")
	}
	var inspect struct {
		ID     string `json:"Id"`
		Config struct {
			Image  string            `json:"Image"`
			Labels map[string]string `json:"Labels"`
		} `json:"Config"`
		HostConfig struct {
			RestartPolicy struct {
				Name              string `json:"Name"`
				MaximumRetryCount int    `json:"MaximumRetryCount"`
			} `json:"RestartPolicy"`
		} `json:"HostConfig"`
		State struct {
			Running bool `json:"Running"`
		} `json:"State"`
	}
	if len(containers) > 0 {
		if err = e.imageJSON(ctx, nil, &inspect, "docker", "inspect", "--format", "{{json .}}", containers[0].ID); err != nil {
			return err
		}
		if inspect.Config.Image != ManagedRegistryImage || inspect.Config.Labels["dev.cove.managed-registry"] != r.Workload || inspect.Config.Labels["com.docker.compose.service"] != "registry" {
			return errors.New("registry container identity mismatch")
		}
	}
	e.mu.Lock()
	j, err := e.read(r.ID)
	if err == nil {
		j.RegistryCleanup = &managedRegistryCleanup{Container: inspect.ID, Running: inspect.State.Running, Restart: inspect.HostConfig.RestartPolicy.Name, Retries: inspect.HostConfig.RestartPolicy.MaximumRetryCount}
		err = writeJSON(e.path(r.ID), j)
	}
	e.mu.Unlock()
	if err != nil {
		return err
	}
	defer func() {
		cleanup, stop := context.WithTimeout(context.Background(), 2*time.Minute)
		defer stop()
		j, err := e.read(r.ID)
		if err == nil {
			err = e.recoverManagedRegistry(cleanup, j)
		}
		if err != nil {
			result = errors.Join(result, errors.New("registry maintenance cleanup remains pending: "+err.Error()))
		}
	}()
	if inspect.ID != "" {
		if err = e.Run(ctx, e.Root, []string{"docker", "update", "--restart=no", inspect.ID}, nil, io.Discard); err != nil {
			return err
		}
	}
	if inspect.State.Running {
		if err = e.Run(ctx, e.Root, []string{"docker", "stop", "--time", "30", inspect.ID}, nil, io.Discard); err != nil {
			return err
		}
	}
	if r.ManagedRegistry.Mode == "remove" {
		e.mu.Lock()
		j, err = e.read(r.ID)
		if err != nil {
			e.mu.Unlock()
			return err
		}
		j.RegistryCleanup.Removing = true
		if err = writeJSON(e.path(r.ID), j); err != nil {
			e.mu.Unlock()
			return err
		}
		e.mu.Unlock()
		return e.finishManagedRegistryRemoval(ctx, j)
	}
	image, err := e.linkGatewayImage(ctx)
	if err != nil {
		return err
	}
	input := ManagedRegistryStorageRequest{Policy: r.ManagedRegistry.Policy, Protected: r.ManagedRegistry.Protected, Now: time.Now().UTC(), Delete: true}
	config := filepath.Join(e.Root, "registry-maintenance", r.ID, "request.json")
	if err = os.MkdirAll(filepath.Dir(config), 0700); err != nil {
		return err
	}
	if err = writeJSON(config, input); err != nil {
		return err
	}
	// The worker can be non-root; the capability-free root helper still needs its
	// metadata-only input. The containing worker directory remains private.
	if err = os.Chmod(config, 0444); err != nil {
		return err
	}
	output := &limitedBuffer{Limit: 2 << 20}
	if err = e.registryMaintenanceContainer(ctx, r, "inventory", image, nil, output); err != nil {
		return errors.New("registry retention inventory failed; no incomplete inventory is accepted")
	}
	var inventory ManagedRegistryStorageResult
	if err = json.Unmarshal(output.data.Bytes(), &inventory); err != nil {
		return err
	}
	if err = e.registryMaintenanceContainer(ctx, r, "gc", ManagedRegistryImage, []string{"garbage-collect", "/etc/distribution/config.yml"}, io.Discard); err != nil {
		return errors.New("registry garbage collection failed")
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	j, err = e.read(r.ID)
	if err != nil {
		return err
	}
	j.Operation.ManagedRegistry = &ManagedRegistryResult{Storage: ManagedRegistryStorageSupport(), Manifests: inventory.Manifests, Deleted: inventory.Deleted}
	return writeJSON(e.path(r.ID), j)
}

func (e *Engine) recoverManagedRegistry(ctx context.Context, j journal) error {
	if j.RegistryCleanup == nil {
		return nil
	}
	for _, role := range []string{"inventory", "gc"} {
		name := registryMaintenanceName(j.Request, role)
		ids, err := e.output(ctx, "docker", "ps", "-aq", "--filter", "name=^/"+name+"$")
		if err != nil {
			return err
		}
		for _, id := range strings.Fields(ids) {
			var labels map[string]string
			if err = e.imageJSON(ctx, nil, &labels, "docker", "inspect", "--format", "{{json .Config.Labels}}", id); err != nil {
				return err
			}
			if labels["cove.registry-operation"] != j.Request.ID || labels["cove.registry-workload"] != j.Request.Workload || labels["cove.managed"] != "registry-maintenance" {
				return errors.New("foreign registry maintenance container")
			}
			if err = e.Run(ctx, e.Root, []string{"docker", "rm", "-f", id}, nil, io.Discard); err != nil {
				return err
			}
		}
	}
	if j.RegistryCleanup.Removing {
		if err := e.finishManagedRegistryRemoval(ctx, j); err != nil {
			return err
		}
	} else {
		restart := j.RegistryCleanup.Restart
		if restart == "" {
			restart = "no"
		}
		if restart == "on-failure" && j.RegistryCleanup.Retries > 0 {
			restart += ":" + strconv.Itoa(j.RegistryCleanup.Retries)
		}
		if err := e.Run(ctx, e.Root, []string{"docker", "update", "--restart=" + restart, j.RegistryCleanup.Container}, nil, io.Discard); err != nil {
			return err
		}
		if j.RegistryCleanup.Running {
			if err := e.Run(ctx, e.Root, []string{"docker", "start", j.RegistryCleanup.Container}, nil, io.Discard); err != nil {
				return err
			}
			for {
				var health string
				if err := e.imageJSON(ctx, nil, &health, "docker", "inspect", "--format", "{{json .State.Health.Status}}", j.RegistryCleanup.Container); err != nil {
					return err
				}
				if health == "healthy" {
					break
				}
				if health == "unhealthy" {
					return errors.New("registry has not recovered its authenticated HTTP health check")
				}
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(250 * time.Millisecond):
				}
			}
		}
	}
	if err := os.Remove(filepath.Join(e.Root, "registry-maintenance", j.Request.ID, "request.json")); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	current, err := e.read(j.Request.ID)
	if err != nil {
		return err
	}
	current.RegistryCleanup = nil
	return writeJSON(e.path(j.Request.ID), current)
}

func (e *Engine) finishManagedRegistryRemoval(ctx context.Context, j journal) error {
	if err := e.applyIngress(ctx, j.Request.Workload, nil, io.Discard); err != nil {
		return err
	}
	ids, err := e.output(ctx, "docker", "ps", "-aq", "--no-trunc", "--filter", "label=com.docker.compose.project=cove-"+j.Request.Workload)
	if err != nil {
		return err
	}
	for _, id := range strings.Fields(ids) {
		if id != j.RegistryCleanup.Container {
			return errors.New("unexpected container during registry removal")
		}
		if err = e.Run(ctx, e.Root, []string{"docker", "rm", "-f", id}, nil, io.Discard); err != nil {
			return err
		}
	}
	networks, err := e.output(ctx, "docker", "network", "ls", "-q", "--filter", "label=com.docker.compose.project=cove-"+j.Request.Workload)
	if err != nil {
		return err
	}
	for _, id := range strings.Fields(networks) {
		var labels map[string]string
		if err = e.imageJSON(ctx, nil, &labels, "docker", "network", "inspect", "--format", "{{json .Labels}}", id); err != nil {
			return err
		}
		if labels["com.docker.compose.project"] != "cove-"+j.Request.Workload {
			return errors.New("foreign registry network")
		}
		if err = e.Run(ctx, e.Root, []string{"docker", "network", "rm", id}, nil, io.Discard); err != nil {
			return err
		}
	}
	volumes, err := e.output(ctx, "docker", "volume", "ls", "-q", "--filter", "name=^cove-"+j.Request.Workload+"_data$")
	if err != nil {
		return err
	}
	if j.Request.ManagedRegistry.DeleteData {
		if strings.TrimSpace(volumes) != "" {
			if err = e.ownedRegistryVolume(ctx, j.Request.Workload); err != nil {
				return err
			}
			if err = e.Run(ctx, e.Root, []string{"docker", "volume", "rm", "cove-" + j.Request.Workload + "_data"}, nil, io.Discard); err != nil {
				return err
			}
		}
	}
	if err = os.RemoveAll(filepath.Join(e.Root, "file-secrets", j.Request.Workload)); err != nil {
		return err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	current, err := e.read(j.Request.ID)
	if err != nil {
		return err
	}
	current.Operation.ManagedRegistry = &ManagedRegistryResult{Storage: ManagedRegistryStorageSupport(), Removed: true, DataRetained: !j.Request.ManagedRegistry.DeleteData && strings.TrimSpace(volumes) != ""}
	return writeJSON(e.path(j.Request.ID), current)
}

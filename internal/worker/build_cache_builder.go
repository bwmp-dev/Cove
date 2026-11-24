package worker

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"time"
)

type cacheContainer struct {
	ID     string `json:"Id"`
	Name   string
	Config struct {
		Image      string
		Entrypoint []string
		Cmd        []string
		Labels     map[string]string
	}
	HostConfig struct {
		Privileged     bool
		ReadonlyRootfs bool
		PortBindings   map[string]json.RawMessage
		Tmpfs          map[string]string
		SecurityOpt    []string
	}
	State  struct{ Running bool }
	Mounts []struct {
		Type, Source, Destination string
		RW                        bool
	}
}

func cachePathsOverlap(a, b string) bool {
	a, b = filepath.Clean(a), filepath.Clean(b)
	within := func(child, parent string) bool {
		relative, err := filepath.Rel(parent, child)
		return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
	}
	return within(a, b) || within(b, a)
}

func (e *Engine) verifyCacheMountUsers(ctx context.Context, location cacheLocation) error {
	e.cacheProbeMu.Lock()
	defer e.cacheProbeMu.Unlock()
	out := &limitedBuffer{Limit: 65536}
	if err := e.Run(ctx, e.Root, []string{"docker", "ps", "-aq", "--no-trunc"}, nil, out); err != nil {
		return errors.New("Docker mount ownership could not be verified")
	}
	ids := strings.Fields(out.data.String())
	if len(ids) > 1024 {
		return errors.New("Docker mount inventory exceeds the verification bound")
	}
	if len(ids) == 0 {
		return nil
	}
	state, err := e.readBuildCache()
	if err != nil {
		return err
	}
	allowed := map[string]bool{cacheBuilderName(location): true}
	if state.Current != nil {
		allowed[cacheBuilderName(*state.Current)] = true
	}
	for _, retired := range state.Retired {
		allowed[cacheBuilderName(retired)] = true
	}
	out = &limitedBuffer{Limit: 2 * 1024 * 1024}
	format := `{"Name":{{json .Name}},"Config":{"Labels":{{json .Config.Labels}}},"Mounts":{{json .Mounts}}}`
	args := append([]string{"docker", "inspect", "--format", format}, ids...)
	if err = e.Run(ctx, e.Root, args, nil, out); err != nil {
		return errors.New("Docker mount ownership could not be verified")
	}
	decoder := json.NewDecoder(&out.data)
	for range ids {
		var container cacheContainer
		if decoder.Decode(&container) != nil {
			return errors.New("Docker mount inventory is incomplete")
		}
		for _, mount := range container.Mounts {
			if mount.Source == "" || !cachePathsOverlap(mount.Source, location.Path) {
				continue
			}
			if !allowed[strings.TrimPrefix(container.Name, "/")] || container.Config.Labels["cove.owner"] != location.Owner || container.Config.Labels["cove.managed"] != "build-cache" || mount.Source != location.Path || mount.Destination != "/var/lib/buildkit" {
				return errors.New("the supplied build cache filesystem is mounted by another container")
			}
		}
	}
	return nil
}

func (e *Engine) cacheContainer(ctx context.Context, location cacheLocation) (*cacheContainer, error) {
	daemon, err := e.cacheDockerRoot(ctx)
	if err != nil || daemon != location.Docker {
		return nil, errors.New("the original Docker daemon must be available before using or retiring its cache builder")
	}
	name := cacheBuilderName(location)
	out := &limitedBuffer{Limit: 4096}
	if err := e.Run(ctx, e.Root, []string{"docker", "ps", "-aq", "--no-trunc", "--filter", "name=^/" + name + "$"}, nil, out); err != nil {
		return nil, errors.New("owned cache builder identity is unavailable")
	}
	ids := strings.Fields(out.data.String())
	if len(ids) == 0 {
		return nil, nil
	}
	if len(ids) != 1 {
		return nil, errors.New("owned cache builder identity is ambiguous")
	}
	out = &limitedBuffer{Limit: 32768}
	format := `{"Id":{{json .Id}},"Name":{{json .Name}},"Config":{"Image":{{json .Config.Image}},"Entrypoint":{{json .Config.Entrypoint}},"Cmd":{{json .Config.Cmd}},"Labels":{{json .Config.Labels}}},"HostConfig":{"Privileged":{{json .HostConfig.Privileged}},"ReadonlyRootfs":{{json .HostConfig.ReadonlyRootfs}},"PortBindings":{{json .HostConfig.PortBindings}},"Tmpfs":{{json .HostConfig.Tmpfs}},"SecurityOpt":{{json .HostConfig.SecurityOpt}}},"State":{"Running":{{json .State.Running}}},"Mounts":{{json .Mounts}}}`
	if err := e.Run(ctx, e.Root, []string{"docker", "inspect", "--format", format, ids[0]}, nil, out); err != nil {
		return nil, errors.New("owned cache builder launch could not be verified")
	}
	var container cacheContainer
	if json.Unmarshal(out.data.Bytes(), &container) != nil || container.ID != ids[0] || strings.TrimPrefix(container.Name, "/") != name || container.Config.Labels["cove.owner"] != location.Owner || container.Config.Labels["cove.managed"] != "build-cache" || container.Config.Image != location.Image {
		return nil, errors.New("cache builder ownership does not match this worker")
	}
	// Docker appends label=disable when creating a privileged container.
	security := reflect.DeepEqual(container.HostConfig.SecurityOpt, []string{"no-new-privileges:true"}) || reflect.DeepEqual(container.HostConfig.SecurityOpt, []string{"no-new-privileges:true", "label=disable"})
	if !container.HostConfig.Privileged || !container.HostConfig.ReadonlyRootfs || len(container.HostConfig.PortBindings) != 0 || !security || !reflect.DeepEqual(container.Config.Entrypoint, []string{"buildkitd"}) || !reflect.DeepEqual(container.Config.Cmd, []string{"--addr=unix:///run/buildkit/buildkitd.sock", "--root=/var/lib/buildkit/state"}) {
		return nil, errors.New("owned cache builder launch was changed")
	}
	stateMount := 0
	for _, mount := range container.Mounts {
		switch mount.Destination {
		case "/var/lib/buildkit":
			if mount.Type != "bind" || mount.Source != location.Path || !mount.RW {
				return nil, errors.New("owned cache builder state mount was changed")
			}
			stateMount++
		case "/run", "/tmp":
			if mount.Type != "tmpfs" {
				return nil, errors.New("owned cache builder has an unexpected mount")
			}
		default:
			return nil, errors.New("owned cache builder has an unexpected mount")
		}
	}
	if stateMount != 1 || container.HostConfig.Tmpfs["/run"] != "size=16m,mode=0755" || container.HostConfig.Tmpfs["/tmp"] != "size=64m,mode=1777" {
		return nil, errors.New("owned cache builder storage was changed")
	}
	return &container, nil
}

func (e *Engine) cacheBuildx(ctx context.Context, location cacheLocation, out io.Writer) error {
	name := cacheBuilderName(location)
	env := []string{"BUILDX_CONFIG=" + filepath.Join(e.Root, "buildx")}
	if _, err := os.Stat(filepath.Join(e.Root, "buildx", "instances", name)); errors.Is(err, os.ErrNotExist) {
		err = e.Run(ctx, e.Root, []string{"docker", "buildx", "create", "--name", name, "--driver", "remote", "--driver-opt", "default-load=true", "docker-container://" + name}, env, out)
		if err != nil {
			// Reconcile the private record after a lost acknowledgement instead of creating a second builder.
			if _, statErr := os.Stat(filepath.Join(e.Root, "buildx", "instances", name)); statErr != nil {
				return errors.New("owned cache builder registration remains pending")
			}
		}
	} else if err != nil {
		return err
	}
	file, err := os.Open(filepath.Join(e.Root, "buildx", "instances", name))
	if err != nil {
		return errors.New("owned cache builder registration could not be verified")
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, 65537))
	if err != nil || len(data) > 65536 {
		return errors.New("owned cache builder registration exceeds the verification bound")
	}
	var config struct {
		Name, Driver string
		Nodes        []struct {
			Endpoint   string
			DriverOpts map[string]string
		}
	}
	if json.Unmarshal(data, &config) != nil || config.Name != name || config.Driver != "remote" || len(config.Nodes) != 1 || config.Nodes[0].Endpoint != "docker-container://"+name || !reflect.DeepEqual(config.Nodes[0].DriverOpts, map[string]string{"default-load": "true"}) {
		return errors.New("owned cache builder registration does not match its container")
	}
	return nil
}

func (e *Engine) ensureCacheBuilder(ctx context.Context, location cacheLocation, out io.Writer) error {
	container, err := e.cacheContainer(ctx, location)
	if err != nil {
		return err
	}
	if container == nil {
		image := &limitedBuffer{Limit: 8192}
		if err = e.Run(ctx, e.Root, []string{"docker", "image", "inspect", "--format", "{{.Id}}", location.Image}, nil, image); err != nil {
			config, err := os.MkdirTemp(e.Root, "builder-pull-")
			if err != nil {
				return err
			}
			defer os.RemoveAll(config)
			endpoint, err := e.cacheDockerEndpoint(ctx)
			if err != nil {
				return err
			}
			pull, cancel := context.WithTimeout(ctx, 10*time.Minute)
			err = e.Run(pull, e.Root, []string{"docker", "--config", config, "pull", location.Image}, []string{"DOCKER_HOST=" + endpoint, "DOCKER_CONTEXT=", "DOCKER_TLS_VERIFY=", "DOCKER_CERT_PATH="}, out)
			cancel()
			if err != nil {
				return errors.New("the pinned BuildKit image could not be fetched anonymously")
			}
		}
		name := cacheBuilderName(location)
		args := []string{"docker", "create", "--pull", "never", "--name", name, "--label", "cove.managed=build-cache", "--label", "cove.owner=" + location.Owner,
			"--privileged", "--read-only", "--security-opt", "no-new-privileges:true", "--tmpfs", "/run:size=16m,mode=0755", "--tmpfs", "/tmp:size=64m,mode=1777",
			"--mount", "type=bind,source=" + location.Path + ",target=/var/lib/buildkit", "--entrypoint", "buildkitd", location.Image, "--addr=unix:///run/buildkit/buildkitd.sock", "--root=/var/lib/buildkit/state"}
		createErr := e.Run(ctx, e.Root, args, nil, out)
		container, err = e.cacheContainer(ctx, location)
		if err != nil || container == nil {
			return errors.Join(errors.New("owned cache builder creation remains pending"), createErr, err)
		}
	}
	if !container.State.Running {
		if err = e.Run(ctx, e.Root, []string{"docker", "start", container.ID}, nil, out); err != nil {
			return errors.New("owned cache builder could not start")
		}
	}
	if err = e.cacheBuildx(ctx, location, out); err != nil {
		return err
	}
	return e.Run(ctx, e.Root, []string{"docker", "buildx", "inspect", cacheBuilderName(location), "--bootstrap"}, []string{"BUILDX_CONFIG=" + filepath.Join(e.Root, "buildx")}, out)
}

func (e *Engine) settleCacheRetirement(ctx context.Context, state *buildCacheState, out io.Writer) error {
	for len(state.Retired) > 0 {
		location := state.Retired[0]
		owner, err := e.buildOwner()
		if err != nil || owner != location.Owner {
			return errors.New("retired cache builder ownership could not be verified")
		}
		container, err := e.cacheContainer(ctx, location)
		if err != nil {
			return err
		}
		if container != nil {
			if err = e.Run(ctx, e.Root, []string{"docker", "rm", "-f", container.ID}, nil, out); err != nil {
				return errors.New("retired cache builder removal remains pending")
			}
		}
		name := cacheBuilderName(location)
		if _, err = os.Stat(filepath.Join(e.Root, "buildx", "instances", name)); err == nil {
			if err = e.cacheBuildx(ctx, location, out); err != nil {
				return err
			}
			if err = e.Run(ctx, e.Root, []string{"docker", "buildx", "rm", "--force", name}, []string{"BUILDX_CONFIG=" + filepath.Join(e.Root, "buildx")}, out); err != nil {
				return errors.New("retired cache builder registration removal remains pending")
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		state.Retired = state.Retired[1:]
		if err = writeJSON(filepath.Join(e.Root, "build-cache.json"), state); err != nil {
			return err
		}
	}
	return nil
}

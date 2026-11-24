package worker

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"reflect"
	"strconv"
	"strings"
	"time"
)

func (e *Engine) removeCacheProbe(ctx context.Context, owner string) error {
	name := "cove-cache-probe-" + owner
	ids := &limitedBuffer{Limit: 4096}
	if err := e.Run(ctx, e.Root, []string{"docker", "ps", "-aq", "--no-trunc", "--filter", "name=^/" + name + "$"}, nil, ids); err != nil {
		return errors.New("build cache filesystem probe cleanup could not be verified")
	}
	values := strings.Fields(ids.data.String())
	if len(values) == 0 {
		return nil
	}
	if len(values) != 1 {
		return errors.New("build cache filesystem probe identity is ambiguous")
	}
	out := &limitedBuffer{Limit: 32768}
	format := `{"Name":{{json .Name}},"Config":{"Image":{{json .Config.Image}},"Labels":{{json .Config.Labels}},"Cmd":{{json .Config.Cmd}},"Entrypoint":{{json .Config.Entrypoint}}},"HostConfig":{"Privileged":{{json .HostConfig.Privileged}},"ReadonlyRootfs":{{json .HostConfig.ReadonlyRootfs}},"NetworkMode":{{json .HostConfig.NetworkMode}},"CapDrop":{{json .HostConfig.CapDrop}}}}`
	if err := e.Run(ctx, e.Root, []string{"docker", "inspect", "--format", format, values[0]}, nil, out); err != nil {
		return errors.New("build cache filesystem probe ownership is unavailable")
	}
	var container struct {
		Name   string
		Config struct {
			Image           string
			Labels          map[string]string
			Cmd, Entrypoint []string
		}
		HostConfig struct {
			Privileged, ReadonlyRootfs bool
			NetworkMode                string
			CapDrop                    []string
		}
	}
	if json.Unmarshal(out.data.Bytes(), &container) != nil || container.Name != "/"+name || container.Config.Labels["cove.owner"] != owner || container.Config.Labels["cove.managed"] != "build-cache-probe" || !strings.HasPrefix(container.Config.Image, "cove-link:") || !reflect.DeepEqual(container.Config.Entrypoint, []string{"/cove-worker"}) || !reflect.DeepEqual(container.Config.Cmd, []string{"build-cache-filesystem"}) || container.HostConfig.Privileged || !container.HostConfig.ReadonlyRootfs || container.HostConfig.NetworkMode != "none" || !reflect.DeepEqual(container.HostConfig.CapDrop, []string{"ALL"}) {
		return errors.New("build cache filesystem probe ownership or confinement changed")
	}
	if err := e.Run(ctx, e.Root, []string{"docker", "rm", "-f", values[0]}, nil, io.Discard); err != nil {
		return errors.New("build cache filesystem probe cleanup remains pending")
	}
	return nil
}

func (e *Engine) verifyCacheDaemonView(ctx context.Context, location cacheLocation) (result error) {
	e.cacheProbeMu.Lock()
	defer e.cacheProbeMu.Unlock()
	if err := e.removeCacheProbe(ctx, location.Owner); err != nil {
		return err
	}
	image, err := e.linkGatewayImage(ctx)
	if err != nil {
		return errors.New("the private filesystem verification helper could not be prepared")
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
		defer cancel()
		result = errors.Join(result, e.removeCacheProbe(cleanup, location.Owner))
	}()
	out := &limitedBuffer{Limit: 4096}
	args := []string{"docker", "run", "--rm", "--pull", "never", "--name", "cove-cache-probe-" + location.Owner, "--label", "cove.managed=build-cache-probe", "--label", "cove.owner=" + location.Owner,
		"--network", "none", "--read-only", "--cap-drop", "ALL", "--security-opt", "no-new-privileges:true", "--memory", "32m", "--cpus", "0.25", "--pids-limit", "16", "--user", strconv.Itoa(os.Getuid()) + ":" + strconv.Itoa(os.Getgid()),
		"--mount", "type=bind,source=" + location.Path + ",target=/state,readonly", "--entrypoint", "/cove-worker", image, "build-cache-filesystem"}
	probe, cancel := context.WithTimeout(ctx, 30*time.Second)
	err = e.Run(probe, e.Root, args, nil, out)
	cancel()
	if err != nil {
		return errors.New("Docker's view of the supplied cache filesystem could not be verified")
	}
	var actual cacheFilesystem
	if json.Unmarshal(out.data.Bytes(), &actual) != nil || actual.Filesystem != location.Filesystem.Filesystem || actual.Device != location.Filesystem.Device || actual.Capacity != location.Filesystem.Capacity {
		return errors.New("the worker and Docker daemon do not see the same bounded cache filesystem")
	}
	return nil
}

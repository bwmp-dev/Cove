package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type deployGuard struct {
	Previous  *deployedState `json:"previous"`
	Next      string         `json:"next"`
	Compose   string         `json:"compose"`
	Operation string         `json:"operation"`
}

func guardedService(config map[string]any) (string, error) {
	services, ok := config["services"].(map[string]any)
	if !ok || len(services) != 1 {
		return "", errors.New("guarded deployment requires exactly one stateless service")
	}
	for name, raw := range services {
		service, ok := raw.(map[string]any)
		if !ok {
			return "", errors.New("invalid Compose service")
		}
		for _, key := range []string{"volumes", "ports", "devices", "volumes_from", "container_name", "network_mode", "pid", "ipc", "cap_add", "privileged", "post_start", "pre_stop"} {
			if value := service[key]; value != nil && value != false {
				return "", fmt.Errorf("guarded deployment does not support %s", key)
			}
		}
		health, _ := service["healthcheck"].(map[string]any)
		test, _ := health["test"].([]any)
		if len(test) == 0 || test[0] == "NONE" || health["disable"] == true {
			return "", errors.New("guarded deployment requires an enabled Docker healthcheck")
		}
		return name, nil
	}
	return "", errors.New("missing service")
}

func (e *Engine) testDeploymentCandidate(ctx context.Context, r Request, dir string, args, env []string, out io.Writer) (path string, result error) {
	resolved := &limitedBuffer{Limit: 2 << 20}
	if err := e.Run(ctx, dir, append(append([]string{}, args...), "config", "--format", "json"), env, resolved); err != nil {
		return "", err
	}
	var config map[string]any
	if err := decodeResolvedCompose(resolved.data.Bytes(), &config); err != nil {
		return "", err
	}
	name, err := guardedService(config)
	if err != nil {
		return "", err
	}
	images := &limitedBuffer{Limit: 4096}
	if err = e.Run(ctx, dir, append(append([]string{}, args...), "config", "--images", name), env, images); err != nil {
		return "", err
	}
	refs := strings.Fields(images.data.String())
	if len(refs) != 1 {
		return "", errors.New("could not identify candidate image")
	}
	service := config["services"].(map[string]any)[name].(map[string]any)
	policy, _ := service["pull_policy"].(string)
	if service["build"] == nil && policy != "" && policy != "always" && policy != "never" && policy != "missing" && policy != "if_not_present" {
		return "", errors.New("guarded deployment supports image pull policies always, missing, and never")
	}
	lastSegment := refs[0][strings.LastIndex(refs[0], "/")+1:]
	latest := !strings.Contains(refs[0], "@") && (!strings.Contains(lastSegment, ":") || strings.HasSuffix(lastSegment, ":latest"))
	if service["build"] == nil && (policy == "always" || policy != "never" && latest) {
		if err = e.Run(ctx, dir, []string{"docker", "pull", refs[0]}, env, out); err != nil {
			return "", err
		}
	}
	digest, err := e.output(ctx, "docker", "image", "inspect", "--format", "{{.Id}}", refs[0])
	if err != nil {
		if imageDigest.MatchString(refs[0]) || policy == "never" {
			return "", errors.New("candidate image is no longer available")
		}
		if err = e.Run(ctx, dir, []string{"docker", "pull", refs[0]}, env, out); err != nil {
			return "", err
		}
		digest, err = e.output(ctx, "docker", "image", "inspect", "--format", "{{.Id}}", refs[0])
		if err != nil {
			return "", err
		}
	}
	if !imageDigest.MatchString(digest) {
		return "", errors.New("candidate image is not immutable")
	}
	declared, err := e.output(ctx, "docker", "image", "inspect", "--format", "{{json .Config.Volumes}}", digest)
	if err != nil {
		return "", err
	}
	if declared != "null" && declared != "{}" {
		return "", errors.New("guarded deployment does not support images with declared persistent volumes")
	}
	preserveImageReference(service, refs[0])
	service["image"] = digest
	service["pull_policy"] = "never"
	delete(service, "build")
	path = filepath.Join(dir, r.ID+".guarded.compose.json")
	if err = writeJSON(path, literalCompose(config)); err != nil {
		return "", err
	}
	if err = writeJSON(filepath.Join(dir, "pending-candidate.json"), r.ID); err != nil {
		return "", err
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
		defer cancel()
		result = errors.Join(result, e.recoverCandidate(cleanup, dir))
	}()
	candidate := "cove-candidate-" + r.ID
	command := []string{"docker", "compose", "--project-name", "cove-" + r.Workload, "--file", path, "run", "--detach", "--no-deps", "--no-TTY", "--name", candidate, "--label", "dev.cove.candidate=" + r.ID, name}
	if err = e.Run(ctx, dir, command, env, out); err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	for {
		status, err := e.output(ctx, "docker", "inspect", "--format", `{{.State.Status}} {{if .State.Health}}{{.State.Health.Status}}{{end}}`, candidate)
		if err != nil {
			return "", err
		}
		if status == "running healthy" {
			fmt.Fprintln(out, "Candidate passed its healthcheck. Replacing the application with the same image; a brief interruption is possible.")
			return path, nil
		}
		if strings.Contains(status, "unhealthy") || !strings.HasPrefix(status, "running ") {
			return "", errors.New("candidate failed its healthcheck; existing application was not replaced")
		}
		select {
		case <-ctx.Done():
			return "", errors.New("candidate healthcheck timed out; existing application was not replaced")
		case <-time.After(time.Second):
		}
	}
}

func (e *Engine) recoverCandidate(ctx context.Context, dir string) error {
	path := filepath.Join(dir, "pending-candidate.json")
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var id string
	if json.Unmarshal(raw, &id) != nil || !identifier.MatchString(id) {
		return errors.New("invalid candidate recovery marker")
	}
	ids, err := e.output(ctx, "docker", "ps", "-aq", "--filter", "label=dev.cove.candidate="+id)
	if err != nil {
		return err
	}
	if list := strings.Fields(ids); len(list) > 0 {
		if err = e.Run(ctx, e.Root, append([]string{"docker", "rm", "--force", "--volumes"}, list...), nil, io.Discard); err != nil {
			return err
		}
	}
	return os.Remove(path)
}

func (e *Engine) beginDeployGuard(r Request, dir, compose string) error {
	guard := deployGuard{Operation: r.ID, Compose: compose, Next: filepath.Join(dir, "release-states", r.ID+".compose.json")}
	raw, err := os.ReadFile(filepath.Join(dir, "current.json"))
	if err == nil {
		guard.Previous = &deployedState{}
		if err = json.Unmarshal(raw, guard.Previous); err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	return writeJSON(filepath.Join(dir, "deploy-guard.json"), guard)
}

func (e *Engine) recoverDeployGuard(ctx context.Context, workload string, out io.Writer) error {
	dir := filepath.Join(e.Root, "workloads", workload)
	path := filepath.Join(dir, "deploy-guard.json")
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var guard deployGuard
	if err = json.Unmarshal(raw, &guard); err != nil {
		return err
	}
	current, err := os.ReadFile(filepath.Join(dir, "current.json"))
	if err == nil {
		var state deployedState
		if err = json.Unmarshal(current, &state); err != nil {
			return err
		}
		if state.ComposePath == guard.Next {
			return os.Remove(path)
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	if guard.Previous == nil {
		if err = e.Run(ctx, dir, []string{"docker", "compose", "--project-name", "cove-" + workload, "--file", guard.Compose, "down", "--remove-orphans"}, nil, out); err != nil {
			return err
		}
	} else {
		previous := guard.Previous
		env := []string{}
		for key, value := range previous.Environment {
			env = append(env, key+"="+value)
		}
		if err = e.Run(ctx, dir, []string{"docker", "compose", "--project-name", "cove-" + workload, "--file", previous.ComposePath, "up", "--detach", "--no-build", "--pull", "never", "--wait", "--wait-timeout", "120", "--remove-orphans"}, env, io.Discard); err != nil {
			return errors.New("automatic recovery of the previous release failed; recovery marker retained")
		}
		if err = e.applyIngress(ctx, workload, previous.Routes, out); err != nil {
			return err
		}
		if err = writeJSON(filepath.Join(dir, "current.json"), previous); err != nil {
			return err
		}
		fmt.Fprintln(out, "Previous release recovered automatically. Database changes and external side effects were not reversed.")
	}
	return os.Remove(path)
}

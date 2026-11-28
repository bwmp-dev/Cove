package worker

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

type recipePlannerConfig struct {
	BuildCommand string   `json:"build_command"`
	StartCommand string   `json:"start_command"`
	Secrets      []string `json:"secrets"`
}

type recipePlannerExit struct {
	ExitCode int `json:"exit_code"`
}

func ReadRailpackOutput(kind string, out io.Writer) error {
	path, limit := "", int64(0)
	switch kind {
	case "exit":
		path, limit = "/output/exit.json", 1024
	case "plan":
		path, limit = "/output/plan.json", 2<<20
	default:
		return errors.New("unsupported planner output")
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > limit {
		return errors.New("planner output is unavailable or exceeds its bound")
	}
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	_, err = io.CopyN(out, file, info.Size())
	return err
}

func RunRailpackMise(ctx context.Context, arguments []string, out, stderr io.Writer) error {
	data, err := os.ReadFile("/tmp/cove-planner-proxies.json")
	if err != nil || len(data) > 32768 {
		return errors.New("confined mise proxy policy is unavailable")
	}
	var proxies map[string]string
	if json.Unmarshal(data, &proxies) != nil {
		return errors.New("confined mise proxy policy is invalid")
	}
	command := exec.CommandContext(ctx, "/inputs/mise", arguments...)
	command.Env = os.Environ()
	for _, name := range []string{"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "NO_PROXY", "http_proxy", "https_proxy", "all_proxy", "no_proxy"} {
		if value, exists := proxies[name]; exists {
			command.Env = append(command.Env, name+"="+value)
		}
	}
	command.Stdout, command.Stderr = out, stderr
	return command.Run()
}

func RunRailpackPlanner(ctx context.Context) error {
	configBytes, err := os.ReadFile("/inputs/planner.json")
	if err != nil {
		return err
	}
	var config recipePlannerConfig
	if len(configBytes) > 32<<10 || json.Unmarshal(configBytes, &config) != nil {
		return errors.New("invalid Railpack planner configuration")
	}
	mise, err := os.Stat("/inputs/mise")
	if err != nil || !mise.Mode().IsRegular() || mise.Size() > 256<<20 {
		return errors.New("pinned Railpack mise executable is unavailable or too large")
	}
	if err := os.MkdirAll("/tmp/railpack/mise", 0700); err != nil {
		return err
	}
	// Railpack replaces mise's environment. The trusted wrapper restores only the confined relay policy, never host or Cove secrets.
	if err := os.Symlink("/inputs/planner-helper", "/tmp/railpack/mise/mise-"+RailpackMiseVersion); err != nil {
		return err
	}
	proxies := map[string]string{}
	for _, name := range []string{"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "NO_PROXY", "http_proxy", "https_proxy", "all_proxy", "no_proxy"} {
		if value, exists := os.LookupEnv(name); exists {
			proxies[name] = value
		}
	}
	if err := writeJSON("/tmp/cove-planner-proxies.json", proxies); err != nil {
		return err
	}
	arguments := []string{"prepare", "--hide-pretty-plan", "--error-missing-start", "--plan-out", "/output/plan.json", "--info-out", "/output/info.json"}
	if config.BuildCommand != "" {
		arguments = append(arguments, "--build-cmd", config.BuildCommand)
	}
	if config.StartCommand != "" {
		arguments = append(arguments, "--start-cmd", config.StartCommand)
	}
	for _, name := range config.Secrets {
		if !environmentIdentifier.MatchString(name) {
			return errors.New("invalid selected build secret name")
		}
		arguments = append(arguments, "--env", name+"=")
	}
	arguments = append(arguments, "/inputs/source")
	planning, cancel := context.WithTimeout(ctx, 90*time.Second)
	command := exec.CommandContext(planning, "/railpack", arguments...)
	command.Env = []string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin", "HOME=/tmp", "TMPDIR=/tmp"}
	for _, name := range []string{"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "NO_PROXY", "http_proxy", "https_proxy", "all_proxy", "no_proxy"} {
		if value, ok := os.LookupEnv(name); ok {
			command.Env = append(command.Env, name+"="+value)
		}
	}
	log := &limitedBuffer{Limit: 128 << 10}
	command.Stdout, command.Stderr = log, log
	failure := command.Run()
	cancel()
	code := 0
	if failure != nil {
		code = 1
	}
	if err := writeJSON("/output/exit.json", recipePlannerExit{ExitCode: code}); err != nil {
		return err
	}
	// Keep the bounded tmpfs mounted until the worker copies its output and removes the owned container.
	<-ctx.Done()
	return nil
}

func (e *Engine) prepareRailpackPlan(ctx context.Context, r Request, network string, proxies map[string]string, env []string) ([]byte, error) {
	artifacts, artifactCancel := context.WithTimeout(ctx, 5*time.Minute)
	stageErr := e.stageRecipePlanner(artifacts, r, env)
	artifactCancel()
	if stageErr != nil {
		return nil, stageErr
	}
	ctx, cancel := context.WithTimeout(ctx, 110*time.Second)
	defer cancel()
	networkName, proxy, err := e.prepareRecipeEgress(ctx, r, network, proxies)
	if err != nil {
		return nil, err
	}
	arguments := []string{"docker", "run", "-d", "--pull", "never", "--name", recipeResourceName(r, "planner"), "--network", networkName, "--read-only", "--user", "65534:65534", "--cap-drop", "ALL", "--security-opt", "no-new-privileges:true", "--cpus", "1", "--memory", "256m", "--pids-limit", "64", "--tmpfs", "/tmp:size=128m,mode=1777", "--tmpfs", "/output:size=16m,mode=0700,uid=65534,gid=65534", "--mount", "type=volume,src=" + recipeResourceName(r, "inputs") + ",dst=/inputs,readonly", "--entrypoint", "/inputs/planner-helper"}
	arguments = append(arguments, recipeResourceLabels(r)...)
	if proxy != "" {
		arguments = append(arguments, "--dns", recipeProxyIP(proxy))
		for _, name := range []string{"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "http_proxy", "https_proxy", "all_proxy"} {
			arguments = append(arguments, "--env", name+"="+proxy)
		}
		arguments = append(arguments, "--env", "NO_PROXY=", "--env", "no_proxy=")
	}
	arguments = append(arguments, RailpackFrontendImage, "build-recipe-plan")
	if err := e.Run(ctx, e.Root, arguments, env, io.Discard); err != nil {
		return nil, errors.New("confined Railpack planner could not start")
	}
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		data, err := e.recipeContainerFile(ctx, r, "planner", "/output/exit.json", 1024)
		if err == nil {
			var exit recipePlannerExit
			if json.Unmarshal(data, &exit) != nil {
				return nil, errors.New("Railpack planner returned an invalid completion record")
			}
			if exit.ExitCode != 0 {
				return nil, errors.New("Railpack planning failed: required language metadata is unavailable under the planner's network policy; check the build node's metadata access")
			}
			return e.recipeContainerFile(ctx, r, "planner", "/output/plan.json", 2<<20)
		}
		running, inspectErr := e.output(ctx, "docker", "container", "inspect", "--format", "{{.State.Running}}", recipeResourceName(r, "planner"))
		if inspectErr == nil && running != "true" {
			return nil, errors.New("confined Railpack planner exited before producing a completion record")
		}
		select {
		case <-ctx.Done():
			return nil, errors.New("Railpack planning timed out while obtaining required language metadata under the selected build network/proxy policy")
		case <-ticker.C:
		}
	}
}

func (e *Engine) stageRecipePlanner(ctx context.Context, r Request, env []string) error {
	config, err := os.MkdirTemp(e.recipeInputs(r), "artifact-auth-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(config)
	for _, image := range []string{RailpackFrontendImage, RailpackBuilderImage} {
		if _, err := e.output(ctx, "docker", "image", "inspect", "--format", "{{.Id}}", image); err != nil {
			if err := e.Run(ctx, e.Root, []string{"docker", "--config", config, "pull", image}, nil, io.Discard); err != nil {
				return errors.New("pinned Railpack image is unavailable")
			}
		}
	}
	inputs := e.recipeInputs(r)
	if err := writeJSON(filepath.Join(inputs, "planner.json"), recipePlannerConfig{BuildCommand: r.BuildRecipe.BuildCommand, StartCommand: r.BuildRecipe.StartCommand, Secrets: recipeSecretNames(r)}); err != nil {
		return err
	}
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	if err := copyRecipeFile(executable, filepath.Join(inputs, "planner-helper"), 0755); err != nil {
		return err
	}
	if err := os.Chmod(filepath.Join(inputs, "planner.json"), 0644); err != nil {
		return err
	}
	if err := e.recipeCreateVolume(ctx, r, "inputs"); err != nil {
		return err
	}
	command := []string{"docker", "create", "--pull", "never", "--name", recipeResourceName(r, "stage"), "--network", "none", "--read-only", "--cap-drop", "ALL", "--security-opt", "no-new-privileges:true", "--mount", "type=volume,src=" + recipeResourceName(r, "inputs") + ",dst=/inputs", "--entrypoint", "/bin/sh"}
	command = append(command, recipeResourceLabels(r)...)
	command = append(command, RailpackBuilderImage, "-ec", "mkdir -p /inputs/source; cp /usr/local/bin/mise /inputs/mise; chmod 755 /inputs /inputs/source /inputs/mise")
	if err := e.Run(ctx, e.Root, command, env, io.Discard); err != nil {
		return err
	}
	if err := e.Run(ctx, e.Root, []string{"docker", "start", "-a", recipeResourceName(r, "stage")}, nil, io.Discard); err != nil {
		return err
	}
	for _, name := range []string{"source", "planner.json", "planner-helper"} {
		if err := e.Run(ctx, e.Root, []string{"docker", "cp", filepath.Join(inputs, name), recipeResourceName(r, "stage") + ":/inputs/"}, nil, io.Discard); err != nil {
			return err
		}
	}
	return nil
}

func copyRecipeFile(source, target string, mode os.FileMode) error {
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer input.Close()
	output, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	_, failure := io.Copy(output, input)
	return errors.Join(failure, output.Chmod(mode), output.Close())
}

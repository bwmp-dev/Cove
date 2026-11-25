package worker

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

func RunNixpacksPlanner(ctx context.Context) error {
	data, err := os.ReadFile("/inputs/planner.json")
	if err != nil || len(data) > 128<<10 {
		return errors.New("Nixpacks planner configuration is unavailable")
	}
	var config recipePlannerConfig
	if json.Unmarshal(data, &config) != nil || len(config.Secrets) != 0 {
		return errors.New("Nixpacks planner configuration is invalid")
	}
	arguments := []string{"build", "/inputs/source", "--out", "/output", "--name", "cove-generated"}
	if config.BuildCommand != "" {
		arguments = append(arguments, "--build-cmd", config.BuildCommand)
	}
	if config.StartCommand != "" {
		arguments = append(arguments, "--start-cmd", config.StartCommand)
	}
	plan, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	command := exec.CommandContext(plan, "/nixpacks", arguments...)
	command.Dir = "/inputs/source"
	command.Env = []string{"HOME=/tmp", "PATH=/nonexistent", "NO_COLOR=1"}
	output := &limitedBuffer{Limit: 1 << 20}
	command.Stdout, command.Stderr = output, output
	exit := recipePlannerExit{}
	if err := command.Run(); err != nil {
		exit.ExitCode = 1
		if plan.Err() != nil {
			exit.ExitCode = 124
		}
	}
	if err := writeJSON("/output/exit.json", exit); err != nil {
		return err
	}
	<-ctx.Done()
	return nil
}

func (e *Engine) prepareNixpacksBuild(ctx context.Context, r Request, project string, original map[string]any) (map[string]any, error) {
	if err := validateNixpacksBuildPolicy(original); err != nil {
		return nil, err
	}
	if err := e.checkNixpacksPlatform(ctx, r.BuildPlatform); err != nil {
		return nil, err
	}
	if _, err := os.Lstat(filepath.Join(project, ".nixpacks")); !os.IsNotExist(err) {
		return nil, errors.New("the source contains reserved .nixpacks generated inputs; remove them before selecting Nixpacks")
	}
	inputs := e.recipeInputs(r)
	if err := copyRecipeTree(ctx, project, filepath.Join(inputs, "source"), false); err != nil {
		return nil, err
	}
	image, err := e.nixpacksPlannerImage(ctx)
	if err != nil {
		return nil, err
	}
	if err := writeJSON(filepath.Join(inputs, "planner.json"), recipePlannerConfig{BuildCommand: r.BuildRecipe.BuildCommand, StartCommand: r.BuildRecipe.StartCommand}); err != nil {
		return nil, err
	}
	if err := os.Chmod(filepath.Join(inputs, "planner.json"), 0644); err != nil {
		return nil, err
	}
	if err := e.recipeCreateVolume(ctx, r, "inputs"); err != nil {
		return nil, err
	}
	stage := []string{"docker", "create", "--pull", "never", "--name", recipeResourceName(r, "stage"), "--network", "none", "--read-only", "--cap-drop", "ALL", "--security-opt", "no-new-privileges:true", "--memory", "32m", "--cpus", "0.1", "--pids-limit", "16", "--mount", "type=volume,src=" + recipeResourceName(r, "inputs") + ",dst=/inputs"}
	stage = append(stage, recipeResourceLabels(r)...)
	stage = append(stage, image, "nixpacks-output")
	if err := e.Run(ctx, e.Root, stage, nil, io.Discard); err != nil {
		return nil, err
	}
	for _, name := range []string{"source", "planner.json"} {
		if err := e.Run(ctx, e.Root, []string{"docker", "cp", filepath.Join(inputs, name), recipeResourceName(r, "stage") + ":/inputs/"}, nil, io.Discard); err != nil {
			return nil, err
		}
	}
	planning, cancel := context.WithTimeout(ctx, 75*time.Second)
	defer cancel()
	arguments := []string{"docker", "run", "-d", "--pull", "never", "--name", recipeResourceName(r, "planner"), "--network", "none", "--read-only", "--user", "65534:65534", "--cap-drop", "ALL", "--security-opt", "no-new-privileges:true", "--cpus", "1", "--memory", "512m", "--memory-swap", "512m", "--pids-limit", "128", "--log-driver", "none", "--tmpfs", "/tmp:rw,nosuid,nodev,noexec,size=16m,uid=65534,gid=65534", "--tmpfs", "/output:rw,nosuid,nodev,noexec,size=128m,mode=0700,uid=65534,gid=65534", "--mount", "type=volume,src=" + recipeResourceName(r, "inputs") + ",dst=/inputs,readonly"}
	arguments = append(arguments, recipeResourceLabels(r)...)
	arguments = append(arguments, image, "nixpacks-plan")
	if err := e.Run(planning, e.Root, arguments, nil, io.Discard); err != nil {
		return nil, errors.New("the confined Nixpacks planner could not start")
	}
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		exit := &limitedBuffer{Limit: 1024}
		err := e.Run(planning, e.Root, []string{"docker", "exec", recipeResourceName(r, "planner"), "/cove-worker", "nixpacks-exit"}, nil, exit)
		if err == nil {
			var record recipePlannerExit
			if json.Unmarshal(exit.data.Bytes(), &record) != nil || record.ExitCode != 0 {
				return nil, errors.New("Nixpacks generation failed or timed out in its offline planner; check the repository configuration and supported providers")
			}
			break
		}
		running, inspectErr := e.output(planning, "docker", "container", "inspect", "--format", "{{.State.Running}}", recipeResourceName(r, "planner"))
		if inspectErr != nil || running != "true" {
			return nil, errors.New("the confined Nixpacks planner exited without a valid completion record")
		}
		select {
		case <-planning.Done():
			return nil, errors.New("Nixpacks generation exceeded its 60-second offline planning budget")
		case <-ticker.C:
		}
	}
	generated := &limitedBuffer{Limit: 12 << 20}
	if err := e.Run(planning, e.Root, []string{"docker", "exec", recipeResourceName(r, "planner"), "/cove-worker", "nixpacks-output"}, nil, generated); err != nil {
		return nil, errors.New("Nixpacks generated output was unavailable or exceeded its bound")
	}
	var files map[string][]byte
	if json.Unmarshal(generated.data.Bytes(), &files) != nil || len(files) == 0 || len(files) > 128 {
		return nil, errors.New("Nixpacks generated output is invalid")
	}
	network, _, _ := recipeBuildPolicy(original)
	dockerfile, err := validateNixpacksDockerfile(files[".nixpacks/Dockerfile"], network, recipeSecretNames(r))
	if err != nil {
		return nil, err
	}
	files[".nixpacks/Dockerfile"] = dockerfile
	total := 0
	for name, data := range files {
		total += len(data)
		if !recipeRelativePath(name) || !strings.HasPrefix(name, ".nixpacks/") || name == ".nixpacks/build.sh" || total > nixpacksOutputLimit {
			return nil, errors.New("Nixpacks generated supporting files are unsafe or exceed their bound")
		}
		file := filepath.Join(inputs, "source", filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(file), 0700); err != nil {
			return nil, err
		}
		if err := os.WriteFile(file, data, 0600); err != nil {
			return nil, err
		}
	}
	configuration, err := os.MkdirTemp(inputs, "nixpacks-artifact-auth-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(configuration)
	platform := "linux/" + runtime.GOARCH
	if value, err := e.output(ctx, "docker", "image", "inspect", "--format", "{{.Os}}/{{.Architecture}}", NixpacksBaseImage); err != nil || value != platform {
		if err := e.Run(ctx, e.Root, []string{"docker", "--config", configuration, "pull", "--platform", platform, NixpacksBaseImage}, nil, io.Discard); err != nil {
			return nil, errors.New("the digest-pinned Nixpacks base image is unavailable")
		}
	}
	if value, err := e.output(ctx, "docker", "image", "inspect", "--format", "{{.Os}}/{{.Architecture}}", NixpacksBaseImage); err != nil || value != platform {
		return nil, errors.New("the pinned Nixpacks base image does not match the build worker's platform")
	}
	build := map[string]any{"context": filepath.Join(inputs, "source"), "dockerfile": ".nixpacks/Dockerfile"}
	if network != "" {
		build["network"] = network
	}
	return build, nil
}

package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

func (e *Engine) startBuildpacksKeeper(ctx context.Context, r Request) error {
	image, err := e.linkGatewayImage(ctx)
	if err != nil {
		return err
	}
	owner := []string{"docker", "run", "--pull", "never", "--name", recipeResourceName(r, "cnb-owner"), "--network", "none", "--read-only", "--cap-drop", "ALL", "--cap-add", "CHOWN", "--security-opt", "no-new-privileges:true", "--user", "0:0", "--cpus", "0.25", "--memory", "64m", "--pids-limit", "16", "--log-driver", "none", "--mount", "type=volume,src=" + recipeResourceName(r, "cnb-work") + ",dst=/work"}
	owner = append(owner, recipeResourceLabels(r)...)
	if err := e.runBuildpacks(ctx, r, append(owner, image, "buildpacks-helper", "own"), io.Discard); err != nil {
		return errors.New("Cove could not initialize ownership of its empty CNB work volume")
	}
	arguments := []string{"docker", "run", "-d", "--pull", "never", "--name", recipeResourceName(r, "cnb-keeper"), "--network", "none", "--read-only", "--cap-drop", "ALL", "--security-opt", "no-new-privileges:true", "--user", "1002:1001", "--memory", "128m", "--cpus", "0.25", "--pids-limit", "32"}
	arguments = append(arguments, recipeResourceLabels(r)...)
	for _, mount := range []string{"type=volume,src=" + recipeResourceName(r, "cnb-work") + ",dst=/work", "type=volume,src=" + recipeResourceName(r, "cnb-meta") + ",dst=/meta", "type=volume,src=" + recipeResourceName(r, "cnb-inputs") + ",dst=/inputs,readonly"} {
		arguments = append(arguments, "--mount", mount)
	}
	arguments = append(arguments, image, "buildpacks-helper", "wait")
	if err := e.runBuildpacks(ctx, r, arguments, io.Discard); err != nil {
		return err
	}
	return e.runBuildpacks(ctx, r, []string{"docker", "exec", recipeResourceName(r, "cnb-keeper"), "/cove-worker", "buildpacks-helper", "init"}, io.Discard)
}

func buildpacksPhaseArguments(r Request, phase, network, proxy string, memory uint64) ([]string, error) {
	if memory < BuildpacksMinimumMemoryBytes || memory > BuildpacksMemoryBytes {
		return nil, errors.New("CNB phase memory admission is unavailable")
	}
	limit := strconv.FormatUint(memory, 10)
	arguments := []string{"docker", "create", "--pull", "never", "--name", recipeResourceName(r, "cnb-"+phase), "--network", network, "--read-only", "--cap-drop", "ALL", "--security-opt", "no-new-privileges:true", "--user", "1002:1001", "--cpus", strconv.Itoa(BuildpacksCPUs), "--memory", limit, "--memory-swap", limit, "--pids-limit", "256", "--log-driver", "none", "--tmpfs", "/tmp:size=128m,mode=1777", "--env", "CNB_PLATFORM_API=0.13", "--env", "CNB_EXPERIMENTAL_MODE=warn", "--env", "CNB_USER_ID=1002", "--env", "CNB_GROUP_ID=1001"}
	arguments = append(arguments, recipeResourceLabels(r)...)
	arguments = append(arguments, "--mount", "type=volume,src="+recipeResourceName(r, "cnb-inputs")+",dst=/inputs,readonly", "--mount", "type=volume,src="+recipeResourceName(r, "cnb-inputs")+",dst=/layout/run,volume-subpath=run,readonly")
	metadataMode := ""
	if phase == "builder" || phase == "exporter" {
		metadataMode = ",readonly"
	}
	arguments = append(arguments, "--mount", "type=volume,src="+recipeResourceName(r, "cnb-meta")+",dst=/meta"+metadataMode)
	if phase == "detector" {
		arguments = append(arguments, "--mount", "type=volume,src="+recipeResourceName(r, "cnb-inputs")+",dst=/work/app,volume-subpath=source,readonly", "--tmpfs", "/layers:size=64m,uid=1002,gid=1001")
	} else {
		arguments = append(arguments, "--mount", "type=volume,src="+recipeResourceName(r, "cnb-work")+",dst=/work")
	}
	if phase == "builder" {
		arguments = append(arguments, "--mount", "type=volume,src="+recipeResourceName(r, "cnb-secrets")+",dst=/platform,readonly")
	}
	if proxy != "" {
		arguments = append(arguments, "--dns", recipeProxyIP(proxy))
		for _, name := range []string{"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "http_proxy", "https_proxy", "all_proxy"} {
			arguments = append(arguments, "--env", name+"="+proxy)
		}
		arguments = append(arguments, "--env", "NO_PROXY=", "--env", "no_proxy=")
	}
	var flags []string
	switch phase {
	case "analyzer":
		flags = []string{"-layout", "-layout-dir", "/layout", "-run-image", "/layout/run", "-layers", "/work/layers", "-analyzed", "/meta/analyzed.toml", "cove.invalid/build/output:result"}
	case "detector":
		flags = []string{"-app", "/work/app", "-layers", "/layers", "-order", "/cnb/order.toml", "-analyzed", "/meta/analyzed.toml", "-group", "/meta/group.toml", "-plan", "/meta/plan.toml"}
	case "restorer":
		flags = []string{"-layout", "-layout-dir", "/layout", "-layers", "/work/layers", "-analyzed", "/meta/analyzed.toml", "-group", "/meta/group.toml", "-cache-dir", "/work/cache"}
	case "builder":
		flags = []string{"-app", "/work/app", "-layers", "/work/layers", "-analyzed", "/meta/analyzed.toml", "-group", "/meta/group.toml", "-plan", "/meta/plan.toml"}
	case "exporter":
		flags = []string{"-layout", "-layout-dir", "/work/layout", "-app", "/work/app", "-layers", "/work/layers", "-analyzed", "/meta/analyzed.toml", "-group", "/meta/group.toml", "-cache-dir", "/work/cache", "-launcher", "/inputs/lifecycle/launcher", "-report", "/work/report.toml", "cove.invalid/build/output:result"}
	default:
		return nil, errors.New("unsupported CNB lifecycle phase")
	}
	arguments = append(arguments, "--entrypoint", "/inputs/lifecycle/"+phase, BuildpacksBuilderImage)
	return append(arguments, flags...), nil
}

func (e *Engine) runBuildpacksPhase(ctx context.Context, r Request, phase, network, proxy string, out io.Writer) error {
	admission, err := e.recordedBuildpacksAdmission(r)
	if err != nil {
		return err
	}
	arguments, err := buildpacksPhaseArguments(r, phase, network, proxy, admission.PhaseMemoryBytes)
	if err != nil {
		return err
	}
	if err := e.runBuildpacks(ctx, r, arguments, io.Discard); err != nil {
		return err
	}
	fmt.Fprintln(out, "Cloud Native Buildpacks phase:", phase)
	if err := e.runBuildpacks(ctx, r, []string{"docker", "start", "--attach", recipeResourceName(r, "cnb-"+phase)}, out); err != nil {
		inspection, stop := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		killed, inspectErr := e.outputBuildpacks(inspection, r, "docker", "container", "inspect", "--format", "{{.State.OOMKilled}}", recipeResourceName(r, "cnb-"+phase))
		stop()
		if inspectErr != nil {
			return fmt.Errorf("CNB %s failed with a %d-byte phase memory limit; its cgroup OOM state could not be inspected: %w", phase, admission.PhaseMemoryBytes, errors.Join(err, inspectErr))
		}
		if killed == "true" {
			return &buildpacksMemoryError{limit: admission.PhaseMemoryBytes}
		}
		return fmt.Errorf("CNB %s failed under the selected network policy; dependencies may be unavailable or the bounded work state may be full: %w", phase, err)
	}
	return nil
}

func (e *Engine) readBuildpacksSnapshot(ctx context.Context, r Request) (cnbSnapshot, error) {
	var snapshot cnbSnapshot
	data, err := e.outputBuildpacks(ctx, r, "docker", "exec", recipeResourceName(r, "cnb-keeper"), "/cove-worker", "buildpacks-helper", "snapshot")
	if err != nil || json.Unmarshal([]byte(data), &snapshot) != nil || len(snapshot.Metadata) != 3 || len(snapshot.Run) < 4 || len(snapshot.Run) > 512 {
		return snapshot, errors.New("CNB frozen metadata and pinned run layout could not be verified")
	}
	return snapshot, nil
}

func verifyBuildpacksSnapshot(before, after cnbSnapshot) error {
	if !maps.Equal(before.Metadata, after.Metadata) || !maps.Equal(before.Run, after.Run) {
		return errors.New("CNB analyzed/group/plan metadata or the pinned run image changed during build; export refused")
	}
	return nil
}

func (e *Engine) stageBuildpacksSecrets(ctx context.Context, r Request) ([]string, error) {
	directory := filepath.Join(e.recipeInputs(r), "cnb-platform")
	environment := filepath.Join(directory, "env")
	if err := os.MkdirAll(environment, 0700); err != nil {
		return nil, err
	}
	for _, path := range []string{directory, environment} {
		if err := os.Chmod(path, 0755); err != nil {
			return nil, err
		}
	}
	selected := map[string]bool{}
	for _, name := range recipeSecretNames(r) {
		selected[name] = true
	}
	var values []string
	var total int
	for _, secret := range r.FileSecrets {
		if !selected[secret.Name] {
			continue
		}
		value, ok := r.Environment[secret.Variable]
		if !ok {
			return nil, errors.New("CNB selected build secret is unavailable")
		}
		total += len(value)
		if total > 1<<20 {
			return nil, errors.New("CNB selected build secrets exceed the 1 MiB platform limit")
		}
		if err := writeCNBStageFile(filepath.Join(environment, secret.Name), []byte(value), 0444); err != nil {
			return nil, err
		}
		values = append(values, value)
	}
	if err := e.recipeCreateVolume(ctx, r, "cnb-secrets"); err != nil {
		return nil, err
	}
	arguments := []string{"docker", "create", "--pull", "never", "--name", recipeResourceName(r, "cnb-secret-stage"), "--network", "none", "--read-only", "--cap-drop", "ALL", "--mount", "type=volume,src=" + recipeResourceName(r, "cnb-secrets") + ",dst=/platform", "--entrypoint", "/bin/true"}
	arguments = append(arguments, recipeResourceLabels(r)...)
	if err := e.runBuildpacks(ctx, r, append(arguments, BuildpacksBuilderImage), io.Discard); err != nil {
		return nil, err
	}
	if err := e.runBuildpacks(ctx, r, []string{"docker", "cp", "--archive", environment, recipeResourceName(r, "cnb-secret-stage") + ":/platform/env"}, io.Discard); err != nil {
		return nil, err
	}
	return values, nil
}

func buildpacksSecretNameAllowed(name string) bool {
	name = strings.ToUpper(name)
	for _, prefix := range []string{"CNB_", "COVE_", "DOCKER_", "BUILDKIT_", "LD_"} {
		if strings.HasPrefix(name, prefix) {
			return false
		}
	}
	return name != "HTTP_PROXY" && name != "HTTPS_PROXY" && name != "ALL_PROXY" && name != "NO_PROXY"
}

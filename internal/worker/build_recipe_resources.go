package worker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

func recipeResourceName(r Request, role string) string {
	sum := sha256.Sum256([]byte(r.Workload + "/" + r.ID))
	return "cove-recipe-" + hex.EncodeToString(sum[:12]) + "-" + role
}

func recipeResourceLabels(r Request) []string {
	return []string{"--label", "cove.managed=build-recipe", "--label", "cove.workload=" + r.Workload, "--label", "cove.operation=" + r.ID}
}

func (e *Engine) recipeCreateVolume(ctx context.Context, r Request, role string) error {
	existing, err := e.output(ctx, "docker", "volume", "ls", "-q", "--filter", "name=^"+recipeResourceName(r, role)+"$")
	if err != nil {
		return err
	}
	if existing != "" {
		return errors.New("build recipe volume already exists; reconcile its operation before retrying")
	}
	args := append([]string{"docker", "volume", "create"}, recipeResourceLabels(r)...)
	return e.Run(ctx, e.Root, append(args, recipeResourceName(r, role)), nil, io.Discard)
}

func (e *Engine) recipeContainerFile(ctx context.Context, r Request, role, path string, limit int64) ([]byte, error) {
	kind := ""
	switch path {
	case "/output/exit.json":
		kind = "exit"
	case "/output/plan.json":
		kind = "plan"
	default:
		return nil, errors.New("unsupported planner output")
	}
	buffer := &limitedBuffer{Limit: int(limit)}
	if err := e.Run(ctx, e.Root, []string{"docker", "exec", recipeResourceName(r, role), "/inputs/planner-helper", "build-recipe-output", kind}, nil, buffer); err != nil {
		return nil, err
	}
	return buffer.data.Bytes(), nil
}

func (e *Engine) cleanupRecipeResources(ctx context.Context, r Request) error {
	for _, resource := range []struct{ kind, role string }{
		{"container", "cnb-analyzer"}, {"container", "cnb-detector"}, {"container", "cnb-restorer"}, {"container", "cnb-builder"}, {"container", "cnb-exporter"},
		{"container", "cnb-keeper"}, {"container", "cnb-stage"}, {"container", "cnb-lifecycle-stage"}, {"container", "cnb-secret-stage"}, {"container", "cnb-owner"},
		{"volume", "cnb-work"}, {"volume", "cnb-meta"}, {"volume", "cnb-inputs"}, {"volume", "cnb-secrets"},
		{"container", "planner"}, {"container", "egress"}, {"container", "stage"}, {"container", "config-stage"},
		{"volume", "inputs"}, {"volume", "egress-config"}, {"network", "network"},
	} {
		name := recipeResourceName(r, resource.role)
		args := []string{"docker", resource.kind, "ls", "-q", "--filter", "name=^" + name + "$"}
		if resource.kind == "container" {
			args = append(args, "--all")
		}
		ids, err := e.output(ctx, args...)
		if err != nil {
			return err
		}
		if ids == "" {
			continue
		}
		if len(strings.Fields(ids)) != 1 {
			return errors.New("build recipe cleanup found ambiguous ownership")
		}
		format := "{{json .Labels}}"
		if resource.kind == "container" {
			format = "{{json .Config.Labels}}"
		}
		metadata, err := e.output(ctx, "docker", resource.kind, "inspect", "--format", format, ids)
		if err != nil {
			return err
		}
		var labels map[string]string
		if json.Unmarshal([]byte(metadata), &labels) != nil || labels["cove.managed"] != "build-recipe" || labels["cove.workload"] != r.Workload || labels["cove.operation"] != r.ID {
			return errors.New("build recipe cleanup refused resources with different ownership")
		}
		remove := []string{"docker", resource.kind, "rm"}
		if resource.kind == "container" {
			remove = append(remove, "--force")
		}
		if err := e.Run(ctx, e.Root, append(remove, ids), nil, io.Discard); err != nil {
			return err
		}
	}
	return nil
}

func recipeProxyIP(proxy string) string { parsed, _ := url.Parse(proxy); return parsed.Hostname() }

func recipeDockerIsolation(version string) bool {
	major, _ := strconv.Atoi(strings.Split(version, ".")[0])
	return major >= 28
}

func recipePublicRoots() (string, error) {
	data, err := routeAuxRoots()
	if err != nil {
		return "", err
	}
	var result []byte
	remaining := []byte(data)
	for len(remaining) > 0 {
		block, rest := pem.Decode(remaining)
		if block == nil {
			break
		}
		remaining = rest
		if block.Type == "CERTIFICATE" {
			result = append(result, pem.EncodeToMemory(block)...)
		}
		if len(result) > 1<<20 {
			return "", errors.New("planner public CA bundle exceeds 1 MiB")
		}
	}
	if len(result) == 0 {
		return "", errors.New("planner requires public CA certificates")
	}
	return string(result), nil
}

func (e *Engine) prepareRecipeEgress(ctx context.Context, r Request, network string, proxies map[string]string) (string, string, error) {
	if network == "none" {
		return "none", "", nil
	}
	version, err := e.output(ctx, "docker", "version", "--format", "{{.Server.Version}}")
	if err != nil || !recipeDockerIsolation(version) {
		return "", "", errors.New("confined build egress requires Docker 28 or newer; upgrade and verify Docker on the build node")
	}
	name := recipeResourceName(r, "network")
	create := []string{"docker", "network", "create", "--internal", "--opt", "com.docker.network.bridge.gateway_mode_ipv4=isolated"}
	create = append(create, recipeResourceLabels(r)...)
	if err := e.Run(ctx, e.Root, append(create, name), nil, io.Discard); err != nil {
		return "", "", err
	}
	metadata, err := e.output(ctx, "docker", "network", "inspect", "--format", "{{json .}}", name)
	if err != nil {
		return "", "", err
	}
	var inspected struct {
		Internal bool
		Driver   string
		Options  map[string]string
		IPAM     struct{ Config []struct{ Subnet string } }
	}
	if json.Unmarshal([]byte(metadata), &inspected) != nil || !inspected.Internal || inspected.Driver != "bridge" || inspected.Options["com.docker.network.bridge.gateway_mode_ipv4"] != "isolated" || len(inspected.IPAM.Config) != 1 {
		return "", "", errors.New("Docker did not confirm isolated planner networking")
	}
	address, err := idleAddress(inspected.IPAM.Config[0].Subnet, 2)
	if err != nil {
		return "", "", err
	}
	roots, err := recipePublicRoots()
	if err != nil {
		return "", "", err
	}
	buildpacks := r.BuildRecipe != nil && r.BuildRecipe.Engine == "buildpacks"
	config := recipeEgressConfig{Listen: address + ":3128", HTTPProxy: proxies["HTTP_PROXY"], HTTPSProxy: proxies["HTTPS_PROXY"], NoProxy: proxies["NO_PROXY"], Roots: roots, Build: buildpacks}
	if _, err := newRecipeEgress(config); err != nil {
		return "", "", err
	}
	configPath := filepath.Join(e.recipeInputs(r), "egress.json")
	if err := writeJSON(configPath, config); err != nil {
		return "", "", err
	}
	if err := os.Chmod(configPath, 0644); err != nil {
		return "", "", err
	}
	// The proxy configuration is mounted only in the trusted relay; the planner cannot read upstream credentials.
	if err := e.recipeCreateVolume(ctx, r, "egress-config"); err != nil {
		return "", "", err
	}
	stage := []string{"docker", "create", "--pull", "never", "--name", recipeResourceName(r, "config-stage"), "--network", "none", "--read-only", "--cap-drop", "ALL", "--mount", "type=volume,src=" + recipeResourceName(r, "egress-config") + ",dst=/config", "--entrypoint", "/bin/true"}
	stage = append(stage, recipeResourceLabels(r)...)
	stageImage := RailpackBuilderImage
	var dockerEnvironment []string
	if buildpacks {
		stageImage = BuildpacksBuilderImage
		dockerEnvironment = []string{"DOCKER_CONFIG=" + filepath.Join(e.recipeInputs(r), "cnb-docker-config")}
	}
	stage = append(stage, stageImage)
	if err := e.Run(ctx, e.Root, stage, dockerEnvironment, io.Discard); err != nil {
		return "", "", err
	}
	// Docker cp preserves the private source mode; only this unprivileged relay needs read access in the private volume.
	if err := e.Run(ctx, e.Root, []string{"docker", "cp", "--archive", configPath, recipeResourceName(r, "config-stage") + ":/config/egress.json"}, nil, io.Discard); err != nil {
		return "", "", err
	}
	image, err := e.linkGatewayImage(ctx)
	if err != nil {
		return "", "", err
	}
	proxy := []string{"docker", "create", "--pull", "never", "--name", recipeResourceName(r, "egress"), "--network", "bridge", "--read-only", "--user", "65534:65534", "--cap-drop", "ALL", "--security-opt", "no-new-privileges:true", "--cpus", "0.25", "--memory", "64m", "--pids-limit", "32", "--mount", "type=volume,src=" + recipeResourceName(r, "egress-config") + ",dst=/config,readonly"}
	proxy = append(proxy, recipeResourceLabels(r)...)
	proxy = append(proxy, image, "build-recipe-egress")
	if err := e.Run(ctx, e.Root, proxy, dockerEnvironment, io.Discard); err != nil {
		return "", "", err
	}
	if err := e.Run(ctx, e.Root, []string{"docker", "network", "connect", "--ip", address, name, recipeResourceName(r, "egress")}, nil, io.Discard); err != nil {
		return "", "", err
	}
	if err := e.Run(ctx, e.Root, []string{"docker", "start", recipeResourceName(r, "egress")}, nil, io.Discard); err != nil {
		return "", "", err
	}
	return name, "http://" + address + ":3128", nil
}

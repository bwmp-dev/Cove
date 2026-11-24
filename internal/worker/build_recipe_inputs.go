package worker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

func (e *Engine) recipeInputs(r Request) string {
	return filepath.Join(e.Root, "build-recipes", r.Workload, r.ID)
}

func recipeBuildPolicy(build map[string]any) (string, map[string]string, error) {
	network, _ := build["network"].(string)
	if value := build["network"]; value != nil {
		if _, ok := value.(string); !ok {
			return "", nil, errors.New("build network policy must be an explicit mode")
		}
	}
	if network != "" && network != "default" && network != "none" {
		return "", nil, errors.New("Railpack planning supports the default or none build network; host and application networks are refused")
	}
	proxies := map[string]string{}
	arguments, _ := build["args"].(map[string]any)
	for _, name := range []string{"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "NO_PROXY"} {
		upper, _ := arguments[name].(string)
		lower, _ := arguments[strings.ToLower(name)].(string)
		if upper != "" && lower != "" && upper != lower {
			return "", nil, errors.New("build proxy arguments disagree between upper and lower case")
		}
		if upper == "" {
			upper = lower
		}
		if upper != "" {
			proxies[name] = upper
		}
	}
	if all := proxies["ALL_PROXY"]; all != "" {
		if proxies["HTTP_PROXY"] == "" {
			proxies["HTTP_PROXY"] = all
		}
		if proxies["HTTPS_PROXY"] == "" {
			proxies["HTTPS_PROXY"] = all
		}
	}
	return network, proxies, nil
}

func validateRailpackBuildPolicy(build map[string]any) error {
	network, proxies, err := recipeBuildPolicy(build)
	if err != nil {
		return err
	}
	if network == "none" {
		return errors.New("Railpack is unavailable with build network none: the pinned Railpack frontend does not apply the build network mode to its build steps")
	}
	proxyConfigured := proxies["HTTP_PROXY"] != "" || proxies["HTTPS_PROXY"] != "" || proxies["ALL_PROXY"] != ""
	switch arguments := build["args"].(type) {
	case map[string]any:
		for name, value := range arguments {
			switch strings.ToUpper(name) {
			case "HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY":
				if value == nil {
					proxyConfigured = true
				} else if text, ok := value.(string); !ok || text != "" {
					proxyConfigured = true
				}
			}
		}
	case []any:
		for _, value := range arguments {
			text, ok := value.(string)
			if !ok {
				return errors.New("build arguments are invalid")
			}
			name, value, hasValue := strings.Cut(text, "=")
			switch strings.ToUpper(name) {
			case "HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY":
				if !hasValue || value != "" {
					proxyConfigured = true
				}
			}
		}
	default:
		if build["args"] != nil {
			return errors.New("build arguments are invalid")
		}
	}
	if proxyConfigured {
		return errors.New("Railpack is unavailable with a configured build outbound proxy: the pinned Railpack frontend does not apply the proxy to its build steps")
	}
	return nil
}

func (e *Engine) prepareRecipeBuild(ctx context.Context, r Request, dir string, resolved []byte, env []string, out io.Writer) (string, error) {
	if err := ValidateRecipeSource(*r.BuildRecipe, r.Source, r.Compose, r.Kind); err != nil {
		return "", err
	}
	if err := ValidateRecipeSecrets(*r.BuildRecipe, r.FileSecrets); err != nil {
		return "", err
	}
	var config map[string]any
	if json.Unmarshal(resolved, &config) != nil {
		return "", errors.New("resolved build configuration is invalid")
	}
	services, _ := config["services"].(map[string]any)
	service, _ := services[r.BuildRecipe.Service].(map[string]any)
	if service == nil {
		return "", errors.New("selected build recipe service is unavailable")
	}
	inputs := e.recipeInputs(r)
	if err := os.MkdirAll(inputs, 0700); err != nil {
		return "", err
	}
	project, err := recipeDirectory(filepath.Join(dir, "releases", r.ID), r.BuildRecipe.Directory)
	if err != nil {
		return "", err
	}
	if r.BuildRecipe.Engine == "buildpacks" {
		if err := validateBuildpacksServices(services, r.BuildRecipe.Service); err != nil {
			return "", err
		}
		original, _ := service["build"].(map[string]any)
		previous := service["build"]
		if original == nil {
			service["build"] = map[string]any{}
		}
		err := validateFileSecretTargets(config, r)
		service["build"] = previous
		if err != nil {
			return "", err
		}
		if err := e.prepareBuildpacksBuild(ctx, r, project, original, out); err != nil {
			return "", err
		}
		delete(service, "build")
		service["image"] = buildImageTag(r, r.BuildRecipe.Service)
		path := filepath.Join(dir, r.ID+".recipe.compose.json")
		return path, writeJSON(path, literalCompose(config))
	}
	build := map[string]any{"context": inputs, "dockerfile": "Dockerfile"}
	if r.BuildRecipe.Engine == "static" {
		public, err := recipeDirectory(project, r.BuildRecipe.Output)
		if err != nil {
			return "", err
		}
		if err := copyRecipeTree(ctx, public, filepath.Join(inputs, "public"), true); err != nil {
			return "", err
		}
		executable, err := os.Executable()
		if err != nil {
			return "", err
		}
		binary, err := os.ReadFile(executable)
		if err != nil {
			return "", err
		}
		if err := os.WriteFile(filepath.Join(inputs, "cove-static"), binary, 0755); err != nil {
			return "", err
		}
		if err := os.Chmod(filepath.Join(inputs, "cove-static"), 0755); err != nil {
			return "", err
		}
		dockerfile := "FROM scratch\nCOPY --chmod=0755 cove-static /cove-static\nCOPY --chown=65534:65534 public/ /srv/\nUSER 65534:65534\nWORKDIR /srv\nEXPOSE 8080\nENTRYPOINT [\"/cove-static\",\"static-site\"]\n"
		if err := os.WriteFile(filepath.Join(inputs, "Dockerfile"), []byte(dockerfile), 0600); err != nil {
			return "", err
		}
		sum := sha256.Sum256(binary)
		e.mu.Lock()
		journal, err := e.read(r.ID)
		if err == nil && journal.Operation.BuildRecipe != nil {
			journal.Operation.BuildRecipe.StaticRuntime = "sha256:" + hex.EncodeToString(sum[:])
			err = writeJSON(e.path(r.ID), journal)
		}
		e.mu.Unlock()
		if err != nil {
			return "", err
		}
	} else {
		original, _ := service["build"].(map[string]any)
		if err := validateRailpackBuildPolicy(original); err != nil {
			return "", err
		}
		network, proxies, err := recipeBuildPolicy(original)
		if err != nil {
			return "", err
		}
		if err := copyRecipeTree(ctx, project, filepath.Join(inputs, "source"), false); err != nil {
			return "", err
		}
		plan, err := e.prepareRailpackPlan(ctx, r, network, proxies, env)
		if err != nil {
			return "", err
		}
		plan, err = validateRecipePlan(plan, recipeSecretNames(r))
		if err != nil {
			return "", err
		}
		if r.Build.NoCache || len(recipeSecretNames(r)) > 0 {
			nonce := sha256.Sum256([]byte(r.Workload + "/" + r.ID))
			plan, err = invalidateRecipeCommands(plan, hex.EncodeToString(nonce[:]))
			if err != nil {
				return "", err
			}
		}
		if err := os.WriteFile(filepath.Join(inputs, "source", ".cove-railpack-plan.json"), plan, 0600); err != nil {
			return "", err
		}
		build["context"], build["dockerfile"] = filepath.Join(inputs, "source"), ".cove-railpack-plan.json"
		arguments := map[string]string{"BUILDKIT_SYNTAX": RailpackFrontendImage, "cache-key": r.Workload}
		for name, value := range proxies {
			arguments[name] = value
		}
		build["args"] = arguments
		if network != "" {
			build["network"] = network
		}
		fmt.Fprintln(out, "Railpack plan validated with Cove's pinned frontend, builder and runtime. Build steps use default networking and may download upstream toolchains and packages.")
	}
	service["build"] = build
	path := filepath.Join(dir, r.ID+".recipe.compose.json")
	return path, writeJSON(path, literalCompose(config))
}

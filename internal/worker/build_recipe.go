package worker

import (
	"errors"
	"path"
	"strings"

	"go.yaml.in/yaml/v3"
)

const RailpackVersion = "0.43.1"
const RailpackMiseVersion = "2026.11.12"
const RailpackFrontendImage = "ghcr.io/railwayapp/railpack-frontend@sha256:1f0bb3015946a77d33555756abdceb31885025763c63363ccfb27d83ad919ff9"
const RailpackBuilderImage = "ghcr.io/railwayapp/railpack-builder@sha256:99f681a26282f655ef23d0bcb7ac826116d5bd1b1ac6983be6a4937354e6ceb1"
const RailpackRuntimeImage = "ghcr.io/railwayapp/railpack-runtime@sha256:5283b6b4d8f8eb8cb6f24964a0721e517165242f772faeb5f8600327f53f77fa"

type BuildRecipe struct {
	Engine       string `json:"engine"`
	Service      string `json:"service"`
	Directory    string `json:"directory"`
	Output       string `json:"output"`
	BuildCommand string `json:"build_command"`
	StartCommand string `json:"start_command"`
}

type BuildRecipeInfo struct {
	Engine        string   `json:"engine"`
	Service       string   `json:"service,omitempty"`
	Version       string   `json:"version,omitempty"`
	PinnedImages  []string `json:"pinned_images,omitempty"`
	StaticRuntime string   `json:"static_runtime,omitempty"`
}

func BuildRecipeSummary(recipe *BuildRecipe) *BuildRecipeInfo {
	info := &BuildRecipeInfo{Engine: "compose"}
	if recipe != nil {
		info.Engine, info.Service = recipe.Engine, recipe.Service
		if info.Engine == "railpack" {
			info.Version = RailpackVersion
			info.PinnedImages = []string{RailpackFrontendImage, RailpackBuilderImage, RailpackRuntimeImage}
		}
	}
	return info
}

func recipeRelativePath(value string) bool {
	return value != "" && len(value) <= 512 && path.Clean(value) == value && !path.IsAbs(value) && value != ".." && !strings.HasPrefix(value, "../") && !strings.ContainsAny(value, "\\:\x00\r\n,\"")
}

func ValidateBuildRecipe(recipe BuildRecipe) error {
	if recipe.Engine == "compose" {
		if recipe.Service != "" || recipe.Directory != "" || recipe.Output != "" || recipe.BuildCommand != "" || recipe.StartCommand != "" {
			return errors.New("Compose uses its existing build configuration; clear recipe-specific fields")
		}
		return nil
	}
	if recipe.Engine != "static" && recipe.Engine != "railpack" {
		return errors.New("select an explicit supported build engine: compose, static or railpack")
	}
	if !serviceIdentifier.MatchString(recipe.Service) || !recipeRelativePath(recipe.Directory) {
		return errors.New("select a Compose service and a clean relative project directory")
	}
	if recipe.Engine == "static" {
		if !recipeRelativePath(recipe.Output) || recipe.BuildCommand != "" || recipe.StartCommand != "" {
			return errors.New("static output requires a relative public output directory and accepts no build or start command")
		}
	} else if recipe.Output != "" || len(recipe.BuildCommand) > 8192 || len(recipe.StartCommand) > 8192 || strings.ContainsRune(recipe.BuildCommand+recipe.StartCommand, '\x00') {
		return errors.New("Railpack accepts optional build/start commands up to 8192 bytes and no static output directory")
	}
	return nil
}

func ValidateRecipeSource(recipe BuildRecipe, source *GitSource, compose, kind string) error {
	if err := ValidateBuildRecipe(recipe); err != nil || recipe.Engine == "compose" {
		return err
	}
	if source == nil || !source.InlineCompose {
		return errors.New("a build recipe requires a Git source using the saved Compose configuration; select that explicitly first")
	}
	if recipe.Engine == "static" && kind == "job" {
		return errors.New("static output serves HTTP and requires a service resource")
	}
	var document map[string]any
	if yaml.Unmarshal([]byte(compose), &document) != nil {
		return errors.New("saved Compose configuration is invalid")
	}
	services, _ := document["services"].(map[string]any)
	if _, ok := services[recipe.Service].(map[string]any); !ok {
		return errors.New("the selected build recipe service is absent from saved Compose")
	}
	if recipe.Engine == "static" {
		_, err := StaticRuntimeCompose(compose, recipe)
		return err
	}
	service := services[recipe.Service].(map[string]any)
	build, _ := service["build"].(map[string]any)
	return validateRailpackBuildPolicy(build)
}

func ValidateRecipeSecrets(recipe BuildRecipe, secrets []FileSecret) error {
	if recipe.Engine != "static" {
		return nil
	}
	for _, secret := range secrets {
		for _, service := range secret.Services {
			if service == recipe.Service {
				return errors.New("static output cannot mount runtime file secrets; remove the selected service from those secrets")
			}
		}
	}
	return nil
}

func StaticRuntimeCompose(compose string, recipe BuildRecipe) (string, error) {
	var document map[string]any
	if yaml.Unmarshal([]byte(compose), &document) != nil {
		return "", errors.New("saved Compose configuration is invalid")
	}
	services, _ := document["services"].(map[string]any)
	service, ok := services[recipe.Service].(map[string]any)
	if !ok {
		return "", errors.New("the selected static service is absent from saved Compose")
	}
	for _, field := range []string{"volumes", "volumes_from", "devices", "device_cgroup_rules", "cap_add", "configs", "secrets", "tmpfs", "extends"} {
		if value := service[field]; value != nil {
			if list, ok := value.([]any); !ok || len(list) != 0 {
				return "", errors.New("static output cannot use Compose mounts, devices or added capabilities; remove them from the selected service")
			}
		}
	}
	if service["privileged"] == true || service["use_api_socket"] == true {
		return "", errors.New("static output cannot use a privileged service or Docker API socket")
	}
	service["user"] = "65534:65534"
	service["read_only"] = true
	service["privileged"] = false
	service["cap_drop"] = []string{"ALL"}
	service["security_opt"] = []string{"no-new-privileges:true"}
	service["entrypoint"] = []string{"/cove-static", "static-site"}
	service["command"] = []string{}
	data, err := yaml.Marshal(document)
	return string(data), err
}

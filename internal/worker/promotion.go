package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

var imageDigest = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)
var repositoryPath = regexp.MustCompile(`^[a-z0-9]+([._/-][a-z0-9]+)*$`)
var pushedDigest = regexp.MustCompile(`digest: (sha256:[a-f0-9]{64})`)

type PromotionExport struct {
	Workload   string    `json:"workload"`
	Release    string    `json:"release"`
	Repository string    `json:"repository,omitempty"`
	Registry   *Registry `json:"registry,omitempty"`
	Portable   bool      `json:"portable"`
}

func (e *Engine) ExportRelease(ctx context.Context, in PromotionExport) (map[string]string, error) {
	if !workloadIdentifier.MatchString(in.Workload) || !identifier.MatchString(in.Release) {
		return nil, errors.New("invalid source release")
	}
	e.mu.Lock()
	if e.busy(in.Workload) {
		e.mu.Unlock()
		return nil, errors.New("source resource has an active operation")
	}
	if e.active == nil {
		e.active = map[string]bool{}
	}
	e.active[in.Workload] = true
	journal, err := e.read(in.Release)
	e.mu.Unlock()
	defer func() { e.mu.Lock(); delete(e.active, in.Workload); e.mu.Unlock() }()
	if err != nil || journal.Operation.Workload != in.Workload || journal.Operation.State != "succeeded" {
		return nil, errors.New("source release was not successful")
	}
	if in.Registry != nil && (ValidateRegistry(in.Registry) != nil || !repositoryPath.MatchString(in.Repository) || len(in.Repository) > 200) {
		return nil, errors.New("invalid promotion registry repository")
	}
	raw, err := os.ReadFile(filepath.Join(e.Root, "workloads", in.Workload, "release-states", in.Release+".json"))
	if err != nil {
		return nil, err
	}
	var state deployedState
	if err = json.Unmarshal(raw, &state); err != nil {
		return nil, err
	}
	raw, err = os.ReadFile(state.ComposePath)
	if err != nil {
		return nil, err
	}
	var config struct {
		Services map[string]struct {
			Image string `json:"image"`
		} `json:"services"`
	}
	if err = json.Unmarshal(raw, &config); err != nil {
		return nil, err
	}
	if len(config.Services) == 0 || len(config.Services) > 64 {
		return nil, errors.New("invalid source services")
	}
	env := []string{}
	if in.Registry != nil {
		dir, err := registryConfig(e.Root, in.Registry)
		if err != nil {
			return nil, err
		}
		defer os.RemoveAll(dir)
		env = append(env, "DOCKER_CONFIG="+dir)
	}
	names := []string{}
	for name := range config.Services {
		names = append(names, name)
	}
	sort.Strings(names)
	result := map[string]string{}
	for _, name := range names {
		image := config.Services[name].Image
		resultName := trafficServiceName(state.Traffic, name)
		if !serviceIdentifier.MatchString(name) || !imageDigest.MatchString(image) {
			return nil, errors.New("release image is not immutable")
		}
		if in.Registry != nil {
			platform, err := e.output(ctx, "docker", "image", "inspect", "--format", "{{.Os}}/{{.Architecture}}", image)
			if err != nil || !regexp.MustCompile(`^[a-z0-9]+/[a-z0-9]+$`).MatchString(platform) {
				return nil, errors.New("source image platform unavailable")
			}
			expected, err := e.output(ctx, "docker", "image", "inspect", "--platform", platform, "--format", "{{.Id}}", image)
			if err != nil || !imageDigest.MatchString(expected) {
				return nil, errors.New("source platform image unavailable")
			}
			ref := in.Registry.Server + "/" + in.Repository + ":" + strings.ToLower(in.Workload+"-"+in.Release+"-"+name)
			if err = e.Run(ctx, e.Root, []string{"docker", "image", "tag", image, ref}, env, io.Discard); err != nil {
				return nil, errors.New("source image is unavailable")
			}
			output := &limitedBuffer{Limit: 65536}
			if err = e.Run(ctx, e.Root, []string{"docker", "push", "--platform", platform, ref}, env, output); err != nil {
				return nil, errors.New("could not publish the immutable release image")
			}
			matches := pushedDigest.FindAllStringSubmatch(output.data.String(), -1)
			if len(matches) == 0 {
				return nil, errors.New("registry publication did not return an immutable digest")
			}
			published := in.Registry.Server + "/" + in.Repository + "@" + matches[len(matches)-1][1]
			if err = e.Run(ctx, e.Root, []string{"docker", "pull", published}, env, io.Discard); err != nil {
				return nil, errors.New("published image digest could not be pulled")
			}
			check := &limitedBuffer{Limit: 1024}
			if err = e.Run(ctx, e.Root, []string{"docker", "image", "inspect", "--platform", platform, "--format", "{{.Id}}", published}, env, check); err != nil || strings.TrimSpace(check.data.String()) != expected {
				return nil, errors.New("published image does not match the retained release")
			}
			result[resultName] = published
			continue
		}
		if !in.Portable && in.Registry == nil {
			result[resultName] = image
			continue
		}
		inspected := &limitedBuffer{Limit: 65536}
		if err = e.Run(ctx, e.Root, []string{"docker", "image", "inspect", "--format", "{{json .RepoDigests}}", image}, env, inspected); err != nil {
			return nil, err
		}
		var digests []string
		if err = json.Unmarshal(inspected.data.Bytes(), &digests); err != nil {
			return nil, err
		}
		for _, digest := range digests {
			repository, hash, ok := strings.Cut(digest, "@")
			if !ok || !imageDigest.MatchString(hash) {
				continue
			}
			if in.Registry != nil && repository != in.Registry.Server+"/"+in.Repository {
				continue
			}
			result[resultName] = digest
			break
		}
		if result[resultName] == "" {
			return nil, errors.New("cross-node promotion requires published image digests; select a registry and repository")
		}
	}
	return result, nil
}

func validateImagePins(pins map[string]string) error {
	if len(pins) > 64 {
		return errors.New("too many promoted services")
	}
	for name, image := range pins {
		if !serviceIdentifier.MatchString(name) || len(image) > 512 || strings.ContainsAny(image, " \t\r\n\x00") {
			return errors.New("invalid promoted image")
		}
		if imageDigest.MatchString(image) {
			continue
		}
		repository, digest, ok := strings.Cut(image, "@")
		if !ok || repository == "" || strings.HasPrefix(repository, "-") || !imageDigest.MatchString(digest) {
			return errors.New("promotion requires immutable image digests")
		}
	}
	return nil
}

func applyImagePins(config map[string]any, pins map[string]string) error {
	if err := validateImagePins(pins); err != nil {
		return err
	}
	services, ok := config["services"].(map[string]any)
	if !ok || len(services) != len(pins) {
		return errors.New("promotion requires identical Compose service names in both environments")
	}
	for name, image := range pins {
		service, ok := services[name].(map[string]any)
		if !ok {
			return errors.New("promoted service is missing in the target")
		}
		delete(service, "build")
		service["image"] = image
		service["pull_policy"] = "missing"
		if imageDigest.MatchString(image) {
			service["pull_policy"] = "never"
		}
	}
	return nil
}

// applyPreviewImagePins runs the production images on the services of a preview's pinned baseline.
// Services Cove added to the production release, such as a Tailscale sidecar, have no counterpart.
func applyPreviewImagePins(config map[string]any, pins map[string]string) error {
	if err := validateImagePins(pins); err != nil {
		return err
	}
	services, ok := config["services"].(map[string]any)
	if !ok {
		return errors.New("Compose has no services")
	}
	for name, raw := range services {
		image, pinned := pins[name]
		service, ok := raw.(map[string]any)
		if !ok {
			return errors.New("invalid Compose service")
		}
		if !pinned {
			return fmt.Errorf("service %s has no deployed production image to pin", name)
		}
		delete(service, "build")
		service["image"] = image
		service["pull_policy"] = "never"
	}
	return nil
}

func (e *Engine) preparePromotion(ctx context.Context, r Request, dir string, args, env []string) (string, error) {
	output := &limitedBuffer{Limit: 2 * 1024 * 1024}
	if err := e.Run(ctx, dir, append(append([]string{}, args...), "config", "--format", "json"), env, output); err != nil {
		return "", err
	}
	var config map[string]any
	if err := unmarshalCompose(output.data.Bytes(), &config); err != nil {
		return "", err
	}
	if r.EnvironmentPreview != nil {
		if r.EnvironmentPreview.ImageRegistry != "" {
			if r.Registry == nil || r.Registry.Server != r.EnvironmentPreview.ImageRegistry {
				return "", errors.New("CI preview images require their connected registry credentials")
			}
			if err := ValidatePreviewRegistryImages(r.ImagePins, r.EnvironmentPreview.ImageRegistry); err != nil {
				return "", err
			}
			services, ok := config["services"].(map[string]any)
			if !ok {
				return "", errors.New("Compose has no services")
			}
			for name := range r.ImagePins {
				if _, exists := services[name]; !exists {
					disabled := false
					for _, omitted := range r.EnvironmentPreview.Disabled {
						disabled = disabled || omitted == name
					}
					if !disabled {
						return "", errors.New("CI preview contains an image for an unknown service")
					}
				}
			}
		}
		if err := applyPreviewImagePins(config, r.ImagePins); err != nil {
			return "", err
		}
		if r.EnvironmentPreview.ImageRegistry != "" {
			for _, raw := range config["services"].(map[string]any) {
				raw.(map[string]any)["pull_policy"] = "always"
			}
		}
	} else if err := applyImagePins(config, r.ImagePins); err != nil {
		return "", err
	}
	path := filepath.Join(dir, r.ID+".promotion.compose.json")
	return path, writeJSON(path, literalCompose(config))
}

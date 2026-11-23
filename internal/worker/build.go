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
	"sort"
	"strings"
	"time"
)

type BuildOptions struct {
	Repository string `json:"repository"`
	NoCache    bool   `json:"no_cache,omitempty"`
}

type BuiltImage struct {
	Service   string `json:"service"`
	ImageID   string `json:"image_id"`
	Reference string `json:"reference"`
}

type BuildResult struct {
	Images []BuiltImage `json:"images"`
}

type pendingBuild struct {
	Operation string   `json:"operation"`
	Owner     string   `json:"owner"`
	Services  []string `json:"services"`
}

func buildImageTag(r Request, service string) string {
	hash := sha256.Sum256([]byte(service))
	operation := sha256.Sum256([]byte(r.Workload + "/" + r.ID))
	return r.Registry.Server + "/" + r.Build.Repository + ":cove-" + hex.EncodeToString(operation[:16]) + "-" + hex.EncodeToString(hash[:8])
}

func (e *Engine) performBuild(ctx context.Context, r Request, dir string, env []string, out io.Writer) (result error) {
	if r.Registry == nil {
		return errors.New("select a registry connection before building")
	}
	if err := ValidateRegistry(r.Registry); err != nil {
		return err
	}
	if err := e.requireDockerProxy(ctx, r.OutboundProxy); err != nil {
		return err
	}
	if err := e.recoverBuild(ctx, r.Workload); err != nil {
		return err
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
		defer cancel()
		if err := e.cleanupBuildInputs(r); err != nil {
			result = errors.Join(result, err)
			return
		}
		result = errors.Join(result, e.recoverBuild(cleanup, r.Workload))
	}()
	path := filepath.Join(dir, r.ID+".build.yaml")
	if r.Source != nil {
		var revision string
		var err error
		path, revision, err = e.checkout(ctx, r, dir, out)
		if err != nil {
			return err
		}
		if _, automatic := r.resolvedPublicBuiltins[CoveCommitSHA]; automatic {
			r.Environment[CoveCommitSHA] = revision
			for index, value := range env {
				if strings.HasPrefix(value, CoveCommitSHA+"=") {
					env[index] = CoveCommitSHA + "=" + revision
				}
			}
		}
		e.mu.Lock()
		j, err := e.read(r.ID)
		if err == nil {
			j.Operation.Revision = revision
			err = writeJSON(e.path(r.ID), j)
		}
		e.mu.Unlock()
		if err != nil {
			return err
		}
	} else if err := os.WriteFile(path, []byte(r.Compose), 0600); err != nil {
		return err
	}
	var err error
	path, err = e.prepareComposeBundle(ctx, r, dir, path, env)
	if err != nil {
		return err
	}
	args := []string{"docker", "compose", "--profile", "*", "--project-name", e.composeProject(r.Workload), "--file", path}
	if r.ComposeBundle != nil {
		secrets, err := composeBundleSecrets(path)
		if err != nil {
			return errors.New("could not prepare build input redaction")
		}
		if log, ok := out.(*boundedLog); ok {
			log.secrets = append(log.secrets, secrets...)
		}
	}
	if len(r.BuiltinDefaults) > 0 {
		path, err = e.prepareBuiltinEnvironment(ctx, r, dir, args, env)
		if err != nil {
			return err
		}
		args = []string{"docker", "compose", "--profile", "*", "--project-name", e.composeProject(r.Workload), "--file", path}
	}
	var config struct {
		Services map[string]struct {
			Build json.RawMessage `json:"build"`
		} `json:"services"`
	}
	resolved := &limitedBuffer{Limit: 2 * 1024 * 1024}
	if err := e.Run(ctx, dir, append(append([]string{}, args...), "config", "--format", "json"), env, resolved); err != nil {
		return err
	}
	if err := json.Unmarshal(resolved.data.Bytes(), &config); err != nil {
		return err
	}
	names := []string{}
	for name, service := range config.Services {
		if len(service.Build) > 0 && string(service.Build) != "null" {
			if !serviceIdentifier.MatchString(name) {
				return errors.New("invalid build service")
			}
			names = append(names, name)
		}
	}
	if len(names) == 0 || len(names) > 64 {
		return errors.New("build requires between one and 64 Compose build services")
	}
	sort.Strings(names)
	e.buildMu.Lock()
	owner, err := e.buildOwner()
	e.buildMu.Unlock()
	if err != nil {
		return err
	}
	if err = writeJSON(filepath.Join(dir, "pending-build.json"), pendingBuild{Operation: r.ID, Owner: owner, Services: names}); err != nil {
		return err
	}
	if len(r.FileSecrets) > 0 {
		path, err = e.prepareFileSecrets(ctx, r, dir, args, env)
		if err != nil {
			return err
		}
		args = []string{"docker", "compose", "--profile", "*", "--project-name", e.composeProject(r.Workload), "--file", path}
	}
	services := map[string]any{}
	for _, name := range names {
		services[name] = map[string]string{"image": buildImageTag(r, name)}
	}
	// Unique tags prevent another workload's build from changing the image identity before publication.
	override := filepath.Join(dir, r.ID+".build-tags.compose.json")
	if err = writeJSON(override, literalCompose(map[string]any{"services": services})); err != nil {
		return err
	}
	args = append(args, "--file", override)
	if err = e.buildImages(ctx, dir, args, env, out, r.Build.NoCache); err != nil {
		return err
	}
	products := BuildResult{Images: []BuiltImage{}}
	for _, name := range names {
		tag := buildImageTag(r, name)
		var image imageMetadata
		if err = e.imageJSON(ctx, env, &image, "docker", "image", "inspect", "--format", `{"id":{{json .Id}},"os":{{json .Os}},"architecture":{{json .Architecture}}}`, tag); err != nil {
			return err
		}
		if !imageDigest.MatchString(image.ID) || image.OS == "" || image.Architecture == "" {
			return errors.New("built image identity or platform unavailable")
		}
		if err = e.Run(ctx, dir, []string{"docker", "push", "--platform", image.OS + "/" + image.Architecture, tag}, env, out); err != nil {
			return err
		}
		var manifest registryManifest
		if err = e.imageJSON(ctx, env, &manifest, "docker", "buildx", "imagetools", "inspect", "--format", "{{json .Manifest}}", tag); err != nil {
			return err
		}
		if !imageDigest.MatchString(manifest.Digest) {
			return errors.New("published build did not return an immutable registry digest")
		}
		reference := r.Registry.Server + "/" + r.Build.Repository + "@" + manifest.Digest
		var verified registryManifest
		if err = e.imageJSON(ctx, env, &verified, "docker", "buildx", "imagetools", "inspect", "--raw", reference); err != nil {
			return err
		}
		if !imageDigest.MatchString(verified.Config.Digest) || (verified.Config.Digest != image.ID && manifest.Digest != image.ID) {
			return fmt.Errorf("published build configuration %q does not match local image %q", verified.Config.Digest, image.ID)
		}
		products.Images = append(products.Images, BuiltImage{Service: name, ImageID: image.ID, Reference: reference})
		fmt.Fprintf(out, "Published %s as %s\n", name, reference)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	j, err := e.read(r.ID)
	if err != nil {
		return err
	}
	j.Operation.Build = &products
	return writeJSON(e.path(r.ID), j)
}

func (e *Engine) recoverBuild(ctx context.Context, workload string) error {
	if !workloadIdentifier.MatchString(workload) {
		return errors.New("invalid build workload")
	}
	path := filepath.Join(e.Root, "workloads", workload, "pending-build.json")
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var pending pendingBuild
	if json.Unmarshal(data, &pending) != nil || !identifier.MatchString(pending.Operation) || len(pending.Owner) != 32 || len(pending.Services) == 0 || len(pending.Services) > 64 {
		return errors.New("invalid build cleanup marker")
	}
	if _, err = hex.DecodeString(pending.Owner); err != nil {
		return errors.New("invalid build owner")
	}
	j, err := e.read(pending.Operation)
	if err != nil {
		return err
	}
	if j.Request.Action != "build" || j.Request.Workload != workload || j.Request.Registry == nil || ValidateRequest(j.Request) != nil {
		return errors.New("build cleanup does not match its journal")
	}
	data, err = os.ReadFile(filepath.Join(e.Root, "build-owner.json"))
	if err != nil {
		return err
	}
	var owner string
	if json.Unmarshal(data, &owner) != nil || owner != pending.Owner {
		return errors.New("build cleanup owner does not match this worker")
	}
	if err := e.cleanupBuildInputs(j.Request); err != nil {
		return err
	}
	expected := map[string]string{}
	for _, name := range pending.Services {
		if !serviceIdentifier.MatchString(name) {
			return errors.New("invalid cleanup service")
		}
		tag := buildImageTag(j.Request, name)
		expected[imageRepository(tag)+tag[strings.LastIndex(tag, ":"):]] = tag
	}
	output := &limitedBuffer{Limit: 2 * 1024 * 1024}
	config, err := os.MkdirTemp(e.Root, "build-cleanup-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(config)
	env := []string{"DOCKER_CONFIG=" + config}
	if err = e.Run(ctx, e.Root, []string{"docker", "image", "ls", "--format", "{{json .}}", "--filter", "label=dev.cove.build-owner=" + owner}, env, output); err != nil {
		return err
	}
	decoder := json.NewDecoder(&output.data)
	for {
		var image struct{ Repository, Tag string }
		if err = decoder.Decode(&image); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return err
		}
		key := imageRepository(image.Repository) + ":" + image.Tag
		if tag := expected[key]; tag != "" {
			if err = e.Run(ctx, e.Root, []string{"docker", "image", "rm", tag}, env, io.Discard); err != nil {
				return err
			}
			delete(expected, key)
		}
	}
	return os.Remove(path)
}

func (e *Engine) cleanupBuildInputs(r Request) error {
	if r.Action != "build" || !identifier.MatchString(r.ID) || !workloadIdentifier.MatchString(r.Workload) {
		return errors.New("invalid build cleanup request")
	}
	dir := filepath.Join(e.Root, "workloads", r.Workload)
	var failures []error
	for _, suffix := range []string{".build.yaml", ".build-tags.compose.json", ".secrets.compose.json"} {
		if err := os.Remove(filepath.Join(dir, r.ID+suffix)); err != nil && !os.IsNotExist(err) {
			failures = append(failures, err)
		}
	}
	if r.Source != nil {
		if err := os.RemoveAll(filepath.Join(dir, "releases", r.ID)); err != nil {
			failures = append(failures, err)
		}
	}
	if err := os.RemoveAll(filepath.Join(e.Root, "file-secrets", r.Workload, r.ID)); err != nil {
		failures = append(failures, err)
	}
	if err := os.RemoveAll(filepath.Join(e.Root, "build-auth", r.ID)); err != nil {
		failures = append(failures, err)
	}
	return errors.Join(failures...)
}

package worker

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

func (e *Engine) runBuildpacks(ctx context.Context, r Request, arguments []string, out io.Writer) error {
	return e.Run(ctx, e.Root, arguments, []string{"DOCKER_CONFIG=" + filepath.Join(e.recipeInputs(r), "cnb-docker-config")}, out)
}

func (e *Engine) outputBuildpacks(ctx context.Context, r Request, arguments ...string) (string, error) {
	out := &limitedBuffer{Limit: 2 << 20}
	err := e.runBuildpacks(ctx, r, arguments, out)
	return strings.TrimSpace(out.data.String()), err
}

func writeCNBStageFile(path string, data []byte, mode os.FileMode) error {
	if err := os.WriteFile(path, data, mode); err != nil {
		return err
	}
	return os.Chmod(path, mode)
}

func (e *Engine) stageBuildpacksImages(ctx context.Context, r Request) error {
	capacity, err := e.buildpacksCapacity(ctx, r.BuildPlatform)
	if err != nil {
		return err
	}
	if err := ValidateBuildpacksCapacity(capacity); err != nil {
		return err
	}
	config, err := os.MkdirTemp(e.recipeInputs(r), "cnb-artifact-auth-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(config)
	for _, image := range []string{BuildpacksLifecycleImage, BuildpacksBuilderImage, BuildpacksRunImage} {
		data, err := e.output(ctx, "docker", "image", "inspect", "--format", `{{.Os}}/{{.Architecture}}`, image)
		if err != nil || data != "linux/"+runtime.GOARCH {
			if err := e.Run(ctx, e.Root, []string{"docker", "--config", config, "pull", "--platform", "linux/" + runtime.GOARCH, image}, nil, io.Discard); err != nil {
				return errors.New("the digest-pinned CNB lifecycle, builder or run image is unavailable for this platform")
			}
		}
		data, err = e.output(ctx, "docker", "image", "inspect", "--format", `{{.Os}}/{{.Architecture}}`, image)
		if err != nil || data != "linux/"+runtime.GOARCH {
			return errors.New("the cached CNB image does not match the worker's pinned Linux platform")
		}
	}
	return nil
}

func stageCNBLayout(source, target string, image cnbImage) error {
	if err := os.MkdirAll(filepath.Join(target, "blobs", "sha256"), 0755); err != nil {
		return err
	}
	for _, directory := range []string{target, filepath.Join(target, "blobs"), filepath.Join(target, "blobs", "sha256")} {
		if err := os.Chmod(directory, 0755); err != nil {
			return err
		}
	}
	for _, descriptor := range append([]cnbDescriptor{image.Descriptor, image.Manifest.Config}, image.Manifest.Layers...) {
		path, err := cnbBlobPath(source, descriptor.Digest)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil || int64(len(data)) != descriptor.Size || cnbDigest(data) != descriptor.Digest {
			return errors.New("the pinned CNB run layout changed before staging")
		}
		if err := writeCNBStageFile(filepath.Join(target, "blobs", "sha256", descriptor.Digest[7:]), data, 0644); err != nil {
			return err
		}
	}
	if err := writeCNBStageFile(filepath.Join(target, "oci-layout"), []byte(`{"imageLayoutVersion":"1.0.0"}`), 0644); err != nil {
		return err
	}
	index, err := json.Marshal(cnbIndex{SchemaVersion: 2, MediaType: "application/vnd.oci.image.index.v1+json", Manifests: []cnbDescriptor{image.Descriptor}})
	if err != nil {
		return err
	}
	return writeCNBStageFile(filepath.Join(target, "index.json"), index, 0644)
}

func (e *Engine) createBuildpacksState(ctx context.Context, r Request) error {
	for _, volume := range []struct {
		role     string
		capacity uint64
	}{{"cnb-work", BuildpacksWorkBytes}, {"cnb-meta", 16 << 20}} {
		name := recipeResourceName(r, volume.role)
		existing, err := e.output(ctx, "docker", "volume", "ls", "-q", "--filter", "name=^"+name+"$")
		if err != nil || existing != "" {
			return errors.New("CNB state already exists or cannot be verified; recover its operation first")
		}
		arguments := []string{"docker", "volume", "create", "--driver", "local"}
		if volume.role == "cnb-meta" {
			arguments = append(arguments, "--opt", "type=tmpfs", "--opt", "device=tmpfs", "--opt", "o=size="+strconv.FormatUint(volume.capacity, 10)+",uid=1002,gid=1001,mode=0700")
		}
		arguments = append(arguments, recipeResourceLabels(r)...)
		if err := e.runBuildpacks(ctx, r, append(arguments, name), io.Discard); err != nil {
			return errors.New("CNB disk-backed operation state or bounded metadata tmpfs is unavailable")
		}
	}
	return nil
}

func (e *Engine) stageBuildpacksInputs(ctx context.Context, r Request, project string) (cnbImage, error) {
	var runImage cnbImage
	inputs := e.recipeInputs(r)
	if err := copyRecipeTree(ctx, project, filepath.Join(inputs, "source"), false); err != nil {
		return runImage, err
	}
	archive := filepath.Join(inputs, "cnb-run.tar")
	if err := e.runBuildpacks(ctx, r, []string{"docker", "image", "save", "--output", archive, BuildpacksRunImage}, io.Discard); err != nil {
		return runImage, err
	}
	raw := filepath.Join(inputs, "run-raw")
	if err := unpackCNBArchive(ctx, archive, raw, true); err != nil {
		return runImage, err
	}
	_, pin, _ := strings.Cut(BuildpacksRunImage, "@")
	runImage, err := readCNBImage(raw, runtime.GOARCH, pin)
	if err != nil {
		return runImage, err
	}
	if err := verifyCNBOutput(ctx, raw, runImage, runImage); err != nil {
		return runImage, err
	}
	if err := stageCNBLayout(raw, filepath.Join(inputs, "run"), runImage); err != nil {
		return runImage, err
	}
	if err := e.recipeCreateVolume(ctx, r, "cnb-inputs"); err != nil {
		return runImage, err
	}
	stage := []string{"docker", "create", "--pull", "never", "--name", recipeResourceName(r, "cnb-stage"), "--network", "none", "--read-only", "--cap-drop", "ALL", "--mount", "type=volume,src=" + recipeResourceName(r, "cnb-inputs") + ",dst=/inputs", "--entrypoint", "/bin/true"}
	stage = append(stage, recipeResourceLabels(r)...)
	if err := e.runBuildpacks(ctx, r, append(stage, BuildpacksBuilderImage), io.Discard); err != nil {
		return runImage, err
	}
	lifecycle := []string{"docker", "create", "--pull", "never", "--name", recipeResourceName(r, "cnb-lifecycle-stage"), "--network", "none", "--read-only", "--cap-drop", "ALL", "--entrypoint", "/cnb/lifecycle/analyzer"}
	lifecycle = append(lifecycle, recipeResourceLabels(r)...)
	if err := e.runBuildpacks(ctx, r, append(lifecycle, BuildpacksLifecycleImage, "-help"), io.Discard); err != nil {
		return runImage, err
	}
	if err := e.runBuildpacks(ctx, r, []string{"docker", "cp", recipeResourceName(r, "cnb-lifecycle-stage") + ":/cnb/lifecycle", inputs}, io.Discard); err != nil {
		return runImage, err
	}
	for _, name := range []string{"source", "run", "lifecycle"} {
		if err := e.runBuildpacks(ctx, r, []string{"docker", "cp", "--archive", filepath.Join(inputs, name), recipeResourceName(r, "cnb-stage") + ":/inputs/" + name}, io.Discard); err != nil {
			return runImage, err
		}
	}
	return runImage, nil
}

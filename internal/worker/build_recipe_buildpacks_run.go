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
	"time"
)

type cnbBoundedWriter struct {
	output    io.Writer
	remaining int64
}

func buildpacksUnderPressure(capacity BuildpacksCapacity) bool {
	return capacity.MemoryAvailable < BuildpacksSafetyBytes || capacity.DiskAvailable < 2<<30 || capacity.StateAvailable < 1<<30
}

func (writer *cnbBoundedWriter) Write(data []byte) (int, error) {
	if int64(len(data)) > writer.remaining {
		return 0, errors.New("CNB exported archive exceeds 2 GiB")
	}
	written, err := writer.output.Write(data)
	writer.remaining -= int64(written)
	return written, err
}

func (e *Engine) watchBuildpacksPressure(ctx context.Context, r Request, cancel context.CancelCauseFunc) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			capacity, err := e.buildpacksCapacity(ctx, r.BuildPlatform)
			if err != nil {
				cancel(fmt.Errorf("%w: live node resource accounting became unavailable", errBuildpacksPressure))
				return
			}
			if buildpacksUnderPressure(capacity) {
				cancel(errBuildpacksPressure)
				return
			}
			usage, err := e.outputBuildpacks(ctx, r, "docker", "exec", recipeResourceName(r, "cnb-keeper"), "/cove-worker", "buildpacks-helper", "usage")
			var used uint64
			if err != nil || json.Unmarshal([]byte(usage), &used) != nil || used > BuildpacksWorkBytes {
				cancel(fmt.Errorf("%w: the monitored disk work target was exceeded or could not be read (not a hard storage cap)", errBuildpacksPressure))
				return
			}
		}
	}
}

func (e *Engine) prepareBuildpacksBuild(parent context.Context, r Request, project string, original map[string]any, out io.Writer) error {
	e.buildMu.Lock()
	defer e.buildMu.Unlock()
	ctx, timeout := context.WithTimeout(parent, 25*time.Minute)
	defer timeout()
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	if err := os.MkdirAll(filepath.Join(e.recipeInputs(r), "cnb-docker-config"), 0700); err != nil {
		return err
	}
	if _, err := e.recoverBuildCache(ctx, out, false); err != nil {
		return err
	}
	if err := validateBuildpacksPolicy(original); err != nil {
		return err
	}
	artifacts, stopArtifacts := context.WithTimeout(ctx, 5*time.Minute)
	err := e.stageBuildpacksImages(artifacts, r)
	stopArtifacts()
	if err != nil {
		return err
	}
	runImage, err := e.stageBuildpacksInputs(ctx, r, project)
	if err != nil {
		return err
	}
	capacity, err := e.buildpacksCapacity(ctx, r.BuildPlatform)
	if err != nil {
		return err
	}
	if buildpacksUnderPressure(capacity) {
		return errBuildpacksPressure
	}
	if err := e.createBuildpacksState(ctx, r); err != nil {
		return err
	}
	if err := e.startBuildpacksKeeper(ctx, r); err != nil {
		return err
	}
	capacity, err = e.buildpacksCapacity(ctx, r.BuildPlatform)
	if err != nil {
		return err
	}
	if buildpacksUnderPressure(capacity) {
		return errBuildpacksPressure
	}
	// Admission precedes allocation of the operation's helper state. Keep that
	// frozen budget while monitoring actual headroom throughout execution.
	if capacity.MemoryAvailable >= BuildpacksMinimumMemoryBytes+BuildpacksSafetyBytes {
		if err := e.admitBuildpacksPhases(r, capacity.MemoryAvailable); err != nil {
			return err
		}
	}
	monitorDone := make(chan struct{})
	go func() { defer close(monitorDone); e.watchBuildpacksPressure(ctx, r, cancel) }()
	defer func() { cancel(nil); <-monitorDone }()
	if err := e.executeBuildpacksPhases(ctx, r, original, out); err != nil {
		if cause := context.Cause(ctx); cause != nil {
			return errors.Join(err, cause)
		}
		return err
	}
	return e.importBuildpacksOutput(ctx, r, runImage)
}

func (e *Engine) executeBuildpacksPhases(ctx context.Context, r Request, original map[string]any, out io.Writer) error {
	network, proxies, err := recipeBuildPolicy(original)
	if err != nil {
		return err
	}
	network, proxy, err := e.prepareRecipeEgress(ctx, r, network, proxies)
	if err != nil {
		return err
	}
	for _, phase := range []string{"analyzer", "detector", "restorer"} {
		phaseNetwork, phaseProxy := "none", ""
		if phase == "detector" {
			phaseNetwork, phaseProxy = network, proxy
		}
		if err := e.runBuildpacksPhase(ctx, r, phase, phaseNetwork, phaseProxy, out); err != nil {
			return err
		}
	}
	before, err := e.readBuildpacksSnapshot(ctx, r)
	if err != nil {
		return err
	}
	expected, err := hashBuildpacksRun(ctx, filepath.Join(e.recipeInputs(r), "run"))
	if err != nil || !maps.Equal(before.Run, expected) {
		return errors.New("CNB staged run image does not match the verified release pin")
	}
	if err := writeJSON(filepath.Join(e.recipeInputs(r), "cnb-frozen.json"), before); err != nil {
		return err
	}
	if _, err := e.stageBuildpacksSecrets(ctx, r); err != nil {
		return err
	}
	if err := e.runBuildpacksPhase(ctx, r, "builder", network, proxy, out); err != nil {
		return err
	}
	after, err := e.readBuildpacksSnapshot(ctx, r)
	if err != nil {
		return err
	}
	if err := verifyBuildpacksSnapshot(before, after); err != nil {
		return err
	}
	return e.runBuildpacksPhase(ctx, r, "exporter", "none", "", out)
}

func (e *Engine) importBuildpacksOutput(ctx context.Context, r Request, runImage cnbImage) error {
	inputs := e.recipeInputs(r)
	archive := filepath.Join(inputs, "cnb-export.tar")
	file, err := os.OpenFile(archive, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	readErr := e.runBuildpacks(ctx, r, []string{"docker", "exec", recipeResourceName(r, "cnb-keeper"), "/cove-worker", "buildpacks-helper", "archive"}, &cnbBoundedWriter{output: file, remaining: cnbArchiveLimit})
	closeErr := file.Close()
	if readErr != nil || closeErr != nil {
		return errors.Join(readErr, closeErr)
	}
	directory := filepath.Join(inputs, "cnb-export")
	if err := unpackCNBArchive(ctx, archive, directory, false); err != nil {
		return err
	}
	image, err := readCNBImage(directory, runImage.Arch, "")
	if err != nil {
		return err
	}
	if err := verifyCNBOutput(ctx, directory, image, runImage); err != nil {
		return err
	}
	selected := map[string]bool{}
	for _, name := range recipeSecretNames(r) {
		selected[name] = true
	}
	for _, secret := range r.FileSecrets {
		if selected[secret.Name] && cnbConfigContainsSecret(image.Config, []string{r.Environment[secret.Variable]}) {
			return errors.New("CNB export contains a selected build secret in image configuration; publication refused")
		}
	}
	owner, err := e.buildOwner()
	if err != nil {
		return err
	}
	importPath := filepath.Join(inputs, "cnb-import.tar")
	if err := writeCNBImport(directory, importPath, buildImageTag(r, r.BuildRecipe.Service), owner, image); err != nil {
		return err
	}
	return e.runBuildpacks(ctx, r, []string{"docker", "image", "load", "--input", importPath}, io.Discard)
}

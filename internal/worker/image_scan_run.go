package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func (e *Engine) recoverImageScan(ctx context.Context, r Request) error {
	if _, err := os.Stat(filepath.Join(e.Root, "build-owner.json")); err == nil {
		owner, err := e.buildOwner()
		if err != nil {
			return err
		}
		for _, name := range []string{"cove-scan-" + owner + "-" + r.ID, "cove-scan-db-" + owner + "-" + r.ID} {
			if err = e.removeScanContainer(ctx, name, owner); err != nil {
				return err
			}
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	root := filepath.Join(e.Root, "scanner-inputs")
	entries, err := os.ReadDir(root)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), r.ID+"-") {
			if err = os.RemoveAll(filepath.Join(root, entry.Name())); err != nil {
				return err
			}
		}
	}
	return nil
}

func (e *Engine) performImageScan(ctx context.Context, r Request, out io.Writer) (err error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Minute)
	defer cancel()
	fmt.Fprintln(out, "Waiting for the node's vulnerability scanner.")
	for !e.scanMu.TryLock() {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
	defer e.scanMu.Unlock()
	report := ImageScan{ScannerImage: ScannerImage, ScannerVersion: scannerVersion, State: "unknown", Images: []ScannedImage{}}
	defer func() {
		if ctx.Err() != nil {
			err = errors.Join(err, ctx.Err())
		}
		report.CheckedAt = time.Now().UTC()
		e.mu.Lock()
		defer e.mu.Unlock()
		j, readErr := e.read(r.ID)
		if readErr != nil {
			err = errors.Join(err, readErr)
			return
		}
		data, marshalErr := json.Marshal(report)
		if marshalErr != nil || len(data) > 512*1024 {
			err = errors.Join(err, errors.New("normalized scanner report exceeds its limit"))
			return
		}
		j.Operation.ImageScan = &report
		err = errors.Join(err, writeJSON(e.path(r.ID), j))
	}()
	if !scannerEnabled() {
		report.Message = "Image scanning has not been enabled by the node operator."
		return nil
	}
	e.mu.Lock()
	pending, readErr := e.read(r.ID)
	if readErr == nil {
		pending.ImageScanCleanup = true
		readErr = writeJSON(e.path(r.ID), pending)
	}
	e.mu.Unlock()
	if readErr != nil {
		return readErr
	}
	defer func() {
		cleanup, stop := context.WithTimeout(context.Background(), time.Minute)
		defer stop()
		if cleanupErr := e.recoverImageScan(cleanup, r); cleanupErr != nil {
			err = errors.Join(err, errors.New("image scanner cleanup is pending; worker recovery will retry"))
			return
		}
		e.mu.Lock()
		defer e.mu.Unlock()
		pending, readErr := e.read(r.ID)
		if readErr == nil {
			pending.ImageScanCleanup = false
			readErr = writeJSON(e.path(r.ID), pending)
		}
		err = errors.Join(err, readErr)
	}()
	inputRoot := filepath.Join(e.Root, "scanner-inputs")
	if err = os.MkdirAll(inputRoot, 0700); err != nil {
		return err
	}
	dir, err := os.MkdirTemp(inputRoot, r.ID+"-")
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, os.RemoveAll(dir)) }()
	owner, err := e.buildOwner()
	if err != nil {
		return err
	}
	scannerImage, err := e.ensureScannerImage(ctx, dir, r.OutboundProxy)
	if err != nil {
		report.Message = "The digest-pinned scanner image is unavailable."
		return nil
	}
	fmt.Fprintln(out, "Checking or refreshing the bounded vulnerability database.")
	var database scanDatabaseCache
	if r.AutomaticScan != nil {
		database, err = e.automaticScanDatabase(ctx, r)
	} else {
		database, err = e.scanDatabase(ctx, owner, r.ID, scannerImage, r.OutboundProxy)
	}
	report.Database = database.ScanDatabase
	if err != nil {
		report.Message = "The vulnerability database is missing, stale or could not be refreshed within its limits."
		fmt.Fprintln(out, report.Message)
		return nil
	}
	fmt.Fprintln(out, "Database ready; scanning exact retained images offline.")
	containers, err := e.Containers(ctx, r.Workload)
	if err != nil || len(containers) == 0 || len(containers) > 64 {
		report.Message = "Retained deployed image coverage is unavailable; deploy the service or run the job first."
		return nil
	}
	seen := map[string]bool{}
	images := map[string]ScannedImage{}
	findingsLeft := 512
	jobPresent := r.Kind != "job"
	for _, container := range containers {
		var deployed struct{ Image, Service string }
		if err = e.imageJSON(ctx, nil, &deployed, "docker", "inspect", "--format", `{"image":{{json .Image}},"service":{{json (or (index .Config.Labels "dev.cove.service") (index .Config.Labels "com.docker.compose.service"))}}}`, container.ID); err != nil || !imageDigest.MatchString(deployed.Image) || !serviceIdentifier.MatchString(deployed.Service) {
			report.Message = "A retained deployed image could not be identified."
			return nil
		}
		jobPresent = jobPresent || deployed.Service == r.JobService
		platform := e.scanPlatform(ctx, container.ID)
		imageIdentity := scanImageIdentity(deployed.Image, platform)
		key := deployed.Service + "/" + imageIdentity
		if seen[key] {
			continue
		}
		seen[key] = true
		image, cached := images[imageIdentity]
		if !cached {
			if len(images) >= 16 {
				report.Message = "This resource exceeds the limit of 16 distinct deployed images per scan."
				return nil
			}
			archive := filepath.Join(dir, "input.tar")
			if removeErr := os.Remove(archive); removeErr != nil && !os.IsNotExist(removeErr) {
				return removeErr
			}
			file, createErr := os.OpenFile(archive, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
			if createErr != nil {
				return createErr
			}
			exportErr := e.stream(ctx, []string{"docker", "image", "save", deployed.Image}, &scanLimitedWriter{Writer: file, Left: scanArchiveLimit}, io.Discard)
			closeErr := file.Close()
			if exportErr != nil || closeErr != nil {
				report.Message = "The exact deployed image could not be exported within its size limit."
				return nil
			}
			identity, inspectErr := inspectScanArchive(ctx, archive)
			if inspectErr != nil {
				report.Message = "The exported image configuration or package coverage could not be verified."
				return nil
			}
			if platform == "" && identity.ConfigDigest != deployed.Image || platform != "" && identity.Manifests[platform] != identity.ConfigDigest {
				report.Message = "The exported image does not prove the container's exact platform configuration; coverage is unknown."
				return nil
			}
			if err = os.Chmod(archive, 0444); err != nil {
				return err
			}
			image, err = e.scanImageArchive(ctx, owner, r.ID, deployed.Image, archive, filepath.Join(e.Root, "scanner", database.Path, "db"), identity, scannerImage)
			if err != nil {
				return err
			}
			image.Identity = imageIdentity
			images[imageIdentity] = image
		}
		image.Service = deployed.Service
		if len(image.Findings) > findingsLeft {
			image.Findings = image.Findings[:findingsLeft]
			image.Truncated = true
		}
		findingsLeft -= len(image.Findings)
		report.Images = append(report.Images, image)
		fmt.Fprintf(out, "Vulnerability scan for service %s: %s.\n", deployed.Service, image.State)
	}
	if !jobPresent {
		report.Message = "The retained containers do not include the selected job service."
		return nil
	}
	if r.AutomaticScan != nil {
		if _, err := e.automaticScanDatabase(ctx, r); err != nil {
			report.Message = "Automatic scan inputs or resource state changed; coverage is unknown."
			return nil
		}
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if !report.Database.Fresh(time.Now().UTC()) {
		report.Message = "The vulnerability database became stale during the scan."
		return nil
	}
	report.State = "complete"
	for _, image := range report.Images {
		if image.State != "complete" {
			report.State = "unknown"
			report.Message = "Some deployed image coverage is unknown; review each image's coverage and findings."
		}
	}
	return nil
}

func (e *Engine) scanImageArchive(ctx context.Context, owner, operation, imageID, archive, database string, identity scanArchive, scannerImage string) (result ScannedImage, err error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	result = ScannedImage{ImageID: imageID, ConfigDigest: identity.ConfigDigest, State: "unknown", Counts: map[string]int{}, Findings: []ScanFinding{}, Coverage: ScanCoverage{OS: "unknown", OtherLanguages: "unknown", JavaArchives: "unknown"}}
	defer func() { applyScanArchiveCoverage(&result, identity) }()
	name := "cove-scan-" + owner + "-" + operation
	args := scannerContainerArgs(name, owner, operation)
	args = append(args, "--network", "none", "--cpus", "1", "--memory", "2g",
		"--tmpfs", "/cache:size=512m,uid=65532,gid=65532", "--tmpfs", "/tmp:size=512m,uid=65532,gid=65532", "--tmpfs", "/report:size=8m,uid=65532,gid=65532",
		"--mount", "type=bind,source="+archive+",target=/input/image.tar,readonly", "--mount", "type=bind,source="+database+",target=/cache/db,readonly",
		"--entrypoint", "/bin/sh", scannerImage, "-c",
		`trivy image --input /input/image.tar --cache-dir /cache --scanners vuln --offline-scan --skip-db-update --skip-java-db-update --skip-check-update --skip-version-check --disable-telemetry --parallel 1 --skip-files '**/*.[jJ][aA][rR]' --skip-files '**/*.[wW][aA][rR]' --skip-files '**/*.[eE][aA][rR]' --format json --list-all-pkgs=false --output /report/result.json; code=$?; printf '%s' "$code" > /report/exit; exec sleep 300`)
	defer func() {
		cleanup, stop := context.WithTimeout(context.Background(), 30*time.Second)
		defer stop()
		if cleanupErr := e.removeScanContainer(cleanup, name, owner); cleanupErr != nil {
			err = errors.Join(err, cleanupErr)
		}
	}()
	if e.Run(ctx, e.Root, args, nil, io.Discard) != nil {
		result.Message = "The offline scanner could not start within its resource limits."
		return result, nil
	}
	for {
		exit, exitErr := e.output(ctx, "docker", "exec", name, "cat", "/report/exit")
		if exitErr == nil {
			if strings.TrimSpace(exit) != "0" {
				result.Message = "The offline scan failed or exceeded its memory, scratch or output limits."
				return result, nil
			}
			break
		}
		state, stateErr := e.output(ctx, "docker", "inspect", "--format", "{{.State.Running}}", name)
		if stateErr != nil || strings.TrimSpace(state) != "true" {
			result.Message = "The offline scanner stopped before reporting a complete result."
			return result, nil
		}
		select {
		case <-ctx.Done():
			result.Message = "The offline scanner timed out or was cancelled."
			return result, ctx.Err()
		case <-time.After(time.Second):
		}
	}
	output := &limitedBuffer{Limit: scanOutputLimit}
	if e.Run(ctx, e.Root, []string{"docker", "exec", name, "cat", "/report/result.json"}, nil, output) != nil {
		result.Message = "The scanner report is unavailable or exceeds its size limit."
		return result, nil
	}
	parsed, parseErr := parseImageScan(output.data.Bytes(), "", imageID, identity.ConfigDigest)
	if parseErr != nil {
		result.Message = "The scanner report could not be bound to the exported image."
		return result, nil
	}
	return parsed, nil
}

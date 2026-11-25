package worker

import (
	"archive/tar"
	"context"
	"cove/internal/outbound"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const scanDatabaseLimit int64 = 2 * 1024 * 1024 * 1024

type scanDatabaseCache struct {
	ScanDatabase
	Path string `json:"path"`
}

type scanLimitedWriter struct {
	Writer io.Writer
	Left   int64
}

func (w *scanLimitedWriter) Write(data []byte) (int, error) {
	if int64(len(data)) > w.Left {
		return 0, errors.New("scanner data exceeds its size limit")
	}
	n, err := w.Writer.Write(data)
	w.Left -= int64(n)
	return n, err
}

func scannerEnabled() bool { return os.Getenv("COVE_IMAGE_SCANNING") == "1" }

func scannerContainerArgs(name, owner, operation string) []string {
	return []string{"docker", "run", "-d", "--pull", "never", "--name", name,
		"--label", "cove.managed=image-scan", "--label", "cove.image-scan-owner=" + owner,
		"--label", "dev.cove.operation=" + operation,
		"--read-only", "--cap-drop", "ALL", "--security-opt", "no-new-privileges:true",
		"--user", "65532:65532", "--pids-limit", "128", "--log-driver", "none"}
}

func (e *Engine) removeScanContainer(ctx context.Context, name, owner string) error {
	ids, err := e.output(ctx, "docker", "ps", "-aq", "--filter", "label=cove.image-scan-owner="+owner, "--filter", "name="+name)
	if err != nil {
		return errors.New("scanner container inventory could not be read")
	}
	if strings.TrimSpace(ids) == "" {
		return nil
	}
	var inspected []struct {
		Name   string
		Config struct{ Labels map[string]string }
	}
	out := &limitedBuffer{Limit: 128 * 1024}
	if err := e.Run(ctx, e.Root, []string{"docker", "inspect", name}, nil, out); err != nil {
		return errors.New("scanner container ownership could not be read")
	}
	if json.Unmarshal(out.data.Bytes(), &inspected) != nil || len(inspected) != 1 || inspected[0].Name != "/"+name || inspected[0].Config.Labels["cove.managed"] != "image-scan" || inspected[0].Config.Labels["cove.image-scan-owner"] != owner {
		return errors.New("scanner container ownership mismatch")
	}
	return e.Run(ctx, e.Root, []string{"docker", "rm", "-f", "-v", name}, nil, io.Discard)
}

func (e *Engine) ensureScannerImage(ctx context.Context, dir string, selected ...*outbound.Config) (string, error) {
	ctx, stop := context.WithTimeout(ctx, 2*time.Minute)
	defer stop()
	config := filepath.Join(dir, "docker-config")
	if err := os.Mkdir(config, 0700); err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(config, "config.json"), []byte("{}"), 0600); err != nil {
		return "", err
	}
	if err := e.Run(ctx, e.Root, []string{"docker", "image", "inspect", ScannerImage}, nil, io.Discard); err == nil {
		return ScannerImage, nil
	}
	digest := strings.Split(ScannerImage, "@")[1]
	if id, err := e.output(ctx, "docker", "image", "inspect", "--format", "{{.Id}}", digest); err == nil && strings.TrimSpace(id) == digest {
		return digest, nil
	}
	proxy, err := e.scanOutboundPolicy(selected...)
	if err != nil {
		return "", err
	}
	if err := e.requireDockerProxy(ctx, &proxy); err != nil {
		return "", err
	}
	environment := append(proxy.Env(), "DOCKER_CONFIG="+config)
	if err := e.Run(ctx, e.Root, []string{"docker", "pull", "--quiet", ScannerImage}, environment, io.Discard); err != nil {
		return "", errors.New("the pinned scanner image is unavailable")
	}
	return ScannerImage, nil
}

func extractScanDatabase(input io.Reader, destination string) (ScanDatabase, error) {
	reader := tar.NewReader(io.LimitReader(input, scanDatabaseLimit+1024*1024))
	allowed := map[string]int64{"db/trivy.db": scanDatabaseLimit, "db/metadata.json": 16384}
	seen := map[string]bool{}
	total := int64(0)
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return ScanDatabase{}, err
		}
		name := strings.TrimPrefix(header.Name, "./")
		if header.Typeflag == tar.TypeDir && (name == "." || name == "db/" || name == "db") {
			continue
		}
		limit, ok := allowed[name]
		if !ok || seen[name] || header.Typeflag != tar.TypeReg || header.Size <= 0 || header.Size > limit || header.Size > scanDatabaseLimit-total {
			return ScanDatabase{}, errors.New("scanner database archive has invalid entries or exceeds its limit")
		}
		seen[name] = true
		total += header.Size
		target := filepath.Join(destination, filepath.FromSlash(name))
		if err = os.MkdirAll(filepath.Dir(target), 0700); err != nil {
			return ScanDatabase{}, err
		}
		file, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			return ScanDatabase{}, err
		}
		_, copyErr := io.CopyN(file, reader, header.Size)
		closeErr := file.Close()
		if copyErr != nil || closeErr != nil {
			return ScanDatabase{}, fmt.Errorf("scanner database copy failed: %w", errors.Join(copyErr, closeErr))
		}
	}
	if len(seen) != len(allowed) {
		return ScanDatabase{}, errors.New("scanner database archive is incomplete")
	}
	return validateScanDatabase(destination)
}

func validateScanDatabase(dir string) (ScanDatabase, error) {
	hash := sha256.New()
	result := ScanDatabase{}
	total := int64(0)
	for _, prefix := range []string{"db"} {
		info, err := os.Lstat(filepath.Join(dir, prefix, "metadata.json"))
		if err != nil || !info.Mode().IsRegular() || info.Size() > 16384 {
			return result, errors.New("scanner database metadata unavailable")
		}
		data, err := os.ReadFile(filepath.Join(dir, prefix, "metadata.json"))
		if err != nil || len(data) > 16384 {
			return result, errors.New("scanner database metadata unavailable")
		}
		var metadata struct {
			Version    int
			UpdatedAt  time.Time
			NextUpdate time.Time
		}
		if json.Unmarshal(data, &metadata) != nil || metadata.Version != 2 || metadata.UpdatedAt.IsZero() || metadata.NextUpdate.IsZero() {
			return result, errors.New("scanner database metadata invalid")
		}
		if result.UpdatedAt.IsZero() || metadata.UpdatedAt.Before(result.UpdatedAt) {
			result.UpdatedAt = metadata.UpdatedAt
		}
		if result.NextUpdate.IsZero() || metadata.NextUpdate.Before(result.NextUpdate) {
			result.NextUpdate = metadata.NextUpdate
		}
		hash.Write(data)
		total += int64(len(data))
		filename := "trivy.db"
		info, err = os.Lstat(filepath.Join(dir, prefix, filename))
		if err != nil || !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > scanDatabaseLimit-total {
			return result, errors.New("scanner database files exceed their limit or are unavailable")
		}
		total += info.Size()
		file, err := os.Open(filepath.Join(dir, prefix, filename))
		if err != nil {
			return result, err
		}
		_, copyErr := io.Copy(hash, io.LimitReader(file, scanDatabaseLimit+1))
		file.Close()
		if copyErr != nil {
			return result, copyErr
		}
	}
	result.Digest = "sha256:" + hex.EncodeToString(hash.Sum(nil))
	if !result.Fresh(time.Now().UTC()) {
		return result, errors.New("scanner database is missing, stale or future-dated")
	}
	return result, nil
}

func (e *Engine) refreshScanDatabase(ctx context.Context, owner, operation, destination, scannerImage string, selected ...*outbound.Config) (result ScanDatabase, err error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	name := "cove-scan-db-" + owner + "-" + operation
	proxy, err := e.scanOutboundPolicy(selected...)
	if err != nil {
		return result, err
	}
	args := scannerContainerArgs(name, owner, operation)
	for _, value := range proxy.Env() {
		key, _, _ := strings.Cut(value, "=")
		args = append(args, "--env", key)
	}
	args = append(args, "--cpus", "2", "--memory", "3g", "--env", "TMPDIR=/cache", "--tmpfs", "/cache:size=2g,uid=65532,gid=65532", "--tmpfs", "/tmp:size=16m,uid=65532,gid=65532", "--entrypoint", "/bin/sh", scannerImage, "-ec",
		"trivy image --cache-dir /cache --skip-version-check --disable-telemetry --db-repository ghcr.io/aquasecurity/trivy-db:2 --download-db-only; touch /tmp/ready; exec sleep 600")
	defer func() {
		cleanup, stop := context.WithTimeout(context.Background(), 30*time.Second)
		defer stop()
		if cleanupErr := e.removeScanContainer(cleanup, name, owner); cleanupErr != nil {
			err = errors.Join(err, cleanupErr)
		}
	}()
	if err = e.Run(ctx, e.Root, args, proxy.Env(), io.Discard); err != nil {
		return result, errors.New("scanner database refresh could not start")
	}
	for {
		if e.Run(ctx, e.Root, []string{"docker", "exec", name, "test", "-f", "/tmp/ready"}, nil, io.Discard) == nil {
			break
		}
		state, stateErr := e.output(ctx, "docker", "inspect", "--format", "{{.State.Running}}", name)
		if stateErr != nil || strings.TrimSpace(state) != "true" {
			return result, errors.New("scanner database refresh failed")
		}
		select {
		case <-ctx.Done():
			return result, errors.New("scanner database refresh timed out")
		case <-time.After(time.Second):
		}
	}
	reader, writer := io.Pipe()
	done := make(chan error, 1)
	go func() {
		copyErr := e.stream(ctx, []string{"docker", "exec", name, "tar", "-C", "/cache", "-cf", "-", "db"}, &scanLimitedWriter{Writer: writer, Left: scanDatabaseLimit + 1024*1024}, io.Discard)
		writer.CloseWithError(copyErr)
		done <- copyErr
	}()
	result, err = extractScanDatabase(reader, destination)
	if err == nil {
		_, err = io.Copy(io.Discard, reader)
	}
	reader.CloseWithError(err)
	if copyErr := <-done; copyErr != nil {
		return result, errors.New("scanner database transfer failed")
	}
	return result, err
}

func (e *Engine) scanOutboundPolicy(selected ...*outbound.Config) (outbound.Config, error) {
	proxy := outbound.Environment()
	if len(selected) > 0 && selected[0] != nil {
		proxy = *selected[0]
	} else {
		saved, err := readOutboundConfig(e.Root)
		if err != nil {
			return proxy, err
		}
		if saved != nil {
			proxy = *saved
		}
	}
	return proxy, proxy.Validate()
}

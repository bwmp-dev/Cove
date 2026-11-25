package worker

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

func extractNixpacksArchive(data []byte, artifact nixpacksArtifact) ([]byte, error) {
	sum := sha256.Sum256(data)
	if int64(len(data)) != artifact.Bytes || hex.EncodeToString(sum[:]) != artifact.SHA256 {
		return nil, errors.New("the pinned Nixpacks archive failed its size or SHA-256 check")
	}
	zipped, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, errors.New("the pinned Nixpacks archive is not valid gzip")
	}
	defer zipped.Close()
	packed := tar.NewReader(io.LimitReader(zipped, 65<<20))
	var binary []byte
	for entries := 0; ; entries++ {
		entry, err := packed.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, errors.New("the pinned Nixpacks archive is truncated")
		}
		name := strings.TrimSuffix(entry.Name, "/")
		if entries >= 20 || path.Clean(name) != name || path.IsAbs(name) || name == ".." || strings.HasPrefix(name, "../") || strings.ContainsAny(name, "\\\x00\r\n:") {
			return nil, errors.New("the pinned Nixpacks archive contains an invalid path or exceeds its bound")
		}
		if entry.Typeflag == tar.TypeDir {
			continue
		}
		if entry.Typeflag != tar.TypeReg || path.Base(entry.Name) != "nixpacks" || binary != nil || entry.Size <= 0 || entry.Size > 64<<20 {
			return nil, errors.New("the pinned Nixpacks archive contains unexpected files")
		}
		binary, err = io.ReadAll(io.LimitReader(packed, entry.Size+1))
		if err != nil || int64(len(binary)) != entry.Size {
			return nil, errors.New("the pinned Nixpacks executable is incomplete")
		}
	}
	if len(binary) < 4 || !bytes.Equal(binary[:4], []byte{0x7f, 'E', 'L', 'F'}) {
		return nil, errors.New("the pinned Nixpacks executable is unavailable")
	}
	return binary, nil
}

func nixpacksArtifactRedirect(request *http.Request, previous []*http.Request) error {
	host := request.URL.Hostname()
	if len(previous) > 3 || request.URL.Scheme != "https" || request.URL.User != nil || request.URL.Port() != "" && request.URL.Port() != "443" || host != "github.com" && host != "release-assets.githubusercontent.com" && host != "objects.githubusercontent.com" {
		return errors.New("Nixpacks artifact redirected outside its official HTTPS distribution")
	}
	return nil
}

func (e *Engine) nixpacksBinary(ctx context.Context) ([]byte, error) {
	artifact, err := nixpacksPlannerArtifact(runtime.GOARCH)
	if err != nil {
		return nil, err
	}
	directory := filepath.Join(e.Root, "build-artifacts", "nixpacks-"+NixpacksVersion, runtime.GOARCH)
	if err := os.MkdirAll(directory, 0700); err != nil {
		return nil, err
	}
	file := filepath.Join(directory, artifact.Name)
	info, err := os.Lstat(file)
	if err == nil {
		if !info.Mode().IsRegular() || info.Size() != artifact.Bytes {
			return nil, errors.New("the cached Nixpacks archive is not the pinned regular file; repair the worker's artifact cache")
		}
		data, err := os.ReadFile(file)
		if err != nil {
			return nil, err
		}
		return extractNixpacksArchive(data, artifact)
	}
	if !os.IsNotExist(err) {
		return nil, err
	}
	transport := &http.Transport{Proxy: nil, DialContext: (&net.Dialer{Timeout: 10 * time.Second}).DialContext, TLSHandshakeTimeout: 10 * time.Second, ResponseHeaderTimeout: 15 * time.Second}
	defer transport.CloseIdleConnections()
	client := &http.Client{Timeout: time.Minute, Transport: transport, CheckRedirect: nixpacksArtifactRedirect}
	request, err := http.NewRequestWithContext(ctx, "GET", "https://github.com/railwayapp/nixpacks/releases/download/v"+NixpacksVersion+"/"+artifact.Name, nil)
	if err != nil {
		return nil, err
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, errors.New("the pinned Nixpacks planner could not be downloaded; check the worker's HTTPS access and public CA trust")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK || response.ContentLength > artifact.Bytes {
		return nil, errors.New("the pinned Nixpacks download returned an unexpected status or size")
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, artifact.Bytes+1))
	if err != nil {
		return nil, errors.New("the pinned Nixpacks download did not complete")
	}
	binary, err := extractNixpacksArchive(data, artifact)
	if err != nil {
		return nil, err
	}
	pending, err := os.CreateTemp(directory, ".verified-")
	if err != nil {
		return nil, err
	}
	defer os.Remove(pending.Name())
	_, writeErr := pending.Write(data)
	closeErr := pending.Close()
	if err := errors.Join(writeErr, closeErr); err != nil {
		return nil, err
	}
	if err := os.Rename(pending.Name(), file); err != nil {
		return nil, err
	}
	return binary, nil
}

func (e *Engine) nixpacksPlannerImage(ctx context.Context) (string, error) {
	e.helperImageMu.Lock()
	defer e.helperImageMu.Unlock()
	binary, err := e.nixpacksBinary(ctx)
	if err != nil {
		return "", err
	}
	executable, err := os.Executable()
	if err != nil {
		return "", err
	}
	helper, err := os.ReadFile(executable)
	if err != nil {
		return "", err
	}
	hash := sha256.New()
	hash.Write(binary)
	hash.Write(helper)
	digest := hex.EncodeToString(hash.Sum(nil))
	tag := "cove-nixpacks:" + NixpacksVersion + "-" + digest[:16]
	if label, err := e.output(ctx, "docker", "image", "inspect", "--format", `{{index .Config.Labels "cove.nixpacks.helper"}}`, tag); err == nil {
		if label != digest {
			return "", errors.New("the Nixpacks helper image has conflicting ownership")
		}
		return tag, nil
	}
	directory, err := os.MkdirTemp(e.Root, "nixpacks-helper-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(directory)
	archive := filepath.Join(directory, "rootfs.tar")
	file, err := os.OpenFile(archive, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return "", err
	}
	packed := tar.NewWriter(file)
	for _, name := range []string{"inputs/", "tmp/", "output/"} {
		if err == nil {
			err = packed.WriteHeader(&tar.Header{Name: name, Mode: 0755, Typeflag: tar.TypeDir})
		}
	}
	for _, entry := range []struct {
		name string
		data []byte
	}{{"nixpacks", binary}, {"cove-worker", helper}} {
		if err == nil {
			err = packed.WriteHeader(&tar.Header{Name: entry.name, Mode: 0555, Size: int64(len(entry.data)), Typeflag: tar.TypeReg})
		}
		if err == nil {
			_, err = packed.Write(entry.data)
		}
	}
	err = errors.Join(err, packed.Close(), file.Close())
	if err != nil {
		return "", err
	}
	arguments := []string{"docker", "image", "import", "--platform", "linux/" + runtime.GOARCH, "--change", `ENTRYPOINT ["/cove-worker"]`, "--change", "USER 65534:65534", "--change", "LABEL cove.managed=build-recipe-helper", "--change", "LABEL cove.nixpacks.helper=" + digest, archive, tag}
	if err := e.Run(ctx, e.Root, arguments, nil, io.Discard); err != nil {
		return "", errors.New("the confined Nixpacks helper image could not be prepared")
	}
	return tag, nil
}

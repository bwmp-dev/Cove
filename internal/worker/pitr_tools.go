package worker

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"time"
)

const pitrToolsVersion = "3.0.10"
const pitrToolLimit int64 = 128 << 20

func (e *Engine) pitrToolStage() (string, error) {
	owner, err := e.backupHelperOwner(true)
	if err != nil {
		return "", err
	}
	stage := filepath.Join(e.Root, "pitr-tool-stage")
	if err := os.MkdirAll(stage, 0700); err != nil {
		return "", err
	}
	info, err := os.Lstat(stage)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0077 != 0 {
		return "", errors.New("PITR tool staging directory is not private")
	}
	entries, err := os.ReadDir(stage)
	if err != nil {
		return "", err
	}
	marker := filepath.Join(stage, "owner")
	if info, err := os.Lstat(marker); err == nil {
		if !info.Mode().IsRegular() || info.Size() != 32 {
			return "", errors.New("PITR staging ownership is invalid")
		}
		data, err := os.ReadFile(marker)
		if err != nil || string(data) != owner {
			return "", errors.New("PITR staging ownership changed")
		}
	} else if !os.IsNotExist(err) {
		return "", err
	} else {
		if len(entries) != 0 {
			return "", errors.New("refusing an unowned PITR staging directory")
		}
		if err := os.WriteFile(marker, []byte(owner), 0600); err != nil {
			return "", err
		}
	}
	for _, entry := range entries {
		if entry.Name() == "owner" {
			continue
		}
		if entry.Name() != "release.tar.gz" && entry.Name() != "wal-g" && entry.Name() != "Dockerfile" {
			return "", errors.New("unexpected PITR staging artifact")
		}
		info, err := entry.Info()
		if err != nil || !info.Mode().IsRegular() || info.Size() > pitrToolLimit {
			return "", errors.New("unsafe PITR staging artifact")
		}
		if err := os.Remove(filepath.Join(stage, entry.Name())); err != nil {
			return "", err
		}
	}
	return stage, nil
}

func clearPITRToolStage(stage string) error {
	var result error
	for _, name := range []string{"release.tar.gz", "wal-g", "Dockerfile"} {
		if err := os.Remove(filepath.Join(stage, name)); err != nil && !os.IsNotExist(err) {
			result = errors.Join(result, err)
		}
	}
	return result
}

func pitrToolRelease(arch string) (string, string, error) {
	switch arch {
	case "amd64":
		return "wal-g-pg-24.04-amd64", "03ead332e08282be8e57dc10762fd2c418ba6a38262f6e3d096252ef45339fd5", nil
	case "arm64":
		return "wal-g-pg-24.04-aarch64", "b472c96cf4b34377bf45a7c2cc3bc7a60e8eef369c7e956786143bebd378ed0c", nil
	default:
		return "", "", errors.New("PITR requires an amd64 or arm64 Linux worker")
	}
}

func extractPITRTool(source io.Reader, destination io.Writer, name string) error {
	compressed, err := gzip.NewReader(source)
	if err != nil {
		return errors.New("invalid WAL-G release archive")
	}
	defer compressed.Close()
	archive := tar.NewReader(compressed)
	entry, err := archive.Next()
	if err != nil || entry.Name != name || entry.Typeflag != tar.TypeReg || entry.Size < 1 || entry.Size > pitrToolLimit {
		return errors.New("unexpected WAL-G release contents")
	}
	if _, err := io.CopyN(destination, archive, entry.Size); err != nil {
		return err
	}
	if _, err := archive.Next(); err != io.EOF {
		return errors.New("WAL-G release contains additional entries")
	}
	return nil
}

func (e *Engine) preparePITRTools(ctx context.Context) (image string, result error) {
	if runtime.GOOS != "linux" {
		return "", errors.New("PITR requires Linux")
	}
	name, checksum, err := pitrToolRelease(runtime.GOARCH)
	if err != nil {
		return "", err
	}
	e.storageMu.Lock()
	defer e.storageMu.Unlock()
	image = "cove-pitr:wal-g" + pitrToolsVersion + "-rclone1.76.0-" + runtime.GOARCH
	if owner, err := e.output(ctx, "docker", "image", "inspect", "--format", `{{index .Config.Labels "dev.cove.pitr.tools"}}`, image); err == nil {
		if owner != checksum {
			return "", errors.New("PITR helper image ownership changed")
		}
		return image, nil
	}
	stage, err := e.pitrToolStage()
	if err != nil {
		return "", err
	}
	defer func() { result = errors.Join(result, clearPITRToolStage(stage)) }()
	budget, err := transferDiskBudget(stage, 2*pitrToolLimit)
	if err != nil || budget < 2*pitrToolLimit {
		return "", errors.New("PITR tool preparation needs 256 MiB plus the 256 MiB free-space reserve")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://github.com/wal-g/wal-g/releases/download/v"+pitrToolsVersion+"/"+name+".tar.gz", nil)
	if err != nil {
		return "", err
	}
	client := &http.Client{Timeout: 2 * time.Minute}
	response, err := client.Do(request)
	if err != nil {
		return "", errors.New("could not download the pinned WAL-G release over verified HTTPS")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "", errors.New("WAL-G release download was rejected")
	}
	archive, err := os.OpenFile(filepath.Join(stage, "release.tar.gz"), os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		return "", err
	}
	defer archive.Close()
	digest := sha256.New()
	n, err := io.Copy(io.MultiWriter(archive, digest), io.LimitReader(response.Body, (32<<20)+1))
	if err != nil || n > 32<<20 || hex.EncodeToString(digest.Sum(nil)) != checksum {
		return "", errors.New("WAL-G release failed checksum or size validation")
	}
	if _, err := archive.Seek(0, io.SeekStart); err != nil {
		return "", err
	}
	tool, err := os.OpenFile(filepath.Join(stage, "wal-g"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0700)
	if err != nil {
		return "", err
	}
	extractErr := extractPITRTool(archive, tool, name)
	closeErr := tool.Close()
	if extractErr != nil || closeErr != nil {
		return "", errors.Join(extractErr, closeErr)
	}
	dockerfile := "FROM rclone/rclone:1.76.0 AS rclone\nFROM ubuntu:24.04\nRUN apt-get update && apt-get install -y --no-install-recommends ca-certificates libsodium23 liblzo2-2 libbrotli1 liblzma5 && rm -rf /var/lib/apt/lists/*\nCOPY --from=rclone /usr/local/bin/rclone /usr/local/bin/rclone\nCOPY --chmod=755 wal-g /usr/local/bin/wal-g\nLABEL dev.cove.pitr.tools=" + checksum + "\n"
	if err := os.WriteFile(filepath.Join(stage, "Dockerfile"), []byte(dockerfile), 0600); err != nil {
		return "", err
	}
	if err := e.Run(ctx, stage, []string{"docker", "build", "--tag", image, "."}, nil, io.Discard); err != nil {
		return "", errors.New("could not prepare the pinned PITR helper image")
	}
	return image, nil
}

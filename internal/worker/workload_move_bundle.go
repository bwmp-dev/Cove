package worker

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

type moveWriter struct {
	Writer    io.Writer
	Remaining int64
}

func (w *moveWriter) Write(data []byte) (int, error) {
	if int64(len(data)) > w.Remaining {
		return 0, errors.New("move bundle exceeds its byte limit")
	}
	n, err := w.Writer.Write(data)
	w.Remaining -= int64(n)
	return n, err
}

func (e *Engine) moveHelper(ctx context.Context, j moveJournal, writable bool, command []string, out io.Writer) error {
	owner, err := e.backupHelperOwner(true)
	if err != nil {
		return err
	}
	args := []string{"docker", "run", "--rm", "--log-driver", "none", "--name", "cove-move-" + j.ID, "--label", "dev.cove.backup-owner=" + owner, "--label", "dev.cove.workload=" + j.Workload, "--label", "dev.cove.operation=" + j.ID, "--network", "none", "--read-only", "--tmpfs", "/tmp", "--cap-drop", "ALL", "--cap-add", "DAC_OVERRIDE", "--tmpfs", "/data"}
	if writable {
		args = append(args, "--cap-add", "CHOWN", "--cap-add", "FOWNER", "--cap-add", "DAC_OVERRIDE", "--cap-add", "FSETID", "--mount", "type=bind,source="+filepath.Join(e.moveDirectory(j.ID), "volumes.tar")+",target=/input.tar,readonly")
	}
	for i, volume := range j.Plan.Volumes {
		mount := fmt.Sprintf("type=volume,source=%s,target=/data/%03d", volume.Name, i)
		if !writable {
			mount += ",readonly"
		}
		args = append(args, "--mount", mount)
	}
	args = append(args, "busybox:1.37")
	args = append(args, command...)
	return e.Run(ctx, e.Root, args, nil, out)
}

func appendMoveFile(tw *tar.Writer, filename, name string, budget *int64) error {
	info, err := os.Lstat(filename)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() < 0 || info.Size() > *budget {
		return errors.New("move metadata contains a link, special file or exceeds its bound")
	}
	*budget -= info.Size()
	header, err := tar.FileInfoHeader(info, "")
	if err != nil {
		return err
	}
	header.Name = name
	if err = tw.WriteHeader(header); err != nil {
		return err
	}
	file, err := os.Open(filename)
	if err != nil {
		return err
	}
	_, copyErr := io.CopyN(tw, file, info.Size())
	return errors.Join(copyErr, file.Close())
}

func (e *Engine) writeMoveMetadata(j moveJournal, tw *tar.Writer) ([]string, error) {
	images := map[string]bool{}
	count := 0
	budget := moveMetadataLimit
	roots := []string{filepath.Join("workloads", j.Workload), filepath.Join("file-secrets", j.Workload)}
	for _, relative := range roots {
		base := filepath.Join(e.Root, relative)
		if _, err := os.Lstat(base); os.IsNotExist(err) {
			continue
		} else if err != nil {
			return nil, err
		}
		err := filepath.WalkDir(base, func(filename string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() {
				if entry.Name() == "releases" {
					return filepath.SkipDir
				}
				return nil
			}
			count++
			if count > 16384 {
				return errors.New("move metadata exceeds 16384 files")
			}
			rel, err := filepath.Rel(e.Root, filename)
			if err != nil {
				return err
			}
			if strings.HasSuffix(filename, ".compose.json") {
				data, err := os.ReadFile(filename)
				if err != nil || len(data) > 2<<20 {
					return errors.New("retained release exceeds its bound")
				}
				var config struct {
					Services map[string]struct{ Image string }
				}
				if json.Unmarshal(data, &config) != nil {
					return errors.New("retained release is invalid")
				}
				for _, service := range config.Services {
					if !strings.HasPrefix(service.Image, "sha256:") || len(service.Image) != 71 {
						return errors.New("retained release image is not immutable")
					}
					images[service.Image] = true
				}
			}
			return appendMoveFile(tw, filename, "metadata/"+filepath.ToSlash(rel), &budget)
		})
		if err != nil {
			return nil, err
		}
	}
	entries, err := os.ReadDir(filepath.Join(e.Root, "operations"))
	if err != nil {
		return nil, err
	}
	if len(entries) > 100000 {
		return nil, errors.New("operation inventory exceeds the move scan bound; run retention first")
	}
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		id := strings.TrimSuffix(entry.Name(), ".json")
		record, err := e.read(id)
		if err != nil {
			return nil, err
		}
		if record.Operation.Workload != j.Workload {
			continue
		}
		if record.Operation.State == "running" || record.Operation.State == "queued" {
			return nil, errors.New("operation became active during move")
		}
		count++
		if count > 16384 {
			return nil, errors.New("move metadata exceeds 16384 files")
		}
		if err = appendMoveFile(tw, e.path(id), "metadata/operations/"+id+".json", &budget); err != nil {
			return nil, err
		}
		log, err := e.Logs(id)
		if err != nil {
			return nil, err
		}
		if int64(len(log)) > budget {
			return nil, errors.New("move journal logs exceed their bound")
		}
		budget -= int64(len(log))
		if err = tw.WriteHeader(&tar.Header{Name: "metadata/operations/" + id + ".log", Mode: 0600, Size: int64(len(log)), Typeflag: tar.TypeReg}); err != nil {
			return nil, err
		}
		if _, err = io.WriteString(tw, log); err != nil {
			return nil, err
		}
	}
	if len(images) == 0 || len(images) > 256 {
		return nil, errors.New("move requires 1-256 retained immutable images")
	}
	result := []string{}
	for image := range images {
		result = append(result, image)
	}
	sort.Strings(result)
	return result, nil
}

func (e *Engine) exportMove(ctx context.Context, j *moveJournal) error {
	dir := e.moveDirectory(j.ID)
	containers, err := e.Containers(ctx, j.Workload)
	if err != nil {
		return err
	}
	known := map[string]bool{}
	for _, container := range j.Containers {
		known[container.ID] = true
	}
	if len(containers) != len(known) {
		return errors.New("source containers changed after freeze")
	}
	for _, container := range containers {
		if !known[container.ID] || container.State == "running" || container.State == "restarting" || container.State == "paused" {
			return errors.New("source is no longer frozen")
		}
	}
	if j.Phase == "exported" {
		sum, err := fileSum(filepath.Join(dir, "bundle.tar"))
		if err != nil || sum != j.SHA256 {
			return errors.New("saved move bundle integrity mismatch")
		}
		return nil
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	free, err := freeBytes(dir)
	if err != nil {
		return err
	}
	limit := min(moveBundleLimit, (free-(256<<20))/2)
	if limit < 1<<20 {
		return errors.New("move staging needs at least 256 MiB of free reserve")
	}
	volumes, err := os.OpenFile(filepath.Join(dir, "volumes.tar"), os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	err = e.moveHelper(ctx, *j, false, []string{"tar", "-C", "/data", "-cpf", "-", "."}, &moveWriter{Writer: volumes, Remaining: limit})
	err = errors.Join(err, volumes.Close())
	if err != nil {
		return err
	}
	metadata, err := os.OpenFile(filepath.Join(dir, "metadata.tar"), os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	tw := tar.NewWriter(metadata)
	images, err := e.writeMoveMetadata(*j, tw)
	err = errors.Join(err, tw.Close(), metadata.Close())
	if err != nil {
		return err
	}
	volumeInfo, err := os.Stat(filepath.Join(dir, "volumes.tar"))
	if err != nil {
		return err
	}
	metadataInfo, err := os.Stat(filepath.Join(dir, "metadata.tar"))
	if err != nil {
		return err
	}
	remaining := limit - volumeInfo.Size() - metadataInfo.Size() - (1 << 20)
	if remaining < 1 {
		return errors.New("move payload exceeds available staging space")
	}
	file, err := os.OpenFile(filepath.Join(dir, "images.tar"), os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	err = e.Run(ctx, e.Root, append([]string{"docker", "image", "save"}, images...), nil, &moveWriter{Writer: file, Remaining: remaining})
	err = errors.Join(err, file.Close())
	if err != nil {
		return err
	}
	file, err = os.OpenFile(filepath.Join(dir, "bundle.part"), os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	hash := sha256.New()
	bounded := &moveWriter{Writer: io.MultiWriter(file, hash), Remaining: limit}
	tw = tar.NewWriter(bounded)
	manifest, err := json.Marshal(j)
	if err == nil {
		err = tw.WriteHeader(&tar.Header{Name: "move.json", Mode: 0600, Size: int64(len(manifest)), Typeflag: tar.TypeReg})
	}
	if err == nil {
		_, err = tw.Write(manifest)
	}
	for _, name := range []string{"metadata.tar", "images.tar", "volumes.tar"} {
		if err != nil {
			break
		}
		remaining := limit
		err = appendMoveFile(tw, filepath.Join(dir, name), name, &remaining)
	}
	err = errors.Join(err, tw.Close(), file.Sync(), file.Close())
	if err != nil {
		return err
	}
	if err = os.Rename(filepath.Join(dir, "bundle.part"), filepath.Join(dir, "bundle.tar")); err != nil {
		return err
	}
	j.Bytes = limit - bounded.Remaining
	j.SHA256 = hex.EncodeToString(hash.Sum(nil))
	j.Phase = "exported"
	if err = e.saveMove(j); err != nil {
		return err
	}
	for _, name := range []string{"metadata.tar", "images.tar", "volumes.tar"} {
		if err = os.Remove(filepath.Join(dir, name)); err != nil {
			return err
		}
	}
	return nil
}

func validMoveTarName(name string) bool {
	return name != "" && !strings.ContainsAny(name, "\\\x00\r\n") && !strings.HasPrefix(name, "/") && path.Clean(name) == strings.TrimSuffix(name, "/") && name != ".." && !strings.HasPrefix(name, "../")
}

package worker

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const cnbLayoutOutput = "/work/layout/cove.invalid/build/output/result"

type cnbSnapshot struct {
	Metadata map[string]string `json:"metadata"`
	Run      map[string]string `json:"run"`
}

func cnbFileHash(ctx context.Context, path string, maximum int64) (string, error) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > maximum {
		return "", errors.New("CNB frozen metadata is missing, oversized or not a regular file")
	}
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	written, err := io.Copy(hash, io.LimitReader(scanContextReader{ctx, file}, maximum+1))
	if err != nil || written != info.Size() {
		return "", errors.New("CNB frozen metadata changed while being read")
	}
	return "sha256:" + hex.EncodeToString(hash.Sum(nil)), nil
}

func snapshotBuildpacks(ctx context.Context, metadata, run string) (cnbSnapshot, error) {
	result := cnbSnapshot{Metadata: map[string]string{}, Run: map[string]string{}}
	for _, name := range []string{"analyzed.toml", "group.toml", "plan.toml"} {
		digest, err := cnbFileHash(ctx, filepath.Join(metadata, name), cnbMetadataLimit)
		if err != nil {
			return result, err
		}
		result.Metadata[name] = digest
	}
	runHashes, err := hashBuildpacksRun(ctx, run)
	result.Run = runHashes
	return result, err
}

func hashBuildpacksRun(ctx context.Context, run string) (map[string]string, error) {
	result := map[string]string{}
	entries, total := 0, int64(0)
	err := filepath.WalkDir(run, func(path string, entry fs.DirEntry, failure error) error {
		if failure != nil {
			return failure
		}
		if entry.IsDir() {
			return nil
		}
		entries++
		if entries > 512 {
			return errors.New("CNB run layout exceeds its entry bound")
		}
		info, err := entry.Info()
		if err != nil || !info.Mode().IsRegular() || info.Size() > cnbArchiveLimit-total {
			return errors.New("CNB run layout contains unsupported entries or exceeds its bound")
		}
		total += info.Size()
		digest, err := cnbFileHash(ctx, path, cnbArchiveLimit)
		if err != nil {
			return err
		}
		name, err := filepath.Rel(run, path)
		if err != nil {
			return err
		}
		result[filepath.ToSlash(name)] = digest
		return nil
	})
	if err == nil && len(result) < 4 {
		err = errors.New("CNB pinned run layout is incomplete")
	}
	return result, err
}

func archiveBuildpacks(ctx context.Context, directory string, out io.Writer) error {
	packed := tar.NewWriter(out)
	var total int64
	entries := 0
	err := filepath.WalkDir(directory, func(path string, entry fs.DirEntry, failure error) error {
		if failure != nil {
			return failure
		}
		if entry.IsDir() {
			return nil
		}
		name, err := filepath.Rel(directory, path)
		if err != nil {
			return err
		}
		name = filepath.ToSlash(name)
		info, err := entry.Info()
		entries++
		if err != nil || !info.Mode().IsRegular() || entries > 1024 || info.Size() < 0 || info.Size() > cnbArchiveLimit-total || name != "index.json" && name != "oci-layout" && !strings.HasPrefix(name, "blobs/sha256/") {
			return errors.New("CNB export exceeds its bounds or contains an unsupported path")
		}
		total += info.Size()
		if err := packed.WriteHeader(&tar.Header{Name: name, Mode: 0600, Typeflag: tar.TypeReg, Size: info.Size()}); err != nil {
			return err
		}
		file, err := os.Open(path)
		if err != nil {
			return err
		}
		written, copyErr := io.Copy(packed, scanContextReader{ctx, file})
		file.Close()
		if copyErr != nil || written != info.Size() {
			return errors.New("CNB exported file changed while being read")
		}
		return nil
	})
	if err != nil {
		return err
	}
	return packed.Close()
}

func RunBuildpacksHelper(ctx context.Context, action string, out io.Writer) error {
	switch action {
	case "own":
		entries, err := os.ReadDir("/work")
		if err != nil || len(entries) != 0 {
			return errors.New("CNB ownership initialization requires an empty operation work volume")
		}
		if err := os.Chmod("/work", 0700); err != nil {
			return err
		}
		return os.Chown("/work", 1002, 1001)
	case "usage":
		var total uint64
		entries := 0
		err := filepath.WalkDir("/work", func(path string, entry fs.DirEntry, failure error) error {
			if failure != nil {
				return failure
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			entries++
			if entries > maxRecipeInputEntries {
				return errors.New("CNB disk work exceeds its monitored entry target")
			}
			info, err := entry.Info()
			if err != nil || info.Size() < 0 {
				return errors.New("CNB disk work usage is unavailable")
			}
			if !info.IsDir() {
				total += uint64(info.Size())
			}
			return nil
		})
		if err != nil {
			return err
		}
		return json.NewEncoder(out).Encode(total)
	case "wait":
		timer := time.NewTimer(30 * time.Minute)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return nil
		case <-timer.C:
			return nil
		}
	case "init":
		if err := verifyBuildpacksWorkFilesystem(); err != nil {
			return err
		}
		for _, directory := range []string{"/work/layers", "/work/cache", "/work/layout"} {
			if err := os.MkdirAll(directory, 0755); err != nil {
				return err
			}
		}
		return copyRecipeTree(ctx, "/inputs/source", "/work/app", false)
	case "snapshot":
		snapshot, err := snapshotBuildpacks(ctx, "/meta", "/inputs/run")
		if err != nil {
			return err
		}
		return json.NewEncoder(out).Encode(snapshot)
	case "archive":
		return archiveBuildpacks(ctx, cnbLayoutOutput, out)
	default:
		return errors.New("unknown CNB helper action")
	}
}

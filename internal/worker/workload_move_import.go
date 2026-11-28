package worker

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
)

func readMoveArchive(filename string, visit func(*tar.Header, io.Reader) error) error {
	file, err := os.Open(filename)
	if err != nil {
		return err
	}
	defer file.Close()
	tr := tar.NewReader(file)
	for count := 0; ; count++ {
		header, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if count >= 100000 {
			return errors.New("move archive exceeds 100000 entries")
		}
		if err = visit(header, tr); err != nil {
			return err
		}
	}
}

func validateMoveVolumes(filename string, count int) error {
	seen := map[string]byte{}
	return readMoveArchive(filename, func(h *tar.Header, _ io.Reader) error {
		name := strings.TrimSuffix(strings.TrimPrefix(h.Name, "./"), "/")
		if name == "." || name == "" {
			return nil
		}
		if !validMoveTarName(name) || len(name) > 4096 || strings.Count(name, "/") > 64 {
			return errors.New("move volume archive path is invalid")
		}
		prefix, _, _ := strings.Cut(name, "/")
		valid := false
		for i := 0; i < count; i++ {
			if prefix == moveVolumeIndex(i) {
				valid = true
				break
			}
		}
		if !valid {
			return errors.New("archive refers to an unowned volume")
		}
		if _, exists := seen[name]; exists {
			return errors.New("move volume archive repeats a path")
		}
		for parent := path.Dir(name); parent != "."; parent = path.Dir(parent) {
			if typ, exists := seen[parent]; exists && typ != tar.TypeDir {
				return errors.New("move archive traverses a non-directory")
			}
		}
		switch h.Typeflag {
		case tar.TypeReg, tar.TypeRegA, tar.TypeDir, tar.TypeFifo:
		case tar.TypeSymlink:
			if strings.ContainsRune(h.Linkname, 0) || len(h.Linkname) > 4096 {
				return errors.New("invalid move symbolic link")
			}
		case tar.TypeLink:
			target := strings.TrimPrefix(h.Linkname, "./")
			if !validMoveTarName(target) || strings.Split(target, "/")[0] != prefix || seen[target] != tar.TypeReg {
				return errors.New("move hard link escapes its volume or target")
			}
		default:
			return errors.New("move archive contains a device or unsupported special entry")
		}
		if h.Size < 0 || h.Size > moveBundleLimit {
			return errors.New("invalid move file size")
		}
		for key := range h.PAXRecords {
			if strings.Contains(strings.ToLower(key), "sparse") {
				return errors.New("sparse move archive is unsupported")
			}
		}
		typ := h.Typeflag
		if typ == tar.TypeRegA {
			typ = tar.TypeReg
		}
		seen[name] = typ
		return nil
	})
}

func (e *Engine) extractMoveMetadata(j moveJournal, filename string) error {
	budget := moveMetadataLimit
	count := 0
	return readMoveArchive(filename, func(h *tar.Header, body io.Reader) error {
		if !strings.HasPrefix(h.Name, "metadata/") || h.Typeflag != tar.TypeReg || h.Size < 0 || h.Size > budget {
			return errors.New("move metadata type or size is invalid")
		}
		budget -= h.Size
		count++
		if count > 32768 {
			return errors.New("move metadata file count exceeded")
		}
		relative := strings.TrimPrefix(h.Name, "metadata/")
		if !validMoveTarName(relative) {
			return errors.New("unsafe move metadata path")
		}
		allowed := strings.HasPrefix(relative, "workloads/"+j.Workload+"/") || strings.HasPrefix(relative, "file-secrets/"+j.Workload+"/")
		if strings.HasPrefix(relative, "operations/") {
			base := strings.TrimPrefix(relative, "operations/")
			allowed = !strings.Contains(base, "/") && (strings.HasSuffix(base, ".json") || strings.HasSuffix(base, ".log")) && identifier.MatchString(strings.TrimSuffix(strings.TrimSuffix(base, ".json"), ".log"))
		}
		if !allowed {
			return errors.New("move metadata crosses resource boundary")
		}
		destination := filepath.Join(e.moveDirectory(j.ID), "metadata", filepath.FromSlash(relative))
		if err := os.MkdirAll(filepath.Dir(destination), 0700); err != nil {
			return err
		}
		file, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			return err
		}
		_, err = io.CopyN(file, body, h.Size)
		return errors.Join(err, file.Close())
	})
}

func validateMoveImages(filename string) error {
	found := false
	err := readMoveArchive(filename, func(h *tar.Header, body io.Reader) error {
		if h.Name != "manifest.json" {
			return nil
		}
		if found || h.Size > 1<<20 {
			return errors.New("move image manifest exceeds its bound")
		}
		found = true
		var manifests []struct{ RepoTags []string }
		if json.NewDecoder(io.LimitReader(body, (1<<20)+1)).Decode(&manifests) != nil || len(manifests) == 0 || len(manifests) > 256 {
			return errors.New("invalid move image manifest")
		}
		for _, manifest := range manifests {
			if len(manifest.RepoTags) > 0 {
				return errors.New("moving images may not replace named registry tags")
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	if !found {
		return errors.New("move image manifest is missing")
	}
	return nil
}

func (e *Engine) receiveMove(ctx context.Context, r MoveRequest, size int64, sum string, input io.Reader) (*MoveState, error) {
	if !identifier.MatchString(r.ID) || !workloadIdentifier.MatchString(r.Workload) || size < 1 || size > moveBundleLimit || len(sum) != 64 {
		return nil, errors.New("invalid move transfer identity or size")
	}
	if decoded, err := hex.DecodeString(sum); err != nil || len(decoded) != 32 {
		return nil, errors.New("invalid move transfer digest")
	}
	e.moveMu.Lock()
	defer e.moveMu.Unlock()
	j, err := e.readMove(r.Workload)
	newReceipt := os.IsNotExist(err) || err == nil && j.ID != r.ID
	if err == nil && j.ID == r.ID && j.Phase == "importing" {
		if j.Bytes != size || j.SHA256 != sum {
			return nil, errors.New("move transfer changed")
		}
		if err = discardMovePayload(input, size); err != nil {
			return nil, err
		}
		err = e.installMove(ctx, &j)
		return &j.MoveState, err
	}
	if err == nil && j.ID == r.ID && (j.Phase == "imported" || j.Phase == "starting" || j.Phase == "started" || j.Phase == "committed") {
		if j.Bytes == size && j.SHA256 == sum {
			if err = discardMovePayload(input, size); err != nil {
				return nil, err
			}
			return &j.MoveState, nil
		}
		return nil, errors.New("move transfer changed")
	}
	if err == nil && j.ID != r.ID && j.Phase != "departed" && j.Phase != "discarded" && j.Phase != "rolled_back" {
		return nil, errors.New("another move owns target resource")
	}
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	if _, err = os.Lstat(filepath.Join(e.Root, "workloads", r.Workload)); !os.IsNotExist(err) {
		return nil, errors.New("target resource directory already exists")
	}
	e.mu.Lock()
	busy := e.active[r.Workload] || e.active[housekeepingWorkload] || e.draining
	for _, op := range e.pending {
		busy = busy || op.Workload == r.Workload
	}
	if !busy {
		e.active[r.Workload] = true
	}
	e.mu.Unlock()
	if busy {
		return nil, errors.New("target worker is busy")
	}
	defer func() { e.mu.Lock(); delete(e.active, r.Workload); e.mu.Unlock() }()
	if newReceipt {
		if err = e.admitMoveReceipt(); err != nil {
			return nil, err
		}
	}
	dir := e.moveDirectory(r.ID)
	if err = os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	j = moveJournal{MoveState: MoveState{ID: r.ID, Workload: r.Workload, Role: "target", Phase: "receiving"}}
	if err = e.saveMove(&j); err != nil {
		return nil, err
	}
	free, err := freeBytes(dir)
	if err != nil || size > (free-(256<<20))/2 {
		return nil, errors.New("target staging needs twice the transfer size plus 256 MiB reserve")
	}
	filename := filepath.Join(dir, "bundle.part")
	file, err := os.OpenFile(filename, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if err != nil {
		return nil, err
	}
	hash := sha256.New()
	n, err := io.CopyN(io.MultiWriter(file, hash), input, size)
	err = errors.Join(err, file.Sync(), file.Close())
	if err != nil || n != size || hex.EncodeToString(hash.Sum(nil)) != sum {
		return nil, errors.New("move transfer was incomplete or failed its checksum")
	}
	var trailing [1]byte
	if n, err := input.Read(trailing[:]); n != 0 || err != io.EOF {
		return nil, errors.New("move transfer has trailing bytes")
	}
	if err = os.Rename(filename, filepath.Join(dir, "bundle.tar")); err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	err = readMoveArchive(filepath.Join(dir, "bundle.tar"), func(h *tar.Header, body io.Reader) error {
		if h.Typeflag != tar.TypeReg || seen[h.Name] || h.Size < 0 || h.Size > moveBundleLimit {
			return errors.New("invalid move bundle entry")
		}
		seen[h.Name] = true
		switch h.Name {
		case "move.json":
			if h.Size > 1<<20 || json.NewDecoder(io.LimitReader(body, (1<<20)+1)).Decode(&j) != nil {
				return errors.New("invalid move manifest")
			}
		case "metadata.tar", "images.tar", "volumes.tar":
			f, err := os.OpenFile(filepath.Join(dir, h.Name), os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
			if err != nil {
				return err
			}
			_, err = io.CopyN(f, body, h.Size)
			return errors.Join(err, f.Close())
		default:
			return errors.New("unexpected move bundle entry")
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(seen) != 4 || j.ID != r.ID || j.Workload != r.Workload || j.Role != "source" || j.Plan == nil || j.Plan.Root != e.Root || len(j.Plan.Volumes) > 17 || len(j.Containers) > 128 {
		return nil, errors.New("move manifest does not match the target or exceeds its bounds")
	}
	identity, err := e.output(ctx, "docker", "info", "--format", "{{.ID}}|{{.Architecture}}")
	if err != nil {
		return nil, err
	}
	if identity == j.Plan.Daemon+"|"+j.Plan.Architecture || !strings.HasSuffix(identity, "|"+j.Plan.Architecture) {
		return nil, errors.New("move needs a different Docker daemon with the same architecture")
	}
	if err = validateMoveVolumes(filepath.Join(dir, "volumes.tar"), len(j.Plan.Volumes)); err != nil {
		return nil, err
	}
	if err = validateMoveImages(filepath.Join(dir, "images.tar")); err != nil {
		return nil, err
	}
	// No target Docker data is changed until both nested archives have been validated.
	if err = os.RemoveAll(filepath.Join(dir, "metadata")); err != nil {
		return nil, err
	}
	if err = e.extractMoveMetadata(j, filepath.Join(dir, "metadata.tar")); err != nil {
		return nil, err
	}
	j.Role, j.Phase, j.Bytes, j.SHA256 = "target", "importing", size, sum
	if err = e.saveMove(&j); err != nil {
		return nil, err
	}
	if err = e.installMove(ctx, &j); err != nil {
		return nil, err
	}
	return &j.MoveState, nil
}

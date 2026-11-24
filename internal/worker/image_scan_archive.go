package worker

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
)

const scanArchiveLimit int64 = 2 * 1024 * 1024 * 1024

type scanContextReader struct {
	Context context.Context
	Reader  io.Reader
}

func (r scanContextReader) Read(data []byte) (int, error) {
	if err := r.Context.Err(); err != nil {
		return 0, err
	}
	return r.Reader.Read(data)
}

type scanArchive struct {
	ConfigDigest string
	JavaArchives bool
	Manifests    map[string]string
}

func inspectScanArchive(ctx context.Context, filename string) (scanArchive, error) {
	file, err := os.Open(filename)
	if err != nil {
		return scanArchive{}, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || info.Size() <= 0 || info.Size() > scanArchiveLimit {
		return scanArchive{}, errors.New("image archive exceeds its size limit")
	}
	reader := tar.NewReader(scanContextReader{ctx, file})
	type imageManifest struct {
		Config string
		Layers []string
	}
	var manifest []imageManifest
	manifestSeen := false
	digests := map[string]string{}
	layers := map[string]tar.Header{}
	manifests := map[string]string{}
	for entries := 0; ; entries++ {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil || entries > 2048 {
			return scanArchive{}, errors.New("image archive is invalid or exceeds its entry limit")
		}
		if header.Typeflag == tar.TypeDir {
			continue
		}
		if header.Typeflag != tar.TypeReg || header.Size < 0 || header.Size > scanArchiveLimit || digests[header.Name] != "" {
			return scanArchive{}, errors.New("image archive has unsupported entries")
		}
		if header.Name == "manifest.json" {
			if manifestSeen || header.Size > 65536 || json.NewDecoder(io.LimitReader(reader, 65537)).Decode(&manifest) != nil {
				return scanArchive{}, errors.New("image archive manifest is invalid")
			}
			manifestSeen = true
			continue
		}
		hash := sha256.New()
		var small bytes.Buffer
		var destination io.Writer = hash
		if header.Size <= 65536 {
			destination = io.MultiWriter(hash, &small)
		}
		if _, err = io.Copy(destination, reader); err != nil {
			return scanArchive{}, err
		}
		digests[header.Name] = "sha256:" + hex.EncodeToString(hash.Sum(nil))
		var platform struct{ Config struct{ Digest string } }
		if json.Unmarshal(small.Bytes(), &platform) == nil && imageDigest.MatchString(platform.Config.Digest) {
			manifests[digests[header.Name]] = platform.Config.Digest
		}
		layers[header.Name] = *header
	}
	if len(manifest) != 1 || len(manifest[0].Layers) > 512 || !imageDigest.MatchString(digests[manifest[0].Config]) || layers[manifest[0].Config].Size > 1024*1024 {
		return scanArchive{}, errors.New("image archive must identify one bounded image configuration")
	}
	result := scanArchive{ConfigDigest: digests[manifest[0].Config], Manifests: manifests}
	wanted := map[string]bool{}
	for _, name := range manifest[0].Layers {
		if !imageDigest.MatchString(digests[name]) || wanted[name] {
			return result, errors.New("image archive layer is missing or repeated")
		}
		wanted[name] = true
	}
	if _, err = file.Seek(0, io.SeekStart); err != nil {
		return result, err
	}
	reader = tar.NewReader(scanContextReader{ctx, file})
	remaining := scanArchiveLimit
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			return result, nil
		}
		if err != nil {
			return result, err
		}
		if !wanted[header.Name] {
			continue
		}
		buffer := bufio.NewReader(reader)
		var source io.Reader = buffer
		magic, _ := buffer.Peek(2)
		var compressed *gzip.Reader
		if len(magic) == 2 && magic[0] == 0x1f && magic[1] == 0x8b {
			compressed, err = gzip.NewReader(buffer)
			if err != nil {
				return result, err
			}
			source = compressed
		}
		bounded := &io.LimitedReader{R: scanContextReader{ctx, source}, N: remaining + 1}
		layer := tar.NewReader(bounded)
		for entries := 0; ; entries++ {
			entry, layerErr := layer.Next()
			if errors.Is(layerErr, io.EOF) {
				break
			}
			if layerErr != nil || entries > 1000000 {
				return result, errors.New("image layer inspection failed or exceeds its limits")
			}
			lower := strings.ToLower(entry.Name)
			if strings.HasSuffix(lower, ".jar") || strings.HasSuffix(lower, ".war") || strings.HasSuffix(lower, ".ear") {
				result.JavaArchives = true
			}
		}
		if _, err = io.Copy(io.Discard, bounded); err != nil {
			return result, err
		}
		if bounded.N == 0 {
			return result, errors.New("image layers exceed the decompressed inspection limit")
		}
		remaining -= remaining + 1 - bounded.N
		if compressed != nil {
			compressed.Close()
		}
	}
}

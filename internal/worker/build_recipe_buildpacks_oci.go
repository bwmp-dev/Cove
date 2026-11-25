package worker

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

const cnbArchiveLimit int64 = 2 << 30
const cnbExpandedLimit int64 = 4 << 30
const cnbMetadataLimit int64 = 2 << 20

type cnbDescriptor struct {
	MediaType   string            `json:"mediaType"`
	Digest      string            `json:"digest"`
	Size        int64             `json:"size"`
	URLs        []string          `json:"urls,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty"`
	Platform    *struct {
		OS           string `json:"os"`
		Architecture string `json:"architecture"`
	} `json:"platform,omitempty"`
}

type cnbIndex struct {
	SchemaVersion int             `json:"schemaVersion"`
	MediaType     string          `json:"mediaType"`
	Manifests     []cnbDescriptor `json:"manifests"`
}

type cnbManifest struct {
	SchemaVersion int             `json:"schemaVersion"`
	MediaType     string          `json:"mediaType"`
	Config        cnbDescriptor   `json:"config"`
	Layers        []cnbDescriptor `json:"layers"`
}

type cnbImage struct {
	Descriptor cnbDescriptor
	Manifest   cnbManifest
	Config     []byte
	DiffIDs    []string
	Arch       string
}

func cnbDigest(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func cnbBlobPath(directory, digest string) (string, error) {
	if !imageDigest.MatchString(digest) {
		return "", errors.New("CNB output contains an invalid blob digest")
	}
	return filepath.Join(directory, "blobs", "sha256", digest[7:]), nil
}

func unpackCNBArchive(ctx context.Context, archive, directory string, dockerSave bool) error {
	file, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer file.Close()
	stat, err := file.Stat()
	if err != nil || stat.Size() <= 0 || stat.Size() > cnbArchiveLimit {
		return errors.New("CNB OCI archive is empty or exceeds 2 GiB")
	}
	if err := os.MkdirAll(filepath.Join(directory, "blobs", "sha256"), 0700); err != nil {
		return err
	}
	reader := tar.NewReader(scanContextReader{ctx, file})
	seen := map[string]bool{}
	var total int64
	for count := 0; ; count++ {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil || count > 1024 || header.Size < 0 || header.Size > cnbArchiveLimit-total {
			return errors.New("CNB OCI archive is invalid or exceeds its bounds")
		}
		if header.Typeflag == tar.TypeDir && (header.Name == "blobs" || header.Name == "blobs/" || header.Name == "blobs/sha256" || header.Name == "blobs/sha256/") {
			continue
		}
		if header.Typeflag != tar.TypeReg || seen[header.Name] {
			return errors.New("CNB OCI archive contains duplicate or unsupported entries")
		}
		seen[header.Name] = true
		blob := strings.HasPrefix(header.Name, "blobs/sha256/") && imageDigest.MatchString("sha256:"+strings.TrimPrefix(header.Name, "blobs/sha256/"))
		metadata := header.Name == "index.json" || header.Name == "oci-layout" || dockerSave && header.Name == "manifest.json"
		if !blob && !metadata || metadata && header.Size > cnbMetadataLimit {
			return errors.New("CNB OCI archive contains an unsupported path or oversized metadata")
		}
		total += header.Size
		path := filepath.Join(directory, filepath.FromSlash(header.Name))
		output, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			return err
		}
		hash := sha256.New()
		written, copyErr := io.Copy(io.MultiWriter(output, hash), reader)
		closeErr := output.Close()
		if copyErr != nil || closeErr != nil || written != header.Size {
			return errors.New("CNB OCI archive could not be read completely")
		}
		if blob && hex.EncodeToString(hash.Sum(nil)) != strings.TrimPrefix(header.Name, "blobs/sha256/") {
			return errors.New("CNB OCI blob digest does not match its contents")
		}
	}
	var layout struct {
		Version string `json:"imageLayoutVersion"`
	}
	data, err := os.ReadFile(filepath.Join(directory, "oci-layout"))
	if err != nil || json.Unmarshal(data, &layout) != nil || layout.Version != "1.0.0" || !seen["index.json"] {
		return errors.New("CNB requires OCI layout 1.0.0 with an image index; Docker legacy archives are unavailable")
	}
	return nil
}

func readCNBBlob(directory string, descriptor cnbDescriptor) ([]byte, error) {
	path, err := cnbBlobPath(directory, descriptor.Digest)
	if err != nil || len(descriptor.URLs) != 0 || descriptor.Size <= 0 || descriptor.Size > cnbMetadataLimit {
		return nil, errors.New("CNB metadata descriptor is invalid or exceeds 2 MiB")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, cnbMetadataLimit+1))
	if err != nil || int64(len(data)) != descriptor.Size || cnbDigest(data) != descriptor.Digest {
		return nil, errors.New("CNB metadata digest or size does not match")
	}
	return data, nil
}

func readCNBImage(directory, architecture, pinnedIndex string) (cnbImage, error) {
	var result cnbImage
	data, err := os.ReadFile(filepath.Join(directory, "index.json"))
	if err != nil || len(data) > int(cnbMetadataLimit) {
		return result, errors.New("CNB image index is unavailable or exceeds its bound")
	}
	var index cnbIndex
	if json.Unmarshal(data, &index) != nil || index.SchemaVersion != 2 || len(index.Manifests) != 1 {
		return result, errors.New("CNB OCI layout must contain exactly one image reference")
	}
	descriptor := index.Manifests[0]
	if pinnedIndex != "" && descriptor.Digest != pinnedIndex {
		return result, errors.New("CNB run image does not match Cove's release pin")
	}
	for depth := 0; depth < 3; depth++ {
		data, err = readCNBBlob(directory, descriptor)
		if err != nil {
			return result, err
		}
		if descriptor.MediaType != "application/vnd.oci.image.index.v1+json" && descriptor.MediaType != "application/vnd.docker.distribution.manifest.list.v2+json" {
			break
		}
		if json.Unmarshal(data, &index) != nil || index.SchemaVersion != 2 || len(index.Manifests) > 16 {
			return result, errors.New("CNB run image platform index is invalid")
		}
		matches := []cnbDescriptor{}
		for _, candidate := range index.Manifests {
			if candidate.Platform != nil && candidate.Platform.OS == "linux" && candidate.Platform.Architecture == architecture {
				matches = append(matches, candidate)
			}
		}
		if len(matches) != 1 {
			return result, errors.New("CNB pinned image does not provide exactly one matching Linux platform")
		}
		descriptor = matches[0]
	}
	if descriptor.MediaType != "application/vnd.oci.image.manifest.v1+json" && descriptor.MediaType != "application/vnd.docker.distribution.manifest.v2+json" {
		return result, errors.New("CNB image manifest media type is unsupported")
	}
	if json.Unmarshal(data, &result.Manifest) != nil || result.Manifest.SchemaVersion != 2 || len(result.Manifest.Layers) == 0 || len(result.Manifest.Layers) > 256 {
		return result, errors.New("CNB image manifest is invalid or exceeds its layer bound")
	}
	result.Descriptor = descriptor
	result.Config, err = readCNBBlob(directory, result.Manifest.Config)
	if err != nil {
		return result, err
	}
	var config struct {
		OS           string `json:"os"`
		Architecture string `json:"architecture"`
		RootFS       struct {
			Type    string   `json:"type"`
			DiffIDs []string `json:"diff_ids"`
		} `json:"rootfs"`
	}
	if json.Unmarshal(result.Config, &config) != nil || config.OS != "linux" || config.Architecture != architecture || config.RootFS.Type != "layers" || len(config.RootFS.DiffIDs) != len(result.Manifest.Layers) {
		return result, errors.New("CNB image configuration platform or root filesystem is invalid")
	}
	result.DiffIDs, result.Arch = config.RootFS.DiffIDs, architecture
	return result, nil
}

func verifyCNBOutput(ctx context.Context, directory string, image, run cnbImage) error {
	if len(image.DiffIDs) < len(run.DiffIDs) || image.Arch != run.Arch {
		return errors.New("CNB export replaced the pinned run image")
	}
	for i, digest := range run.DiffIDs {
		if image.DiffIDs[i] != digest {
			return errors.New("CNB export replaced a pinned run image layer")
		}
	}
	var expanded int64
	for i, descriptor := range image.Manifest.Layers {
		path, err := cnbBlobPath(directory, descriptor.Digest)
		if err != nil || len(descriptor.URLs) != 0 || descriptor.Size <= 0 || descriptor.Size > cnbArchiveLimit {
			return errors.New("CNB export contains an unsupported layer descriptor")
		}
		file, err := os.Open(path)
		if err != nil {
			return err
		}
		stat, statErr := file.Stat()
		if statErr != nil || stat.Size() != descriptor.Size {
			file.Close()
			return errors.New("CNB layer size does not match its descriptor")
		}
		var reader io.Reader = scanContextReader{ctx, file}
		var compressed *gzip.Reader
		switch descriptor.MediaType {
		case "application/vnd.oci.image.layer.v1.tar+gzip", "application/vnd.docker.image.rootfs.diff.tar.gzip":
			compressed, err = gzip.NewReader(reader)
			if err == nil {
				reader = compressed
			}
		case "application/vnd.oci.image.layer.v1.tar", "application/vnd.docker.image.rootfs.diff.tar":
		default:
			err = errors.New("CNB layer compression is unsupported")
		}
		hash := sha256.New()
		var written int64
		if err == nil {
			written, err = io.Copy(hash, io.LimitReader(reader, cnbExpandedLimit-expanded+1))
		}
		if compressed != nil {
			compressed.Close()
		}
		file.Close()
		expanded += written
		if err != nil || expanded > cnbExpandedLimit || "sha256:"+hex.EncodeToString(hash.Sum(nil)) != image.DiffIDs[i] {
			return errors.New("CNB layer expansion exceeds 4 GiB or does not match the configuration digest")
		}
	}
	return nil
}

func writeCNBImport(directory, destination, tag, owner string, image cnbImage) error {
	var config map[string]any
	if err := json.Unmarshal(image.Config, &config); err != nil {
		return err
	}
	settings, ok := config["config"].(map[string]any)
	if !ok {
		return errors.New("CNB exported runtime configuration is invalid")
	}
	labels := map[string]any{}
	if existing := settings["Labels"]; existing != nil {
		labels, ok = existing.(map[string]any)
		if !ok {
			return errors.New("CNB exported labels are invalid")
		}
	}
	labels["dev.cove.build-owner"] = owner
	settings["Labels"] = labels
	configuration, err := json.Marshal(config)
	if err != nil {
		return err
	}
	image.Manifest.Config.Digest, image.Manifest.Config.Size = cnbDigest(configuration), int64(len(configuration))
	manifest, err := json.Marshal(image.Manifest)
	if err != nil {
		return err
	}
	index, err := json.Marshal(cnbIndex{SchemaVersion: 2, MediaType: "application/vnd.oci.image.index.v1+json", Manifests: []cnbDescriptor{{MediaType: image.Manifest.MediaType, Digest: cnbDigest(manifest), Size: int64(len(manifest)), Annotations: map[string]string{"io.containerd.image.name": tag, "org.opencontainers.image.ref.name": tag}}}})
	if err != nil {
		return err
	}
	output, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer output.Close()
	packed := tar.NewWriter(output)
	for _, entry := range []struct {
		name string
		data []byte
	}{{"oci-layout", []byte(`{"imageLayoutVersion":"1.0.0"}`)}, {"index.json", index}, {"blobs/sha256/" + cnbDigest(configuration)[7:], configuration}, {"blobs/sha256/" + cnbDigest(manifest)[7:], manifest}} {
		if err := packed.WriteHeader(&tar.Header{Name: entry.name, Typeflag: tar.TypeReg, Mode: 0600, Size: int64(len(entry.data))}); err != nil {
			return err
		}
		if _, err := packed.Write(entry.data); err != nil {
			return err
		}
	}
	seen := map[string]bool{}
	for _, descriptor := range image.Manifest.Layers {
		if seen[descriptor.Digest] {
			continue
		}
		seen[descriptor.Digest] = true
		path, err := cnbBlobPath(directory, descriptor.Digest)
		if err != nil {
			return err
		}
		if err := packed.WriteHeader(&tar.Header{Name: "blobs/sha256/" + descriptor.Digest[7:], Typeflag: tar.TypeReg, Mode: 0600, Size: descriptor.Size}); err != nil {
			return err
		}
		input, err := os.Open(path)
		if err != nil {
			return err
		}
		written, copyErr := io.Copy(packed, input)
		input.Close()
		if copyErr != nil || written != descriptor.Size {
			return fmt.Errorf("CNB import layer copy failed: %w", errors.Join(copyErr, io.ErrUnexpectedEOF))
		}
	}
	if err := packed.Close(); err != nil {
		return err
	}
	return output.Sync()
}

func cnbConfigContainsSecret(configuration []byte, values []string) bool {
	for _, value := range values {
		encoded, _ := json.Marshal(value)
		if value != "" && (bytes.Contains(configuration, []byte(value)) || len(encoded) > 2 && bytes.Contains(configuration, encoded[1:len(encoded)-1])) {
			return true
		}
	}
	return false
}

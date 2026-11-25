package worker

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func cnbFixtureImage(t *testing.T, directory string, layers ...string) cnbImage {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(directory, "blobs", "sha256"), 0700); err != nil {
		t.Fatal(err)
	}
	write := func(data []byte) cnbDescriptor {
		descriptor := cnbDescriptor{Digest: cnbDigest(data), Size: int64(len(data))}
		path, err := cnbBlobPath(directory, descriptor.Digest)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
		return descriptor
	}
	manifest := cnbManifest{SchemaVersion: 2, MediaType: "application/vnd.oci.image.manifest.v1+json"}
	var diffIDs []string
	for _, content := range layers {
		var compressed bytes.Buffer
		writer := gzip.NewWriter(&compressed)
		if _, err := writer.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
		if err := writer.Close(); err != nil {
			t.Fatal(err)
		}
		descriptor := write(compressed.Bytes())
		descriptor.MediaType = "application/vnd.oci.image.layer.v1.tar+gzip"
		manifest.Layers = append(manifest.Layers, descriptor)
		diffIDs = append(diffIDs, cnbDigest([]byte(content)))
	}
	configuration, _ := json.Marshal(map[string]any{"os": "linux", "architecture": "amd64", "rootfs": map[string]any{"type": "layers", "diff_ids": diffIDs}, "config": map[string]any{"User": "1002:1001", "Env": []string{"PATH=/usr/bin"}, "Labels": map[string]string{"io.buildpacks.test": "fixture"}}})
	manifest.Config = write(configuration)
	manifest.Config.MediaType = "application/vnd.oci.image.config.v1+json"
	encoded, _ := json.Marshal(manifest)
	descriptor := write(encoded)
	descriptor.MediaType = manifest.MediaType
	index, _ := json.Marshal(cnbIndex{SchemaVersion: 2, MediaType: "application/vnd.oci.image.index.v1+json", Manifests: []cnbDescriptor{descriptor}})
	if err := os.WriteFile(filepath.Join(directory, "index.json"), index, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "oci-layout"), []byte(`{"imageLayoutVersion":"1.0.0"}`), 0600); err != nil {
		t.Fatal(err)
	}
	image, err := readCNBImage(directory, "amd64", "")
	if err != nil {
		t.Fatal(err)
	}
	return image
}

func TestBuildpacksOCIExportVerificationAndOwnImportTag(t *testing.T) {
	directory := t.TempDir()
	run := cnbFixtureImage(t, t.TempDir(), "pinned-base")
	image := cnbFixtureImage(t, directory, "pinned-base", "fixture application")
	if err := verifyCNBOutput(context.Background(), directory, image, run); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(t.TempDir(), "image.tar")
	tag := "registry.example.test/team/app:cove-fixture"
	if err := writeCNBImport(directory, archive, tag, "fixture-owner", image); err != nil {
		t.Fatal(err)
	}
	imported := t.TempDir()
	if err := unpackCNBArchive(context.Background(), archive, imported, false); err != nil {
		t.Fatal(err)
	}
	verified, err := readCNBImage(imported, "amd64", "")
	if err != nil || len(verified.Manifest.Layers) != 2 || !bytes.Contains(verified.Config, []byte(`"dev.cove.build-owner":"fixture-owner"`)) {
		t.Fatal("import identity or cleanup ownership lost", err)
	}
	data, err := os.ReadFile(filepath.Join(imported, "index.json"))
	if err != nil || !bytes.Contains(data, []byte(tag)) || bytes.Contains(data, []byte("foreign")) {
		t.Fatal("import did not use the owned tag", err)
	}
	if err := verifyCNBOutput(context.Background(), imported, verified, run); err != nil {
		t.Fatal(err)
	}
}

func TestBuildpacksOCIRefusesChangedBasePlatformAndLayers(t *testing.T) {
	run := cnbFixtureImage(t, t.TempDir(), "pinned-base")
	for _, variant := range []string{"base", "architecture", "remote-layer", "compression", "diff-id", "size", "pin"} {
		t.Run(variant, func(t *testing.T) {
			directory := t.TempDir()
			image := cnbFixtureImage(t, directory, "pinned-base", "application")
			switch variant {
			case "base":
				image.DiffIDs[0] = cnbDigest([]byte("foreign-base"))
			case "architecture":
				image.Arch = "arm64"
			case "remote-layer":
				image.Manifest.Layers[1].URLs = []string{"https://foreign.example.test/private"}
			case "compression":
				image.Manifest.Layers[1].MediaType = "application/vnd.oci.image.layer.v1.tar+zstd"
			case "diff-id":
				image.DiffIDs[1] = cnbDigest([]byte("changed"))
			case "size":
				image.Manifest.Layers[1].Size++
			case "pin":
				if _, err := readCNBImage(directory, "amd64", "sha256:"+strings.Repeat("f", 64)); err == nil {
					t.Fatal("foreign run pin accepted")
				}
				return
			}
			if err := verifyCNBOutput(context.Background(), directory, image, run); err == nil {
				t.Fatal("untrusted export accepted", variant)
			}
		})
	}
}

func TestBuildpacksOCIArchiveRefusesUnsafeEntriesAndDigests(t *testing.T) {
	for _, variant := range []string{"traversal", "symlink", "duplicate", "digest", "oversized"} {
		t.Run(variant, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "output.tar")
			file, err := os.Create(path)
			if err != nil {
				t.Fatal(err)
			}
			packed := tar.NewWriter(file)
			header := &tar.Header{Name: "index.json", Typeflag: tar.TypeReg, Mode: 0600, Size: 2}
			switch variant {
			case "traversal":
				header.Name = "../private"
			case "symlink":
				header.Typeflag, header.Size, header.Linkname = tar.TypeSymlink, 0, "/private"
			case "digest":
				header.Name = "blobs/sha256/" + strings.Repeat("a", 64)
			case "oversized":
				header.Size = cnbArchiveLimit + 1
			}
			if err := packed.WriteHeader(header); err != nil {
				t.Fatal(err)
			}
			if header.Size == 2 {
				packed.Write([]byte("{}"))
			}
			if variant == "duplicate" {
				packed.WriteHeader(header)
				packed.Write([]byte("{}"))
			}
			packed.Close()
			file.Close()
			if err := unpackCNBArchive(context.Background(), path, t.TempDir(), false); err == nil {
				t.Fatal("unsafe archive accepted", variant)
			}
		})
	}
}

func TestBuildpacksResourceAndNetworkAdmission(t *testing.T) {
	capacity := BuildpacksCapacity{MemoryAvailable: 4 << 30, DiskAvailable: 24 << 30, StateAvailable: 10 << 30}
	if err := ValidateBuildpacksCapacity(capacity); err != nil {
		t.Fatal(err)
	}
	for _, candidate := range []BuildpacksCapacity{{(4 << 30) - 1, 24 << 30, 10 << 30}, {4 << 30, (24 << 30) - 1, 10 << 30}, {4 << 30, 24 << 30, (10 << 30) - 1}, {}} {
		if ValidateBuildpacksCapacity(candidate) == nil {
			t.Fatal("insufficient/unknown capacity accepted", candidate)
		}
	}
	for _, build := range []map[string]any{{}, {"network": "none"}, {"network": "default", "args": map[string]any{"HTTPS_PROXY": "https://proxy.example.test:8443"}}} {
		if err := validateBuildpacksPolicy(build); err != nil {
			t.Fatal(err)
		}
	}
	for _, build := range []map[string]any{{"network": "host"}, {"network": "control"}, {"args": map[string]any{"HTTPS_PROXY": nil}}, {"args": []any{"HTTPS_PROXY"}}, {"args": map[string]any{"TOKEN": "private"}}} {
		if validateBuildpacksPolicy(build) == nil {
			t.Fatal("unsupported build policy accepted", build)
		}
	}
	if _, err := buildpacksArchitecture("ppc64le"); err == nil {
		t.Fatal("unsupported pinned platform accepted")
	}
	if !cnbConfigContainsSecret([]byte(`{"Env":["TOKEN=fixture-private"]}`), []string{"fixture-private"}) || cnbConfigContainsSecret([]byte(`{"Env":["PATH=/usr/bin"]}`), []string{"fixture-private"}) {
		t.Fatal("exported configuration secret check failed")
	}
}

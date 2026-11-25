package worker

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestImageScanArchiveIdentityAndJavaCoverage(t *testing.T) {
	for _, compressed := range []bool{false, true} {
		var layer bytes.Buffer
		tw := tar.NewWriter(&layer)
		if err := tw.WriteHeader(&tar.Header{Name: "app/WEB-INF/example.WAR", Typeflag: tar.TypeReg, Mode: 0644}); err != nil {
			t.Fatal(err)
		}
		tw.Close()
		payload := layer.Bytes()
		if compressed {
			var buffer bytes.Buffer
			writer := gzip.NewWriter(&buffer)
			writer.Write(payload)
			writer.Close()
			payload = buffer.Bytes()
		}
		config := []byte(`{"architecture":"amd64"}`)
		configDigest := fmt.Sprintf("sha256:%x", sha256.Sum256(config))
		platform := []byte(`{"config":{"digest":"` + configDigest + `"}}`)
		platformDigest := fmt.Sprintf("sha256:%x", sha256.Sum256(platform))
		var archive bytes.Buffer
		writer := tar.NewWriter(&archive)
		for _, entry := range []struct {
			name string
			data []byte
		}{
			{"config.json", config},
			{"platform.json", platform},
			{"layer.tar", payload},
			{"manifest.json", []byte(`[{"Config":"config.json","Layers":["layer.tar"]}]`)},
		} {
			writer.WriteHeader(&tar.Header{Name: entry.name, Typeflag: tar.TypeReg, Mode: 0644, Size: int64(len(entry.data))})
			writer.Write(entry.data)
		}
		writer.Close()
		file := filepath.Join(t.TempDir(), "image.tar")
		if err := os.WriteFile(file, archive.Bytes(), 0600); err != nil {
			t.Fatal(err)
		}
		result, err := inspectScanArchive(context.Background(), file)
		if err != nil || !result.JavaArchives || !imageDigest.MatchString(result.ConfigDigest) || result.Manifests[platformDigest] != result.ConfigDigest {
			t.Fatal(result, err)
		}
		image := ScannedImage{State: "complete", Counts: map[string]int{"HIGH": 3}, Coverage: ScanCoverage{OS: "scanned", OtherLanguages: "scanned"}}
		applyScanArchiveCoverage(&image, result)
		if image.State != "unknown" || image.Coverage.JavaArchives != "not_scanned" || image.Coverage.OS != "scanned" || image.Counts["HIGH"] != 3 || image.Coverage.JavaMessage == "" {
			t.Fatal(image)
		}
	}
}

func TestImageScanRealTrivyReport(t *testing.T) {
	file := os.Getenv("COVE_TEST_TRIVY_REPORT")
	if file == "" {
		t.Skip("requires the confined official-scanner fixture report")
	}
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	config := "sha256:26b2eb03618e749084668eaff68cff8f81dda12d06ac641be7a6398b82a6f25b"
	result, err := parseImageScan(data, "fixture", "sha256:a3d8aaa63ed8681a604f1dea0aa03f100d5895b6a58ace528858a7b332415373", config)
	if err != nil || result.State != "complete" || len(result.Findings) == 0 || result.Coverage.OS != "scanned" || result.Coverage.OtherLanguages != "scanned" || !result.EndOfLife {
		t.Fatal(result, err)
	}
	encoded, _ := json.Marshal(result)
	if len(encoded) > scanOutputLimit {
		t.Fatal("normalized result exceeds its limit")
	}
}

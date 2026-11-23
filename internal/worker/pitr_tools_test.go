package worker

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"sync"
	"testing"
)

func TestPITRToolExtractionRejectsUntrustedArchives(t *testing.T) {
	for _, kind := range []string{"valid", "traversal", "symlink", "extra", "oversized"} {
		t.Run(kind, func(t *testing.T) {
			var data bytes.Buffer
			compressed := gzip.NewWriter(&data)
			archive := tar.NewWriter(compressed)
			entry := &tar.Header{Name: "wal-g-fixture", Mode: 0700, Size: 4, Typeflag: tar.TypeReg}
			switch kind {
			case "traversal":
				entry.Name = "../wal-g-fixture"
			case "symlink":
				entry.Typeflag = tar.TypeSymlink
				entry.Linkname = "/unrelated"
				entry.Size = 0
			case "oversized":
				entry.Size = pitrToolLimit + 1
			}
			if err := archive.WriteHeader(entry); err != nil {
				t.Fatal(err)
			}
			if entry.Typeflag == tar.TypeReg && kind != "oversized" {
				if _, err := archive.Write([]byte("tool")); err != nil {
					t.Fatal(err)
				}
			}
			if kind == "extra" {
				if err := archive.WriteHeader(&tar.Header{Name: "extra", Size: 0, Typeflag: tar.TypeReg}); err != nil {
					t.Fatal(err)
				}
			}
			closeErr := archive.Close()
			if closeErr != nil && kind != "oversized" {
				t.Fatal(closeErr)
			}
			if err := compressed.Close(); err != nil {
				t.Fatal(err)
			}
			var output bytes.Buffer
			err := extractPITRTool(bytes.NewReader(data.Bytes()), &output, "wal-g-fixture")
			if (err == nil) != (kind == "valid") {
				t.Fatal("archive validation", kind, err)
			}
			if kind == "valid" && output.String() != "tool" {
				t.Fatal("incorrect executable")
			}
		})
	}
}

func TestBackupHelperOwnerIsStableUnderConcurrentCreation(t *testing.T) {
	e, err := NewEngine(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var jobs sync.WaitGroup
	owners := make(chan string, 32)
	for range 32 {
		jobs.Go(func() {
			owner, err := e.backupHelperOwner(true)
			if err != nil {
				t.Error(err)
				return
			}
			owners <- owner
		})
	}
	jobs.Wait()
	close(owners)
	first := ""
	for owner := range owners {
		if first == "" {
			first = owner
		}
		if owner != first {
			t.Fatal("concurrent helper ownership changed")
		}
	}
	if first == "" {
		t.Fatal("no helper identity created")
	}
}

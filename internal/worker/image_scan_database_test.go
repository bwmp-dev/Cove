package worker

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"
)

func TestImageScanDatabaseRefreshUsesOnlyBoundedMainDatabase(t *testing.T) {
	e, err := NewEngine(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	started, cleaned := false, false
	e.Run = func(_ context.Context, _ string, args, _ []string, _ io.Writer) error {
		switch args[1] {
		case "run":
			started = true
			joined := strings.Join(args, " ")
			for _, value := range []string{"--read-only", "--cap-drop ALL", "--user 65532:65532", "--memory 3g", "--cpus 2", "TMPDIR=/cache", "/cache:size=2g", "ghcr.io/aquasecurity/trivy-db:2 --download-db-only", "--skip-version-check", "--disable-telemetry"} {
				if !strings.Contains(joined, value) {
					t.Fatal("unbounded database refresh", value)
				}
			}
			if strings.Contains(joined, "java") || strings.Contains(joined, "docker.sock") || strings.Contains(joined, "--mount") {
				t.Fatal("database refresh has Java, host state or socket access")
			}
		case "exec":
		case "ps":
			cleaned = true
		default:
			t.Fatal(args)
		}
		return nil
	}
	now := time.Now().UTC()
	var archive bytes.Buffer
	writer := tar.NewWriter(&archive)
	for name, data := range map[string][]byte{"db/trivy.db": []byte("main-database"), "db/metadata.json": scanTestMetadata(t, 2, now.Add(-time.Hour), now.Add(time.Hour))} {
		if err = writer.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Size: int64(len(data)), Mode: 0600}); err != nil {
			t.Fatal(err)
		}
		writer.Write(data)
	}
	writer.Close()
	e.Stream = func(_ context.Context, args []string, out, _ io.Writer) error {
		if strings.Join(args, " ") != "docker exec cove-scan-db-owner-op tar -C /cache -cf - db" {
			t.Fatal("unexpected database export", args)
		}
		_, err := io.Copy(out, bytes.NewReader(archive.Bytes()))
		return err
	}
	database, err := e.refreshScanDatabase(context.Background(), "owner", "op", t.TempDir(), ScannerImage)
	if err != nil || !database.Fresh(now) || !started || !cleaned {
		t.Fatal(database, err, started, cleaned)
	}
}

func TestImageScanDatabaseArchiveValidation(t *testing.T) {
	now := time.Now().UTC()
	archive := func(extra *tar.Header, stale bool) []byte {
		var buffer bytes.Buffer
		writer := tar.NewWriter(&buffer)
		updated := now.Add(-time.Hour)
		if stale {
			updated = now.Add(-48 * time.Hour)
		}
		for _, entry := range []struct {
			name string
			data []byte
		}{
			{"db/trivy.db", []byte("database")},
			{"db/metadata.json", scanTestMetadata(t, 2, updated, now.Add(time.Hour))},
		} {
			if err := writer.WriteHeader(&tar.Header{Name: entry.name, Typeflag: tar.TypeReg, Mode: 0600, Size: int64(len(entry.data))}); err != nil {
				t.Fatal(err)
			}
			if _, err := writer.Write(entry.data); err != nil {
				t.Fatal(err)
			}
		}
		if extra != nil {
			if err := writer.WriteHeader(extra); err != nil {
				t.Fatal(err)
			}
		}
		writer.Close()
		return buffer.Bytes()
	}
	database, err := extractScanDatabase(bytes.NewReader(archive(nil, false)), t.TempDir())
	if err != nil || !database.Fresh(now) {
		t.Fatal(database, err)
	}
	for name, input := range map[string][]byte{
		"stale":     archive(nil, true),
		"escape":    archive(&tar.Header{Name: "../foreign", Typeflag: tar.TypeReg, Size: 1}, false),
		"symlink":   archive(&tar.Header{Name: "other", Typeflag: tar.TypeSymlink, Linkname: "/foreign"}, false),
		"duplicate": archive(&tar.Header{Name: "db/trivy.db", Typeflag: tar.TypeReg, Size: 1}, false),
		"oversize":  archive(&tar.Header{Name: "other", Typeflag: tar.TypeReg, Size: scanDatabaseLimit + 1}, false),
		"empty":     {},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := extractScanDatabase(bytes.NewReader(input), t.TempDir()); err == nil {
				t.Fatal("invalid database archive accepted")
			}
		})
	}
}

func scanTestMetadata(t *testing.T, version int, updated, next time.Time) []byte {
	t.Helper()
	data, err := json.Marshal(map[string]any{"Version": version, "UpdatedAt": updated, "NextUpdate": next})
	if err != nil {
		t.Fatal(err)
	}
	return data
}

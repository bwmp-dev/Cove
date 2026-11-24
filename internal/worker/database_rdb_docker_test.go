package worker

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func legacyRDBBackupForTest(t *testing.T, ctx context.Context, e *Engine, request Request, container string) string {
	t.Helper()
	var raw bytes.Buffer
	if err := runInput(ctx, e.Root, []string{"docker", "exec", container, "sh", "-ec", extendedDumpCommand(request.Backup.Database.Engine)}, nil, nil, &sizeBoundWriter{Writer: &raw, remaining: 8 << 20}); err != nil {
		t.Fatal(err)
	}
	data := raw.Bytes()
	if len(data) < 58 || !hasRDBTransportMarker(data[:9], data[len(data)-49:]) {
		t.Fatal("legacy fixture requires a real diskless-framed stdout dump")
	}
	original := request
	original.BackupID = request.ID
	manifest, err := e.loadBackupManifest(ctx, original, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	legacy := request
	legacy.ID = "legacy"
	dataRequest := legacy
	dataRequest.ID += "-data"
	// Seed the encrypted stdout format written before normalization; production capture must not use this path.
	if err := e.resticInput(ctx, dataRequest, nil, false, bytes.NewReader(data), io.Discard, "backup", "--stdin", "--stdin-filename", "/data/000/database.dump", "--tag", "cove-workload:"+request.Workload, "--tag", "cove-operation:"+dataRequest.ID); err != nil {
		t.Fatal(err)
	}
	var found bytes.Buffer
	if err := e.restic(ctx, dataRequest, nil, false, &found, "snapshots", "--json", "--tag", "cove-operation:"+dataRequest.ID); err != nil {
		t.Fatal(err)
	}
	var snapshots []struct{ ID string }
	if err := json.Unmarshal(found.Bytes(), &snapshots); err != nil || len(snapshots) != 1 {
		t.Fatal("legacy data snapshot unavailable", err)
	}
	storedManifest, err := e.makeBackupWithData(ctx, legacy, manifest.Mounts, manifest.State, io.Discard, snapshots[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	storedManifest.Record.VerifiedAt, storedManifest.Record.Verification = manifest.Record.VerifiedAt, manifest.Record.Verification
	if err := writeJSON(filepath.Join(e.Root, "workloads", request.Workload, "backups", legacy.ID+".json"), storedManifest); err != nil {
		t.Fatal(err)
	}
	download := legacy
	download.ID, download.BackupID = "legacy-download", legacy.ID
	want := data[:len(data)-rdbTransportMarkerBytes]
	file, meta, cleanup, err := e.backupDownload(ctx, download, int64(len(want)))
	if err != nil {
		t.Fatal(err)
	}
	got, readErr := io.ReadAll(file)
	closeErr, cleanupErr := file.Close(), cleanup()
	digest := sha256.Sum256(want)
	if readErr != nil || closeErr != nil || cleanupErr != nil || !bytes.Equal(got, want) || meta.Size != int64(len(want)) || meta.SHA256 != hex.EncodeToString(digest[:]) {
		t.Fatal("legacy download changed the RDB or its transfer metadata", readErr, closeErr, cleanupErr)
	}
	var stored bytes.Buffer
	if err := e.resticInput(ctx, legacy, nil, false, strings.NewReader(""), &stored, "dump", snapshots[0].ID, "/data/000/database.dump"); err != nil || !bytes.Equal(stored.Bytes(), data) {
		t.Fatal("normalization mutated the encrypted legacy snapshot", err)
	}
	return legacy.ID
}

func testRDBFramedImport(t *testing.T, f databaseImportFixture) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Minute)
	defer cancel()
	framed := append(bytes.Clone(f.dump), bytes.Repeat([]byte("a"), rdbTransportMarkerBytes)...)
	r := f.request
	r.ID = "legacy-import"
	f.set("after")
	meta, err := f.engine.receiveDatabaseImport(ctx, r, StreamRequest{Action: "backup_upload", Limit: 8 << 20, Size: int64(len(framed))}, bytes.NewReader(framed))
	rawDigest := sha256.Sum256(framed)
	if err != nil || meta.Size != int64(len(framed)) || meta.SHA256 != hex.EncodeToString(rawDigest[:]) || f.value() != "after" {
		t.Fatal("legacy import receipt or source data changed", err)
	}
	r.ID, r.BackupID = "legacy-import-download", "legacy-import"
	file, downloaded, cleanup, err := f.engine.backupDownload(ctx, r, int64(len(f.dump)))
	if err != nil {
		t.Fatal(err)
	}
	data, readErr := io.ReadAll(file)
	closeErr, cleanupErr := file.Close(), cleanup()
	digest := sha256.Sum256(f.dump)
	if readErr != nil || closeErr != nil || cleanupErr != nil || !bytes.Equal(data, f.dump) || downloaded.Size != int64(len(f.dump)) || downloaded.SHA256 != hex.EncodeToString(digest[:]) {
		t.Fatal("imported framed RDB was not stored/exported as loadable data", readErr, closeErr, cleanupErr)
	}
	r.ID, r.Action = "legacy-import-restore", "restore"
	if err := f.engine.performBackup(ctx, r, io.Discard); err != nil || f.value() != "before" {
		t.Fatal("legacy imported RDB failed restore", err)
	}
}

func TestRDBDumpFramingDocker(t *testing.T) {
	fixture := os.Getenv("COVE_DATABASE_FIXTURE")
	if fixture == "" {
		t.Skip("disposable Docker database fixture required")
	}
	for index, sample := range rdbStreamFixtures(t) {
		t.Run(sample.Image, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
			t.Cleanup(cancel)
			root := filepath.Join(fixture, fmt.Sprintf("rdb-%d", index))
			if err := os.MkdirAll(root, 0700); err != nil {
				t.Fatal(err)
			}
			e := &Engine{Root: root, Run: RunCommand}
			r := Request{ID: "verify", Workload: fmt.Sprintf("rdb-%d-%s", index, filepath.Base(fixture)), Backup: &BackupConfig{Database: &DatabaseBackup{Engine: sample.Engine}}}
			source := "cove-" + r.Workload
			if _, err := e.output(ctx, "docker", "image", "inspect", sample.Image); err != nil {
				if err := e.Run(ctx, root, []string{"docker", "pull", sample.Image}, nil, io.Discard); err != nil {
					t.Fatal("stock image unavailable", err)
				}
			}
			if err := e.Run(ctx, root, []string{"docker", "create", "--name", source, "--label", "cove.rdb-fixture=" + r.Workload, "--network", "none", sample.Image}, nil, io.Discard); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				cleanup, stop := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
				defer stop()
				owner, err := e.output(cleanup, "docker", "inspect", "--format", `{{index .Config.Labels "cove.rdb-fixture"}}`, source)
				if err != nil || owner != r.Workload {
					t.Error("stock fixture ownership changed", err)
					return
				}
				if err := e.Run(cleanup, root, []string{"docker", "rm", "-f", "-v", source}, nil, io.Discard); err != nil {
					t.Error(err)
				}
			})
			image, err := e.output(ctx, "docker", "inspect", "--format", "{{.Image}}", source)
			if err != nil {
				t.Fatal(err)
			}
			t.Log("stock image", sample.Image, image, "fixture version", sample.Version)
			for _, input := range [][]byte{sample.Stdout, sample.File} {
				dump := filepath.Join(root, "database.dump")
				if err := os.WriteFile(dump, input, 0600); err != nil {
					t.Fatal(err)
				}
				cleanup, err := e.verifyExtendedDatabase(ctx, r, source, dump, false)
				if err != nil {
					t.Fatal("stock RDB failed unchanged isolated verification", err)
				}
				marker, markerErr := e.output(ctx, "docker", "exec", databaseHelperName(r), redisCLI(sample.Engine), "--raw", "GET", "marker")
				cleanupErr := cleanup()
				if markerErr != nil || cleanupErr != nil || strings.TrimSpace(marker) != "synthetic-before" {
					t.Fatal("stock RDB lost its marker", markerErr, cleanupErr)
				}
				got, err := os.ReadFile(dump)
				want := input
				if bytes.Equal(input, sample.Stdout) {
					want = input[:len(input)-rdbTransportMarkerBytes]
				}
				if err != nil || !bytes.Equal(got, want) {
					t.Fatal("RDB payload or checksum changed", err)
				}
				corrupt := bytes.Clone(input)
				checksum := len(corrupt) - 1
				if bytes.Equal(input, sample.Stdout) {
					checksum -= rdbTransportMarkerBytes
				}
				corrupt[checksum] ^= 1
				if err := os.WriteFile(dump, corrupt, 0600); err != nil {
					t.Fatal(err)
				}
				if cleanup, err := e.verifyExtendedDatabase(ctx, r, source, dump, false); err == nil {
					cleanup()
					t.Fatal("checksum corruption bypassed isolated verification")
				}
			}
		})
	}
}

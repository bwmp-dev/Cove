package worker

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMongoCopyHelperCanReadPrivateWorkerSnapshotWithoutCapabilities(t *testing.T) {
	e, err := NewEngine(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	called := false
	e.Run = func(_ context.Context, _ string, args, env []string, out io.Writer) error {
		if args[1] != "run" {
			return nil
		}
		called = true
		joined := strings.Join(args, " ")
		for _, required := range []string{"--user " + fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()), "--cap-drop ALL", "target=/run/cove-copy.dump,readonly"} {
			if !strings.Contains(joined, required) {
				t.Fatal("snapshot helper ownership or isolation missing", required)
			}
		}
		return nil
	}
	in := maintenanceFixture()
	if err = e.restoreMongoCopy(t.Context(), in, "target", filepath.Join(t.TempDir(), "private.dump")); err != nil || !called {
		t.Fatal("matching-version restore helper not run", err)
	}
}

func TestDatabaseSafetySnapshotRejectsCorruptionAndRequiresChecksum(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "copy.dump")
	if err := os.WriteFile(path, []byte("verified backup"), 0600); err != nil {
		t.Fatal(err)
	}
	if verifyDatabaseSnapshot(path) == nil {
		t.Fatal("unchecked snapshot accepted")
	}
	if err := sealDatabaseSnapshot(path, path+".sha256"); err != nil {
		t.Fatal(err)
	}
	if err := verifyDatabaseSnapshot(path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("damaged backup"), 0600); err != nil {
		t.Fatal(err)
	}
	if verifyDatabaseSnapshot(path) == nil {
		t.Fatal("corrupted snapshot accepted")
	}
	if err := os.WriteFile(path+".partial", []byte("partial backup"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := removeDatabaseDump(dir, "copy"); err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{"", ".sha256", ".partial"} {
		if _, err := os.Stat(path + suffix); !os.IsNotExist(err) {
			t.Fatal("private snapshot remains", suffix, err)
		}
	}
}

func TestDatabaseExtensionRemovalNeverCascadesAndChecksDependencies(t *testing.T) {
	for _, name := range []string{"pg_trgm", "pg_stat_statements", "vector", "postgis"} {
		statement := databaseExtensionStatement(name, true)
		if strings.Contains(statement, "CASCADE") || !strings.Contains(statement, "RESTRICT") {
			t.Fatal("unsafe extension removal", name)
		}
	}
	if !strings.Contains(databaseExtensionStatement("postgis", true), "postgis_topology RESTRICT") {
		t.Fatal("PostGIS companion dependency not handled")
	}
	for _, engine := range []string{"postgresql", "mysql", "mariadb", "mongodb"} {
		command, err := databaseCopyDumpCommand(engine)
		if err != nil || !strings.Contains(command, "app") {
			t.Fatal("missing scoped snapshot", engine, err)
		}
	}
}

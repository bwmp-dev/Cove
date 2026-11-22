package control

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestPanelBackupRestoreIntegration(t *testing.T) {
	database, container := os.Getenv("COVE_TEST_DATABASE_URL"), os.Getenv("COVE_TEST_DATABASE_CONTAINER")
	if database == "" || container == "" {
		t.Skip("disposable database container required")
	}
	ctx := context.Background()
	db := openTestDatabase(t, database)
	s := &Server{DB: db, Key: bytes.Repeat([]byte{6}, 32), AdminToken: strings.Repeat("d", 32), PublicURL: "https://cove.example.com"}
	if err := s.Init(ctx); err != nil {
		t.Fatal(err)
	}
	var schema string
	if err := db.QueryRow(ctx, "SELECT current_schema()").Scan(&schema); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(schema, "cove_test_") {
		t.Fatal("refusing to restore outside test schema")
	}
	dump, err := exec.Command("docker", "exec", container, "pg_dump", "-U", "postgres", "-d", "cove_test", "--schema", schema, "--format=custom").Output()
	if err != nil {
		t.Fatal("backup command failed:", err)
	}
	if _, err = db.Exec(ctx, "DROP SCHEMA "+schema+" CASCADE"); err != nil {
		t.Fatal(err)
	}
	restore := exec.Command("docker", "exec", "-i", container, "pg_restore", "-U", "postgres", "-d", "cove_test", "--single-transaction", "--exit-on-error")
	restore.Stdin = bytes.NewReader(dump)
	if out, err := restore.CombinedOutput(); err != nil {
		t.Fatalf("restore failed: %v %s", err, out)
	}
	if err = s.Init(ctx); err != nil {
		t.Fatal("restored key validation failed:", err)
	}
	wrong := *s
	wrong.Key = bytes.Repeat([]byte{7}, 32)
	if err = wrong.Init(ctx); err == nil {
		t.Fatal("wrong encryption key accepted after restore")
	}
}

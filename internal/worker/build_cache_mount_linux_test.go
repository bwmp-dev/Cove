package worker

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBuildCacheMountInventory(t *testing.T) {
	line := "12 1 0:123 / /state rw,nosuid - tmpfs tmpfs rw,size=64m\n"
	if _, err := cacheMountAt(line, "/state"); err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string]string{
		"subdirectory": strings.Replace(line, " / /state ", " /nested /state ", 1),
		"read only":    strings.Replace(line, " rw,nosuid ", " ro,nosuid ", 1),
		"stacked":      line + line,
		"missing":      "",
		"malformed":    "garbage\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := cacheMountAt(data, "/state"); err == nil {
				t.Fatal("unverified mount accepted")
			}
		})
	}
	if _, err := cacheMountAt(strings.Replace(line, "/state", `/state\040with\040spaces`, 1), "/state with spaces"); err != nil {
		t.Fatal(err)
	}
}

func TestBuildCacheOwnershipPreservesForeignData(t *testing.T) {
	path := t.TempDir()
	if err := os.Chmod(path, 0700); err != nil {
		t.Fatal(err)
	}
	if err := claimCacheOwner(path, "owner"); err != nil {
		t.Fatal(err)
	}
	if err := verifyCacheOwner(path, "owner", true); err != nil {
		t.Fatal(err)
	}
	if err := claimCacheOwner(path, "foreign"); err == nil {
		t.Fatal("foreign state adopted")
	}
	other := t.TempDir()
	if err := os.WriteFile(filepath.Join(other, "foreign-file"), []byte("retain"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := claimCacheOwner(other, "owner"); err == nil {
		t.Fatal("foreign files adopted")
	}
	if data, err := os.ReadFile(filepath.Join(other, "foreign-file")); err != nil || string(data) != "retain" {
		t.Fatal("foreign data changed", err)
	}
	if _, err := os.Stat(filepath.Join(other, ".cove-build-cache-owner.json")); !os.IsNotExist(err) {
		t.Fatal("foreign filesystem marked as owned", err)
	}
}

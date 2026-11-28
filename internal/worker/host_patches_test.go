package worker

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestHostSecurityReportsKeepUnknownAndBoundPackages(t *testing.T) {
	for _, data := range []string{`{}`, `{"count":0,"packages":null,"automatic_reboot":false}`, `{"count":1,"packages":[],"automatic_reboot":false}`, `{"count":2,"packages":["apt","apt"],"automatic_reboot":false}`, `{"count":1,"packages":["bad\nname"],"automatic_reboot":false}`} {
		if _, _, _, err := decodeAPTSecurity(data); err == nil {
			t.Fatalf("invalid security report accepted: %s", data)
		}
	}
	count, packages, reboot, err := decodeAPTSecurity(`{"count":2,"packages":["apt","libc6:amd64"],"automatic_reboot":false}`)
	if err != nil || count != 2 || len(packages) != 2 || reboot {
		t.Fatal("valid APT report rejected", err)
	}
	count, packages, err = decodeDNFSecurity("RHSA-2026:1234 Critical/Sec. openssl-libs-1.2.x86_64\nRHSA-2026:1235 Moderate/Sec. openssl-libs-1.2.x86_64\nRHBA-2026:555 bugfix harmless-1.x86_64\nALSA-2026:999 Important/Sec. kernel-6.x86_64\n")
	if err != nil || count != 2 || len(packages) != 2 {
		t.Fatal("security advisories were not deduplicated or filtered", err)
	}
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "etc"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "etc", "os-release"), []byte("ID=ubuntu\nVERSION_ID=\"24.04\"\nPRETTY_NAME=\"Ubuntu 24.04\"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	report, err := inspectHostPatches(context.Background(), root, "cove-test", func(context.Context, ...string) (string, error) {
		return "", errors.New("injected unavailable host tools")
	})
	if err != nil || report.SecurityUpdates != nil || report.PolicySupported || len(report.Notices) == 0 {
		t.Fatal("missing metadata became healthy", report, err)
	}
}

func TestHostPatchInspectionOnOwnedLinuxVM(t *testing.T) {
	if runtime.GOOS != "linux" || os.Getenv("COVE_TEST_OWNED_HOST_INSPECTION") != "1" {
		t.Skip("explicit owned Linux VM inspection required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	report, err := inspectHostPatches(ctx, "/", "cove-uninstalled-test-inspection", snapshotHostCommand)
	if err != nil || report.Manager != "apt" || report.SecurityUpdates == nil || !report.PolicySupported || !strings.Contains(report.OS, "Ubuntu") {
		t.Fatal("real host inspection failed", report, err)
	}
	if report.LastResult != "" {
		t.Fatal("an uninstalled service claimed a prior result", report.LastResult)
	}
	encoded, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	t.Log(string(encoded))
}

func TestHostSecurityInspectionReportsUnclassifiedApplianceAsUnsupported(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "etc"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "etc/os-release"), []byte("PRETTY_NAME=\"Docker Desktop\"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	report, err := inspectHostPatches(context.Background(), root, "test-appliance", func(context.Context, ...string) (string, error) { return "", errors.New("native tool unavailable") })
	if err != nil || report.OS != "Docker Desktop" || report.Manager != "unsupported" || report.PolicySupported || report.SecurityUpdates != nil {
		t.Fatal("unclassified appliance was misreported", report, err)
	}
}

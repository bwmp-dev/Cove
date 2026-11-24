package worker

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestImageScanRedactsAndBindsReport(t *testing.T) {
	id := "sha256:" + strings.Repeat("a", 64)
	report := `{"SchemaVersion":2,"Metadata":{"ImageID":"` + id + `","ImageConfig":{"Env":["TOKEN=private"]},"OS":{"EOSL":true}},"Results":[{"Class":"os-pkgs","Type":"alpine","Secrets":[{"Match":"private"}],"Vulnerabilities":[{"VulnerabilityID":"CVE-2026-12345","PkgName":"openssl","InstalledVersion":"1.0","FixedVersion":"2.0","Severity":"HIGH","Description":"private","References":["https://private.example"]}]}]}`
	result, err := parseImageScan([]byte(report), "web", id, id)
	if err != nil || result.State != "complete" || result.Counts["HIGH"] != 1 || !result.EndOfLife {
		t.Fatal(result, err)
	}
	data, _ := json.Marshal(result)
	if strings.Contains(string(data), "private") || strings.Contains(string(data), "Secrets") || result.Findings[0].ID != "CVE-2026-12345" {
		t.Fatal(string(data))
	}
	if _, err = parseImageScan([]byte(report), "web", id, "sha256:"+strings.Repeat("b", 64)); err == nil {
		t.Fatal("report for another image accepted")
	}
}

func TestImageScanUnknownCoverageAndBoundedFindings(t *testing.T) {
	id := "sha256:" + strings.Repeat("a", 64)
	if result, err := parseImageScan([]byte(`{"SchemaVersion":2,"Metadata":{"ImageID":"`+id+`"},"Results":[]}`), "web", id, id); err != nil || result.State != "unknown" {
		t.Fatal(result, err)
	}
	findings := []map[string]string{}
	for i := 0; i < scanFindingLimit+1; i++ {
		findings = append(findings, map[string]string{"VulnerabilityID": "CVE-2026-12345", "PkgName": "pkg-" + strings.Repeat("a", i%100) + string(rune('A'+i/100)), "InstalledVersion": "1.0", "Severity": "HIGH"})
	}
	findings = append(findings, map[string]string{"VulnerabilityID": "CVE-2026-12346", "PkgName": "critical", "InstalledVersion": "1.0", "Severity": "CRITICAL"})
	data, _ := json.Marshal(map[string]any{"SchemaVersion": 2, "Metadata": map[string]string{"ImageID": id}, "Results": []any{map[string]any{"Class": "os-pkgs", "Type": "alpine", "Vulnerabilities": findings}}})
	result, err := parseImageScan(data, "web", id, id)
	if err != nil || !result.Truncated || len(result.Findings) != scanFindingLimit || result.Findings[0].Severity != "CRITICAL" || result.Counts["HIGH"] != scanFindingLimit+1 {
		t.Fatal(result, err)
	}
}

func TestImageScanDatabaseFreshness(t *testing.T) {
	now := time.Now().UTC()
	db := ScanDatabase{Digest: "sha256:" + strings.Repeat("a", 64), UpdatedAt: now.Add(-time.Hour), NextUpdate: now.Add(time.Hour)}
	if !db.Fresh(now) {
		t.Fatal("fresh database refused")
	}
	for _, changed := range []ScanDatabase{{}, {Digest: db.Digest, UpdatedAt: now.Add(time.Hour), NextUpdate: db.NextUpdate}, {Digest: db.Digest, UpdatedAt: now.Add(-25 * time.Hour), NextUpdate: db.NextUpdate}, {Digest: db.Digest, UpdatedAt: db.UpdatedAt, NextUpdate: now}} {
		if changed.Fresh(now) {
			t.Fatal("missing, future or stale database accepted", changed)
		}
	}
}

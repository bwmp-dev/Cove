package worker

import (
	"encoding/json"
	"errors"
	"regexp"
	"sort"
	"strings"
	"time"
)

const ScannerImage = "docker.io/aquasec/trivy:0.76.2@sha256:8f6bb947dbc528902e11f2f5be52f7b519f6a39d5d148f41166f2040499768fb"
const scannerVersion = "0.76.2"
const scanOutputLimit = 8 * 1024 * 1024
const scanFindingLimit = 256
const scanDatabaseMaxAge = 24 * time.Hour

type ScanFinding struct {
	ID        string `json:"id"`
	Package   string `json:"package"`
	Installed string `json:"installed"`
	Fixed     string `json:"fixed,omitempty"`
	Severity  string `json:"severity"`
}

type ScannedImage struct {
	Service      string         `json:"service"`
	ImageID      string         `json:"image_id"`
	Identity     string         `json:"image_identity,omitempty"`
	ConfigDigest string         `json:"config_digest"`
	State        string         `json:"state"`
	Message      string         `json:"message,omitempty"`
	Counts       map[string]int `json:"counts"`
	Findings     []ScanFinding  `json:"findings"`
	Truncated    bool           `json:"truncated"`
	EndOfLife    bool           `json:"end_of_life"`
	Coverage     ScanCoverage   `json:"coverage"`
}

type ScanCoverage struct {
	OS             string `json:"os"`
	OtherLanguages string `json:"other_languages"`
	JavaArchives   string `json:"java_archives"`
	JavaMessage    string `json:"java_message,omitempty"`
}

func applyScanArchiveCoverage(result *ScannedImage, archive scanArchive) {
	result.Coverage.JavaArchives = "not_detected"
	if archive.JavaArchives {
		result.Coverage.JavaArchives = "not_scanned"
		result.Coverage.JavaMessage = "JAR, WAR or EAR artifacts were detected. Java archive dependencies are not scanned because the Java index is outside this slice's database budget."
		result.State = "unknown"
		if result.Coverage.OS == "scanned" || result.Coverage.OtherLanguages == "scanned" {
			result.Message = "OS and other-language findings are available, but Java archive dependency coverage is unknown."
		}
	}
}

type ScanDatabase struct {
	Digest     string    `json:"digest"`
	UpdatedAt  time.Time `json:"updated_at"`
	NextUpdate time.Time `json:"next_update"`
}

type ImageScan struct {
	ScannerImage   string         `json:"scanner_image"`
	ScannerVersion string         `json:"scanner_version"`
	Database       ScanDatabase   `json:"database"`
	CheckedAt      time.Time      `json:"checked_at"`
	State          string         `json:"state"`
	Message        string         `json:"message,omitempty"`
	Images         []ScannedImage `json:"images"`
}

func (scan ImageScan) Complete(now time.Time) bool {
	if scan.State != "complete" || scan.ScannerImage != ScannerImage || scan.ScannerVersion != scannerVersion || !scan.Database.Fresh(now) || scan.CheckedAt.IsZero() || scan.CheckedAt.After(now) || len(scan.Images) == 0 || len(scan.Images) > 64 {
		return false
	}
	for _, image := range scan.Images {
		if image.State != "complete" || !imageDigest.MatchString(image.ImageID) || !imageDigest.MatchString(image.ConfigDigest) || image.Coverage.JavaArchives != "not_detected" || (image.Coverage.OS != "scanned" && image.Coverage.OtherLanguages != "scanned") {
			return false
		}
		for _, coverage := range []string{image.Coverage.OS, image.Coverage.OtherLanguages} {
			if coverage != "scanned" && coverage != "not_detected" {
				return false
			}
		}
	}
	return true
}

var scanValue = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_./+:@~%()=,* -]{0,159}$`)
var scanVulnerability = regexp.MustCompile(`^(?:CVE-[0-9]{4}-[0-9]{4,}|GHSA-[a-z0-9-]+|[A-Z][A-Z0-9-]{1,20}-[A-Za-z0-9.-]+)$`)

func (db ScanDatabase) Fresh(now time.Time) bool {
	return imageDigest.MatchString(db.Digest) && !db.UpdatedAt.IsZero() && !db.UpdatedAt.After(now) && now.Sub(db.UpdatedAt) <= scanDatabaseMaxAge && db.NextUpdate.After(now)
}

func scanSeverity(value string) string {
	switch value {
	case "CRITICAL", "HIGH", "MEDIUM", "LOW":
		return value
	default:
		return "UNKNOWN"
	}
}

func scanRank(value string) int {
	switch value {
	case "CRITICAL":
		return 0
	case "HIGH":
		return 1
	case "MEDIUM":
		return 2
	case "LOW":
		return 3
	default:
		return 4
	}
}

func parseImageScan(data []byte, service, imageID, configDigest string) (ScannedImage, error) {
	result := ScannedImage{Service: service, ImageID: imageID, ConfigDigest: configDigest, State: "unknown", Counts: map[string]int{}, Findings: []ScanFinding{}}
	result.Coverage = ScanCoverage{OS: "not_detected", OtherLanguages: "not_detected", JavaArchives: "unknown"}
	if len(data) > scanOutputLimit || !imageDigest.MatchString(imageID) || !imageDigest.MatchString(configDigest) {
		return result, errors.New("scanner output exceeds its limit or image identity is unavailable")
	}
	var report struct {
		SchemaVersion int
		Metadata      struct {
			ImageID string
			OS      *struct{ EOSL bool }
		}
		Results []struct {
			Class           string
			Type            string
			Vulnerabilities []struct {
				VulnerabilityID  string
				PkgName          string
				InstalledVersion string
				FixedVersion     string
				Severity         string
			}
		}
	}
	if json.Unmarshal(data, &report) != nil || report.SchemaVersion != 2 || report.Metadata.ImageID != configDigest || len(report.Results) > 4096 {
		return result, errors.New("scanner report does not identify the exported image")
	}
	covered := false
	seen := map[ScanFinding]bool{}
	for _, target := range report.Results {
		if target.Class != "os-pkgs" && target.Class != "lang-pkgs" {
			continue
		}
		covered = covered || target.Type != ""
		if target.Type != "" && target.Class == "os-pkgs" {
			result.Coverage.OS = "scanned"
		} else if target.Type != "" {
			result.Coverage.OtherLanguages = "scanned"
		}
		for _, finding := range target.Vulnerabilities {
			if !scanVulnerability.MatchString(finding.VulnerabilityID) || !scanValue.MatchString(finding.PkgName) || !scanValue.MatchString(finding.InstalledVersion) || (finding.FixedVersion != "" && !scanValue.MatchString(finding.FixedVersion)) || strings.Contains(finding.PkgName, "://") {
				return result, errors.New("scanner returned an unsupported finding identifier")
			}
			item := ScanFinding{finding.VulnerabilityID, finding.PkgName, finding.InstalledVersion, finding.FixedVersion, scanSeverity(finding.Severity)}
			if seen[item] {
				continue
			}
			seen[item] = true
			result.Counts[item.Severity]++
			result.Findings = append(result.Findings, item)
		}
	}
	if report.Metadata.OS != nil {
		result.EndOfLife = report.Metadata.OS.EOSL
	}
	if !covered {
		result.Message = "The scanner did not report supported operating-system or language package coverage."
		return result, nil
	}
	sort.Slice(result.Findings, func(i, j int) bool {
		a, b := result.Findings[i], result.Findings[j]
		if scanRank(a.Severity) != scanRank(b.Severity) {
			return scanRank(a.Severity) < scanRank(b.Severity)
		}
		return a.ID+"/"+a.Package+"/"+a.Installed < b.ID+"/"+b.Package+"/"+b.Installed
	})
	result.Truncated = len(result.Findings) > scanFindingLimit
	if result.Truncated {
		result.Findings = result.Findings[:scanFindingLimit]
	}
	result.State = "complete"
	return result, nil
}

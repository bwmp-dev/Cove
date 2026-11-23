package worker

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type HostPatchStatus struct {
	CheckedAt       time.Time  `json:"checked_at"`
	OS              string     `json:"os"`
	Version         string     `json:"version"`
	Manager         string     `json:"manager"`
	SecurityUpdates *int       `json:"security_updates"`
	Packages        []string   `json:"packages"`
	Limited         bool       `json:"limited"`
	MetadataAt      *time.Time `json:"metadata_at"`
	RebootRequired  *bool      `json:"reboot_required"`
	AutomaticReboot *bool      `json:"automatic_reboot"`
	PolicySupported bool       `json:"policy_supported"`
	PolicyEnabled   bool       `json:"policy_enabled"`
	LastResult      string     `json:"last_result"`
	Notices         []string   `json:"notices"`
}

const hostMetadataLimit = 64 << 10

var hostPackageName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9.+:_-]{0,255}$`)

const aptSecurityQuery = `import apt,apt_pkg,json
c=apt.Cache()
p=[]
for package in c:
 if package.is_upgradable and package.candidate and any("security" in (origin.archive or "").lower() or "security" in (origin.label or "").lower() for origin in package.candidate.origins):
  p.append(package.name)
print(json.dumps({"count":len(p),"packages":sorted(p)[:200],"automatic_reboot":apt_pkg.config.find_b("Unattended-Upgrade::Automatic-Reboot",False)}))`

func readHostFile(filename string) ([]byte, error) {
	info, err := os.Stat(filename)
	if err != nil || !info.Mode().IsRegular() || info.Size() > hostMetadataLimit {
		return nil, errors.New("host metadata unavailable or oversized")
	}
	file, err := os.Open(filename)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, hostMetadataLimit+1))
	if err != nil || len(data) > hostMetadataLimit {
		return nil, errors.New("host metadata exceeds its limit")
	}
	return data, nil
}

func hostRelease(data []byte) (map[string]string, error) {
	values := map[string]string{}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, found := strings.Cut(line, "=")
		if !found {
			continue
		}
		if key != "ID" && key != "VERSION_ID" && key != "PRETTY_NAME" {
			continue
		}
		if strings.HasPrefix(value, `"`) {
			decoded, err := strconv.Unquote(value)
			if err != nil {
				return nil, errors.New("invalid host distribution metadata")
			}
			value = decoded
		} else {
			value = strings.Trim(value, "'")
		}
		if len(value) > 256 || strings.ContainsAny(value, "\x00\r\n") {
			return nil, errors.New("invalid host distribution field")
		}
		values[key] = value
	}
	if values["ID"] == "" && values["PRETTY_NAME"] == "" {
		return nil, errors.New("host distribution is unknown")
	}
	return values, nil
}

func decodeAPTSecurity(data string) (int, []string, bool, error) {
	var report struct {
		Count           *int      `json:"count"`
		Packages        *[]string `json:"packages"`
		AutomaticReboot *bool     `json:"automatic_reboot"`
	}
	if len(data) > hostMetadataLimit || json.Unmarshal([]byte(data), &report) != nil || report.Count == nil || report.Packages == nil || report.AutomaticReboot == nil || *report.Count < 0 || *report.Count > 1000000 || len(*report.Packages) > 200 || *report.Count < len(*report.Packages) || *report.Count <= 200 && *report.Count != len(*report.Packages) {
		return 0, nil, false, errors.New("invalid security update report")
	}
	seen := map[string]bool{}
	for _, name := range *report.Packages {
		if !hostPackageName.MatchString(name) || seen[name] {
			return 0, nil, false, errors.New("invalid security package name")
		}
		seen[name] = true
	}
	return *report.Count, *report.Packages, *report.AutomaticReboot, nil
}

func decodeDNFSecurity(data string) (int, []string, error) {
	if len(data) > hostMetadataLimit {
		return 0, nil, errors.New("security advisory report exceeds its limit")
	}
	packages := []string{}
	seen := map[string]bool{}
	for _, line := range strings.Split(data, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 || !strings.Contains(strings.ToLower(fields[1]), "sec.") {
			continue
		}
		name := fields[len(fields)-1]
		if !hostPackageName.MatchString(name) {
			return 0, nil, errors.New("invalid security advisory package")
		}
		if seen[name] {
			continue
		}
		seen[name] = true
		if len(packages) < 200 {
			packages = append(packages, name)
		}
	}
	return len(seen), packages, nil
}

func hostMetadataTime(directory string) *time.Time {
	entries, err := os.ReadDir(directory)
	if err != nil || len(entries) > 10000 {
		return nil
	}
	var latest time.Time
	for _, entry := range entries {
		if entry.IsDir() || entry.Type()&os.ModeSymlink != 0 || entry.Name() == "lock" {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return nil
		}
		if info.ModTime().After(latest) {
			latest = info.ModTime()
		}
	}
	if latest.IsZero() {
		return nil
	}
	return &latest
}

func inspectHostPatches(ctx context.Context, root, owner string, run snapshotCommand) (HostPatchStatus, error) {
	status := HostPatchStatus{CheckedAt: time.Now().UTC(), Packages: []string{}, Notices: []string{}}
	data, err := readHostFile(filepath.Join(root, "etc", "os-release"))
	if err != nil {
		return status, err
	}
	release, err := hostRelease(data)
	if err != nil {
		return status, err
	}
	status.OS, status.Version = release["PRETTY_NAME"], release["VERSION_ID"]
	switch release["ID"] {
	case "ubuntu", "debian":
		status.Manager = "apt"
		output, err := run(ctx, "/usr/bin/python3", "-c", aptSecurityQuery)
		if err == nil {
			count, packages, automatic, parseErr := decodeAPTSecurity(output)
			if parseErr == nil {
				status.SecurityUpdates, status.Packages, status.AutomaticReboot = &count, packages, &automatic
				status.Limited = count > len(packages)
			} else {
				status.Notices = append(status.Notices, "Security metadata could not be validated.")
			}
		} else {
			status.Notices = append(status.Notices, "Security metadata is unavailable; install python3-apt and refresh the host's package indexes.")
		}
		status.MetadataAt = hostMetadataTime(filepath.Join(root, "var", "lib", "apt", "lists"))
		_, rebootErr := os.Stat(filepath.Join(root, "run", "reboot-required"))
		if rebootErr == nil || os.IsNotExist(rebootErr) {
			needed := rebootErr == nil
			status.RebootRequired = &needed
		}
		tool, toolErr := os.Stat(filepath.Join(root, "usr", "bin", "unattended-upgrade"))
		status.PolicySupported = toolErr == nil && tool.Mode().IsRegular() && tool.Mode().Perm()&0111 != 0
		if !status.PolicySupported {
			status.Notices = append(status.Notices, "Install unattended-upgrades on the host before enabling Cove's schedule.")
		}
	case "rhel", "almalinux", "rocky", "centos", "fedora":
		status.Manager = "dnf"
		output, err := run(ctx, "/usr/bin/dnf", "-q", "--cacheonly", "updateinfo", "list", "--security", "--available")
		if err == nil {
			count, packages, parseErr := decodeDNFSecurity(output)
			if parseErr == nil {
				status.SecurityUpdates, status.Packages = &count, packages
				status.Limited = count > len(packages)
			} else {
				status.Notices = append(status.Notices, "Security advisory metadata could not be validated.")
			}
		} else {
			status.Notices = append(status.Notices, "Cached security advisory metadata is unavailable; refresh the host's DNF metadata.")
		}
		status.MetadataAt = hostMetadataTime(filepath.Join(root, "var", "cache", "dnf"))
		tool, toolErr := os.Stat(filepath.Join(root, "usr", "bin", "dnf"))
		status.PolicySupported = toolErr == nil && tool.Mode().IsRegular() && tool.Mode().Perm()&0111 != 0
		output, rebootErr := run(ctx, "/usr/bin/needs-restarting", "-r")
		if rebootErr == nil {
			needed := false
			status.RebootRequired = &needed
		} else if strings.Contains(strings.ToLower(output), "reboot is required") {
			needed := true
			status.RebootRequired = &needed
		}
	default:
		status.Manager = "unsupported"
		status.Notices = append(status.Notices, "This host distribution has no supported security-update provider.")
	}
	if status.MetadataAt == nil {
		status.Notices = append(status.Notices, "Package metadata time is unknown; an empty cached result does not prove the host is current.")
	} else if time.Since(*status.MetadataAt) > 24*time.Hour {
		status.Notices = append(status.Notices, "Package metadata is more than one day old; counts describe cached advisories.")
	}
	if _, err := os.Stat(filepath.Join(root, "run", "systemd", "system")); err != nil {
		status.PolicySupported = false
		status.Notices = append(status.Notices, "A running host systemd manager is required for scheduled updates.")
	}
	unit := hostPolicyUnit(owner)
	unitDirectory := filepath.Join(root, "etc", "systemd", "system")
	marker := hostPolicyMarker(owner)
	if ownedHostPolicyFile(filepath.Join(unitDirectory, unit+".timer"), marker) != nil || ownedHostPolicyFile(filepath.Join(unitDirectory, unit+".service"), marker) != nil {
		return status, nil
	}
	load, loadErr := run(ctx, "/usr/bin/systemctl", "show", unit+".timer", "--property=LoadState", "--value")
	if loadErr == nil && strings.TrimSpace(load) == "loaded" {
		enabled, enabledErr := run(ctx, "/usr/bin/systemctl", "is-enabled", unit+".timer")
		active, activeErr := run(ctx, "/usr/bin/systemctl", "is-active", unit+".timer")
		status.PolicyEnabled = enabledErr == nil && activeErr == nil && strings.TrimSpace(enabled) == "enabled" && strings.TrimSpace(active) == "active"
		if enabledErr == nil && activeErr != nil {
			status.Notices = append(status.Notices, "Cove's timer is enabled but not active; retry the saved schedule.")
		}
	}
	result, resultErr := run(ctx, "/usr/bin/systemctl", "show", unit+".service", "--property=LoadState", "--property=ExecMainStartTimestampMonotonic", "--property=Result")
	if resultErr == nil {
		fields := map[string]string{}
		for _, line := range strings.Split(result, "\n") {
			key, value, found := strings.Cut(line, "=")
			if found && len(value) < 64 {
				fields[key] = value
			}
		}
		started, parseErr := strconv.ParseUint(fields["ExecMainStartTimestampMonotonic"], 10, 64)
		if fields["LoadState"] == "loaded" && parseErr == nil && started > 0 {
			status.LastResult = fields["Result"]
		}
	}
	return status, nil
}

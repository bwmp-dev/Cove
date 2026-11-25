package worker

import (
	"context"
	"errors"
	"runtime"
	"strings"
)

const NixpacksVersion = "1.41.0"
const NixpacksBaseImage = "ghcr.io/railwayapp/nixpacks@sha256:d45c89d80e13d7ad0fd555b5130f22a866d9dd10e861f589932303ef2314c7de"
const nixpacksBaseTag = "ghcr.io/railwayapp/nixpacks:ubuntu-1745885067"
const nixpacksOutputLimit = 8 << 20

type NixpacksSupport struct {
	Supported            bool    `json:"supported"`
	Reason               string  `json:"reason"`
	Version              string  `json:"version"`
	PlannerSHA256        string  `json:"planner_sha256"`
	PlannerDownloadBytes int64   `json:"planner_download_bytes"`
	BaseImage            string  `json:"base_image"`
	BaseDownloadBytes    int64   `json:"base_download_bytes"`
	Maintenance          bool    `json:"maintenance"`
	ProxySupported       bool    `json:"proxy_supported"`
	PlannerMemoryBytes   int64   `json:"planner_memory_bytes"`
	PlannerCPU           float64 `json:"planner_cpu"`
	PlannerTimeout       int     `json:"planner_timeout_seconds"`
	PlannerOutputBytes   int64   `json:"planner_output_bytes"`
}

func NixpacksDetails(probe Probe, ready bool) NixpacksSupport {
	status := NixpacksSupport{Version: NixpacksVersion, BaseImage: NixpacksBaseImage, Maintenance: true, PlannerMemoryBytes: 512 << 20, PlannerCPU: 1, PlannerTimeout: 60, PlannerOutputBytes: 128 << 20}
	artifact, err := nixpacksPlannerArtifact(probe.Arch)
	if err == nil {
		status.PlannerSHA256, status.PlannerDownloadBytes = "sha256:"+artifact.SHA256, artifact.Bytes
		status.BaseDownloadBytes = 138966568
		if probe.Arch == "arm64" {
			status.BaseDownloadBytes = 135771180
		}
	}
	switch {
	case !ready:
		status.Reason = "the effective Nixpacks build node is not ready"
	case probe.OS != "linux" || err != nil:
		status.Reason = "the pinned Nixpacks planner requires a Linux amd64 or arm64 build node"
	case probe.NixpacksReason != "":
		status.Reason = probe.NixpacksReason
	case !probe.Nixpacks || !probe.BuildRecipes:
		status.Reason = "upgrade and verify the effective build worker before using Nixpacks"
	default:
		status.Supported = true
	}
	return status
}

type nixpacksArtifact struct {
	Name   string
	SHA256 string
	Bytes  int64
}

func nixpacksPlannerArtifact(architecture string) (nixpacksArtifact, error) {
	switch architecture {
	case "amd64":
		return nixpacksArtifact{"nixpacks-v1.41.0-x86_64-unknown-linux-musl.tar.gz", "0f55de7874507b9cf7502113120bd96f2ab6979f78d10eaf2eb2ade9207b3af6", 3717141}, nil
	case "arm64":
		return nixpacksArtifact{"nixpacks-v1.41.0-aarch64-unknown-linux-musl.tar.gz", "912bd02dd2bb6f9c3a9ed965fe8a68b4aa318dc7a2546e2eca6f2806a894ba39", 4111227}, nil
	default:
		return nixpacksArtifact{}, errors.New("the pinned Nixpacks planner supports Linux amd64 and arm64 only")
	}
}

func NixpacksPlannerDigest(architecture string) string {
	artifact, err := nixpacksPlannerArtifact(architecture)
	if err != nil {
		return ""
	}
	return "sha256:" + artifact.SHA256
}

func NixpacksSupported() bool {
	_, err := nixpacksPlannerArtifact(runtime.GOARCH)
	return runtime.GOOS == "linux" && err == nil
}

func (e *Engine) checkNixpacksPlatform(ctx context.Context, target string) error {
	if !NixpacksSupported() {
		return errors.New("the pinned Nixpacks planner requires a supported Linux build worker")
	}
	if target != "" && target != "linux/"+runtime.GOARCH {
		return errors.New("Nixpacks requires matching build-worker and runtime platforms; emulation is unavailable")
	}
	platform, err := e.output(ctx, "docker", "info", "--format", "{{.OSType}}/{{.Architecture}}")
	operatingSystem, architecture, found := strings.Cut(platform, "/")
	architecture, archErr := buildpacksArchitecture(architecture)
	if err != nil || !found || operatingSystem != "linux" || archErr != nil || architecture != runtime.GOARCH {
		return errors.New("Nixpacks requires Docker and the build worker to share their native Linux architecture; verify the Docker endpoint without emulation")
	}
	return nil
}

func validateNixpacksBuildPolicy(build map[string]any) error {
	_, proxies, err := recipeBuildPolicy(build)
	if err != nil {
		return err
	}
	if len(proxies) != 0 {
		return errors.New("Nixpacks is unavailable with an outbound build proxy: the shared BuildKit network cannot enforce proxy-only egress for generated build commands")
	}
	if arguments, ok := build["args"].(map[string]any); ok {
		for name, value := range arguments {
			if !strings.HasSuffix(strings.ToUpper(name), "_PROXY") || value != "" {
				return errors.New("Nixpacks requires no Compose build arguments; use repository Nixpacks configuration or selected build-secret files")
			}
		}
	} else if build["args"] != nil {
		return errors.New("Nixpacks requires explicit build settings without inherited or list-shaped build arguments")
	}
	return nil
}

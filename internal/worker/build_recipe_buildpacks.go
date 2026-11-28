package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"runtime"
	"strings"
)

const BuildpacksLifecycleVersion = "0.21.29"
const BuildpacksBuilderVersion = "0.0.230"
const BuildpacksLifecycleImage = "docker.io/buildpacksio/lifecycle@sha256:6119d8ac92f81873a43b1db6fbedcc1538809161b93dcf8b9e58086e46002661"
const BuildpacksBuilderImage = "docker.io/paketobuildpacks/ubuntu-noble-builder@sha256:45f9d50b1f96c1cddd9c288823a9291ec2e1295e6fa3dc24d2f7f8ee2b46cc45"
const BuildpacksRunImage = "docker.io/paketobuildpacks/ubuntu-noble-run@sha256:a0386bc457590ffed6ddd6039c569233ac3f9afefe38766641e214e49b3cbfba"
const BuildpacksWorkBytes uint64 = 3 << 30
const BuildpacksMemoryBytes uint64 = 4 << 30
const BuildpacksMinimumMemoryBytes uint64 = 2 << 30
const BuildpacksSafetyBytes uint64 = 2 << 30
const BuildpacksDiskBytes uint64 = 24 << 30
const BuildpacksStateBytes uint64 = 10 << 30
const BuildpacksCPUs = 2
const BuildpacksQuotaCPUs = 2.5
const BuildpacksHelperMemoryMB = 256

type BuildpacksAdmission struct {
	AvailableMemoryBytes uint64 `json:"available_memory_bytes"`
	PhaseMemoryBytes     uint64 `json:"phase_memory_bytes"`
}

func DeriveBuildpacksAdmission(available uint64) (BuildpacksAdmission, error) {
	if available < BuildpacksMinimumMemoryBytes+BuildpacksSafetyBytes {
		return BuildpacksAdmission{}, fmt.Errorf("Cloud Native Buildpacks requires at least 4 GiB actually available memory; observed %d bytes. Admission reserves at least 2 GiB for a phase and 2 GiB safety headroom.", available)
	}
	limit := (available - BuildpacksSafetyBytes) / (1 << 20) * (1 << 20)
	if limit > BuildpacksMemoryBytes {
		limit = BuildpacksMemoryBytes
	}
	return BuildpacksAdmission{AvailableMemoryBytes: available, PhaseMemoryBytes: limit}, nil
}

func (admission BuildpacksAdmission) Validate() error {
	expected, err := DeriveBuildpacksAdmission(admission.AvailableMemoryBytes)
	if err != nil || admission != expected {
		return errors.New("CNB phase memory must be derived from admission's available memory minus 2 GiB, clamped to 2 to 4 GiB")
	}
	return nil
}

type BuildpacksCapacity struct {
	MemoryAvailable uint64 `json:"memory_available"`
	DiskAvailable   uint64 `json:"disk_available"`
	StateAvailable  uint64 `json:"state_available"`
}

func ValidateBuildpacksCapacity(capacity BuildpacksCapacity) error {
	if _, err := DeriveBuildpacksAdmission(capacity.MemoryAvailable); err != nil {
		return err
	}
	if capacity.DiskAvailable < BuildpacksDiskBytes || capacity.StateAvailable < BuildpacksStateBytes {
		return fmt.Errorf("Cloud Native Buildpacks requires at least 24 GiB free on Docker storage and 10 GiB free in worker state; observed Docker=%d bytes and worker=%d bytes. Its pinned builder is about 1.45 GB downloaded and 5.11 GB unpacked.", capacity.DiskAvailable, capacity.StateAvailable)
	}
	return nil
}

type BuildpacksSupport struct {
	Supported                    bool     `json:"supported"`
	Reason                       string   `json:"reason"`
	LifecycleVersion             string   `json:"lifecycle_version"`
	BuilderVersion               string   `json:"builder_version"`
	PlatformAPI                  string   `json:"platform_api"`
	PinnedImages                 []string `json:"pinned_images"`
	WorkStorage                  string   `json:"work_storage"`
	WorkHardCap                  bool     `json:"work_hard_cap"`
	WorkTargetBytes              uint64   `json:"work_target_bytes"`
	MinimumPhaseMemoryBytes      uint64   `json:"minimum_phase_memory_bytes"`
	MaximumPhaseMemoryBytes      uint64   `json:"maximum_phase_memory_bytes"`
	MemorySafetyBytes            uint64   `json:"memory_safety_bytes"`
	RequiredAvailableMemoryBytes uint64   `json:"required_available_memory_bytes"`
	RequiredDockerDiskBytes      uint64   `json:"required_docker_disk_bytes"`
	RequiredWorkerDiskBytes      uint64   `json:"required_worker_disk_bytes"`
	QuotaMemoryMB                uint64   `json:"quota_memory_mb"`
	QuotaCPU                     float64  `json:"quota_cpu"`
	BuilderDownloadBytes         uint64   `json:"builder_download_bytes"`
	BuilderUnpackedBytes         uint64   `json:"builder_unpacked_bytes"`
	Experimental                 bool     `json:"experimental"`
}

func BuildpacksDetails(probe Probe, ready bool) BuildpacksSupport {
	result := BuildpacksSupport{LifecycleVersion: BuildpacksLifecycleVersion, BuilderVersion: BuildpacksBuilderVersion, PlatformAPI: "0.13", PinnedImages: []string{BuildpacksLifecycleImage, BuildpacksBuilderImage, BuildpacksRunImage}, WorkStorage: "disk", WorkHardCap: false, WorkTargetBytes: BuildpacksWorkBytes, MinimumPhaseMemoryBytes: BuildpacksMinimumMemoryBytes, MaximumPhaseMemoryBytes: BuildpacksMemoryBytes, MemorySafetyBytes: BuildpacksSafetyBytes, RequiredAvailableMemoryBytes: BuildpacksMinimumMemoryBytes + BuildpacksSafetyBytes, RequiredDockerDiskBytes: BuildpacksDiskBytes, RequiredWorkerDiskBytes: BuildpacksStateBytes, QuotaMemoryMB: BuildpacksMemoryBytes/(1<<20) + BuildpacksHelperMemoryMB, QuotaCPU: BuildpacksQuotaCPUs, BuilderDownloadBytes: 1446326381, BuilderUnpackedBytes: 5108638067, Experimental: true}
	switch {
	case !ready:
		result.Reason = "the effective build node must be ready before using Cloud Native Buildpacks"
	case !probe.Buildpacks || !probe.BuildRecipes || probe.OS != "linux" || probe.Arch != "amd64" && probe.Arch != "arm64":
		result.Reason = "upgrade and verify a matching Linux amd64 or arm64 worker for Cloud Native Buildpacks"
	case probe.BuildpacksReason != "":
		result.Reason = probe.BuildpacksReason
	case probe.BuildpacksCapacity == nil:
		result.Reason = "Cloud Native Buildpacks resource admission is unknown; verify the build worker"
	default:
		if err := ValidateBuildpacksCapacity(*probe.BuildpacksCapacity); err != nil {
			result.Reason = err.Error()
		}
	}
	result.Supported = result.Reason == ""
	return result
}

func (e *Engine) buildpacksCapacity(ctx context.Context, platform string) (BuildpacksCapacity, error) {
	var capacity BuildpacksCapacity
	architecture, err := buildpacksArchitecture(runtime.GOARCH)
	if runtime.GOOS != "linux" || err != nil {
		return capacity, errors.New("the pinned Cloud Native Buildpacks builder requires a matching Linux amd64 or arm64 worker")
	}
	if platform != "" && platform != "linux/"+architecture {
		return capacity, errors.New("Cloud Native Buildpacks builder and runtime architectures must match; emulation is not supported")
	}
	daemon, err := e.cacheDockerRoot(ctx)
	if err != nil {
		return capacity, errors.New("CNB resource admission requires a local Unix Docker endpoint with a shared daemon process, cgroup and storage view")
	}
	data, err := e.output(ctx, "docker", "info", "--format", `{"os":{{json .OSType}},"architecture":{{json .Architecture}},"driver":{{json .DriverStatus}}}`)
	var info struct {
		OS           string     `json:"os"`
		Architecture string     `json:"architecture"`
		Driver       [][]string `json:"driver"`
	}
	if err != nil || json.Unmarshal([]byte(data), &info) != nil {
		return capacity, errors.New("CNB cannot verify the Docker platform and OCI image store")
	}
	daemonArch, err := buildpacksArchitecture(info.Architecture)
	if err != nil || info.OS != "linux" || daemonArch != architecture {
		return capacity, errors.New("the pinned CNB builder does not match the Docker daemon's Linux platform")
	}
	oci := false
	for _, driver := range info.Driver {
		if len(driver) == 2 && driver[0] == "driver-type" && driver[1] == "io.containerd.snapshotter.v1" {
			oci = true
		}
	}
	if !oci {
		return capacity, errors.New("CNB daemonless OCI import requires Docker's containerd image store")
	}
	return inspectBuildpacksCapacity(e.Root, daemon.Root)
}

func buildpacksArchitecture(value string) (string, error) {
	switch value {
	case "amd64", "x86_64":
		return "amd64", nil
	case "arm64", "aarch64":
		return "arm64", nil
	default:
		return "", errors.New("the pinned Cloud Native Buildpacks builder supports Linux amd64 and arm64 only")
	}
}

func validateBuildpacksPolicy(build map[string]any) error {
	if _, _, err := recipeBuildPolicy(build); err != nil {
		return errors.New("Cloud Native Buildpacks supports default or none build networking; host and custom networks are refused")
	}
	arguments, ok := build["args"].(map[string]any)
	if build["args"] != nil && !ok {
		return errors.New("Cloud Native Buildpacks requires explicit proxy argument values; inherited or list-shaped build arguments are unavailable")
	}
	for name, value := range arguments {
		switch strings.ToUpper(name) {
		case "HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "NO_PROXY":
			if _, ok := value.(string); !ok {
				return errors.New("Cloud Native Buildpacks requires explicit proxy argument values")
			}
		default:
			return errors.New("Cloud Native Buildpacks does not translate Compose build arguments; use repository buildpack configuration or selected build secrets")
		}
	}
	return nil
}

func validateBuildpacksServices(services map[string]any, selected string) error {
	for name, value := range services {
		service, _ := value.(map[string]any)
		if name != selected && service["build"] != nil {
			return errors.New("Cloud Native Buildpacks requires the selected service to be the only Compose build service; other image-only services are unchanged")
		}
	}
	return nil
}

func buildpacksComposeSecrets(request Request) Request {
	if request.BuildRecipe == nil || request.BuildRecipe.Engine != "buildpacks" {
		return request
	}
	filtered := make([]FileSecret, 0, len(request.FileSecrets))
	for _, secret := range request.FileSecrets {
		buildServices := make([]string, 0, len(secret.BuildServices))
		for _, service := range secret.BuildServices {
			if service != request.BuildRecipe.Service {
				buildServices = append(buildServices, service)
			}
		}
		secret.BuildServices = buildServices
		if len(secret.Services)+len(secret.BuildServices) > 0 {
			filtered = append(filtered, secret)
		}
	}
	request.FileSecrets = filtered
	return request
}

func (e *Engine) buildpacksProxies(r Request, explicit map[string]string) (map[string]string, error) {
	var environment []string
	for name, value := range r.Environment {
		environment = append(environment, name+"="+value)
	}
	proxy, err := e.buildProxy(environment)
	if err != nil {
		return nil, err
	}
	if r.OutboundProxy != nil {
		proxy = *r.OutboundProxy
		if err := proxy.Validate(); err != nil {
			return nil, err
		}
	}
	if proxy.HTTP != "" || proxy.HTTPS != "" {
		return map[string]string{"HTTP_PROXY": proxy.HTTP, "HTTPS_PROXY": proxy.HTTPS, "NO_PROXY": proxy.NoProxy}, nil
	}
	return explicit, nil
}

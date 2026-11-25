package worker

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

type Probe struct {
	AutomaticImageScans  bool                `json:"automatic_image_scans"`
	Nixpacks             bool                `json:"nixpacks"`
	NixpacksReason       string              `json:"nixpacks_reason,omitempty"`
	Buildpacks           bool                `json:"buildpacks"`
	BuildpacksReason     string              `json:"buildpacks_reason,omitempty"`
	BuildpacksCapacity   *BuildpacksCapacity `json:"buildpacks_capacity,omitempty"`
	BuildRecipes         bool                `json:"build_recipes"`
	BuildCacheLimits     bool                `json:"build_cache_limits"`
	RemoteBuilds         bool                `json:"remote_builds"`
	ManagedRegistry      bool                `json:"managed_registry"`
	ImageScanning        bool                `json:"image_scanning"`
	ImageUpdates         bool                `json:"image_updates"`
	CloudflarePublishing bool                `json:"cloudflare_publishing"`
	Adoption             bool                `json:"adoption"`
	Imports              bool                `json:"imports"`
	Drift                bool                `json:"drift"`
	SourceFiles          bool                `json:"source_files"`
	ComposeBundles       bool                `json:"compose_bundles"`
	Exec                 bool                `json:"exec"`
	BuiltinVariables     bool                `json:"builtin_variables"`
	ManagedDatabases     bool                `json:"managed_databases"`
	OutboundProxy        bool                `json:"outbound_proxy"`
	ReleaseDiff          bool                `json:"release_diff"`
	BinarySHA256         string              `json:"binary_sha256"`
	WorkloadMoves        bool                `json:"workload_moves"`
	GPUAssignments       bool                `json:"gpu_assignments"`
	ResourceDefaults     bool                `json:"resource_defaults"`
	Firewall             bool                `json:"firewall"`
	HostMaintenance      bool                `json:"host_maintenance"`
	SSHIdentity          bool                `json:"ssh_identity"`
	FileTransfers        bool                `json:"file_transfers"`
	PostgresPITR         bool                `json:"postgres_pitr"`
	FilesystemSnapshots  bool                `json:"filesystem_snapshots"`
	BackupTransfers      bool                `json:"backup_transfers"`
	StreamedBackups      bool                `json:"extended_logical_backups"`
	TrafficLogs          bool                `json:"traffic_logs"`
	CapacityAdvice       bool                `json:"capacity_advice"`
	StructuredLogs       bool                `json:"structured_logs"`
	ScheduledCleanup     bool                `json:"scheduled_cleanup"`
	JournalRetention     bool                `json:"journal_retention"`
	MountMetrics         bool                `json:"mount_metrics"`
	AlertObservations    bool                `json:"alert_observations"`
	FileBrowser          bool                `json:"file_browser"`
	BackblazeB2          bool                `json:"backblaze_b2"`
	BackupRetention      bool                `json:"backup_retention"`
	Telemetry            bool                `json:"telemetry"`
	IngressExposures     bool                `json:"ingress_exposures"`
	RouteProtocol        int                 `json:"route_protocol"`
	CPUAutoscale         bool                `json:"cpu_autoscale"`
	IdleSleep            bool                `json:"idle_sleep"`
	Suspensions          bool                `json:"suspensions"`
	UnhealthyRestart     bool                `json:"unhealthy_restart"`
	JobRuns              bool                `json:"job_runs"`
	Rollouts             bool                `json:"rollouts"`
	EnvironmentPreviews  bool                `json:"environment_previews"`
	PreviewCIImages      bool                `json:"preview_ci_images"`
	GuardedHooks         bool                `json:"guarded_hooks"`
	Previews             bool                `json:"previews"`
	GuardedDeploy        bool                `json:"guarded_deploy"`
	RestoreCopies        bool                `json:"restore_copies"`
	DatabaseAccess       bool                `json:"database_access"`
	FileSecrets          bool                `json:"file_secrets"`
	ExactPromotion       bool                `json:"exact_promotion"`
	DeploymentHooks      bool                `json:"deployment_hooks"`
	RestoreDrills        bool                `json:"restore_drills"`
	Preflight            bool                `json:"preflight"`
	Metrics              *NodeMetrics        `json:"metrics,omitempty"`
	SharedHost           *SharedHost         `json:"shared_host,omitempty"`
	ResourceLimits       bool                `json:"resource_limits"`
	PrivateNetworks      bool                `json:"private_networks"`
	LogicalBackups       bool                `json:"logical_backups"`
	Infrastructure       bool                `json:"infrastructure"`
	Terminal             bool                `json:"terminal"`
	SharedNetworks       bool                `json:"shared_networks"`
	PrivateLinks         bool                `json:"private_links"`
	LiveRoutes           bool                `json:"live_routes"`
	RoutePolicies        bool                `json:"route_policies"`
	ImageLifecycle       bool                `json:"image_lifecycle"`
	JobImageLifecycle    bool                `json:"job_image_lifecycle"`
	BuildPublish         bool                `json:"build_publish"`
	Protocol             int                 `json:"protocol"`
	OS                   string              `json:"os"`
	Arch                 string              `json:"arch"`
	Docker               string              `json:"docker"`
	Compose              string              `json:"compose"`
	Ready                bool                `json:"ready"`
}

func Inspect(ctx context.Context) Probe {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	p := Probe{Drift: true, SourceFiles: true, ComposeBundles: true, Exec: true, BuiltinVariables: true, GuardedDeploy: true, RestoreCopies: true, DatabaseAccess: runtime.GOOS == "linux", FileSecrets: true, ExactPromotion: true, DeploymentHooks: true, RestoreDrills: true, Preflight: true, ResourceLimits: true, PrivateNetworks: true, LogicalBackups: true, Infrastructure: true, Terminal: runtime.GOOS == "linux", SharedNetworks: true, PrivateLinks: runtime.GOOS == "linux", LiveRoutes: true, Protocol: 1, OS: runtime.GOOS, Arch: runtime.GOARCH}
	if exe, err := os.Executable(); err == nil {
		if data, err := os.ReadFile(exe); err == nil {
			p.BinarySHA256 = fmt.Sprintf("%x", sha256.Sum256(data))
		}
	}
	p.OutboundProxy = true
	p.ManagedDatabases = true
	p.ReleaseDiff = true
	p.WorkloadMoves = runtime.GOOS == "linux"
	p.Previews = true
	p.Imports = true
	p.Adoption = true
	p.Telemetry = true
	p.BackupRetention = true
	p.StreamedBackups = true
	p.BackupTransfers = true
	p.FilesystemSnapshots = runtime.GOOS == "linux"
	p.PostgresPITR = runtime.GOOS == "linux" && (runtime.GOARCH == "amd64" || runtime.GOARCH == "arm64")
	p.BackblazeB2 = true
	p.FileBrowser = true
	p.FileTransfers = true
	p.SSHIdentity = runtime.GOOS == "linux"
	p.HostMaintenance = runtime.GOOS == "linux"
	p.Firewall = runtime.GOOS == "linux"
	p.CapacityAdvice = true
	p.ResourceDefaults = true
	p.GPUAssignments = runtime.GOOS == "linux"
	p.TrafficLogs = runtime.GOOS == "linux"
	p.StructuredLogs = true
	p.AlertObservations = true
	p.ScheduledCleanup = true
	p.JournalRetention = true
	p.MountMetrics = runtime.GOOS == "linux"
	p.ManagedRegistry = runtime.GOOS == "linux"
	p.EnvironmentPreviews = runtime.GOOS == "linux"
	p.PreviewCIImages = p.EnvironmentPreviews
	p.GuardedHooks = true
	p.IngressExposures = runtime.GOOS == "linux"
	p.CloudflarePublishing = runtime.GOOS == "linux"
	p.RoutePolicies = true
	if runtime.GOOS == "linux" {
		p.RouteProtocol = 6
	}
	p.Rollouts = true
	p.CPUAutoscale = true
	p.JobRuns = true
	p.UnhealthyRestart = true
	p.Suspensions = true
	p.IdleSleep = runtime.GOOS == "linux"
	if _, err := exec.CommandContext(ctx, "docker", "buildx", "version").Output(); err == nil {
		if help, err := exec.CommandContext(ctx, "docker", "compose", "build", "--help").Output(); err == nil {
			if push, err := exec.CommandContext(ctx, "docker", "push", "--help").Output(); err == nil {
				p.BuildPublish = strings.Contains(string(help), "--builder") && strings.Contains(string(push), "--platform")
			}
		}
		if help, err := exec.CommandContext(ctx, "docker", "compose", "pull", "--help").Output(); err == nil {
			p.ImageLifecycle = strings.Contains(string(help), "--policy") && strings.Contains(string(help), "--ignore-buildable")
		}
	}
	if out, err := exec.CommandContext(ctx, "docker", "info", "--format", "{{.ServerVersion}}").Output(); err == nil {
		p.Docker = strings.TrimSpace(string(out))
	}
	if out, err := exec.CommandContext(ctx, "docker", "compose", "version", "--short").Output(); err == nil {
		p.Compose = strings.TrimSpace(string(out))
	}
	p.BuildCacheLimits = runtime.GOOS == "linux" && p.BuildPublish
	p.BuildRecipes = runtime.GOOS == "linux" && p.BuildPublish && recipeDockerIsolation(p.Docker)
	p.Nixpacks = p.BuildRecipes && NixpacksSupported()
	if p.Nixpacks {
		engine := &Engine{Root: "/var/lib/cove", Run: RunCommand}
		if err := engine.checkNixpacksPlatform(ctx, "linux/"+p.Arch); err != nil {
			p.Nixpacks, p.NixpacksReason = false, err.Error()
		}
	}
	_, buildpacksArchError := buildpacksArchitecture(p.Arch)
	p.Buildpacks = p.BuildRecipes && buildpacksArchError == nil
	if p.Buildpacks {
		engine := &Engine{Root: "/var/lib/cove", Run: RunCommand}
		capacity, err := engine.buildpacksCapacity(ctx, "linux/"+p.Arch)
		if err == nil {
			p.BuildpacksCapacity = &capacity
			err = ValidateBuildpacksCapacity(capacity)
		}
		if err != nil {
			p.BuildpacksReason = err.Error()
		}
	}
	p.RemoteBuilds = p.BuildPublish
	p.JobImageLifecycle = p.ImageLifecycle
	p.ImageUpdates = p.ImageLifecycle
	p.ImageScanning = runtime.GOOS == "linux" && scannerEnabled()
	p.AutomaticImageScans = p.ImageScanning
	p.Metrics = inspectMetrics(ctx, "/var/lib/cove")
	p.SharedHost = inspectSharedHost(ctx, RunCommand, "/")
	p.Ready = p.OS == "linux" && p.Docker != "" && p.Compose != ""
	return p
}

func Serve(ctx context.Context, out io.Writer) error {
	return json.NewEncoder(out).Encode(Inspect(ctx))
}

package worker

import (
	"context"
	"encoding/json"
	"io"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

type Probe struct {
	SSHIdentity         bool         `json:"ssh_identity"`
	FileTransfers       bool         `json:"file_transfers"`
	PostgresPITR        bool         `json:"postgres_pitr"`
	FilesystemSnapshots bool         `json:"filesystem_snapshots"`
	BackupTransfers     bool         `json:"backup_transfers"`
	StreamedBackups     bool         `json:"extended_logical_backups"`
	TrafficLogs         bool         `json:"traffic_logs"`
	CapacityAdvice      bool         `json:"capacity_advice"`
	StructuredLogs      bool         `json:"structured_logs"`
	ScheduledCleanup    bool         `json:"scheduled_cleanup"`
	JournalRetention    bool         `json:"journal_retention"`
	MountMetrics        bool         `json:"mount_metrics"`
	AlertObservations   bool         `json:"alert_observations"`
	FileBrowser         bool         `json:"file_browser"`
	BackblazeB2         bool         `json:"backblaze_b2"`
	BackupRetention     bool         `json:"backup_retention"`
	Telemetry           bool         `json:"telemetry"`
	EnvironmentPreviews bool         `json:"environment_previews"`
	GuardedHooks        bool         `json:"guarded_hooks"`
	Previews            bool         `json:"previews"`
	GuardedDeploy       bool         `json:"guarded_deploy"`
	RestoreCopies       bool         `json:"restore_copies"`
	DatabaseAccess      bool         `json:"database_access"`
	FileSecrets         bool         `json:"file_secrets"`
	ExactPromotion      bool         `json:"exact_promotion"`
	DeploymentHooks     bool         `json:"deployment_hooks"`
	RestoreDrills       bool         `json:"restore_drills"`
	Preflight           bool         `json:"preflight"`
	Metrics             *NodeMetrics `json:"metrics,omitempty"`
	SharedHost          *SharedHost  `json:"shared_host,omitempty"`
	ResourceLimits      bool         `json:"resource_limits"`
	PrivateNetworks     bool         `json:"private_networks"`
	LogicalBackups      bool         `json:"logical_backups"`
	Infrastructure      bool         `json:"infrastructure"`
	Terminal            bool         `json:"terminal"`
	SharedNetworks      bool         `json:"shared_networks"`
	PrivateLinks        bool         `json:"private_links"`
	LiveRoutes          bool         `json:"live_routes"`
	Protocol            int          `json:"protocol"`
	OS                  string       `json:"os"`
	Arch                string       `json:"arch"`
	Docker              string       `json:"docker"`
	Compose             string       `json:"compose"`
	Ready               bool         `json:"ready"`
}

func Inspect(ctx context.Context) Probe {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	p := Probe{GuardedDeploy: true, RestoreCopies: true, DatabaseAccess: runtime.GOOS == "linux", FileSecrets: true, ExactPromotion: true, DeploymentHooks: true, RestoreDrills: true, Preflight: true, ResourceLimits: true, PrivateNetworks: true, LogicalBackups: true, Infrastructure: true, Terminal: runtime.GOOS == "linux", SharedNetworks: true, PrivateLinks: runtime.GOOS == "linux", LiveRoutes: true, Protocol: 1, OS: runtime.GOOS, Arch: runtime.GOARCH}
	p.Previews = true
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
	p.CapacityAdvice = true
	p.TrafficLogs = runtime.GOOS == "linux"
	p.StructuredLogs = true
	p.AlertObservations = true
	p.ScheduledCleanup = true
	p.JournalRetention = true
	p.MountMetrics = runtime.GOOS == "linux"
	p.EnvironmentPreviews = runtime.GOOS == "linux"
	p.GuardedHooks = true
	if out, err := exec.CommandContext(ctx, "docker", "info", "--format", "{{.ServerVersion}}").Output(); err == nil {
		p.Docker = strings.TrimSpace(string(out))
	}
	if out, err := exec.CommandContext(ctx, "docker", "compose", "version", "--short").Output(); err == nil {
		p.Compose = strings.TrimSpace(string(out))
	}
	p.Metrics = inspectMetrics(ctx, "/var/lib/cove")
	p.SharedHost = inspectSharedHost(ctx, RunCommand, "/")
	p.Ready = p.OS == "linux" && p.Docker != "" && p.Compose != ""
	return p
}

func Serve(ctx context.Context, out io.Writer) error {
	return json.NewEncoder(out).Encode(Inspect(ctx))
}

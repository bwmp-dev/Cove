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
	RoutePolicies       bool         `json:"route_policies"`
	ImageLifecycle      bool         `json:"image_lifecycle"`
	BuildPublish        bool         `json:"build_publish"`
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
	p.EnvironmentPreviews = runtime.GOOS == "linux"
	p.GuardedHooks = true
	p.RoutePolicies = true
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
	p.Metrics = inspectMetrics(ctx, "/var/lib/cove")
	p.SharedHost = inspectSharedHost(ctx, RunCommand, "/")
	p.Ready = p.OS == "linux" && p.Docker != "" && p.Compose != ""
	return p
}

func Serve(ctx context.Context, out io.Writer) error {
	return json.NewEncoder(out).Encode(Inspect(ctx))
}

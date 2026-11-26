package worker

import (
	"bytes"
	"context"
	"cove/internal/outbound"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const rpcMessageLimit = 2 * 1024 * 1024

type RPCRequest struct {
	RefreshScanDatabase bool                       `json:"refresh_scan_database,omitempty"`
	BuildCache          *BuildCacheSettings        `json:"build_cache,omitempty"`
	RegistryCertificate string                     `json:"registry_certificate,omitempty"`
	Publishing          *CloudflarePublishing      `json:"publishing,omitempty"`
	Exposures           *IngressExposureRequest    `json:"exposures,omitempty"`
	Database            *DatabaseInspectionRequest `json:"database_inspection,omitempty"`
	Bucket              *ObjectBucket              `json:"object_bucket,omitempty"`
	Diff                *ReleaseComparisonRequest  `json:"release_comparison,omitempty"`
	Proxy               *outbound.Config           `json:"outbound_proxy,omitempty"`
	Adoption            *AdoptionRequest           `json:"adoption,omitempty"`
	Exec                *ExecRequest               `json:"exec,omitempty"`
	Links               *LinkConfig                `json:"links,omitempty"`
	Ingress             *IngressConfigRequest      `json:"ingress,omitempty"`
	Tunnel              *TunnelRequest             `json:"tunnel,omitempty"`
	Promotion           *PromotionExport           `json:"promotion,omitempty"`
	Registry            *Registry                  `json:"registry,omitempty"`
	Storage             *StorageLocation           `json:"storage,omitempty"`
	Terminal            *TerminalRequest           `json:"terminal,omitempty"`
	Method              string                     `json:"method"`
	Request             *Request                   `json:"request,omitempty"`
	ID                  string                     `json:"id,omitempty"`
	Snapshot            *VolumeSnapshotConfig      `json:"snapshot,omitempty"`
	Stream              *StreamRequest             `json:"stream,omitempty"`
	TrafficControl      *ProxyTrafficControl       `json:"traffic_control,omitempty"`
	CleanupControl      *CleanupControl            `json:"cleanup_control,omitempty"`
	JournalPrune        *JournalPruneRequest       `json:"journal_prune,omitempty"`
	Files               *FileRequest               `json:"files,omitempty"`
	Telemetry           *TelemetryRequest          `json:"telemetry,omitempty"`
	Move                *MoveRequest               `json:"move,omitempty"`
	Firewall            *FirewallControl           `json:"firewall,omitempty"`
	HostMaintenance     *HostMaintenanceRequest    `json:"host_maintenance,omitempty"`
	Identity            *IdentityRequest           `json:"identity,omitempty"`
}
type RPCResponse struct {
	CertificateRetries []RouteCertificateRetry     `json:"certificate_retries,omitempty"`
	Removal            *WorkloadRemovalResult      `json:"removal,omitempty"`
	ScanDatabase       *ScanDatabaseStatus         `json:"scan_database,omitempty"`
	ScanObservation    *ScanObservation            `json:"scan_observation,omitempty"`
	BuildCache         *BuildCacheStatus           `json:"build_cache,omitempty"`
	RegistryReferences []string                    `json:"registry_references,omitempty"`
	Publishing         *CloudflarePublishingStatus `json:"publishing,omitempty"`
	Exposures          *IngressExposureStatus      `json:"exposures,omitempty"`
	ExposurePlan       *IngressExposurePlan        `json:"exposure_plan,omitempty"`
	Database           *DatabaseInspection         `json:"database_inspection,omitempty"`
	Comparison         *ReleaseComparison          `json:"release_comparison,omitempty"`
	Adopted            *AdoptionRequest            `json:"adopted,omitempty"`
	ComposeProjects    []ComposeProject            `json:"compose_projects,omitempty"`
	Adoption           *AdoptionPlan               `json:"adoption,omitempty"`
	ImportedCompose    *ImportedCompose            `json:"imported_compose,omitempty"`
	Drift              *DriftReport                `json:"drift,omitempty"`
	SourceFiles        *SourceFiles                `json:"source_files,omitempty"`
	Exec               *ExecResult                 `json:"exec,omitempty"`
	Links              *LinkStatus                 `json:"links,omitempty"`
	Ingress            *IngressConfigState         `json:"ingress,omitempty"`
	IngressFile        *IngressConfigFile          `json:"ingress_file,omitempty"`
	IngressResult      *IngressConfigResult        `json:"ingress_result,omitempty"`
	RestoreCopy        *RestoreCopy                `json:"restore_copy,omitempty"`
	Images             map[string]string           `json:"images,omitempty"`
	Inventory          *PreviewInventory           `json:"inventory,omitempty"`
	Cleanup            *CleanupPreview             `json:"cleanup,omitempty"`
	Preflight          *PreflightReport            `json:"preflight,omitempty"`
	Terminal           *TerminalResponse           `json:"terminal,omitempty"`
	Releases           []Release                   `json:"releases,omitempty"`
	Backups            []BackupRecord              `json:"backups,omitempty"`
	Maintenance        *Maintenance                `json:"maintenance,omitempty"`
	Probe              *Probe                      `json:"probe,omitempty"`
	Operation          *Operation                  `json:"operation,omitempty"`
	Logs               string                      `json:"logs,omitempty"`
	Error              string                      `json:"error,omitempty"`
	Containers         []Container                 `json:"containers,omitempty"`
	Snapshot           *SnapshotPreview            `json:"snapshot,omitempty"`
	TrafficState       *ProxyTrafficState          `json:"traffic_state,omitempty"`
	TrafficPlan        *ProxyTrafficPlan           `json:"traffic_plan,omitempty"`
	Traffic            *TrafficReport              `json:"traffic,omitempty"`
	CleanupState       *CleanupState               `json:"cleanup_state,omitempty"`
	CleanupPlan        *CleanupPlan                `json:"cleanup_plan,omitempty"`
	JournalPrune       *JournalPruneResult         `json:"journal_prune,omitempty"`
	Observations       *WorkloadObservations       `json:"observations,omitempty"`
	Files              *FileResponse               `json:"files,omitempty"`
	Telemetry          *Telemetry                  `json:"telemetry,omitempty"`
	CPUScale           *CPUScaleObservation        `json:"cpu_scale,omitempty"`
	Idle               *IdleStatus                 `json:"idle,omitempty"`
	Rollout            *RolloutStatus              `json:"rollout,omitempty"`
	Suspension         *SuspensionStatus           `json:"suspension,omitempty"`
	Move               *MoveState                  `json:"move,omitempty"`
	GPUs               *GPUInventory               `json:"gpus,omitempty"`
	Firewall           *FirewallResult             `json:"firewall,omitempty"`
	HostPatches        *HostPatchStatus            `json:"host_patches,omitempty"`
	Identity           *IdentityState              `json:"identity,omitempty"`
	PITR               *PITRStatus                 `json:"pitr,omitempty"`
}

func (e *Engine) RPC(ctx context.Context, r RPCRequest) RPCResponse {
	if r.Method != "cloudflare_publishing_disable" {
		if err := e.configureOutbound(ctx, r.Proxy); err != nil {
			return RPCResponse{Error: err.Error()}
		}
	}
	switch r.Method {
	case "compose_adoption_status":
		if !workloadIdentifier.MatchString(r.ID) {
			return RPCResponse{Error: "invalid resource"}
		}
		e.adoptionMu.RLock()
		entry, ok := e.adoptedProjects[r.ID]
		e.adoptionMu.RUnlock()
		if !ok {
			return RPCResponse{}
		}
		return RPCResponse{Adopted: &AdoptionRequest{Workload: r.ID, Project: entry.Project, Fingerprint: entry.Fingerprint}}
	case "compose_discover":
		projects, err := e.ScanComposeProjects(ctx)
		if err != nil {
			return RPCResponse{Error: err.Error()}
		}
		return RPCResponse{ComposeProjects: projects}
	case "compose_plan":
		if r.Adoption == nil {
			return RPCResponse{Error: "project selection required"}
		}
		plan, err := e.PlanComposeAdoption(ctx, r.Adoption.Project)
		if err != nil {
			return RPCResponse{Error: err.Error()}
		}
		return RPCResponse{Adoption: plan}
	case "compose_adopt":
		if r.Adoption == nil {
			return RPCResponse{Error: "adoption review required"}
		}
		if err := e.AdoptCompose(ctx, *r.Adoption); err != nil {
			return RPCResponse{Error: err.Error()}
		}
		return RPCResponse{}
	case "import_compose":
		if r.Request == nil {
			return RPCResponse{Error: "import configuration required"}
		}
		result, err := e.PrepareImportedCompose(ctx, *r.Request)
		if err != nil {
			return RPCResponse{Error: err.Error()}
		}
		return RPCResponse{ImportedCompose: result}
	case "drift":
		report, err := e.Drift(ctx, r.ID)
		if err != nil {
			return RPCResponse{Error: err.Error()}
		}
		return RPCResponse{Drift: &report}
	case "source_files":
		if r.Request == nil {
			return RPCResponse{Error: "source selection required"}
		}
		files, err := e.ReadSourceFiles(ctx, *r.Request)
		if err != nil {
			return RPCResponse{Error: err.Error()}
		}
		return RPCResponse{SourceFiles: files}
	case "exec":
		if r.Exec == nil {
			return RPCResponse{Error: "command required"}
		}
		result, err := e.Exec(ctx, *r.Exec)
		if err != nil {
			return RPCResponse{Error: err.Error()}
		}
		return RPCResponse{Exec: &result}

	case "database_discard":
		if err := e.DiscardDatabaseCopy(ctx, r.ID); err != nil {
			return RPCResponse{Error: err.Error()}
		}
		return RPCResponse{}
	case "database_inspection":
		if r.Database == nil {
			return RPCResponse{Error: "database inspection required"}
		}
		result, err := e.InspectDatabase(ctx, *r.Database)
		if err != nil {
			return RPCResponse{Error: err.Error()}
		}
		return RPCResponse{Database: result}
	case "object_bucket":
		if r.Bucket == nil {
			return RPCResponse{Error: "bucket request required"}
		}
		if err := e.ProvisionObjectBucket(ctx, *r.Bucket); err != nil {
			return RPCResponse{Error: err.Error()}
		}
		return RPCResponse{}
	case "database_retry":
		if r.Request == nil || r.Request.DatabaseMaintenance == nil || r.ID != r.Request.ID {
			return RPCResponse{Error: "maintenance retry request required"}
		}
		e.mu.Lock()
		defer e.mu.Unlock()
		op, err := e.submitLocked(*r.Request)
		if err != nil {
			return RPCResponse{Error: "maintenance retry input differs from its journal"}
		}
		if op.State != "failed" && op.State != "interrupted" {
			return RPCResponse{Operation: &op}
		}
		j, err := e.read(r.ID)
		if err != nil {
			return RPCResponse{Error: "maintenance journal unavailable"}
		}
		j.Operation.State = "queued"
		j.Operation.Error = ""
		j.Operation.FinishedAt = nil
		if err = writeJSON(e.path(r.ID), j); err != nil {
			return RPCResponse{Error: "could not persist maintenance retry"}
		}
		e.pending = append(e.pending, j.Operation)
		return RPCResponse{Operation: &j.Operation}
	case "release_comparison":
		if r.Diff == nil {
			return RPCResponse{Error: "release comparison required"}
		}
		comparison, err := e.CompareReleases(ctx, *r.Diff, r.Proxy)
		if err != nil {
			return RPCResponse{Error: "retained release comparison unavailable: " + err.Error()}
		}
		return RPCResponse{Comparison: comparison}
	case "move":
		if r.Move == nil {
			return RPCResponse{Error: "move request required"}
		}
		state, err := e.Move(ctx, *r.Move)
		response := RPCResponse{Move: state}
		if err != nil {
			response.Error = err.Error()
		}
		return response
	case "gpus":
		state, err := e.GPUs(ctx)
		response := RPCResponse{GPUs: state}
		if err != nil {
			response.Error = err.Error()
		}
		return response
	case "firewall":
		if r.Firewall == nil {
			return RPCResponse{Error: "firewall options required"}
		}
		state, err := e.ConfigureFirewall(ctx, *r.Firewall)
		response := RPCResponse{Firewall: state}
		if err != nil {
			response.Error = err.Error()
		}
		return response
	case "host_maintenance":
		if r.HostMaintenance == nil {
			return RPCResponse{Error: "host maintenance options required"}
		}
		status, err := e.HostMaintenance(ctx, *r.HostMaintenance)
		response := RPCResponse{HostPatches: status}
		if err != nil {
			response.Error = err.Error()
		}
		return response
	case "pitr_status":
		status, err := e.PITRStatus(ctx, r.ID)
		if err != nil {
			return RPCResponse{Error: "PITR status unavailable; verify worker metadata and capture state"}
		}
		return RPCResponse{PITR: &status}
	case "pitr_copy_plan":
		if r.Request == nil {
			return RPCResponse{Error: "PITR copy request required"}
		}
		plan, err := e.PlanPITRCopy(ctx, *r.Request)
		if err != nil {
			return RPCResponse{Error: err.Error()}
		}
		return RPCResponse{RestoreCopy: plan}
	case "snapshot_preview":
		if r.Snapshot == nil {
			return RPCResponse{Error: "snapshot options required"}
		}
		preview, err := e.SnapshotPreview(ctx, r.ID, r.Snapshot.MaxBytes)
		if err != nil {
			return RPCResponse{Error: "snapshot inspection unavailable; check worker state, tools and privileges"}
		}
		return RPCResponse{Snapshot: &preview}
	case "traffic_control":
		if r.TrafficControl == nil {
			return RPCResponse{Error: "traffic settings required"}
		}
		state, plan, err := e.ConfigureTraffic(ctx, *r.TrafficControl)
		response := RPCResponse{TrafficState: state, TrafficPlan: plan}
		if err != nil {
			response.Error = err.Error()
		}
		return response
	case "traffic":
		if r.Telemetry == nil {
			return RPCResponse{Error: "traffic cursor required"}
		}
		report, err := e.CollectTraffic(ctx, r.Telemetry.Since)
		if err != nil {
			return RPCResponse{Error: "traffic collection unavailable"}
		}
		return RPCResponse{Traffic: report}
	case "cleanup_control":
		if r.CleanupControl == nil {
			return RPCResponse{Error: "cleanup request required"}
		}
		state, plan, err := e.CleanupPolicy(ctx, *r.CleanupControl)
		if err != nil {
			return RPCResponse{CleanupState: state, CleanupPlan: plan, Error: err.Error()}
		}
		return RPCResponse{CleanupState: state, CleanupPlan: plan}
	case "prune_journals":
		if r.JournalPrune == nil {
			return RPCResponse{Error: "journal request required"}
		}
		result, err := e.PruneJournals(ctx, *r.JournalPrune)
		if err != nil {
			return RPCResponse{JournalPrune: result, Error: "journal pruning unavailable; worker busy or metadata unsafe"}
		}
		return RPCResponse{JournalPrune: result}

	case "observations":
		result, err := e.ObserveWorkload(ctx, r.ID)
		if err != nil {
			return RPCResponse{Error: "resource observations unavailable"}
		}
		return RPCResponse{Observations: result}
	case "files":
		if r.Files == nil {
			return RPCResponse{Error: "file request required"}
		}
		result, err := e.FileOperation(ctx, r.ID, *r.Files)
		if err != nil {
			return RPCResponse{Error: err.Error()}
		}
		return RPCResponse{Files: result}
	case "identity":
		if r.Identity == nil {
			return RPCResponse{Error: "identity change required"}
		}
		result, err := e.ChangeIdentity(*r.Identity)
		if err != nil {
			return RPCResponse{Error: "SSH identity change failed; inspect its pending state and authorized key restrictions"}
		}
		return RPCResponse{Identity: result}
	case "capacity":
		if r.Request == nil {
			return RPCResponse{Error: "deployment request required"}
		}
		report := e.Capacity(ctx, *r.Request)
		return RPCResponse{Preflight: &report}
	case "live_logs":
		if r.Telemetry == nil {
			return RPCResponse{Error: "live log cursor required"}
		}
		report, err := e.CollectLiveLogs(ctx, r.ID, r.Telemetry.Since)
		if err != nil {
			return RPCResponse{Error: "live logs unavailable; check worker state or retry after other readers finish"}
		}
		return RPCResponse{Telemetry: report}
	case "telemetry":
		if r.Telemetry == nil {
			return RPCResponse{Error: "telemetry request required"}
		}
		result, err := e.CollectTelemetry(ctx, r.ID, r.Telemetry.Since)
		if err != nil {
			return RPCResponse{Error: err.Error()}
		}
		return RPCResponse{Telemetry: result}
	case "retire_image_scan":
		if r.Request == nil {
			return RPCResponse{Error: "automatic scan request required"}
		}
		if err := e.retireImageScan(ctx, *r.Request); err != nil {
			return RPCResponse{Error: "automatic scan cleanup has not settled"}
		}
		return RPCResponse{}
	case "image_scan_database":
		status, err := e.scanDatabaseStatus(r.RefreshScanDatabase)
		if err != nil {
			return RPCResponse{Error: "automatic vulnerability database state is unavailable"}
		}
		return RPCResponse{ScanDatabase: &status}
	case "image_scan_observe":
		if r.Request == nil {
			return RPCResponse{Error: "scan resource required"}
		}
		observation, err := e.observeScan(ctx, *r.Request)
		if err != nil {
			return RPCResponse{Error: "automatic scan resource state is unavailable"}
		}
		return RPCResponse{ScanObservation: &observation}
	case "build_cache":
		status := e.BuildCacheStatus(ctx)
		return RPCResponse{BuildCache: &status}
	case "build_cache_configure":
		if r.BuildCache == nil {
			return RPCResponse{Error: "build-cache settings required"}
		}
		status, err := e.ConfigureBuildCache(ctx, *r.BuildCache)
		if err != nil {
			var pathError *os.PathError
			if errors.As(err, &pathError) {
				return RPCResponse{Error: "build-cache state or cleanup could not be saved; check worker storage and permissions"}
			}
			return RPCResponse{Error: err.Error()}
		}
		return RPCResponse{BuildCache: &status}
	case "cancel_build":
		if r.Request == nil || r.Request.Action != "build" || !identifier.MatchString(r.Request.ID) || !workloadIdentifier.MatchString(r.Request.Workload) {
			return RPCResponse{Error: "build request required"}
		}
		e.mu.Lock()
		j, err := e.read(r.Request.ID)
		e.mu.Unlock()
		request := *r.Request
		if err == nil {
			if j.Request.Workload != request.Workload || j.Request.Action != "build" {
				return RPCResponse{Error: "build ownership mismatch"}
			}
			request = j.Request
		}
		op, err := e.Cancel(request)
		if err != nil {
			return RPCResponse{Error: "build cancellation remains pending"}
		}
		return RPCResponse{Operation: &op}
	case "retire_build":
		if r.Request == nil {
			return RPCResponse{Error: "build request required"}
		}
		if err := e.RetireBuild(ctx, *r.Request); err != nil {
			return RPCResponse{Error: "build cleanup has not settled"}
		}
		return RPCResponse{}
	case "certificate_retries":
		retries, err := e.CertificateRetries(r.ID)
		if err != nil {
			return RPCResponse{Error: err.Error()}
		}
		return RPCResponse{CertificateRetries: retries}
	case "managed_registry_verify":
		if err := VerifyManagedRegistry(ctx, r.Registry, r.RegistryCertificate, r.Proxy); err != nil {
			return RPCResponse{Error: err.Error()}
		}
		return RPCResponse{}
	case "managed_registry_recover":
		if !identifier.MatchString(r.ID) || r.Request == nil || r.Request.ID != r.ID || r.Request.Action != "registry_maintenance" {
			return RPCResponse{Error: "invalid registry operation"}
		}
		if _, err := e.Cancel(*r.Request); err != nil {
			return RPCResponse{Error: "registry recovery could not retire the original submission"}
		}
		e.mu.Lock()
		j, err := e.read(r.ID)
		if err != nil || j.Request.Action != "registry_maintenance" || e.busy(j.Request.Workload) {
			e.mu.Unlock()
			return RPCResponse{Error: "registry operation unavailable or active"}
		}
		e.active[j.Request.Workload] = true
		e.mu.Unlock()
		err = e.recoverManagedRegistry(ctx, j)
		e.mu.Lock()
		delete(e.active, j.Request.Workload)
		e.mu.Unlock()
		if err != nil {
			return RPCResponse{Error: "registry recovery remains pending"}
		}
		op, err := e.Status(r.ID)
		if err != nil {
			return RPCResponse{Error: "registry recovery status unavailable"}
		}
		return RPCResponse{Operation: &op}
	case "managed_registry_references":
		if r.Registry == nil {
			return RPCResponse{Error: "registry scope required"}
		}
		refs, err := e.ManagedRegistryReferences(ctx, r.ID, r.Registry.Server)
		if err != nil {
			return RPCResponse{Error: err.Error()}
		}
		return RPCResponse{RegistryReferences: refs}
	case "cloudflare_publishing", "cloudflare_publishing_apply", "cloudflare_publishing_disable":
		var status CloudflarePublishingStatus
		var err error
		if r.Method == "cloudflare_publishing" {
			status, err = e.CloudflarePublishingStatus(ctx)
		} else {
			if r.Method == "cloudflare_publishing_apply" && r.Publishing == nil {
				return RPCResponse{Error: "managed tunnel specification required"}
			}
			value := r.Publishing
			if r.Method == "cloudflare_publishing_disable" {
				value = nil
			}
			status, err = e.ApplyCloudflarePublishing(ctx, value)
		}
		if err != nil {
			return RPCResponse{Error: err.Error()}
		}
		return RPCResponse{Publishing: &status}

	case "ingress_exposures", "ingress_exposures_initialize":
		var status IngressExposureStatus
		var err error
		if r.Method == "ingress_exposures_initialize" {
			status, err = e.InitializeIngressExposures(ctx)
		} else {
			status, err = e.IngressExposures(ctx)
		}
		if err != nil {
			return RPCResponse{Error: err.Error()}
		}
		return RPCResponse{Exposures: &status}
	case "ingress_exposures_plan":
		if r.Exposures == nil {
			return RPCResponse{Error: "listener settings required"}
		}
		plan, err := e.PlanIngressExposures(ctx, r.Exposures.Settings)
		if err != nil {
			return RPCResponse{Error: err.Error()}
		}
		return RPCResponse{ExposurePlan: &plan}
	case "ingress_exposures_apply":
		if r.Exposures == nil {
			return RPCResponse{Error: "reviewed listener plan required"}
		}
		status, err := e.ChangeIngressExposures(ctx, *r.Exposures)
		if err != nil {
			return RPCResponse{Error: err.Error()}
		}
		return RPCResponse{Exposures: &status}
	case "cpu_scale_sample":
		sample, err := e.CPUScaleSample(ctx, r.ID)
		if err != nil {
			return RPCResponse{Error: err.Error()}
		}
		return RPCResponse{CPUScale: sample}
	case "link_identity":
		fingerprint, err := e.LinkIdentity()
		if err != nil {
			return RPCResponse{Error: "could not prepare the private link identity: " + err.Error()}
		}
		return RPCResponse{Links: &LinkStatus{Fingerprint: fingerprint}}
	case "links_apply":
		if r.Links == nil {
			return RPCResponse{Error: "private link configuration required"}
		}
		status, err := e.ApplyLinks(ctx, *r.Links)
		if err != nil {
			return RPCResponse{Error: "private links could not be applied: " + err.Error()}
		}
		return RPCResponse{Links: &status}
	case "ingress_config":
		state, err := e.IngressConfig(ctx)
		if err != nil {
			return RPCResponse{Error: err.Error()}
		}
		return RPCResponse{Ingress: &state}
	case "ingress_config_file":
		if r.Ingress == nil {
			return RPCResponse{Error: "request required"}
		}
		file, err := e.IngressConfigFile(ctx, r.Ingress.Name)
		if err != nil {
			return RPCResponse{Error: err.Error()}
		}
		return RPCResponse{IngressFile: &file}
	case "ingress_config_change":
		if r.Ingress == nil {
			return RPCResponse{Error: "request required"}
		}
		result := e.ChangeIngressConfig(ctx, *r.Ingress)
		return RPCResponse{IngressResult: &result}
	case "restore_copy_plan":
		if r.Request == nil {
			return RPCResponse{Error: "request required"}
		}
		plan, err := e.PlanRestoreCopy(ctx, *r.Request, r.ID)
		if err != nil {
			return RPCResponse{Error: err.Error()}
		}
		return RPCResponse{RestoreCopy: plan}
	case "tunnel_open":
		if r.Tunnel == nil {
			return RPCResponse{Error: "access request required"}
		}
		if err := e.OpenTunnel(ctx, *r.Tunnel); err != nil {
			return RPCResponse{Error: err.Error()}
		}
		return RPCResponse{}
	case "tunnel_close":
		if err := e.CloseTunnel(r.ID); err != nil {
			return RPCResponse{Error: err.Error()}
		}
		return RPCResponse{}
	case "environment_preview_inventory":
		var database *DatabaseBackup
		if r.Request != nil && r.Request.Backup != nil {
			database = r.Request.Backup.Database
		}
		inventory, err := e.InventoryPreviewSource(ctx, r.ID, database)
		if err != nil {
			return RPCResponse{Error: err.Error()}
		}
		return RPCResponse{Inventory: inventory}
	case "export_release":
		if r.Promotion == nil {
			return RPCResponse{Error: "promotion source required"}
		}
		images, err := e.ExportRelease(ctx, *r.Promotion)
		if err != nil {
			return RPCResponse{Error: err.Error()}
		}
		return RPCResponse{Images: images}
	case "cleanup_preview":
		preview, err := e.PreviewCleanup(ctx)
		if err != nil {
			return RPCResponse{Error: "cleanup preview unavailable: " + err.Error()}
		}
		return RPCResponse{Cleanup: &preview}
	case "preflight":
		if r.Request == nil {
			return RPCResponse{Error: "request required"}
		}
		report := e.Preflight(ctx, *r.Request)
		return RPCResponse{Preflight: &report}
	case "registry_test":
		if err := e.TestRegistry(ctx, r.Registry); err != nil {
			return RPCResponse{Error: err.Error()}
		}
		return RPCResponse{}
	case "storage_test":
		if err := e.TestStorage(ctx, r.Storage); err != nil {
			return RPCResponse{Error: err.Error()}
		}
		return RPCResponse{}
	case "terminal_open", "terminal_read", "terminal_write", "terminal_close":
		result, err := e.terminal(r.Method, r.Terminal)
		if err != nil {
			return RPCResponse{Error: err.Error()}
		}
		return RPCResponse{Terminal: result}
	case "remove_workload":
		result, err := e.RemoveWorkload(ctx, r.ID)
		if err != nil {
			return RPCResponse{Error: err.Error()}
		}
		return RPCResponse{Removal: result}
	case "releases":
		releases, err := e.Releases(r.ID)
		if err != nil {
			return RPCResponse{Error: "could not read retained releases"}
		}
		return RPCResponse{Releases: releases}
	case "backups":
		backups, err := e.Backups(r.ID)
		if err != nil {
			return RPCResponse{Error: "could not read backups"}
		}
		return RPCResponse{Backups: backups}
	case "cancel":
		if r.Request == nil {
			return RPCResponse{Error: "request required"}
		}
		op, err := e.Cancel(*r.Request)
		if err != nil {
			return RPCResponse{Error: "could not cancel operation"}
		}
		return RPCResponse{Operation: &op}
	case "drain", "resume":
		state, err := e.Maintenance(r.Method == "drain")
		if err != nil {
			return RPCResponse{Error: "could not persist maintenance state"}
		}
		return RPCResponse{Maintenance: &state}
	case "container_health":
		containers, err := e.HealthContainers(ctx, r.ID)
		if err != nil {
			return RPCResponse{Error: "could not inspect resource health"}
		}
		suspension, err := e.suspensionStatus(r.ID)
		if err != nil {
			return RPCResponse{Error: "could not inspect resource suspension"}
		}
		return RPCResponse{Containers: containers, Suspension: suspension}
	case "containers":
		containers, err := e.HealthContainers(ctx, r.ID)
		if err != nil {
			return RPCResponse{Error: "could not inspect workload containers"}
		}
		if err := e.containerMetrics(ctx, containers); err != nil {
			for i := range containers {
				containers[i].MetricsUnavailable = true
			}
		}
		rollout, err := e.rolloutStatus(r.ID)
		if err != nil {
			return RPCResponse{Error: "could not inspect deployment traffic state"}
		}
		suspension, err := e.suspensionStatus(r.ID)
		if err != nil {
			return RPCResponse{Error: "could not inspect resource suspension"}
		}
		idle, err := e.IdleStatus(ctx, r.ID)
		if err != nil {
			return RPCResponse{Error: "could not inspect idle policy"}
		}
		return RPCResponse{Containers: containers, Rollout: rollout, Suspension: suspension, Idle: idle}
	case "container_logs":
		logs, err := e.ContainerLogs(ctx, r.ID)
		if err != nil {
			return RPCResponse{Error: "could not read workload logs; check that it has deployed"}
		}
		return RPCResponse{Logs: logs}
	case "probe":
		p := Inspect(ctx)
		return RPCResponse{Probe: &p}
	case "submit_automatic_image_scan", "submit_build_recipe", "submit_build_v2", "submit_image_updates", "submit", "submit_cpu_scale", "submit_routes_v2", "submit_routes_v3", "submit_routes_v4", "submit_routes_v5", "submit_routes_v6":
		if r.Request == nil {
			return RPCResponse{Error: "request required"}
		}
		expected := "submit"
		if r.Request.BuildPlatform != "" {
			expected = "submit_build_v2"
		}
		if r.Request.ImageUpdates != nil {
			expected = "submit_image_updates"
		}
		if r.Request.Scale != nil {
			expected = "submit_cpu_scale"
		}
		if HasRouteExtensions(r.Request.Routes) {
			expected = "submit_routes_v2"
		}
		if RouteProtocol(r.Request.Routes) >= 3 {
			expected = "submit_routes_v3"
		}
		if RouteProtocol(r.Request.Routes) >= 4 {
			expected = "submit_routes_v4"
		}
		if RouteProtocol(r.Request.Routes) >= 5 {
			expected = "submit_routes_v5"
		}
		if RouteProtocol(r.Request.Routes) >= 6 {
			expected = "submit_routes_v6"
		}
		if r.Request.BuildRecipe != nil {
			expected = "submit_build_recipe"
		}
		if r.Request.AutomaticScan != nil {
			expected = "submit_automatic_image_scan"
		}
		if r.Method != expected {
			return RPCResponse{Error: "request requires its dedicated submission method"}
		}
		op, err := e.Submit(*r.Request)
		if err != nil {
			return RPCResponse{Error: err.Error()}
		}
		return RPCResponse{Operation: &op}
	case "status":
		op, err := e.Status(r.ID)
		if errors.Is(err, os.ErrNotExist) {
			return RPCResponse{Error: "operation not found"}
		}
		if err != nil {
			return RPCResponse{Error: "could not read operation"}
		}
		return RPCResponse{Operation: &op}
	case "logs":
		out, err := e.Logs(r.ID)
		if err != nil {
			return RPCResponse{Error: "could not read logs"}
		}
		return RPCResponse{Logs: out}
	default:
		return RPCResponse{Error: "unsupported method"}
	}
}
func (e *Engine) Listen(ctx context.Context, socket string) error {
	unlock, err := lockWorker(e.Root)
	if err != nil {
		return err
	}
	defer unlock()
	// The helper owns this root-only scope; an unprivileged daemon cannot rule out a pending checkpoint.
	if _, err := os.Lstat(filepath.Join(e.Root, "host-maintenance", "firewall-arming.json")); err == nil || os.IsPermission(err) {
		cleanup, cancelCleanup := context.WithTimeout(ctx, time.Minute)
		_, err := e.runFirewallHost(cleanup, FirewallControl{Action: "recover"})
		cancelCleanup()
		if err != nil {
			return fmt.Errorf("firewall arming recovery: %w", err)
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	recovery, cancelRecovery := context.WithTimeout(ctx, time.Minute)
	err = e.recoverBackupTransfersOnStartup(recovery)
	if err == nil {
		err = e.recoverFilesystemSnapshots(recovery)
	}
	cancelRecovery()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(socket), 0700); err != nil {
		return err
	}
	if err := os.Chmod(filepath.Dir(socket), 0750); err != nil {
		return err
	}
	// A live daemon owns its socket; never unlink it to replace another instance.
	if conn, err := net.DialTimeout("unix", socket, time.Second); err == nil {
		conn.Close()
		return errors.New("worker daemon is already running")
	}
	if err := os.Remove(socket); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	listener, err := net.Listen("unix", socket)
	if err != nil {
		return err
	}
	defer listener.Close()
	if err = os.Chmod(socket, 0660); err != nil {
		return err
	}
	server := &http.Server{ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 30 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "CONNECT" && strings.HasPrefix(r.URL.Path, "/tunnel/") {
			e.serveTunnel(w, r)
			return
		}
		if r.Method != "POST" || r.URL.Path != "/rpc" {
			http.NotFound(w, r)
			return
		}
		request, input, err := decodeRPCFrame(r.Body)
		if err != nil {
			http.Error(w, "invalid request", 400)
			return
		}
		if request.Method == "stream" {
			if err := e.configureOutbound(r.Context(), request.Proxy); err != nil {
				w.Header().Set("Content-Type", "application/octet-stream")
				_ = json.NewEncoder(w).Encode(StreamResponse{Error: err.Error()})
				return
			}
			e.serveStream(w, r, request.Stream, input)
			return
		}
		if timeout := RPCTimeout(request.Method); timeout > 30*time.Second {
			_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(timeout))
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(e.RPC(r.Context(), request))
	})}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	engineResult := make(chan error, 1)
	go func() { engineResult <- e.Serve(runCtx); server.Close() }()
	stop := context.AfterFunc(ctx, func() { server.Close() })
	defer stop()
	err = server.Serve(listener)
	cancel()
	engineErr := <-engineResult
	if engineErr != nil {
		return engineErr
	}
	if err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

// RPCTimeout bounds one worker call. Custom proxy configuration changes include validation in a
// separate Traefik and confirmation against the live proxy, which can each take tens of seconds.
func RPCTimeout(method string) time.Duration {
	if method == "release_comparison" {
		return 40 * time.Second
	}
	switch method {
	case "host_maintenance", "firewall", "gpus":
		return 90 * time.Second
	case "stream", "move":
		return 30 * time.Minute
	case "cloudflare_publishing_apply", "cloudflare_publishing_disable", "ingress_config_change", "ingress_exposures_apply":
		return 5 * time.Minute
	case "links_apply", "traffic_control":
		return 4 * time.Minute
	case "build_cache_configure", "ingress_exposures_initialize", "ingress_exposures_plan", "compose_plan", "compose_adopt", "source_files", "storage_test", "preflight", "export_release", "ingress_config", "ingress_config_file", "environment_preview_inventory":
		return 3 * time.Minute
	}
	return 30 * time.Second
}

func Proxy(ctx context.Context, socket string, in io.Reader, out io.Writer) error {
	rpc, stream, err := decodeRPCFrame(in)
	if err != nil {
		return err
	}
	data, err := json.Marshal(rpc)
	if err != nil {
		return err
	}
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}}
	defer transport.CloseIdleConnections()

	client := &http.Client{Transport: transport, Timeout: RPCTimeout(rpc.Method)}
	var body io.Reader = bytes.NewReader(data)
	if rpc.Method == "stream" {
		body = io.MultiReader(bytes.NewReader(append(data, '\n')), stream)
	}
	request, err := http.NewRequestWithContext(ctx, "POST", "http://worker/rpc", body)
	if err != nil {
		return err
	}
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		return errors.New("worker rejected request")
	}
	limit := int64(2 << 20)
	if rpc.Method == "stream" {
		limit = MaxTransferBytes + (64 << 10)
	}
	n, err := io.Copy(out, io.LimitReader(response.Body, limit+1))
	if err == nil && n > limit {
		err = errors.New("worker response exceeds limit")
	}
	return err
}

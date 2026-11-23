package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type RPCRequest struct {
	Identity       *IdentityRequest      `json:"identity,omitempty"`
	Snapshot       *VolumeSnapshotConfig `json:"snapshot,omitempty"`
	Stream         *StreamRequest        `json:"stream,omitempty"`
	TrafficControl *ProxyTrafficControl  `json:"traffic_control,omitempty"`
	CleanupControl *CleanupControl       `json:"cleanup_control,omitempty"`
	JournalPrune   *JournalPruneRequest  `json:"journal_prune,omitempty"`
	Files          *FileRequest          `json:"files,omitempty"`
	Telemetry      *TelemetryRequest     `json:"telemetry,omitempty"`
	Links          *LinkConfig           `json:"links,omitempty"`
	Ingress        *IngressConfigRequest `json:"ingress,omitempty"`
	Tunnel         *TunnelRequest        `json:"tunnel,omitempty"`
	Promotion      *PromotionExport      `json:"promotion,omitempty"`
	Registry       *Registry             `json:"registry,omitempty"`
	Storage        *StorageLocation      `json:"storage,omitempty"`
	Terminal       *TerminalRequest      `json:"terminal,omitempty"`
	Method         string                `json:"method"`
	Request        *Request              `json:"request,omitempty"`
	ID             string                `json:"id,omitempty"`
}
type RPCResponse struct {
	Identity      *IdentityState        `json:"identity,omitempty"`
	PITR          *PITRStatus           `json:"pitr,omitempty"`
	Snapshot      *SnapshotPreview      `json:"snapshot,omitempty"`
	TrafficState  *ProxyTrafficState    `json:"traffic_state,omitempty"`
	TrafficPlan   *ProxyTrafficPlan     `json:"traffic_plan,omitempty"`
	Traffic       *TrafficReport        `json:"traffic,omitempty"`
	CleanupState  *CleanupState         `json:"cleanup_state,omitempty"`
	CleanupPlan   *CleanupPlan          `json:"cleanup_plan,omitempty"`
	JournalPrune  *JournalPruneResult   `json:"journal_prune,omitempty"`
	Observations  *WorkloadObservations `json:"observations,omitempty"`
	Files         *FileResponse         `json:"files,omitempty"`
	Telemetry     *Telemetry            `json:"telemetry,omitempty"`
	Links         *LinkStatus           `json:"links,omitempty"`
	Ingress       *IngressConfigState   `json:"ingress,omitempty"`
	IngressFile   *IngressConfigFile    `json:"ingress_file,omitempty"`
	IngressResult *IngressConfigResult  `json:"ingress_result,omitempty"`
	RestoreCopy   *RestoreCopy          `json:"restore_copy,omitempty"`
	Images        map[string]string     `json:"images,omitempty"`
	Inventory     *PreviewInventory     `json:"inventory,omitempty"`
	Cleanup       *CleanupPreview       `json:"cleanup,omitempty"`
	Preflight     *PreflightReport      `json:"preflight,omitempty"`
	Terminal      *TerminalResponse     `json:"terminal,omitempty"`
	Releases      []Release             `json:"releases,omitempty"`
	Backups       []BackupRecord        `json:"backups,omitempty"`
	Maintenance   *Maintenance          `json:"maintenance,omitempty"`
	Probe         *Probe                `json:"probe,omitempty"`
	Operation     *Operation            `json:"operation,omitempty"`
	Logs          string                `json:"logs,omitempty"`
	Error         string                `json:"error,omitempty"`
	Containers    []Container           `json:"containers,omitempty"`
}

func (e *Engine) RPC(ctx context.Context, r RPCRequest) RPCResponse {
	switch r.Method {
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
		if err := e.RemoveWorkload(ctx, r.ID); err != nil {
			return RPCResponse{Error: err.Error()}
		}
		return RPCResponse{}
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
		return RPCResponse{Containers: containers}
	case "containers":
		containers, err := e.Containers(ctx, r.ID)
		if err != nil {
			return RPCResponse{Error: "could not inspect workload containers"}
		}
		if err := e.containerMetrics(ctx, containers); err != nil {
			for i := range containers {
				containers[i].MetricsUnavailable = true
			}
		}
		return RPCResponse{Containers: containers}
	case "container_logs":
		logs, err := e.ContainerLogs(ctx, r.ID)
		if err != nil {
			return RPCResponse{Error: "could not read workload logs; check that it has deployed"}
		}
		return RPCResponse{Logs: logs}
	case "probe":
		p := Inspect(ctx)
		return RPCResponse{Probe: &p}
	case "submit":
		if r.Request == nil {
			return RPCResponse{Error: "request required"}
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
	switch method {
	case "stream":
		return 30 * time.Minute
	case "ingress_config_change":
		return 5 * time.Minute
	case "links_apply", "traffic_control":
		return 4 * time.Minute
	case "storage_test", "preflight", "export_release", "ingress_config", "ingress_config_file", "environment_preview_inventory":
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

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

const rpcMessageLimit = 2 * 1024 * 1024

type RPCRequest struct {
	Exposures *IngressExposureRequest `json:"exposures,omitempty"`
	Links     *LinkConfig             `json:"links,omitempty"`
	Ingress   *IngressConfigRequest   `json:"ingress,omitempty"`
	Tunnel    *TunnelRequest          `json:"tunnel,omitempty"`
	Promotion *PromotionExport        `json:"promotion,omitempty"`
	Registry  *Registry               `json:"registry,omitempty"`
	Storage   *StorageLocation        `json:"storage,omitempty"`
	Terminal  *TerminalRequest        `json:"terminal,omitempty"`
	Method    string                  `json:"method"`
	Request   *Request                `json:"request,omitempty"`
	ID        string                  `json:"id,omitempty"`
}
type RPCResponse struct {
	Exposures     *IngressExposureStatus `json:"exposures,omitempty"`
	ExposurePlan  *IngressExposurePlan   `json:"exposure_plan,omitempty"`
	CPUScale      *CPUScaleObservation   `json:"cpu_scale,omitempty"`
	Idle          *IdleStatus            `json:"idle,omitempty"`
	Rollout       *RolloutStatus         `json:"rollout,omitempty"`
	Suspension    *SuspensionStatus      `json:"suspension,omitempty"`
	Links         *LinkStatus            `json:"links,omitempty"`
	Ingress       *IngressConfigState    `json:"ingress,omitempty"`
	IngressFile   *IngressConfigFile     `json:"ingress_file,omitempty"`
	IngressResult *IngressConfigResult   `json:"ingress_result,omitempty"`
	RestoreCopy   *RestoreCopy           `json:"restore_copy,omitempty"`
	Images        map[string]string      `json:"images,omitempty"`
	Inventory     *PreviewInventory      `json:"inventory,omitempty"`
	Cleanup       *CleanupPreview        `json:"cleanup,omitempty"`
	Preflight     *PreflightReport       `json:"preflight,omitempty"`
	Terminal      *TerminalResponse      `json:"terminal,omitempty"`
	Releases      []Release              `json:"releases,omitempty"`
	Backups       []BackupRecord         `json:"backups,omitempty"`
	Maintenance   *Maintenance           `json:"maintenance,omitempty"`
	Probe         *Probe                 `json:"probe,omitempty"`
	Operation     *Operation             `json:"operation,omitempty"`
	Logs          string                 `json:"logs,omitempty"`
	Error         string                 `json:"error,omitempty"`
	Containers    []Container            `json:"containers,omitempty"`
}

func (e *Engine) RPC(ctx context.Context, r RPCRequest) RPCResponse {
	switch r.Method {
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
	case "submit", "submit_cpu_scale", "submit_routes_v2", "submit_routes_v3", "submit_routes_v4", "submit_routes_v5":
		if r.Request == nil {
			return RPCResponse{Error: "request required"}
		}
		expected := "submit"
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
		var request RPCRequest
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, rpcMessageLimit))
		decoder.DisallowUnknownFields()
		if decoder.Decode(&request) != nil {
			http.Error(w, "invalid request", 400)
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
	case "ingress_config_change", "ingress_exposures_apply":
		return 5 * time.Minute
	case "links_apply":
		return 4 * time.Minute
	case "ingress_exposures_initialize", "ingress_exposures_plan", "storage_test", "preflight", "export_release", "ingress_config", "ingress_config_file", "environment_preview_inventory":
		return 3 * time.Minute
	}
	return 30 * time.Second
}

func Proxy(ctx context.Context, socket string, in io.Reader, out io.Writer) error {
	data, err := io.ReadAll(io.LimitReader(in, rpcMessageLimit+1))
	if err != nil {
		return err
	}
	if len(data) > rpcMessageLimit {
		return errors.New("request too large")
	}
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}}
	defer transport.CloseIdleConnections()
	var rpc RPCRequest
	if err = json.Unmarshal(data, &rpc); err != nil {
		return err
	}
	client := &http.Client{Transport: transport, Timeout: RPCTimeout(rpc.Method)}
	request, err := http.NewRequestWithContext(ctx, "POST", "http://worker/rpc", bytes.NewReader(data))
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
	_, err = io.Copy(out, io.LimitReader(response.Body, rpcMessageLimit))
	return err
}

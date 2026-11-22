package worker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

var identifier = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)
var workloadIdentifier = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)
var environmentIdentifier = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
var serviceIdentifier = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,63}$`)

type Request struct {
	RecordDrift            bool                     `json:"record_drift,omitempty"`
	ComposeBundle          *ComposeBundle           `json:"compose_bundle,omitempty"`
	BuiltinDefaults        map[string]string        `json:"builtin_defaults,omitempty"`
	EnvironmentPreview     *EnvironmentPreview      `json:"environment_preview,omitempty"`
	Preview                bool                     `json:"preview,omitempty"`
	GuardedDeploy          *bool                    `json:"guarded_deploy,omitempty"`
	RestoreCopy            *RestoreCopy             `json:"restore_copy,omitempty"`
	FileSecrets            []FileSecret             `json:"file_secrets,omitempty"`
	ImagePins              map[string]string        `json:"image_pins,omitempty"`
	Hooks                  *DeploymentHooks         `json:"hooks,omitempty"`
	Limits                 map[string]ServiceLimits `json:"limits,omitempty"`
	Private                *PrivateNetwork          `json:"private,omitempty"`
	InfrastructurePrepared bool                     `json:"infrastructure_prepared,omitempty"`
	Registry               *Registry                `json:"registry,omitempty"`
	Networks               []NetworkAttachment      `json:"networks,omitempty"`
	Backup                 *BackupConfig            `json:"backup,omitempty"`
	BackupID               string                   `json:"backup_id,omitempty"`
	Routes                 []Route                  `json:"routes,omitempty"`
	Release                string                   `json:"release,omitempty"`
	ID                     string                   `json:"id"`
	Workload               string                   `json:"workload"`
	Action                 string                   `json:"action"`
	Compose                string                   `json:"compose,omitempty"`
	Environment            map[string]string        `json:"environment,omitempty"`
	Source                 *GitSource               `json:"source,omitempty"`
	Kind                   string                   `json:"kind,omitempty"`
	JobService             string                   `json:"job_service,omitempty"`
}
type Operation struct {
	ID         string         `json:"id"`
	Workload   string         `json:"workload"`
	Action     string         `json:"action"`
	State      string         `json:"state"`
	CreatedAt  time.Time      `json:"created_at"`
	FinishedAt *time.Time     `json:"finished_at,omitempty"`
	Error      string         `json:"error,omitempty"`
	Revision   string         `json:"revision,omitempty"`
	Preview    *PreviewReport `json:"environment_preview,omitempty"`
}
type journal struct {
	CancelRequested bool         `json:"cancel_requested,omitempty"`
	RouteChange     *routeChange `json:"route_change,omitempty"`
	Operation       Operation    `json:"operation"`
	Request         Request      `json:"request"`
	Digest          string       `json:"digest"`
}

// routeChange is journaled by a route operation before it changes the proxy. Until Committed, the
// operation's outcome is the previous routes; once Committed, it is the requested routes.
type routeChange struct {
	Previous  []Route `json:"previous"`
	Committed bool    `json:"committed,omitempty"`
}

// routeRecovery tracks a running route operation without an executor whose outcome must still be
// settled from its journal: restored to the previous routes, or completed once committed.
type routeRecovery struct {
	workload string
	state    string
	next     time.Time
	running  bool
}
type deployedState struct {
	AdditionalSecrets []string            `json:"additional_secrets,omitempty"`
	PublicBuiltins    map[string]string   `json:"public_builtins,omitempty"`
	Private           *PrivateNetwork     `json:"private,omitempty"`
	Networks          []NetworkAttachment `json:"networks,omitempty"`
	Routes            []Route             `json:"routes,omitempty"`
	Revision          string              `json:"revision,omitempty"`
	ComposePath       string              `json:"compose_path"`
	Environment       map[string]string   `json:"environment"`
}
type CommandRunner func(context.Context, string, []string, []string, io.Writer) error
type Engine struct {
	adoptionMu      sync.RWMutex
	adoptedProjects map[string]adoptedProject
	execRunning     int
	// Stream runs a command with separate output streams; tests replace it.
	Stream        func(context.Context, []string, io.Writer, io.Writer) error
	previewDataMu sync.Mutex
	tunnelMu      sync.Mutex
	tunnels       map[string]*tunnelLease
	preflightMu   sync.Mutex
	buildMu       sync.Mutex
	storageMu     sync.Mutex
	terminalMu    sync.Mutex
	terminals     map[string]*terminalSession
	ACMEDirectory string
	ACMERoot      string
	networkMu     sync.Mutex
	linkMu        sync.Mutex
	linkKick      chan struct{}
	backupMu      sync.Mutex
	ingressMu     sync.Mutex
	Root          string
	Run           CommandRunner
	mu            sync.Mutex
	active        map[string]bool
	pending       []Operation
	fatal         chan error
	draining      bool
	cancels       map[string]context.CancelFunc
	recovering    map[string]routeRecovery
	// A custom proxy configuration change that did not commit is restored until it settles.
	customUnsettled bool
	customRunning   bool
	customRetry     time.Time
}

func RunCommand(ctx context.Context, dir string, args, environment []string, out io.Writer) error {
	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	configureCommand(cmd)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), environment...)
	cmd.Stdout = out
	cmd.Stderr = out
	return cmd.Run()
}
func NewEngine(root string) (*Engine, error) {
	if err := os.MkdirAll(filepath.Join(root, "operations"), 0700); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(root, "workloads"), 0700); err != nil {
		return nil, err
	}
	e := &Engine{Root: root, Run: RunCommand, active: map[string]bool{}, cancels: map[string]context.CancelFunc{}, recovering: map[string]routeRecovery{}, fatal: make(chan error, 1), linkKick: make(chan struct{}, 1)}
	if err := e.loadAdoptedProjects(); err != nil {
		return nil, err
	}
	if _, err := os.Stat(filepath.Join(root, "draining.json")); err == nil {
		e.draining = true
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	entries, err := os.ReadDir(filepath.Join(root, "operations"))
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		j, err := e.read(strings.TrimSuffix(entry.Name(), ".json"))
		if err != nil {
			return nil, err
		}
		if j.Operation.State == "queued" {
			e.pending = append(e.pending, j.Operation)
		}
	}
	return e, nil
}
func ValidateRequest(r Request) error {
	if r.Preview && (r.Private != nil || len(r.Networks) > 0 || r.Hooks != nil || len(r.FileSecrets) > 0 || r.Registry != nil || len(r.ImagePins) > 0 || len(r.Limits) > 0 || r.Backup != nil || r.Kind == "job" || (r.Source != nil && r.Source.Revision == "")) {
		return errors.New("preview deployment must use a pinned revision and isolated configuration")
	}
	if r.GuardedDeploy != nil && *r.GuardedDeploy && r.Kind == "job" {
		return errors.New("guarded deployment requires a service")
	}
	if err := validateEnvironmentPreview(r); err != nil {
		return err
	}
	if err := ValidateFileSecrets(r.FileSecrets); err != nil {
		return err
	}
	if err := ValidateLimits(r.Limits); err != nil {
		return err
	}
	if err := ValidatePrivateNetwork(r.Private); err != nil {
		return err
	}
	if r.Private != nil && (len(r.Routes) > 0 || r.Kind == "job") {
		return errors.New("private access requires a service without public routes")
	}
	if err := ValidateRegistry(r.Registry); err != nil {
		return err
	}
	if err := validateImagePins(r.ImagePins); err != nil {
		return err
	}
	if err := validateHooks(r.Hooks, r.Kind); err != nil {
		return err
	}
	if err := ValidateNetworks(r.Networks); err != nil {
		return err
	}
	if err := ValidateBackup(r.Backup); err != nil {
		return err
	}
	if err := ValidateRoutes(r.Routes, r.Kind); err != nil {
		return err
	}
	if !identifier.MatchString(r.ID) || !workloadIdentifier.MatchString(r.Workload) {
		return errors.New("invalid operation or workload ID")
	}
	if err := ValidateComposeBundle(r.ComposeBundle, r.Source, r.Preview); err != nil {
		return err
	}
	if r.ComposeBundle != nil {
		encoded, err := json.Marshal(r)
		if err != nil || len(encoded) > (2<<20)-(16<<10) {
			return errors.New("Compose bundle and environment exceed the worker request limit")
		}
	}
	switch r.Action {
	case "preview_data", "environment_preview_remove":
	case "preview_remove":
		if !r.Preview {
			return errors.New("preview ownership required")
		}
	case "restore_copy":
		if r.RestoreCopy == nil || r.Backup == nil || !workloadIdentifier.MatchString(r.RestoreCopy.Source) || r.RestoreCopy.Source == r.Workload || !identifier.MatchString(r.BackupID) || len(r.Compose) == 0 || len(r.Compose) > 1024*1024 {
			return errors.New("invalid restore copy")
		}
	case "deploy":
		if (r.Source == nil || r.Source.InlineCompose) && (len(r.Compose) == 0 || len(r.Compose) > 1024*1024) {
			return errors.New("compose is required and must not exceed 1 MiB")
		}
		if r.Preview && r.Source != nil && r.Source.InlineCompose {
			return errors.New("previews require a repository Compose file")
		}
	case "stop", "start", "restart":
	case "routes":
		if r.Kind == "job" {
			return errors.New("only services can have routes")
		}
	case "backup", "restore", "drill":
		if r.Backup == nil || r.Kind == "job" {
			return errors.New("configure backups for a service before using backup or restore")
		}
		if (r.Action == "restore" || r.Action == "drill") && !identifier.MatchString(r.BackupID) {
			return errors.New("backup ID is required")
		}
	case "rollback":
		if !identifier.MatchString(r.Release) || r.Kind == "job" {
			return errors.New("rollback requires a service release ID")
		}
	default:
		return errors.New("unsupported operation")
	}
	if err := validateSource(r.Source); err != nil {
		return err
	}
	if r.Kind != "" && r.Kind != "service" && r.Kind != "job" {
		return errors.New("unsupported workload kind")
	}
	if r.Kind == "job" {
		if !serviceIdentifier.MatchString(r.JobService) {
			return errors.New("job_service must name a Compose service")
		}
		if r.Action == "start" || r.Action == "restart" {
			return errors.New("deploy a new operation to run a job again")
		}
	}
	if err := validateBuiltins(r.BuiltinDefaults); err != nil {
		return err
	}
	for k, v := range r.Environment {
		if !environmentIdentifier.MatchString(k) || strings.ContainsRune(v, 0) || len(v) > 65536 {
			return errors.New("invalid environment variable")
		}
		if k == "PATH" || strings.HasPrefix(k, "DOCKER_") || strings.HasPrefix(k, "COMPOSE_") || strings.HasPrefix(k, "LD_") {
			return errors.New("reserved environment variable")
		}
	}
	return nil
}
func writeJSON(path string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".write-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err = tmp.Chmod(0600); err == nil {
		_, err = tmp.Write(data)
	}
	if err == nil {
		err = tmp.Sync()
	}
	closeErr := tmp.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err = os.Rename(name, path); err != nil {
		return err
	}
	return syncDirectory(filepath.Dir(path))
}
func (e *Engine) path(id string) string { return filepath.Join(e.Root, "operations", id+".json") }
func (e *Engine) read(id string) (journal, error) {
	var j journal
	if !identifier.MatchString(id) {
		return j, errors.New("invalid operation ID")
	}
	b, err := os.ReadFile(e.path(id))
	if err != nil {
		return j, err
	}
	err = json.Unmarshal(b, &j)
	return j, err
}
func (e *Engine) Submit(r Request) (Operation, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.submitLocked(r)
}
func (e *Engine) submitLocked(r Request) (Operation, error) {
	if err := ValidateRequest(r); err != nil {
		return Operation{}, err
	}
	data, _ := json.Marshal(r)
	sum := sha256.Sum256(data)
	digest := hex.EncodeToString(sum[:])
	old, err := e.read(r.ID)
	if err == nil {
		if old.Digest != digest {
			return Operation{}, errors.New("operation ID already has different input")
		}
		return old.Operation, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return Operation{}, err
	}
	j := journal{Operation: Operation{ID: r.ID, Workload: r.Workload, Action: r.Action, State: "queued", CreatedAt: time.Now().UTC()}, Request: r, Digest: digest}
	if err = writeJSON(e.path(r.ID), j); err != nil {
		return Operation{}, err
	}
	e.pending = append(e.pending, j.Operation)
	return j.Operation, nil
}
func (e *Engine) Status(id string) (Operation, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	j, err := e.read(id)
	return j.Operation, err
}
func (e *Engine) Logs(id string) (string, error) {
	if !identifier.MatchString(id) {
		return "", errors.New("invalid operation ID")
	}
	f, err := os.Open(filepath.Join(e.Root, "operations", id+".log"))
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return "", err
	}
	if info.Size() > 65536 {
		if _, err = f.Seek(-65536, io.SeekEnd); err != nil {
			return "", err
		}
	}
	data, err := io.ReadAll(io.LimitReader(f, 65536))
	return string(data), err
}
func (e *Engine) Recover() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if state, err := e.loadCustomIngress(); err != nil {
		fmt.Fprintln(os.Stderr, "Custom proxy configuration state is unreadable; custom configuration changes and new resource hostnames are refused until it is repaired:", err)
	} else if unsettledCustom(state.Change) {
		e.customUnsettled = true
	}
	entries, err := os.ReadDir(filepath.Join(e.Root, "operations"))
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		id := strings.TrimSuffix(entry.Name(), ".json")
		j, err := e.read(id)
		if err != nil {
			return err
		}
		if j.Operation.State == "running" && j.RouteChange != nil {
			// The proxy may already serve the new routes, so the outcome is settled by route recovery.
			e.recovering[id] = routeRecovery{workload: j.Request.Workload, state: "interrupted"}
			if !j.RouteChange.Committed {
				j.Operation.Error = routeRestoring
				if err = writeJSON(e.path(id), j); err != nil {
					return err
				}
			}
			continue
		}
		if j.Operation.State == "running" {
			if j.Request.GuardedDeploy != nil && *j.Request.GuardedDeploy {
				cleanup, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
				err := e.recoverCandidate(cleanup, filepath.Join(e.Root, "workloads", j.Request.Workload))
				if err == nil {
					err = e.recoverDeployGuard(cleanup, j.Request.Workload, io.Discard)
				}
				cancel()
				if err != nil {
					return err
				}
			}
			if j.Request.Hooks != nil {
				cleanup, cancel := context.WithTimeout(context.Background(), time.Minute)
				err := e.recoverHook(cleanup, filepath.Join(e.Root, "workloads", j.Request.Workload))
				cancel()
				if err != nil {
					return err
				}
			}
			now := time.Now().UTC()
			j.Operation.State = "interrupted"
			j.Operation.FinishedAt = &now
			j.Operation.Error = "Worker restarted during execution; inspect workload before starting another operation."
			if j.CancelRequested {
				j.Operation.State = "cancelled"
				j.Operation.Error = "Cancellation requested; inspect workload state."
			}
			if err = writeJSON(e.path(id), j); err != nil {
				return err
			}
		}
	}
	return nil
}
func (e *Engine) Serve(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	var jobs sync.WaitGroup
	defer func() { cancel(); jobs.Wait() }()
	if err := e.Recover(); err != nil {
		return err
	}
	if err := e.recoverDrills(ctx); err != nil {
		return err
	}
	if err := e.recoverStorage(ctx); err != nil {
		return err
	}
	if routes, err := e.loadIngress(); err != nil {
		return err
	} else if len(routes) > 0 {
		if err = e.syncIngress(ctx, routes, os.Stderr); err != nil {
			fmt.Fprintln(os.Stderr, "Ingress recovery failed; worker remains available for repairs:", err)
		}
	}
	jobs.Go(func() { e.cleanupLoop(ctx) })
	jobs.Go(func() { e.linkLoop(ctx) })
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		if err := e.dispatch(ctx, &jobs); err != nil {
			return err
		}
		select {
		case err := <-e.fatal:
			return err
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}
func (e *Engine) dispatch(ctx context.Context, jobs *sync.WaitGroup) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	now := time.Now()
	for id, recovery := range e.recovering {
		if recovery.running || now.Before(recovery.next) {
			continue
		}
		recovery.running = true
		e.recovering[id] = recovery
		jobs.Go(func() { e.recoverRoutes(ctx, id, recovery.state) })
	}
	if e.customUnsettled && !e.customRunning && !now.Before(e.customRetry) {
		e.customRunning = true
		jobs.Go(func() { e.runCustomRecovery(ctx) })
	}
	if e.draining {
		return nil
	}
	sort.Slice(e.pending, func(i, j int) bool { return e.pending[i].CreatedAt.Before(e.pending[j].CreatedAt) })
	remaining := make([]Operation, 0, len(e.pending))
	for _, op := range e.pending {
		if e.busy(op.Workload) || len(e.active) >= 4 {
			remaining = append(remaining, op)
			continue
		}
		j, err := e.read(op.ID)
		if err != nil {
			return err
		}
		if j.Operation.State != "queued" {
			continue
		}
		e.active[j.Request.Workload] = true
		j.Operation.State = "running"
		if err = writeJSON(e.path(j.Request.ID), j); err != nil {
			delete(e.active, j.Request.Workload)
			return err
		}
		operationCtx, cancel := context.WithCancel(ctx)
		e.cancels[j.Request.ID] = cancel
		jobs.Go(func() { defer cancel(); e.execute(operationCtx, j) })
	}
	e.pending = remaining
	return nil
}

type boundedLog struct {
	file              io.Writer
	remaining         int64
	secrets           []string
	publicIdentifiers []string
	pending           []byte
}

func (l *boundedLog) Write(p []byte) (int, error) {
	n := len(p)
	if l.remaining <= 0 {
		l.pending = nil
		return n, nil
	}
	l.pending = append(l.pending, p...)
	if err := l.flush(false); err != nil {
		return 0, err
	}
	return n, nil
}
func (l *boundedLog) flush(final bool) error {
	maxLength := 1
	for _, identifier := range l.publicIdentifiers {
		if len(identifier) > maxLength {
			maxLength = len(identifier)
		}
	}
	for _, secret := range l.secrets {
		if len(secret) > maxLength {
			maxLength = len(secret)
		}
	}
	var output strings.Builder
	data := string(l.pending)
	offset := 0
	for offset < len(data) && (final || len(data)-offset >= maxLength) {
		public := ""
		for _, identifier := range l.publicIdentifiers {
			if len(identifier) > len(public) && strings.HasPrefix(data[offset:], identifier) {
				public = identifier
			}
		}
		if public != "" {
			output.WriteString(public)
			offset += len(public)
			continue
		}
		matched := 0
		for _, secret := range l.secrets {
			if len(secret) > matched && strings.HasPrefix(data[offset:], secret) {
				matched = len(secret)
			}
		}
		if matched > 0 {
			output.WriteString("[redacted]")
			offset += matched
		} else {
			output.WriteByte(data[offset])
			offset++
		}
	}
	l.pending = append(l.pending[:0], data[offset:]...)
	b := []byte(output.String())
	if int64(len(b)) > l.remaining {
		b = b[:l.remaining]
	}
	if len(b) == 0 {
		return nil
	}
	n, err := l.file.Write(b)
	l.remaining -= int64(n)
	return err
}
func (e *Engine) execute(parent context.Context, j journal) {
	ctx, cancel := context.WithTimeout(parent, 30*time.Minute)
	defer cancel()
	err := e.perform(ctx, j.Request)
	baselineFailed := false
	if err == nil && j.Request.Kind != "job" {
		switch j.Request.Action {
		case "deploy", "stop", "start", "restart", "rollback", "routes", "restore", "restore_copy":
			record := j.Request.RecordDrift
			if !record {
				_, baselineErr := os.Stat(filepath.Join(e.Root, "workloads", j.Request.Workload, "drift.json"))
				record = baselineErr == nil
				if baselineErr != nil && !os.IsNotExist(baselineErr) {
					err = baselineErr
				}
			}
			if record && err == nil {
				err = e.recordDriftBaseline(ctx, j.Request)
			}
			baselineFailed = err != nil
		}
	}
	// Containers may have been recreated; attach them to their private links again.
	defer e.kickLinks()
	e.mu.Lock()
	defer e.mu.Unlock()
	defer delete(e.active, j.Request.Workload)
	defer delete(e.cancels, j.Request.ID)
	committed := false
	persisted, readErr := e.read(j.Request.ID)
	if readErr == nil {
		j.Operation.Revision = persisted.Operation.Revision
		j.Operation.Preview = persisted.Operation.Preview
		j.CancelRequested = persisted.CancelRequested
		committed = persisted.RouteChange != nil && persisted.RouteChange.Committed
	}
	if errors.Is(err, errRoutesUnresolved) {
		// The operation stays running, which keeps its hostnames claimed until recovery settles it.
		e.recovering[j.Request.ID] = routeRecovery{workload: j.Request.Workload, state: "failed"}
		if readErr == nil && !committed {
			persisted.Operation.Error = routeRestoring
			readErr = writeJSON(e.path(j.Request.ID), persisted)
		}
		if readErr != nil {
			fmt.Fprintln(os.Stderr, "could not record unsettled route change:", readErr)
			select {
			case e.fatal <- readErr:
			default:
			}
		}
		return
	}
	now := time.Now().UTC()
	j.Operation.FinishedAt = &now
	j.Operation.State = "succeeded"
	if err != nil {
		j.Operation.State = "failed"
		j.Operation.Error = "Operation failed; inspect the operation log."
		if baselineFailed {
			j.Operation.Error = "The operation completed, but its runtime configuration baseline could not be recorded; inspect the resource before retrying."
		}
		if ctx.Err() != nil {
			j.Operation.State = "interrupted"
			j.Operation.Error = "Execution interrupted; inspect workload before retrying."
		}
	}
	if j.CancelRequested {
		j.Operation.State = "cancelled"
		j.Operation.Error = "Cancellation requested; inspect workload state before retrying."
	}
	if j.Request.Action == "routes" {
		// A committed route change is final, and an uncommitted one left the previous routes in place.
		if committed {
			j.Operation.State, j.Operation.Error = "succeeded", ""
		} else if j.Operation.State != "succeeded" {
			j.Operation.Error = routeOutcome(j.Operation.State)
		}
	}
	if saveErr := writeJSON(e.path(j.Request.ID), j); saveErr != nil {
		fmt.Fprintln(os.Stderr, "could not persist operation result:", saveErr)
		select {
		case e.fatal <- saveErr:
		default:
		}
	}
}
func (e *Engine) perform(ctx context.Context, r Request) (result error) {
	dir := filepath.Join(e.Root, "workloads", r.Workload)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	file, err := os.OpenFile(filepath.Join(e.Root, "operations", r.ID+".log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return err
	}
	defer file.Close()
	// This generated Docker project name is public even when an environment value happens to match part of it.
	log := &boundedLog{file: file, remaining: 4 * 1024 * 1024, publicIdentifiers: []string{e.composeProject(r.Workload)}}
	defer func() {
		if err := log.flush(true); err != nil && result == nil {
			result = err
		}
	}()
	var bundleSecrets []string
	var publicBuiltins map[string]string
	if len(r.BuiltinDefaults) > 0 {
		r.Environment, publicBuiltins = mergeBuiltinDefaults(r.Environment, r.BuiltinDefaults)
	}
	env := []string{}
	if r.Private != nil {
		log.secrets = append(log.secrets, r.Private.AuthKey)
	}
	for k, v := range r.Environment {
		env = append(env, k+"="+v)
		if _, public := publicBuiltins[k]; !public {
			log.secrets = append(log.secrets, v)
		}
	}
	if r.Registry != nil && r.Action == "deploy" {
		log.secrets = append(log.secrets, r.Registry.Password)
		config, err := registryConfig(dir, r.Registry)
		if err != nil {
			return err
		}
		defer os.RemoveAll(config)
		env = append(env, "DOCKER_CONFIG="+config)
	}
	if r.Backup != nil && r.Backup.Remote != nil {
		log.secrets = append(log.secrets, r.Backup.Remote.Password, r.Backup.Remote.SecretKey)
	}
	if r.Source != nil {
		log.secrets = append(log.secrets, r.Source.Token, r.Source.SSHPrivateKey)
		log.secrets = append(log.secrets, strings.Split(r.Source.SSHPrivateKey, "\n")...)
	}
	if r.Backup != nil {
		log.secrets = append(log.secrets, r.Backup.Password, r.Backup.AccessKey, r.Backup.SecretKey, r.Backup.SessionToken)
	}
	if r.Action == "routes" {
		err := e.updateRoutes(ctx, r, dir, log)
		if err != nil {
			fmt.Fprintln(log, err)
		}
		return err
	}
	if r.Action == "restore_copy" {
		return e.performRestoreCopy(ctx, r, log)
	}
	if r.Action == "preview_remove" {
		return e.removePreview(ctx, r, log)
	}
	if r.Action == "preview_data" {
		err := e.performPreviewData(ctx, r, log)
		if err != nil {
			fmt.Fprintln(log, err)
		}
		return err
	}
	if r.Action == "environment_preview_remove" {
		err := e.removeEnvironmentPreview(ctx, r, log)
		if err != nil {
			fmt.Fprintln(log, err)
		}
		return err
	}
	if _, err := os.Stat(filepath.Join(dir, "copy-recovery.json")); err == nil && r.Action != "stop" {
		return errors.New("restore copy is incomplete; inspect its retained storage or create a new restore copy")
	} else if err != nil && !os.IsNotExist(err) {
		return err
	}
	if err = e.recoverHook(ctx, dir); err != nil {
		return errors.New("previous deployment hook cleanup requires repair before another operation")
	}
	if err = e.recoverCandidate(ctx, dir); err != nil {
		return err
	}
	if err = e.recoverDeployGuard(ctx, r.Workload, log); err != nil {
		return err
	}
	if data, readErr := os.ReadFile(filepath.Join(dir, "storage-recovery.json")); readErr == nil {
		var recovery storageRecovery
		if err = json.Unmarshal(data, &recovery); err != nil {
			return err
		}
		if recovery.Restoring && r.Action != "restore" && r.Action != "stop" {
			fmt.Fprintln(log, "An interrupted restore requires another restore before this workload can start.")
			return errors.New("storage recovery required")
		}
	} else if !os.IsNotExist(readErr) {
		return readErr
	}
	if r.Action == "drill" {
		return e.performDrill(ctx, r, log)
	}
	if r.Action == "backup" || r.Action == "restore" {
		err := e.performBackup(ctx, r, log)
		if err == nil && r.Action == "backup" {
			err = e.pruneBackups(ctx, r, log)
		}
		if err != nil {
			fmt.Fprintln(log, err)
		}
		return err
	}
	if r.Action == "stop" {
		containers, err := e.Containers(ctx, r.Workload)
		if err != nil {
			return err
		}
		if len(containers) == 0 {
			return nil
		}
		args := []string{"docker", "stop", "--time", "30"}
		for _, container := range containers {
			if !regexp.MustCompile(`^[a-f0-9]{12,64}$`).MatchString(container.ID) {
				return errors.New("invalid container identity")
			}
			args = append(args, container.ID)
		}
		return e.Run(ctx, dir, args, nil, log)
	}
	composePath := filepath.Join(dir, "compose.yaml")
	if r.Action == "deploy" {
		if r.Preview {
			if _, markerErr := os.Stat(filepath.Join(dir, "preview.json")); os.IsNotExist(markerErr) {
				if _, currentErr := os.Stat(filepath.Join(dir, "current.json")); currentErr == nil {
					return errors.New("refusing to convert an existing deployment to a preview")
				}
			}
			if err = writeJSON(filepath.Join(dir, "preview.json"), r.Workload); err != nil {
				return err
			}
		}
		if r.Source != nil {
			var revision string
			composePath, revision, err = e.checkout(ctx, r, dir, log)
			if err != nil {
				return err
			}
			if _, automatic := publicBuiltins[CoveCommitSHA]; automatic {
				r.Environment[CoveCommitSHA] = revision
				publicBuiltins[CoveCommitSHA] = revision
				for index, value := range env {
					if strings.HasPrefix(value, CoveCommitSHA+"=") {
						env[index] = CoveCommitSHA + "=" + revision
					}
				}
			}
			e.mu.Lock()
			j, readErr := e.read(r.ID)
			if readErr == nil {
				j.Operation.Revision = revision
				readErr = writeJSON(e.path(r.ID), j)
			}
			e.mu.Unlock()
			if readErr != nil {
				return readErr
			}
		} else {
			composePath = filepath.Join(dir, r.ID+".yaml")
			if err = os.WriteFile(composePath, []byte(r.Compose), 0600); err != nil {
				return err
			}
		}
	} else if r.Action == "rollback" {
		var release deployedState
		previous, readErr := e.read(r.Release)
		if readErr != nil || previous.Operation.Workload != r.Workload || previous.Operation.State != "succeeded" {
			return errors.New("release is not a successful operation for this workload")
		}
		data, readErr := os.ReadFile(filepath.Join(dir, "release-states", r.Release+".json"))
		if readErr != nil {
			return readErr
		}
		if err = json.Unmarshal(data, &release); err != nil {
			return err
		}
		e.mu.Lock()
		j, readErr := e.read(r.ID)
		if readErr == nil {
			j.Operation.Revision = release.Revision
			readErr = writeJSON(e.path(r.ID), j)
		}
		e.mu.Unlock()
		if readErr != nil {
			return readErr
		}
		if !reflect.DeepEqual(release.Networks, r.Networks) {
			return errors.New("network connections changed since this release; deploy the current configuration instead")
		}
		if !reflect.DeepEqual(release.Private, r.Private) {
			return errors.New("private networking changed since this release; deploy the current configuration instead")
		}
		composePath = release.ComposePath
		env = nil
		log.secrets = append(log.secrets, release.AdditionalSecrets...)
		for key, value := range release.Environment {
			env = append(env, key+"="+value)
			if public, ok := release.PublicBuiltins[key]; !ok || public != value {
				log.secrets = append(log.secrets, value)
			}
		}
		args := []string{"docker", "compose", "--project-name", e.composeProject(r.Workload), "--file", composePath, "up", "--detach", "--no-build", "--pull", "never", "--wait", "--wait-timeout", "120", "--remove-orphans"}
		if err = e.Run(ctx, dir, args, env, log); err != nil {
			return err
		}
		// Routes are live settings changed without redeploying, so a rollback keeps the current ones.
		release.Routes = r.Routes
		if err = e.applyIngress(ctx, r.Workload, release.Routes, log); err != nil {
			fmt.Fprintln(log, err)
			return err
		}
		if err = writeJSON(filepath.Join(dir, "release-states", r.ID+".json"), release); err != nil {
			return err
		}
		return writeJSON(filepath.Join(dir, "current.json"), release)
	} else {
		var current deployedState
		data, err := os.ReadFile(filepath.Join(dir, "current.json"))
		if err != nil {
			return err
		}
		if err = json.Unmarshal(data, &current); err != nil {
			return err
		}
		composePath = current.ComposePath
		env = nil
		log.secrets = append(log.secrets, current.AdditionalSecrets...)
		for key, value := range current.Environment {
			env = append(env, key+"="+value)
			if public, ok := current.PublicBuiltins[key]; !ok || public != value {
				log.secrets = append(log.secrets, value)
			}
		}
	}
	args := []string{"docker", "compose", "--project-name", e.composeProject(r.Workload), "--file", composePath}
	if r.Action == "deploy" {
		if r.Preview {
			composePath, err = e.preparePreview(r, dir, composePath)
			if err != nil {
				fmt.Fprintln(log, err)
				return err
			}
			args = []string{"docker", "compose", "--project-name", e.composeProject(r.Workload), "--file", composePath}
		}
		composePath, err = e.prepareComposeBundle(ctx, r, dir, composePath, env)
		if err != nil {
			return err
		}
		args = []string{"docker", "compose", "--project-name", e.composeProject(r.Workload), "--file", composePath}
		if r.ComposeBundle != nil {
			bundleSecrets, err = composeBundleSecrets(composePath)
			if err != nil {
				return errors.New("could not prepare Compose bundle log redaction")
			}
			log.secrets = append(log.secrets, bundleSecrets...)
		}
		if len(r.BuiltinDefaults) > 0 {
			prepared, err := e.prepareBuiltinEnvironment(ctx, r, dir, args, env)
			if err != nil {
				fmt.Fprintln(log, "Could not prepare built-in variables; check Compose required variables and build settings.")
				return err
			}
			composePath = prepared
			args = []string{"docker", "compose", "--project-name", e.composeProject(r.Workload), "--file", composePath}
		}
		if r.Source != nil {
			resolved := &limitedBuffer{Limit: 2 * 1024 * 1024}
			if err = e.Run(ctx, dir, append(append([]string{}, args...), "config", "--format", "json"), env, resolved); err != nil {
				fmt.Fprintln(log, "Could not resolve Compose configuration for storage validation.")
				return err
			}
			if err = validateReleaseStorage(resolved.data.Bytes(), filepath.Join(dir, "releases")); err != nil {
				fmt.Fprintln(log, err.Error())
				return err
			}
		}
		if r.EnvironmentPreview != nil {
			composePath, err = e.preparePreviewStorage(ctx, r, dir, args, env, log)
			if err != nil {
				fmt.Fprintln(log, err)
				return err
			}
			args = []string{"docker", "compose", "--project-name", e.composeProject(r.Workload), "--file", composePath}
		}
		if len(r.FileSecrets) > 0 {
			composePath, err = e.prepareFileSecrets(ctx, r, dir, args, env)
			if err != nil {
				return err
			}
			args = []string{"docker", "compose", "--project-name", e.composeProject(r.Workload), "--file", composePath}
		}
		if len(r.Networks) > 0 {
			composePath, err = e.prepareNetworks(ctx, r, dir, args, env, log)
			if err != nil {
				return err
			}
			args = []string{"docker", "compose", "--project-name", e.composeProject(r.Workload), "--file", composePath}
		}
		if !r.Preview {
			linkedPath, linked, err := e.prepareLinks(ctx, r, dir, args, env)
			if err != nil {
				fmt.Fprintln(log, err)
				return err
			}
			if linked {
				composePath = linkedPath
				args = []string{"docker", "compose", "--project-name", e.composeProject(r.Workload), "--file", composePath}
			}
		}
		if r.Private != nil {
			composePath, err = e.preparePrivateNetwork(ctx, r, dir, args, env, log)
			if err != nil {
				return err
			}
			args = []string{"docker", "compose", "--project-name", e.composeProject(r.Workload), "--file", composePath}
		}
		if len(r.Limits) > 0 {
			composePath, err = e.prepareLimits(ctx, r, dir, args, env, log)
			if err != nil {
				return err
			}
			args = []string{"docker", "compose", "--project-name", e.composeProject(r.Workload), "--file", composePath}
		}
		if len(r.ImagePins) > 0 {
			composePath, err = e.preparePromotion(ctx, r, dir, args, env)
			if err != nil {
				return err
			}
			args = []string{"docker", "compose", "--project-name", e.composeProject(r.Workload), "--file", composePath}
		}
		if err = e.Run(ctx, dir, append(append([]string{}, args...), "config", "--quiet"), env, log); err != nil {
			return err
		}
		if err = e.buildImages(ctx, dir, args, env, log); err != nil {
			return err
		}
		if r.EnvironmentPreview != nil {
			if err = e.seedPreviewDatabase(ctx, r, dir, args, env, log); err != nil {
				fmt.Fprintln(log, err)
				return err
			}
		}
		if r.Hooks != nil {
			if err = e.runBeforeHooks(ctx, dir, r.ID, args, env, r.Hooks, log); err != nil {
				return err
			}
		}
		up := []string{"up", "--detach", "--no-build", "--wait", "--wait-timeout", "120", "--remove-orphans"}
		if r.GuardedDeploy != nil && *r.GuardedDeploy {
			composePath, err = e.testDeploymentCandidate(ctx, r, dir, args, env, log)
			if err != nil {
				fmt.Fprintln(log, err)
				return err
			}
			args = []string{"docker", "compose", "--project-name", e.composeProject(r.Workload), "--file", composePath}
			if err = e.beginDeployGuard(r, dir, composePath); err != nil {
				return err
			}
			defer func() {
				cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Minute)
				defer cancel()
				result = errors.Join(result, e.recoverDeployGuard(cleanup, r.Workload, log))
			}()
		}
		if r.Kind == "job" {
			up = []string{"up", "--no-build", "--abort-on-container-exit", "--exit-code-from", r.JobService, "--remove-orphans"}
		}
		if err = e.Run(ctx, dir, append(append([]string{}, args...), up...), env, log); err != nil {
			return err
		}
		state := deployedState{ComposePath: composePath, Environment: r.Environment}
		if r.Kind != "job" {
			state, err = e.captureRelease(ctx, r, dir, args, env)
			if err != nil {
				fmt.Fprintln(log, "Deployment ran, but its immutable release snapshot could not be saved.")
				return err
			}
		}
		state.AdditionalSecrets = bundleSecrets
		state.PublicBuiltins = publicBuiltins
		state.Networks = r.Networks
		state.Private = r.Private
		state.Routes = r.Routes
		if err = e.applyIngress(ctx, r.Workload, r.Routes, log); err != nil {
			fmt.Fprintln(log, err)
			return err
		}
		if r.Kind != "job" {
			if err = writeJSON(filepath.Join(dir, "release-states", r.ID+".json"), state); err != nil {
				return err
			}
		}
		if err = writeJSON(filepath.Join(dir, "current.json"), state); err != nil {
			return err
		}
		if r.Kind != "job" {
			if r.Hooks != nil {
				if err = e.runHook(ctx, dir, r.ID, args, env, r.Hooks.After, false, log); err != nil {
					return err
				}
			}
			return e.pruneReleases(ctx, r.Workload, log)
		}
		return nil
	}
	return e.Run(ctx, dir, append(args, r.Action), env, log)
}

package control

import (
	"context"
	"cove/internal/worker"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"net/http"
	"sync"
	"time"
)

func resourceID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

type workloadInput struct {
	Autoscale        *worker.AutoscalePolicy         `json:"autoscale,omitempty"`
	Rollout          *worker.RolloutOptions          `json:"rollout,omitempty"`
	GuardedDeploy    *bool                           `json:"guarded_deploy,omitempty"`
	FileSecrets      []worker.FileSecret             `json:"file_secrets,omitempty"`
	Hooks            *worker.DeploymentHooks         `json:"hooks,omitempty"`
	Limits           map[string]worker.ServiceLimits `json:"limits,omitempty"`
	Private          *worker.PrivateNetwork          `json:"private,omitempty"`
	Backup           *worker.BackupConfig            `json:"backup,omitempty"`
	Routes           []worker.Route                  `json:"routes,omitempty"`
	EnvironmentPatch map[string]*string              `json:"environment_patch,omitempty"`
	Name             string                          `json:"name"`
	NodeID           string                          `json:"node_id"`
	Compose          string                          `json:"compose"`
	Environment      map[string]string               `json:"environment"`
	Source           *worker.GitSource               `json:"source,omitempty"`
	Kind             string                          `json:"kind,omitempty"`
	JobService       string                          `json:"job_service,omitempty"`
	// Placement in a project environment is stored on the workload row, never in the sealed configuration.
	EnvironmentID string `json:"environment_id,omitempty"`
	ProjectID     string `json:"project_id,omitempty"`
}

func (s *Server) createWorkload(w http.ResponseWriter, r *http.Request) {
	var in workloadInput
	if !decode(w, r, &in) {
		return
	}
	if in.Autoscale != nil {
		http.Error(w, "create the resource first, then configure CPU scaling", 400)
		return
	}
	if in.Backup != nil && (in.Backup.Remote != nil || in.Backup.StorageID != "" || in.Backup.Database != nil) {
		http.Error(w, "use the backup policy endpoint to configure logical backups or a storage location", 400)
		return
	}
	if in.EnvironmentPatch != nil {
		http.Error(w, "environment_patch is only supported when editing", 400)
		return
	}
	if in.Private != nil {
		http.Error(w, "create the resource first, then configure private networking", 400)
		return
	}
	if err := validateWorkload(in); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	if in.Kind == "" {
		in.Kind = "service"
	}
	projectID, ok := s.resourceProject(w, r, in.ProjectID, in.EnvironmentID)
	if !ok {
		return
	}
	environmentID := in.EnvironmentID
	in.EnvironmentID, in.ProjectID = "", ""
	var state string
	if err := s.DB.QueryRow(r.Context(), "SELECT state FROM nodes WHERE id=$1", in.NodeID).Scan(&state); err != nil || state != "ready" {
		http.Error(w, "select a ready node", 409)
		return
	}
	if in.Rollout != nil {
		value := in.Rollout.Normalized()
		in.Rollout = &value
	}
	data, err := json.Marshal(in)
	if err != nil {
		http.Error(w, "invalid configuration", 400)
		return
	}
	encrypted, err := seal(s.Key, data)
	if err != nil {
		http.Error(w, "could not protect configuration", 500)
		return
	}
	id := resourceID()
	interval := 0
	if in.Backup != nil {
		interval = in.Backup.IntervalHours
	}
	tx, err := s.DB.Begin(r.Context())
	if err != nil {
		http.Error(w, "database unavailable", 503)
		return
	}
	defer tx.Rollback(r.Context())
	if !s.checkRouteClaims(w, r.Context(), tx, id, in.NodeID, in.Routes) {
		return
	}
	if !s.checkRouteListenerSelection(w, r, in.NodeID, in.Routes) {
		return
	}
	cpu, memory := configuredUsage(in)
	_, err = tx.Exec(r.Context(), "INSERT INTO workloads(id,node_id,name,configuration,kind,backup_interval,next_backup_at,environment_id,team_id,project_id,configured_cpu,configured_memory_mb) VALUES($1,$2,$3,$4,$5,$6,now()+make_interval(hours=>$6),NULLIF($7,''),$8,$9,$10,$11)", id, in.NodeID, in.Name, encrypted, in.Kind, interval, environmentID, actor(r).Team, projectID, cpu, memory)
	if quotaError(w, err) {
		return
	}
	var constraint *pgconn.PgError
	if errors.As(err, &constraint) && constraint.Code == "23503" {
		http.Error(w, "the selected node, project, or environment changed; refresh and try again", 409)
		return
	}
	if err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		http.Error(w, "could not save workload", 500)
		return
	}
	respond(w, 201, map[string]string{"id": id})
}
func (s *Server) listWorkloads(w http.ResponseWriter, r *http.Request) {
	rows, err := s.DB.Query(r.Context(), "SELECT w.id,w.name,w.node_id,n.name,w.kind,COALESCE(e.id,''),COALESCE(e.name,''),COALESCE(p.name,''),w.project_id,CASE WHEN $3 THEN 3 ELSE cove_access($1,w.team_id,COALESCE(p.id,''),COALESCE(e.id,'')) END FROM workloads w JOIN nodes n ON n.id=w.node_id LEFT JOIN environments e ON e.id=w.environment_id JOIN projects p ON p.id=w.project_id WHERE w.team_id=$2 AND ($3 OR cove_access($1,w.team_id,COALESCE(p.id,''),COALESCE(e.id,''))>0) ORDER BY w.created_at", actor(r).ID, actor(r).Team, actor(r).Admin)
	if err != nil {
		http.Error(w, "database unavailable", 503)
		return
	}
	defer rows.Close()
	type item struct {
		ProjectID     string   `json:"project_id"`
		EnvironmentID string   `json:"environment_id"`
		Environment   string   `json:"environment"`
		Project       string   `json:"project"`
		ID            string   `json:"id"`
		Name          string   `json:"name"`
		NodeID        string   `json:"node_id"`
		Node          string   `json:"node"`
		Kind          string   `json:"kind"`
		Permissions   []string `json:"permissions"`
	}
	out := []item{}
	for rows.Next() {
		var v item
		var level int
		if err = rows.Scan(&v.ID, &v.Name, &v.NodeID, &v.Node, &v.Kind, &v.EnvironmentID, &v.Environment, &v.Project, &v.ProjectID, &level); err != nil {
			http.Error(w, "could not read workloads", 500)
			return
		}
		v.Permissions = permissions(level)
		out = append(out, v)
	}
	if rows.Err() != nil {
		http.Error(w, "could not read workloads", 500)
		return
	}
	respond(w, 200, out)
}
func (s *Server) queueOperation(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Action     string               `json:"action"`
		Release    string               `json:"release"`
		BackupID   string               `json:"backup_id"`
		PullLatest bool                 `json:"pull_latest"`
		Build      *worker.BuildOptions `json:"build"`
	}
	if !decode(w, r, &in) {
		return
	}
	switch in.Action {
	case "deploy", "stop", "start", "restart", "rollback", "backup", "restore", "drill", "image_check", "image_scan", "build", "rollout_promote", "rollout_abort":
	default:
		http.Error(w, "unsupported action", 400)
		return
	}
	var encrypted []byte
	tx, err := s.DB.Begin(r.Context())
	if err != nil {
		http.Error(w, "database unavailable", 503)
		return
	}
	defer tx.Rollback(r.Context())
	err = tx.QueryRow(r.Context(), "SELECT configuration FROM workloads WHERE id=$1 FOR UPDATE", r.PathValue("id")).Scan(&encrypted)
	if errors.Is(err, pgx.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, "database unavailable", 503)
		return
	}
	data, err := unseal(s.Key, encrypted)
	if err != nil {
		http.Error(w, "configuration unavailable", 500)
		return
	}
	var config workloadInput
	if json.Unmarshal(data, &config) != nil {
		http.Error(w, "configuration unavailable", 500)
		return
	}
	repair, err := environmentOperationGate(r.Context(), tx, r.PathValue("id"), in.Action)
	if err != nil {
		if errors.Is(err, errEnvironmentPaused) {
			http.Error(w, err.Error(), 409)
		} else {
			http.Error(w, "environment state unavailable", 503)
		}
		return
	}
	if err = s.idleOperationGate(r.Context(), tx, r.PathValue("id"), in.Action); err != nil {
		if errors.Is(err, errIdleAsleep) {
			http.Error(w, err.Error(), 409)
		} else {
			http.Error(w, "idle state unavailable; confirm the app is awake before deploying", 502)
		}
		return
	}
	id := resourceID()
	request := worker.Request{ID: id, Workload: r.PathValue("id"), Action: in.Action, Release: in.Release, Compose: config.Compose, Environment: config.Environment, Source: config.Source, Kind: config.Kind, JobService: config.JobService, Routes: config.Routes, Private: config.Private, Limits: config.Limits, Hooks: config.Hooks, FileSecrets: config.FileSecrets, GuardedDeploy: config.GuardedDeploy, Rollout: config.Rollout}
	request.Backup, request.BackupID = config.Backup, in.BackupID
	request.PullLatest = in.PullLatest
	if request.Kind == "job" && request.Action == "deploy" {
		request.Job, err = jobExecutionOptions(r.Context(), tx, request.Workload)
		if err != nil {
			if errors.Is(err, errJobWorkerUnsupported) {
				http.Error(w, err.Error(), 409)
			} else {
				http.Error(w, "job execution settings unavailable", 503)
			}
			return
		}
	}
	if in.Action != "deploy" && in.Action != "rollback" || request.Rollout != nil && request.Rollout.Strategy == "disabled" {
		request.Rollout = nil
	}
	if in.Action == "rollout_promote" || in.Action == "rollout_abort" {
		if config.Kind == "job" || in.PullLatest || in.Build != nil {
			http.Error(w, "rollout actions require a service", 400)
			return
		}
		request = worker.Request{ID: id, Workload: request.Workload, Action: in.Action, Kind: "service"}
	}
	request.Build = in.Build
	request.ResumeSuspended = repair
	if request.Rollout != nil || in.Action == "rollout_promote" || in.Action == "rollout_abort" {
		var supported bool
		if err = tx.QueryRow(r.Context(), `SELECT COALESCE((n.capabilities->>'rollouts')::boolean,false) FROM nodes n JOIN workloads w ON w.node_id=n.id WHERE w.id=$1`, request.Workload).Scan(&supported); err != nil {
			http.Error(w, "worker capabilities unavailable", 503)
			return
		}
		if !supported {
			http.Error(w, "upgrade and verify the worker before using deployment rollouts", 409)
			return
		}
	}
	if in.Build != nil && in.Action != "build" {
		http.Error(w, "build options require a build operation", 400)
		return
	}
	executionNode := ""
	if in.Action == "build" {
		executionNode, err = buildExecutionNode(r.Context(), tx, request.Workload, "")
		if err != nil {
			http.Error(w, err.Error(), 409)
			return
		}
		var supported, connected, preview bool
		if err = tx.QueryRow(r.Context(), `SELECT COALESCE((n.capabilities->>'build_publish')::boolean,false), EXISTS(SELECT 1 FROM workload_connections c WHERE c.workload_id=w.id AND c.kind='registry'), EXISTS(SELECT 1 FROM previews p WHERE p.workload_id=w.id) OR EXISTS(SELECT 1 FROM environment_preview_resources p WHERE p.resource_id=w.id) FROM nodes n JOIN workloads w ON w.id=$1 WHERE n.id=$2`, request.Workload, executionNode).Scan(&supported, &connected, &preview); err != nil {
			http.Error(w, "build configuration unavailable", 503)
			return
		}
		if preview || in.PullLatest || in.Release != "" || in.BackupID != "" {
			http.Error(w, "build requires source inputs outside managed previews", 400)
			return
		}
		if !supported || !connected {
			http.Error(w, "connect a registry and upgrade and verify the worker before building", 409)
			return
		}
		request = worker.Request{ID: id, Workload: request.Workload, Action: "build", Build: in.Build, Compose: config.Compose, Source: config.Source, Environment: config.Environment, FileSecrets: config.FileSecrets, Kind: config.Kind, JobService: config.JobService}
		recipe, recipeErr := s.readBuildRecipe(r.Context(), tx, request.Workload)
		if recipeErr != nil {
			http.Error(w, "build recipe unavailable", 503)
			return
		}
		if recipe.Engine != "compose" {
			var supported bool
			if err = tx.QueryRow(r.Context(), `SELECT COALESCE((capabilities->>'build_recipes')::boolean,false) FROM nodes WHERE id=$1`, executionNode).Scan(&supported); err != nil || !supported {
				http.Error(w, "upgrade and verify the effective build worker before using build recipes", 409)
				return
			}
			request.BuildRecipe = &recipe
			if recipe.Engine == "buildpacks" {
				if err := requireBuildpacks(r.Context(), tx, executionNode); err != nil {
					http.Error(w, err.Error(), 409)
					return
				}
			}
		}
		var remoteBuild bool
		var platform string
		if err = tx.QueryRow(r.Context(), `SELECT w.node_id<>$2,COALESCE(n.capabilities->>'os','')||'/'||COALESCE(n.capabilities->>'arch','') FROM workloads w JOIN nodes n ON n.id=w.node_id WHERE w.id=$1`, request.Workload, executionNode).Scan(&remoteBuild, &platform); err != nil {
			http.Error(w, "runtime platform unavailable", 409)
			return
		}
		if remoteBuild || recipe.Engine == "buildpacks" {
			request.BuildPlatform = platform
		}
	}
	if in.Action == "image_scan" {
		if in.PullLatest || in.Release != "" || in.BackupID != "" {
			http.Error(w, "a vulnerability scan only inspects retained deployed images", 400)
			return
		}
		if err = s.requireImageScanning(r.Context(), request.Workload); err != nil {
			http.Error(w, "image scanning requires node-operator and team/node opt-in on a ready worker", 409)
			return
		}
		request = worker.Request{ID: id, Workload: request.Workload, Action: "image_scan", Kind: config.Kind, JobService: config.JobService}
	}
	if in.Action == "image_check" {
		if in.PullLatest {
			http.Error(w, "pull latest requires a deploy operation", 400)
			return
		}
		request = worker.Request{ID: id, Workload: request.Workload, Action: "image_check", Kind: config.Kind, JobService: config.JobService}
		if err = s.prepareImageUpdateCheck(r.Context(), tx, &request); err != nil {
			http.Error(w, "image update configuration unavailable", 409)
			return
		}
	}
	if in.PullLatest && config.Kind == "job" {
		http.Error(w, "pull and redeploy is unavailable for jobs; run the job explicitly", 400)
		return
	}
	if in.PullLatest || in.Action == "image_check" {
		var supported bool
		if err = tx.QueryRow(r.Context(), `SELECT COALESCE((n.capabilities->>'image_lifecycle')::boolean,false) AND ($2<>'job' OR COALESCE((n.capabilities->>'job_image_lifecycle')::boolean,false)) FROM nodes n JOIN workloads w ON w.node_id=n.id WHERE w.id=$1`, request.Workload, request.Kind).Scan(&supported); err != nil {
			http.Error(w, "worker capabilities unavailable", 503)
			return
		}
		if !supported {
			http.Error(w, "upgrade and verify the worker before using image lifecycle operations", 409)
			return
		}
	}
	if in.Action == "drill" {
		var supported bool
		if err = tx.QueryRow(r.Context(), `SELECT COALESCE((n.capabilities->>'restore_drills')::boolean,false) FROM nodes n JOIN workloads w ON w.node_id=n.id WHERE w.id=$1`, request.Workload).Scan(&supported); err != nil {
			http.Error(w, "worker capabilities unavailable", 503)
			return
		}
		if !supported {
			http.Error(w, "upgrade and verify the worker before running restore drills", 409)
			return
		}
	}
	if in.Action == "rollback" {
		var exists bool
		if err = tx.QueryRow(r.Context(), "SELECT EXISTS(SELECT 1 FROM operations WHERE id=$1 AND workload_id=$2 AND state='succeeded' AND action IN ('deploy','rollback'))", in.Release, request.Workload).Scan(&exists); err != nil {
			http.Error(w, "database unavailable", 503)
			return
		}
		if !exists {
			http.Error(w, "select a successful release for this workload", 400)
			return
		}
	}

	if in.Action == "deploy" || in.Action == "rollback" {
		if err = loadNetworkRequest(r.Context(), tx, &request); err != nil {
			if errors.Is(err, errNetworkWorkerUnsupported) {
				http.Error(w, err.Error(), 409)
			} else {
				http.Error(w, "could not load network connections", 503)
			}
			return
		}
	}

	if err = worker.ValidateRequest(request); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	data, err = json.Marshal(request)
	if err != nil {
		http.Error(w, "invalid configuration", 500)
		return
	}
	payload, err := seal(s.Key, data)
	if err != nil {
		http.Error(w, "could not protect operation", 500)
		return
	}
	_, err = tx.Exec(r.Context(), "INSERT INTO operations(id,workload_id,action,payload,execution_node_id) VALUES($1,$2,$3,$4,NULLIF($5,''))", id, request.Workload, in.Action, payload, executionNode)
	if managedRegistryAdmissionError(w, err) {
		return
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		http.Error(w, "workload already has an active operation", 409)
		return
	}
	if err != nil {
		http.Error(w, "could not queue operation", 500)
		return
	}
	if in.Action == "build" {
		metadata, problem := json.Marshal(map[string]any{"build_recipe": worker.BuildRecipeSummary(request.BuildRecipe)})
		if problem != nil {
			http.Error(w, "build metadata unavailable", 500)
			return
		}
		if _, err = tx.Exec(r.Context(), `UPDATE operations SET result=$2 WHERE id=$1`, id, metadata); err != nil {
			http.Error(w, "build metadata could not be saved", 503)
			return
		}
		if request.BuildRecipe != nil && request.BuildRecipe.Engine == "buildpacks" {
			if err := reserveBuildpacks(r.Context(), tx, id, executionNode); err != nil {
				http.Error(w, "Cloud Native Buildpacks reservation could not be saved", 503)
				return
			}
		}
	}
	if in.Action == "image_check" {
		if _, err = tx.Exec(r.Context(), `UPDATE workloads SET next_image_check_at=now()+interval '1 hour' WHERE id=$1`, request.Workload); err != nil {
			http.Error(w, "could not schedule the next image check", 503)
			return
		}
	}
	if repair && in.Action != "restore" {
		if _, err = tx.Exec(r.Context(), `UPDATE environment_suspensions SET operation_id=$2,action='resume',error='' WHERE workload_id=$1`, request.Workload, id); err != nil {
			http.Error(w, "could not queue environment repair", 503)
			return
		}
	}
	if in.Action == "stop" || in.Action == "start" || in.Action == "restart" || in.Action == "deploy" || in.Action == "rollback" {
		if _, err = tx.Exec(r.Context(), "UPDATE workloads SET manually_stopped=$2 WHERE id=$1", request.Workload, in.Action == "stop"); err != nil {
			http.Error(w, "could not save workload intent", 500)
			return
		}
		if in.Action == "stop" {
			if _, err = tx.Exec(r.Context(), `UPDATE environment_suspensions SET resume_wanted=false WHERE workload_id=$1`, request.Workload); err != nil {
				http.Error(w, "could not save stop intent", 503)
				return
			}
			if _, err = tx.Exec(r.Context(), `UPDATE workloads SET job_pending_at=NULL WHERE id=$1`, request.Workload); err != nil {
				http.Error(w, "could not pause scheduled jobs", 503)
				return
			}
			if _, err = tx.Exec(r.Context(), "DELETE FROM webhook_deliveries WHERE workload_id=$1 AND operation_id IS NULL", request.Workload); err != nil {
				http.Error(w, "could not pause automatic deployments", 500)
				return
			}
			if _, err = tx.Exec(r.Context(), "UPDATE registry_webhooks SET pending_at=NULL,last_ignored_reason='Resource manually stopped; pending push ignored.' WHERE workload_id=$1", request.Workload); err != nil {
				http.Error(w, "could not pause registry deployments", 503)
				return
			}
		}
	}
	if err = tx.Commit(r.Context()); err != nil {
		http.Error(w, "could not queue operation", 500)
		return
	}
	respond(w, 202, map[string]string{"id": id, "state": "queued"})
}
func (s *Server) listOperations(w http.ResponseWriter, r *http.Request) {
	rows, err := s.DB.Query(r.Context(), "SELECT o.id,o.workload_id,o.action,o.state,o.result,o.created_at,CASE WHEN o.action='build' THEN COALESCE(o.execution_node_id,'') ELSE w.node_id END FROM operations o JOIN workloads w ON w.id=o.workload_id LEFT JOIN environments e ON e.id=w.environment_id WHERE ($1='' OR o.workload_id=$1) AND w.team_id=$3 AND ($4 OR cove_access($2,w.team_id,w.project_id,COALESCE(e.id,''))>0) ORDER BY o.created_at DESC LIMIT 100", r.URL.Query().Get("workload_id"), actor(r).ID, actor(r).Team, actor(r).Admin)
	if err != nil {
		http.Error(w, "database unavailable", 503)
		return
	}
	defer rows.Close()
	type item struct {
		ExecutionNode string          `json:"execution_node_id"`
		ID            string          `json:"id"`
		Workload      string          `json:"workload_id"`
		Action        string          `json:"action"`
		State         string          `json:"state"`
		Result        json.RawMessage `json:"result"`
		Created       time.Time       `json:"created_at"`
	}
	out := []item{}
	for rows.Next() {
		var v item
		if rows.Scan(&v.ID, &v.Workload, &v.Action, &v.State, &v.Result, &v.Created, &v.ExecutionNode) != nil {
			http.Error(w, "could not read operations", 500)
			return
		}
		out = append(out, v)
	}
	if rows.Err() != nil {
		http.Error(w, "could not read operations", 500)
		return
	}
	respond(w, 200, out)
}
func (s *Server) nodeRPC(ctx context.Context, nodeID string, request worker.RPCRequest) (worker.RPCResponse, error) {
	if s.workerRPC != nil {
		return s.workerRPC(ctx, nodeID, request)
	}
	var address, mode string
	var host *string
	var port int
	var encrypted []byte
	err := s.DB.QueryRow(ctx, "SELECT address,port,host_key,private_key,mode FROM nodes WHERE id=$1", nodeID).Scan(&address, &port, &host, &encrypted, &mode)
	if err != nil {
		return worker.RPCResponse{}, err
	}
	if mode == "local" {
		return s.localRPC(ctx, request)
	}
	if host == nil {
		return worker.RPCResponse{}, errors.New("node is not enrolled")
	}
	seed, err := unseal(s.Key, encrypted)
	if err != nil {
		return worker.RPCResponse{}, err
	}
	return callSSH(ctx, address, port, *host, seed, request)
}
func (s *Server) operationLogs(w http.ResponseWriter, r *http.Request) {
	var nodeID string
	var state string
	err := s.DB.QueryRow(r.Context(), "SELECT COALESCE(o.execution_node_id,w.node_id),o.state FROM operations o JOIN workloads w ON w.id=o.workload_id WHERE o.id=$1", r.PathValue("id")).Scan(&nodeID, &state)
	if errors.Is(err, pgx.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, "database unavailable", 503)
		return
	}
	if state == "queued" {
		respond(w, 200, map[string]string{"logs": ""})
		return
	}
	result, err := s.nodeRPC(r.Context(), nodeID, worker.RPCRequest{Method: "logs", ID: r.PathValue("id")})
	if err != nil {
		http.Error(w, "worker unavailable", 502)
		return
	}
	respond(w, 200, map[string]string{"logs": result.Logs})
}
func (s *Server) Dispatch(ctx context.Context) error {
	owner, err := s.DB.Acquire(ctx)
	if err != nil {
		return err
	}
	defer owner.Release()
	var acquired bool
	if err = owner.QueryRow(ctx, "SELECT pg_try_advisory_lock(1129272901)").Scan(&acquired); err != nil {
		return err
	}
	if !acquired {
		return errors.New("another Cove dispatcher owns this database")
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := owner.Exec(cleanup, "SELECT pg_advisory_unlock(1129272901)"); err != nil {
			owner.Conn().Close(cleanup)
		}
	}()
	timer := time.NewTicker(3 * time.Second)
	defer timer.Stop()
	dnsContext, stopDNS := context.WithCancel(ctx)
	dnsDone := make(chan error, 1)
	go func() { dnsDone <- s.runDNS(dnsContext) }()
	defer func() { stopDNS(); <-dnsDone }()
	tunnelDone := make(chan struct{})
	go func() { defer close(tunnelDone); s.runCloudflareTunnels(dnsContext) }()
	defer func() { stopDNS(); <-tunnelDone }()
	nextAuthCleanup := time.Time{}
	nextHealthCheck := time.Time{}
	for {
		if time.Now().After(nextAuthCleanup) {
			if _, err := s.DB.Exec(ctx, "DELETE FROM terminal_sessions WHERE expires_at<now(); DELETE FROM oauth_states WHERE expires_at<now(); DELETE FROM account_actions WHERE expires_at<now(); DELETE FROM sessions WHERE expires_at<now(); DELETE FROM login_attempts WHERE window_start<now()-interval '1 day'"); err != nil {
				return err
			}
			nextAuthCleanup = time.Now().Add(time.Hour)
		}
		if err := owner.Ping(ctx); err != nil {
			return err
		}
		if err := s.reconcileEnvironmentSuspensions(ctx); err != nil && ctx.Err() == nil {
			return err
		}
		if err := s.dispatchOperations(ctx); err != nil && ctx.Err() == nil {
			return err
		}
		if err := s.reconcileManagedRegistries(ctx); err != nil && ctx.Err() == nil {
			return err
		}
		if err := s.scheduleRestoreDrills(ctx); err != nil && ctx.Err() == nil {
			return err
		}
		// Registry admission rolls scheduling back; retain due work for the next pass.
		if err := s.scheduleJobs(ctx); err != nil && ctx.Err() == nil && !managedRegistryBusy(err) {
			return err
		}
		if err := s.scheduleImageChecks(ctx); err != nil && ctx.Err() == nil {
			return err
		}
		if err := s.scheduleCPUScales(ctx); err != nil && ctx.Err() == nil && !managedRegistryBusy(err) {
			return err
		}
		if err := s.scheduleCertificateDNSIntents(ctx); err != nil && ctx.Err() == nil {
			return err
		}
		if err := s.scheduleCertificateIssuers(ctx); err != nil && ctx.Err() == nil {
			return err
		}
		if err := s.scheduleUnhealthyRestarts(ctx); err != nil && ctx.Err() == nil {
			return err
		}
		if err := s.reconcilePreviews(ctx); err != nil && ctx.Err() == nil && !managedRegistryBusy(err) {
			return err
		}
		if err := s.reconcileEnvironmentPreviews(ctx); err != nil && ctx.Err() == nil && !managedRegistryBusy(err) {
			return err
		}
		if err := s.scheduleBackups(ctx); err != nil && ctx.Err() == nil {
			return err
		}
		if err := s.dispatchWebhooks(ctx); err != nil && ctx.Err() == nil && !managedRegistryBusy(err) {
			return err
		}
		if err := s.dispatchImageUpdates(ctx); err != nil && ctx.Err() == nil && !managedRegistryBusy(err) {
			return err
		}
		if err := s.dispatchRegistryWebhooks(ctx); err != nil && ctx.Err() == nil && !managedRegistryBusy(err) {
			return err
		}
		if err := s.dispatchNotifications(ctx); err != nil && ctx.Err() == nil {
			return err
		}
		if time.Now().After(nextHealthCheck) {
			if err := s.checkBackupHealth(ctx); err != nil && ctx.Err() == nil {
				return err
			}
			if err := s.checkResourceHealth(ctx); err != nil && ctx.Err() == nil {
				return err
			}
			nextHealthCheck = time.Now().Add(time.Minute)
		}
		if err := s.dispatchHealthNotifications(ctx); err != nil && ctx.Err() == nil {
			return err
		}
		if err := s.refreshNodes(ctx); err != nil && ctx.Err() == nil {
			return err
		}
		if err := s.reconcileLinks(ctx); err != nil && ctx.Err() == nil {
			return err
		}
		select {
		case <-ctx.Done():
			return nil
		case <-timer.C:
		}
	}
}
func (s *Server) dispatchOperations(ctx context.Context) error {
	rows, err := s.DB.Query(ctx, "SELECT o.id,COALESCE(o.execution_node_id,w.node_id),o.payload FROM operations o JOIN workloads w ON w.id=o.workload_id WHERE o.state IN ('queued','running') ORDER BY o.updated_at LIMIT 32")
	if err != nil {
		return err
	}
	type job struct {
		id, node string
		payload  []byte
	}
	var jobs []job
	for rows.Next() {
		var j job
		if err = rows.Scan(&j.id, &j.node, &j.payload); err != nil {
			rows.Close()
			return err
		}
		jobs = append(jobs, j)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	semaphore := make(chan struct{}, 8)
	failures := make(chan error, len(jobs))
	var wg sync.WaitGroup
	for _, j := range jobs {
		select {
		case semaphore <- struct{}{}:
		case <-ctx.Done():
			wg.Wait()
			return ctx.Err()
		}
		wg.Go(func() {
			defer func() { <-semaphore }()
			if err := s.processOperation(ctx, j.id, j.node, j.payload); err != nil {
				failures <- err
			}
		})
	}
	wg.Wait()
	close(failures)
	for err := range failures {
		return err
	}
	return nil
}
func (s *Server) processOperation(ctx context.Context, id, node string, payload []byte) error {
	ready, dependencyErr := s.dependencyReady(ctx, id)
	if dependencyErr != nil || !ready {
		return dependencyErr
	}
	admitted, err := s.admitDeployment(ctx, id)
	if err != nil {
		return err
	}
	if !admitted {
		return nil
	}
	data, err := unseal(s.Key, payload)
	if err != nil {
		return s.failOperation(ctx, id, "Operation configuration could not be decrypted.")
	}
	var request worker.Request
	if json.Unmarshal(data, &request) != nil {
		return s.failOperation(ctx, id, "Operation configuration is invalid.")
	}
	// Re-submission is safe only because the worker persists the immutable ID and input digest before acknowledging.
	var cancelRequested, buildPrepared, legacyBuild bool
	if err = s.DB.QueryRow(ctx, "SELECT cancel_requested,build_prepared,build_legacy FROM operations WHERE id=$1", id).Scan(&cancelRequested, &buildPrepared, &legacyBuild); err != nil {
		return err
	}
	if !cancelRequested && request.Action == "build" && request.BuildRecipe != nil && request.BuildRecipe.Engine == "buildpacks" {
		admitted, admission, err := s.admitBuildpacks(ctx, id, node)
		if err != nil || !admitted {
			return err
		}
		if request.BuildpacksAdmission != nil && *request.BuildpacksAdmission != *admission {
			return s.deferBuildResult(ctx, id)
		}
		request.BuildpacksAdmission = admission
	}
	if request.Action == "build" && !buildPrepared {
		// An older controller may already have submitted inputs that were not saved
		// in the database. Reconcile that journal without resolving new credentials.
		if existing, readErr := s.nodeRPC(ctx, node, worker.RPCRequest{Method: "status", ID: id}); readErr == nil && existing.Operation != nil {
			if cancelRequested {
				cancelled, cancelErr := s.cancelBuild(ctx, node, request)
				if cancelErr == nil && cancelled.Operation != nil {
					existing = cancelled
				}
			}
			return s.saveBuildOutcome(ctx, id, request, existing)
		}
		if legacyBuild {
			if cancelRequested {
				if cancelled, cancelErr := s.cancelBuild(ctx, node, request); cancelErr == nil && cancelled.Operation != nil {
					return s.saveBuildOutcome(ctx, id, request, cancelled)
				}
			}
			return s.deferBuildResult(ctx, id)
		}
	}
	if !cancelRequested {
		if err := s.prepareRouteCertificates(ctx, &request); err != nil {
			return s.failOperation(ctx, id, "Selected route certificates are unavailable; check certificate ownership and expiry.")
		}
	}
	// Route changes use no connections, and failing one here could release hostnames while the worker still applies it.
	if !cancelRequested && !(request.Action == "build" && buildPrepared) && request.Scale == nil && request.Action != "routes" && request.Action != "suspend" && request.Action != "resume" && request.Action != "idle_policy" {
		if err = s.prepareInfrastructure(ctx, &request); err != nil {
			if request.Action == "image_scan" {
				return s.failOperation(ctx, id, "Image scanning is no longer enabled for this team and ready worker.")
			}
			if errors.Is(err, errSharedVariables) {
				return s.failOperation(ctx, id, "Shared variable resolution failed; check for missing names in this resource's team, project, and environment, or values exceeding 64 KiB.")
			}
			return s.failOperation(ctx, id, "Infrastructure connection unavailable; check the selected registry or storage location.")
		}
		if err = s.prepareAppDeployment(ctx, &request); err != nil {
			return s.failOperation(ctx, id, "GitHub repository access unavailable; check the app installation and reconnect the resource.")
		}
	}
	if request.Action == "build" && !buildPrepared {
		prepared, prepareErr := s.sealJSON(request)
		if prepareErr != nil {
			return prepareErr
		}
		if _, err = s.DB.Exec(ctx, `UPDATE operations SET payload=$2,build_prepared=true WHERE id=$1`, id, prepared); err != nil {
			return err
		}
	}
	method := "submit"
	if request.BuildPlatform != "" {
		method = "submit_build_v2"
	}
	if request.BuildRecipe != nil {
		method = "submit_build_recipe"
	}
	if request.ImageUpdates != nil {
		method = "submit_image_updates"
	}
	if request.Scale != nil {
		method = "submit_cpu_scale"
	}
	if worker.HasRouteExtensions(request.Routes) {
		method = "submit_routes_v2"
	}
	if worker.RouteProtocol(request.Routes) >= 3 {
		method = "submit_routes_v3"
	}
	if worker.RouteProtocol(request.Routes) >= 4 {
		method = "submit_routes_v4"
	}
	if worker.RouteProtocol(request.Routes) >= 5 {
		method = "submit_routes_v5"
	}
	if worker.RouteProtocol(request.Routes) >= 6 {
		method = "submit_routes_v6"
	}
	if cancelRequested {
		method = "cancel"
		if request.Action == "build" {
			method = "cancel_build"
		}
	}
	response, err := s.nodeRPC(ctx, node, worker.RPCRequest{Method: method, Request: &request})
	if request.Action == "build" && cancelRequested && err != nil {
		var remote *RemoteError
		if errors.As(err, &remote) {
			response, err = s.nodeRPC(ctx, node, worker.RPCRequest{Method: "cancel", Request: &request})
		}
	}
	if request.Action == "build" && err != nil {
		response, err = s.nodeRPC(ctx, node, worker.RPCRequest{Method: "status", ID: id})
		if err != nil {
			return s.deferBuildResult(ctx, id)
		}
	}
	if request.Action == "build" {
		return s.saveBuildOutcome(ctx, id, request, response)
	}
	if err != nil && cancelRequested {
		// A failed cancellation is not evidence that execution stopped. Keep the
		// workload locked until the worker reports a terminal operation state.
		response, err = s.nodeRPC(ctx, node, worker.RPCRequest{Method: "status", ID: request.ID})
		if err != nil {
			_, err = s.DB.Exec(ctx, "UPDATE operations SET updated_at=now() WHERE id=$1", id)
			return err
		}
	}
	if err != nil {
		if request.Action == "routes" {
			return s.deferRouteResult(ctx, id)
		}
		if request.Action == "idle_policy" {
			return s.deferIdlePolicyResult(ctx, id)
		}
		var rejected *RemoteError
		if errors.As(err, &rejected) {
			return s.failOperation(ctx, id, "Worker rejected the operation; check its configuration.")
		}
		_, err = s.DB.Exec(ctx, "UPDATE operations SET updated_at=now() WHERE id=$1", id)
		return err
	}
	if response.Operation == nil {
		if request.Action == "routes" {
			return s.deferRouteResult(ctx, id)
		}
		if request.Action == "idle_policy" {
			return s.deferIdlePolicyResult(ctx, id)
		}
		return s.failOperation(ctx, id, "Worker returned an invalid response.")
	}
	state := response.Operation.State
	switch state {
	case "queued", "running", "succeeded", "failed", "interrupted", "cancelled":
	default:
		if request.Action == "routes" {
			return s.deferRouteResult(ctx, id)
		}
		if request.Action == "idle_policy" {
			return s.deferIdlePolicyResult(ctx, id)
		}
		return s.failOperation(ctx, id, "Worker returned an unsupported state.")
	}
	result, err := json.Marshal(response.Operation)
	if err != nil {
		return err
	}
	if request.Scale != nil && state == "succeeded" {
		if !confirmedCPUScale(request.Scale, response.Operation) {
			_, err = s.DB.Exec(ctx, `UPDATE operations SET result=$2,updated_at=now() WHERE id=$1`, id, []byte(`{"error":"Waiting for the worker to confirm the CPU scaling outcome; the resource remains locked."}`))
			return err
		}
		return s.commitCPUScale(ctx, id, request, result)
	}
	if request.Action == "idle_policy" && state == "succeeded" {
		return s.commitIdlePolicy(ctx, id, request, result)
	}
	if request.Action == "routes" && state == "succeeded" {
		return s.commitRoutes(ctx, id, request, result)
	}
	if rollout := response.Operation.Rollout; rollout != nil && rollout.Release != "" && rollout.Release != id && (rollout.Phase == "completed" || rollout.Phase == "aborted") {
		tx, err := s.DB.Begin(ctx)
		if err != nil {
			return err
		}
		defer tx.Rollback(ctx)
		if _, err = tx.Exec(ctx, `UPDATE operations SET result=jsonb_set(result,'{rollout,phase}',to_jsonb($3::text)),updated_at=now() WHERE id=$1 AND workload_id=$2 AND action IN ('deploy','rollback') AND result->'rollout'->>'phase'='waiting'`, rollout.Release, request.Workload, rollout.Phase); err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, "UPDATE operations SET state=$1,result=$2,updated_at=now() WHERE id=$3", state, result, id); err != nil {
			return err
		}
		return tx.Commit(ctx)
	}
	_, err = s.DB.Exec(ctx, "UPDATE operations SET state=$1,result=$2,updated_at=now() WHERE id=$3", state, result, id)
	return err
}

func (s *Server) failOperation(ctx context.Context, id, message string) error {
	result, err := json.Marshal(map[string]string{"error": message})
	if err != nil {
		return err
	}
	_, err = s.DB.Exec(ctx, `UPDATE operations SET state='failed',result=CASE WHEN action='build' AND result ? 'build_recipe' THEN $1::jsonb||jsonb_build_object('build_recipe',result->'build_recipe') ELSE $1::jsonb END,updated_at=now() WHERE id=$2`, result, id)
	return err
}

// An ambiguous route reply cannot release hostname claims: the journal may exist
// even when the worker reports a submit error after renaming it onto disk.
func (s *Server) deferRouteResult(ctx context.Context, id string) error {
	result, err := json.Marshal(map[string]string{"error": "Waiting for the worker to confirm the route outcome; hostnames remain reserved."})
	if err != nil {
		return err
	}
	_, err = s.DB.Exec(ctx, "UPDATE operations SET result=$2,updated_at=now() WHERE id=$1", id, result)
	return err
}

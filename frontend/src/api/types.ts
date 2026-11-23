export type NodeState = "pending" | "enrolled" | "unreachable" | "ssh_verified" | "ready";

// A filesystem mounted in the worker's namespace. Pseudo filesystems report known=false without an error;
// an error means the mount could not be read. Neither has a capacity reading.
export interface MountMetric { path: string; filesystem: string; total: number; available: number; known: boolean; error?: string }
export interface NodeMetrics { sampled_at: string; cpu_percent: number; memory_total: number; memory_available: number; disk_total: number; disk_available: number; network_rx?: number; network_tx?: number; cpus?: number; mounts?: MountMetric[] | null; mounts_limited?: boolean }
export interface Capabilities {
  metrics?: NodeMetrics;
  shared_host?: { coolify_detected: boolean; cleanup_verified: boolean };
  resource_limits?: boolean;
  terminal?: boolean;
  exec?: boolean;
  infrastructure?: boolean;
  imports?: boolean;
  adoption?: boolean;
  backup_retention?: boolean;
  extended_logical_backups?: boolean;
  backup_transfers?: boolean;
  backblaze_b2?: boolean;
  file_browser?: boolean;
  alert_observations?: boolean;
  scheduled_cleanup?: boolean;
  journal_retention?: boolean;
  mount_metrics?: boolean;
  structured_logs?: boolean;
  capacity_advice?: boolean;
  filesystem_snapshots?: boolean;
  traffic_logs?: boolean;
  route_policies?: boolean;
  // 2 means the worker applies maintenance pages, custom error pages and forward-auth. Missing, 0 and 1 do not.
  route_protocol?: number;
  // Docker Buildx is present, so the worker can check deployed images and pull before redeploying.
  image_lifecycle?: boolean;
  // Buildx, the Compose builder and pushing the local platform are available, so the worker can build and publish images.
  build_publish?: boolean;
  // The worker supports rolling, blue/green and canary replacements with promote and abort.
  rollouts?: boolean;
  // The worker enforces job timeouts, reports job results and stops every service a job run started.
  job_runs?: boolean;
  // The worker can recheck Docker health and restart only the running containers that report unhealthy.
  unhealthy_restart?: boolean;
  // The worker can pause and resume a resource for its environment, keeping the containers it captured.
  suspensions?: boolean;
  // The worker can stop idle request-driven services and wake them on the next authorized request.
  idle_sleep?: boolean;
  // The worker reports per-service CPU samples, so the panel can scale rolling services between replica bounds.
  cpu_autoscale?: boolean;
  protocol: number;
  os: string;
  arch: string;
  docker: string;
  compose: string;
  ready: boolean;
}

export interface Node {
  enrollment_expires_at?: string | null;
  id: string;
  name: string;
  address: string;
  port: number;
  state: NodeState;
  capabilities: Capabilities | null;
  mode: "ssh" | "local";
  last_checked_at: string | null;
}

export interface NodeInput {
  name: string;
  address: string;
  port: number;
}

export interface Enrollment {
  id: string;
  expires_at: string;
  setup_command: string;
}

export interface VerifyResult {
  state: NodeState;
  capabilities: Capabilities | null;
}

// The fixed custom-role catalogue; GET /api/session reports which of these apply team-wide in the active team.
export type TeamPermission = "view" | "read_secrets" | "operate" | "edit_configuration" | "manage_backups" | "manage_members";
// Project, environment and resource permissions: legacy read/operate/edit plus the exact catalogue names. Preview
// details report the legacy names only. Edit never implies read_secrets.
export type Permission = "read" | "operate" | "edit" | TeamPermission;
export type TeamRole = "owner" | "admin" | "member";
export type GrantRole = "viewer" | "operator" | "editor";

export interface Grant {
  scope: "team" | "project" | "environment";
  scope_id: string;
  // A built-in GrantRole or the ID of one of the team's custom roles.
  role: string;
}

export interface TeamMembership {
  id: string;
  name: string;
  role: TeamRole;
  grants: Grant[];
}

export interface AuthConfig {
  setup_required: boolean;
  registration_enabled: boolean;
  github_enabled: boolean;
  email_enabled: boolean;
}

export interface WorkloadSummary {
  permissions: Permission[];
  project_id: string;
  environment_id?: string;
  environment?: string;
  project?: string;
  id: string;
  name: string;
  node_id: string;
  node: string;
  kind: ResourceKind;
}

export interface GitSource {
  inline_compose?: boolean;
  repository: string;
  branch: string;
  compose_path: string;
  watch_paths?: string[];
  // The pinned public host key of an ssh:// repository. Its deploy key is managed separately and never sent to the browser.
  ssh_host_key?: string;
}

export interface GitSourceInput extends GitSource {
  token?: string;
}

export interface GitHubSourceConnection {
  installation_id: number;
  repository_id: number;
  automatic: boolean;
}

export interface GitDeployKey {
  public_key: string;
  repository: string;
  host_key: string;
}

export type WorkloadKind = "service" | "job";
// The public kind of a resource. Managed databases run as services on the worker but are listed separately.
export type ResourceKind = WorkloadKind | "database";

export interface DatabaseResource {
  engine: DatabaseEngineId;
  version: string;
  template_revision: string;
  service: string;
  image: string;
  port: number;
  host: string;
  variant?: "vector" | "postgis" | "";
  // Extensions enabled through Cove maintenance; the runtime may hold others.
  extensions?: string[];
  pooler?: boolean;
  // The pooler's canonical private alias; present only while the pooler is enabled.
  pooler_host?: string;
  public_access?: DatabasePublicAccess | null;
  // The original private hostname while public TLS access replaces host with the certificate hostname.
  private_host?: string;
  settings_operation?: string;
}

export interface DatabasePublicAccess {
  hostname: string;
  bind_address: string;
  port: number;
  fingerprint: string;
  expires_at: string;
}

export type DatabaseEngineId = "postgresql" | "mysql" | "mariadb" | "mongodb" | "redis" | "valkey" | "clickhouse";

export interface ServiceLimits { cpus: number; memory_mb: number }

export interface DeploymentHook { service: string; command: string }
export interface DeploymentAssertions {
  volumes?: { name: string; labels?: Record<string, string> }[];
  networks?: { name: string; internal: boolean; labels?: Record<string, string> }[];
  services?: { service: string; image: string; networks: string[]; mounts: { volume: string; target: string; read_only: boolean }[] }[];
}
export interface DeploymentHooks { before?: DeploymentHook; after?: DeploymentHook; before_steps?: DeploymentHook[]; assertions?: DeploymentAssertions }

export type RolloutStrategy = "disabled" | "rolling" | "blue_green" | "canary";

export interface HTTPHealthCheck { port: number; path: string }

export interface RolloutOptions {
  strategy: RolloutStrategy;
  replicas?: Record<string, number>;
  http_checks?: Record<string, HTTPHealthCheck>;
  parallel_volumes: boolean;
  drain_seconds: number;
  timeout_seconds: number;
  canary_percent: number;
}

export type RolloutPhase = "preparing" | "replacing" | "waiting" | "promoting" | "aborting" | "draining" | "completed" | "aborted";

export interface RolloutStatus {
  operation: string;
  // The candidate deploy's operation ID.
  release?: string;
  strategy: Exclude<RolloutStrategy, "disabled"> | "guarded";
  phase: RolloutPhase;
  reason?: string;
  candidate_percent: number;
  replicas?: Record<string, number>;
}

export interface Workload {
  rollout?: RolloutOptions | null;
  preview_source_id?: string;
  // Set (non-empty) when the resource belongs to a managed preview environment.
  environment_preview_id?: string;
  guarded_deploy?: boolean;
  file_secrets?: FileSecret[];
  hooks?: DeploymentHooks;
  limits?: Record<string, ServiceLimits>;
  manually_stopped: boolean;
  // The environment's requested pause state. It is intent, not proof that every container has stopped.
  environment_paused?: boolean;
  // True once a deploy or rollback has succeeded, so route changes apply without redeploying.
  deployed: boolean;
  permissions: Permission[];
  // Set while a node adoption that reserved this resource is still pending; the resource is read-only until it completes.
  pending_adoption_id?: string;
  project_id: string;
  environment_id?: string;
  // The API sends null when the resource has no routes.
  routes?: Route[] | null;
  id: string;
  name: string;
  node_id: string;
  // Empty or missing means a long-running service.
  kind?: ResourceKind | "";
  // Present only on managed databases.
  database?: DatabaseResource | null;
  job_service?: string;
  compose: string;
  environment_keys: string[];
  source: GitSource | null;
  has_source_token: boolean;
}

export interface WorkloadInput {
  // Omitted on update to keep the saved options; send strategy "disabled" to turn rollouts off.
  rollout?: RolloutOptions;
  guarded_deploy?: boolean;
  file_secrets?: FileSecret[];
  hooks?: DeploymentHooks;
  limits?: Record<string, ServiceLimits>;
  // Accepted on create only; placement changes go through the reviewed move endpoint.
  project_id?: string;
  environment_id?: string;
  routes?: Route[];
  name: string;
  node_id: string;
  kind: WorkloadKind;
  job_service?: string;
  compose: string;
  source: GitSourceInput | null;
  // Omitted on update to keep the saved variables.
  environment?: Record<string, string>;
  environment_patch?: Record<string, string | null>;
}

export interface RouteUpstream { service: string; port: number }

// Raw worker diagnostics, read before the panel removes events inside operation windows, so these are not alert counts.
export interface WorkloadObservations {
  sampled_at: string;
  // manual_stop and exit_unknown are absent from workers that predate exit classification.
  containers: { id: string; restart_count: number; oom_killed: boolean; exit_code: number; exited: boolean; expected_completed: boolean; manual_stop?: boolean; exit_unknown?: boolean }[];
  certificates: { domain: string; expires_at: string | null }[];
  events_available: boolean;
  restart_events: number;
  oom_events: number;
  crash_events: number;
}

export interface RouteMaintenance { enabled: boolean; html?: string }

// Either a public HTTPS url, or a Compose service and port in the same resource with an optional path.
export interface RouteForwardAuth {
  url?: string;
  service?: string;
  port?: number;
  path?: string;
  response_headers?: string[];
  response_cookies?: string[];
}

// The API keeps a saved maintenance, error_pages or forward_auth value when the field is omitted; null removes it.
export interface RoutePolicy {
  maintenance?: RouteMaintenance | null;
  error_pages?: Record<string, string> | null;
  forward_auth?: RouteForwardAuth | null;
  path_prefix?: string;
  request_headers?: Record<string, string>;
  response_headers?: Record<string, string>;
  security_headers?: boolean;
  ip_allow_list?: string[];
  redirect_host?: string;
  redirect_permanent?: boolean;
  certificate_source?: "acme" | "default";
  upstreams?: RouteUpstream[];
  sticky?: boolean;
}

export interface Route {
  domain: string;
  service: string;
  port: number;
  // An htpasswd bcrypt entry set through the API. The dashboard has no editor for it and passes it through unchanged.
  auth?: string;
  policy?: RoutePolicy;
}

export type OperationAction = "deploy" | "stop" | "start" | "restart" | "rollback" | "backup" | "restore" | "drill" | "restore_copy" | "preview_remove" | "routes" | "adopt" | "image_check" | "build" | "rollout_promote" | "rollout_abort" | "restart_unhealthy" | "suspend" | "resume" | "idle_policy";
export type OperationState = "queued" | "running" | "succeeded" | "failed" | "interrupted" | "cancelled";

export interface WorkerOperation {
  id: string;
  workload: string;
  action: OperationAction;
  state: OperationState;
  created_at: string;
  finished_at?: string;
  error?: string;
  revision?: string;
  image_check?: ImageCheck;
  build?: BuildResult;
  rollout?: RolloutStatus;
  // Reported only by workers with the job_runs capability.
  job?: JobResult;
  // Reported by environment pause and resume operations, and by a Start that wakes a sleeping app (reason idle).
  suspension?: SuspensionResult;
  // Present on a deploy the panel's CPU scaler queued: a rolling replacement of the current release with new replica counts.
  scale?: ScaleResult;
}

export interface ScaleResult {
  // The deployed generation the CPU samples were read from.
  generation: string;
  before: Record<string, number>;
  replicas: Record<string, number>;
  // Each replica's CPU as a percentage of its CPU limit, averaged over all replicas of the service.
  cpu_percent: Record<string, number>;
}

export type SuspensionPhase = "active" | "pausing" | "paused" | "resuming" | "error";

export interface SuspendedResource {
  id: string;
  name: string;
  kind: WorkloadKind;
  node_id: string;
  // False until the resource's worker reports the suspensions capability.
  supported: boolean;
  phase: SuspensionPhase;
  // Missing until the control plane has queued the resource's pause or resume operation.
  operation_id?: string;
  // True only after the worker confirmed the service was running before the pause and was not stopped manually since.
  resume_wanted: boolean;
  error?: string;
}

// paused is the requested state; phase and resources report what the queue and workers have actually done.
export interface EnvironmentSuspension {
  paused: boolean;
  phase: SuspensionPhase;
  resources: SuspendedResource[];
}

export interface ResourceSuspension {
  operation: string;
  scope: string;
  // idle: the worker put the service to sleep after its idle timeout; environment: the environment was paused;
  // manual_stop: the worker holds containers that were deliberately stopped, which Start, Restart or Deploy repairs.
  reason: "environment" | "idle" | "manual_stop";
  phase: "pausing" | "paused" | "resuming";
  captured: number;
  previously_running: number;
}

export interface SuspensionResult extends Omit<ResourceSuspension, "phase"> {
  phase: ResourceSuspension["phase"] | "resumed" | "stopped";
}

export interface JobResult {
  // Docker's exit status for the job service. Missing means the exit status is unknown, not success.
  exit_code?: number;
  timed_out: boolean;
  // False means the worker could not confirm that every container the run started has stopped.
  containers_stopped: boolean;
  logs_unavailable?: boolean;
}

export type JobOverlapPolicy = "skip" | "queue_one" | "replace";

export interface JobSchedule {
  cron: string;
  timezone: string;
  next_run_at: string | null;
  overlap_policy: JobOverlapPolicy;
  timeout_seconds: number;
  // Set while one coalesced scheduled run waits for the current operation to finish.
  pending_since: string | null;
}

// Omitted execution fields keep the saved values.
export interface JobScheduleInput {
  cron: string;
  timezone?: string;
  overlap_policy?: JobOverlapPolicy;
  timeout_seconds?: number;
}

export interface BuiltImage {
  service: string;
  image_id: string;
  // An immutable registry reference: registry/path@sha256:...
  reference: string;
}

// Present only when a build succeeded and every published image was verified in the registry.
export interface BuildResult {
  images: BuiltImage[];
}

export interface BuildOptions {
  // A lowercase repository path within the resource's registry, without host, tag or digest.
  repository: string;
  no_cache?: boolean;
}

export type ImageStatusValue = "current" | "update_available" | "pinned" | "unpublished" | "unknown";

export interface ImageStatus {
  service: string;
  reference: string;
  image_id: string;
  registry_digest?: string;
  status: ImageStatusValue;
  message?: string;
  release_notes?: string;
}

export interface ImageCheck {
  checked_at: string;
  images: ImageStatus[];
}

// The most recent successful image check. Stale means a deploy or rollback succeeded after it.
export type WorkloadImages =
  | { images: ImageStatus[]; stale: boolean; checked_at: null; operation_id?: undefined }
  | { images: ImageStatus[]; stale: boolean; checked_at: string; operation_id: string };

export interface OperationRequest {
  // Only the panel's health monitor queues restart_unhealthy, only environment pause and resume queue suspend and resume, and idle policy changes and adoption have their own endpoints; the API rejects them here.
  action: Exclude<OperationAction, "routes" | "adopt" | "restart_unhealthy" | "suspend" | "resume" | "idle_policy">;
  // Deploy, rollback, rollout promote and abort, and build only (acceptsReason); the API rejects it on other actions.
  reason?: string;
  // Deploy only: pull the configured tags before building and replacing containers.
  pull_latest?: boolean;
  build?: BuildOptions;
}

// Written by the control plane instead of a worker report: a policy, approval or quota hold (hold_reason, with
// error repeated for older clients) or a failure recorded before any worker accepted the operation.
export interface ControlPlaneResult {
  error: string;
  hold_reason?: string;
}

export interface Operation {
  approval_required?: boolean;
  // Present on the detail endpoint only; database maintenance cannot be cancelled once queued.
  can_cancel?: boolean;
  approved_at?: string | null;
  can_approve?: boolean;
  cancel_requested?: boolean;
  // Optional free text given when a deploy, rollback, promotion or sequence was queued.
  reason?: string;
  // Present on the detail endpoint only.
  emergency_override_by?: string | null;
  emergency_override_reason?: string;
  emergency_override_at?: string | null;
  can_override?: boolean;
  id: string;
  workload_id: string;
  action: OperationAction;
  state: OperationState;
  result: WorkerOperation | ControlPlaneResult | null;
  created_at: string;
}

// Automatic restart of unhealthy containers. Attempts and the window cover the last 60 minutes; window_started_at is the
// oldest attempt still counted. last_error is empty or a safe message about the most recent failed health read.
export interface RestartPolicy {
  enabled: boolean;
  last_queued_at: string | null;
  attempts_in_window: number;
  window_started_at: string | null;
  operation_id: string | null;
  last_error: string;
}

// The saved idle sleep policy. It changes only after the worker applies an idle_policy operation.
export interface IdlePolicy {
  enabled: boolean;
  after_seconds: number;
  request_driven: boolean;
}

export type IdlePhase = "awake" | "sleeping_pending" | "quiescing" | "sleeping" | "wake_queued" | "waking" | "wake_failed" | "paused" | "blocked" | "unknown";

// What the worker's idle helper reports now. phase stays a string because newer workers may add phases.
export interface IdleRuntime {
  enabled: boolean;
  after_seconds: number;
  phase: string;
  active_requests: number;
  // When the idle timer last reset: a request finished, or the app was started, restarted or woken.
  last_activity?: string;
  last_sleep?: string;
  last_wake?: string;
  error?: string;
}

export interface AutoscaleService {
  min_replicas: number;
  max_replicas: number;
  target_cpu_percent: number;
}

// Saved through the CPU scaling endpoint only; ordinary resource saves keep it unchanged.
export interface AutoscalePolicy {
  enabled: boolean;
  services: Record<string, AutoscaleService>;
}

export interface CPUSample {
  sampled_at: string;
  generation: string;
  // Deployed replicas per service when the sample was read, not the configured count.
  replicas: Record<string, number>;
  cpu_percent: Record<string, number>;
}

// status stays a string because newer panels may add states.
export interface AutoscaleState {
  policy: AutoscalePolicy;
  // At most the latest 3 samples, oldest first.
  samples: CPUSample[] | null;
  last_queued_at: string | null;
  // Empty when the scaler has not queued an operation.
  operation_id: string;
  status: string;
  last_error: string;
}

export interface QueuedOperation {
  id: string;
  state: "queued";
}

export type RoutesUpdate = QueuedOperation | { state: "saved" | "unchanged" };

export interface OperationLogs {
  logs?: string;
  error?: string;
}

// Parsed numeric usage. Bytes except CPU, which is a percentage of one core and can exceed 100. Network and
// block I/O are cumulative counters. Fields the worker could not parse are omitted, never zero.
export interface ContainerUsage {
  cpu_percent?: number;
  memory_used?: number;
  memory_limit?: number;
  network_rx?: number;
  network_tx?: number;
  block_read?: number;
  block_written?: number;
}

export interface Container {
  metrics?: { cpu: string; memory: string; memory_percent: string; network: string; disk_io: string; pids: string; usage?: ContainerUsage };
  metrics_unavailable?: boolean;
  // Compose service label; absent in samples from workers that predate it.
  service?: string;
  id: string;
  name: string;
  image: string;
  state: string;
  status: string;
  ports: string;
  // True when Docker publishes at least one port on every interface (0.0.0.0 or ::).
  exposed_ports?: boolean;
  // Docker healthcheck result; omitted when the container has no healthcheck or the worker predates typed health.
  health?: "starting" | "healthy" | "unhealthy";
  // Present only for exited or dead containers, including 0.
  exit_code?: number;
  // True for exited (0) init services that other services wait on with service_completed_successfully.
  expected_completed?: boolean;
}

export interface ExecRequest { service: string; container?: string; command: string[]; timeout_seconds: number }
export interface ExecResult { output: string; exit_code: number; truncated: boolean; timed_out: boolean }

export interface WorkloadRuntime {
  containers: Container[];
  // Null when no replacement is pending or staged. This, not operation history, is the current rollout state.
  rollout?: RolloutStatus | null;
  // Null when the worker holds no containers stopped for the resource. Present from the stop (environment pause, idle sleep or manual stop) until it is resumed; `reason` says which.
  suspension?: ResourceSuspension | null;
  // Null or missing when the worker has no idle policy for the resource.
  idle?: IdleRuntime | null;
}

export interface FileSecret { name: string; variable: string; services: string[]; build_services: string[] }

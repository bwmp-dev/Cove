export type NodeState = "pending" | "enrolled" | "unreachable" | "ssh_verified" | "ready";

export interface NodeMetrics { sampled_at: string; cpu_percent: number; memory_total: number; memory_available: number; disk_total: number; disk_available: number }
export interface Capabilities {
  metrics?: NodeMetrics;
  shared_host?: { coolify_detected: boolean; cleanup_verified: boolean };
  resource_limits?: boolean;
  terminal?: boolean;
  infrastructure?: boolean;
  route_policies?: boolean;
  // Docker Buildx is present, so the worker can check deployed images and pull before redeploying.
  image_lifecycle?: boolean;
  // Buildx, the Compose builder and pushing the local platform are available, so the worker can build and publish images.
  build_publish?: boolean;
  // The worker supports rolling, blue/green and canary replacements with promote and abort.
  rollouts?: boolean;
  // The worker enforces job timeouts, reports job results and stops every service a job run started.
  job_runs?: boolean;
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

export type Permission = "read" | "operate" | "edit";
export type TeamRole = "owner" | "admin" | "member";
export type GrantRole = "viewer" | "operator" | "editor";

export interface Grant {
  scope: "team" | "project" | "environment";
  scope_id: string;
  role: GrantRole;
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
  kind: WorkloadKind;
}

export interface GitSource {
  inline_compose?: boolean;
  repository: string;
  branch: string;
  compose_path: string;
  watch_paths?: string[];
}

export interface GitSourceInput extends GitSource {
  token?: string;
}

export type WorkloadKind = "service" | "job";

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
  // True once a deploy or rollback has succeeded, so route changes apply without redeploying.
  deployed: boolean;
  permissions: Permission[];
  project_id: string;
  environment_id?: string;
  routes?: Route[];
  id: string;
  name: string;
  node_id: string;
  // Empty or missing means a long-running service.
  kind?: WorkloadKind | "";
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
  // Accepted on create only; placement changes go through the environment endpoint.
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

export interface RoutePolicy {
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

export type OperationAction = "deploy" | "stop" | "start" | "restart" | "rollback" | "backup" | "restore" | "drill" | "restore_copy" | "preview_remove" | "routes" | "image_check" | "build" | "rollout_promote" | "rollout_abort";
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
  action: Exclude<OperationAction, "routes">;
  // Deploy only: pull the configured tags before building and replacing containers.
  pull_latest?: boolean;
  build?: BuildOptions;
}

export interface Operation {
  approval_required?: boolean;
  approved_at?: string | null;
  can_approve?: boolean;
  cancel_requested?: boolean;
  id: string;
  workload_id: string;
  action: OperationAction;
  state: OperationState;
  result: WorkerOperation | null;
  created_at: string;
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

export interface Container {
  metrics?: { cpu: string; memory: string; memory_percent: string; network: string; disk_io: string; pids: string };
  metrics_unavailable?: boolean;
  id: string;
  name: string;
  image: string;
  state: string;
  status: string;
  ports: string;
  // Docker healthcheck result; omitted when the container has no healthcheck or the worker predates typed health.
  health?: "starting" | "healthy" | "unhealthy";
  // Present only for exited or dead containers, including 0.
  exit_code?: number;
  service?: string;
  // True for exited (0) init services that other services wait on with service_completed_successfully.
  expected_completed?: boolean;
}

export interface WorkloadRuntime {
  containers: Container[];
  // Null when no replacement is pending or staged. This, not operation history, is the current rollout state.
  rollout?: RolloutStatus | null;
}

export interface FileSecret { name: string; variable: string; services: string[]; build_services: string[] }

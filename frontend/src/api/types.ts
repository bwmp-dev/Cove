export type NodeState = "pending" | "enrolled" | "unreachable" | "ssh_verified" | "ready";

export interface NodeMetrics { sampled_at: string; cpu_percent: number; memory_total: number; memory_available: number; disk_total: number; disk_available: number }
export interface Capabilities {
  metrics?: NodeMetrics;
  shared_host?: { coolify_detected: boolean; cleanup_verified: boolean };
  resource_limits?: boolean;
  terminal?: boolean;
  infrastructure?: boolean;
  route_policies?: boolean;
  // 2 means the worker applies maintenance pages, custom error pages and forward-auth; 3 adds stored route
  // certificates; 4 adds wildcard hostnames and the proxy default certificate choice. Missing, 0 and 1 support none.
  route_protocol?: number;
  // Docker Buildx is present, so the worker can check deployed images and pull before redeploying.
  image_lifecycle?: boolean;
  // With image_lifecycle, the worker can check a job's retained containers from its last run without running the job.
  job_image_lifecycle?: boolean;
  // Buildx, the Compose builder and pushing the local platform are available, so the worker can build and publish images.
  build_publish?: boolean;
  // With build_publish, the worker can build for resources that run on another node and clean those builds up.
  remote_builds?: boolean;
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
  // The worker can manage the shared proxy's public bindings, HTTP/3 and extra listeners. Only Linux workers report it.
  ingress_exposures?: boolean;
  // The worker can run a locally managed Cloudflare Tunnel connector for this node's public HTTP routes.
  cloudflare_publishing?: boolean;
  // The worker can discover newer image tags and digests for image update policies and deploy exact pinned images.
  image_updates?: boolean;
  // The worker can run and maintain a team-owned managed registry. Creating one also needs file_secrets.
  managed_registry?: boolean;
  // The worker mounts saved variables as files under /run/secrets.
  file_secrets?: boolean;
  // The worker can deploy previews from CI-built registry images. Eligibility comes from the server.
  preview_ci_images?: boolean;
  // The worker can verify and enforce a hard build-cache cap on a dedicated filesystem. Linux workers only.
  build_cache_limits?: boolean;
  // The worker can build with explicit static output and Railpack recipes. The build recipe read decides whether a resource can use them.
  build_recipes?: boolean;
  // The worker can build Cloud Native Buildpacks recipes. Eligibility and its reasons come from the build recipe read.
  buildpacks?: boolean;
  // The worker can plan and build Nixpacks recipes. Eligibility and its reasons come from the build recipe read.
  nixpacks?: boolean;
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
  // The environment's requested pause state. It is intent, not proof that every container has stopped.
  environment_paused?: boolean;
  // True once a deploy or rollback has succeeded, so route changes apply without redeploying.
  deployed: boolean;
  permissions: Permission[];
  project_id: string;
  environment_id?: string;
  routes?: Route[] | null;
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
  certificate_source?: "acme" | "default" | "uploaded";
  // Set only with certificate_source "uploaded": a certificate stored for this resource, uploaded or issued. Like the extensions above, a
  // saved selection is kept while both certificate fields are omitted; null, "acme" or "default" clears it.
  certificate_id?: string | null;
  upstreams?: RouteUpstream[];
  sticky?: boolean;
  // Route protocol 5. A private route ("lan" or "tailscale") is served only on the named managed HTTPS listener of the
  // same visibility. While both are omitted the API keeps the hostname's saved values, so switching a private hostname
  // back to public sends "public" with a null listener.
  visibility?: RouteVisibility;
  listener?: string | null;
  // Route protocol 6, HTTP routes only. Whole seconds; an omitted or zero field keeps the proxy default. Kept like the
  // extensions above while omitted; null clears saved timeouts.
  timeouts?: RouteTimeouts | null;
}

export interface RouteTimeouts {
  dial_seconds?: number;
  response_header_seconds?: number;
  idle_connection_seconds?: number;
}

// "tcp" and "udp" routes (route protocol 6) have an empty domain and forward raw bytes from one dedicated managed
// listener; their policy holds only listener, visibility, upstreams and, for TCP, ip_allow_list. Omitted means HTTP.
export type RouteProtocolName = "http" | "tcp" | "udp";

export type RouteVisibility = "public" | "lan" | "tailscale";

// Public metadata of a certificate stored for a resource. The certificate chain and private key are never returned.
export interface RouteCertificate {
  id: string;
  name: string;
  // "uploaded", "cloudflare_origin" or "acme_dns". Missing from older panels, which only stored uploads; other values may come later.
  source?: string;
  fingerprint: string;
  common_name?: string;
  dns_names: string[];
  ip_addresses?: string[];
  not_before: string;
  expires_at: string;
  created_at: string;
}

// A team share of a connected Cloudflare zone. domains are the hostname patterns shared with the team; never credentials.
export interface CertificateZone {
  zone_id: string;
  domain: string;
  domains: string[];
}

// A managed certificate request for a resource: Cloudflare Origin CA or ACME DNS-01 ("acme_dns"). certificate_id is
// the latest issued version, which routes use only once they select it (or a renewal's route update succeeds); empty
// strings mean none. The ACME fields are set only for acme_dns: the operator's fixed directory and the exact terms
// identity accepted, with terms_text when those terms are bounded plain text. validity_days is 0 for acme_dns.
export interface CertificateIssuer {
  id: string;
  name: string;
  provider: string;
  zone_id: string;
  domains: string[];
  validity_days: number;
  auto_renew: boolean;
  phase: string;
  certificate_id: string;
  operation_id: string;
  last_error: string;
  next_check_at: string | null;
  directory?: string;
  terms_url?: string;
  terms_text?: string;
  terms_accepted_at?: string;
  created_at: string;
}

interface CertificateIssuerRequest {
  name: string;
  zone_id: string;
  domains: string[];
  auto_renew: boolean;
}

// terms_url is the exact identity of the terms the editor accepted, as the authority metadata returned it.
export type CertificateIssuerInput =
  | (CertificateIssuerRequest & { provider: "cloudflare_origin"; validity_days: number })
  | (CertificateIssuerRequest & { provider: "acme_dns"; validity_days: 0; accept_terms: true; terms_url: string });

// The panel operator's ACME certificate authority. terms_url may be empty, an HTTPS link or a data:text/plain URI;
// terms_text carries the decoded text of a plain-text URI and is displayed only as text.
export interface CertificateAuthority {
  directory: string;
  terms_url: string;
  terms_text: string;
}

// A DNS-01 TXT record Cove has published or still has to remove. Never includes the TXT value or ownership proof.
// issuer_id and workload_id are null once the issuer or resource was removed.
export interface CertificateChallenge {
  id: string;
  issuer_id: string | null;
  workload_id: string | null;
  zone_id: string;
  hostname: string;
  phase: string;
  cleanup_requested: boolean;
  last_error: string;
  next_check_at: string | null;
  created_at: string;
}

// The oldest unresolved challenges, at most 128; more is true when the list was cut off.
export interface CertificateChallengeList {
  items: CertificateChallenge[];
  more: boolean;
}

export interface Route {
  protocol?: RouteProtocolName;
  // Empty for TCP and UDP routes.
  domain: string;
  service: string;
  port: number;
  // An htpasswd bcrypt entry set through the API. The dashboard has no editor for it and passes it through unchanged.
  auth?: string;
  policy?: RoutePolicy;
}

export type OperationAction = "deploy" | "stop" | "start" | "restart" | "rollback" | "backup" | "restore" | "drill" | "restore_copy" | "preview_remove" | "routes" | "image_check" | "image_scan" | "build" | "rollout_promote" | "rollout_abort" | "restart_unhealthy" | "suspend" | "resume" | "idle_policy" | "registry_maintenance";
export type OperationState = "queued" | "running" | "succeeded" | "failed" | "interrupted" | "cancelled";

// The engine stays a plain string so a value this panel does not know is shown as recorded, not as Compose.
export interface OperationBuildRecipe {
  engine: string;
  service?: string;
  version?: string;
  // Recorded by Cloud Native Buildpacks builds, alongside the lifecycle version in version.
  builder_version?: string;
  platform_api?: string;
  pinned_images?: string[];
  static_runtime?: string;
  // Cloud Native Buildpacks phase memory limit set at admission, and the available memory it was derived from. Absent until admission.
  phase_memory_bytes?: number;
  admission_available_memory_bytes?: number;
  // Recorded by Nixpacks builds: the checksum of the planner release archive (not an image or the extracted binary). Older or queued builds may omit it.
  planner_sha256?: string;
}

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
  image_scan?: ImageScan;
  // How an image_scan started. Older workers omit it.
  scan_origin?: string;
  build?: BuildResult;
  // The build engine this build used, recorded even when it fails. Builds from older control planes omit it.
  build_recipe?: OperationBuildRecipe;
  rollout?: RolloutStatus;
  // Reported only by workers with the job_runs capability.
  job?: JobResult;
  // Reported by environment pause and resume operations, and by a Start that wakes a sleeping app (reason idle).
  suspension?: SuspensionResult;
  // Present on a deploy the panel's CPU scaler queued: a rolling replacement of the current release with new replica counts.
  scale?: ScaleResult;
  // Reported by registry_maintenance operations of a managed registry.
  managed_registry?: ManagedRegistryResult;
}

// Counts of manifests, not bytes: the worker does not measure reclaimed space.
export interface ManagedRegistryResult {
  storage?: { hard_cap_verified: boolean; reason: string };
  manifests: number;
  deleted: number;
  removed: boolean;
  data_retained: boolean;
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

export type ScanOrigin = "manual" | "post_deploy" | "scheduled";

export type ScanSeverity = "CRITICAL" | "HIGH" | "MEDIUM" | "LOW" | "UNKNOWN";

export interface ScanFinding {
  id: string;
  package: string;
  installed: string;
  // Missing or empty means the scanner reported no fixed version, not that the package is safe.
  fixed?: string;
  severity: ScanSeverity;
}

// Coverage values are "scanned", "not_detected" or "unknown"; Java archives are "not_detected" or "not_scanned".
// Any other value is shown as unknown.
export interface ScanCoverage {
  os: string;
  other_languages: string;
  java_archives: string;
  java_message?: string;
}

export interface ScannedImage {
  service: string;
  image_id: string;
  config_digest: string;
  // "complete" or "unknown"; any other value is unknown.
  state: string;
  message?: string;
  // Counts by severity, including findings beyond the stored list.
  counts: Record<string, number>;
  findings: ScanFinding[];
  truncated: boolean;
  end_of_life: boolean;
  coverage: ScanCoverage;
}

export interface ImageScan {
  scanner_image: string;
  scanner_version: string;
  database: { digest: string; updated_at: string; next_update: string };
  checked_at: string;
  state: string;
  message?: string;
  images: ScannedImage[];
}

// The most recent successful image check. Stale means a deploy or rollback succeeded after it.
export type WorkloadImages =
  | { images: ImageStatus[]; stale: boolean; checked_at: null; operation_id?: undefined }
  | { images: ImageStatus[]; stale: boolean; checked_at: string; operation_id: string };

export interface OperationRequest {
  // Only the panel's health monitor queues restart_unhealthy, only environment pause and resume queue suspend and resume, and idle policy changes have their own endpoint; the API rejects them here.
  action: Exclude<OperationAction, "routes" | "restart_unhealthy" | "suspend" | "resume" | "idle_policy" | "registry_maintenance">;
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
  // The worker that ran the operation: recorded for builds, the runtime node otherwise. Older control planes omit it.
  execution_node_id?: string;
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
  // Null when the worker holds no containers stopped for the resource. Present from the stop (environment pause, idle sleep or manual stop) until it is resumed; `reason` says which.
  suspension?: ResourceSuspension | null;
  // Null or missing when the worker has no idle policy for the resource.
  idle?: IdleRuntime | null;
}

export interface FileSecret { name: string; variable: string; services: string[]; build_services: string[] }

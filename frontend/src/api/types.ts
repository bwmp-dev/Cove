export type NodeState = "pending" | "enrolled" | "unreachable" | "ssh_verified" | "ready";

// A filesystem mounted in the worker's namespace. Pseudo filesystems report known=false without an error;
// an error means the mount could not be read. Neither has a capacity reading.
export interface MountMetric { path: string; filesystem: string; total: number; available: number; known: boolean; error?: string }
export interface NodeMetrics { sampled_at: string; cpu_percent: number; memory_total: number; memory_available: number; disk_total: number; disk_available: number; network_rx?: number; network_tx?: number; cpus?: number; mounts?: MountMetric[] | null; mounts_limited?: boolean }
export interface Capabilities {
  // Protocol support for per-service GPU selections; it does not mean the host has a GPU.
  gpu_assignments?: boolean;
  firewall?: boolean;
  host_maintenance?: boolean;
  metrics?: NodeMetrics;
  shared_host?: { coolify_detected: boolean; cleanup_verified: boolean };
  resource_limits?: boolean;
  terminal?: boolean;
  infrastructure?: boolean;
  backup_retention?: boolean;
  extended_logical_backups?: boolean;
  backup_transfers?: boolean;
  backblaze_b2?: boolean;
  file_browser?: boolean;
  file_transfers?: boolean;
  alert_observations?: boolean;
  scheduled_cleanup?: boolean;
  journal_retention?: boolean;
  mount_metrics?: boolean;
  structured_logs?: boolean;
  capacity_advice?: boolean;
  filesystem_snapshots?: boolean;
  traffic_logs?: boolean;
  ssh_identity?: boolean;
  resource_defaults?: boolean;
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
// Null or omitted gpus inherit Compose GPU requests, [] clears them, and IDs replace them. Resource defaults never carry gpus.
export interface WorkloadServiceLimits extends ServiceLimits { gpus?: string[] | null }

export interface DeploymentHook { service: string; command: string }
export interface DeploymentAssertions {
  volumes?: { name: string; labels?: Record<string, string> }[];
  networks?: { name: string; internal: boolean; labels?: Record<string, string> }[];
  services?: { service: string; image: string; networks: string[]; mounts: { volume: string; target: string; read_only: boolean }[] }[];
}
export interface DeploymentHooks { before?: DeploymentHook; after?: DeploymentHook; before_steps?: DeploymentHook[]; assertions?: DeploymentAssertions }

export interface Workload {
  preview_source_id?: string;
  guarded_deploy?: boolean;
  file_secrets?: FileSecret[];
  hooks?: DeploymentHooks;
  limits?: Record<string, WorkloadServiceLimits>;
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
  guarded_deploy?: boolean;
  file_secrets?: FileSecret[];
  hooks?: DeploymentHooks;
  limits?: Record<string, WorkloadServiceLimits>;
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

export interface Route { domain: string; service: string; port: number }

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

export type OperationAction = "deploy" | "stop" | "start" | "restart" | "rollback" | "backup" | "restore" | "drill" | "restore_copy" | "preview_remove" | "routes" | "pitr_configure" | "pitr_backup" | "pitr_forget" | "pitr_restore";
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
}

export interface WorkloadRuntime {
  containers: Container[];
}

export interface FileSecret { name: string; variable: string; services: string[]; build_services: string[] }

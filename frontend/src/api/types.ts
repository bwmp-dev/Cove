export type NodeState = "pending" | "enrolled" | "unreachable" | "ssh_verified" | "ready";

export interface NodeMetrics { sampled_at: string; cpu_percent: number; memory_total: number; memory_available: number; disk_total: number; disk_available: number }
export interface Capabilities {
  metrics?: NodeMetrics;
  shared_host?: { coolify_detected: boolean; cleanup_verified: boolean };
  resource_limits?: boolean;
  terminal?: boolean;
  infrastructure?: boolean;
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
// The fixed custom-role catalogue; GET /api/session reports which of these apply team-wide in the active team.
export type TeamPermission = "view" | "read_secrets" | "operate" | "edit_configuration" | "manage_backups" | "manage_members";
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
}

export interface GitSourceInput extends GitSource {
  token?: string;
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

export interface Workload {
  preview_source_id?: string;
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

export interface Route { domain: string; service: string; port: number }

export type OperationAction = "deploy" | "stop" | "start" | "restart" | "rollback" | "backup" | "restore" | "drill" | "restore_copy" | "preview_remove" | "routes";
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
  // True when Docker publishes at least one port on every interface (0.0.0.0 or ::).
  exposed_ports?: boolean;
}

export interface WorkloadRuntime {
  containers: Container[];
}

export interface FileSecret { name: string; variable: string; services: string[]; build_services: string[] }

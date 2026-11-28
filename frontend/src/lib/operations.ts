import type { Operation, OperationAction, OperationState, WorkerOperation } from "../api/types";
import type { Tone } from "./nodes";

// Lifecycle tones shared by operations, resources, nodes and connections: ok is healthy or done, info is
// in progress, warn needs attention, bad failed, and neutral is idle, stopped or not started.
export const operationTones: Record<OperationState, Tone> = {
  queued: "neutral",
  running: "info",
  succeeded: "ok",
  failed: "bad",
  cancelled: "neutral",
  interrupted: "warn",
};

export function isActive(state: OperationState) {
  return state === "queued" || state === "running";
}

// Image checks, vulnerability scans and builds never change what is deployed, environment pause and resume only stop and start the
// deployed containers, idle policy changes only configure sleep, and registry maintenance only prunes or removes a managed registry,
// so deployment summaries and release lists skip them. Operation lists, details and active progress still show them.
export function isHousekeeping(operation: Operation) {
  return operation.action === "image_check" || operation.action === "image_scan" || operation.action === "build" || operation.action === "idle_policy" || operation.action === "registry_maintenance" || isSuspensionAction(operation.action);
}

// Queued only by the control plane when an environment is paused or resumed; people cannot queue or cancel them.
export function isSuspensionAction(action: OperationAction) {
  return action === "suspend" || action === "resume";
}

// The newest operation that changed or acted on the resource, ignoring housekeeping.
export function latestDeploymentOperation(operations: Operation[], workloadId: string) {
  return operations.find((operation) => operation.workload_id === workloadId && !isHousekeeping(operation));
}

// The list endpoint returns operations newest first.
export function latestByWorkload(operations: Operation[]) {
  const latest = new Map<string, Operation>();
  for (const operation of operations) {
    if (isHousekeeping(operation)) continue;
    const current = latest.get(operation.workload_id);
    if (!current || Date.parse(operation.created_at) > Date.parse(current.created_at)) {
      latest.set(operation.workload_id, operation);
    }
  }
  return latest;
}

export function activeOperation(operations: Operation[], workloadId: string) {
  return operations.find((operation) => operation.workload_id === workloadId && isActive(operation.state));
}

// Only a worker report carries timing and revision fields; control-plane results hold just an error or hold reason.
export function workerResult(operation: Operation): WorkerOperation | null {
  const result = operation.result;
  return result && "id" in result ? result : null;
}

export function holdReason(operation: Operation): string | undefined {
  const result = operation.result;
  return result && "hold_reason" in result ? result.hold_reason : undefined;
}

export function durationMs(operation: Operation): number | null {
  const worker = workerResult(operation);
  const finished = worker?.finished_at;
  if (!worker || !finished) {
    return null;
  }
  const value = Date.parse(finished) - Date.parse(worker.created_at);
  return Number.isFinite(value) && value >= 0 ? value : null;
}

export const actionLabels = {
	backup: "Backup",
	restore: "Restore",
 restore_copy: "Restore copy",
 preview_remove: "Close preview",
  drill: "Restore drill",
  rollback: "Rollback",
  deploy: "Deploy",
  start: "Start",
  stop: "Stop",
  restart: "Restart",
  routes: "Update routes",
  // Records ownership of running containers; no deployment runs.
  adopt: "Adopt",
  image_check: "Check images",
  image_scan: "Scan for vulnerabilities",
  build: "Build and publish",
  rollout_promote: "Promote rollout",
  rollout_abort: "Abort rollout",
  restart_unhealthy: "Restart unhealthy containers",
  suspend: "Pause environment resource",
  resume: "Resume environment resource",
  idle_policy: "Change idle sleep policy",
  pitr_configure: "Configure PITR",
  pitr_backup: "PITR base backup",
  pitr_forget: "Delete PITR history",
  pitr_restore: "PITR restore copy",
  move: "Move to another node",
  registry_maintenance: "Registry maintenance",
} as const satisfies Record<OperationAction, string>;

export const stateLabels: Record<OperationState, string> = {
  cancelled: "Cancelled",
  queued: "Queued",
  running: "Running",
  succeeded: "Succeeded",
  failed: "Failed",
  interrupted: "Interrupted",
};

// How a resource deploys. Managed databases deploy as services.
export function workloadKind(kind: string | undefined) {
  return kind === "job" ? "job" : "service";
}

export function resourceTypeLabel(kind: string | undefined) {
  if (kind === "job") return "Job";
  if (kind === "database") return "Database";
  return "Service";
}

// Jobs run to completion on deploy, so start and restart do not apply to them.
export function allowedActions(kind: "service" | "job"): ("deploy" | "start" | "stop" | "restart")[] {
  return kind === "job" ? ["deploy", "stop"] : ["deploy", "restart", "start", "stop"];
}

export const stateFilters = {
  cancelled: "Cancelled",
  all: "All states",
  active: "Queued or running",
  succeeded: "Succeeded",
  failed: "Failed",
  interrupted: "Interrupted",
} as const;

export type StateFilter = keyof typeof stateFilters;

export interface OperationsSearch {
  state?: Exclude<StateFilter, "all">;
  workload?: string;
}

export function parseOperationsSearch(search: Record<string, unknown>): OperationsSearch {
  const result: OperationsSearch = {};
  if (typeof search.state === "string" && search.state in stateFilters && search.state !== "all") {
    result.state = search.state as OperationsSearch["state"];
  }
  if (typeof search.workload === "string" && search.workload !== "") {
    result.workload = search.workload;
  }
  return result;
}

export function matchesSearch(operation: Operation, search: OperationsSearch) {
  if (search.workload && operation.workload_id !== search.workload) return false;
  if (!search.state) return true;
  if (search.state === "active") return isActive(operation.state);
  return operation.state === search.state;
}

// Matches the API: at most 1000 bytes of UTF-8 and no NUL characters. Blank means no reason.
export const reasonLimit = 1000;

export function reasonError(reason: string, required = false): string | undefined {
  const trimmed = reason.trim();
  if (!trimmed) return required ? "Enter a reason." : undefined;
  if (trimmed.includes("\0")) return "Remove the NUL character.";
  const bytes = new TextEncoder().encode(trimmed).length;
  if (bytes > reasonLimit) return `Shorten the reason to ${reasonLimit} bytes (currently ${bytes}).`;
  return undefined;
}

// The operations endpoint rejects a reason on any other action.
export function acceptsReason(action: OperationAction) {
  return action === "deploy" || action === "rollback" || action === "rollout_promote" || action === "rollout_abort" || action === "build";
}

// Sends a reason only when one was entered, so ordinary requests are unchanged.
export function withReason<T extends object>(body: T, reason: string): T & { reason?: string } {
  const trimmed = reason.trim();
  return trimmed ? { ...body, reason: trimmed } : body;
}

// can_override is the API's full eligibility decision at read time. The local checks only hide the action
// between refetches once the cached operation changes; the API rechecks eligibility on submit.
export function overrideEligible(operation: Operation) {
  return Boolean(operation.can_override) && operation.state === "queued" && (operation.action === "deploy" || operation.action === "rollback") && !operation.cancel_requested && !operation.emergency_override_at;
}

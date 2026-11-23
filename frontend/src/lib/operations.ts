import type { Operation, OperationAction, OperationState } from "../api/types";
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

// The list endpoint returns operations newest first.
export function latestByWorkload(operations: Operation[]) {
  const latest = new Map<string, Operation>();
  for (const operation of operations) {
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

export function durationMs(operation: Operation): number | null {
  const finished = operation.result?.finished_at;
  if (!finished) {
    return null;
  }
  const value = Date.parse(finished) - Date.parse(operation.result?.created_at ?? operation.created_at);
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
  pitr_configure: "Configure PITR",
  pitr_backup: "PITR base backup",
  pitr_forget: "Delete PITR history",
  pitr_restore: "PITR restore copy",
} as const satisfies Record<OperationAction, string>;

export const stateLabels: Record<OperationState, string> = {
  cancelled: "Cancelled",
  queued: "Queued",
  running: "Running",
  succeeded: "Succeeded",
  failed: "Failed",
  interrupted: "Interrupted",
};

export function workloadKind(kind: string | undefined) {
  return kind === "job" ? "job" : "service";
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

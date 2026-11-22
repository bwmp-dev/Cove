import type { RolloutOptions, RolloutPhase, RolloutStatus, RolloutStrategy, Workload } from "../api/types";
import type { Tone } from "./nodes";
import { workloadKind } from "./operations";
import { isManagedPreview } from "./previews";

export interface ReplicaRow {
  id: string;
  service: string;
  count: string;
}

export interface CheckRow {
  id: string;
  service: string;
  port: string;
  path: string;
}

export interface RolloutDraft {
  strategy: RolloutStrategy;
  replicas: ReplicaRow[];
  checks: CheckRow[];
  parallelVolumes: boolean;
  drain: string;
  timeout: string;
  canary: string;
}

// Jobs and managed previews cannot use rollouts; their saved options are left untouched.
export function rolloutsApply(workload: Pick<Workload, "kind" | "preview_source_id" | "environment_preview_id">) {
  return workloadKind(workload.kind) === "service" && !isManagedPreview(workload);
}

export const rolloutLimits = { serviceRows: 32, replicasPerService: 16, totalReplicas: 64 } as const;

export const strategyLabels: Record<RolloutStatus["strategy"] | "disabled", string> = {
  disabled: "Off",
  rolling: "Rolling",
  blue_green: "Blue/green",
  canary: "Canary",
  guarded: "Guarded replacement",
};

// Matches the control plane's normalization: an empty strategy is rolling, zero timeout and canary share take defaults.
function normalized(options: RolloutOptions | null | undefined): RolloutOptions {
  if (!options) return { strategy: "disabled", parallel_volumes: false, drain_seconds: 0, timeout_seconds: 120, canary_percent: 10 };
  return {
    ...options,
    strategy: options.strategy || "rolling",
    timeout_seconds: options.timeout_seconds || 120,
    canary_percent: options.canary_percent || 10,
  };
}

export function rolloutDraft(options: RolloutOptions | null | undefined): RolloutDraft {
  const value = normalized(options);
  return {
    strategy: value.strategy,
    replicas: Object.entries(value.replicas ?? {}).map(([service, count]) => ({ id: crypto.randomUUID(), service, count: String(count) })),
    checks: Object.entries(value.http_checks ?? {}).map(([service, check]) => ({ id: crypto.randomUUID(), service, port: String(check.port), path: check.path })),
    parallelVolumes: value.parallel_volumes,
    drain: String(value.drain_seconds),
    timeout: String(value.timeout_seconds),
    canary: String(value.canary_percent),
  };
}

export function rolloutEnabled(options: RolloutOptions | null | undefined) {
  return normalized(options).strategy !== "disabled";
}

const composeService = /^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,63}$/;

function integer(text: string, min: number, max: number): number | null {
  if (!/^\d+$/.test(text.trim())) return null;
  const value = Number(text);
  return value >= min && value <= max ? value : null;
}

function hasControlCharacter(value: string) {
  return [...value].some((c) => {
    const code = c.charCodeAt(0);
    return code < 32 || code === 127;
  });
}

function serviceError(name: string, seen: Set<string>) {
  if (name === "") return "Enter the Compose service name.";
  if (!composeService.test(name)) return "Use a Compose service name: letters, digits, dots, dashes and underscores, up to 64 characters.";
  if (seen.has(name)) return "Each service can be listed once.";
  return null;
}

export type RolloutErrors = Record<string, string>;

// Mirrors the worker's validation so mistakes are reported beside each field. Row errors are keyed
// "<row id>.service", "<row id>.count", "<row id>.port" and "<row id>.path".
// A disabled strategy keeps the saved tuning so turning rollouts back on restores it.
export function validateRollout(draft: RolloutDraft, saved: RolloutOptions | null | undefined): { options?: RolloutOptions; errors: RolloutErrors } {
  if (draft.strategy === "disabled") {
    return { options: { ...normalized(saved), strategy: "disabled" }, errors: {} };
  }
  const errors: RolloutErrors = {};
  const drain = integer(draft.drain, 0, 120);
  const timeout = integer(draft.timeout, 5, 600);
  // The canary share is hidden for other strategies, so an edit there cannot block saving.
  const canary = integer(draft.canary, 1, 99) ?? (draft.strategy === "canary" ? null : normalized(saved).canary_percent);
  if (drain === null) errors.drain = "Use a whole number of seconds from 0 to 120.";
  if (timeout === null) errors.timeout = "Use a whole number of seconds from 5 to 600.";
  if (canary === null) errors.canary = "Use a whole percentage from 1 to 99.";

  const replicas: Record<string, number> = {};
  const replicaServices = new Set<string>();
  let total = 0;
  if (draft.replicas.length > rolloutLimits.serviceRows) errors.replicas = `Use at most ${rolloutLimits.serviceRows} services.`;
  for (const row of draft.replicas) {
    const service = row.service.trim();
    const nameError = serviceError(service, replicaServices);
    if (nameError) errors[`${row.id}.service`] = nameError;
    replicaServices.add(service);
    const count = integer(row.count, 1, rolloutLimits.replicasPerService);
    if (count === null) errors[`${row.id}.count`] = `Use a whole number from 1 to ${rolloutLimits.replicasPerService}.`;
    else {
      total += count;
      replicas[service] = count;
    }
  }
  if (!errors.replicas && total > rolloutLimits.totalReplicas) errors.replicas = `Use at most ${rolloutLimits.totalReplicas} replicas in total.`;

  const checks: NonNullable<RolloutOptions["http_checks"]> = {};
  const checkServices = new Set<string>();
  if (draft.checks.length > rolloutLimits.serviceRows) errors.checks = `Use at most ${rolloutLimits.serviceRows} checks.`;
  for (const row of draft.checks) {
    const service = row.service.trim();
    const nameError = serviceError(service, checkServices);
    if (nameError) errors[`${row.id}.service`] = nameError;
    checkServices.add(service);
    const port = integer(row.port, 1, 65535);
    if (port === null) errors[`${row.id}.port`] = "Use a container port from 1 to 65535.";
    const path = row.path;
    if (!path.startsWith("/") || path.startsWith("//") || path.length > 1024 || hasControlCharacter(path)) {
      errors[`${row.id}.path`] = "Start with a single / and use at most 1024 characters without control characters.";
    }
    if (port !== null) checks[service] = { port, path };
  }

  if (Object.keys(errors).length > 0 || drain === null || timeout === null || canary === null) return { errors };
  const options: RolloutOptions = { strategy: draft.strategy, parallel_volumes: draft.parallelVolumes, drain_seconds: drain, timeout_seconds: timeout, canary_percent: canary };
  if (draft.replicas.length > 0) options.replicas = replicas;
  if (draft.checks.length > 0) options.http_checks = checks;
  return { options, errors };
}

// Compares what would be saved, ignoring row order (the API returns maps sorted by service) and row ids.
function draftKey(draft: RolloutDraft) {
  if (draft.strategy === "disabled") return "disabled";
  const byService = <T extends { service: string }>(rows: T[]) => [...rows].sort((a, b) => (a.service < b.service ? -1 : a.service > b.service ? 1 : 0));
  return JSON.stringify([
    draft.strategy,
    byService(draft.replicas.map((row) => ({ service: row.service.trim(), count: row.count.trim() }))),
    byService(draft.checks.map((row) => ({ service: row.service.trim(), port: row.port.trim(), path: row.path }))),
    draft.parallelVolumes,
    draft.drain.trim(),
    draft.timeout.trim(),
    draft.strategy === "canary" ? draft.canary.trim() : "",
  ]);
}

export function rolloutChanged(draft: RolloutDraft, saved: RolloutOptions | null | undefined) {
  return draftKey(draft) !== draftKey(rolloutDraft(saved));
}

export const rolloutPhases: Record<RolloutPhase, { tone: Tone; label: string }> = {
  preparing: { tone: "info", label: "Preparing candidate" },
  replacing: { tone: "info", label: "Replacing containers" },
  waiting: { tone: "warn", label: "Waiting for promotion" },
  promoting: { tone: "info", label: "Promoting" },
  aborting: { tone: "info", label: "Aborting" },
  draining: { tone: "info", label: "Draining old release" },
  completed: { tone: "ok", label: "Completed" },
  aborted: { tone: "neutral", label: "Aborted" },
};

// An unrecognised phase is never presented as finished.
export function rolloutPhase(phase: string) {
  return rolloutPhases[phase as RolloutPhase] ?? { tone: "warn" as const, label: `Unknown phase: ${phase}` };
}

export function rolloutStrategyLabel(strategy: string) {
  return strategyLabels[strategy as keyof typeof strategyLabels] ?? strategy;
}

export function trafficSummary(status: RolloutStatus) {
  const percent = Math.min(100, Math.max(0, Math.round(status.candidate_percent)));
  if (status.phase === "waiting" && percent === 0) return "The candidate passed its health checks and receives no traffic yet.";
  if (status.phase === "waiting") return `The candidate receives ${percent}% of traffic; the current release serves the rest.`;
  return `The candidate receives ${percent}% of traffic.`;
}

// An operation result that is still waiting describes the staged state when the operation finished; runtime is
// authoritative for whether it is still pending.
export function resultSummary(status: RolloutStatus) {
  if (status.strategy === "guarded") return "Fell back to guarded Compose replacement.";
  const label = rolloutStrategyLabel(status.strategy);
  if (status.phase === "waiting") return `${label} rollout staged. Traffic moves fully only after it is promoted.`;
  if (status.phase === "completed") return `${label} replacement completed.`;
  if (status.phase === "aborted") return `${label} rollout aborted. The previous release kept serving traffic.`;
  return `${label} replacement: ${rolloutPhase(status.phase).label.toLowerCase()}.`;
}

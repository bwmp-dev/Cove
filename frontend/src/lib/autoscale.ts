import type { AutoscalePolicy, AutoscaleService, AutoscaleState, CPUSample, ScaleResult, ServiceLimits } from "../api/types";
import type { Tone } from "./nodes";
import { rolloutLimits } from "./rollouts";

// Enforced by the API; shown here so the form and copy match the server.
export const autoscaleLimits = {
  minReplicas: 1,
  maxReplicas: rolloutLimits.replicasPerService,
  minTarget: 10,
  maxTarget: 90,
  services: rolloutLimits.serviceRows,
  totalReplicas: rolloutLimits.totalReplicas,
  // A sample older than this cannot start scaling.
  freshSeconds: 90,
  samplesNeeded: 3,
  cooldownSeconds: 300,
} as const;

export const autoscaleDefaults: AutoscaleService = { min_replicas: 1, max_replicas: 4, target_cpu_percent: 70 };

export type AutoscaleStatus = "disabled" | "waiting" | "unavailable" | "waiting_for_samples" | "stale_sample" | "replicas_unknown" | "partial_sample" | "invalid_policy" | "within_target" | "cooldown" | "queued";

export const autoscaleStatuses: Record<AutoscaleStatus, { tone: Tone; label: string; detail: string }> = {
  disabled: { tone: "neutral", label: "Off", detail: "CPU scaling is off, so replica counts change only when you edit them." },
  waiting: { tone: "neutral", label: "Waiting", detail: "Not eligible right now: the resource is stopped, paused or asleep, the worker is not ready or lacks CPU scaling support, or an operation is active." },
  unavailable: { tone: "warn", label: "Sample unavailable", detail: "The last check could not read a complete, healthy CPU sample, so nothing was queued." },
  waiting_for_samples: { tone: "info", label: "Collecting samples", detail: "Scaling needs 3 consistent samples no more than 90 seconds apart. Samples are read at most once a minute, so this takes at least a few minutes." },
  stale_sample: { tone: "warn", label: "Samples stale", detail: "The samples are too old or come from different deployments, so nothing is queued until fresh ones arrive." },
  replicas_unknown: { tone: "warn", label: "Replica count unknown", detail: "A selected service had no deployed replicas in the latest sample, so nothing was queued." },
  partial_sample: { tone: "warn", label: "Incomplete samples", detail: "A selected service was missing from a sample or its replica count changed between samples, so nothing was queued." },
  invalid_policy: { tone: "bad", label: "Invalid policy", detail: "The saved policy no longer passes validation. Review the bounds and save again." },
  within_target: { tone: "ok", label: "No replica change", detail: "The latest 3 samples did not call for a change: no service was at or above its target in all 3, or at or below half of it in all 3, or it was already at its bound." },
  cooldown: { tone: "info", label: "Cooling down", detail: "Scaling waits 5 minutes after each queued attempt, whether it succeeded, failed or was cancelled." },
  queued: { tone: "info", label: "Scaling queued", detail: "A deploy with new replica counts was queued." },
};

export function autoscaleStatus(status: string) {
  if (Object.hasOwn(autoscaleStatuses, status)) return autoscaleStatuses[status as AutoscaleStatus];
  return { tone: "warn" as const, label: status ? `Unknown status: ${status}` : "Unknown status", detail: "The panel reported a status this page does not recognise, so it is not treated as healthy." };
}

export function latestSample(state: Pick<AutoscaleState, "samples">): CPUSample | undefined {
  const samples = state.samples ?? [];
  return samples.at(-1);
}

// Sample timestamps are authoritative. A missing, unparsable or future one (a worker clock ahead of the panel) counts as stale, as the panel rejects it.
export function sampleIsStale(sample: CPUSample, now: number) {
  const taken = Date.parse(sample.sampled_at);
  return Number.isNaN(taken) || taken > now || now - taken > autoscaleLimits.freshSeconds * 1000;
}

export function cooldownEnds(lastQueuedAt: string | null) {
  if (!lastQueuedAt) return null;
  const queued = Date.parse(lastQueuedAt);
  return Number.isNaN(queued) ? null : new Date(queued + autoscaleLimits.cooldownSeconds * 1000).toISOString();
}

export function formatCPU(percent: number) {
  return `${percent.toLocaleString("en-US", { maximumFractionDigits: 1 })}%`;
}

export function hasLimits(limit: ServiceLimits | undefined) {
  return Boolean(limit && limit.cpus > 0 && limit.memory_mb > 0);
}

export interface ServiceDraft {
  selected: boolean;
  min: string;
  max: string;
  target: string;
}

export interface AutoscaleDraft {
  enabled: boolean;
  services: Record<string, ServiceDraft>;
}

// Services with explicit limits can be selected; saved services stay listed even if their limits were removed since.
export function candidateServices(limits: Record<string, ServiceLimits> | undefined, policy: AutoscalePolicy) {
  return [...new Set([...Object.keys(limits ?? {}), ...Object.keys(policy.services ?? {})])].sort();
}

function serviceDraft(service: AutoscaleService | undefined): ServiceDraft {
  const value = service ?? autoscaleDefaults;
  return { selected: Boolean(service), min: String(value.min_replicas), max: String(value.max_replicas), target: String(value.target_cpu_percent) };
}

export function autoscaleDraft(policy: AutoscalePolicy, services: string[]): AutoscaleDraft {
  return { enabled: policy.enabled, services: Object.fromEntries(services.map((name) => [name, serviceDraft(policy.services?.[name])])) };
}

// Unselected services do not change what is saved, so their typed bounds do not make the form dirty.
function draftKey(draft: AutoscaleDraft) {
  const selected = Object.entries(draft.services)
    .filter(([, service]) => service.selected)
    .sort(([a], [b]) => (a < b ? -1 : a > b ? 1 : 0))
    .map(([name, service]) => [name, service.min.trim(), service.max.trim(), service.target.trim()]);
  return JSON.stringify([draft.enabled, selected]);
}

export function sameAutoscaleDraft(a: AutoscaleDraft, b: AutoscaleDraft) {
  return draftKey(a) === draftKey(b);
}

// Keys: "form", "<service>.min", "<service>.max", "<service>.target" and "<service>.limits".
export type AutoscaleErrors = Record<string, string>;

function integer(text: string, min: number, max: number) {
  if (!/^\d+$/.test(text.trim())) return null;
  const value = Number(text);
  return value >= min && value <= max ? value : null;
}

// Mirrors the API: bounds per service, and the replica budget counts the larger of each service's manual count and its maximum.
export function validateAutoscale(draft: AutoscaleDraft, { limits, manualReplicas }: { limits?: Record<string, ServiceLimits>; manualReplicas?: Record<string, number> }): { errors: AutoscaleErrors; policy?: AutoscalePolicy } {
  const errors: AutoscaleErrors = {};
  const services: Record<string, AutoscaleService> = {};
  const selected = Object.entries(draft.services).filter(([, service]) => service.selected);
  if (draft.enabled && selected.length === 0) errors.form = "Select at least one service to scale.";
  if (selected.length > autoscaleLimits.services) errors.form = `Select at most ${autoscaleLimits.services} services.`;
  for (const [name, service] of selected) {
    const min = integer(service.min, autoscaleLimits.minReplicas, autoscaleLimits.maxReplicas);
    const max = integer(service.max, autoscaleLimits.minReplicas, autoscaleLimits.maxReplicas);
    const target = integer(service.target, autoscaleLimits.minTarget, autoscaleLimits.maxTarget);
    const range = `Use a whole number from ${autoscaleLimits.minReplicas} to ${autoscaleLimits.maxReplicas}.`;
    if (min === null) errors[`${name}.min`] = range;
    if (max === null) errors[`${name}.max`] = range;
    else if (min !== null && min > max) errors[`${name}.max`] = "Use a maximum at least as large as the minimum.";
    if (target === null) errors[`${name}.target`] = `Use a whole percentage from ${autoscaleLimits.minTarget} to ${autoscaleLimits.maxTarget}.`;
    if (draft.enabled && !hasLimits(limits?.[name])) errors[`${name}.limits`] = "Set explicit CPU and memory limits for this service before scaling it.";
    if (min !== null && max !== null && target !== null) services[name] = { min_replicas: min, max_replicas: max, target_cpu_percent: target };
  }
  if (!errors.form) {
    const budget = { ...manualReplicas };
    for (const [name, service] of Object.entries(services)) budget[name] = Math.max(budget[name] ?? 0, service.max_replicas);
    const total = Object.values(budget).reduce((sum, count) => sum + count, 0);
    if (total > autoscaleLimits.totalReplicas) errors.form = `Maximum replicas, together with fixed replica counts, add up to ${total}. Use at most ${autoscaleLimits.totalReplicas} in total.`;
  }
  if (Object.keys(errors).length > 0) return { errors };
  return { errors, policy: { enabled: draft.enabled, services } };
}

// Replica changes in a CPU scaling deploy, in service order. Services whose count did not change are left out.
export function scaleChanges(scale: ScaleResult) {
  return Object.keys(scale.replicas)
    .sort()
    .map((service) => ({ service, before: scale.before?.[service], after: scale.replicas[service], cpu: scale.cpu_percent?.[service] }));
}

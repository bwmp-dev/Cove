import type { ManagedRegistry, ManagedRegistryRetention } from "../api/managed-registries";
import type { Node } from "../api/types";
import type { Tone } from "./nodes";
import { isHostname } from "./workload-form";

export const retentionLimits = { keep: { min: 1, max: 100 }, age: { min: 1, max: 3650 } } as const;
export const maxRegistryNameBytes = 120;

const states: Record<string, { tone: Tone; label: string }> = {
  provisioning: { tone: "info", label: "Provisioning" },
  ready: { tone: "ok", label: "Ready" },
  maintenance: { tone: "info", label: "Maintenance" },
  removing: { tone: "info", label: "Removing" },
  failed: { tone: "bad", label: "Failed" },
  removed: { tone: "neutral", label: "Removed" },
};

// States from a newer panel are shown as they are and never unlock an action.
export function registryState(state: string) {
  return states[state] ?? { tone: "neutral" as Tone, label: state ? `Unknown state (${state})` : "Unknown state" };
}

export function operationActive(registry: ManagedRegistry) {
  return registry.operation_state === "queued" || registry.operation_state === "running";
}

export function registryWorking(registry: ManagedRegistry) {
  return registry.state === "provisioning" || registry.state === "maintenance" || registry.state === "removing" || operationActive(registry);
}

// Only an empty removed registry: the API refuses while data is retained or an operation is active.
export function recordDeletable(registry: ManagedRegistry) {
  return registry.state === "removed" && !registry.data_retained && !operationActive(registry);
}

// Every state except removed holds the team's one live registry, including states this panel does not know.
export function isLive(registry: ManagedRegistry) {
  return registry.state !== "removed";
}

export function registryPollInterval(registries: ManagedRegistry[] | undefined) {
  return registries?.some(registryWorking) ? 3_000 : 30_000;
}

// The API refuses retention and certificate changes during an operation, maintenance or removal.
export function editable(registry: ManagedRegistry) {
  return (registry.state === "ready" || registry.state === "failed") && !operationActive(registry);
}

export interface RetentionDraft {
  keep: string;
  age: string;
}

export type RetentionErrors = Partial<Record<keyof RetentionDraft, string>>;

export function retentionDraft(retention: ManagedRegistryRetention): RetentionDraft {
  return { keep: String(retention.keep_per_repository), age: String(retention.minimum_age_days) };
}

function wholeNumber(value: string) {
  return /^\d+$/.test(value.trim()) ? Number(value.trim()) : NaN;
}

function rangeError(value: string, limits: { min: number; max: number }) {
  const number = wholeNumber(value);
  return Number.isInteger(number) && number >= limits.min && number <= limits.max ? undefined : `Enter a whole number from ${limits.min} to ${limits.max}.`;
}

export function validateRetention(draft: RetentionDraft): RetentionErrors {
  const errors: RetentionErrors = {};
  const keep = rangeError(draft.keep, retentionLimits.keep);
  const age = rangeError(draft.age, retentionLimits.age);
  if (keep) errors.keep = keep;
  if (age) errors.age = age;
  return errors;
}

export function retentionInput(draft: RetentionDraft): ManagedRegistryRetention {
  return { keep_per_repository: wholeNumber(draft.keep), minimum_age_days: wholeNumber(draft.age) };
}

export function retentionSummary(retention: ManagedRegistryRetention) {
  if (retention.keep_per_repository < 1 || retention.minimum_age_days < 1) return null;
  const manifests = retention.keep_per_repository === 1 ? "manifest" : "manifests";
  const days = retention.minimum_age_days === 1 ? "day" : "days";
  return `newest ${retention.keep_per_repository} ${manifests} of each repository and every manifest younger than ${retention.minimum_age_days} ${days}`;
}

// The API counts the trimmed name in bytes.
export function nameError(name: string) {
  const trimmed = name.trim();
  if (trimmed === "") return "Enter a name.";
  if (new TextEncoder().encode(trimmed).length > maxRegistryNameBytes) return `Use at most ${maxRegistryNameBytes} characters.`;
  return undefined;
}

export function hostnameError(hostname: string) {
  const value = hostname.trim();
  if (value === "") return "Enter the hostname builds and deploys will use.";
  if (value.includes("*")) return "Wildcard hostnames cannot be used. Enter one exact hostname.";
  if (/[/:]/.test(value)) return "Enter the hostname only, without a scheme, path or port.";
  if (value !== value.toLowerCase()) return "Use lowercase letters.";
  if (!isHostname(value)) return "Enter a complete DNS hostname, such as registry.example.com.";
  return undefined;
}

// Capabilities that are missing or not yet read count as unsupported.
export function nodeProblem(node: Node): string | null {
  if (node.state !== "ready") return "not ready";
  if (!node.capabilities) return "capabilities not reported";
  if (node.capabilities.managed_registry !== true) return "no managed registry support";
  if (node.capabilities.file_secrets !== true) return "no secret file support";
  return null;
}

import type { ImageUpdateConfiguration, ImageUpdateMode, ImageUpdatePolicyStatus } from "../api/image-updates";
import type { Tone } from "./nodes";

// Shape checks only. Service names, mutable references and the full SemVer range syntax are validated by the
// control plane, which returns the error to show.
export const maxImageUpdateRules = 32;
export const maxRangeLength = 200;
const utcTime = /^([01]\d|2[0-3]):[0-5]\d$/;

export const modeLabels: Record<ImageUpdateMode, string> = { notify: "Notify only", auto: "Deploy automatically" };

let counter = 0;

export interface RuleDraft {
  key: string;
  service: string;
  range: string;
}

export interface PolicyDraft {
  enabled: boolean;
  mode: ImageUpdateMode;
  rules: RuleDraft[];
  windowStart: string;
  windowEnd: string;
}

export function ruleDraft(rule?: { service: string; range: string }): RuleDraft {
  return { key: `rule-${++counter}`, service: rule?.service ?? "", range: rule?.range ?? "" };
}

// A policy that was never saved starts off and in notify mode, so automatic deploys are always chosen deliberately.
// A saved policy is copied as is, including services that are no longer in the saved Compose file.
export function policyDraftFrom(configuration: ImageUpdateConfiguration | null): PolicyDraft {
  return {
    enabled: configuration?.enabled ?? false,
    mode: configuration?.mode === "auto" ? "auto" : "notify",
    rules: configuration && configuration.rules.length > 0 ? configuration.rules.map(ruleDraft) : [ruleDraft()],
    windowStart: configuration?.window_start ?? "",
    windowEnd: configuration?.window_end ?? "",
  };
}

// All five fields are always sent.
export function policyInput(draft: PolicyDraft): ImageUpdateConfiguration {
  return {
    enabled: draft.enabled,
    mode: draft.mode,
    rules: draft.rules.map((rule) => ({ service: rule.service.trim(), range: rule.range.trim() })),
    window_start: draft.windowStart.trim(),
    window_end: draft.windowEnd.trim(),
  };
}

export function policyDraftKey(draft: PolicyDraft) {
  return JSON.stringify(policyInput(draft));
}

export type PolicyErrors = Record<string, string>;

// Errors are keyed by `${key}.service`, `${key}.range`, "rules" and "window".
export function validatePolicyDraft(draft: PolicyDraft): PolicyErrors {
  const errors: PolicyErrors = {};
  if (draft.rules.length === 0) errors.rules = "Add at least one service.";
  if (draft.rules.length > maxImageUpdateRules) errors.rules = `Use at most ${maxImageUpdateRules} services.`;
  const seen = new Set<string>();
  for (const rule of draft.rules) {
    const service = rule.service.trim();
    if (!service) errors[`${rule.key}.service`] = "Choose a service.";
    else if (seen.has(service)) errors[`${rule.key}.service`] = "This service is already listed.";
    seen.add(service);
    if (rule.range.trim().length > maxRangeLength) errors[`${rule.key}.range`] = `Use at most ${maxRangeLength} characters.`;
  }
  const start = draft.windowStart.trim();
  const end = draft.windowEnd.trim();
  if (Boolean(start) !== Boolean(end)) errors.window = "Set both times, or clear both for any time of day.";
  else if (start && (!utcTime.test(start) || !utcTime.test(end))) errors.window = "Use 24-hour HH:MM times.";
  else if (start && start === end) errors.window = "Use two different times, or clear both for any time of day.";
  return errors;
}

// Times are zero-padded HH:MM, so string order is time order. A window that ends at 00:00 ends at midnight.
export function windowSummary(start: string, end: string) {
  if (!start || !end) return "Any time of day";
  if (end < start && end !== "00:00") return `${start}–${end} UTC, continuing past midnight`;
  return `${start}–${end} UTC`;
}

export function updateState(state: string | undefined): { tone: Tone; label: string } {
  if (state === "available") return { tone: "warn", label: "Update available" };
  if (state === "current") return { tone: "ok", label: "Current" };
  return { tone: "neutral", label: "Unknown" };
}

// Why the policy cannot be switched on, in the order the control plane checks.
export function enableBlocker(status: ImageUpdatePolicyStatus): string | null {
  if (!status.eligible) return status.unavailable_reason || "Image updates require an image-only service outside managed previews.";
  if (!status.worker_supported) return "The worker does not report image update support. Upgrade and verify the worker first.";
  if (!status.node_ready) return "The node is not ready. Image updates can be turned on once it is ready.";
  return null;
}

// Saved rules can name services that were later removed or switched to a digest or variable reference.
export function ruleProblem(service: string, status: ImageUpdatePolicyStatus): string | null {
  if (!service) return null;
  const found = status.services.find((item) => item.service === service);
  if (!found) return `${service} is not in the saved Compose file. Remove this rule or add the service back.`;
  if (!found.mutable) return `${service} has no plain image tag (its image is missing or uses a digest or variable), so it cannot be updated by tag.`;
  return null;
}

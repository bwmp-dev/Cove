// Deployment policy as stored by GET/PUT /api/{teams,projects,environments}/{id}/deployment-policy.
export interface RecurringWindow { days: number[]; start: string; end: string }
export interface PolicyFreeze { from: string; until: string; reason: string }

export interface DeploymentPolicyValue {
  approval: boolean;
  // The original single freeze; kept so saving never drops it.
  freeze_from: string | null;
  freeze_until: string | null;
  // Daily bounds in the policy time zone.
  window_start: string;
  window_end: string;
  timezone: string;
  recurring_windows: RecurringWindow[];
  freezes: PolicyFreeze[];
  outside_window: "block" | "approval";
}

// Form state: absolute times are `datetime-local` values read as UTC, and rows carry stable keys.
export interface WindowRow extends RecurringWindow { id: number }
export interface FreezeRow extends PolicyFreeze { id: number }
export interface PolicyDraft {
  approval: boolean;
  legacyFrom: string;
  legacyUntil: string;
  window_start: string;
  window_end: string;
  timezone: string;
  outside_window: "block" | "approval";
  windows: WindowRow[];
  freezes: FreezeRow[];
}

export const policyLimit = 32;
const maxFreeze = 366 * 24 * 60 * 60 * 1000;

export const weekdays = [
  { value: 0, short: "Sun", long: "Sunday" },
  { value: 1, short: "Mon", long: "Monday" },
  { value: 2, short: "Tue", long: "Tuesday" },
  { value: 3, short: "Wed", long: "Wednesday" },
  { value: 4, short: "Thu", long: "Thursday" },
  { value: 5, short: "Fri", long: "Friday" },
  { value: 6, short: "Sat", long: "Saturday" },
] as const;

let rowId = 0;
const nextId = () => ++rowId;

export function newWindow(): WindowRow {
  return { id: nextId(), days: [1, 2, 3, 4, 5], start: "09:00", end: "17:00" };
}

export function newFreeze(): FreezeRow {
  return { id: nextId(), from: "", until: "", reason: "" };
}

// An absolute instant shown as a UTC `datetime-local` value (YYYY-MM-DDTHH:MM).
export function toUtcInput(iso: string | null | undefined) {
  if (!iso) return "";
  const time = Date.parse(iso);
  return Number.isFinite(time) ? new Date(time).toISOString().slice(0, 16) : "";
}

// A UTC `datetime-local` value as an explicit ISO 8601 instant, never interpreted in the browser's zone.
export function fromUtcInput(value: string) {
  return value ? `${value}:00Z` : "";
}

export function fromPolicy(policy: DeploymentPolicyValue): PolicyDraft {
  return {
    approval: policy.approval,
    legacyFrom: toUtcInput(policy.freeze_from),
    legacyUntil: toUtcInput(policy.freeze_until),
    window_start: policy.window_start ?? "",
    window_end: policy.window_end ?? "",
    timezone: policy.timezone || "UTC",
    outside_window: policy.outside_window === "approval" ? "approval" : "block",
    windows: (policy.recurring_windows ?? []).map((window) => ({ id: nextId(), days: [...window.days].sort((a, b) => a - b), start: window.start, end: window.end })),
    freezes: (policy.freezes ?? []).map((freeze) => ({ id: nextId(), from: toUtcInput(freeze.from), until: toUtcInput(freeze.until), reason: freeze.reason ?? "" })),
  };
}

export function toPolicy(draft: PolicyDraft): DeploymentPolicyValue {
  return {
    approval: draft.approval,
    freeze_from: draft.legacyFrom ? fromUtcInput(draft.legacyFrom) : null,
    freeze_until: draft.legacyUntil ? fromUtcInput(draft.legacyUntil) : null,
    window_start: draft.window_start,
    window_end: draft.window_end,
    timezone: draft.timezone.trim(),
    outside_window: draft.outside_window,
    recurring_windows: draft.windows.map(({ days, start, end }) => ({ days, start, end })),
    freezes: draft.freezes.map(({ from, until, reason }) => ({ from: fromUtcInput(from), until: fromUtcInput(until), reason: reason.trim() })),
  };
}

export function isValidTimeZone(zone: string) {
  if (!zone || zone === "Local") return false;
  try {
    new Intl.DateTimeFormat("en", { timeZone: zone });
    return true;
  } catch {
    return false;
  }
}

function freezeError(from: string, until: string) {
  if (!from || !until) return "Set both the start and the end.";
  const span = Date.parse(fromUtcInput(until)) - Date.parse(fromUtcInput(from));
  if (!(span > 0)) return "The end must be after the start.";
  if (span > maxFreeze) return "A freeze can last at most 366 days.";
  return undefined;
}

// Mirrors the API's validation so mistakes are shown beside the field before saving; the API stays authoritative.
export function draftErrors(draft: PolicyDraft): Record<string, string> {
  const errors: Record<string, string> = {};
  if (!isValidTimeZone(draft.timezone.trim())) errors.timezone = "Enter a valid IANA time zone, such as UTC or America/New_York.";
  if (Boolean(draft.window_start) !== Boolean(draft.window_end)) errors.daily = "Set both daily times or clear both.";
  else if (draft.window_start && draft.window_start === draft.window_end) errors.daily = "The daily window must start and end at different times.";
  if (draft.legacyFrom || draft.legacyUntil) {
    const error = freezeError(draft.legacyFrom, draft.legacyUntil);
    if (error) errors.legacy = error;
  }
  if (draft.windows.length > policyLimit) errors.windowCount = `At most ${policyLimit} weekly windows.`;
  if (draft.freezes.length > policyLimit) errors.freezeCount = `At most ${policyLimit} freezes.`;
  for (const window of draft.windows) {
    if (!window.days.length) errors[`window-${window.id}`] = "Choose at least one day.";
    else if (!window.start || !window.end) errors[`window-${window.id}`] = "Set both the start and end time.";
    else if (window.start === window.end) errors[`window-${window.id}`] = "The window must start and end at different times.";
  }
  for (const freeze of draft.freezes) {
    const error = freezeError(freeze.from, freeze.until) ?? (new TextEncoder().encode(freeze.reason.trim()).length > 1000 ? "Shorten the reason to 1000 bytes." : undefined);
    if (error) errors[`freeze-${freeze.id}`] = error;
  }
  return errors;
}

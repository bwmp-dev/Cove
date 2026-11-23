import type { PITRBase, PITRSettings, PITRStatus } from "../api/pitr";
import type { Workload } from "../api/types";
import { MiB, bytesToMiBText, parseTransferLimit } from "./backups";
import type { Tone } from "./nodes";

// A recovery copy keeps its managed settings only for PostgreSQL, so other managed engines do not offer PITR.
export function pitrApplies(workload: Pick<Workload, "database">) {
  return !workload.database || workload.database.engine === "postgresql";
}

export const PITR_INTERVAL_MAX = 168;
export const PITR_KEEP_MAX = 32;
export const PITR_DAYS_MAX = 30;
export const PITR_MIN_BYTES = 1024 * MiB;
export const PITR_MAX_BYTES = 64 * 1024 * MiB;
export const PITR_OBJECT_LIMIT = 100_000;
// The worker reports a heartbeat continuously; older readings say nothing about the current state.
export const PITR_STALE_MS = 30_000;

export const defaultPITRSettings: Omit<PITRSettings, "service"> = { enabled: true, interval_hours: 24, keep_last: 7, retention_days: 7, max_bytes: 10 * 1024 * MiB };

export const pitrStates: Record<string, { tone: Tone; label: string }> = {
  not_configured: { tone: "neutral", label: "Not configured" },
  starting: { tone: "info", label: "Starting" },
  configuring: { tone: "info", label: "Configuring PostgreSQL" },
  capturing: { tone: "ok", label: "Capturing" },
  waiting_for_base: { tone: "warn", label: "Waiting for a base backup" },
  base_backup: { tone: "info", label: "Taking a base backup" },
  paused: { tone: "warn", label: "Paused" },
  disabled: { tone: "neutral", label: "Capture disabled" },
  error: { tone: "bad", label: "Error" },
};

export function pitrStateLabel(state: string) {
  return pitrStates[state] ?? { tone: "neutral" as Tone, label: state ? `Unknown state (${state})` : "Unknown state" };
}

// Go encodes a time that was never set as 0001-01-01T00:00:00Z; treat it, and anything unparsable, as unknown.
export function knownTime(iso: string | undefined) {
  if (!iso) return undefined;
  const time = Date.parse(iso);
  return Number.isFinite(time) && time > 0 ? iso : undefined;
}

export function statusIsStale(status: Pick<PITRStatus, "checked_at">, now: number) {
  const checked = knownTime(status.checked_at);
  return checked === undefined || now - Date.parse(checked) > PITR_STALE_MS;
}

export interface SettingsDraft { service: string; interval_hours: string; keep_last: string; retention_days: string; max_mib: string }
export type SettingsErrors = Partial<Record<keyof SettingsDraft, string>>;

export function settingsDraft(settings: Omit<PITRSettings, "service"> & { service?: string }): SettingsDraft {
  return {
    service: settings.service ?? "",
    interval_hours: String(settings.interval_hours),
    keep_last: String(settings.keep_last),
    retention_days: String(settings.retention_days),
    max_mib: bytesToMiBText(settings.max_bytes),
  };
}

function wholeNumber(text: string, min: number, max: number) {
  const trimmed = text.trim();
  if (!/^\d+$/.test(trimmed)) return undefined;
  const value = Number(trimmed);
  return value >= min && value <= max ? value : undefined;
}

// Mirrors the control plane's Compose service identifier rule; the server stays the authority.
const serviceName = /^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,63}$/;

// An untouched size keeps the exact saved byte count, even when it is not a whole MiB.
export function parseSettingsDraft(draft: SettingsDraft, saved: Pick<PITRSettings, "max_bytes" | "enabled"> | null): { settings: PITRSettings } | { errors: SettingsErrors } {
  const errors: SettingsErrors = {};
  const service = draft.service.trim();
  if (!serviceName.test(service)) errors.service = "Enter the PostgreSQL Compose service name, such as db or postgres.";
  const interval = wholeNumber(draft.interval_hours, 1, PITR_INTERVAL_MAX);
  if (interval === undefined) errors.interval_hours = `Enter a whole number of hours from 1 to ${PITR_INTERVAL_MAX}.`;
  const keepLast = wholeNumber(draft.keep_last, 1, PITR_KEEP_MAX);
  if (keepLast === undefined) errors.keep_last = `Enter a whole number from 1 to ${PITR_KEEP_MAX}.`;
  const days = wholeNumber(draft.retention_days, 1, PITR_DAYS_MAX);
  if (days === undefined) errors.retention_days = `Enter a whole number of days from 1 to ${PITR_DAYS_MAX}.`;
  const size = parseTransferLimit(draft.max_mib, saved?.max_bytes ?? defaultPITRSettings.max_bytes, PITR_MIN_BYTES, PITR_MAX_BYTES);
  if ("error" in size) errors.max_mib = size.error;
  if (Object.keys(errors).length > 0 || interval === undefined || keepLast === undefined || days === undefined || "error" in size) return { errors };
  return { settings: { enabled: saved?.enabled ?? true, service, interval_hours: interval, keep_last: keepLast, retention_days: days, max_bytes: size.bytes } };
}

const pad = (value: number) => String(value).padStart(2, "0");

// "YYYY-MM-DDTHH:MM:SS" in UTC.
export function utcInputValue(time: number) {
  const date = new Date(time);
  return `${date.getUTCFullYear()}-${pad(date.getUTCMonth() + 1)}-${pad(date.getUTCDate())}T${pad(date.getUTCHours())}:${pad(date.getUTCMinutes())}:${pad(date.getUTCSeconds())}`;
}

export function formatUtc(iso: string) {
  const time = Date.parse(iso);
  return Number.isFinite(time) ? `${utcInputValue(time).replace("T", " ")} UTC` : "Unknown time";
}

export interface RecoveryWindow { from: number; to: number }

// Whole seconds inside the base's observed window that are already in the past. Undefined when no window is known.
export function recoveryWindow(base: Pick<PITRBase, "created_at" | "available_to">, now: number): RecoveryWindow | undefined {
  if (!base.available_to) return undefined;
  const created = Date.parse(base.created_at);
  const available = Date.parse(base.available_to);
  if (!Number.isFinite(created) || !Number.isFinite(available)) return undefined;
  const from = Math.ceil(created / 1000) * 1000;
  const to = Math.floor(Math.min(available, now) / 1000) * 1000;
  return to >= from ? { from, to } : undefined;
}

// Reads "YYYY-MM-DD HH:MM[:SS]" (or with a T separator) as UTC and returns the ISO timestamp the API expects.
export function parseRecoveryTime(text: string, window: RecoveryWindow | undefined): { timestamp: string } | { error: string } {
  if (!window) return { error: "This base has no known continuous recovery window." };
  const match = /^(\d{4})-(\d{2})-(\d{2})[T ](\d{2}):(\d{2})(?::(\d{2}))?$/.exec(text.trim());
  if (!match) return { error: "Enter a date and time in UTC." };
  const [, year, month, day, hour, minute, second = "0"] = match;
  const time = Date.UTC(Number(year), Number(month) - 1, Number(day), Number(hour), Number(minute), Number(second));
  if (!Number.isFinite(time) || utcInputValue(time) !== `${year}-${month}-${day}T${hour}:${minute}:${pad(Number(second))}`) return { error: "Enter a valid date and time in UTC." };
  if (time < window.from || time > window.to) return { error: `Choose a time from ${formatUtc(new Date(window.from).toISOString())} to ${formatUtc(new Date(window.to).toISOString())}.` };
  return { timestamp: new Date(time).toISOString() };
}

export function sortedBases(bases: PITRBase[] | null) {
  return [...(bases ?? [])].sort((a, b) => Date.parse(b.created_at) - Date.parse(a.created_at));
}

export function usesInlineRepository(policy: { storage_id?: string; repository: string } | null) {
  return Boolean(policy && !policy.storage_id && policy.repository.startsWith("s3:"));
}

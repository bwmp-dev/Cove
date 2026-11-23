import type { PanelBackupConfig, PanelBackupInput, PanelJob, PanelJobAction, PanelRecovery, PanelSnapshot } from "../api/panel-recovery";
import { MiB, bytesToMiBText, parseTransferLimit } from "./backups";
import type { Tone } from "./nodes";

export const MIN_BACKUP_BYTES = 64 * MiB;
export const MAX_BACKUP_BYTES = 64 * 1024 * MiB;
// The host checks in every minute; the API refuses actions without contact in the last three minutes.
export const HOST_CONTACT_WINDOW_MS = 3 * 60_000;
export const SETUP_COMMAND = "sudo sh scripts/install-panel-recovery.sh /opt/cove";
// For a rollback to a panel version that predates this settings page.
export const RESUME_COMMAND = "sudo python3 /usr/local/lib/cove/panel-recovery.py resume-updates --installation /opt/cove";

export const jobActionLabels: Record<PanelJobAction, string> = { backup: "Back up", restore: "Restore snapshot", resume_updates: "Resume updates" };

export type HostStatus = "unregistered" | "connected" | "quiet";

export function hostStatus(recovery: Pick<PanelRecovery, "agent_id" | "checked_at">, now: number): HostStatus {
  if (!recovery.agent_id || !recovery.checked_at) return "unregistered";
  const checked = Date.parse(recovery.checked_at);
  return Number.isFinite(checked) && now - checked < HOST_CONTACT_WINDOW_MS ? "connected" : "quiet";
}

export const hostStatusLabels: Record<HostStatus, { tone: Tone; label: string }> = {
  unregistered: { tone: "neutral", label: "Host timer not connected" },
  connected: { tone: "ok", label: "Host timer connected" },
  quiet: { tone: "warn", label: "No recent host contact" },
};

export function activeJob(jobs: PanelJob[] | null) {
  return jobs?.find((job) => job.state === "queued" || job.state === "running");
}

export function sortedSnapshots(snapshots: PanelSnapshot[] | null) {
  return [...(snapshots ?? [])].sort((a, b) => Date.parse(b.created_at) - Date.parse(a.created_at));
}

// Reason the panel cannot start any recovery action right now, or undefined when it can.
export function actionBlockReason(recovery: PanelRecovery, now: number, online: boolean): string | undefined {
  if (!online) return "You are offline. Reconnect to start a recovery action.";
  const status = hostStatus(recovery, now);
  if (status === "unregistered") return "Install the host recovery timer on the panel host first.";
  if (recovery.pending_recovery) return "The host is still recovering an earlier action. Inspect the host before starting another.";
  const active = activeJob(recovery.jobs);
  if (active) return `${jobActionLabels[active.action]} is ${active.state}. Wait for it to finish.`;
  if (status === "quiet") return "The host has not checked in for over 3 minutes. It may be busy with a long backup or offline; actions are accepted again once it checks in.";
  return undefined;
}

export function restoreBlockReason(snapshot: PanelSnapshot, publicOrigin: string): string | undefined {
  if (!snapshot.verified) return "Not verified, so it cannot be restored.";
  if (snapshot.origin !== publicOrigin) return `Taken for ${snapshot.origin || "another address"}, not ${publicOrigin}.`;
  return undefined;
}

export interface ConfigDraft {
  enabled: boolean;
  interval_hours: string;
  keep_last: string;
  keep_days: string;
  max_mib: string;
  storage_id: string;
  password: string;
}

export function configDraft(config: PanelBackupConfig): ConfigDraft {
  return {
    enabled: config.enabled,
    interval_hours: String(config.interval_hours),
    keep_last: String(config.keep_last),
    keep_days: String(config.keep_days),
    max_mib: bytesToMiBText(config.max_bytes),
    storage_id: config.storage_id,
    password: "",
  };
}

export function draftChanged(draft: ConfigDraft, config: PanelBackupConfig) {
  const saved = configDraft(config);
  return (Object.keys(saved) as (keyof ConfigDraft)[]).some((key) => (typeof draft[key] === "string" ? String(draft[key]).trim() !== String(saved[key]).trim() : draft[key] !== saved[key]));
}

export type ConfigErrors = Partial<Record<"interval_hours" | "keep_last" | "keep_days" | "max_mib" | "password", string>>;

function wholeNumber(text: string, min: number, max: number) {
  const trimmed = text.trim();
  if (!/^\d+$/.test(trimmed)) return undefined;
  const value = Number(trimmed);
  return value >= min && value <= max ? value : undefined;
}

export function passwordBytes(password: string) {
  return new TextEncoder().encode(password).length;
}

// The saved password is reused only when the destination stays the same and no new one is typed.
export function needsNewPassword(draft: Pick<ConfigDraft, "storage_id">, config: PanelBackupConfig, passwordSet: boolean) {
  return draft.storage_id !== "" && (draft.storage_id !== config.storage_id || !passwordSet);
}

export function parseConfigDraft(draft: ConfigDraft, config: PanelBackupConfig, passwordSet: boolean): { input: PanelBackupInput } | { errors: ConfigErrors } {
  const errors: ConfigErrors = {};
  const interval = wholeNumber(draft.interval_hours, 1, 8760);
  if (interval === undefined) errors.interval_hours = "Enter a whole number of hours from 1 to 8,760.";
  const keepLast = wholeNumber(draft.keep_last, 1, 100);
  if (keepLast === undefined) errors.keep_last = "Enter a whole number from 1 to 100.";
  const keepDays = wholeNumber(draft.keep_days, 1, 365);
  if (keepDays === undefined) errors.keep_days = "Enter a whole number of days from 1 to 365.";
  const size = parseTransferLimit(draft.max_mib, config.max_bytes, MIN_BACKUP_BYTES, MAX_BACKUP_BYTES);
  if ("error" in size) errors.max_mib = size.error;
  const password = draft.storage_id ? draft.password : "";
  if (password) {
    const length = passwordBytes(password);
    if (length < 16 || length > 4096) errors.password = "Use 16 to 4,096 bytes.";
  } else if (needsNewPassword(draft, config, passwordSet)) {
    errors.password = "Enter an encryption password for the offsite copy.";
  }
  if (interval === undefined || keepLast === undefined || keepDays === undefined || "error" in size || errors.password) return { errors };
  const input: PanelBackupInput = { enabled: draft.enabled, interval_hours: interval, keep_last: keepLast, keep_days: keepDays, max_bytes: size.bytes, storage_id: draft.storage_id };
  if (password) input.password = password;
  return { input };
}

import { expect, it } from "vitest";
import type { PanelBackupConfig, PanelRecovery } from "../api/panel-recovery";
import { actionBlockReason, configDraft, draftChanged, hostStatus, parseConfigDraft, restoreBlockReason, sortedSnapshots } from "./panel-recovery";

const MiB = 1024 * 1024;
const config: PanelBackupConfig = { enabled: true, interval_hours: 24, keep_last: 7, keep_days: 30, max_bytes: 8 * 1024 * MiB, storage_id: "" };
const now = Date.parse("2026-11-22T12:00:00Z");
const recovery: PanelRecovery = {
  config,
  password_set: false,
  agent_id: "agent-1",
  checked_at: "2026-11-22T11:59:30Z",
  last_error: "",
  updates_paused: false,
  pending_recovery: false,
  offsite_pending: false,
  public_origin: "https://panel.example.com",
  snapshots: [],
  jobs: [],
};

it("tells an unregistered host apart from one that has gone quiet", () => {
  expect(hostStatus({ agent_id: "", checked_at: null }, now)).toBe("unregistered");
  expect(hostStatus(recovery, now)).toBe("connected");
  expect(hostStatus({ ...recovery, checked_at: "2026-11-22T11:56:00Z" }, now)).toBe("quiet");
});

it("blocks duplicate actions while a job is active or the host is recovering", () => {
  expect(actionBlockReason(recovery, now, true)).toBeUndefined();
  expect(actionBlockReason(recovery, now, false)).toMatch(/offline/);
  expect(actionBlockReason({ ...recovery, agent_id: "", checked_at: null }, now, true)).toMatch(/Install the host recovery timer/);
  expect(actionBlockReason({ ...recovery, pending_recovery: true }, now, true)).toMatch(/still recovering/);
  const job = { id: "j", action: "backup" as const, snapshot_id: "", state: "running" as const, requested_at: "2026-11-22T11:58:00Z", finished_at: null, error: "" };
  expect(actionBlockReason({ ...recovery, jobs: [job] }, now, true)).toBe("Back up is running. Wait for it to finish.");
  expect(actionBlockReason({ ...recovery, checked_at: "2026-11-22T11:50:00Z" }, now, true)).toMatch(/may be busy with a long backup/);
});

it("offers restore only for verified snapshots of this public address, newest first", () => {
  const base = { version: "v1", revision: "abc", bytes: 1, offsite: false, origin: "https://panel.example.com", verified: true };
  const older = { ...base, id: "a".repeat(64), created_at: "2026-11-20T00:00:00Z" };
  const newer = { ...base, id: "b".repeat(64), created_at: "2026-11-21T00:00:00Z" };
  expect(sortedSnapshots([older, newer]).map((snapshot) => snapshot.id)).toEqual([newer.id, older.id]);
  expect(restoreBlockReason(older, recovery.public_origin)).toBeUndefined();
  expect(restoreBlockReason({ ...older, verified: false }, recovery.public_origin)).toMatch(/Not verified/);
  expect(restoreBlockReason({ ...older, origin: "https://old.example.com" }, recovery.public_origin)).toMatch(/old\.example\.com/);
});

it("validates ranges and keeps an untouched odd byte limit exact", () => {
  const odd = { ...config, max_bytes: 100 * MiB + 5 };
  const parsed = parseConfigDraft(configDraft(odd), odd, false);
  expect(parsed).toEqual({ input: { ...odd } });
  const invalid = parseConfigDraft({ ...configDraft(config), interval_hours: "0", keep_last: "101", keep_days: "1.5", max_mib: "63" }, config, false);
  expect("errors" in invalid && Object.keys(invalid.errors).sort()).toEqual(["interval_hours", "keep_days", "keep_last", "max_mib"]);
});

it("requires a new offsite password only for a new destination and measures it in bytes", () => {
  const offsite = { ...config, storage_id: "s1" };
  expect(parseConfigDraft(configDraft(offsite), offsite, true)).toEqual({ input: offsite });
  expect(parseConfigDraft({ ...configDraft(offsite), storage_id: "s2" }, offsite, true)).toEqual({ errors: { password: "Enter an encryption password for the offsite copy." } });
  expect(parseConfigDraft({ ...configDraft(config), storage_id: "s1", password: "short" }, config, false)).toEqual({ errors: { password: "Use 16 to 4,096 bytes." } });
  // Eight two-byte characters reach the 16-byte minimum.
  expect(parseConfigDraft({ ...configDraft(config), storage_id: "s1", password: "éééééééé" }, config, false)).toEqual({ input: { ...config, storage_id: "s1", password: "éééééééé" } });
  expect(parseConfigDraft({ ...configDraft(offsite), storage_id: "", password: "x".repeat(20) }, offsite, true)).toEqual({ input: config });
});

it("detects unsaved changes", () => {
  expect(draftChanged(configDraft(config), config)).toBe(false);
  expect(draftChanged({ ...configDraft(config), enabled: false }, config)).toBe(true);
  expect(draftChanged({ ...configDraft(config), password: "new" }, config)).toBe(true);
});

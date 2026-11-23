import { describe, expect, it } from "vitest";
import { MiB } from "./backups";
import { formatUtc, parseRecoveryTime, parseSettingsDraft, pitrStateLabel, recoveryWindow, settingsDraft, sortedBases, statusIsStale, usesInlineRepository } from "./pitr";

const saved = { enabled: false, service: "db", interval_hours: 12, keep_last: 4, retention_days: 7, max_bytes: 5 * 1024 * MiB + 123 };

describe("parseSettingsDraft", () => {
  it("keeps an untouched odd byte cap exactly and preserves the enabled flag", () => {
    expect(parseSettingsDraft(settingsDraft(saved), saved)).toEqual({ settings: saved });
  });

  it("converts an edited cap to exact bytes and enables new settings", () => {
    const draft = { ...settingsDraft(saved), max_mib: "2048" };
    expect(parseSettingsDraft(draft, null)).toEqual({ settings: { ...saved, enabled: true, max_bytes: 2048 * MiB } });
  });

  it("rejects values outside the API ranges", () => {
    const result = parseSettingsDraft({ service: "-db", interval_hours: "169", keep_last: "0", retention_days: "31", max_mib: "1023" }, null);
    expect("errors" in result && Object.keys(result.errors).sort()).toEqual(["interval_hours", "keep_last", "max_mib", "retention_days", "service"]);
  });

  it("rejects fractional and padded numbers", () => {
    expect("errors" in parseSettingsDraft({ ...settingsDraft(saved), interval_hours: "1.5" }, saved)).toBe(true);
    expect("errors" in parseSettingsDraft({ ...settingsDraft(saved), max_mib: "65537" }, saved)).toBe(true);
  });
});

describe("recovery windows", () => {
  const base = { created_at: "2026-11-22T10:00:00.400Z", available_to: "2026-11-22T11:30:15.900Z" };
  const now = Date.parse("2026-11-22T12:00:00Z");

  it("rounds inward to whole seconds and is absent without available_to", () => {
    expect(recoveryWindow(base, now)).toEqual({ from: Date.parse("2026-11-22T10:00:01Z"), to: Date.parse("2026-11-22T11:30:15Z") });
    expect(recoveryWindow({ created_at: base.created_at }, now)).toBeUndefined();
  });

  it("never extends past the current time", () => {
    expect(recoveryWindow(base, Date.parse("2026-11-22T11:00:00.700Z"))?.to).toBe(Date.parse("2026-11-22T11:00:00Z"));
  });

  it("reads the input as UTC and keeps it inside the window", () => {
    const window = recoveryWindow(base, now);
    expect(parseRecoveryTime("2026-11-22T11:00:30", window)).toEqual({ timestamp: "2026-11-22T11:00:30.000Z" });
    expect(parseRecoveryTime("2026-11-22T11:00", window)).toEqual({ timestamp: "2026-11-22T11:00:00.000Z" });
    expect(parseRecoveryTime(" 2026-11-22 11:00:30 ", window)).toEqual({ timestamp: "2026-11-22T11:00:30.000Z" });
    expect(parseRecoveryTime("2026-11-22T10:00:00", window)).toEqual({ error: "Choose a time from 2026-11-22 10:00:01 UTC to 2026-11-22 11:30:15 UTC." });
    expect(parseRecoveryTime("2026-02-30T10:00:00", window)).toEqual({ error: "Enter a valid date and time in UTC." });
    expect(parseRecoveryTime("2026-11-22T11:00:00", undefined)).toEqual({ error: "This base has no known continuous recovery window." });
  });

  it("formats UTC and unknown times", () => {
    expect(formatUtc("2026-11-22T09:05:03Z")).toBe("2026-11-22 09:05:03 UTC");
    expect(formatUtc("")).toBe("Unknown time");
  });
});

it("treats heartbeats older than 30 seconds or missing as stale", () => {
  const now = Date.parse("2026-11-22T12:00:00Z");
  expect(statusIsStale({ checked_at: "2026-11-22T11:59:40Z" }, now)).toBe(false);
  expect(statusIsStale({ checked_at: "2026-11-22T11:59:29Z" }, now)).toBe(true);
  expect(statusIsStale({ checked_at: "" }, now)).toBe(true);
  expect(statusIsStale({ checked_at: "0001-01-01T00:00:00Z" }, now)).toBe(true);
});

it("labels unknown states without guessing a tone", () => {
  expect(pitrStateLabel("paused")).toEqual({ tone: "warn", label: "Paused" });
  expect(pitrStateLabel("rewinding")).toEqual({ tone: "neutral", label: "Unknown state (rewinding)" });
});

it("sorts bases newest first and tolerates a null list", () => {
  expect(sortedBases(null)).toEqual([]);
  const bases = [{ id: "a", created_at: "2026-11-20T00:00:00Z", bytes: 1, postgres_major: 17 }, { id: "b", created_at: "2026-11-21T00:00:00Z", bytes: 1, postgres_major: 17 }];
  expect(sortedBases(bases).map((base) => base.id)).toEqual(["b", "a"]);
});

it("detects legacy inline S3 repositories", () => {
  expect(usesInlineRepository({ repository: "s3:https://s3.example.com/bucket" })).toBe(true);
  expect(usesInlineRepository({ repository: "s3:https://s3.example.com/bucket", storage_id: "loc" })).toBe(false);
  expect(usesInlineRepository({ repository: "local" })).toBe(false);
});

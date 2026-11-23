import { expect, it } from "vitest";
import { backupScopeLabel, parseSnapshotSpace, SNAPSHOT_DEFAULT_BYTES, volumeMethodLabel, bytesToMiBText, calendarRetention, downloadFileName, matchesPolicy, MiB, objectKeyError, parseTransferLimit, retentionSummary, usesCalendarRetention } from "./backups";

it("treats omitted calendar tiers as disabled", () => {
  expect(calendarRetention({ keep_weekly: 4 })).toEqual({ keep_daily: 0, keep_weekly: 4, keep_monthly: 0 });
  expect(calendarRetention(null)).toEqual({ keep_daily: 0, keep_weekly: 0, keep_monthly: 0 });
  expect(usesCalendarRetention(calendarRetention({}))).toBe(false);
  expect(usesCalendarRetention(calendarRetention({ keep_monthly: 1 }))).toBe(true);
});

it("summarises latest snapshots as additional to calendar tiers", () => {
  expect(retentionSummary({ keep_last: 7 })).toBe("Keep 7 latest snapshots");
  expect(retentionSummary({ keep_last: 1, keep_daily: 7, keep_monthly: 12 })).toBe("Keep 1 latest snapshot plus 7 daily, 12 monthly");
});

it("labels instance engines by instance and others by database", () => {
  expect(backupScopeLabel({ engine: "valkey", name: "all" })).toBe("Valkey complete instance");
  expect(backupScopeLabel({ engine: "mongodb", name: "all" })).toBe("MongoDB complete instance");
  expect(backupScopeLabel({ engine: "mariadb", name: "all" })).toBe("MariaDB database all");
});

it("matches a snapshot only to a policy with the same repository and exact database scope", () => {
  const db = { engine: "redis", service: "cache", name: "all" };
  expect(matchesPolicy({ repository: "local" }, { repository: "local" })).toBe(true);
  expect(matchesPolicy({ repository: "local", database: db }, { repository: "local", database: { ...db } })).toBe(true);
  expect(matchesPolicy({ repository: "local", database: db }, { repository: "local", database: { ...db, service: "redis" } })).toBe(false);
  expect(matchesPolicy({ repository: "local", database: db }, { repository: "local" })).toBe(false);
  expect(matchesPolicy({ repository: "s3:https://a/b" }, { repository: "local" })).toBe(false);
  expect(matchesPolicy(null, { repository: "local" })).toBe(false);
});

it("names downloads by what the backup contains", () => {
  expect(downloadFileName("b1", undefined)).toBe("cove-backup-b1.tar");
  expect(downloadFileName("b1", { engine: "postgresql", service: "db", name: "app" })).toBe("cove-backup-b1.dump");
  expect(downloadFileName("b1", { engine: "mongodb", service: "db", name: "all" })).toBe("cove-backup-b1.dump");
  expect(downloadFileName("b1", { engine: "valkey", service: "db", name: "all" })).toBe("cove-backup-b1.rdb");
});

it("accepts only relative object keys that stay inside the storage path", () => {
  expect(objectKeyError("dumps/2026/app.dump")).toBeUndefined();
  expect(objectKeyError("")).toBeDefined();
  expect(objectKeyError("/dumps/app.dump")).toBeDefined();
  expect(objectKeyError("dumps/../secret")).toBeDefined();
  expect(objectKeyError("./app.dump")).toBeDefined();
  expect(objectKeyError("dumps//app.dump")).toBeDefined();
  expect(objectKeyError("dumps/")).toBeDefined();
  expect(objectKeyError("dumps\\app.dump")).toBeDefined();
  expect(objectKeyError("c:app.dump")).toBeDefined();
  expect(objectKeyError("dumps/app\u0007.dump")).toBeDefined();
  expect(objectKeyError("\u00e9".repeat(512))).toBeUndefined();
  expect(objectKeyError("\u00e9".repeat(513))).toBeDefined();
});

it("converts edited transfer limits to exact bytes and keeps an untouched saved value", () => {
  const min = MiB;
  const max = 64 * 1024 * MiB;
  expect(parseTransferLimit("2048", 1024 * MiB, min, max)).toEqual({ bytes: 2048 * MiB });
  expect(parseTransferLimit("65536", 1024 * MiB, min, max)).toEqual({ bytes: max });
  const odd = 1536 * MiB + 7;
  expect(bytesToMiBText(odd)).toBe("1536.00");
  expect(parseTransferLimit(bytesToMiBText(odd), odd, min, max)).toEqual({ bytes: odd });
  expect(parseTransferLimit("0", 1024 * MiB, min, max)).toHaveProperty("error");
  expect(parseTransferLimit("65537", 1024 * MiB, min, max)).toHaveProperty("error");
  expect(parseTransferLimit("1.5", 1024 * MiB, min, max)).toHaveProperty("error");
  expect(parseTransferLimit("", 1024 * MiB, min, max)).toHaveProperty("error");
});

it("edits snapshot space in MiB while keeping untouched saved values exact", () => {
  expect(parseSnapshotSpace("1024", null)).toEqual({ bytes: SNAPSHOT_DEFAULT_BYTES });
  expect(parseSnapshotSpace("1024", { max_bytes: 0 })).toEqual({ bytes: 0 });
  const odd = 100 * MiB + 1;
  expect(parseSnapshotSpace(bytesToMiBText(odd), { max_bytes: odd })).toEqual({ bytes: odd });
  expect(parseSnapshotSpace("2048", { max_bytes: odd })).toEqual({ bytes: 2048 * MiB });
  expect(parseSnapshotSpace("63", null)).toEqual({ error: "Enter a whole number from 64 to 65,536 MiB." });
  expect(parseSnapshotSpace("65537", null)).toHaveProperty("error");
  expect(parseSnapshotSpace("1.5", null)).toHaveProperty("error");
});

it("names the actual volume method of a backup", () => {
  expect(volumeMethodLabel(undefined)).toBeUndefined();
  expect(volumeMethodLabel("stopped")).toBe("Stopped containers");
  expect(volumeMethodLabel("zfs")).toBe("ZFS filesystem snapshot");
  expect(volumeMethodLabel("btrfs+lvm")).toBe("Btrfs + LVM filesystem snapshot");
});

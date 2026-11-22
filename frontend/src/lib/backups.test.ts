import { expect, it } from "vitest";
import { backupScopeLabel, calendarRetention, retentionSummary, usesCalendarRetention } from "./backups";

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

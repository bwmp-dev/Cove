export type CalendarTier = "keep_daily" | "keep_weekly" | "keep_monthly";
export type CalendarRetention = Record<CalendarTier, number>;

export const calendarTiers: { key: CalendarTier; label: string; unit: string; max: number }[] = [
  { key: "keep_daily", label: "Daily snapshots", unit: "days", max: 366 },
  { key: "keep_weekly", label: "Weekly snapshots", unit: "weeks", max: 520 },
  { key: "keep_monthly", label: "Monthly snapshots", unit: "months", max: 120 },
];

export const KEEP_LAST_MAX = 1000;

// The API omits zero tiers, so a missing value means the tier is off.
export function calendarRetention(policy: Partial<CalendarRetention> | null | undefined): CalendarRetention {
  return { keep_daily: policy?.keep_daily ?? 0, keep_weekly: policy?.keep_weekly ?? 0, keep_monthly: policy?.keep_monthly ?? 0 };
}

export function usesCalendarRetention(retention: CalendarRetention) {
  return retention.keep_daily > 0 || retention.keep_weekly > 0 || retention.keep_monthly > 0;
}

export function retentionSummary(policy: { keep_last: number } & Partial<CalendarRetention>) {
  const retention = calendarRetention(policy);
  const tiers = [
    retention.keep_daily && `${retention.keep_daily} daily`,
    retention.keep_weekly && `${retention.keep_weekly} weekly`,
    retention.keep_monthly && `${retention.keep_monthly} monthly`,
  ].filter(Boolean);
  const latest = `${policy.keep_last} latest ${policy.keep_last === 1 ? "snapshot" : "snapshots"}`;
  return tiers.length ? `Keep ${latest} plus ${tiers.join(", ")}` : `Keep ${latest}`;
}

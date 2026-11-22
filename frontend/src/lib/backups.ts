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

export type DatabaseScope = "database" | "instance";
export interface DatabaseEngine { value: string; name: string; option: string; scope: DatabaseScope }

// Instance engines back up the whole server; the API requires name "all" for them.
export const databaseEngines: DatabaseEngine[] = [
  { value: "postgresql", name: "PostgreSQL", option: "PostgreSQL logical dump (online)", scope: "database" },
  { value: "mariadb", name: "MariaDB", option: "MariaDB logical dump (online, InnoDB only)", scope: "database" },
  { value: "mongodb", name: "MongoDB", option: "MongoDB replica set archive (online)", scope: "instance" },
  { value: "redis", name: "Redis", option: "Redis RDB snapshot (online)", scope: "instance" },
  { value: "valkey", name: "Valkey", option: "Valkey RDB snapshot (online)", scope: "instance" },
];

export const INSTANCE_SCOPE_NAME = "all";

export function databaseEngine(value: string | undefined) {
  return databaseEngines.find((engine) => engine.value === value);
}

export function backupScopeLabel(database: { engine: string; name: string }) {
  const engine = databaseEngine(database.engine);
  if (!engine) return `${database.engine} database ${database.name}`;
  return engine.scope === "instance" ? `${engine.name} complete instance` : `${engine.name} database ${database.name}`;
}

export interface DatabaseBackup { engine: string; service: string; name: string }

// The worker refuses a download unless the snapshot's repository and database scope equal the saved policy's.
export function matchesPolicy(policy: { repository: string; database?: DatabaseBackup } | null, snapshot: { repository: string; database?: DatabaseBackup }) {
  if (!policy || policy.repository !== snapshot.repository) return false;
  const a = policy.database;
  const b = snapshot.database;
  if (!a || !b) return !a && !b;
  return a.engine === b.engine && a.service === b.service && a.name === b.name;
}

export interface ImportFormat { file: string; description: string; warning?: string }

export const importFormats: Record<string, ImportFormat> = {
  postgresql: { file: "PostgreSQL custom-format dump", description: "Created with pg_dump --format=custom (the format of a downloaded PostgreSQL backup). Plain SQL and directory dumps are refused." },
  mariadb: {
    file: "MariaDB SQL dump",
    description: "A mysqldump or mariadb-dump SQL file of the configured database.",
    warning: "The SQL runs with the database administrator's credentials during verification and on a later restore, so it can change server-wide objects. Import only dumps you trust.",
  },
  mongodb: { file: "MongoDB archive", description: "A full mongodump --archive of the complete instance taken with --oplog, with the configured root credentials preserved (the format of a downloaded MongoDB backup)." },
  redis: { file: "Redis RDB snapshot", description: "An RDB file such as dump.rdb or one created with redis-cli --rdb. AOF files are refused." },
  valkey: { file: "Valkey RDB snapshot", description: "An RDB file such as dump.rdb or one created with valkey-cli --rdb. AOF files are refused." },
};

export function downloadFileName(backupId: string, database: DatabaseBackup | undefined) {
  if (!database) return `cove-backup-${backupId}.tar`;
  return `cove-backup-${backupId}${database.engine === "redis" || database.engine === "valkey" ? ".rdb" : ".dump"}`;
}

const encoder = new TextEncoder();

// Mirrors the control plane's object key rule; the server stays the authority.
export function objectKeyError(key: string): string | undefined {
  if (key === "") return "Enter the object key.";
  if (encoder.encode(key).length > 1024) return "Object keys are limited to 1024 bytes.";
  // eslint-disable-next-line no-control-regex
  if (/[\u0000-\u001f\u007f-\u009f\\:]/.test(key)) return "Object keys cannot contain control characters, backslashes or colons.";
  if (key.startsWith("/")) return "Enter a key relative to the storage location's path, without a leading slash.";
  const parts = key.split("/");
  if (parts.some((part) => part === "" || part === "." || part === "..")) return "Object keys cannot contain empty, . or .. segments.";
  return undefined;
}

export const MiB = 1024 * 1024;

export function bytesToMiBText(bytes: number) {
  const mib = bytes / MiB;
  return Number.isInteger(mib) ? String(mib) : mib.toFixed(2);
}

// Limits are edited in whole MiB so the saved value is an exact integer number of bytes.
// Untouched text keeps the exact saved value even when it is not a whole MiB.
export function parseTransferLimit(text: string, saved: number, min: number, max: number): { bytes: number } | { error: string } {
  const trimmed = text.trim();
  if (trimmed === bytesToMiBText(saved)) return { bytes: saved };
  const minMiB = Math.ceil(min / MiB);
  const maxMiB = Math.floor(max / MiB);
  const error = `Enter a whole number from ${minMiB.toLocaleString("en")} to ${maxMiB.toLocaleString("en")} MiB.`;
  if (!/^\d+$/.test(trimmed)) return { error };
  const mib = Number(trimmed);
  if (mib < minMiB || mib > maxMiB) return { error };
  return { bytes: mib * MiB };
}

const relative = new Intl.RelativeTimeFormat("en", { numeric: "auto" });
const absolute = new Intl.DateTimeFormat("en", { dateStyle: "medium", timeStyle: "medium" });

const units: [Intl.RelativeTimeFormatUnit, number][] = [
  ["day", 86_400_000],
  ["hour", 3_600_000],
  ["minute", 60_000],
  ["second", 1_000],
];

export function formatRelative(iso: string, now: number) {
  const time = Date.parse(iso);
  if (!Number.isFinite(time)) {
    return "Unknown time";
  }
  const diff = time - now;
  if (Math.abs(diff) < 10_000) {
    return "just now";
  }
  for (const [unit, size] of units) {
    if (Math.abs(diff) >= size || unit === "second") {
      return relative.format(Math.round(diff / size), unit);
    }
  }
  return absolute.format(time);
}

export function formatAbsolute(iso: string) {
  const time = Date.parse(iso);
  return Number.isFinite(time) ? absolute.format(time) : "Unknown time";
}

export function formatDuration(ms: number) {
  const seconds = Math.round(ms / 1000);
  if (seconds < 60) {
    return `${seconds}s`;
  }
  const minutes = Math.floor(seconds / 60);
  if (minutes < 60) {
    return `${minutes}m ${seconds % 60}s`;
  }
  return `${Math.floor(minutes / 60)}h ${minutes % 60}m`;
}

export function formatCountdown(ms: number) {
  const total = Math.max(0, Math.ceil(ms / 1000));
  const minutes = Math.floor(total / 60);
  return `${minutes}:${String(total % 60).padStart(2, "0")}`;
}

export function shortId(id: string) {
  return id.slice(0, 8);
}

export function formatBytes(bytes: number) {
  if (bytes < 1024) {
    return `${bytes} B`;
  }
  if (bytes < 1024 * 1024) {
    return `${(bytes / 1024).toFixed(1)} KB`;
  }
  return `${(bytes / 1024 / 1024).toFixed(2)} MB`;
}

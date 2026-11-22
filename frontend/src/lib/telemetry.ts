import type { MetricHistory, MetricRange, MetricTier, RetentionSettings } from "../api/telemetry";
import type { Container, NodeMetrics } from "../api/types";

export const rangeOptions: { value: MetricRange; label: string }[] = [
  { value: "hour", label: "Last hour" },
  { value: "day", label: "Last 24 hours" },
  { value: "week", label: "Last 7 days" },
  { value: "month", label: "Last 30 days" },
  { value: "year", label: "Last 365 days" },
];

export function isMetricRange(value: string): value is MetricRange {
  return rangeOptions.some((option) => option.value === value);
}

const hour = 3_600_000;
const day = 24 * hour;

export const rangeWindows: Record<MetricRange, number> = { hour, day, week: 7 * day, month: 30 * day, year: 365 * day };

const tierSteps: Record<MetricTier, number> = { minute: 60_000, hour, day };

export const tierLabels: Record<MetricTier, string> = { minute: "minute", hour: "hour", day: "day" };

// A bucket can be skipped by a slow collection pass without data being lost, so only a longer
// silence than two missed buckets is drawn as a gap.
export function maxGap(tier: MetricTier) {
  return tierSteps[tier] * 3;
}

// A value is a reading only when it is a finite number; anything else is unknown and is never drawn as zero.
export function reading(value: unknown): number | undefined {
  return typeof value === "number" && Number.isFinite(value) ? value : undefined;
}

export interface ChartPoint {
  t: number;
  v: number | undefined;
}

// Six categorical colours for containers and paired lines. Class names are literal so Tailwind generates them.
const seriesColours = [
  { stroke: "stroke-series-1", bg: "bg-series-1" },
  { stroke: "stroke-series-2", bg: "bg-series-2" },
  { stroke: "stroke-series-3", bg: "bg-series-3" },
  { stroke: "stroke-series-4", bg: "bg-series-4" },
  { stroke: "stroke-series-5", bg: "bg-series-5" },
  { stroke: "stroke-series-6", bg: "bg-series-6" },
] as const;

export type SeriesColour = (typeof seriesColours)[number];

export function seriesColour(index: number): SeriesColour {
  return seriesColours[((index % seriesColours.length) + seriesColours.length) % seriesColours.length] ?? seriesColours[0];
}

// The same container name always gets the same colour, on every page and in every chart.
export function containerColour(name: string): SeriesColour {
  let hash = 2166136261;
  for (let index = 0; index < name.length; index++) {
    hash ^= name.charCodeAt(index);
    hash = Math.imul(hash, 16777619);
  }
  return seriesColour(hash >>> 0);
}

export interface ChartSeries {
  key: string;
  label: string;
  colour: SeriesColour;
  points: ChartPoint[];
}

// Splits a line wherever a value is unknown or samples are further apart than maxGap, so gaps stay visible.
export function segments(points: ChartPoint[], gap: number): { t: number; v: number }[][] {
  const result: { t: number; v: number }[][] = [];
  let current: { t: number; v: number }[] = [];
  let previous: number | undefined;
  for (const point of points) {
    if (point.v === undefined || (previous !== undefined && point.t - previous > gap)) {
      if (current.length) result.push(current);
      current = [];
    }
    if (point.v !== undefined) current.push({ t: point.t, v: point.v });
    previous = point.t;
  }
  if (current.length) result.push(current);
  return result;
}

interface CounterSample {
  t: number;
  // Counters only continue within one container or one boot; a different id starts a new counter.
  id: string;
  v: number | undefined;
}

// Average bytes per second between consecutive samples of a cumulative counter. A counter that went backwards
// (restart, interface reset), a new container, an unknown value or a long silence produces a gap, not a rate.
export function counterRates(samples: CounterSample[], gap: number): ChartPoint[] {
  const points: ChartPoint[] = [];
  let previous: CounterSample | undefined;
  for (const current of samples) {
    if (previous) {
      const elapsed = current.t - previous.t;
      const delta = current.v !== undefined && previous.v !== undefined ? current.v - previous.v : undefined;
      const valid = previous.id === current.id && delta !== undefined && delta >= 0 && elapsed > 0 && elapsed <= gap;
      points.push({ t: current.t, v: valid ? delta / (elapsed / 1000) : undefined });
    }
    previous = current;
  }
  return points;
}

function time(value: string) {
  const parsed = Date.parse(value);
  return Number.isFinite(parsed) ? parsed : undefined;
}

export interface NodeCharts {
  cpu: ChartSeries[];
  memory: ChartSeries[];
  disk: ChartSeries[];
  network: ChartSeries[];
  memoryTotal: number | undefined;
  diskTotal: number | undefined;
  hasNetwork: boolean;
}

function used(total: unknown, available: unknown) {
  const all = reading(total);
  const free = reading(available);
  return all !== undefined && free !== undefined ? all - free : undefined;
}

export function nodeCharts(history: MetricHistory<NodeMetrics>): NodeCharts {
  const samples = history.points
    .map((point) => ({ t: time(point.sampled_at), sample: point.sample }))
    .filter((point): point is { t: number; sample: NodeMetrics } => point.t !== undefined)
    .sort((a, b) => a.t - b.t);
  const gap = maxGap(history.tier);
  const line = (key: string, label: string, colour: number, value: (sample: NodeMetrics) => number | undefined): ChartSeries => ({
    key,
    label,
    colour: seriesColour(colour),
    points: samples.map(({ t, sample }) => ({ t, v: value(sample) })),
  });
  const counter = (key: string, label: string, colour: number, value: (sample: NodeMetrics) => unknown): ChartSeries => ({
    key,
    label,
    colour: seriesColour(colour),
    // Node counters have no boot identifier; a decrease is treated as a reset.
    points: counterRates(samples.map(({ t, sample }) => ({ t, id: "node", v: reading(value(sample)) })), gap),
  });
  const latest = [...samples].reverse();
  return {
    cpu: [line("cpu", "CPU", 0, (sample) => reading(sample.cpu_percent))],
    memory: [line("memory", "Memory used", 0, (sample) => used(sample.memory_total, sample.memory_available))],
    disk: [line("disk", "Disk used", 0, (sample) => used(sample.disk_total, sample.disk_available))],
    network: [counter("rx", "Received", 0, (sample) => sample.network_rx), counter("tx", "Sent", 1, (sample) => sample.network_tx)],
    memoryTotal: latest.map(({ sample }) => reading(sample.memory_total)).find((value) => value !== undefined),
    diskTotal: latest.map(({ sample }) => reading(sample.disk_total)).find((value) => value !== undefined),
    hasNetwork: samples.some(({ sample }) => reading(sample.network_rx) !== undefined || reading(sample.network_tx) !== undefined),
  };
}

export type WorkloadMetric = "cpu" | "memory" | "networkRx" | "networkTx" | "blockRead" | "blockWritten";

export type WorkloadCharts = Record<WorkloadMetric, ChartSeries[]> & { containers: string[] };

// Lines are grouped by container name, which Compose keeps across redeploys and node moves, so one service
// stays one line. Counters still restart whenever the container id changes.
export function workloadCharts(history: MetricHistory<Container>): WorkloadCharts {
  const gap = maxGap(history.tier);
  const groups = new Map<string, { t: number; id: string; sample: Container }[]>();
  for (const point of history.points) {
    const t = time(point.sampled_at);
    if (t === undefined) continue;
    const name = point.sample.name || point.series;
    const group = groups.get(name) ?? [];
    group.push({ t, id: point.series, sample: point.sample });
    groups.set(name, group);
  }
  const names = [...groups.keys()].sort((a, b) => a.localeCompare(b));
  const charts: WorkloadCharts = { cpu: [], memory: [], networkRx: [], networkTx: [], blockRead: [], blockWritten: [], containers: names };
  for (const name of names) {
    const samples = groups.get(name)!.sort((a, b) => a.t - b.t);
    const colour = containerColour(name);
    const usage = (sample: Container) => (sample.metrics_unavailable ? undefined : sample.metrics?.usage);
    const gauge = (value: (sample: Container) => unknown) => samples.map(({ t, sample }) => ({ t, v: reading(value(sample)) }));
    const counter = (value: (sample: Container) => unknown) => counterRates(samples.map(({ t, id, sample }) => ({ t, id, v: reading(value(sample)) })), gap);
    const series = (points: ChartPoint[]): ChartSeries => ({ key: name, label: name, colour, points });
    charts.cpu.push(series(gauge((sample) => usage(sample)?.cpu_percent)));
    charts.memory.push(series(gauge((sample) => usage(sample)?.memory_used)));
    charts.networkRx.push(series(counter((sample) => usage(sample)?.network_rx)));
    charts.networkTx.push(series(counter((sample) => usage(sample)?.network_tx)));
    charts.blockRead.push(series(counter((sample) => usage(sample)?.block_read)));
    charts.blockWritten.push(series(counter((sample) => usage(sample)?.block_written)));
  }
  return charts;
}

export function hasReadings(series: ChartSeries[]) {
  return series.some((line) => line.points.some((point) => point.v !== undefined));
}

// Rounds up to 1, 2 or 5 times a power of ten so axis labels stay readable.
export function niceCeiling(value: number) {
  if (!(value > 0)) return 1;
  const power = 10 ** Math.floor(Math.log10(value));
  const scaled = value / power;
  const step = scaled <= 1 ? 1 : scaled <= 2 ? 2 : scaled <= 5 ? 5 : 10;
  return step * power;
}

const binaryUnits = ["B", "KiB", "MiB", "GiB", "TiB", "PiB"];

export function formatBinaryBytes(bytes: number) {
  if (!Number.isFinite(bytes)) return "Unknown";
  let value = Math.abs(bytes);
  let unit = 0;
  while (value >= 1024 && unit < binaryUnits.length - 1) {
    value /= 1024;
    unit++;
  }
  const digits = unit === 0 ? 0 : value < 10 ? 2 : value < 100 ? 1 : 0;
  return `${bytes < 0 ? "-" : ""}${value.toFixed(digits)} ${binaryUnits[unit]}`;
}

export function formatRate(bytesPerSecond: number) {
  return `${formatBinaryBytes(bytesPerSecond)}/s`;
}

export function formatPercent(value: number) {
  return `${value.toFixed(value >= 100 ? 0 : 1)}%`;
}

const MiB = 1024 * 1024;

type RetentionKey = keyof RetentionSettings;

// Ranges the control plane enforces, repeated here so each field can explain and check its own limit
// before saving. The server remains the authority and its message is shown if it disagrees.
export const retentionLimits: Record<RetentionKey, { min: number; max: number }> = {
  metric_rows: { min: 1000, max: 1_000_000 },
  log_rows: { min: 1000, max: 1_000_000 },
  audit_rows: { min: 1000, max: 1_000_000 },
  metric_bytes: { min: 1, max: 10 * 1024 },
  log_bytes: { min: 1, max: 10 * 1024 },
  log_days: { min: 1, max: 365 },
  audit_days: { min: 1, max: 3650 },
  raw_days: { min: 1, max: 7 },
  hour_days: { min: 1, max: 90 },
  day_days: { min: 1, max: 3650 },
};

export const retentionKeys = Object.keys(retentionLimits) as RetentionKey[];

const byteKeys = new Set<RetentionKey>(["metric_bytes", "log_bytes"]);

export function isByteLimit(key: RetentionKey) {
  return byteKeys.has(key);
}

export type RetentionDraft = Record<RetentionKey, string>;

function formatMiB(bytes: number) {
  const mib = bytes / MiB;
  return Number.isInteger(mib) ? String(mib) : mib.toFixed(2);
}

// Byte limits are edited in MiB; the rest are whole rows or days.
export function retentionDraft(settings: RetentionSettings): RetentionDraft {
  const draft = {} as RetentionDraft;
  for (const key of retentionKeys) draft[key] = isByteLimit(key) ? formatMiB(settings[key]) : String(settings[key]);
  return draft;
}

export type RetentionErrors = Partial<Record<RetentionKey, string>>;

export function parseRetention(draft: RetentionDraft, saved: RetentionSettings): { values: RetentionSettings | null; errors: RetentionErrors } {
  const errors: RetentionErrors = {};
  const values = { ...saved };
  const unchanged = retentionDraft(saved);
  for (const key of retentionKeys) {
    const text = draft[key].trim();
    // An untouched byte limit keeps its exact saved value instead of a rounded MiB conversion.
    if (text === unchanged[key]) continue;
    const { min, max } = retentionLimits[key];
    const number = Number(text);
    const whole = isByteLimit(key) ? text !== "" && Number.isFinite(number) : /^\d+$/.test(text);
    if (!whole || number < min || number > max) {
      errors[key] = isByteLimit(key) ? `Enter ${min} to ${max.toLocaleString("en")} MiB.` : `Enter a whole number from ${min.toLocaleString("en")} to ${max.toLocaleString("en")}.`;
      continue;
    }
    values[key] = isByteLimit(key) ? Math.round(number * MiB) : number;
  }
  if (!errors.hour_days && !errors.raw_days && values.hour_days < values.raw_days) errors.hour_days = "Keep hourly samples at least as long as per-minute samples.";
  if (!errors.day_days && !errors.hour_days && values.day_days < values.hour_days) errors.day_days = "Keep daily samples at least as long as hourly samples.";
  return Object.keys(errors).length ? { values: null, errors } : { values, errors };
}

export function retentionChanges(saved: RetentionSettings, values: RetentionSettings): Partial<RetentionSettings> {
  const changes: Partial<RetentionSettings> = {};
  for (const key of retentionKeys) if (values[key] !== saved[key]) changes[key] = values[key];
  return changes;
}

export function retentionValueLabel(key: RetentionKey, value: number) {
  if (isByteLimit(key)) return formatBinaryBytes(value);
  if (key.endsWith("_days")) return `${value.toLocaleString("en")} ${value === 1 ? "day" : "days"}`;
  return `${value.toLocaleString("en")} rows`;
}

// The control plane rejects longer searches; the limit is in UTF-8 bytes, not characters.
export const maxLogSearchBytes = 200;

export function logSearchTooLong(text: string) {
  return new TextEncoder().encode(text).length > maxLogSearchBytes;
}

export interface LogContainer {
  id: string;
  name: string;
}

// Containers that can be chosen as a filter: those on the loaded page plus the one already selected,
// so the active filter stays visible even when it hides every other container.
export function logContainers(entries: { container_id: string; container: string }[], selected: LogContainer | null): LogContainer[] {
  const containers = new Map<string, string>();
  if (selected) containers.set(selected.id, selected.name);
  for (const entry of entries) if (!containers.has(entry.container_id)) containers.set(entry.container_id, entry.container || entry.container_id);
  return [...containers.entries()].map(([id, name]) => ({ id, name })).sort((a, b) => a.name.localeCompare(b.name) || a.id.localeCompare(b.id));
}

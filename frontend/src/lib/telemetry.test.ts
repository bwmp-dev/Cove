import { describe, expect, it } from "vitest";
import type { MetricHistory, NodeHistorySample, RetentionSettings } from "../api/telemetry";
import type { Container, MountMetric, NodeMetrics } from "../api/types";
import { containerColour, counterRates, logContainers, logSearchTooLong, mountHistories, nodeCharts, parseRetention, retentionChanges, retentionDraft, segments, workloadCharts } from "./telemetry";

const minute = 60_000;
const at = (minutes: number) => new Date(Date.UTC(2026, 10, 22, 12, minutes)).toISOString();
const t = (minutes: number) => Date.parse(at(minutes));

describe("segments", () => {
  it("breaks lines at unknown values and long silences instead of drawing zeros", () => {
    const points = [
      { t: t(0), v: 1 },
      { t: t(1), v: 2 },
      { t: t(2), v: undefined },
      { t: t(3), v: 4 },
      { t: t(10), v: 5 },
      { t: t(11), v: 6 },
    ];
    expect(segments(points, 3 * minute)).toEqual([
      [{ t: t(0), v: 1 }, { t: t(1), v: 2 }],
      [{ t: t(3), v: 4 }],
      [{ t: t(10), v: 5 }, { t: t(11), v: 6 }],
    ]);
  });
});

describe("counterRates", () => {
  it("computes per-second rates and leaves gaps at resets, new containers and long silences", () => {
    const rates = counterRates(
      [
        { t: t(0), id: "a", v: 0 },
        { t: t(1), id: "a", v: 6000 },
        { t: t(2), id: "a", v: 100 },
        { t: t(3), id: "b", v: 200 },
        { t: t(4), id: "b", v: 260 },
        { t: t(20), id: "b", v: 9000 },
        { t: t(21), id: "b", v: undefined },
      ],
      3 * minute,
    );
    expect(rates.map((point) => point.v)).toEqual([100, undefined, undefined, 1, undefined, undefined]);
    expect(rates[0]?.t).toBe(t(1));
  });
});

describe("nodeCharts", () => {
  it("orders newest-first points, keeps missing fields unknown and reports network support", () => {
    const sample = (minutes: number, extra: Partial<NodeMetrics>): NodeMetrics => ({ sampled_at: at(minutes), cpu_percent: 10, memory_total: 1000, memory_available: 400, disk_total: 5000, disk_available: 1000, ...extra });
    const history: MetricHistory<NodeHistorySample> = {
      tier: "minute",
      limited: false,
      deploy_markers: [],
      points: [
        { series: "", sampled_at: at(2), sample: sample(2, { network_rx: 600, network_tx: 1200 }) },
        { series: "", sampled_at: at(1), sample: sample(1, { cpu_percent: undefined, network_rx: 0, network_tx: 0 }) },
        { series: "", sampled_at: at(0), sample: sample(0, { memory_available: undefined }) },
        // Mount series share the history response but must not become base samples.
        { series: "mount:abc", sampled_at: at(1), sample: { path: "/data", filesystem: "ext4", total: 100, available: 40, known: true } },
      ],
    };
    const charts = nodeCharts(history);
    expect(charts.cpu[0]?.points.map((point) => point.v)).toEqual([10, undefined, 10]);
    expect(charts.memory[0]?.points.map((point) => point.v)).toEqual([undefined, 600, 600]);
    expect(charts.disk[0]?.points.map((point) => point.v)).toEqual([4000, 4000, 4000]);
    // The first sample has no network counters, so its pair yields no rate.
    expect(charts.network.map((line) => line.points.map((point) => point.v))).toEqual([
      [undefined, 10],
      [undefined, 20],
    ]);
    expect(charts.memoryTotal).toBe(1000);
    expect(charts.hasNetwork).toBe(true);
  });
});

describe("mountHistories", () => {
  it("groups mount series by path and keeps samples without capacity unknown", () => {
    const mount = (extra: Partial<MountMetric>): MountMetric => ({ path: "/data", filesystem: "ext4", total: 1000, available: 400, known: true, ...extra });
    const history: MetricHistory<NodeHistorySample> = {
      tier: "minute",
      limited: false,
      deploy_markers: [],
      points: [
        { series: "mount:b", sampled_at: at(2), sample: mount({ path: "/var", total: 0, available: 0 }) },
        { series: "mount:a", sampled_at: at(2), sample: mount({ available: 100 }) },
        { series: "mount:a", sampled_at: at(1), sample: mount({ total: 0, available: 0 }) },
        { series: "mount:a", sampled_at: at(0), sample: mount({}) },
        { series: "", sampled_at: at(0), sample: { sampled_at: at(0), cpu_percent: 1, memory_total: 1, memory_available: 1, disk_total: 1, disk_available: 1 } },
      ],
    };
    const mounts = mountHistories(history);
    expect(mounts.map((item) => item.path)).toEqual(["/data", "/var"]);
    expect(mounts[0]?.used.points.map((point) => point.v)).toEqual([600, undefined, 900]);
    expect(mounts[0]?.total).toBe(1000);
    expect(mounts[1]?.total).toBeUndefined();
    expect(mounts[1]?.used.points.map((point) => point.v)).toEqual([undefined]);
  });
});

describe("workloadCharts", () => {
  const container = (id: string, name: string, usage?: NonNullable<Container["metrics"]>["usage"], unavailable = false): Container => ({
    id,
    name,
    image: "app",
    state: "running",
    status: "Up",
    ports: "",
    metrics_unavailable: unavailable || undefined,
    metrics: { cpu: "", memory: "", memory_percent: "", network: "", disk_io: "", pids: "", usage },
  });

  it("keeps one line per container name across recreated containers without joining their counters", () => {
    const history: MetricHistory<Container> = {
      tier: "minute",
      limited: false,
      deploy_markers: [],
      points: [
        { series: "new", sampled_at: at(2), sample: container("new", "web-1", { cpu_percent: 150, network_rx: 50 }) },
        { series: "db", sampled_at: at(1), sample: container("db", "db-1", { cpu_percent: 5 }, true) },
        { series: "old", sampled_at: at(1), sample: container("old", "web-1", { cpu_percent: 20, network_rx: 9000 }) },
        { series: "old", sampled_at: at(0), sample: container("old", "web-1", { cpu_percent: 10, network_rx: 3000 }) },
      ],
    };
    const charts = workloadCharts(history);
    expect(charts.containers).toEqual(["db-1", "web-1"]);
    const web = charts.cpu.find((line) => line.key === "web-1");
    expect(web?.points.map((point) => point.v)).toEqual([10, 20, 150]);
    expect(charts.networkRx.find((line) => line.key === "web-1")?.points.map((point) => point.v)).toEqual([100, undefined]);
    // A sample flagged as unavailable is unknown even if it carries numbers.
    expect(charts.cpu.find((line) => line.key === "db-1")?.points.map((point) => point.v)).toEqual([undefined]);
    expect(web?.colour).toBe(containerColour("web-1"));
  });
});

describe("retention", () => {
  const saved: RetentionSettings = {
    metric_rows: 200000,
    metric_bytes: 256 * 1024 * 1024 + 1,
    log_rows: 200000,
    log_bytes: 128 * 1024 * 1024,
    log_days: 7,
    audit_rows: 100000,
    audit_days: 90,
    raw_days: 1,
    hour_days: 30,
    day_days: 365,
    operation_rows: 100000,
    operation_days: 90,
  };

  it("sends only changed limits and keeps untouched byte limits exact", () => {
    const draft = { ...retentionDraft(saved), log_bytes: "64", log_days: "3" };
    const { values, errors } = parseRetention(draft, saved);
    expect(errors).toEqual({});
    expect(values && retentionChanges(saved, values)).toEqual({ log_bytes: 64 * 1024 * 1024, log_days: 3 });
  });

  it("reports range and ordering errors next to the field", () => {
    const draft = { ...retentionDraft(saved), metric_rows: "10", log_days: "2.5", raw_days: "7", hour_days: "5" };
    const { values, errors } = parseRetention(draft, saved);
    expect(values).toBeNull();
    expect(errors.metric_rows).toMatch(/1,000 to 1,000,000/);
    expect(errors.log_days).toMatch(/whole number/);
    expect(errors.hour_days).toMatch(/at least as long as per-minute/);
  });
});

describe("log helpers", () => {
  it("limits search by UTF-8 bytes", () => {
    expect(logSearchTooLong("a".repeat(200))).toBe(false);
    expect(logSearchTooLong("é".repeat(101))).toBe(true);
  });

  it("keeps the selected container available when the page no longer contains it", () => {
    expect(
      logContainers(
        [
          { container_id: "b", container: "worker-1" },
          { container_id: "a", container: "api-1" },
          { container_id: "a", container: "api-1" },
        ],
        { id: "z", name: "web-1" },
      ),
    ).toEqual([
      { id: "a", name: "api-1" },
      { id: "z", name: "web-1" },
      { id: "b", name: "worker-1" },
    ]);
  });
});

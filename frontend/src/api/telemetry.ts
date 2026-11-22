import { keepPreviousData, queryOptions, useMutation, useQueryClient } from "@tanstack/react-query";
import { request, requestText } from "./client";
import type { Container, MountMetric, NodeMetrics, OperationState } from "./types";

export type MetricRange = "hour" | "day" | "week" | "month" | "year";
export type MetricTier = "minute" | "hour" | "day";

export interface MetricPoint<TSample> {
  series: string;
  sampled_at: string;
  sample: TSample;
}

export interface DeployMarker {
  operation_id: string;
  action: "deploy" | "rollback";
  state: OperationState;
  timestamp: string;
}

// Points are newest first. Each one is the latest snapshot in its tier bucket, not an average or maximum.
export interface MetricHistory<TSample> {
  tier: MetricTier;
  limited: boolean;
  points: MetricPoint<TSample>[];
  deploy_markers: DeployMarker[];
}

// Node history mixes the base host series ("") with one series per measured mount ("mount:<hash>").
export type NodeHistorySample = NodeMetrics | MountMetric;

export interface LogEntry {
  id: string;
  container_id: string;
  container: string;
  timestamp: string;
  message: string;
  clipped: boolean;
}

export interface LogCursor {
  before: string;
  before_id: string;
}

export interface LogHistory {
  entries: LogEntry[];
  collected_at: string | null;
  limited: boolean;
  collection_error: string;
  limit: number;
  next_cursor: LogCursor | null;
}

export interface LogFilter {
  q: string;
  container: string;
}

export interface RetentionSettings {
  metric_rows: number;
  metric_bytes: number;
  log_rows: number;
  log_bytes: number;
  log_days: number;
  audit_rows: number;
  audit_days: number;
  raw_days: number;
  hour_days: number;
  day_days: number;
  operation_rows: number;
  operation_days: number;
}

export interface StorageCategory {
  rows: number;
  bytes: number;
  basis: string;
}

// Stores with fixed global caps report the cap as display text; older control planes omit them.
export interface BoundedStorageCategory extends StorageCategory {
  limit: string;
}

export interface StorageUsage {
  metrics: StorageCategory;
  logs: StorageCategory;
  audit: StorageCategory;
  operations: StorageCategory;
  traffic?: BoundedStorageCategory;
  uptime?: BoundedStorageCategory;
  forwarding?: BoundedStorageCategory;
}

// Values describe each node's last bounded pass only; they are not cumulative or instance-wide totals.
export interface OperationRetentionStatus {
  node_id: string;
  name: string;
  checked_at: string | null;
  error: string;
  pruned: number;
  protected: number;
}

export const telemetryKeys = {
  workloadMetrics: (id: string, range: MetricRange) => ["workloads", id, "metrics", range] as const,
  nodeMetrics: (id: string, range: MetricRange) => ["nodes", id, "metrics", range] as const,
  logHistory: (id: string, filter: LogFilter, cursor: LogCursor | null) => ["workloads", id, "log-history", filter, cursor] as const,
  retention: ["settings", "retention"] as const,
  storageUsage: ["settings", "storage-usage"] as const,
  operationRetention: ["settings", "operation-retention"] as const,
};

// Collection runs about once a minute, so refreshing faster only repeats the same buckets.
const metricRefresh = 60_000;

export const workloadMetricsQuery = (id: string, range: MetricRange) =>
  queryOptions({
    queryKey: telemetryKeys.workloadMetrics(id, range),
    queryFn: ({ signal }) => request<MetricHistory<Container>>("GET", `/api/workloads/${encodeURIComponent(id)}/metrics?range=${range}`, undefined, signal),
    refetchInterval: metricRefresh,
    placeholderData: keepPreviousData,
  });

export const nodeMetricsQuery = (id: string, range: MetricRange) =>
  queryOptions({
    queryKey: telemetryKeys.nodeMetrics(id, range),
    queryFn: ({ signal }) => request<MetricHistory<NodeHistorySample>>("GET", `/api/nodes/${encodeURIComponent(id)}/metrics?range=${range}`, undefined, signal),
    refetchInterval: metricRefresh,
    placeholderData: keepPreviousData,
  });

export function logHistoryPath(id: string, filter: LogFilter, cursor: LogCursor | null, download = false) {
  const params = new URLSearchParams();
  if (filter.q) params.set("q", filter.q);
  if (filter.container) params.set("container", filter.container);
  if (cursor) {
    params.set("before", cursor.before);
    params.set("before_id", cursor.before_id);
  }
  if (download) params.set("download", "1");
  const query = params.toString();
  return `/api/workloads/${encodeURIComponent(id)}/log-history${query ? `?${query}` : ""}`;
}

export const logHistoryQuery = (id: string, filter: LogFilter, cursor: LogCursor | null) =>
  queryOptions({
    queryKey: telemetryKeys.logHistory(id, filter, cursor),
    queryFn: ({ signal }) => request<LogHistory>("GET", logHistoryPath(id, filter, cursor), undefined, signal),
    staleTime: 30_000,
    // Log lines can contain sensitive output; drop pages as soon as nothing shows them.
    gcTime: 0,
    placeholderData: keepPreviousData,
  });

export function downloadLogHistory(id: string, filter: LogFilter, cursor: LogCursor | null) {
  return requestText(logHistoryPath(id, filter, cursor, true));
}

export const retentionQuery = queryOptions({
  queryKey: telemetryKeys.retention,
  queryFn: ({ signal }) => request<RetentionSettings>("GET", "/api/settings/retention", undefined, signal),
});

export const storageUsageQuery = queryOptions({
  queryKey: telemetryKeys.storageUsage,
  queryFn: ({ signal }) => request<StorageUsage>("GET", "/api/settings/storage-usage", undefined, signal),
  refetchInterval: 60_000,
});

export function useUpdateRetention() {
  const client = useQueryClient();
  return useMutation({
    // Omitted fields keep their saved value, so only the changed limits are sent.
    mutationFn: (changes: Partial<RetentionSettings>) => request<RetentionSettings>("PUT", "/api/settings/retention", changes),
    onSuccess: (saved) => client.setQueryData(telemetryKeys.retention, saved),
    onSettled: () => client.invalidateQueries({ queryKey: telemetryKeys.storageUsage }),
  });
}

export const operationRetentionQuery = queryOptions({
  queryKey: telemetryKeys.operationRetention,
  queryFn: ({ signal }) => request<OperationRetentionStatus[]>("GET", "/api/settings/operation-retention", undefined, signal),
  refetchInterval: 60_000,
});

export function useRunOperationRetention() {
  const client = useQueryClient();
  return useMutation({
    mutationFn: () => request<OperationRetentionStatus[]>("POST", "/api/settings/operation-retention/run", {}),
    onSuccess: (statuses) => client.setQueryData(telemetryKeys.operationRetention, statuses),
    // A failed pass may still have pruned some nodes, so its status is read again.
    onError: () => client.invalidateQueries({ queryKey: telemetryKeys.operationRetention }),
    onSettled: () => client.invalidateQueries({ queryKey: telemetryKeys.storageUsage }),
  });
}

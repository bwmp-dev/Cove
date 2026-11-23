import { queryOptions } from "@tanstack/react-query";
import { request, requestPublic } from "./client";

export type TrafficRange = "hour" | "day" | "week";

export interface RouteTrafficSummary {
  domain: string;
  requests: number;
  status_1xx: number;
  status_2xx: number;
  status_3xx: number;
  status_4xx: number;
  status_5xx: number;
  bytes: number;
  p95_ms: number;
}

export interface TrafficEntry {
  timestamp: string;
  domain: string;
  status: number;
  bytes: number;
  duration_ms: number;
  client_ip: string;
}

export interface WorkloadTraffic {
  routes: RouteTrafficSummary[] | null;
  entries: TrafficEntry[] | null;
  collected_at: string | null;
  available: boolean;
  limited: boolean;
  collection_error: string;
  hours: number;
  retention_days: number;
  notice: string;
}

export const workloadTrafficQuery = (workloadId: string, range: TrafficRange) =>
  queryOptions({
    queryKey: ["workload-traffic", workloadId, range],
    queryFn: ({ signal }) => request<WorkloadTraffic>("GET", `/api/workloads/${encodeURIComponent(workloadId)}/traffic?range=${range}`, undefined, signal),
    refetchInterval: 60_000,
  });

export interface TrafficSettings {
  enabled: boolean;
  trusted_proxies: string[] | null;
}

export interface ProxyTrafficState {
  settings: TrafficSettings;
  configured: boolean;
  pending: boolean;
  notice: string;
}

export interface ProxyTrafficResponse {
  state: ProxyTrafficState | null;
  plan: { settings: TrafficSettings; revision: string } | null;
}

export const nodeTrafficQuery = (nodeId: string) =>
  queryOptions({
    queryKey: ["node-traffic", nodeId],
    queryFn: ({ signal }) => request<ProxyTrafficResponse>("GET", `/api/nodes/${encodeURIComponent(nodeId)}/traffic`, undefined, signal),
  });

export interface UptimeCheckInput {
  domain: string;
  path: string;
  enabled: boolean;
  interval_seconds: number;
  timeout_seconds: number;
  expected_min: number;
  expected_max: number;
  public_label: string;
}

export interface UptimeCheck extends UptimeCheckInput {
  id: string;
  checked_at: string | null;
  up: boolean | null;
  status: number | null;
  latency_ms: number | null;
  error: string;
  consecutive_failures: number;
}

export const uptimeChecksQuery = (workloadId: string) =>
  queryOptions({
    queryKey: ["uptime-checks", workloadId],
    queryFn: ({ signal }) => request<UptimeCheck[]>("GET", `/api/workloads/${encodeURIComponent(workloadId)}/uptime`, undefined, signal),
    refetchInterval: 60_000,
  });

export interface StatusPageSettings {
  enabled: boolean;
  title: string;
  slug: string;
}

export const statusPageQuery = (projectId: string) =>
  queryOptions({
    queryKey: ["status-page", projectId],
    queryFn: ({ signal }) => request<StatusPageSettings>("GET", `/api/projects/${encodeURIComponent(projectId)}/status-page`, undefined, signal),
  });

export interface PublicStatusCheck {
  label: string;
  up: boolean | null;
  checked_at: string | null;
  latency_ms: number | null;
  observed_samples_24h: number;
  observed_up_ratio_24h: number | null;
}

export interface PublicStatus {
  title: string;
  checks: PublicStatusCheck[] | null;
  source: string;
}

export const publicStatusQuery = (slug: string) =>
  queryOptions({
    queryKey: ["public-status", slug],
    // Read without the session cookie or team header, so it behaves exactly as it does for visitors.
    queryFn: ({ signal }) => requestPublic<PublicStatus>(`/api/status/${encodeURIComponent(slug)}`, signal),
    refetchInterval: 60_000,
  });

export type ForwarderKind = "loki" | "syslog" | "otlp" | "http" | "axiom" | "datadog";

export interface LogForwarder {
  id: string;
  name: string;
  kind: ForwarderKind;
  enabled: boolean;
  endpoint_set: boolean;
  token_set: boolean;
  username_set: boolean;
  last_success: string | null;
  last_error: string;
  sent_entries: number;
  dropped_entries: number;
  pending_batches: number;
}

export interface LogForwarderInput {
  name: string;
  kind: ForwarderKind;
  endpoint: string;
  token: string;
  username: string;
  enabled: boolean;
}

export const logForwardersQuery = queryOptions({
  queryKey: ["log-forwarders"],
  queryFn: ({ signal }) => request<LogForwarder[]>("GET", "/api/notifications/forwarders", undefined, signal),
  refetchInterval: 30_000,
});

export const metricsTokenQuery = queryOptions({
  queryKey: ["metrics-token"],
  queryFn: ({ signal }) => request<{ configured: boolean }>("GET", "/api/settings/metrics-token", undefined, signal),
});

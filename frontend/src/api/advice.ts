import { queryOptions, useMutation, useQueryClient } from "@tanstack/react-query";
import { request } from "./client";

export interface LiveLogEntry {
  id: string;
  container: string;
  timestamp: string;
  message: string;
  clipped: boolean;
}

export interface LiveLogSample {
  entries: LiveLogEntry[];
  sampled_at: string;
  limited: boolean;
}

export type CheckState = "pass" | "warning" | "error";

export interface CapacityCheck {
  name: string;
  state: CheckState;
  message: string;
}

export interface CapacityReport {
  checks: CapacityCheck[];
  revision?: string;
}

export interface ServiceSizing {
  service: string;
  samples: number;
  hours: number;
  first_sample: string;
  last_sample: string;
  cpu_p95_percent: number;
  memory_p95_bytes: number;
  suggested_cpus: number | null;
  suggested_memory_mb: number | null;
  notice: string;
}

export interface SizingReport {
  window: string;
  sampling: string;
  services: ServiceSizing[];
}

export interface NodeCost {
  monthly_minor: number;
  currency: string;
  configured: boolean;
}

// One row per environment and node. monthly_minor is this project's share of the node price and may be fractional.
export interface ProjectCostItem {
  environment_id: string;
  environment_name: string;
  node_id: string;
  node_name: string;
  currency: string | null;
  monthly_minor: number | null;
  resources: number;
}

export interface ProjectCosts {
  allocation: string;
  items: ProjectCostItem[];
}

export const adviceKeys = {
  liveLogs: (id: string) => ["workloads", id, "live-logs"] as const,
  sizing: (id: string) => ["workloads", id, "sizing"] as const,
  nodeCost: (id: string) => ["nodes", id, "cost"] as const,
  projectCosts: (id: string) => ["projects", id, "costs"] as const,
};

export function fetchLiveLogs(id: string, since: string | null, signal?: AbortSignal) {
  const query = since ? `?since=${encodeURIComponent(since)}` : "";
  return request<LiveLogSample>("GET", `/api/workloads/${encodeURIComponent(id)}/logs/live${query}`, undefined, signal);
}

// Capacity is inspected on the worker and can fetch Git source, so it only runs when someone asks.
export function fetchCapacity(id: string) {
  return request<CapacityReport>("GET", `/api/workloads/${encodeURIComponent(id)}/capacity`);
}

export const sizingQuery = (id: string) =>
  queryOptions({
    queryKey: adviceKeys.sizing(id),
    queryFn: ({ signal }) => request<SizingReport>("GET", `/api/workloads/${encodeURIComponent(id)}/sizing`, undefined, signal),
    // Suggestions come from hourly samples, so they change at most once an hour.
    staleTime: 5 * 60_000,
  });

export const nodeCostQuery = (id: string) =>
  queryOptions({
    queryKey: adviceKeys.nodeCost(id),
    queryFn: ({ signal }) => request<NodeCost>("GET", `/api/nodes/${encodeURIComponent(id)}/cost`, undefined, signal),
  });

export function useUpdateNodeCost(id: string) {
  const client = useQueryClient();
  return useMutation({
    mutationFn: (cost: { monthly_minor: number; currency: string }) => request<NodeCost>("PUT", `/api/nodes/${encodeURIComponent(id)}/cost`, cost),
    onSuccess: (saved) => client.setQueryData(adviceKeys.nodeCost(id), saved),
    // Every project with a resource on this node has a new allocation.
    onSettled: () => client.invalidateQueries({ predicate: (query) => query.queryKey[0] === "projects" && query.queryKey[2] === "costs" }),
  });
}

export const projectCostsQuery = (id: string) =>
  queryOptions({
    queryKey: adviceKeys.projectCosts(id),
    queryFn: ({ signal }) => request<ProjectCosts>("GET", `/api/projects/${encodeURIComponent(id)}/costs`, undefined, signal),
  });

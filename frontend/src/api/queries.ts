import { queryOptions } from "@tanstack/react-query";
import { ApiError, request } from "./client";
import type { AuthConfig, Node, Operation, OperationLogs, TeamMembership, Workload, WorkloadImages, WorkloadRuntime, WorkloadSummary } from "./types";
import { isActive } from "../lib/operations";
import { rememberTeam } from "../lib/team";

export const keys = {
  session: ["session"] as const,
  authConfig: ["auth-config"] as const,
  nodes: ["nodes"] as const,
  projects: ["projects"] as const,
  workloads: ["workloads"] as const,
  workload: (id: string) => ["workloads", id] as const,
  workloadRuntime: (id: string) => ["workloads", id, "runtime"] as const,
  workloadLogs: (id: string) => ["workloads", id, "logs"] as const,
  workloadImages: (id: string) => ["workloads", id, "images"] as const,
  operations: ["operations"] as const,
  operationLogs: (id: string) => ["operations", id, "logs"] as const,
};

export interface Session {
  user: string;
  user_id: string;
  email: string;
  name: string;
  is_admin: boolean;
  has_password: boolean;
  github_linked: boolean;
  team_id: string;
  teams: TeamMembership[];
}

export const sessionQuery = queryOptions({
  queryKey: keys.session,
  queryFn: async ({ signal }): Promise<Session | null> => {
    try {
      return await request<Session>("GET", "/api/session", undefined, signal);
    } catch (error) {
      if (error instanceof ApiError && error.status === 401) {
        // Covers a remembered team this account can no longer use: signing in again starts from the default team with an empty cache.
        rememberTeam(null);
      }
      throw error;
    }
  },
  staleTime: 60_000,
});

export const authConfigQuery = queryOptions({
  queryKey: keys.authConfig,
  queryFn: ({ signal }) => request<AuthConfig>("GET", "/auth/config", undefined, signal),
  staleTime: 60_000,
});

const nodeSettling = new Set(["pending", "enrolled"]);

export const nodesQuery = queryOptions({
  queryKey: keys.nodes,
  queryFn: ({ signal }) => request<Node[]>("GET", "/api/nodes", undefined, signal),
  // Registration happens out of band on the worker, so watch nodes that are mid-enrollment.
  refetchInterval: (query) => (query.state.data?.some((node) => nodeSettling.has(node.state)) ? 5_000 : 60_000),
});

export const workloadsQuery = queryOptions({
  queryKey: keys.workloads,
  queryFn: ({ signal }) => request<WorkloadSummary[]>("GET", "/api/workloads", undefined, signal),
});

export const workloadQuery = (id: string) =>
  queryOptions({
    queryKey: keys.workload(id),
    queryFn: ({ signal }) => request<Workload>("GET", `/api/workloads/${encodeURIComponent(id)}`, undefined, signal),
  });

export const operationsQuery = queryOptions({
  queryKey: keys.operations,
  queryFn: ({ signal }) => request<Operation[]>("GET", "/api/operations", undefined, signal),
  // The dispatcher advances operations every few seconds; idle lists refresh slowly.
  refetchInterval: (query) => (query.state.data?.some((operation) => isActive(operation.state)) ? 2_500 : 30_000),
});

export const operationLogsQuery = (id: string, active: boolean) =>
  queryOptions({
    queryKey: keys.operationLogs(id),
    queryFn: ({ signal }) => request<OperationLogs>("GET", `/api/operations/${encodeURIComponent(id)}/logs`, undefined, signal),
    refetchInterval: active ? 3_000 : false,
    staleTime: active ? 0 : 30_000,
  });

export const operationQuery = (id: string) => queryOptions({
  queryKey: [...keys.operations, "detail", id],
  queryFn: ({ signal }) => request<Operation>("GET", `/api/operations/${encodeURIComponent(id)}`, undefined, signal),
  refetchInterval: (query) => query.state.data && isActive(query.state.data.state) ? 2_500 : false,
});

export const workloadOperationsQuery = (id: string) => queryOptions({
  queryKey: [...keys.operations, "workload", id],
  queryFn: ({ signal }) => request<Operation[]>("GET", `/api/operations?workload_id=${encodeURIComponent(id)}`, undefined, signal),
  refetchInterval: (query) => query.state.data?.some((operation) => isActive(operation.state)) ? 2_500 : 30_000,
});

// Runtime state is read live from Docker on the worker. Failed reads are never retried
// silently in the background so stale container state is not mistaken for current health.
export const workloadRuntimeQuery = (id: string, operationActive: boolean) =>
  queryOptions({
    queryKey: keys.workloadRuntime(id),
    queryFn: ({ signal }) => request<WorkloadRuntime>("GET", `/api/workloads/${encodeURIComponent(id)}/runtime`, undefined, signal),
    refetchInterval: operationActive ? 3_000 : 15_000,
    staleTime: 0,
    retry: false,
  });

export const workloadLogsQuery = (id: string) =>
  queryOptions({
    queryKey: keys.workloadLogs(id),
    queryFn: ({ signal }) => request<OperationLogs>("GET", `/api/workloads/${encodeURIComponent(id)}/logs`, undefined, signal),
    refetchInterval: 5_000,
    staleTime: 0,
    gcTime: 0,
    retry: false,
  });

export const workloadRegistryQuery = (id: string) =>
  queryOptions({
    queryKey: ["resource-registry", id] as const,
    queryFn: ({ signal }) => request<{ registry_id: string }>("GET", `/api/workloads/${id}/registry`, undefined, signal),
  });

// Cached results only; the API never contacts a registry on read. The key includes the newest finished
// check, deploy or rollback so a completed check or a new deployment refreshes the result.
export const workloadImagesQuery = (id: string, refresh: string | null) =>
  queryOptions({
    queryKey: [...keys.workloadImages(id), refresh] as const,
    queryFn: ({ signal }) => request<WorkloadImages>("GET", `/api/workloads/${encodeURIComponent(id)}/images`, undefined, signal),
  });

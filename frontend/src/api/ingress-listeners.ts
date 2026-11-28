import { queryOptions, useMutation, useQueryClient } from "@tanstack/react-query";
import { request } from "./client";
import { keys } from "./queries";

export interface ListenerBinding {
  address: string;
  port: number;
}

export interface PublicBindings {
  http: ListenerBinding[];
  https: ListenerBinding[];
}

export type ListenerProtocol = "https" | "tcp" | "udp";
export type ListenerVisibility = "public" | "lan" | "tailscale";

export interface Listener {
  id: string;
  protocol: ListenerProtocol;
  visibility: ListenerVisibility;
  bindings: ListenerBinding[];
}

export interface ListenerSettings {
  // Omitted keeps the proxy's current public Docker publications exactly as they are.
  public?: PublicBindings;
  http3: boolean;
  listeners: Listener[];
}

export interface ListenerChange {
  id: string;
  phase: string;
  committed: boolean;
  started_at: string;
  finished_at?: string;
}

export interface ListenerStatus {
  settings: ListenerSettings;
  ports: Record<string, number> | null;
  change?: ListenerChange;
  proxy_present: boolean;
  running: boolean;
  // The proxy's actual public publications. Absent when the proxy is missing or the reply came from an apply.
  public_bindings?: PublicBindings;
}

export interface ListenerPlan {
  affected_workloads: string[] | null;
  change_id: string;
  revision: string;
  digest: string;
  settings: ListenerSettings;
  public_bindings: PublicBindings;
  ports: Record<string, number> | null;
  requires_restart: boolean;
  warnings: string[] | null;
}

const base = (nodeId: string) => `/api/nodes/${encodeURIComponent(nodeId)}/ingress-listeners`;

export const listenerKeys = {
  status: (nodeId: string) => ["ingress-listeners", nodeId] as const,
};

export const activePhases = new Set(["validating", "applying", "restoring", "cleanup"]);

export function changeActive(change: ListenerChange | undefined) {
  return change !== undefined && activePhases.has(change.phase) && !change.finished_at;
}

// Go encodes empty lists as null; normalise them once so the UI can rely on arrays.
const bindingsOf = (value?: Partial<PublicBindings> | null): PublicBindings => ({ http: value?.http ?? [], https: value?.https ?? [] });

function normalSettings(settings: ListenerSettings): ListenerSettings {
  return {
    ...(settings.public ? { public: bindingsOf(settings.public) } : {}),
    http3: settings.http3,
    listeners: (settings.listeners ?? []).map((listener) => ({ ...listener, bindings: listener.bindings ?? [] })),
  };
}

function normalStatus(status: ListenerStatus): ListenerStatus {
  return { ...status, settings: normalSettings(status.settings), ...(status.public_bindings ? { public_bindings: bindingsOf(status.public_bindings) } : {}) };
}

export const listenerStatusQuery = (nodeId: string) =>
  queryOptions({
    queryKey: listenerKeys.status(nodeId),
    queryFn: async ({ signal }) => normalStatus(await request<ListenerStatus>("GET", base(nodeId), undefined, signal)),
    // The worker finishes or restores an applied change on its own; follow it until it settles.
    refetchInterval: (query) => (changeActive(query.state.data?.change) ? 3_000 : false),
    retry: false,
  });

export const routeListenersKey = (workloadId: string) => [...keys.workload(workloadId), "route-listeners"] as const;

// The saved managed listeners of the resource's node, readable by anyone who can view the resource. Listeners are
// configured by instance administrators on the node's Proxy page; this never changes them.
export const routeListenersQuery = (workloadId: string) =>
  queryOptions({
    queryKey: routeListenersKey(workloadId),
    queryFn: async ({ signal }) => {
      const reply = await request<{ listeners: Listener[] | null }>("GET", `/api/workloads/${encodeURIComponent(workloadId)}/route-listeners`, undefined, signal);
      return (reply.listeners ?? []).map((listener) => ({ ...listener, bindings: listener.bindings ?? [] }));
    },
    retry: false,
  });

// Status is reloaded after every outcome, because a lost or failed reply can hide work the worker still did. A plan
// also reloads it, so a refusal caused by a change in progress or a stale proxy shows the current state.
export function useInitializeListeners(nodeId: string) {
  const client = useQueryClient();
  return useMutation({
    mutationFn: () => request<ListenerStatus>("POST", `${base(nodeId)}/initialize`),
    onSettled: () => client.invalidateQueries({ queryKey: listenerKeys.status(nodeId), exact: true }),
  });
}

export function usePlanListeners(nodeId: string) {
  const client = useQueryClient();
  return useMutation({
    mutationFn: async (settings: ListenerSettings) => {
      const plan = await request<ListenerPlan>("POST", `${base(nodeId)}/plan`, settings);
      return { ...plan, settings: normalSettings(plan.settings), public_bindings: bindingsOf(plan.public_bindings) };
    },
    onSettled: () => client.invalidateQueries({ queryKey: listenerKeys.status(nodeId), exact: true }),
  });
}

export function useApplyListeners(nodeId: string) {
  const client = useQueryClient();
  return useMutation({
    // Sends exactly the settings that were planned, with the reviewed revision and digest.
    mutationFn: ({ settings, plan }: { settings: ListenerSettings; plan: ListenerPlan }) => request<ListenerStatus>("PUT", base(nodeId), { settings, revision: plan.revision, digest: plan.digest, accept_interruption: true }),
    onSettled: () => client.invalidateQueries({ queryKey: listenerKeys.status(nodeId), exact: true }),
  });
}

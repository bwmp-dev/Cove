import { queryOptions, useMutation, useQueryClient, type QueryClient } from "@tanstack/react-query";
import { request } from "./client";
import { keys } from "./queries";

export type ProviderKind = "hetzner" | "digitalocean" | "vultr";

// Saved provider tokens are write-only: the API never returns them.
export interface NodeProvider {
  id: string;
  kind: ProviderKind;
  name: string;
  has_token: boolean;
}

export interface NodeProviderInput {
  kind: ProviderKind;
  name: string;
  // Empty on update keeps the saved token.
  token: string;
}

export interface CatalogOption {
  id: string;
  name: string;
  architecture?: string;
  cpus?: number;
  memory_mb?: number;
  // Provider's monthly estimate as a decimal string; absent means unknown, not free.
  monthly?: string;
  currency?: string;
  regions?: string[];
}

export interface ProviderCatalog {
  regions: CatalogOption[];
  plans: CatalogOption[];
  images: CatalogOption[];
  limited: boolean;
  checked_at: string;
}

export type ProvisionPhase = "queued" | "creating" | "server_pending" | "enrolling" | "succeeded" | "failed" | "uncertain" | "cancelled";

export interface Provision {
  id: string;
  provider_id: string | null;
  provider_kind: ProviderKind;
  provider_name: string;
  node_id: string | null;
  server_id: string;
  server_name: string;
  phase: ProvisionPhase;
  last_error: string;
  created_at: string;
  updated_at: string;
  deadline: string;
}

export interface ProvisionInput {
  provider_id: string;
  name: string;
  region: string;
  plan: string;
  image: string;
  cost?: { monthly_minor: number; currency: string };
}

export interface NodeCloudOrigin {
  provider_id: string | null;
  // Empty for a node that was added manually.
  kind: ProviderKind | "";
  server_id: string;
  server_name: string;
  checked_at: string;
}

export const maxProviders = 16;
export const maxUnresolvedProvisions = 4;

// Phases Cove is still working through on its own and that change without anyone acting.
const activePhases: ReadonlySet<ProvisionPhase> = new Set(["queued", "creating", "server_pending", "enrolling"]);
// Phases that keep a job open: active work plus an outcome that needs reconciling.
const unresolvedPhases: ReadonlySet<ProvisionPhase> = new Set([...activePhases, "uncertain"]);

export const isActivePhase = (phase: ProvisionPhase) => activePhases.has(phase);
export const isUnresolved = (phase: ProvisionPhase) => unresolvedPhases.has(phase);

export const providersKey = ["node-providers"] as const;
const catalogKey = (providerId: string) => ["node-providers", providerId, "catalog"] as const;
const provisionsKey = ["node-provisions"] as const;
const cloudKey = (nodeId: string) => ["nodes", nodeId, "cloud"] as const;
const providerPath = (id: string) => `/api/node-providers/${encodeURIComponent(id)}`;
const provisionPath = (id: string) => `/api/node-provisions/${encodeURIComponent(id)}`;

export const nodeProvidersQuery = queryOptions({
  queryKey: providersKey,
  queryFn: ({ signal }) => request<NodeProvider[]>("GET", "/api/node-providers", undefined, signal),
});

// The catalogue calls the provider's API, so it is only fetched when someone asks for it.
export const providerCatalogQuery = (providerId: string) =>
  queryOptions({
    queryKey: catalogKey(providerId),
    queryFn: ({ signal }) => request<ProviderCatalog>("GET", `${providerPath(providerId)}/catalog`, undefined, signal),
    enabled: false,
    staleTime: Infinity,
    retry: false,
  });

export function hasActive(provisions: Provision[] | undefined) {
  return provisions?.some((job) => isActivePhase(job.phase)) ?? false;
}

// A job reaching any new phase can add, enroll or release a pending node, so the node list is refreshed
// when a poll observes a change. Only the list itself is invalidated: the "nodes" prefix also covers
// per-node queries, and broad invalidation from inside a poll would refetch them on every tick.
async function fetchProvisions(client: QueryClient, signal: AbortSignal) {
  const previous = client.getQueryData<Provision[]>(provisionsKey);
  const provisions = await request<Provision[]>("GET", "/api/node-provisions", undefined, signal);
  if (previous) {
    const before = new Map(previous.map((job) => [job.id, job.phase]));
    if (provisions.some((job) => before.has(job.id) && before.get(job.id) !== job.phase)) void client.invalidateQueries({ queryKey: keys.nodes, exact: true });
  }
  return provisions;
}

export const provisionsQuery = (client: QueryClient) =>
  queryOptions({
    queryKey: provisionsKey,
    queryFn: ({ signal }) => fetchProvisions(client, signal),
    refetchInterval: (query) => (hasActive(query.state.data) ? 5_000 : false),
  });

export const nodeCloudQuery = (nodeId: string) =>
  queryOptions({
    queryKey: cloudKey(nodeId),
    queryFn: ({ signal }) => request<NodeCloudOrigin>("GET", `/api/nodes/${encodeURIComponent(nodeId)}/cloud`, undefined, signal),
  });

// Token mutations are dropped from the mutation cache as soon as nothing observes them, so the
// submitted token does not linger in memory after the form closes.
export function useSaveProvider(id: string | undefined) {
  const client = useQueryClient();
  return useMutation({
    gcTime: 0,
    mutationFn: (input: NodeProviderInput) => request<{ id: string }>(id ? "PUT" : "POST", id ? providerPath(id) : "/api/node-providers", input),
    onSettled: () => {
      if (id) client.removeQueries({ queryKey: catalogKey(id) });
      return client.invalidateQueries({ queryKey: providersKey, exact: true });
    },
  });
}

export function useDeleteProvider() {
  const client = useQueryClient();
  return useMutation({
    mutationFn: (id: string) => request<{ ok: boolean }>("DELETE", providerPath(id)),
    onSettled: (_data, _error, id) => {
      client.removeQueries({ queryKey: catalogKey(id) });
      return client.invalidateQueries({ queryKey: providersKey, exact: true });
    },
  });
}

// A lost response does not say whether the job was queued, so the history is always read again
// instead of inferring the outcome; nothing here retries automatically.
export function useCreateProvision() {
  const client = useQueryClient();
  return useMutation({
    mutationFn: (input: ProvisionInput) => request<{ id: string }>("POST", "/api/node-provisions", { ...input, confirm: true }),
    onSettled: () => Promise.all([client.invalidateQueries({ queryKey: provisionsKey }), client.invalidateQueries({ queryKey: keys.nodes, exact: true })]),
  });
}

export function useReconcileProvision() {
  const client = useQueryClient();
  return useMutation({
    mutationFn: ({ id, serverId }: { id: string; serverId?: string }) => request<{ id: string }>("POST", `${provisionPath(id)}/reconcile`, serverId ? { confirm: true, server_id: serverId } : { confirm: true }),
    onSettled: () => client.invalidateQueries({ queryKey: provisionsKey }),
  });
}

export function useAbandonProvision() {
  const client = useQueryClient();
  return useMutation({
    mutationFn: (id: string) => request<{ ok: boolean }>("DELETE", provisionPath(id), { confirm: true }),
    onSettled: () => Promise.all([client.invalidateQueries({ queryKey: provisionsKey }), client.invalidateQueries({ queryKey: keys.nodes, exact: true })]),
  });
}

import { queryOptions, useMutation, useQueryClient, type QueryClient } from "@tanstack/react-query";
import { request } from "./client";
import { keys } from "./queries";

export interface BastionInput {
  name: string;
  address: string;
  port: number;
  user: string;
  host_key: string;
}

// The panel generates and seals the bastion's private identity; only public values are returned.
export interface Bastion extends BastionInput {
  id: string;
  public_key: string;
  fingerprint: string;
  targets: string[];
  limited: boolean;
}

export interface NodeBastion {
  bastion_id: string;
  host_key_error: string;
}

export type IdentityState = "idle" | "queued" | "staged" | "switched" | "aborting" | "succeeded" | "cancelled";
export type IdentityAction = "rotate" | "retry" | "cancel";

export interface NodeIdentity {
  id: string;
  state: IdentityState;
  error: string;
  updated_at: string | null;
}

export const maxBastions = 32;

const bastionsKey = ["bastions"] as const;
const nodePath = (nodeId: string, suffix: string) => `/api/nodes/${encodeURIComponent(nodeId)}/${suffix}`;
const nodeBastionKey = (nodeId: string) => ["nodes", nodeId, "bastion"] as const;
const identityKey = (nodeId: string) => ["nodes", nodeId, "identity"] as const;

export const pendingIdentityStates: readonly IdentityState[] = ["queued", "staged", "switched", "aborting"];

export function identityPending(state: IdentityState | undefined) {
  return state !== undefined && pendingIdentityStates.includes(state);
}

export const bastionsQuery = queryOptions({
  queryKey: bastionsKey,
  queryFn: ({ signal }) => request<Bastion[]>("GET", "/api/bastions", undefined, signal),
});

export const nodeBastionQuery = (nodeId: string) =>
  queryOptions({
    queryKey: nodeBastionKey(nodeId),
    queryFn: ({ signal }) => request<NodeBastion>("GET", nodePath(nodeId, "bastion"), undefined, signal),
  });

// A rotation that reaches a terminal state changes the node's saved identity and readiness, so the
// node list is refreshed when a poll observes that transition. Only the list itself is invalidated: the
// "nodes" prefix also covers this identity query, and refetching it from inside its own fetch would loop.
async function fetchIdentity(client: QueryClient, nodeId: string, signal: AbortSignal) {
  const previous = client.getQueryData<NodeIdentity>(identityKey(nodeId));
  const identity = await request<NodeIdentity>("GET", nodePath(nodeId, "identity"), undefined, signal);
  if (identityPending(previous?.state) && !identityPending(identity.state)) void client.invalidateQueries({ queryKey: keys.nodes, exact: true });
  return identity;
}

export const nodeIdentityQuery = (client: QueryClient, nodeId: string) =>
  queryOptions({
    queryKey: identityKey(nodeId),
    queryFn: ({ signal }) => fetchIdentity(client, nodeId, signal),
    refetchInterval: (query) => (identityPending(query.state.data?.state) ? 5_000 : false),
  });

export function useCreateBastion() {
  const client = useQueryClient();
  return useMutation({
    mutationFn: (input: BastionInput) => request<Bastion>("POST", "/api/bastions", input),
    onSettled: () => client.invalidateQueries({ queryKey: bastionsKey }),
  });
}

export function useDeleteBastion() {
  const client = useQueryClient();
  return useMutation({
    mutationFn: (id: string) => request<{ ok: boolean }>("DELETE", `/api/bastions/${encodeURIComponent(id)}`),
    onSettled: () => client.invalidateQueries({ queryKey: bastionsKey }),
  });
}

export function useSaveNodeBastion(nodeId: string) {
  const client = useQueryClient();
  return useMutation({
    mutationFn: (bastionId: string) => request<NodeBastion>("PUT", nodePath(nodeId, "bastion"), { bastion_id: bastionId }),
    onSuccess: (saved) => client.setQueryData(nodeBastionKey(nodeId), saved),
    onSettled: () => Promise.all([client.invalidateQueries({ queryKey: nodeBastionKey(nodeId) }), client.invalidateQueries({ queryKey: bastionsKey }), client.invalidateQueries({ queryKey: keys.nodes, exact: true })]),
  });
}

// Ambiguous failures (dropped connections, 5xx) do not say whether the intent was saved, so the
// status is always read again instead of being inferred from the response.
export function useIdentityAction(nodeId: string) {
  const client = useQueryClient();
  return useMutation({
    mutationFn: (action: IdentityAction) => request<{ id: string; state: IdentityState }>("POST", nodePath(nodeId, "identity"), { action, confirm: true }),
    onSettled: () => Promise.all([client.invalidateQueries({ queryKey: identityKey(nodeId) }), client.invalidateQueries({ queryKey: keys.nodes, exact: true })]),
  });
}

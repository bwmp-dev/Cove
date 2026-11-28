import { queryOptions, useMutation, useQueryClient } from "@tanstack/react-query";
import { request } from "./client";

export interface TunnelConfiguration {
  zone_id: string;
  hostnames: string[];
  image: string;
}

export interface TunnelWorkerStatus {
  id?: string;
  tunnel_id?: string;
  image: string;
  hostnames: string[];
  phase: string;
  running: boolean;
  connected: boolean;
  last_error?: string;
  // Hostname to the reason the worker does not publish it.
  withheld: Record<string, string> | null;
}

export interface TunnelZone {
  id: string;
  domain: string;
  connection_kind: "token" | "oauth";
}

export interface TunnelDNSRecord {
  hostname: string;
  phase: string;
  cleanup_requested: boolean;
  last_error: string;
}

// Metadata only: the API never returns the token, tunnel secret, private key, CSR or DNS proof.
export interface TunnelStatus {
  configuration: TunnelConfiguration | null;
  phase: string;
  cleanup_requested: boolean;
  last_error: string;
  next_check_at: string | null;
  worker: TunnelWorkerStatus | null;
  worker_error: string | null;
  zones: TunnelZone[];
  available_hostnames: string[];
  dns: TunnelDNSRecord[];
  default_image: string;
}

const base = (nodeId: string) => `/api/nodes/${encodeURIComponent(nodeId)}/cloudflare-tunnel`;

export const tunnelKeys = {
  status: (nodeId: string) => ["cloudflare-tunnel", nodeId] as const,
};

type Nullable<T, K extends keyof T> = Omit<T, K> & { [P in K]: T[P] | null };

// The reply as sent: Go encodes empty lists as null.
export type TunnelStatusReply = Nullable<Omit<TunnelStatus, "configuration" | "worker">, "zones" | "available_hostnames" | "dns"> & {
  configuration: Nullable<TunnelConfiguration, "hostnames"> | null;
  worker: Nullable<TunnelWorkerStatus, "hostnames"> | null;
};

// Normalise null lists once so the UI can rely on arrays.
export function normalTunnelStatus(status: TunnelStatusReply): TunnelStatus {
  return {
    ...status,
    configuration: status.configuration ? { ...status.configuration, hostnames: status.configuration.hostnames ?? [] } : null,
    worker: status.worker ? { ...status.worker, hostnames: status.worker.hostnames ?? [], withheld: status.worker.withheld ?? null } : null,
    worker_error: status.worker_error || null,
    zones: status.zones ?? [],
    available_hostnames: status.available_hostnames ?? [],
    dns: status.dns ?? [],
  };
}

const busyPhases = new Set(["pending", "provisioning", "confirming", "connecting", "publishing", "disabling", "cleanup"]);
const busyDNSPhases = new Set(["pending", "presenting", "cleanup"]);

// Work in progress is followed closely; settled or blocked states are rechecked slowly.
export function tunnelPollInterval(status?: TunnelStatus) {
  if (!status) return 30_000;
  const busy = busyPhases.has(status.phase) || status.cleanup_requested || status.dns.some((record) => record.cleanup_requested || busyDNSPhases.has(record.phase));
  return busy ? 3_000 : 30_000;
}

export const tunnelStatusQuery = (nodeId: string) =>
  queryOptions({
    queryKey: tunnelKeys.status(nodeId),
    queryFn: async ({ signal }) => normalTunnelStatus(await request<TunnelStatusReply>("GET", base(nodeId), undefined, signal)),
    refetchInterval: (query) => (query.state.status === "error" ? false : tunnelPollInterval(query.state.data)),
    retry: false,
  });

export interface TunnelInput {
  zone_id: string;
  hostnames: string[];
  image: string;
}

// Every request is accepted asynchronously, so status is always read again rather than trusting the reply as an outcome.
function useTunnelMutation<T>(nodeId: string, send: (input: T) => Promise<TunnelStatusReply>) {
  const client = useQueryClient();
  return useMutation({
    mutationFn: async (input: T) => normalTunnelStatus(await send(input)),
    onSuccess: async (status) => {
      // A poll already in flight would resolve with the state from before the request and overwrite it.
      await client.cancelQueries({ queryKey: tunnelKeys.status(nodeId), exact: true });
      client.setQueryData(tunnelKeys.status(nodeId), status);
    },
    onSettled: () => client.invalidateQueries({ queryKey: tunnelKeys.status(nodeId), exact: true }),
  });
}

export function useSaveTunnel(nodeId: string) {
  return useTunnelMutation(nodeId, (input: TunnelInput) => request<TunnelStatusReply>("PUT", base(nodeId), input));
}

export function useRetryTunnel(nodeId: string) {
  return useTunnelMutation(nodeId, () => request<TunnelStatusReply>("POST", `${base(nodeId)}/retry`));
}

export function useDisableTunnel(nodeId: string) {
  return useTunnelMutation(nodeId, () => request<TunnelStatusReply>("DELETE", base(nodeId)));
}

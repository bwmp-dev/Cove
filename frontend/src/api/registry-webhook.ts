import { queryOptions, useMutation, useQueryClient } from "@tanstack/react-query";
import { request } from "./client";
import type { OperationState } from "./types";

export type RegistryWebhookProvider = "distribution" | "docker_hub" | "ghcr";

export interface RegistryWebhookFilter {
  repository: string;
  // An exact tag, or "*" for every tag of the repository.
  tag: string;
}

export interface RegistryWebhookConfiguration {
  provider: RegistryWebhookProvider;
  enabled: boolean;
  filters: RegistryWebhookFilter[];
  registry_id: string;
  registry_server: string;
}

export interface RegistryWebhookStatus {
  configuration: RegistryWebhookConfiguration | null;
  endpoint: string | null;
  registry: { id: string; name: string; server: string } | null;
  matches_saved_registry: boolean;
  worker_supported: boolean;
  last_received_at: string | null;
  last_matched_at: string | null;
  // A matching push is waiting to be deployed.
  pending_at: string | null;
  last_operation: { id: string; state: OperationState; created_at: string } | null;
  last_error: string;
  last_ignored_reason: string;
}

// Create, rotation and a save that changes the provider or registry return the token once. It never reaches the query cache.
interface RegistryWebhookReply extends RegistryWebhookStatus {
  token?: string;
}

export interface RegistryWebhookInput {
  provider: RegistryWebhookProvider;
  enabled: boolean;
  filters: RegistryWebhookFilter[];
}

const path = (workloadId: string) => `/api/workloads/${encodeURIComponent(workloadId)}/registry-webhook`;

export const registryWebhookKey = (workloadId: string) => ["registry-webhook", workloadId] as const;

// Copies only the documented metadata, so an unexpected field such as a token is never cached.
function metadata(status: RegistryWebhookStatus): RegistryWebhookStatus {
  return {
    configuration: status.configuration ? { ...status.configuration, filters: status.configuration.filters ?? [] } : null,
    endpoint: status.endpoint,
    registry: status.registry,
    matches_saved_registry: status.matches_saved_registry,
    worker_supported: status.worker_supported,
    last_received_at: status.last_received_at,
    last_matched_at: status.last_matched_at,
    pending_at: status.pending_at,
    last_operation: status.last_operation,
    last_error: status.last_error ?? "",
    last_ignored_reason: status.last_ignored_reason ?? "",
  };
}

// Pushes arrive from outside the panel, so an idle page still rechecks; a pending push or its unfinished deploy is
// followed closely until the deploy settles.
export function registryWebhookPollInterval(status: RegistryWebhookStatus | undefined) {
  const state = status?.last_operation?.state;
  return status?.pending_at || state === "queued" || state === "running" ? 3_000 : 30_000;
}

export const registryWebhookQuery = (workloadId: string) =>
  queryOptions({
    queryKey: registryWebhookKey(workloadId),
    queryFn: async ({ signal }) => metadata(await request<RegistryWebhookStatus>("GET", path(workloadId), undefined, signal)),
    refetchInterval: (query) => (query.state.status === "error" ? false : registryWebhookPollInterval(query.state.data)),
    retry: false,
  });

// The token is handed to the caller once and stripped before the status is cached. The mutation keeps no cached
// result (gcTime 0), and callers reset it as soon as they have taken the token.
function useWebhookMutation<T>(workloadId: string, send: (input: T) => Promise<RegistryWebhookReply>) {
  const client = useQueryClient();
  return useMutation({
    gcTime: 0,
    mutationFn: async (input: T) => {
      const reply = await send(input);
      return { status: metadata(reply), token: reply.token || null };
    },
    onSuccess: ({ status }) => client.setQueryData(registryWebhookKey(workloadId), status),
    onSettled: () => client.invalidateQueries({ queryKey: registryWebhookKey(workloadId), exact: true }),
  });
}

export function useSaveRegistryWebhook(workloadId: string) {
  return useWebhookMutation(workloadId, (input: RegistryWebhookInput) => request<RegistryWebhookReply>("PUT", path(workloadId), input));
}

export function useRotateRegistryWebhook(workloadId: string) {
  return useWebhookMutation(workloadId, () => request<RegistryWebhookReply>("POST", `${path(workloadId)}/rotate`));
}

export function useRemoveRegistryWebhook(workloadId: string) {
  const client = useQueryClient();
  return useMutation({
    gcTime: 0,
    mutationFn: () => request<{ ok: true }>("DELETE", path(workloadId)),
    onSettled: () => client.invalidateQueries({ queryKey: registryWebhookKey(workloadId), exact: true }),
  });
}

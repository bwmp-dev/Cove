import { queryOptions, useMutation, useQueryClient } from "@tanstack/react-query";
import { request } from "./client";

export interface CleanupPolicy {
  enabled: boolean;
  interval_hours: number;
  minimum_age_hours: number;
  images: boolean;
  build_cache: boolean;
  volumes: boolean;
  non_cove: boolean;
}

export interface CleanupState {
  policy: CleanupPolicy;
  last_run: string | null;
  last_error: string;
  removed: number;
  estimated_bytes: number;
}

export type CleanupItemKind = "image" | "volume" | "cache";

export interface CleanupItem {
  kind: CleanupItemKind;
  id: string;
  // Null means Docker did not report a size; it is unknown, not zero.
  bytes: number | null;
}

export interface CleanupPlan {
  items: CleanupItem[];
  estimated_bytes: number;
  unknown_sizes: boolean;
  skipped_cache: number;
  revision: string;
  sampled_at: string;
}

export interface CleanupEnvelope {
  state: CleanupState;
  plan: CleanupPlan | null;
}

export const cleanupLimits = {
  interval_hours: { min: 1, max: 168 },
  minimum_age_hours: { min: 24, max: 8760 },
} as const;

const path = (nodeId: string, suffix: string) => `/api/nodes/${encodeURIComponent(nodeId)}/${suffix}`;
const policyKey = (nodeId: string) => ["nodes", nodeId, "cleanup-policy"] as const;

export const cleanupPolicyQuery = (nodeId: string) =>
  queryOptions({
    queryKey: policyKey(nodeId),
    queryFn: ({ signal }) => request<CleanupEnvelope>("GET", path(nodeId, "cleanup-policy"), undefined, signal),
    select: (envelope) => envelope.state,
  });

export function useSaveCleanupPolicy(nodeId: string) {
  const client = useQueryClient();
  return useMutation({
    mutationFn: (policy: CleanupPolicy) => request<CleanupEnvelope>("PUT", path(nodeId, "cleanup-policy"), policy),
    onSuccess: (envelope) => client.setQueryData(policyKey(nodeId), envelope),
  });
}

export function usePreviewCleanup(nodeId: string) {
  const client = useQueryClient();
  return useMutation({
    mutationFn: () => request<CleanupEnvelope>("POST", path(nodeId, "cleanup-plan"), {}),
    onSuccess: (envelope) => client.setQueryData(policyKey(nodeId), envelope),
  });
}

export function useRunCleanup(nodeId: string) {
  const client = useQueryClient();
  return useMutation({
    mutationFn: (revision: string) => request<CleanupEnvelope>("POST", path(nodeId, "cleanup-run"), { revision }),
    onSuccess: (envelope) => client.setQueryData(policyKey(nodeId), envelope),
    // A failed run may have removed some items, so the saved state is read again. It is never retried.
    onError: () => client.invalidateQueries({ queryKey: policyKey(nodeId) }),
  });
}

import { queryOptions, useMutation, useQueryClient } from "@tanstack/react-query";
import { request } from "./client";
import { keys } from "./queries";

// Node-global for every team. Only a verified status means the hard cap is enforced; everything else is no cap or unknown.
export interface BuildCacheStatus {
  supported: boolean;
  can_configure: boolean;
  configured: boolean;
  verified: boolean;
  cleanup_pending: boolean;
  path: string;
  image: string;
  reason: string;
  active_operation_id: string;
  capacity_bytes: number;
  available_bytes: number;
  can_manage: boolean;
}

const booleans = ["supported", "can_configure", "configured", "verified", "cleanup_pending", "can_manage"] as const;
const strings = ["path", "image", "reason", "active_operation_id"] as const;
const numbers = ["capacity_bytes", "available_bytes"] as const;

// A reply with a missing or mistyped field cannot say whether a cap is enforced, so it is treated as a failed read
// rather than defaulted to "no cap" or "verified".
export function buildCacheStatus(data: unknown): BuildCacheStatus {
  const value = (typeof data === "object" && data !== null ? data : {}) as Record<string, unknown>;
  const complete =
    booleans.every((key) => typeof value[key] === "boolean") &&
    strings.every((key) => typeof value[key] === "string") &&
    numbers.every((key) => typeof value[key] === "number" && Number.isFinite(value[key]) && value[key] >= 0);
  if (!complete) throw new Error("The build-cache status was incomplete.");
  return value as unknown as BuildCacheStatus;
}

const path = (nodeId: string) => `/api/nodes/${encodeURIComponent(nodeId)}/build-cache`;

export const buildCacheKey = (nodeId: string) => [...keys.nodes, nodeId, "build-cache"] as const;

export const buildCacheQuery = (nodeId: string) =>
  queryOptions({
    queryKey: buildCacheKey(nodeId),
    queryFn: async ({ signal }) => buildCacheStatus(await request<unknown>("GET", path(nodeId), undefined, signal)),
    // Polling stops after a failed read until someone retries, so a failure is never replaced by a guess.
    refetchInterval: (query) => (query.state.status === "error" ? false : query.state.data?.cleanup_pending ? 3_000 : 30_000),
    retry: false,
  });

// An empty path disables the cap. Status, nodes and operations are reloaded after every outcome, because a lost
// reply can hide work the worker still did.
export function useConfigureBuildCache(nodeId: string) {
  const client = useQueryClient();
  return useMutation({
    mutationFn: async (cachePath: string) => buildCacheStatus(await request<unknown>("PUT", path(nodeId), { path: cachePath })),
    onSettled: () =>
      Promise.all([
        client.invalidateQueries({ queryKey: buildCacheKey(nodeId), exact: true }),
        client.invalidateQueries({ queryKey: keys.nodes, exact: true }),
        client.invalidateQueries({ queryKey: keys.operations }),
      ]),
  });
}

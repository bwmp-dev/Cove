import { queryOptions, useMutation, useQueryClient } from "@tanstack/react-query";
import { isUnresolvedPhase, type MovePhase } from "../lib/moves";
import { request } from "./client";
import { keys } from "./queries";

// The id is the move's operation id.
export interface WorkloadMove {
  id: string;
  source_node_id: string;
  destination_node_id: string;
  phase: MovePhase;
  last_error: string;
  updated_at: string;
}

// root, daemon and volume labels are only validation inputs for the control plane.
export interface MovePlan {
  root: string;
  daemon: string;
  architecture: string;
  volumes: { name: string; labels?: Record<string, string> }[];
  containers: number;
  running: number;
  max_bytes: number;
}

export interface MovePreview {
  source_node_id: string;
  destination_node_id: string;
  source: MovePlan;
  target: MovePlan;
  revision: string;
}

// Physical node moves are separate from the reviewed project and team move at /move.
const movePath = (workloadId: string) => `/api/workloads/${encodeURIComponent(workloadId)}/node-move`;

export const moveKey = (workloadId: string) => [...keys.workload(workloadId), "node-move"] as const;

export function workloadMoveQuery(workloadId: string) {
  return queryOptions({
    queryKey: moveKey(workloadId),
    queryFn: async ({ signal }) => (await request<{ move: WorkloadMove | null }>("GET", movePath(workloadId), undefined, signal)).move,
    refetchInterval: (query) => (query.state.data && isUnresolvedPhase(query.state.data.phase) ? 5_000 : false),
  });
}

export function usePreviewMove(workloadId: string) {
  return useMutation({
    gcTime: 0,
    mutationFn: (destination: string) => request<MovePreview>("POST", `${movePath(workloadId)}/preview`, { destination_node_id: destination }),
  });
}

export function useQueueMove(workloadId: string) {
  const client = useQueryClient();
  return useMutation({
    gcTime: 0,
    mutationFn: (preview: MovePreview) =>
      request<{ id: string }>("POST", movePath(workloadId), { destination_node_id: preview.destination_node_id, revision: preview.revision, confirm: true }),
    // Refreshes after failures too: a lost response may still have queued the move.
    onSettled: () => Promise.all([client.invalidateQueries({ queryKey: moveKey(workloadId) }), client.invalidateQueries({ queryKey: keys.operations })]),
  });
}

import { queryOptions, useMutation, useQueryClient } from "@tanstack/react-query";
import { request } from "./client";
import { keys } from "./queries";

// The server decides eligibility; the panel never derives it from cached node capabilities.
export interface BuildNodeChoice {
  id: string;
  name: string;
  eligible: boolean;
  reason: string;
}

export interface BuildNodeSettings {
  // Empty inherits the runtime node.
  node_id: string;
  runtime_node_id: string;
  effective_node_id: string;
  nodes: BuildNodeChoice[];
}

const path = (workloadId: string) => `/api/workloads/${encodeURIComponent(workloadId)}/build-node`;

export const buildNodeKey = (workloadId: string) => [...keys.workload(workloadId), "build-node"] as const;

// A response without a runtime or effective node cannot say where a build would run, so it is treated as a failed read.
function settings(data: BuildNodeSettings): BuildNodeSettings {
  if (typeof data.runtime_node_id !== "string" || data.runtime_node_id === "" || typeof data.effective_node_id !== "string" || data.effective_node_id === "") {
    throw new Error("The build node settings were incomplete.");
  }
  return {
    node_id: data.node_id ?? "",
    runtime_node_id: data.runtime_node_id,
    effective_node_id: data.effective_node_id,
    nodes: (data.nodes ?? []).map((node) => ({ id: node.id, name: node.name ?? "", eligible: node.eligible === true, reason: node.reason ?? "" })),
  };
}

export const buildNodeQuery = (workloadId: string) =>
  queryOptions({
    queryKey: buildNodeKey(workloadId),
    queryFn: async ({ signal }) => settings(await request<BuildNodeSettings>("GET", path(workloadId), undefined, signal)),
    refetchInterval: (query) => (query.state.status === "error" ? false : 30_000),
    retry: false,
  });

export function useSaveBuildNode(workloadId: string) {
  const client = useQueryClient();
  return useMutation({
    mutationFn: (nodeId: string) => request<{ ok: boolean }>("PUT", path(workloadId), { node_id: nodeId }),
    onSettled: () => Promise.all([client.invalidateQueries({ queryKey: buildNodeKey(workloadId), exact: true }), client.invalidateQueries({ queryKey: keys.operations })]),
  });
}

export function choiceName(settings: BuildNodeSettings | undefined, id: string) {
  return settings?.nodes.find((node) => node.id === id)?.name || id;
}

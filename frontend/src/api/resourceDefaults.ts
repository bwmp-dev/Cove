import { queryOptions, useMutation, useQueryClient } from "@tanstack/react-query";
import { request } from "./client";
import type { NodeState, ServiceLimits } from "./types";

export interface ResourceDefaults {
  configured: ServiceLimits;
  team: ServiceLimits;
  effective: ServiceLimits;
}

export type DefaultsScope = { scope: "team" } | { scope: "environment"; id: string };

function defaultsPath(target: DefaultsScope) {
  return target.scope === "team" ? "/api/resource-defaults" : `/api/environments/${target.id}/resource-defaults`;
}

export function resourceDefaultsKey(target: DefaultsScope) {
  return target.scope === "team" ? ["resource-defaults", "team"] : ["resource-defaults", "environment", target.id];
}

export function resourceDefaultsQuery(target: DefaultsScope) {
  return queryOptions({
    queryKey: resourceDefaultsKey(target),
    queryFn: ({ signal }) => request<ResourceDefaults>("GET", defaultsPath(target), undefined, signal),
  });
}

export function useSaveResourceDefaults(target: DefaultsScope) {
  const client = useQueryClient();
  return useMutation({
    mutationFn: (limits: ServiceLimits) => request<ResourceDefaults>("PUT", defaultsPath(target), limits),
    onSuccess: (saved) => {
      client.setQueryData(resourceDefaultsKey(target), saved);
      // Environment effective values depend on the team defaults.
      if (target.scope === "team") void client.invalidateQueries({ queryKey: ["resource-defaults", "environment"] });
    },
  });
}

export interface PlacementSuggestion {
  id: string;
  name: string;
  state: NodeState;
  sampled_at: string | null;
  available_cpus: number | null;
  available_memory_bytes: number | null;
  fits: boolean | null;
  notice: string;
}

export interface PlacementReport {
  requirements: ServiceLimits;
  nodes: PlacementSuggestion[];
  limited: boolean;
}

export interface PlacementRequest {
  projectId: string;
  environmentId: string;
  requirements: ServiceLimits;
}

export function fetchPlacementSuggestions({ projectId, environmentId, requirements }: PlacementRequest, signal?: AbortSignal) {
  const query = new URLSearchParams({ project_id: projectId });
  if (environmentId) query.set("environment_id", environmentId);
  if (requirements.cpus > 0) query.set("cpus", String(requirements.cpus));
  if (requirements.memory_mb > 0) query.set("memory_mb", String(requirements.memory_mb));
  return request<PlacementReport>("GET", `/api/placement-suggestions?${query}`, undefined, signal);
}

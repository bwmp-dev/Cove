import { queryOptions, useMutation, useQueryClient } from "@tanstack/react-query";
import { request } from "./client";
import { keys } from "./queries";
import type { EnvironmentSuspension, Permission } from "./types";

export interface Environment {
  id: string;
  name: string;
  // Set on environments that an environment preview created and manages.
  preview_id?: string;
  permissions: Permission[];
}

export interface Project {
  id: string;
  name: string;
  permissions: Permission[];
  environments: Environment[];
}

export const projectsQuery = queryOptions({
  queryKey: keys.projects,
  queryFn: ({ signal }) => request<Project[]>("GET", "/api/projects", undefined, signal),
});

export function useCreateProject() {
  const client = useQueryClient();
  return useMutation({
    mutationFn: (name: string) => request<{ id: string }>("POST", "/api/projects", { name }),
    onSettled: () => client.invalidateQueries({ queryKey: keys.projects }),
  });
}

export function useCreateEnvironment() {
  const client = useQueryClient();
  return useMutation({
    mutationFn: ({ projectId, name }: { projectId: string; name: string }) =>
      request<{ id: string }>("POST", `/api/projects/${encodeURIComponent(projectId)}/environments`, { name }),
    onSettled: () => client.invalidateQueries({ queryKey: keys.projects }),
  });
}

export function useDeleteProject() {
  const client = useQueryClient();
  return useMutation({
    mutationFn: (projectId: string) => request<{ ok: boolean }>("DELETE", `/api/projects/${encodeURIComponent(projectId)}`),
    onSettled: () => client.invalidateQueries({ queryKey: keys.projects }),
  });
}

export function useDeleteEnvironment() {
  const client = useQueryClient();
  return useMutation({
    mutationFn: (environmentId: string) => request<{ ok: boolean }>("DELETE", `/api/environments/${encodeURIComponent(environmentId)}`),
    onSettled: () => client.invalidateQueries({ queryKey: keys.projects }),
  });
}

export function useAssignEnvironment(workloadId: string) {
  const client = useQueryClient();
  return useMutation({
    mutationFn: (placement: { project_id: string; environment_id: string }) =>
      request<{ ok: boolean }>("PUT", `/api/workloads/${encodeURIComponent(workloadId)}/environment`, placement),
    onSettled: () => client.invalidateQueries({ queryKey: keys.workloads }),
  });
}

// Pause and resume progress is read from the durable queue and workers. Settling environments refresh at the operation
// cadence; settled and failed ones keep refreshing slowly so a later worker report is not missed.
export const environmentSuspensionQuery = (environmentId: string) =>
  queryOptions({
    queryKey: keys.environmentSuspension(environmentId),
    queryFn: ({ signal }) => request<EnvironmentSuspension>("GET", `/api/environments/${encodeURIComponent(environmentId)}/suspension`, undefined, signal),
    refetchInterval: (query) => (query.state.data?.phase === "pausing" || query.state.data?.phase === "resuming" ? 2_500 : 30_000),
    retry: 1,
  });

// A 202 means the requested state was saved and queued, not that any resource has paused or resumed yet.
export function useSetEnvironmentPaused(environmentId: string) {
  const client = useQueryClient();
  return useMutation({
    mutationFn: (paused: boolean) => request<EnvironmentSuspension>("PUT", `/api/environments/${encodeURIComponent(environmentId)}/suspension`, { paused }),
    onSuccess: (state) => client.setQueryData(keys.environmentSuspension(environmentId), state),
    onSettled: () =>
      Promise.all([
        client.invalidateQueries({ queryKey: keys.environmentSuspension(environmentId) }),
        client.invalidateQueries({ queryKey: keys.workloads }),
        client.invalidateQueries({ queryKey: keys.operations }),
      ]),
  });
}

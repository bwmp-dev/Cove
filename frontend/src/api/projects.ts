import { queryOptions, useMutation, useQueryClient } from "@tanstack/react-query";
import { request } from "./client";
import { keys } from "./queries";
import type { Permission } from "./types";

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

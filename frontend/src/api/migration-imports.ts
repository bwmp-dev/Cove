import { queryOptions } from "@tanstack/react-query";
import { request } from "./client";
import type { ConfigurationChange } from "./project-configuration";

export interface ImportSession {
  id: string;
  created_at: string;
  expires_at: string;
}

export interface ImportCandidate {
  id: string;
  name: string;
  kind: string;
  blockers: string[] | null;
}

// Discovery metadata only. The server keeps the Compose definitions and variable values sealed and never returns them.
export interface ImportDiscovery {
  id: string;
  candidates: ImportCandidate[] | null;
  warnings: string[] | null;
}

export interface ImportMapping {
  source_id: string;
  key: string;
  name: string;
  environment: string;
  node_id: string;
}

export interface ImportDryRun {
  draft_id: string;
  base_version: string;
  changes: ConfigurationChange[] | null;
  warnings: string[] | null;
}

// Native Coolify API responses, passed through as exported. They can hold real secrets.
export interface CoolifyExportItem {
  kind: "application" | "service";
  resource: Record<string, unknown>;
  environment_variables?: unknown[];
}

const base = (project: string) => `/api/projects/${encodeURIComponent(project)}/imports`;

export const importKeys = {
  all: (project: string) => ["projects", project, "imports"] as const,
  detail: (project: string, id: string) => ["projects", project, "imports", id] as const,
};

export const importSessionsQuery = (project: string) =>
  queryOptions({
    queryKey: importKeys.all(project),
    queryFn: ({ signal }) => request<ImportSession[] | null>("GET", base(project), undefined, signal),
  });

export const importDiscoveryQuery = (project: string, id: string) =>
  queryOptions({
    queryKey: importKeys.detail(project, id),
    queryFn: ({ signal }) => request<ImportDiscovery>("GET", `${base(project)}/${encodeURIComponent(id)}`, undefined, signal),
    // A discovery never changes after it is created; expiry surfaces as a 404 on the next request.
    staleTime: Infinity,
    retry: false,
  });

export function discoverCoolify(project: string, items: CoolifyExportItem[]) {
  return request<ImportDiscovery>("POST", `${base(project)}/coolify`, { items });
}

export function deleteImport(project: string, id: string) {
  return request<{ ok: boolean }>("DELETE", `${base(project)}/${encodeURIComponent(id)}`);
}

export function dryRunImport(project: string, id: string, mappings: ImportMapping[]) {
  return request<ImportDryRun>("POST", `${base(project)}/${encodeURIComponent(id)}/dry-run`, { mappings });
}

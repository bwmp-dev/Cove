import { queryOptions } from "@tanstack/react-query";
import { request, requestFile } from "./client";

// Before and after are JSON-encoded strings chosen by the server; they never contain variable values, file contents or credentials.
export interface FieldDiff {
  field: string;
  before: string;
  after: string;
}

export interface ConfigurationChange {
  resource: string;
  action: "create" | "update";
  fields: string[] | null;
  diffs: FieldDiff[] | null;
}

export interface ProjectConfigurationManifest {
  api_version: string;
  project: string;
  resources?: { key: string; environment?: string; configuration: { name: string; source?: unknown } & Record<string, unknown> }[] | null;
  environments?: { name: string }[] | null;
  [field: string]: unknown;
}

export interface ProjectConfiguration {
  version: string;
  manifest: ProjectConfigurationManifest;
  // Live workload ID for each manifest resource key; kept outside the portable manifest.
  resource_ids: Record<string, string> | null;
}

export interface ConfigurationDraft {
  id: string;
  base_version: string;
  changes: ConfigurationChange[] | null;
  created_at: string;
  applied_at: string | null;
  operation_ids: string[] | null;
}

export interface SavedDraft {
  id: string;
  base_version: string;
  changes: ConfigurationChange[] | null;
}

export interface AppliedDraft {
  id: string;
  operation_ids: string[] | null;
  state: "saved";
}

export interface GitOpsSource {
  workload_id: string;
  path: string;
  automatic: boolean;
}

export type GitOpsStatus =
  | { connected: false }
  | { connected: true; source: GitOpsSource; last_revision: string; last_error: string; draft_id: string; synced_at: string | null; drift: ConfigurationChange[] | null };

export interface GitOpsSync {
  state: "unchanged" | "pending" | "saved";
  draft_id: string;
  changes: ConfigurationChange[] | null;
  revision: string;
}

export interface EnvironmentComparison {
  from: string;
  to: string;
  resources: { name: string; state: "equal" | "different" | "only_from" | "only_to"; fields: string[] | null; diffs: FieldDiff[] | null; from_revision: string; to_revision: string }[] | null;
  variable_changes: string[] | null;
  policy_changed: boolean;
}

export interface WorkloadConfigurationDiff {
  has_baseline: boolean;
  operation_id: string;
  revision: string;
  fields: string[] | null;
  diffs: FieldDiff[] | null;
}

export interface DriftChange {
  container: string;
  service: string;
  state: "changed" | "added" | "missing";
  fields: string[] | null;
}

export interface WorkloadDrift {
  baseline: boolean;
  busy: boolean;
  // Set with busy when the resource is paused with its environment or asleep, so the runtime was not inspected.
  deferred?: "suspended";
  operation: string;
  captured_at: string | null;
  checked_at: string;
  changes: DriftChange[] | null;
}

const base = (project: string) => `/api/projects/${encodeURIComponent(project)}/configuration`;

export const configurationKeys = {
  all: (project: string) => ["projects", project, "configuration"] as const,
  current: (project: string) => ["projects", project, "configuration", "current"] as const,
  drafts: (project: string) => ["projects", project, "configuration", "drafts"] as const,
  gitops: (project: string) => ["projects", project, "configuration", "gitops"] as const,
  compare: (project: string, from: string, to: string) => ["projects", project, "configuration", "compare", from, to] as const,
};

export const projectConfigurationQuery = (project: string) =>
  queryOptions({
    queryKey: configurationKeys.current(project),
    queryFn: ({ signal }) => request<ProjectConfiguration>("GET", base(project), undefined, signal),
  });

export const configurationDraftsQuery = (project: string) =>
  queryOptions({
    queryKey: configurationKeys.drafts(project),
    queryFn: ({ signal }) => request<ConfigurationDraft[] | null>("GET", `${base(project)}/drafts`, undefined, signal),
  });

export const gitOpsQuery = (project: string) =>
  queryOptions({
    queryKey: configurationKeys.gitops(project),
    queryFn: ({ signal }) => request<GitOpsStatus>("GET", `${base(project)}/gitops`, undefined, signal),
  });

export const environmentComparisonQuery = (project: string, from: string, to: string) =>
  queryOptions({
    queryKey: configurationKeys.compare(project, from, to),
    queryFn: ({ signal }) => request<EnvironmentComparison>("GET", `${base(project)}/compare?from=${encodeURIComponent(from)}&to=${encodeURIComponent(to)}`, undefined, signal),
    staleTime: 0,
    retry: false,
  });

export function saveConfigurationDraft(project: string, content: string) {
  return request<SavedDraft>("POST", `${base(project)}/drafts`, { content });
}

export function applyConfigurationDraft(project: string, draft: string) {
  return request<AppliedDraft>("POST", `${base(project)}/drafts/${encodeURIComponent(draft)}/apply`);
}

export function deleteConfigurationDraft(project: string, draft: string) {
  return request<{ ok: boolean }>("DELETE", `${base(project)}/drafts/${encodeURIComponent(draft)}`);
}

export function saveGitOps(project: string, source: GitOpsSource) {
  return request<GitOpsStatus>("PUT", `${base(project)}/gitops`, source);
}

export function disconnectGitOps(project: string) {
  return request<{ ok: boolean }>("DELETE", `${base(project)}/gitops`);
}

export function syncGitOps(project: string) {
  return request<GitOpsSync>("POST", `${base(project)}/gitops/sync`);
}

export function exportProjectConfiguration(project: string, environment: string | null) {
  return requestFile("POST", `${base(project)}/export`, environment ? { environment_id: environment } : {});
}

const workloadPath = (workload: string) => `/api/workloads/${encodeURIComponent(workload)}`;

export const workloadConfigurationDiffQuery = (workload: string) =>
  queryOptions({
    queryKey: ["workloads", workload, "configuration-diff"],
    queryFn: ({ signal }) => request<WorkloadConfigurationDiff>("GET", `${workloadPath(workload)}/configuration-diff`, undefined, signal),
    staleTime: 0,
    gcTime: 0,
    retry: false,
  });

// Read-only Docker inspection on the worker. Refreshed on request only, never polled or retried silently.
export const workloadDriftQuery = (workload: string) =>
  queryOptions({
    queryKey: ["workloads", workload, "drift"],
    queryFn: ({ signal }) => request<WorkloadDrift>("GET", `${workloadPath(workload)}/drift`, undefined, signal),
    staleTime: Infinity,
    retry: false,
    refetchOnWindowFocus: false,
  });

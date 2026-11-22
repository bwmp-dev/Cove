import { queryOptions, useMutation, useQueryClient } from "@tanstack/react-query";
import { request } from "./client";
import { keys } from "./queries";
import type { Permission } from "./types";

export type VariableMode = "copy" | "override" | "remove";

export interface PreviewVariable {
  mode: VariableMode;
  // Write-only. The server never returns override values; has_value says one is saved.
  value?: string;
  has_value?: boolean;
}

export interface PreviewResourcePolicy {
  exclude?: boolean;
  database?: "copy" | "empty";
  volumes?: Record<string, "copy" | "empty">;
  services?: Record<string, boolean>;
  schedule?: boolean;
  variables?: Record<string, PreviewVariable>;
  file_secrets?: "copy" | "remove";
  external?: Record<string, "omit">;
  image_tags?: boolean;
}

export interface PreviewRecipe {
  enabled: boolean;
  base_environment_id: string;
  branch_pattern: string;
  repositories: number[];
  domain_template: string;
  access: "private" | "public";
  ttl_hours: number;
  max_previews: number;
  require_approval: boolean;
  resources: Record<string, PreviewResourcePolicy>;
}

export interface PreviewBlocker {
  code: string;
  message: string;
  resolution: string;
  key?: string;
}

export interface PlannedVolume {
  key: string;
  type: "volume" | "bind";
  source: string;
  services: string[];
  mode: "copy" | "empty" | "database" | "";
  default: string;
  allowed: string[];
  reason?: string;
}

export interface PlannedService {
  name: string;
  image: string;
  tags?: string[];
  run: boolean;
  default: boolean;
  reason: string;
}

export interface PlannedVariable {
  key: string;
  classification: "plain" | "internal" | "sensitive";
  mode: VariableMode | "";
  has_override: boolean;
  reason: string;
}

export interface PlannedDependency {
  key: string;
  kind: "connection" | "network" | "variable" | "external_network";
  label: string;
  inside: boolean;
}

export interface PlannedDatabase {
  engine: "postgresql" | "mariadb";
  service: string;
  name: string;
  mode: "copy" | "empty";
  supported: boolean;
  reason?: string;
  bytes?: number;
}

export interface PlannedResource {
  id: string;
  name: string;
  kind: "service" | "job";
  included: boolean;
  repository?: string;
  repository_id?: number;
  follows_branch: boolean;
  source_kind: "git" | "compose";
  deployed_revision?: string;
  deployed_at?: string | null;
  database?: PlannedDatabase;
  services: PlannedService[];
  volumes: PlannedVolume[];
  variables: PlannedVariable[];
  dependencies: PlannedDependency[];
  routes: string[];
  blockers: PreviewBlocker[];
  notes: string[];
  schedule?: string;
  file_secrets?: number;
}

export interface PreviewRepository {
  id: number;
  name: string;
  selected: boolean;
  assigned: boolean;
}

export interface PreviewPlan {
  resources: PlannedResource[];
  repositories: PreviewRepository[];
  blocked: boolean;
  example_hostnames: string[];
  dns: "cloudflare" | "manual" | "";
}

export interface RecipeView {
  recipe: PreviewRecipe;
  saved: boolean;
  plan: PreviewPlan | null;
}

export type PreviewState = "awaiting_approval" | "creating" | "ready" | "updating" | "failed" | "deleting";
export type PreviewStage = "approval" | "plan" | "data" | "connect" | "deploy" | "publish" | "ready" | "remove";

export interface PreviewBranch {
  repository_id: number;
  repository: string;
  branch: string;
  head: string;
  checked_at: string | null;
}

export interface PreviewResource {
  base_id: string;
  resource_id: string;
  name: string;
  kind: "service" | "job";
  source: "branch" | "baseline" | "disabled";
  revision: string;
  baseline_revision: string;
  baseline_release: string;
  baseline_deployed_at: string | null;
  deployed: boolean;
  data_state: "pending" | "copying" | "ready" | "failed" | "none";
  snapshot_at: string | null;
  data_report: PreviewDataReport | null;
  routes: string[];
  published: boolean;
  operation: string;
}

export interface PreviewDataReport {
  volumes?: { target: string; mode: string; files: number; bytes: number; digest?: string }[];
  database?: { engine: string; snapshot_at: string; dump_bytes: number };
  seeded?: { engine: string; snapshot_at: string; tables?: number; rows?: number; verified?: boolean };
}

export interface EnvironmentPreview {
  id: string;
  project_id: string;
  environment_id: string;
  base_environment_id: string;
  suffix: string;
  slug: string;
  state: PreviewState;
  stage: PreviewStage;
  detail: string;
  pinned: boolean;
  access: "private" | "public";
  delete_reason: string;
  last_activity_at: string;
  expires_at: string | null;
  created_at: string;
  branches: PreviewBranch[];
  resources?: PreviewResource[];
  permissions: Permission[];
}

export interface WaitingBranch {
  repository_id: number;
  branch: string;
  reason: string;
  received_at: string;
}

export interface PreviewList {
  previews: EnvironmentPreview[];
  waiting: WaitingBranch[];
}

const settling = new Set<PreviewState>(["creating", "updating", "deleting"]);

export const previewKeys = {
  recipe: (projectId: string, environmentId: string) => ["previews", "recipe", projectId, environmentId] as const,
  list: (projectId: string) => ["previews", "list", projectId] as const,
  detail: (id: string) => ["previews", "detail", id] as const,
};

export const recipeQuery = (projectId: string, environmentId: string) =>
  queryOptions({
    queryKey: previewKeys.recipe(projectId, environmentId),
    queryFn: ({ signal }) => {
      const query = environmentId ? `?environment=${encodeURIComponent(environmentId)}` : "";
      return request<RecipeView>("GET", `/api/projects/${encodeURIComponent(projectId)}/preview-recipe${query}`, undefined, signal);
    },
    // The plan reads every production resource on its worker; keep it until asked again.
    staleTime: 60_000,
  });

export const previewListQuery = (projectId: string) =>
  queryOptions({
    queryKey: previewKeys.list(projectId),
    queryFn: ({ signal }) => request<PreviewList>("GET", `/api/projects/${encodeURIComponent(projectId)}/environment-previews`, undefined, signal),
    refetchInterval: (query) => (query.state.data?.previews.some((preview) => settling.has(preview.state)) ? 4_000 : 30_000),
  });

export const previewQuery = (id: string) =>
  queryOptions({
    queryKey: previewKeys.detail(id),
    queryFn: ({ signal }) => request<EnvironmentPreview>("GET", `/api/environment-previews/${encodeURIComponent(id)}`, undefined, signal),
    refetchInterval: (query) => (query.state.data && settling.has(query.state.data.state) ? 4_000 : 30_000),
  });

function useInvalidatePreviews() {
  const client = useQueryClient();
  return () => Promise.all([client.invalidateQueries({ queryKey: ["previews"] }), client.invalidateQueries({ queryKey: keys.projects }), client.invalidateQueries({ queryKey: keys.workloads })]);
}

export function useSaveRecipe(projectId: string) {
  const invalidate = useInvalidatePreviews();
  return useMutation({
    mutationFn: (recipe: PreviewRecipe) => request<{ ok: boolean }>("PUT", `/api/projects/${encodeURIComponent(projectId)}/preview-recipe`, recipe),
    onSettled: invalidate,
  });
}

export function useRequestPreview(projectId: string) {
  const invalidate = useInvalidatePreviews();
  return useMutation({
    mutationFn: (suffix: string) => request<{ branch: string }>("POST", `/api/projects/${encodeURIComponent(projectId)}/environment-previews`, { suffix }),
    onSettled: invalidate,
  });
}

export type PreviewAction = "approve" | "retry" | "redeploy";

export function usePreviewAction() {
  const invalidate = useInvalidatePreviews();
  return useMutation({
    mutationFn: ({ id, action }: { id: string; action: PreviewAction }) => request<{ ok: boolean }>("POST", `/api/environment-previews/${encodeURIComponent(id)}/${action}`),
    onSettled: invalidate,
  });
}

export function useRefreshPreview() {
  const invalidate = useInvalidatePreviews();
  return useMutation({
    mutationFn: ({ id, confirm }: { id: string; confirm: string }) => request<{ ok: boolean }>("POST", `/api/environment-previews/${encodeURIComponent(id)}/refresh`, { confirm }),
    onSettled: invalidate,
  });
}

export function usePinPreview() {
  const invalidate = useInvalidatePreviews();
  return useMutation({
    mutationFn: ({ id, pinned }: { id: string; pinned: boolean }) => request<{ ok: boolean }>("PUT", `/api/environment-previews/${encodeURIComponent(id)}`, { pinned }),
    onSettled: invalidate,
  });
}

export function useDeletePreview() {
  const invalidate = useInvalidatePreviews();
  return useMutation({
    mutationFn: ({ id, confirm }: { id: string; confirm: string }) => request<{ ok: boolean }>("DELETE", `/api/environment-previews/${encodeURIComponent(id)}`, { confirm }),
    onSettled: invalidate,
  });
}

// Credentials are fetched on demand and never cached, so they leave memory with the dialog.
export function fetchPreviewCredentials(id: string) {
  return request<{ username: string; password: string }>("GET", `/api/environment-previews/${encodeURIComponent(id)}/credentials`);
}

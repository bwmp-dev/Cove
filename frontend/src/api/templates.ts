import { queryOptions, useMutation, useQueryClient } from "@tanstack/react-query";
import { isStatus, request } from "./client";
import type { FileSecret, Route } from "./types";

export type TemplateOrigin = "builtin" | "repository" | "team";
export type VariableType = "string" | "secret" | "integer" | "boolean" | "hostname" | "url" | "email";

export interface TemplateVariable {
  key: string;
  label: string;
  type: VariableType;
  default: string;
  required: boolean;
  generated: boolean;
}

export interface TemplateWeb {
  route: boolean;
  service: string;
  port: number;
  url_variable: string;
}

export interface TemplateMinimum {
  cpus: number;
  memory_mb: number;
  disk_mb: number;
}

export interface CatalogueTemplate {
  schema_version: 1;
  id: string;
  name: string;
  description: string;
  category: string;
  source: string;
  license: string;
  docs: string;
  notes: string;
  images: Record<string, string>;
  compose: string;
  variables: TemplateVariable[];
  web: TemplateWeb | null;
  storage: string[] | null;
  backup: string;
  minimum: TemplateMinimum;
  capabilities: string[] | null;
  file_secrets: FileSecret[] | null;
}

export interface TemplateEvidence {
  compose_sha256: string;
  schema_validated: boolean;
  compose_config: boolean;
  runtime: "config_only" | "started_healthy";
  checked_at: string;
  message: string;
  services?: Record<string, string>;
}

export interface TemplateEntry {
  origin: TemplateOrigin;
  repository_id: string;
  // Name and public URL of the template source; empty for built-in and team templates.
  repository_name: string;
  repository_url: string;
  revision: string;
  warnings: string[] | null;
  template: CatalogueTemplate;
  validation: TemplateEvidence;
}

export interface TemplateCatalogue {
  schema_version: 1;
  can_manage_team: boolean;
  templates: TemplateEntry[];
}

// A prepared, undeployed resource configuration. Generated secrets live only in component state until the resource is created.
export interface ResourceDraft {
  name: string;
  compose: string;
  environment: Record<string, string>;
  routes: Route[];
  file_secrets: FileSecret[] | null;
}

export interface DraftRequest {
  origin: TemplateOrigin;
  repository_id: string;
  id: string;
  revision: string;
  values: Record<string, string>;
  hostname: string;
  acknowledge_host_access: boolean;
}

export type RepositoryKind = "git" | "index";

export interface TemplateRepository {
  id: string;
  name: string;
  kind: RepositoryKind;
  url: string;
  ref: string;
  revision: string;
  error: string;
  checked_at: string | null;
  next_refresh_at: string;
  refreshing: boolean;
  template_count: number;
}

export interface RepositoryInput {
  name: string;
  kind: RepositoryKind;
  url: string;
  ref: string;
}

// Instance sources are visible to every team; team sources only to the team that added them.
export type RepositoryScope = { kind: "instance" } | { kind: "team"; teamId: string };

export interface TemplateMetadata {
  schema_version: 1;
  id: string;
  name: string;
  description: string;
  category: string;
  source: string;
  license: string;
  docs: string;
  minimum: TemplateMinimum;
}

export interface TemplatePreview {
  template: CatalogueTemplate;
  configuration_revision: string;
  warnings: string[] | null;
}

export const MAX_REPOSITORIES = 8;

export const templateKeys = {
  catalogue: ["templates"] as const,
  repositories: (scope: RepositoryScope) => ["template-repositories", scope.kind === "team" ? scope.teamId : ""] as const,
};

export function entryKey(entry: Pick<TemplateEntry, "origin" | "repository_id"> & { template: Pick<CatalogueTemplate, "id"> }) {
  return `${entry.origin}:${entry.repository_id}:${entry.template.id}`;
}

export const templatesQuery = queryOptions({
  queryKey: templateKeys.catalogue,
  queryFn: ({ signal }) => request<TemplateCatalogue>("GET", "/api/templates", undefined, signal),
  retry: false,
});

function repositoriesPath(scope: RepositoryScope) {
  return scope.kind === "team" ? `/api/teams/${encodeURIComponent(scope.teamId)}/template-repositories` : "/api/settings/template-repositories";
}

export const templateRepositoriesQuery = (scope: RepositoryScope) =>
  queryOptions({
    queryKey: templateKeys.repositories(scope),
    queryFn: ({ signal }) => request<{ repositories: TemplateRepository[] }>("GET", repositoriesPath(scope), undefined, signal),
    // Follows a refresh started elsewhere, or a new source's first scheduled check, only while the manager is open.
    refetchInterval: (query) => (query.state.data?.repositories.some((repository) => repository.refreshing || repository.checked_at === null) ? 5_000 : false),
    retry: false,
  });

// The response holds freshly generated secrets, so it never enters the query cache and leaves the mutation cache on unmount.
export function useTemplateDraft() {
  const client = useQueryClient();
  return useMutation({
    gcTime: 0,
    mutationFn: (input: DraftRequest) => request<ResourceDraft>("POST", "/api/templates/draft", input),
    onError: async (error) => {
      if (isStatus(error, 409) || isStatus(error, 404)) {
        await client.invalidateQueries({ queryKey: templateKeys.catalogue, exact: true });
      }
    },
  });
}

function useRepositoryRefetch(scope: RepositoryScope) {
  const client = useQueryClient();
  return () =>
    Promise.all([
      client.invalidateQueries({ queryKey: templateKeys.repositories(scope), exact: true }),
      client.invalidateQueries({ queryKey: templateKeys.catalogue, exact: true }),
    ]);
}

export function useAddRepository(scope: RepositoryScope) {
  const refetch = useRepositoryRefetch(scope);
  return useMutation({
    mutationFn: (input: RepositoryInput) => request<{ id: string }>("POST", repositoriesPath(scope), input),
    onSettled: refetch,
  });
}

export function useRemoveRepository(scope: RepositoryScope) {
  const refetch = useRepositoryRefetch(scope);
  return useMutation({
    mutationFn: (id: string) => request<{ id: string }>("DELETE", `${repositoriesPath(scope)}/${encodeURIComponent(id)}`),
    onSettled: refetch,
  });
}

// A failed refresh (422) still records its check time and error, so the list and catalogue are reloaded either way.
export function useRefreshRepository(scope: RepositoryScope) {
  const refetch = useRepositoryRefetch(scope);
  return useMutation({
    mutationFn: (id: string) => request<{ refreshed: boolean }>("POST", `${repositoriesPath(scope)}/${encodeURIComponent(id)}/refresh`, {}),
    onSettled: refetch,
  });
}

function exportPath(workloadId: string) {
  return `/api/workloads/${encodeURIComponent(workloadId)}/template`;
}

export function useTemplatePreview(workloadId: string) {
  return useMutation({
    gcTime: 0,
    mutationFn: (template: TemplateMetadata) => request<TemplatePreview>("POST", exportPath(workloadId), { template, confirm: false }),
  });
}

export function useSaveTemplate(workloadId: string) {
  const client = useQueryClient();
  return useMutation({
    gcTime: 0,
    mutationFn: ({ template, revision }: { template: TemplateMetadata; revision: string }) =>
      request<{ id: string; revision: string }>("POST", exportPath(workloadId), { template, confirm: true, configuration_revision: revision }),
    onSuccess: () => client.invalidateQueries({ queryKey: templateKeys.catalogue, exact: true }),
  });
}

export function useDeleteTeamTemplate(teamId: string) {
  const client = useQueryClient();
  return useMutation({
    mutationFn: (templateId: string) => request<{ deleted: boolean }>("DELETE", `/api/teams/${encodeURIComponent(teamId)}/templates/${encodeURIComponent(templateId)}`),
    onSettled: () => client.invalidateQueries({ queryKey: templateKeys.catalogue, exact: true }),
  });
}

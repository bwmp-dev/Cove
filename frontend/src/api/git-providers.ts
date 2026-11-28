import { queryOptions } from "@tanstack/react-query";
import { request } from "./client";
import type { OperationState } from "./types";

// Forgejo and Codeberg are created with their own kind but the server stores them as "gitea".
export type GitProviderKind = "gitlab" | "gitea" | "forgejo" | "codeberg" | "bitbucket";

export interface GitConnection {
  id: number;
  name: string;
  kind: GitProviderKind;
  url: string;
  workspace: string;
  webhook_url: string;
}

// Only the create response carries the webhook signing secret; it is never returned again.
export interface CreatedGitConnection extends GitConnection {
  webhook_secret: string;
}

export interface GitConnectionInput {
  name: string;
  kind: GitProviderKind;
  url: string;
  workspace?: string;
  // Bitbucket only: the account email of an API token. Without it the token is used as an OAuth bearer token.
  username?: string;
  token: string;
}

export interface ProviderRepository {
  id: number;
  full_name: string;
  clone_url: string;
  default_branch: string;
  installation_id: number;
}

export interface ProviderReport {
  operation_id: string;
  reported_state: OperationState | "";
  error: string;
  next_attempt_at: string;
}

export interface ProviderSource {
  connection_id: number;
  repository_id: number;
  automatic: boolean;
  reporting: boolean;
  reports: ProviderReport[];
}

export interface ProviderSourceInput {
  connection_id: number;
  repository_id: number;
  branch: string;
  compose_path: string;
  inline_compose?: boolean;
  automatic: boolean;
  reporting: boolean;
}

export interface ProviderSelection {
  connectionId: number | null;
  repositoryId: number | null;
  branch: string;
}

export const emptySelection: ProviderSelection = { connectionId: null, repositoryId: null, branch: "" };

export const providerLabels: Record<GitProviderKind, string> = {
  gitlab: "GitLab",
  gitea: "Gitea or Forgejo",
  forgejo: "Forgejo",
  codeberg: "Codeberg",
  bitbucket: "Bitbucket Cloud",
};

export const automaticDescription = "Deploys the pushed commit when the selected branch changes. Events wait while another operation is active.";
export const reportingDescription = "Posts each deployment as a commit status and keeps one comment, authored by the connection's account, updated on pull or merge request previews. Previews follow this setting.";

export const gitProviderKeys = {
  connections: ["git-connections"] as const,
  repositories: (connection: number) => ["git-connections", connection, "repositories"] as const,
  branches: (connection: number, repository: number) => ["git-connections", connection, "repositories", repository, "branches"] as const,
  source: (workload: string) => ["workloads", workload, "git-provider"] as const,
};

export const gitConnectionsQuery = queryOptions({
  queryKey: gitProviderKeys.connections,
  queryFn: ({ signal }) => request<GitConnection[]>("GET", "/api/git/connections", undefined, signal),
});

export const providerRepositoriesQuery = (connection: number) =>
  queryOptions({
    queryKey: gitProviderKeys.repositories(connection),
    queryFn: ({ signal }) => request<ProviderRepository[]>("GET", `/api/git/connections/${connection}/repositories`, undefined, signal),
  });

// The server returns full refs; deployments take the short branch name.
export const providerBranchesQuery = (connection: number, repository: number) =>
  queryOptions({
    queryKey: gitProviderKeys.branches(connection, repository),
    queryFn: ({ signal }) => request<{ ref: string }[]>("GET", `/api/git/connections/${connection}/repositories/${repository}/branches`, undefined, signal),
    select: (refs) => refs.map(({ ref }) => ref.replace(/^refs\/heads\//, "")),
  });

export const providerSourceQuery = (workload: string) =>
  queryOptions({
    queryKey: gitProviderKeys.source(workload),
    queryFn: ({ signal }) => request<ProviderSource | null>("GET", `/api/workloads/${encodeURIComponent(workload)}/git-provider`, undefined, signal),
  });

export function saveProviderSource(workload: string, input: ProviderSourceInput) {
  return request<{ ok: boolean }>("PUT", `/api/workloads/${encodeURIComponent(workload)}/git-provider`, input);
}

export type TriggerKind = "branch" | "tags" | "releases";

export interface GitTrigger {
  kind: TriggerKind;
  pattern: string;
}

export const gitTriggerQuery = (workload: string) =>
  queryOptions({
    queryKey: ["workloads", workload, "git-trigger"] as const,
    queryFn: ({ signal }) => request<GitTrigger>("GET", `/api/workloads/${encodeURIComponent(workload)}/git-trigger`, undefined, signal),
  });

export interface DeployHook {
  id: string;
  url: string;
  expires_at: string;
}

// Only the create or rotate response carries the bearer token; it is never returned again.
export interface CreatedDeployHook extends DeployHook {
  token: string;
}

export const deployHookQuery = (workload: string) =>
  queryOptions({
    queryKey: ["workloads", workload, "deploy-hook"] as const,
    queryFn: ({ signal }) => request<DeployHook | null>("GET", `/api/workloads/${encodeURIComponent(workload)}/deploy-hook`, undefined, signal),
  });

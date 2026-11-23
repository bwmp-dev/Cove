import { queryOptions, useMutation, useQueryClient, type QueryClient } from "@tanstack/react-query";
import { rememberTeam } from "../lib/team";
import { request } from "./client";
import { keys } from "./queries";
import { securityKeys } from "./security";
import { teamKeys } from "./teams";
import type { Grant } from "./types";

// Directory credentials (bind passwords, typed directory passwords, provisioning tokens) leave the mutation cache
// as soon as the form or dialog that used them unmounts.
const transient = { gcTime: 0 } as const;

export type DirectorySourceKind = "github" | "scim" | "ldap";

// role is a built-in team role (owner, admin, member), a built-in team-wide grant role (viewer, operator, editor)
// or a custom role ID of that exact team.
export interface DirectoryMapping {
  group: string;
  team_id: string;
  role: string;
}

export interface MemberState {
  role: string;
  grants: Grant[];
}

export interface DirectoryChange {
  team_id: string;
  user_id: string;
  // Profile labels for the reviewing administrator; absent from older responses.
  user_name?: string;
  email?: string;
  before: MemberState | null;
  after: MemberState | null;
  warning?: string;
}

export interface GithubTeamSource {
  id: string;
  enabled: boolean;
  version: number;
  checked_at: string | null;
  applied_at: string | null;
  next_check_at: string;
  last_error: string;
  credential_configured: boolean;
  configuration: {
    name: string;
    mode: "app" | "oauth";
    account: string;
    installation_id?: number;
    oauth_user_id?: string;
    oauth_github_id?: string;
  };
  mappings: DirectoryMapping[] | null;
}

export interface GithubTeamSourceInput {
  name: string;
  mode: "app" | "oauth";
  account: string;
  installation_id?: number;
  enabled: boolean;
  version?: number;
  mappings: DirectoryMapping[];
}

export interface GithubTeamReport {
  version: number;
  roster_hash: string;
  applied: boolean;
  unmatched_github_users: number;
  changes: DirectoryChange[] | null;
}

export interface ScimSettings {
  enabled: boolean;
  token_configured: boolean;
  oidc_provider_id: string;
  rotated_at: string | null;
  version: number;
  base_url: string;
}

export interface ScimGroup {
  id: string;
  display_name: string;
  external_id: string;
  version: number;
  members: number;
  mappings: DirectoryMapping[] | null;
}

export interface ScimLink {
  user_id: string;
  user_name: string;
  external_id: string;
  oidc_provider_id: string;
  active: boolean;
  deleted_at: string | null;
  version: number;
}

export interface LdapConfiguration {
  display_name: string;
  host: string;
  port: number;
  transport: "ldaps" | "starttls";
  ca_pem: string;
  bind_dn: string;
  // Write-only: never returned. Omitted or empty on update keeps the saved password.
  bind_password?: string;
  user_base_dn: string;
  user_filter: string;
  subject_attribute: "entryUUID" | "objectGUID";
  email_attribute: string;
  name_attribute: string;
  group_base_dn: string;
  group_filter: string;
  allow_registration: boolean;
}

export interface LdapProvider {
  id: string;
  enabled: boolean;
  version: number;
  password_configured: boolean;
  configuration: LdapConfiguration;
  mappings: DirectoryMapping[] | null;
}

export interface LdapProviders {
  network_policy_configured: boolean;
  providers: LdapProvider[] | null;
}

export interface DirectoryTeam {
  id: string;
  name: string;
}

export interface ScimActivity {
  id: number;
  time: string;
  operation: string;
  // A retained user or group ID; empty for a create that failed before an ID existed.
  target_id: string;
  status: number;
  outcome: "succeeded" | "failed";
}

export interface PublicLdapProvider {
  id: string;
  display_name: string;
}

export const directoryKeys = {
  githubSources: ["github-team-sync"] as const,
  scim: ["scim-settings"] as const,
  scimGroups: ["scim-settings", "groups"] as const,
  scimLinks: ["scim-settings", "links"] as const,
  ldap: ["ldap-providers"] as const,
  publicLdap: ["ldap-sign-in-providers"] as const,
  teams: ["directory-teams"] as const,
  scimActivity: ["scim-settings", "activity"] as const,
};

const githubSource = (id: string) => `/api/settings/github-team-sync/${encodeURIComponent(id)}`;
const ldapProvider = (id: string) => `/api/settings/ldap/providers/${encodeURIComponent(id)}`;

export const githubTeamSourcesQuery = queryOptions({
  queryKey: directoryKeys.githubSources,
  queryFn: async ({ signal }) => (await request<{ sources: GithubTeamSource[] | null }>("GET", "/api/settings/github-team-sync", undefined, signal)).sources ?? [],
});

export const scimSettingsQuery = queryOptions({
  queryKey: directoryKeys.scim,
  queryFn: ({ signal }) => request<ScimSettings>("GET", "/api/settings/scim", undefined, signal),
});

export const scimGroupsQuery = queryOptions({
  queryKey: directoryKeys.scimGroups,
  queryFn: async ({ signal }) => (await request<{ groups: ScimGroup[] | null }>("GET", "/api/settings/scim/groups", undefined, signal)).groups ?? [],
});

export const scimLinksQuery = queryOptions({
  queryKey: directoryKeys.scimLinks,
  queryFn: async ({ signal }) => (await request<{ links: ScimLink[] | null }>("GET", "/api/settings/scim/links", undefined, signal)).links ?? [],
});

// Every team on the instance, for mapping pickers and labels. Instance administrators only.
export const directoryTeamsQuery = queryOptions({
  queryKey: directoryKeys.teams,
  queryFn: async ({ signal }) => (await request<{ teams: DirectoryTeam[] | null }>("GET", "/api/settings/directory/teams", undefined, signal)).teams ?? [],
});

export const scimActivityQuery = queryOptions({
  queryKey: directoryKeys.scimActivity,
  queryFn: async ({ signal }) => (await request<{ activity: ScimActivity[] | null }>("GET", "/api/settings/scim/activity", undefined, signal)).activity ?? [],
  // Provisioning happens out of band, so keep the list current while the page is open.
  refetchInterval: 30_000,
});

export const ldapProvidersQuery = queryOptions({
  queryKey: directoryKeys.ldap,
  queryFn: ({ signal }) => request<LdapProviders>("GET", "/api/settings/ldap/providers", undefined, signal),
});

export const publicLdapProvidersQuery = queryOptions({
  queryKey: directoryKeys.publicLdap,
  queryFn: ({ signal }) => request<PublicLdapProvider[] | null>("GET", "/auth/ldap/providers", undefined, signal).then((providers) => providers ?? []),
  staleTime: 60_000,
  retry: false,
});

// Directory changes rewrite team memberships, so every team's member list, the session's teams and the users list may change.
function invalidateMemberships(client: QueryClient) {
  return Promise.all([
    client.invalidateQueries({ queryKey: ["teams"] }),
    client.invalidateQueries({ queryKey: keys.session }),
    client.invalidateQueries({ queryKey: teamKeys.users }),
  ]);
}

export function useSaveGithubTeamSource() {
  const client = useQueryClient();
  return useMutation({
    mutationFn: ({ id, input }: { id?: string; input: GithubTeamSourceInput }) => request<{ id: string }>(id ? "PUT" : "POST", id ? githubSource(id) : "/api/settings/github-team-sync", input),
    onSettled: () => client.invalidateQueries({ queryKey: directoryKeys.githubSources }),
  });
}

export function usePreviewGithubTeamSource() {
  return useMutation({
    mutationFn: ({ id, version }: { id: string; version: number }) => request<GithubTeamReport>("POST", `${githubSource(id)}/preview`, { version }),
  });
}

export function useApplyGithubTeamSource() {
  const client = useQueryClient();
  return useMutation({
    mutationFn: ({ id, version, roster_hash }: { id: string; version: number; roster_hash: string }) =>
      request<GithubTeamReport>("POST", `${githubSource(id)}/reconcile`, { version, roster_hash, confirm: true }),
    onSettled: () => Promise.all([client.invalidateQueries({ queryKey: directoryKeys.githubSources }), invalidateMemberships(client)]),
  });
}

export function useDeleteGithubTeamSource() {
  const client = useQueryClient();
  return useMutation({
    mutationFn: ({ id, version }: { id: string; version: number }) => request<{ id: string }>("DELETE", githubSource(id), { version, confirm: true }),
    onSettled: () => Promise.all([client.invalidateQueries({ queryKey: directoryKeys.githubSources }), invalidateMemberships(client)]),
  });
}

// The token is returned once. It lives only in this mutation's result, which is dropped when the component unmounts.
export function useGenerateScimToken() {
  const client = useQueryClient();
  return useMutation({
    ...transient,
    mutationFn: (version: number) => request<{ ok: boolean; version: number; token: string }>("POST", "/api/settings/scim/token", { version, confirm: true }),
    onSettled: () => client.invalidateQueries({ queryKey: directoryKeys.scim, exact: true }),
  });
}

export function useUpdateScimSettings() {
  const client = useQueryClient();
  return useMutation({
    mutationFn: (input: { version: number; enabled: boolean; oidc_provider_id: string }) => request<{ ok: boolean; version: number }>("PUT", "/api/settings/scim", input),
    onSettled: () => client.invalidateQueries({ queryKey: directoryKeys.scim, exact: true }),
  });
}

export function useSaveScimGroupMappings() {
  const client = useQueryClient();
  return useMutation({
    mutationFn: ({ group, group_version, mappings }: { group: string; group_version: number; mappings: Omit<DirectoryMapping, "group">[] }) =>
      request<{ ok: boolean; group_version: number }>("PUT", `/api/settings/scim/groups/${encodeURIComponent(group)}/mappings`, { group_version, confirm: true, mappings }),
    onSettled: () => Promise.all([client.invalidateQueries({ queryKey: directoryKeys.scimGroups }), invalidateMemberships(client)]),
  });
}

export function useAdoptScimAccount() {
  const client = useQueryClient();
  return useMutation({
    mutationFn: (input: { user_id: string; user_name?: string; external_id: string }) => request<{ ok: boolean }>("POST", "/api/settings/scim/links", { ...input, confirm: true }),
    onSettled: () => client.invalidateQueries({ queryKey: directoryKeys.scimLinks }),
  });
}

export function useDetachScimAccount() {
  const client = useQueryClient();
  return useMutation({
    mutationFn: (userId: string) => request<{ ok: boolean }>("DELETE", `/api/settings/scim/links/${encodeURIComponent(userId)}`, { confirm: true }),
    onSettled: () => Promise.all([client.invalidateQueries({ queryKey: directoryKeys.scimLinks }), client.invalidateQueries({ queryKey: directoryKeys.scimGroups }), invalidateMemberships(client)]),
  });
}

export function useSaveLdapProvider() {
  const client = useQueryClient();
  return useMutation({
    ...transient,
    mutationFn: ({ id, input }: { id?: string; input: { configuration: LdapConfiguration; enabled: boolean; version?: number; mappings: DirectoryMapping[] } }) =>
      request<{ id: string; version: number; ok: boolean }>(id ? "PUT" : "POST", id ? ldapProvider(id) : "/api/settings/ldap/providers", input),
    onSettled: () => Promise.all([client.invalidateQueries({ queryKey: directoryKeys.ldap }), client.invalidateQueries({ queryKey: directoryKeys.publicLdap })]),
  });
}

export function useTestLdapProvider() {
  return useMutation({
    mutationFn: (id: string) => request<{ ok: boolean }>("POST", `${ldapProvider(id)}/test`),
  });
}

export function useDeleteLdapProvider() {
  const client = useQueryClient();
  return useMutation({
    mutationFn: ({ id, version }: { id: string; version: number }) => request<{ id: string; version: number; ok: boolean }>("DELETE", ldapProvider(id), { version, confirm: true }),
    onSettled: () => Promise.all([client.invalidateQueries({ queryKey: directoryKeys.ldap }), client.invalidateQueries({ queryKey: directoryKeys.publicLdap }), invalidateMemberships(client)]),
  });
}

export interface LdapSignInResult {
  user: string;
  linked: boolean;
  second_factor_required: boolean;
}

// The API names the lockout field "email", but it carries the directory login name (uid, sAMAccountName, …).
export function useLdapSignIn() {
  const client = useQueryClient();
  return useMutation({
    ...transient,
    mutationFn: ({ provider, username, password }: { provider: string; username: string; password: string }) =>
      request<LdapSignInResult>("POST", `/auth/ldap/${encodeURIComponent(provider)}/sign-in`, { email: username, password }),
    // A completed sign-in may be a different account than the cached one, so start from an empty cache and the default team.
    onSuccess: () => {
      rememberTeam(null);
      client.clear();
    },
  });
}

export function useLinkLdapIdentity() {
  const client = useQueryClient();
  return useMutation({
    ...transient,
    mutationFn: ({ provider, username, password, proof }: { provider: string; username: string; password: string; proof: { current_password?: string; code?: string } }) =>
      request<LdapSignInResult>("POST", `/auth/ldap/${encodeURIComponent(provider)}/sign-in`, { email: username, password, link: true, ...proof }),
    onSuccess: () =>
      Promise.all([client.invalidateQueries({ queryKey: securityKeys.identities }), client.invalidateQueries({ queryKey: securityKeys.sessions }), invalidateMemberships(client)]),
  });
}

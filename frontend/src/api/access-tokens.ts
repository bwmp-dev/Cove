import { queryOptions, useMutation, useQueryClient } from "@tanstack/react-query";
import { request } from "./client";
import type { TeamPermission } from "./types";

export type ApiScopeLevel = 1 | 2 | 3;
// permissions lists the catalogue permissions the creator must hold wherever the key is used; grants can differ per project.
export interface ApiScope { name: string; level: ApiScopeLevel; description: string; permissions: TeamPermission[]; instance_admin: boolean; personal_only: boolean }
export interface AccessToken { id: string; name: string; user_id: string; team_id: string; scope: "user" | "team"; scopes: string[]; expires_at: string }
export interface AccessTokenInput { name: string; scope: AccessToken["scope"]; scopes: string[]; days: number }
export interface IssuedAccessToken { id: string; token: string; expires_at: string }

export const accessTokenScopesQuery = queryOptions({
  queryKey: ["access-tokens", "scopes"],
  queryFn: ({ signal }) => request<ApiScope[]>("GET", "/api/access-tokens/scopes", undefined, signal),
  staleTime: Infinity,
});

export const accessTokensQuery = queryOptions({
  queryKey: ["access-tokens", "list"],
  queryFn: ({ signal }) => request<AccessToken[]>("GET", "/api/access-tokens", undefined, signal),
});

// The issued token lives only in this mutation's result, which is dropped as soon as the form is dismissed.
export function useCreateAccessToken() {
  const client = useQueryClient();
  return useMutation({
    gcTime: 0,
    mutationFn: (input: AccessTokenInput) => request<IssuedAccessToken>("POST", "/api/access-tokens", input),
    onSuccess: () => client.invalidateQueries({ queryKey: accessTokensQuery.queryKey }),
  });
}

export function useRevokeAccessToken() {
  const client = useQueryClient();
  return useMutation({
    mutationFn: (id: string) => request<{ ok: boolean }>("DELETE", `/api/access-tokens/${encodeURIComponent(id)}`),
    onSuccess: () => client.invalidateQueries({ queryKey: accessTokensQuery.queryKey }),
  });
}

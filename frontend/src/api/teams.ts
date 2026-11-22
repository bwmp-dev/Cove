import { queryOptions, useMutation, useQueryClient } from "@tanstack/react-query";
import { request } from "./client";
import { keys } from "./queries";
import type { Grant, TeamRole } from "./types";

export interface Member {
  user_id: string;
  email: string;
  name: string;
  role: TeamRole;
  grants: Grant[];
}

export interface Invitation {
  id: string;
  email: string;
  role: "admin" | "member";
  grants: Grant[];
  expires_at: string;
}

export interface IssuedInvitation {
  id: string;
  url: string;
  expires_at: string;
  email_sent: boolean;
}

export interface InstanceSettings {
  registration_enabled: boolean;
  email_enabled: boolean;
  github_enabled: boolean;
}

export interface User {
  id: string;
  email: string;
  name: string;
  is_admin: boolean;
  disabled: boolean;
}

const team = (id: string) => `/api/teams/${encodeURIComponent(id)}`;

export const teamKeys = {
  members: (teamId: string) => ["teams", teamId, "members"] as const,
  invitations: (teamId: string) => ["teams", teamId, "invitations"] as const,
  nodes: (teamId: string) => ["teams", teamId, "nodes"] as const,
  settings: ["settings"] as const,
  users: ["users"] as const,
};

export const membersQuery = (teamId: string) =>
  queryOptions({ queryKey: teamKeys.members(teamId), queryFn: ({ signal }) => request<Member[]>("GET", `${team(teamId)}/members`, undefined, signal) });

export const invitationsQuery = (teamId: string) =>
  queryOptions({ queryKey: teamKeys.invitations(teamId), queryFn: ({ signal }) => request<Invitation[]>("GET", `${team(teamId)}/invitations`, undefined, signal) });

export const teamNodesQuery = (teamId: string) =>
  queryOptions({ queryKey: teamKeys.nodes(teamId), queryFn: ({ signal }) => request<string[]>("GET", `${team(teamId)}/nodes`, undefined, signal) });

export const settingsQuery = queryOptions({ queryKey: teamKeys.settings, queryFn: ({ signal }) => request<InstanceSettings>("GET", "/api/settings", undefined, signal) });

export const usersQuery = queryOptions({ queryKey: teamKeys.users, queryFn: ({ signal }) => request<User[]>("GET", "/api/users", undefined, signal) });

export function useCreateTeam() {
  return useMutation({ mutationFn: (name: string) => request<{ id: string }>("POST", "/api/teams", { name }) });
}

export function useUpdateMember(teamId: string) {
  const client = useQueryClient();
  return useMutation({
    mutationFn: ({ userId, role, grants }: { userId: string; role: TeamRole; grants: Grant[] }) =>
      request<{ ok: boolean }>("PUT", `${team(teamId)}/members/${encodeURIComponent(userId)}`, { role, grants }),
    onSettled: () => Promise.all([client.invalidateQueries({ queryKey: teamKeys.members(teamId) }), client.invalidateQueries({ queryKey: keys.session })]),
  });
}

export function useRemoveMember(teamId: string) {
  const client = useQueryClient();
  return useMutation({
    mutationFn: (userId: string) => request<{ ok: boolean }>("DELETE", `${team(teamId)}/members/${encodeURIComponent(userId)}`),
    onSettled: () => client.invalidateQueries({ queryKey: teamKeys.members(teamId) }),
  });
}

export function useCreateInvitation(teamId: string) {
  const client = useQueryClient();
  return useMutation({
    // The invitation URL is a bearer credential: keep it only while the result is on screen.
    gcTime: 0,
    mutationFn: (input: { email: string; role: "admin" | "member"; grants: Grant[] }) => request<IssuedInvitation>("POST", `${team(teamId)}/invitations`, input),
    onSettled: () => client.invalidateQueries({ queryKey: teamKeys.invitations(teamId) }),
  });
}

export function useRevokeInvitation(teamId: string) {
  const client = useQueryClient();
  return useMutation({
    mutationFn: (invitationId: string) => request<{ ok: boolean }>("DELETE", `${team(teamId)}/invitations/${encodeURIComponent(invitationId)}`),
    onSettled: () => client.invalidateQueries({ queryKey: teamKeys.invitations(teamId) }),
  });
}

export function useSetTeamNodes(teamId: string) {
  const client = useQueryClient();
  return useMutation({
    mutationFn: (nodeIds: string[]) => request<{ ok: boolean }>("PUT", `${team(teamId)}/nodes`, { node_ids: nodeIds }),
    onSettled: () => Promise.all([client.invalidateQueries({ queryKey: teamKeys.nodes(teamId) }), client.invalidateQueries({ queryKey: keys.nodes })]),
  });
}

export function useUpdateSettings() {
  const client = useQueryClient();
  return useMutation({
    mutationFn: (input: { registration_enabled: boolean }) => request<InstanceSettings>("PUT", "/api/settings", input),
    onSettled: () => Promise.all([client.invalidateQueries({ queryKey: teamKeys.settings }), client.invalidateQueries({ queryKey: keys.authConfig })]),
  });
}

export function useUpdateUser() {
  const client = useQueryClient();
  return useMutation({
    mutationFn: ({ id, ...input }: { id: string; disabled: boolean; is_admin: boolean }) => request<{ ok: boolean }>("PUT", `/api/users/${encodeURIComponent(id)}`, input),
    onSettled: () => client.invalidateQueries({ queryKey: teamKeys.users }),
  });
}

export function useResetLink() {
  return useMutation({
    gcTime: 0,
    mutationFn: (userId: string) => request<{ url: string; expires_at: string }>("POST", `/api/users/${encodeURIComponent(userId)}/reset`),
  });
}

export function useUpdateAccount() {
  const client = useQueryClient();
  return useMutation({
    gcTime: 0,
    mutationFn: (input: { name: string; email: string; current_password: string }) => request<{ ok: boolean; verification_required: boolean }>("PUT", "/api/account", input),
    onSettled: () => client.invalidateQueries({ queryKey: keys.session }),
  });
}

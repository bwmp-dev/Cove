import { useMutation, useQueryClient } from "@tanstack/react-query";
import { rememberTeam } from "../lib/team";
import { ApiError, request } from "./client";
import type { Grant, TeamRole } from "./types";

export function isGitHubAuthorizeURL(value: string) {
  try {
    const url = new URL(value);
    return url.protocol === "https:" && url.host === "github.com" && url.pathname === "/login/oauth/authorize" && url.username === "" && url.password === "";
  } catch {
    return false;
  }
}

// Every flow here carries passwords or one-time tokens, so nothing is kept in the mutation cache.
const transient = { gcTime: 0 } as const;

export interface InvitationDetails {
  email: string;
  team_name: string;
  role: TeamRole;
  grants: Grant[];
  account_exists: boolean;
}

export function useRegister() {
  return useMutation({
    ...transient,
    mutationFn: (input: { email: string; name: string; password: string }) => request<{ ok: boolean }>("POST", "/auth/register", input),
  });
}

export function useVerifyEmail() {
  return useMutation({
    ...transient,
    mutationFn: (token: string) => request<{ ok: boolean }>("POST", "/auth/verify", { token }),
  });
}

export function useForgotPassword() {
  return useMutation({
    ...transient,
    mutationFn: (email: string) => request<{ ok: boolean }>("POST", "/auth/forgot-password", { email }),
  });
}

export function useResetPassword() {
  return useMutation({
    ...transient,
    mutationFn: (input: { token: string; password: string }) => request<{ ok: boolean }>("POST", "/auth/reset-password", input),
  });
}

export function useInvitationDetails() {
  return useMutation({
    ...transient,
    mutationFn: (token: string) => request<InvitationDetails>("POST", "/auth/invitation", { token }),
  });
}

export function useAcceptInvitation() {
  const client = useQueryClient();
  return useMutation({
    ...transient,
    mutationFn: (input: { token: string; name?: string; password?: string }) => request<{ ok: boolean }>("POST", "/auth/accept-invitation", input),
    onSuccess: () => {
      rememberTeam(null);
      client.clear();
    },
  });
}

export function useGitHubStart() {
  return useMutation({
    ...transient,
    mutationFn: async (input: { invitation_token?: string; link?: boolean; current_password?: string }) => {
      const result = await request<{ url: string }>("POST", "/auth/github/start", input);
      if (!isGitHubAuthorizeURL(result.url)) throw new ApiError(502, "GitHub sign-in returned an unexpected address.");
      return result;
    },
    // The provider URL carries OAuth state; leaving immediately keeps it out of the page.
    onSuccess: ({ url }) => window.location.assign(url),
  });
}

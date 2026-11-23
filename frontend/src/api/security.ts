import { queryOptions, useMutation, useQueryClient, type QueryClient } from "@tanstack/react-query";
import type { ReauthProof } from "../lib/reauth";
import type { SignInMethods } from "../lib/sign-in";
import { rememberTeam } from "../lib/team";
import { createPasskey, getPasskey, type CreationOptionsJSON, type RequestOptionsJSON } from "../lib/webauthn";
import { ApiError, request } from "./client";
import { keys } from "./queries";

// Codes, passwords, provisioning secrets and recovery codes leave the mutation cache as soon as the form unmounts.
const transient = { gcTime: 0 } as const;

export interface AccountSignInMethods {
  totp_enrolled: boolean;
  passkey_enrolled: boolean;
  second_factor_required: boolean;
  enrolment_required: boolean;
  recovery_codes_remaining: number;
}

export interface LinkedIdentity {
  // An OIDC provider ID, or "github".
  provider_id: string;
  subject: string;
}

export interface Passkey {
  id: string;
  name: string;
  created_at: string;
}

export interface LoginEvent {
  id: number;
  success: boolean;
  method: string;
  address: string;
  user_agent: string;
  created_at: string;
}

export interface SecondFactorPolicy {
  require_second_factor: boolean;
}

export const securityKeys = {
  signInMethods: ["sign-in-methods"] as const,
  account: ["account", "sign-in-methods"] as const,
  passkeys: ["account", "passkeys"] as const,
  identities: ["account", "identities"] as const,
  sessions: ["account-sessions"] as const,
  loginHistory: (userId: string | null) => ["login-history", userId ?? "self"] as const,
  policy: (teamId: string | null) => ["second-factor-policy", teamId ?? "instance"] as const,
};

// Whether a second factor is pending depends on the current cookie, so this is always read fresh.
export const signInMethodsQuery = queryOptions({
  queryKey: securityKeys.signInMethods,
  queryFn: ({ signal }) => request<SignInMethods>("GET", "/auth/sign-in-methods", undefined, signal),
  staleTime: 0,
  retry: false,
});

export const accountSignInMethodsQuery = queryOptions({
  queryKey: securityKeys.account,
  queryFn: ({ signal }) => request<AccountSignInMethods>("GET", "/api/account/sign-in-methods", undefined, signal),
  staleTime: 30_000,
});

export const passkeysQuery = queryOptions({
  queryKey: securityKeys.passkeys,
  queryFn: ({ signal }) => request<Passkey[]>("GET", "/api/account/passkeys", undefined, signal),
});

export const identitiesQuery = queryOptions({
  queryKey: securityKeys.identities,
  queryFn: ({ signal }) => request<LinkedIdentity[]>("GET", "/api/account/identities", undefined, signal),
});

export const loginHistoryQuery = (userId: string | null) =>
  queryOptions({
    queryKey: securityKeys.loginHistory(userId),
    queryFn: ({ signal }) => request<LoginEvent[]>("GET", userId ? `/api/users/${encodeURIComponent(userId)}/login-history` : "/api/account/login-history", undefined, signal),
  });

const policyPath = (teamId: string | null) => (teamId ? `/api/teams/${encodeURIComponent(teamId)}/second-factor` : "/api/settings/second-factor");

export const secondFactorPolicyQuery = (teamId: string | null) =>
  queryOptions({
    queryKey: securityKeys.policy(teamId),
    queryFn: ({ signal }) => request<SecondFactorPolicy>("GET", policyPath(teamId), undefined, signal),
  });

// A completed sign-in may be a different account than the cached one, so start from an empty cache and the default team.
function signedIn(client: QueryClient) {
  rememberTeam(null);
  client.clear();
}

export function useSecondFactorChallenge() {
  const client = useQueryClient();
  return useMutation({
    ...transient,
    mutationFn: (code: string) => request<{ ok: boolean }>("POST", "/auth/second-factor", { code }),
    onSuccess: () => signedIn(client),
  });
}

export function usePasskeySignIn() {
  const client = useQueryClient();
  return useMutation({
    ...transient,
    mutationFn: async () => {
      const options = await request<RequestOptionsJSON>("POST", "/auth/passkeys/begin", {});
      return request<{ ok: boolean }>("POST", "/auth/passkeys/finish", await getPasskey(options));
    },
    onSuccess: () => signedIn(client),
  });
}

export function isProviderURL(value: string) {
  try {
    const url = new URL(value);
    return url.protocol === "https:" && url.username === "" && url.password === "";
  } catch {
    return false;
  }
}

export function useOidcStart() {
  return useMutation({
    ...transient,
    mutationFn: async ({ provider, ...input }: { provider: string; link: boolean; current_password?: string }) => {
      const result = await request<{ url: string }>("POST", `/auth/oidc/${encodeURIComponent(provider)}/start`, input);
      if (!isProviderURL(result.url)) throw new ApiError(502, "The identity provider returned an unexpected address.");
      return result;
    },
    // The provider URL carries state and a PKCE challenge; leaving immediately keeps them out of the page.
    onSuccess: ({ url }) => window.location.assign(url),
  });
}

function useInvalidateFactors() {
  const client = useQueryClient();
  return () => Promise.all([client.invalidateQueries({ queryKey: securityKeys.account }), client.invalidateQueries({ queryKey: securityKeys.passkeys })]);
}

export function useRegenerateRecoveryCodes() {
  const client = useQueryClient();
  return useMutation({
    ...transient,
    mutationFn: (proof: ReauthProof) => request<{ recovery_codes: string[] }>("POST", "/api/account/recovery-codes", proof),
    onSuccess: () => client.invalidateQueries({ queryKey: securityKeys.account }),
  });
}

// Removal signs out other sessions, so their list changes too.
export function useRemoveTotp() {
  const client = useQueryClient();
  const invalidate = useInvalidateFactors();
  return useMutation({
    ...transient,
    mutationFn: (proof: ReauthProof) => request<{ ok: boolean }>("DELETE", "/api/account/totp", proof),
    onSuccess: () => Promise.all([invalidate(), client.invalidateQueries({ queryKey: securityKeys.sessions })]),
  });
}

export function useUnlinkIdentity() {
  const client = useQueryClient();
  return useMutation({
    ...transient,
    mutationFn: ({ provider, proof }: { provider: string; proof: ReauthProof }) => request<{ ok: boolean }>("DELETE", `/api/account/identities/${encodeURIComponent(provider)}`, proof),
    // The session reports whether GitHub is linked.
    onSuccess: () =>
      Promise.all([
        client.invalidateQueries({ queryKey: securityKeys.identities }),
        client.invalidateQueries({ queryKey: securityKeys.sessions }),
        client.invalidateQueries({ queryKey: keys.session }),
      ]),
  });
}

export function useBeginTotp() {
  return useMutation({
    ...transient,
    mutationFn: (currentPassword: string) => request<{ provisioning_uri: string }>("POST", "/api/account/totp", { current_password: currentPassword }),
  });
}

export function useConfirmTotp() {
  const invalidate = useInvalidateFactors();
  return useMutation({
    ...transient,
    meta: { sessionSurvives401: true },
    mutationFn: (code: string) => request<{ recovery_codes: string[] }>("POST", "/api/account/totp/confirm", { code }),
    onSuccess: invalidate,
  });
}

export function useRegisterPasskey() {
  const invalidate = useInvalidateFactors();
  return useMutation({
    ...transient,
    meta: { sessionSurvives401: true },
    mutationFn: async (currentPassword: string) => {
      const options = await request<CreationOptionsJSON>("POST", "/api/account/passkeys/begin", { current_password: currentPassword });
      return request<{ ok: boolean }>("POST", "/api/account/passkeys/finish", await createPasskey(options));
    },
    onSettled: invalidate,
  });
}

export function useRenamePasskey() {
  const client = useQueryClient();
  return useMutation({
    mutationFn: ({ id, name }: { id: string; name: string }) => request<{ ok: boolean }>("PATCH", `/api/account/passkeys/${encodeURIComponent(id)}`, { name }),
    onSettled: () => client.invalidateQueries({ queryKey: securityKeys.passkeys }),
  });
}

export function useDeletePasskey() {
  const invalidate = useInvalidateFactors();
  return useMutation({
    mutationFn: (id: string) => request<{ ok: boolean }>("DELETE", `/api/account/passkeys/${encodeURIComponent(id)}`),
    onSettled: invalidate,
  });
}

export function useUpdateSecondFactorPolicy(teamId: string | null) {
  const client = useQueryClient();
  return useMutation({
    mutationFn: (required: boolean) => request<SecondFactorPolicy>("PUT", policyPath(teamId), { require_second_factor: required }),
    onSuccess: (policy) => client.setQueryData(securityKeys.policy(teamId), policy),
    onSettled: () => client.invalidateQueries({ queryKey: securityKeys.account }),
  });
}

export function useResetSecondFactor() {
  const client = useQueryClient();
  return useMutation({
    mutationFn: (userId: string) => request<{ ok: boolean }>("DELETE", `/api/users/${encodeURIComponent(userId)}/second-factor`),
    onSettled: (_data, _error, userId) => client.invalidateQueries({ queryKey: securityKeys.loginHistory(userId) }),
  });
}

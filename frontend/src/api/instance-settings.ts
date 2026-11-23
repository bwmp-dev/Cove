import { queryOptions, useMutation, useQueryClient } from "@tanstack/react-query";
import { request } from "./client";
import { authConfigQuery } from "./queries";
import { telemetryKeys } from "./telemetry";
import { settingsQuery } from "./teams";

export interface InstanceIntegrations {
  smtp: { host: string; port: string; username: string; from: string; password_set: boolean };
  github: { client_id: string; client_secret_set: boolean; callback_url: string };
}

type IntegrationInput = { smtp: Omit<InstanceIntegrations["smtp"], "password_set"> & { password?: string } } | { github: { client_id: string; client_secret?: string } };

export const integrationsQuery = queryOptions({
  queryKey: ["instance-integrations"],
  queryFn: ({ signal }) => request<InstanceIntegrations>("GET", "/api/settings/integrations", undefined, signal),
});

export function useUpdateIntegrations() {
  const client = useQueryClient();
  return useMutation({
    gcTime: 0,
    mutationFn: (input: IntegrationInput) => request<InstanceIntegrations>("PUT", "/api/settings/integrations", input),
    onSuccess: (settings) => {
      client.setQueryData(integrationsQuery.queryKey, settings);
      void client.invalidateQueries({ queryKey: authConfigQuery.queryKey });
      void client.invalidateQueries({ queryKey: settingsQuery.queryKey });
    },
  });
}

export function useSendTestEmail() {
  return useMutation({ gcTime: 0, mutationFn: (email: string) => request<{ ok: boolean }>("POST", "/api/settings/smtp/test", { email }) });
}

export interface OidcProviderConfiguration {
  display_name: string;
  issuer: string;
  client_id: string;
  scopes: string[];
}

export interface OidcProvider {
  id: string;
  configuration: OidcProviderConfiguration;
  // The exact callback the control plane sends to the provider, derived from COVE_PUBLIC_URL.
  redirect_uri: string;
}

export type OidcProviderInput = OidcProviderConfiguration & { client_secret?: string };

export interface SignInPolicy {
  password_login_disabled: boolean;
  oidc_registration_enabled: boolean;
  oidc_auto_redirect: string;
}

export interface AuditExportConfiguration {
  webhook_url: string;
  syslog_address: string;
  syslog_tls: boolean;
  retention_days: number;
}

export interface AuditExportSettings {
  configuration: AuditExportConfiguration;
  webhook_secret_configured: boolean;
  pending_deliveries: number;
}

export const oidcProvidersQuery = queryOptions({
  queryKey: ["oidc-providers"],
  queryFn: ({ signal }) => request<OidcProvider[]>("GET", "/api/settings/oidc/providers", undefined, signal),
});

export const signInPolicyQuery = queryOptions({
  queryKey: ["sign-in-policy"],
  queryFn: ({ signal }) => request<SignInPolicy>("GET", "/api/settings/sign-in", undefined, signal),
});

export const auditExportQuery = queryOptions({
  queryKey: ["audit-export"],
  queryFn: ({ signal }) => request<AuditExportSettings>("GET", "/api/settings/audit-export", undefined, signal),
  // The backlog drains in the background, so keep the count current while the page is open.
  refetchInterval: 30_000,
});

export function useSaveOidcProvider() {
  const client = useQueryClient();
  return useMutation({
    gcTime: 0,
    mutationFn: ({ id, input }: { id?: string; input: OidcProviderInput }) =>
      request<{ id: string; redirect_uri: string }>(id ? "PUT" : "POST", id ? `/api/settings/oidc/providers/${encodeURIComponent(id)}` : "/api/settings/oidc/providers", input),
    onSettled: () => client.invalidateQueries({ queryKey: oidcProvidersQuery.queryKey }),
  });
}

export function useDeleteOidcProvider() {
  const client = useQueryClient();
  return useMutation({
    mutationFn: (id: string) => request<{ ok: boolean }>("DELETE", `/api/settings/oidc/providers/${encodeURIComponent(id)}`),
    onSettled: () => Promise.all([client.invalidateQueries({ queryKey: oidcProvidersQuery.queryKey }), client.invalidateQueries({ queryKey: signInPolicyQuery.queryKey })]),
  });
}

export function useUpdateSignInPolicy() {
  const client = useQueryClient();
  return useMutation({
    mutationFn: (input: SignInPolicy) => request<SignInPolicy>("PUT", "/api/settings/sign-in", input),
    onSuccess: (policy) => client.setQueryData(signInPolicyQuery.queryKey, policy),
  });
}

export function useUpdateAuditExport() {
  const client = useQueryClient();
  return useMutation({
    gcTime: 0,
    mutationFn: (input: AuditExportConfiguration & { webhook_secret?: string }) => request<AuditExportSettings>("PUT", "/api/settings/audit-export", input),
    onSuccess: (settings) => {
      client.setQueryData(auditExportQuery.queryKey, settings);
      // The retention period is also Telemetry retention's audit days.
      void client.invalidateQueries({ queryKey: telemetryKeys.retention });
      void client.invalidateQueries({ queryKey: telemetryKeys.storageUsage });
    },
  });
}

export interface PanelAccess {
  allowlist: string[];
  // COVE_PANEL_ALLOWLIST on the host replaces the saved list while it is set.
  environment_override: boolean;
  client_address: string;
}

export const panelAccessQuery = queryOptions({
  queryKey: ["panel-access"],
  queryFn: async ({ signal }) => {
    const access = await request<PanelAccess>("GET", "/api/settings/panel-access", undefined, signal);
    return { ...access, allowlist: access.allowlist ?? [] };
  },
});

export function useUpdatePanelAccess() {
  const client = useQueryClient();
  return useMutation({
    mutationFn: (allowlist: string[]) => request<PanelAccess>("PUT", "/api/settings/panel-access", { allowlist }),
    onSuccess: (access) => {
      client.setQueryData(panelAccessQuery.queryKey, { ...access, allowlist: access.allowlist ?? [] });
      void client.invalidateQueries({ queryKey: securityPostureQuery.queryKey });
    },
  });
}

export interface SecurityCheck {
  id: string;
  status: "pass" | "warn";
  message: string;
  remediation: string;
}

export const securityPostureQuery = queryOptions({
  queryKey: ["security-posture"],
  queryFn: ({ signal }) => request<{ checks: SecurityCheck[] | null }>("GET", "/api/settings/security-posture", undefined, signal),
});

export interface OutboundProxySettings {
  // Origins without credentials; whether credentials are saved is reported separately.
  http_proxy: string;
  https_proxy: string;
  no_proxy: string;
  http_credentials_set: boolean;
  https_credentials_set: boolean;
}

// An omitted field keeps the saved value, including credentials; an empty string clears it.
export type OutboundProxyInput = Partial<Pick<OutboundProxySettings, "http_proxy" | "https_proxy" | "no_proxy">>;

export const outboundProxyQuery = queryOptions({
  queryKey: ["outbound-proxy"],
  queryFn: ({ signal }) => request<OutboundProxySettings>("GET", "/api/settings/outbound-proxy", undefined, signal),
});

export function useUpdateOutboundProxy() {
  const client = useQueryClient();
  return useMutation({
    gcTime: 0,
    mutationFn: (input: OutboundProxyInput) => request<OutboundProxySettings>("PUT", "/api/settings/outbound-proxy", input),
    onSuccess: (settings) => client.setQueryData(outboundProxyQuery.queryKey, settings),
  });
}

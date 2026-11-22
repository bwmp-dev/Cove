import { queryOptions, useMutation, useQueryClient } from "@tanstack/react-query";
import { request } from "./client";
import { authConfigQuery } from "./queries";
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

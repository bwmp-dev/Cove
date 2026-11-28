import { queryOptions } from "@tanstack/react-query";
import { request } from "./client";

export type SecretProviderKind = "vault" | "openbao" | "infisical" | "doppler" | "onepassword-connect" | "onepassword-service" | "bitwarden";

// Public metadata only. `available` reports whether the control host has the provider's CLI, not provider health or authentication.
export interface SecretProvider {
  id: string;
  name: string;
  kind: SecretProviderKind;
  url: string;
  project: string;
  environment: string;
  namespace: string;
  available: boolean;
}

export interface SecretProviderInput {
  name: string;
  kind: SecretProviderKind;
  url: string;
  // Blank on update keeps the saved token.
  token: string;
  project?: string;
  environment?: string;
  namespace?: string;
}

export interface BuiltinVariables {
  available: boolean;
  variables: { name: string; description: string }[];
}

export const secretProviderKinds: SecretProviderKind[] = ["vault", "openbao", "infisical", "doppler", "onepassword-connect", "onepassword-service", "bitwarden"];

export const secretProviderLabels: Record<SecretProviderKind, string> = {
  vault: "HashiCorp Vault",
  openbao: "OpenBao",
  infisical: "Infisical",
  doppler: "Doppler",
  "onepassword-connect": "1Password Connect",
  "onepassword-service": "1Password service account",
  bitwarden: "Bitwarden Secrets Manager",
};

// Kinds the control plane reads through the vendor's CLI on the control host.
export const providerCommands: Partial<Record<SecretProviderKind, string>> = { "onepassword-service": "op", bitwarden: "bws" };

export const referenceFormats: Record<SecretProviderKind, { format: string; hint: string }> = {
  vault: { format: "mount/path#field", hint: "A KV version 2 secret and one string field." },
  openbao: { format: "mount/path#field", hint: "A KV version 2 secret and one string field." },
  infisical: { format: "/folder/SECRET_NAME", hint: "The secret's folder path and name. Secrets at the root use /SECRET_NAME." },
  doppler: { format: "SECRET_NAME", hint: "The secret name in the connection's project and config." },
  "onepassword-connect": { format: "vaultID/itemID/fieldID", hint: "Vault, item and field IDs. Field labels are not accepted." },
  "onepassword-service": { format: "vault/item/field", hint: "Cove adds the op:// prefix." },
  bitwarden: { format: "secret UUID", hint: "The secret's ID from Bitwarden Secrets Manager." },
};

export function secretReference(connectionId: string, reference: string) {
  return `{{secret.${connectionId}:${reference}}}`;
}

export function validateReference(reference: string): string | null {
  if (reference === "") return "Enter the secret reference.";
  if (reference.length > 2048) return "Use at most 2048 characters.";
  if (/[{}\r\n]/.test(reference)) return "Remove braces and line breaks.";
  return null;
}

export const secretProviderKeys = {
  list: ["secret-providers"] as const,
  builtins: (workload: string) => ["workloads", workload, "builtin-variables"] as const,
};

export const secretProvidersQuery = queryOptions({
  queryKey: secretProviderKeys.list,
  queryFn: ({ signal }) => request<SecretProvider[]>("GET", "/api/secret-providers", undefined, signal),
});

export const builtinVariablesQuery = (workload: string) =>
  queryOptions({
    queryKey: secretProviderKeys.builtins(workload),
    queryFn: ({ signal }) => request<BuiltinVariables>("GET", `/api/workloads/${encodeURIComponent(workload)}/variables/builtins`, undefined, signal),
    staleTime: 60_000,
  });

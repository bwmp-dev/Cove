import { queryOptions, useMutation, useQueryClient } from "@tanstack/react-query";
import { request } from "./client";
import { keys } from "./queries";
import type { DatabaseEngineId, DatabaseResource } from "./types";
import type { SettingsState } from "../lib/databases";

export interface DatabaseVersion {
  version: string;
  image: string;
  variant: string;
  warning?: string;
}

export interface DatabaseEngine {
  engine: DatabaseEngineId;
  name: string;
  version: string;
  versions: string[];
  // Details for the versions other than the current default.
  version_details?: DatabaseVersion[];
  image: string;
  service: string;
  port: number;
  template_id: string;
  template_revision: string;
  logical_backup: boolean;
  minimum?: { cpus?: number; memory_mb?: number; disk_mb?: number };
  docs?: string;
}

export interface DatabaseDetails {
  revision: string;
  maintenance_locked: boolean;
  // False until the database has been deployed successfully and while it is manually stopped.
  maintenance_available?: boolean;
  database: DatabaseResource;
  host: string;
  user: string;
  name: string;
  exports: { key: string; secret: boolean }[];
  logical_backup: boolean;
  connection_required: boolean;
  admin_tools: string[];
  settings_state?: SettingsState;
  can_cutover?: boolean;
  dependents?: DatabaseDependent[];
  // Bound resources the viewer cannot read; only the count is disclosed.
  hidden_dependents?: number;
  copy_targets?: DatabaseVersion[];
  extension_options?: string[];
  // Public PEM CA for TLS clients. Never contains a private key.
  tls_ca?: string;
}

export interface DatabaseDependent {
  id: string;
  name: string;
  needs_redeploy: boolean;
}

export interface Endpoint {
  host: string;
  port: number;
  user: string;
  password: string;
  database: string;
  url: string;
}

export interface DatabaseCredentials extends Endpoint {
  ca_certificate?: string;
  pooler?: Endpoint;
  public?: Endpoint;
}

export type MaintenanceKind = "rotate" | "extension" | "upgrade";

export interface MaintenanceTask {
  id: string;
  kind: MaintenanceKind;
  resource_id: string;
  source_id: string;
  state: "pending" | "failed" | "succeeded" | "discarded";
  operation_state: string;
  error: string;
  created_at: string;
  cutover_confirmed?: boolean;
  // Exact for the caller: a verified, unconfirmed copy plus team admin and edit access to source and target.
  can_cutover?: boolean;
}

export interface MaintenanceRequest {
  kind: MaintenanceKind;
  revision: string;
  confirm: true;
  extension?: string;
  version?: string;
  name?: string;
  enabled?: boolean;
  // Required for upgrade copies: a snapshot taken while applications write is not guaranteed consistent.
  confirm_writes_stopped?: true;
}

export interface PreflightResult {
  revision: string;
  runtime: { version: string; extensions: string[] | null; feature_compatibility?: string; arch: string };
  copy_targets: DatabaseVersion[];
  backup_required: boolean;
  scope: string;
}

export interface PublicAccessInput {
  hostname: string;
  bind_address: string;
  port: number;
  certificate: string;
  private_key: string;
  ca_certificate?: string;
}

export type PublicAccessRequest = ({ enabled: true } & PublicAccessInput) | { enabled: false };

export interface DatabaseCreateInput {
  name: string;
  node_id: string;
  project_id: string;
  environment_id?: string;
  kind: "database";
  database: { engine: DatabaseEngineId; version: string; template_revision: string };
}

const path = (id: string) => `/api/workloads/${encodeURIComponent(id)}/database`;

export const databaseKeys = {
  engines: ["database-engines"] as const,
  details: (id: string) => ["workloads", id, "database"] as const,
  maintenance: (id: string) => ["workloads", id, "database", "maintenance"] as const,
};

export const databaseEnginesQuery = queryOptions({
  queryKey: databaseKeys.engines,
  queryFn: ({ signal }) => request<{ engines: DatabaseEngine[] }>("GET", "/api/database-engines", undefined, signal),
  select: (data) => data.engines,
  staleTime: 5 * 60_000,
});

export const databaseDetailsQuery = (id: string) =>
  queryOptions({
    queryKey: databaseKeys.details(id),
    queryFn: ({ signal }) => request<DatabaseDetails>("GET", path(id), undefined, signal),
  });

export const databaseMaintenanceQuery = (id: string) =>
  queryOptions({
    queryKey: databaseKeys.maintenance(id),
    queryFn: ({ signal }) => request<MaintenanceTask[]>("GET", `${path(id)}/maintenance`, undefined, signal),
    // Pending work advances through ordinary operations; settled history refreshes slowly.
    refetchInterval: (query) => (query.state.data?.some((task) => task.state === "pending") ? 3_000 : 30_000),
  });

export function useCreateDatabase() {
  const client = useQueryClient();
  return useMutation({
    mutationFn: (input: DatabaseCreateInput) => request<{ id: string }>("POST", "/api/workloads", input),
    onSuccess: () => client.invalidateQueries({ queryKey: keys.workloads }),
  });
}

// The response holds live credentials: it is never cached and lives only in the calling component's state.
export function useRevealCredentials(id: string) {
  return useMutation({
    gcTime: 0,
    mutationFn: () => request<DatabaseCredentials>("GET", `${path(id)}/credentials`),
  });
}

function invalidateDatabase(client: ReturnType<typeof useQueryClient>, ids: string[]) {
  return Promise.all([
    client.invalidateQueries({ queryKey: keys.operations }),
    client.invalidateQueries({ queryKey: keys.workloads }),
    ...ids.map((id) => client.invalidateQueries({ queryKey: keys.workload(id) })),
  ]);
}

export function useStartMaintenance(id: string) {
  const client = useQueryClient();
  return useMutation({
    mutationFn: (input: MaintenanceRequest) => request<{ id: string; resource_id: string }>("POST", `${path(id)}/maintenance`, input),
    onSettled: (data) => invalidateDatabase(client, data ? [id, data.resource_id] : [id]),
  });
}

export function useRetryMaintenance(id: string) {
  const client = useQueryClient();
  return useMutation({
    // Copy retries take a new snapshot, so they need the writes-stopped acknowledgement again; other kinds send {}.
    mutationFn: ({ task, writesStopped = false }: { task: string; writesStopped?: boolean }) =>
      request<{ id: string; resource_id: string }>("POST", `${path(id)}/maintenance/${encodeURIComponent(task)}/retry`, writesStopped ? { confirm_writes_stopped: true } : {}),
    onSettled: (data) => invalidateDatabase(client, data ? [id, data.resource_id] : [id]),
  });
}

export function useDiscardCopy(id: string) {
  const client = useQueryClient();
  return useMutation({
    mutationFn: ({ task, name }: { task: string; name: string }) => request<{ ok: true }>("POST", `${path(id)}/maintenance/${encodeURIComponent(task)}/discard`, { confirm: true, name }),
    onSettled: () => invalidateDatabase(client, [id]),
  });
}

export function useCutover(id: string) {
  const client = useQueryClient();
  return useMutation({
    mutationFn: ({ task, revision }: { task: string; revision: string }) =>
      request<{ ok: true; source_retained: true; redeploy_required?: boolean; resource_id?: string }>("POST", `${path(id)}/maintenance/${encodeURIComponent(task)}/cutover`, { confirm: true, confirm_writes_stopped: true, revision }),
    onSettled: (data) => invalidateDatabase(client, data?.resource_id ? [id, data.resource_id] : [id]),
  });
}

// Read-only runtime inspection; it creates nothing.
export function usePreflight(id: string) {
  return useMutation({
    mutationFn: (input: { revision: string; version?: string }) => request<PreflightResult>("POST", `${path(id)}/preflight`, input),
  });
}

export function usePoolerSetting(id: string) {
  const client = useQueryClient();
  return useMutation({
    mutationFn: (input: { enabled: boolean; revision: string }) => request<{ id: string }>("POST", `${path(id)}/pooler`, { ...input, confirm: true }),
    onSettled: () => invalidateDatabase(client, [id]),
  });
}

// The request carries the TLS private key. Mutation variables stay in the cache after a failure, so
// the mutation takes none: mutationFn reads the body from the caller's state when it runs.
export function usePublicAccess(id: string, body: () => PublicAccessRequest & { revision: string }) {
  const client = useQueryClient();
  return useMutation({
    gcTime: 0,
    mutationFn: () => request<{ id: string }>("POST", `${path(id)}/public-access`, { ...body(), confirm: true }),
    onSettled: () => invalidateDatabase(client, [id]),
  });
}

import { queryOptions, useMutation, useQueryClient } from "@tanstack/react-query";
import { request } from "./client";

export interface PanelBackupConfig {
  enabled: boolean;
  interval_hours: number;
  keep_last: number;
  keep_days: number;
  max_bytes: number;
  storage_id: string;
}

export interface PanelSnapshot {
  id: string;
  created_at: string;
  version: string;
  revision: string;
  bytes: number;
  verified: boolean;
  offsite: boolean;
  origin: string;
}

export type PanelJobAction = "backup" | "restore" | "resume_updates";
export type PanelJobState = "queued" | "running" | "succeeded" | "failed";

export interface PanelJob {
  id: string;
  action: PanelJobAction;
  snapshot_id: string;
  state: PanelJobState;
  requested_at: string;
  finished_at: string | null;
  error: string;
}

export interface PanelRecovery {
  config: PanelBackupConfig;
  password_set: boolean;
  agent_id: string;
  checked_at: string | null;
  last_error: string;
  updates_paused: boolean;
  pending_recovery: boolean;
  offsite_pending: boolean;
  public_origin: string;
  snapshots: PanelSnapshot[] | null;
  jobs: PanelJob[] | null;
}

// A blank password with the saved storage location keeps the saved password; the API never returns it.
export type PanelBackupInput = PanelBackupConfig & { password?: string };

export type PanelRecoveryAction = { action: "backup" } | { action: "restore"; snapshot_id: string; confirm: true } | { action: "resume_updates"; confirm: true };

const settling = new Set<PanelJobState>(["queued", "running"]);

export const panelRecoveryQuery = queryOptions({
  queryKey: ["panel-recovery"],
  queryFn: ({ signal }) => request<PanelRecovery>("GET", "/api/settings/panel-backup", undefined, signal),
  refetchInterval: (query) => {
    const data = query.state.data;
    return data && (data.pending_recovery || data.jobs?.some((job) => settling.has(job.state))) ? 5_000 : 30_000;
  },
});

export function useSavePanelBackup() {
  const client = useQueryClient();
  return useMutation({
    gcTime: 0,
    mutationFn: (input: PanelBackupInput) => request<PanelRecovery>("PUT", "/api/settings/panel-backup", input),
    onSuccess: (recovery) => client.setQueryData(panelRecoveryQuery.queryKey, recovery),
  });
}

export function usePanelRecoveryAction() {
  const client = useQueryClient();
  return useMutation({
    gcTime: 0,
    mutationFn: (action: PanelRecoveryAction) => request<{ id: string; state: PanelJobState }>("POST", "/api/settings/panel-backup/actions", action),
    onSettled: () => client.invalidateQueries({ queryKey: panelRecoveryQuery.queryKey }),
  });
}

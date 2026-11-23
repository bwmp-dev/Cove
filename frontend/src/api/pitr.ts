import { queryOptions } from "@tanstack/react-query";
import { request } from "./client";

export interface PITRSettings {
  enabled: boolean;
  service: string;
  interval_hours: number;
  keep_last: number;
  retention_days: number;
  max_bytes: number;
}

// available_to is absent when no continuous recovery window is known for the base.
export interface PITRBase {
  id: string;
  created_at: string;
  available_to?: string;
  bytes: number;
  postgres_major: number;
}

export type PITRState = "not_configured" | "starting" | "configuring" | "capturing" | "waiting_for_base" | "base_backup" | "paused" | "disabled" | "error";

export interface PITRStatus {
  enabled: boolean;
  state: PITRState | (string & {});
  checked_at: string;
  measured_at?: string;
  last_archive_at?: string;
  bytes: number;
  objects: number;
  receiver: boolean;
  needs_base: boolean;
  gaps: boolean;
  last_error?: string;
  bases: PITRBase[] | null;
}

export type PITRInfo =
  | { configured: false; supported: boolean; settings: null; status: null }
  | { configured: true; supported: boolean; settings: PITRSettings; storage_id: string; operation_id: string | null; status: PITRStatus };

export interface PITRRestoreInput { name: string; base: string; timestamp: string }

// The copy's generated administrator credentials are returned only in this creation response; later reads never include them.
export interface PITRRestoreResult { id: string; resource_id: string; credentials: { username: string; password: string } }

const settlingStates = new Set(["starting", "configuring", "base_backup"]);

export function pitrQuery(workloadId: string, busy: boolean) {
  return queryOptions({
    queryKey: ["pitr", workloadId],
    queryFn: ({ signal }) => request<PITRInfo>("GET", `/api/workloads/${workloadId}/pitr`, undefined, signal),
    refetchInterval: (query) => {
      const data = query.state.data;
      return busy || (data?.configured && settlingStates.has(data.status.state)) ? 3_000 : 15_000;
    },
  });
}

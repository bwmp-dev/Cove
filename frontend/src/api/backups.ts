import { queryOptions } from "@tanstack/react-query";
import type { DatabaseBackup, VolumeSnapshotPolicy } from "../lib/backups";
import { request } from "./client";

export interface BackupPolicy { drill_interval_hours?: number; database?: DatabaseBackup; storage_id?: string; repository: string; keep_last: number; keep_daily?: number; keep_weekly?: number; keep_monthly?: number; interval_hours: number; ca_certificate?: string; volume_snapshot?: VolumeSnapshotPolicy | null }

export function backupPolicyQuery(workloadId: string) {
  return queryOptions({ queryKey: ["backup-policy", workloadId], queryFn: ({ signal }) => request<{ backup: BackupPolicy | null }>("GET", `/api/workloads/${workloadId}/backup-policy`, undefined, signal) });
}

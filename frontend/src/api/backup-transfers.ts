import { queryOptions, useMutation, useQueryClient } from "@tanstack/react-query";
import { selectedTeam } from "../lib/team";
import { request, uploadFile } from "./client";

export interface TransferLimits { backup_bytes: number; min_backup_bytes: number; max_backup_bytes: number }
export interface ImportedBackup { backup_id: string; size: number; sha256: string; verified: boolean }

export const transferLimitsQuery = queryOptions({
  queryKey: ["transfer-limits"],
  queryFn: ({ signal }) => request<TransferLimits>("GET", "/api/settings/transfer-limits", undefined, signal),
});

export function resourceTransferLimitsQuery(workloadId: string) {
  return queryOptions({
    queryKey: ["transfer-limits", workloadId],
    queryFn: ({ signal }) => request<TransferLimits>("GET", `/api/workloads/${workloadId}/backups/transfer-limits`, undefined, signal),
  });
}

export function useUpdateTransferLimits() {
  const client = useQueryClient();
  return useMutation({
    gcTime: 0,
    mutationFn: (backupBytes: number) => request<TransferLimits>("PUT", "/api/settings/transfer-limits", { backup_bytes: backupBytes }),
    onSuccess: (limits) => client.setQueryData(transferLimitsQuery.queryKey, limits),
  });
}

// A plain same-origin link, so the browser streams the download to disk with its own progress and errors.
// The team ID is not a credential: it selects the same team context as the X-Cove-Team header, and the session cookie authorizes the request.
export function backupDownloadUrl(workloadId: string, backupId: string) {
  const path = `/api/workloads/${encodeURIComponent(workloadId)}/backups/${encodeURIComponent(backupId)}/download`;
  const team = selectedTeam();
  return team ? `${path}?${new URLSearchParams({ team_id: team }).toString()}` : path;
}

export type ImportSource = { kind: "upload"; file: File } | { kind: "s3"; storage_id: string; object_key: string };

export function importBackup(workloadId: string, source: ImportSource, signal?: AbortSignal) {
  if (source.kind === "upload") return uploadFile<ImportedBackup>(`/api/workloads/${workloadId}/backups/import`, source.file, signal);
  return request<ImportedBackup>("POST", `/api/workloads/${workloadId}/backups/import-s3`, { storage_id: source.storage_id, object_key: source.object_key }, signal);
}

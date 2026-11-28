import { queryOptions, useMutation, useQueryClient } from "@tanstack/react-query";
import { withBasePath } from "../lib/base-path";
import { parentPath } from "../lib/files";
import { selectedTeam } from "../lib/team";
import { request, uploadFile } from "./client";

export interface FileTarget {
  kind: "container" | "volume";
  id: string;
  name: string;
  state: string;
}

export interface FileEntry {
  name: string;
  kind: "file" | "directory" | "link" | "other";
  size: number;
}

export interface FileResponse {
  targets: FileTarget[];
  entries: FileEntry[];
  data: string | null;
  revision: string;
  limited: boolean;
  max_bytes: number;
  // Present on metadata responses: the exact size of a file whose revision was read without its contents.
  size?: number;
}

export interface FileLimits {
  file_bytes: number;
  min_file_bytes: number;
  max_file_bytes: number;
  editor_bytes: number;
}

export interface FileUploadReceipt {
  revision: string;
  size: number;
}

export interface FileUpload extends FileLocation {
  file: File;
  // Empty creates the file and fails if it already exists; otherwise the revision inspected before replacing it.
  revision: string;
  signal?: AbortSignal;
}

export interface FileLocation {
  kind: FileTarget["kind"];
  target: string;
  path: string;
}

export interface FileWrite extends FileLocation {
  data: string;
  // Empty creates the file and fails if it already exists; otherwise the revision read before editing.
  revision: string;
}

function filesPath(workloadId: string, suffix = "") {
  return `/api/workloads/${encodeURIComponent(workloadId)}/files${suffix}`;
}

function locationQuery({ kind, target, path }: FileLocation) {
  return new URLSearchParams({ kind, target, path }).toString();
}

// File names and contents can be sensitive application data: nothing here is kept after the page stops using it.
const ephemeral = { staleTime: 0, gcTime: 0, retry: false } as const;

export const fileKeys = {
  targets: (workloadId: string) => ["workloads", workloadId, "files", "targets"] as const,
  list: (workloadId: string, location: FileLocation) => ["workloads", workloadId, "files", "list", location.kind, location.target, location.path] as const,
  limits: (workloadId: string) => ["workloads", workloadId, "files", "limits"] as const,
  // The listing of the folder that contains a file.
  folder: (workloadId: string, location: FileLocation) => fileKeys.list(workloadId, { ...location, path: parentPath(location.path) }),
};

export const fileTargetsQuery = (workloadId: string) =>
  queryOptions({
    queryKey: fileKeys.targets(workloadId),
    queryFn: ({ signal }) => request<FileResponse>("GET", filesPath(workloadId, "/targets"), undefined, signal),
    ...ephemeral,
  });

export const fileListQuery = (workloadId: string, location: FileLocation) =>
  queryOptions({
    queryKey: fileKeys.list(workloadId, location),
    queryFn: ({ signal }) => request<FileResponse>("GET", `${filesPath(workloadId)}?${locationQuery(location)}`, undefined, signal),
    ...ephemeral,
  });

export function useReadFile(workloadId: string) {
  return useMutation({ gcTime: 0, mutationFn: (location: FileLocation) => request<FileResponse>("GET", `${filesPath(workloadId, "/content")}?${locationQuery(location)}`) });
}

export const fileLimitsQuery = (workloadId: string) =>
  queryOptions({
    queryKey: fileKeys.limits(workloadId),
    queryFn: ({ signal }) => request<FileLimits>("GET", filesPath(workloadId, "/limits"), undefined, signal),
    ...ephemeral,
  });

// Reads a file's size and revision without its contents, so a large or binary file can be replaced safely.
export function useFileMetadata(workloadId: string) {
  return useMutation({ gcTime: 0, mutationFn: (location: FileLocation) => request<FileResponse>("GET", `${filesPath(workloadId, "/metadata")}?${locationQuery(location)}`) });
}

// A plain same-origin link, so the browser streams the file to disk with its own progress and errors.
// The team ID is not a credential: it selects the same team context as the X-Cove-Team header, and the session cookie authorizes the request.
export function fileDownloadUrl(workloadId: string, location: FileLocation) {
  const query = new URLSearchParams({ kind: location.kind, target: location.target, path: location.path });
  const team = selectedTeam();
  if (team) query.set("team_id", team);
  return withBasePath(`${filesPath(workloadId, "/download")}?${query.toString()}`);
}

export function useWriteFile(workloadId: string) {
  return useMutation({ gcTime: 0, mutationFn: (body: FileWrite) => request<FileResponse>("PUT", filesPath(workloadId, "/content"), body) });
}

// Sends the File itself as the request body, so the browser streams it from disk without reading it into memory.
export function useUploadFile(workloadId: string) {
  const client = useQueryClient();
  return useMutation({
    gcTime: 0,
    mutationFn: ({ file, revision, signal, ...location }: FileUpload) =>
      uploadFile<FileUploadReceipt>(`${filesPath(workloadId, "/upload")}?${locationQuery(location)}`, file, signal, revision ? { "X-Cove-Revision": revision } : {}),
    onSuccess: (_receipt, { kind, target, path }) => client.invalidateQueries({ queryKey: fileKeys.folder(workloadId, { kind, target, path }) }),
  });
}

export const fileTransferLimitsQuery = queryOptions({
  queryKey: ["file-transfer-limits"],
  queryFn: ({ signal }) => request<FileLimits>("GET", "/api/settings/file-transfer-limits", undefined, signal),
});

export function useUpdateFileTransferLimits() {
  const client = useQueryClient();
  return useMutation({
    gcTime: 0,
    mutationFn: (fileBytes: number) => request<FileLimits>("PUT", "/api/settings/file-transfer-limits", { file_bytes: fileBytes }),
    onSuccess: (limits) => client.setQueryData(fileTransferLimitsQuery.queryKey, limits),
  });
}

export function useDeleteFile(workloadId: string) {
  return useMutation({ gcTime: 0, mutationFn: (body: FileLocation) => request<FileResponse>("DELETE", filesPath(workloadId, "/content"), body) });
}

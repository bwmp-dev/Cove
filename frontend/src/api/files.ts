import { queryOptions, useMutation } from "@tanstack/react-query";
import { request, requestBlob } from "./client";

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

export function useDownloadFile(workloadId: string) {
  return useMutation({ gcTime: 0, mutationFn: (location: FileLocation) => requestBlob(`${filesPath(workloadId, "/download")}?${locationQuery(location)}`) });
}

export function useWriteFile(workloadId: string) {
  return useMutation({ gcTime: 0, mutationFn: (body: FileWrite) => request<FileResponse>("PUT", filesPath(workloadId, "/content"), body) });
}

export function useUploadFile(workloadId: string) {
  return useMutation({ gcTime: 0, mutationFn: (body: FileWrite) => request<FileResponse>("POST", filesPath(workloadId, "/upload"), body) });
}

export function useDeleteFile(workloadId: string) {
  return useMutation({ gcTime: 0, mutationFn: (body: FileLocation) => request<FileResponse>("DELETE", filesPath(workloadId, "/content"), body) });
}

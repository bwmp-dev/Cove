import { queryOptions, useMutation, useQueryClient } from "@tanstack/react-query";
import { request } from "./client";
import type { ConnectionGraph, ConnectionInput } from "../lib/connections";

const graphKey = (projectId: string, environmentId?: string) => ["private-connections", projectId, environmentId ?? ""] as const;

export function connectionGraphQuery(projectId: string, environmentId?: string) {
  const query = environmentId ? `?environment=${encodeURIComponent(environmentId)}` : "";
  return queryOptions({
    queryKey: graphKey(projectId, environmentId),
    queryFn: ({ signal }) => request<ConnectionGraph>("GET", `/api/projects/${encodeURIComponent(projectId)}/connections${query}`, undefined, signal),
    // Workers confirm changes asynchronously; poll while anything is still settling.
    refetchInterval: (query) => (query.state.data?.connections.some((connection) => connection.state === "pending" || connection.state === "revoking") ? 3000 : 30000),
  });
}

function useInvalidate() {
  const client = useQueryClient();
  return () => client.invalidateQueries({ queryKey: ["private-connections"] });
}

export function useCreateConnection() {
  const invalidate = useInvalidate();
  return useMutation({ mutationFn: (input: ConnectionInput) => request<{ id: string }>("POST", "/api/private-connections", input), onSettled: invalidate });
}

export function useUpdateConnection() {
  const invalidate = useInvalidate();
  return useMutation({
    mutationFn: ({ id, ...input }: Omit<ConnectionInput, "source_id" | "target_id"> & { id: string }) => request<{ id: string }>("PUT", `/api/private-connections/${encodeURIComponent(id)}`, input),
    onSettled: invalidate,
  });
}

export function useRemoveConnection() {
  const invalidate = useInvalidate();
  return useMutation({ mutationFn: (id: string) => request<{ id: string }>("DELETE", `/api/private-connections/${encodeURIComponent(id)}`), onSettled: invalidate });
}

export function useRetryConnection() {
  const invalidate = useInvalidate();
  return useMutation({ mutationFn: (id: string) => request<{ id: string }>("POST", `/api/private-connections/${encodeURIComponent(id)}/retry`), onSettled: invalidate });
}

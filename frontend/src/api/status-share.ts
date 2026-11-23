import { queryOptions } from "@tanstack/react-query";
import { request, requestPublic } from "./client";

// ownership_required: the link has no currently authorised creator (deactivated, lacking current project edit rights or
// required two-step verification, or never recorded), so the public page and badge are not served. Creating or rotating
// the link makes the current account its creator.
export type StatusShare =
  | { enabled: false; title?: string; expires_at?: string; created_at?: string; ownership_required?: boolean }
  | { enabled: true; title: string; expires_at: string; created_at: string; ownership_required?: boolean };

// Returned once when a link is created or rotated. The server keeps only a hash of the token.
export interface CreatedStatusShare {
  title: string;
  expires_at: string;
  dashboard_url: string;
  badge_url: string;
}

export type PublicDeploymentState = "succeeded" | "failed" | "pending" | "stopped" | "unknown";

export interface PublicProjectStatus {
  title: string;
  state: PublicDeploymentState;
  counts: Record<"total" | PublicDeploymentState, number>;
  as_of: string;
  description: string;
}

export const statusShareQuery = (projectId: string) =>
  queryOptions({
    queryKey: ["projects", projectId, "status-share"] as const,
    queryFn: ({ signal }) => request<StatusShare>("GET", `/api/projects/${encodeURIComponent(projectId)}/status-share`, undefined, signal),
  });

export function createStatusShare(projectId: string, title: string, days: number) {
  return request<CreatedStatusShare>("POST", `/api/projects/${encodeURIComponent(projectId)}/status-share`, { title, days, acknowledge: true });
}

export function revokeStatusShare(projectId: string) {
  return request<{ ok: boolean }>("DELETE", `/api/projects/${encodeURIComponent(projectId)}/status-share`);
}

// The share token is in the key only for the lifetime of the page; gcTime 0 drops it as soon as the page closes.
export const publicProjectStatusQuery = (share: string) =>
  queryOptions({
    queryKey: ["public-status", share] as const,
    queryFn: ({ signal }) => requestPublic<PublicProjectStatus>(`/api/public/projects/${encodeURIComponent(share)}`, signal),
    gcTime: 0,
    retry: false,
    refetchOnWindowFocus: false,
    refetchOnReconnect: false,
  });

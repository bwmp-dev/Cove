import { queryOptions } from "@tanstack/react-query";
import { request } from "./client";

// Names only. File and asset contents are returned by the audited export, never by this endpoint.
export interface ComposeBundleSummary {
  enabled: boolean;
  available: boolean;
  files: string[];
  assets: string[];
  paths: string[];
  profiles: string[];
}

export interface ComposeBundle {
  files?: { path: string; content: string }[];
  assets?: Record<string, string>;
  paths?: string[];
  profiles?: string[];
}

export const composeBundleKey = (workload: string) => ["workloads", workload, "compose-bundle"] as const;

const path = (workload: string) => `/api/workloads/${encodeURIComponent(workload)}/compose-bundle`;

export const composeBundleQuery = (workload: string) =>
  queryOptions({
    queryKey: composeBundleKey(workload),
    queryFn: ({ signal }) => request<ComposeBundleSummary>("GET", path(workload), undefined, signal),
  });

export function saveComposeBundle(workload: string, bundle: ComposeBundle | null) {
  return request<ComposeBundleSummary>("PUT", path(workload), { bundle });
}

export function exportComposeBundle(workload: string) {
  return request<{ bundle: ComposeBundle | null }>("POST", `${path(workload)}/export`);
}

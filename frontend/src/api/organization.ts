import { keepPreviousData, queryOptions, useMutation, useQueryClient } from "@tanstack/react-query";
import { request } from "./client";
import { keys } from "./queries";
import type { Permission } from "./types";

export interface BrowserResource {
  id: string;
  name: string;
  node_id: string;
  node: string;
  kind: string;
  project_id: string;
  project: string;
  environment_id: string;
  environment: string;
  permissions: Permission[];
  tags: string[];
  pinned: boolean;
}

export interface BrowserPage {
  items: BrowserResource[];
  page: number;
  page_size: number;
  total: number;
  has_more: boolean;
}

export interface BrowserFilters {
  q: string;
  tag: string;
  project_id: string;
  environment_id: string;
  node_id: string;
  pinned: boolean;
  page: number;
  page_size: number;
}

export type PinKind = "project" | "environment" | "workload";

export interface Pin {
  kind: PinKind;
  id: string;
}

export const bulkActions = ["deploy", "restart", "stop"] as const;
export type BulkAction = (typeof bulkActions)[number];

export interface BulkResult {
  id: string;
  status: number;
  operation_id?: string;
  error?: string;
}

export interface ActivityItem {
  kind: "operation" | "configuration";
  id: string;
  action: string;
  state: string;
  name: string;
  path: string;
  created_at: string;
}

export interface OnboardingStep {
  key: string;
  name: string;
  complete: boolean;
  path: string;
}

export interface Onboarding {
  dismissed: boolean;
  complete: boolean;
  can_enroll_node: boolean;
  steps: OnboardingStep[];
}

const organizationKeys = {
  browser: (filters: BrowserFilters) => ["resource-browser", filters] as const,
  pins: ["pins"] as const,
  activity: ["activity"] as const,
  onboarding: ["onboarding"] as const,
};

export function browserSearch(filters: BrowserFilters) {
  const params = new URLSearchParams();
  if (filters.q) params.set("q", filters.q);
  if (filters.tag) params.set("tag", filters.tag);
  if (filters.project_id) params.set("project_id", filters.project_id);
  if (filters.environment_id) params.set("environment_id", filters.environment_id);
  if (filters.node_id) params.set("node_id", filters.node_id);
  if (filters.pinned) params.set("pinned", "true");
  params.set("page", String(filters.page));
  params.set("page_size", String(filters.page_size));
  return params.toString();
}

export function resourceBrowserQuery(filters: BrowserFilters) {
  return queryOptions({
    queryKey: organizationKeys.browser(filters),
    queryFn: ({ signal }) => request<BrowserPage>("GET", `/api/resource-browser?${browserSearch(filters)}`, undefined, signal),
    placeholderData: keepPreviousData,
  });
}

export const pinsQuery = queryOptions({
  queryKey: organizationKeys.pins,
  queryFn: async ({ signal }) => (await request<{ items: Pin[] }>("GET", "/api/pins", undefined, signal)).items,
});

export const activityQuery = queryOptions({
  queryKey: organizationKeys.activity,
  queryFn: async ({ signal }) => (await request<{ items: ActivityItem[] }>("GET", "/api/activity", undefined, signal)).items,
  refetchInterval: 30_000,
});

export const onboardingQuery = queryOptions({
  queryKey: organizationKeys.onboarding,
  queryFn: ({ signal }) => request<Onboarding>("GET", "/api/onboarding", undefined, signal),
});

export function useSetPin() {
  const client = useQueryClient();
  return useMutation({
    mutationFn: (pin: Pin & { pinned: boolean }) => request<{ items: Pin[] }>("PUT", "/api/pins", pin),
    onSuccess: (response) => client.setQueryData(organizationKeys.pins, response.items),
    onSettled: () => client.invalidateQueries({ queryKey: ["resource-browser"] }),
  });
}

export function useSetTags() {
  const client = useQueryClient();
  return useMutation({
    mutationFn: ({ id, tags }: { id: string; tags: string[] }) => request<{ tags: string[] }>("PUT", `/api/workloads/${encodeURIComponent(id)}/tags`, { tags }),
    onSettled: () => client.invalidateQueries({ queryKey: ["resource-browser"] }),
  });
}

export function useBulkAction() {
  const client = useQueryClient();
  return useMutation({
    mutationFn: ({ ids, action }: { ids: string[]; action: BulkAction }) => request<{ items: BulkResult[] }>("POST", "/api/bulk-actions", { ids, action, acknowledge: true }),
    onSettled: () => Promise.all([client.invalidateQueries({ queryKey: keys.operations }), client.invalidateQueries({ queryKey: organizationKeys.activity })]),
  });
}

export function useDismissOnboarding() {
  const client = useQueryClient();
  return useMutation({
    mutationFn: (dismissed: boolean) => request<Onboarding>("PUT", "/api/onboarding", { dismissed }),
    onSuccess: (response) => client.setQueryData(organizationKeys.onboarding, response),
  });
}

import { queryOptions, useMutation, useQueryClient } from "@tanstack/react-query";
import { parseIssuedCredential, parsePreviewImages, type IssuedCredential } from "../lib/preview-images";
import { request } from "./client";
import { previewKeys } from "./previews";

export interface PreviewImagesConfiguration {
  resource_ids: string[];
  recipe_version: number;
  // False whenever the credential no longer matches its saved scope: expiry, a recipe or resource configuration change
  // (including Compose services or registry), a revoked owner or an unavailable worker.
  matches_scope: boolean;
  expires_at: string;
  created_at: string;
  last_received_at: string | null;
}

export interface PreviewImagesResource {
  id: string;
  name: string;
  services: string[];
  registry: { id: string; server: string } | null;
  eligible: boolean;
  reason: string;
}

export interface PreviewImagesStatus {
  configuration: PreviewImagesConfiguration | null;
  resources: PreviewImagesResource[];
  endpoint: string;
  can_manage: boolean;
  setup_reason: string;
  max_resources: number;
  max_preview_keys: number;
  max_deliveries: number;
  used_preview_keys: number;
  retained_deliveries: number;
}

export interface PreviewImagesInput {
  resource_ids: string[];
  days: number;
}

const path = (projectId: string) => `/api/projects/${encodeURIComponent(projectId)}/preview-images`;

// CI submissions arrive from outside the panel, so the page rechecks; polling stops after an error until Retry.
export const previewImagesQuery = (projectId: string) =>
  queryOptions({
    queryKey: previewKeys.images(projectId),
    queryFn: async ({ signal }) => parsePreviewImages(await request<unknown>("GET", path(projectId), undefined, signal)),
    refetchInterval: (query) => (query.state.status === "error" ? false : 30_000),
    retry: false,
  });

// The one-time token is handed to onToken inside the request and never becomes mutation data, so neither the query nor
// the mutation cache sees it. A reply that cannot be read throws after the server may already have issued a credential.
export function useIssuePreviewImagesCredential(projectId: string, onToken: (credential: IssuedCredential) => void) {
  const client = useQueryClient();
  return useMutation({
    gcTime: 0,
    mutationFn: async (input: PreviewImagesInput) => {
      onToken(parseIssuedCredential(await request<unknown>("PUT", path(projectId), input)));
    },
    onSettled: () => client.invalidateQueries({ queryKey: previewKeys.images(projectId), exact: true }),
  });
}

export function useRevokePreviewImagesCredential(projectId: string) {
  const client = useQueryClient();
  return useMutation({
    gcTime: 0,
    mutationFn: async () => {
      await request<unknown>("DELETE", path(projectId));
    },
    onSettled: () => client.invalidateQueries({ queryKey: previewKeys.images(projectId), exact: true }),
  });
}

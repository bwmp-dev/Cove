import { queryOptions, useMutation, useQueryClient } from "@tanstack/react-query";
import { request } from "./client";
import { keys } from "./queries";

export type ImageUpdateMode = "notify" | "auto";

export interface ImageUpdateRule {
  service: string;
  // Empty follows the configured mutable tag; otherwise a SemVer constraint the control plane validates.
  range: string;
}

export interface ImageUpdateConfiguration {
  enabled: boolean;
  mode: ImageUpdateMode;
  rules: ImageUpdateRule[];
  window_start: string;
  window_end: string;
}

export interface ImageUpdateService {
  service: string;
  image: string;
  // A literal tag reference without a digest or variable.
  mutable: boolean;
}

export interface ImageUpdateCandidate {
  service: string;
  current: string;
  target: string;
}

export interface ImageUpdateResult {
  // "current", "available" or "unknown"; any other value is shown as unknown.
  state: string;
  message?: string;
  candidates: ImageUpdateCandidate[];
}

export interface ImageUpdatePolicyStatus {
  configuration: ImageUpdateConfiguration | null;
  services: ImageUpdateService[];
  eligible: boolean;
  unavailable_reason: string;
  worker_supported: boolean;
  node_ready: boolean;
  active_operation_id: string | null;
  last_check_id: string | null;
  last_operation_id: string | null;
  last_error: string;
  checked_at: string | null;
  // The result predates the saved configuration or a newer lifecycle attempt, or no usable check exists.
  stale: boolean;
  updates: ImageUpdateResult | null;
  // Whoever last saved the policy no longer holds Edit configuration and Operate access, whether or not it is on.
  // Version discovery, notifications and automatic deploys wait until that access returns or someone saves it again.
  ownership_required: boolean;
}

const path = (workloadId: string) => `/api/workloads/${encodeURIComponent(workloadId)}/image-update-policy`;

export const imageUpdatePolicyKey = (workloadId: string) => ["image-update-policy", workloadId] as const;

// Null lists are normalised, missing booleans read as false (and a missing stale flag as stale) so unknown data never
// enables anything or reads as current, and
// pins and candidate keys are dropped because the dashboard never edits them.
function metadata(status: ImageUpdatePolicyStatus): ImageUpdatePolicyStatus {
  const configuration = status.configuration;
  const updates = status.updates;
  return {
    configuration: configuration
      ? { enabled: configuration.enabled === true, mode: configuration.mode, rules: configuration.rules ?? [], window_start: configuration.window_start ?? "", window_end: configuration.window_end ?? "" }
      : null,
    services: status.services ?? [],
    eligible: status.eligible === true,
    unavailable_reason: status.unavailable_reason ?? "",
    worker_supported: status.worker_supported === true,
    node_ready: status.node_ready === true,
    active_operation_id: status.active_operation_id ?? null,
    last_check_id: status.last_check_id ?? null,
    last_operation_id: status.last_operation_id ?? null,
    last_error: status.last_error ?? "",
    checked_at: status.checked_at ?? null,
    stale: status.stale !== false,
    updates: updates ? { state: updates.state, message: updates.message, candidates: updates.candidates ?? [] } : null,
    ownership_required: status.ownership_required === true,
  };
}

// An active operation (including a queued check or deploy) is followed closely; otherwise hourly checks and registry
// pushes change the status from outside the panel, so an idle page still rechecks.
export function imageUpdatePollInterval(status: ImageUpdatePolicyStatus | undefined) {
  return status?.active_operation_id ? 3_000 : 30_000;
}

export const imageUpdatePolicyQuery = (workloadId: string) =>
  queryOptions({
    queryKey: imageUpdatePolicyKey(workloadId),
    queryFn: async ({ signal }) => metadata(await request<ImageUpdatePolicyStatus>("GET", path(workloadId), undefined, signal)),
    refetchInterval: (query) => (query.state.status === "error" ? false : imageUpdatePollInterval(query.state.data)),
    retry: false,
  });

export function useSaveImageUpdatePolicy(workloadId: string) {
  const client = useQueryClient();
  return useMutation({
    mutationFn: async (input: ImageUpdateConfiguration) => metadata(await request<ImageUpdatePolicyStatus>("PUT", path(workloadId), input)),
    onSuccess: (status) => client.setQueryData(imageUpdatePolicyKey(workloadId), status),
    onSettled: () => Promise.all([client.invalidateQueries({ queryKey: imageUpdatePolicyKey(workloadId), exact: true }), client.invalidateQueries({ queryKey: keys.workloadImages(workloadId) })]),
  });
}

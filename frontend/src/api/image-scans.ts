import { queryOptions, useMutation, useQueryClient } from "@tanstack/react-query";
import { request } from "./client";
import { keys } from "./queries";
import type { ImageScan, ScanFinding, ScannedImage, ScanOrigin } from "./types";

// One setting per team and node: it applies to every resource of the team on the resource's node.
export interface ImageScanningSettings {
  enabled: boolean;
  notify: boolean;
  // Team administrators only.
  can_manage: boolean;
  node_id: string;
  // The worker reports the image_scanning capability, which needs COVE_IMAGE_SCANNING=1 in its daemon environment.
  worker_supported: boolean;
  node_ready: boolean;
  // Any active operation of this resource, not only a scan.
  active_operation_id: string | null;
  scanner_image: string;
  java_archives: string;
  database_limit_bytes: number;
  database_max_age_hours: number;
  // Saved with enabled and notify for the same team and node. Older control planes omit the automatic fields, which read as off.
  automatic: boolean;
  // The worker reports the automatic_image_scans capability.
  automatic_supported: boolean;
  // This resource holds a background scan lease. It has no operation until the scan settles, and it blocks nothing.
  automatic_running: boolean;
  // When the control plane next looks at this resource. A scheduling estimate, not a promise that a scan finishes then.
  next_automatic_check_at: string | null;
  // Why automatic scanning is holding or its last result is unknown.
  automatic_message: string;
}

export interface ImageScanningInput {
  enabled: boolean;
  notify: boolean;
  automatic: boolean;
}

// The latest terminal scan. state is the effective state: "complete" only when the scan succeeded with full coverage
// on a fresh database and nothing changed the resource since.
export interface ImageScanResults {
  state: string;
  // Empty when the control plane did not record or recognise how the scan started.
  origin: ScanOrigin | "";
  operation_state: string | null;
  stale: boolean;
  operation_id: string | null;
  result: ImageScan | null;
  message?: string;
}

const settingsPath = (workloadId: string) => `/api/workloads/${encodeURIComponent(workloadId)}/image-scanning`;

export const imageScanningKey = (workloadId: string) => [...keys.workload(workloadId), "image-scanning"] as const;
export const imageScansKey = (workloadId: string) => [...keys.workload(workloadId), "image-scans"] as const;

// Missing booleans read as false so unknown data never enables anything.
function settings(value: ImageScanningSettings): ImageScanningSettings {
  return {
    enabled: value.enabled === true,
    notify: value.notify === true,
    can_manage: value.can_manage === true,
    node_id: value.node_id ?? "",
    worker_supported: value.worker_supported === true,
    node_ready: value.node_ready === true,
    active_operation_id: value.active_operation_id ?? null,
    scanner_image: value.scanner_image ?? "",
    java_archives: value.java_archives ?? "",
    database_limit_bytes: typeof value.database_limit_bytes === "number" ? value.database_limit_bytes : 0,
    database_max_age_hours: typeof value.database_max_age_hours === "number" ? value.database_max_age_hours : 24,
    automatic: value.automatic === true,
    automatic_supported: value.automatic_supported === true,
    automatic_running: value.automatic_running === true,
    next_automatic_check_at: typeof value.next_automatic_check_at === "string" && value.next_automatic_check_at ? value.next_automatic_check_at : null,
    automatic_message: typeof value.automatic_message === "string" ? value.automatic_message : "",
  };
}

function finding(value: ScanFinding): ScanFinding {
  return { id: value.id ?? "", package: value.package ?? "", installed: value.installed ?? "", fixed: value.fixed || undefined, severity: value.severity };
}

function scannedImage(value: ScannedImage): ScannedImage {
  const coverage = value.coverage ?? { os: "", other_languages: "", java_archives: "" };
  return {
    service: value.service ?? "",
    image_id: value.image_id ?? "",
    image_identity: value.image_identity || undefined,
    config_digest: value.config_digest ?? "",
    state: value.state ?? "",
    message: value.message || undefined,
    counts: value.counts ?? {},
    findings: (value.findings ?? []).map(finding),
    truncated: value.truncated === true,
    end_of_life: value.end_of_life === true,
    coverage: { os: coverage.os ?? "", other_languages: coverage.other_languages ?? "", java_archives: coverage.java_archives ?? "", java_message: coverage.java_message || undefined },
  };
}

// Null lists and maps from the worker's report are normalised; missing values stay empty so they read as unknown.
export function imageScan(result: ImageScan): ImageScan {
  return {
    scanner_image: result.scanner_image ?? "",
    scanner_version: result.scanner_version ?? "",
    database: { digest: result.database?.digest ?? "", updated_at: result.database?.updated_at ?? "", next_update: result.database?.next_update ?? "" },
    checked_at: result.checked_at ?? "",
    state: result.state ?? "",
    message: result.message || undefined,
    images: (result.images ?? []).map(scannedImage),
  };
}

const origins: readonly string[] = ["manual", "post_deploy", "scheduled"] satisfies ScanOrigin[];

export function scanOrigin(value: unknown): ScanOrigin | "" {
  return typeof value === "string" && origins.includes(value) ? (value as ScanOrigin) : "";
}

// A missing stale flag reads as stale and only an explicit "complete" state is complete.
function results(value: ImageScanResults): ImageScanResults {
  return {
    state: value.state === "complete" ? "complete" : "unknown",
    origin: scanOrigin(value.origin),
    operation_state: value.operation_state ?? null,
    stale: value.stale !== false,
    operation_id: value.operation_id ?? null,
    result: value.result ? imageScan(value.result) : null,
    message: value.message || undefined,
  };
}

export function imageScanPollInterval(active: boolean) {
  return active ? 3_000 : 30_000;
}

export const imageScanningQuery = (workloadId: string) =>
  queryOptions({
    queryKey: imageScanningKey(workloadId),
    queryFn: async ({ signal }) => settings(await request<ImageScanningSettings>("GET", settingsPath(workloadId), undefined, signal)),
    refetchInterval: (query) => (query.state.status === "error" ? false : imageScanPollInterval(Boolean(query.state.data?.active_operation_id) || query.state.data?.automatic_running === true)),
    retry: false,
  });

// The active operation and background scan come from the settings, so the caller passes them in to follow a running scan closely.
export const imageScansQuery = (workloadId: string, active: boolean) =>
  queryOptions({
    queryKey: imageScansKey(workloadId),
    queryFn: async ({ signal }) => results(await request<ImageScanResults>("GET", `/api/workloads/${encodeURIComponent(workloadId)}/image-scans`, undefined, signal)),
    refetchInterval: (query) => (query.state.status === "error" ? false : imageScanPollInterval(active)),
    retry: false,
  });

export function useSaveImageScanning(workloadId: string) {
  const client = useQueryClient();
  return useMutation({
    mutationFn: async (input: ImageScanningInput) => settings(await request<ImageScanningSettings>("PUT", settingsPath(workloadId), input)),
    // A read that started before the save could otherwise land afterwards and show the old setting.
    onSuccess: async (saved) => {
      await client.cancelQueries({ queryKey: imageScanningKey(workloadId), exact: true });
      client.setQueryData(imageScanningKey(workloadId), saved);
    },
    // Not awaited, so the save settles (and reports success) without waiting for the refetch.
    onSettled: () => {
      void client.invalidateQueries({ queryKey: imageScanningKey(workloadId), exact: true });
    },
  });
}

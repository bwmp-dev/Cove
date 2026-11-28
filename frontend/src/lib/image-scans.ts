import type { ImageScanResults, ImageScanningSettings } from "../api/image-scans";
import { scanOrigin } from "../api/image-scans";
import type { ImageScan, ScannedImage, ScanOrigin, ScanSeverity } from "../api/types";
import type { Tone } from "./nodes";

export const severities: readonly ScanSeverity[] = ["CRITICAL", "HIGH", "MEDIUM", "LOW", "UNKNOWN"];

export const severityLabels: Record<ScanSeverity, string> = { CRITICAL: "Critical", HIGH: "High", MEDIUM: "Medium", LOW: "Low", UNKNOWN: "Unknown" };

export const severityTones: Record<ScanSeverity, Tone> = { CRITICAL: "bad", HIGH: "bad", MEDIUM: "warn", LOW: "neutral", UNKNOWN: "neutral" };

export function severityOf(value: string): ScanSeverity {
  return (severities as readonly string[]).includes(value) ? (value as ScanSeverity) : "UNKNOWN";
}

export type SeverityCounts = Record<ScanSeverity, number>;

// Unrecognised severities are counted as unknown; negative or non-integer counts are ignored.
export function severityCounts(images: ScannedImage[]): SeverityCounts {
  const counts: SeverityCounts = { CRITICAL: 0, HIGH: 0, MEDIUM: 0, LOW: 0, UNKNOWN: 0 };
  for (const image of images) {
    for (const [severity, count] of Object.entries(image.counts)) {
      if (Number.isSafeInteger(count) && count > 0) counts[severityOf(severity)] += count;
    }
  }
  return counts;
}

export function totalFindings(counts: SeverityCounts) {
  return severities.reduce((sum, severity) => sum + counts[severity], 0);
}

const coverageLabels: Record<string, { tone: Tone; label: string }> = {
  scanned: { tone: "ok", label: "Scanned" },
  not_detected: { tone: "neutral", label: "None detected" },
  unknown: { tone: "warn", label: "Unknown" },
};

// Java archives are never scanned in this release; "not_scanned" there means their dependencies are unknown.
export function coverageLabel(value: string, java = false): { tone: Tone; label: string } {
  if (java && value === "not_scanned") return { tone: "warn", label: "Not scanned (unknown)" };
  if (java && value === "scanned") return { tone: "warn", label: "Unknown (scanned)" };
  return coverageLabels[value] ?? { tone: "warn", label: value ? `Unknown (${value})` : "Unknown" };
}

const knownCoverage = new Set(["scanned", "not_detected"]);

// Mirrors the control plane's rule: a complete image has a recognised state, no Java archives and at least one scanned
// package class. It can only downgrade what the control plane reports, never upgrade it.
export function imageComplete(image: ScannedImage) {
  const { os, other_languages: languages, java_archives: java } = image.coverage;
  return image.state === "complete" && java === "not_detected" && knownCoverage.has(os) && knownCoverage.has(languages) && (os === "scanned" || languages === "scanned");
}

export function imageStateLabel(image: ScannedImage): { tone: Tone; label: string } {
  if (imageComplete(image)) return { tone: "ok", label: "Scan completed" };
  if (image.state === "complete" || image.state === "unknown" || !image.state) return { tone: "warn", label: "Coverage unknown" };
  return { tone: "warn", label: `Unknown (${image.state})` };
}

// Why a recorded database cannot back a complete result now. Used to explain an unknown result, never to mark one complete.
export function databaseProblem(database: ImageScan["database"], now: number, maxAgeHours: number): string | null {
  const updated = Date.parse(database.updated_at);
  const next = Date.parse(database.next_update);
  if (!/^sha256:[a-f0-9]{64}$/.test(database.digest) || !Number.isFinite(updated) || !Number.isFinite(next)) return "The scan did not record a recognised vulnerability database, so its coverage is unknown.";
  if (updated > now) return "The vulnerability database is dated in the future, so its age cannot be trusted and coverage is unknown.";
  if (now - updated > maxAgeHours * 3_600_000) return `The vulnerability database used by this scan is more than ${maxAgeHours} hours old, so newer advisories may be missing. Scan again for a current result.`;
  if (next <= now) return "The vulnerability database has passed its next update time, so newer advisories may be missing. Scan again for a current result.";
  return null;
}

// Complete only when the control plane says so, the result is fresh and readable, and every image is complete.
export function resultComplete(data: ImageScanResults, unverified: boolean) {
  const result = data.result;
  return data.state === "complete" && !data.stale && !unverified && result !== null && result.images.length > 0 && result.images.every(imageComplete);
}

const terminalLabels: Record<string, string> = { failed: "Scan failed", cancelled: "Scan cancelled", interrupted: "Scan interrupted" };

function verifiedResultLabel(data: ImageScanResults): { tone: Tone; label: string } {
  if (!data.operation_id) return { tone: "neutral", label: "No completed scan" };
  const terminal = data.operation_state ? terminalLabels[data.operation_state] : undefined;
  if (terminal) return { tone: data.operation_state === "failed" ? "bad" : "warn", label: terminal };
  if (resultComplete(data, false)) return { tone: totalFindings(severityCounts(data.result?.images ?? [])) > 0 ? "warn" : "ok", label: "Scan completed" };
  return { tone: "warn", label: "Coverage unknown" };
}

export function resultLabel(data: ImageScanResults, unverified: boolean): { tone: Tone; label: string } {
  const label = verifiedResultLabel(data);
  return unverified ? { tone: "neutral", label: `${label.label} (unverified)` } : label;
}

// What a single scan operation recorded, without the later freshness checks the Registry page applies.
export function reportComplete(scan: ImageScan) {
  return scan.state === "complete" && scan.images.length > 0 && scan.images.every(imageComplete);
}

export function formatGiB(bytes: number) {
  if (!(bytes > 0)) return "";
  const value = bytes / 1024 ** 3;
  return `${Number.isInteger(value) ? value : value.toFixed(1)} GiB`;
}

export function enableBlocker(settings: ImageScanningSettings, node: string): string | null {
  if (!settings.worker_supported) return `The worker on ${node} does not report vulnerability scanning. The node operator must set COVE_IMAGE_SCANNING=1 in the worker daemon's environment, restart the daemon and verify the node first.`;
  if (!settings.node_ready) return `Scans can be allowed once ${node} is ready.`;
  return null;
}

// Turning automatic scans off never needs a capability; turning them on needs scans allowed and an upgraded ready worker.
export function automaticBlocker(settings: ImageScanningSettings, enabled: boolean, node: string): string | null {
  if (!enabled) return "Automatic scans need vulnerability scans to be allowed.";
  if (!settings.worker_supported || !settings.automatic_supported) return `The worker on ${node} does not report automatic scans. The node operator must upgrade the worker and verify the node first. Manual scans are not affected.`;
  if (!settings.node_ready) return `Automatic scans can be turned on once ${node} is ready.`;
  return null;
}

const originLabels: Record<ScanOrigin, string> = { manual: "Manual", post_deploy: "Automatic, after a deploy or job run", scheduled: "Automatic, after a vulnerability database update" };

// Never inferred: a missing or unrecognised origin says so.
export function scanOriginLabel(value: unknown) {
  const origin = scanOrigin(value);
  return origin ? originLabels[origin] : "Not recorded";
}

interface ScanState {
  online: boolean;
  settings: ImageScanningSettings | undefined;
  settingsPending: boolean;
  settingsError: boolean;
  resultsError: boolean;
  node: string;
}

// Unknown, failed or cached-but-unverified reads keep scanning blocked. An active operation is reported separately.
export function scanBlocker({ online, settings, settingsPending, settingsError, resultsError, node }: ScanState): string | null {
  if (!online) return "You are offline. Scanning is available when the connection returns.";
  if (settingsPending) return "Checking whether scanning is allowed.";
  if (settingsError || !settings) return "Cove cannot read the scanning settings. Scanning is paused until they load.";
  if (resultsError) return "Cove cannot read the latest results. Scanning is paused until they load.";
  if (settings.active_operation_id) return null;
  if (!settings.worker_supported) return `The worker on ${node} does not report vulnerability scanning.`;
  if (!settings.node_ready) return `Scanning is available once ${node} is ready.`;
  if (!settings.enabled) return `Vulnerability scans are off for this team on ${node}. A team administrator can allow them in the setting below.`;
  return null;
}

export function plural(count: number, one: string, many: string) {
  return `${count} ${count === 1 ? one : many}`;
}

export function activeScanOperation(settings: ImageScanningSettings | undefined) {
  return settings?.active_operation_id ?? null;
}

export function databaseMaxAge(settings: ImageScanningSettings | undefined) {
  return settings?.database_max_age_hours ?? 24;
}

const unfinished: Record<string, string> = { failed: "failed", cancelled: "was cancelled", interrupted: "was interrupted" };

function operationReason(data: ImageScanResults): string | null {
  const verb = data.operation_state ? unfinished[data.operation_state] : undefined;
  if (verb) return `The last scan ${verb}. ${data.result ? "Only part of its report is available; images and packages it did not reach are not covered." : "It recorded no report."}`;
  if (data.operation_state === "succeeded") return "The scan operation succeeded, but its coverage is not complete.";
  return null;
}

function reportReasons(data: ImageScanResults, scan: ImageScan, now: number, maxAgeHours: number): string[] {
  const reasons: string[] = [];
  if (scan.message) reasons.push(scan.message);
  const database = databaseProblem(scan.database, now, maxAgeHours);
  if (database) reasons.push(database);
  else if (data.stale) reasons.push("The resource was deployed, started, stopped or rolled back after this scan, so it may not describe the current images.");
  const java = scan.images.filter((image) => image.coverage.java_archives === "not_scanned").length;
  if (java > 0) reasons.push(`Java archive (JAR, WAR or EAR) dependencies were not scanned in ${plural(java, "image", "images")}. Other findings are kept below.`);
  const incomplete = scan.images.filter((image) => !imageComplete(image) && image.coverage.java_archives !== "not_scanned").length;
  if (incomplete > 0) reasons.push(`${plural(incomplete, "image has", "images have")} unknown coverage.`);
  if (scan.images.length === 0) reasons.push("The report lists no images.");
  return reasons;
}

// Why a result is not complete, in the order people should read them.
export function unknownReasons(data: ImageScanResults, now: number, maxAgeHours: number) {
  const operation = operationReason(data);
  const reasons = [...(operation ? [operation] : []), ...(data.result ? reportReasons(data, data.result, now, maxAgeHours) : [])];
  return { terminal: Boolean(data.operation_state && unfinished[data.operation_state]), reasons };
}

import { describe, expect, it } from "vitest";
import type { ImageScanningSettings } from "../api/image-scans";
import type { Operation, ScannedImage } from "../api/types";
import { coverageLabel, databaseProblem, formatGiB, imageComplete, scanBlocker, severityCounts, totalFindings } from "./image-scans";
import { actionLabels, latestByWorkload, latestDeploymentOperation } from "./operations";

const digest = `sha256:${"d".repeat(64)}`;
const now = Date.parse("2026-11-23T12:00:00Z");
const image = (extra: Partial<ScannedImage> = {}): ScannedImage => ({
  service: "app",
  image_id: digest,
  config_digest: digest,
  state: "complete",
  counts: {},
  findings: [],
  truncated: false,
  end_of_life: false,
  coverage: { os: "scanned", other_languages: "not_detected", java_archives: "not_detected" },
  ...extra,
});
const settings = (extra: Partial<ImageScanningSettings> = {}): ImageScanningSettings => ({
  enabled: true,
  notify: false,
  can_manage: false,
  node_id: "n1",
  worker_supported: true,
  node_ready: true,
  active_operation_id: null,
  scanner_image: "",
  java_archives: "not_scanned",
  database_limit_bytes: 0,
  database_max_age_hours: 24,
  ...extra,
});
const ready = { online: true, settings: settings(), settingsPending: false, settingsError: false, resultsError: false, node: "edge-1" };

describe("vulnerability scan results", () => {
  it("counts every severity, folding unrecognised ones into unknown and ignoring invalid counts", () => {
    const counts = severityCounts([image({ counts: { HIGH: 2, critical: 1, LOW: -1, MEDIUM: 1.5 } }), image({ counts: { HIGH: 1, UNKNOWN: 3 } })]);
    expect(counts).toEqual({ CRITICAL: 0, HIGH: 3, MEDIUM: 0, LOW: 0, UNKNOWN: 4 });
    expect(totalFindings(counts)).toBe(7);
  });

  it("only treats an image as complete with scanned packages and no Java archives", () => {
    expect(imageComplete(image())).toBe(true);
    expect(imageComplete(image({ coverage: { os: "scanned", other_languages: "scanned", java_archives: "not_scanned" } }))).toBe(false);
    expect(imageComplete(image({ coverage: { os: "not_detected", other_languages: "not_detected", java_archives: "not_detected" } }))).toBe(false);
    expect(imageComplete(image({ coverage: { os: "scanned", other_languages: "partial", java_archives: "not_detected" } }))).toBe(false);
    expect(imageComplete(image({ state: "done" }))).toBe(false);
  });

  it("labels future coverage values as unknown with the value", () => {
    expect(coverageLabel("scanned").label).toBe("Scanned");
    expect(coverageLabel("not_scanned", true).label).toBe("Not scanned (unknown)");
    expect(coverageLabel("indexed", true).label).toBe("Unknown (indexed)");
    expect(coverageLabel("").label).toBe("Unknown");
  });

  it("explains missing, future, old and expired databases", () => {
    const fresh = { digest, updated_at: "2026-11-23T10:00:00Z", next_update: "2026-11-23T22:00:00Z" };
    expect(databaseProblem(fresh, now, 24)).toBeNull();
    expect(databaseProblem({ ...fresh, digest: "" }, now, 24)).toMatch(/recognised vulnerability database/);
    expect(databaseProblem({ ...fresh, updated_at: "2026-11-23T13:00:00Z" }, now, 24)).toMatch(/future/);
    expect(databaseProblem({ ...fresh, updated_at: "2026-11-22T11:00:00Z" }, now, 24)).toMatch(/more than 24 hours old/);
    expect(databaseProblem({ ...fresh, next_update: "2026-11-23T11:00:00Z" }, now, 24)).toMatch(/next update time/);
  });

  it("formats the database limit in GiB", () => {
    expect(formatGiB(2 * 1024 ** 3)).toBe("2 GiB");
    expect(formatGiB(0)).toBe("");
  });
});

describe("queuing a scan", () => {
  it("fails closed on unknown, failed or disabled state", () => {
    expect(scanBlocker(ready)).toBeNull();
    expect(scanBlocker({ ...ready, online: false })).toMatch(/offline/);
    expect(scanBlocker({ ...ready, settings: undefined, settingsPending: true })).toMatch(/Checking/);
    expect(scanBlocker({ ...ready, settingsError: true })).toMatch(/cannot read the scanning settings/);
    expect(scanBlocker({ ...ready, resultsError: true })).toMatch(/cannot read the latest results/);
    expect(scanBlocker({ ...ready, settings: settings({ worker_supported: false }) })).toMatch(/does not report vulnerability scanning/);
    expect(scanBlocker({ ...ready, settings: settings({ node_ready: false }) })).toMatch(/once edge-1 is ready/);
    expect(scanBlocker({ ...ready, settings: settings({ enabled: false }) })).toMatch(/off for this team on edge-1/);
  });

  it("is housekeeping that never replaces the latest deployment", () => {
    const operations: Operation[] = [
      { id: "scan", workload_id: "w1", action: "image_scan", state: "succeeded", result: null, created_at: "2026-11-23T10:00:00Z" },
      { id: "deploy", workload_id: "w1", action: "deploy", state: "succeeded", result: null, created_at: "2026-11-23T09:00:00Z" },
    ];
    expect(latestDeploymentOperation(operations, "w1")?.id).toBe("deploy");
    expect(latestByWorkload(operations).get("w1")?.id).toBe("deploy");
    expect(actionLabels.image_scan).toBe("Scan for vulnerabilities");
  });
});

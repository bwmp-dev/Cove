import { describe, expect, it } from "vitest";
import type { ImageStatus, Operation, Workload } from "../api/types";
import { imageChecksSupported, imageRefreshKey, imageStatus, imageSummary, releaseLink, shortDigest, supportsImageLifecycle } from "./images";
import { latestByWorkload, latestDeploymentOperation } from "./operations";

const image = (status: ImageStatus["status"], service = "web"): ImageStatus => ({ service, reference: "ghcr.io/acme/web:1", image_id: "sha256:0123456789abcdef0123", status });

function operation(id: string, action: Operation["action"], state: Operation["state"], created_at: string): Operation {
  return { id, workload_id: "w1", action, state, result: null, created_at };
}

describe("image statuses", () => {
  it("labels each status and falls back to unknown", () => {
    expect(imageStatus("update_available")).toMatchObject({ tone: "warn", label: "Update available" });
    expect(imageStatus("unpublished").label).toBe("Unpublished");
    expect(imageStatus("something-new").label).toBe("Unknown");
  });

  it("counts unknown results instead of reporting everything current", () => {
    expect(imageSummary([image("current"), image("current", "api"), image("unknown", "db")])).toBe("2 current, 1 unknown");
    expect(imageSummary([])).toBe("");
  });

  it("shortens digests but keeps the algorithm", () => {
    expect(shortDigest("sha256:0123456789abcdef0123")).toBe("sha256:0123456789ab");
  });

  it("checks services and jobs but not managed previews", () => {
    expect(supportsImageLifecycle({ kind: "service", preview_source_id: "", environment_preview_id: "" } as Workload)).toBe(true);
    expect(supportsImageLifecycle({ kind: "job" } as Workload)).toBe(true);
    expect(supportsImageLifecycle({ kind: "service", preview_source_id: "w0" } as Workload)).toBe(false);
    expect(supportsImageLifecycle({ kind: "service", environment_preview_id: "p1" } as Workload)).toBe(false);
  });

  it("only links GitHub HTTPS release pages", () => {
    expect(releaseLink("https://github.com/acme/web/releases/tag/v1.2.3")).toBe("https://github.com/acme/web/releases/tag/v1.2.3");
    expect(releaseLink("javascript:alert(1)")).toBeNull();
    expect(releaseLink("https://example.com/acme/web/releases/tag/v1")).toBeNull();
    expect(releaseLink(undefined)).toBeNull();
  });
});

describe("image checks as housekeeping", () => {
  const operations = [
    operation("check", "image_check", "succeeded", "2026-11-22T10:00:00Z"),
    operation("deploy", "deploy", "succeeded", "2026-11-22T09:00:00Z"),
  ];

  it("does not replace the latest deployment in summaries", () => {
    expect(latestDeploymentOperation(operations, "w1")?.id).toBe("deploy");
    expect(latestByWorkload(operations).get("w1")?.id).toBe("deploy");
  });

  it("refreshes cached images after a completed check or deployment", () => {
    expect(imageRefreshKey(operations, "w1")).toBe("check");
    expect(imageRefreshKey([operation("running", "image_check", "running", "2026-11-22T11:00:00Z"), ...operations], "w1")).toBe("check");
    expect(imageRefreshKey([], "w1")).toBeNull();
  });

  it("refreshes a service only after successful deployments", () => {
    expect(imageRefreshKey([operation("failed", "deploy", "failed", "2026-11-22T11:00:00Z"), ...operations], "w1")).toBe("check");
  });

  it("refreshes a job after any newer run attempt and again when that run finishes", () => {
    expect(imageRefreshKey(operations, "w1", "job")).toBe("check:succeeded");
    const running = [operation("run", "deploy", "running", "2026-11-22T11:00:00Z"), ...operations];
    const failed = [operation("run", "deploy", "failed", "2026-11-22T11:00:00Z"), ...operations];
    const cancelled = [operation("run", "deploy", "cancelled", "2026-11-22T11:00:00Z"), ...operations];
    expect(imageRefreshKey(running, "w1", "job")).toBe("run:running");
    expect(imageRefreshKey(failed, "w1", "job")).toBe("run:failed");
    expect(imageRefreshKey(cancelled, "w1", "job")).toBe("run:cancelled");
    // An unfinished check does not change the cached result.
    expect(imageRefreshKey([operation("check2", "image_check", "running", "2026-11-22T12:00:00Z"), ...failed], "w1", "job")).toBe("run:failed");
  });
});

describe("image check support", () => {
  const base = { protocol: 1, os: "linux", arch: "amd64", docker: "27", compose: "2", ready: true };

  it("needs both image capabilities for jobs and only image lifecycle for services", () => {
    expect(imageChecksSupported({ ...base, image_lifecycle: true }, "service")).toBe(true);
    expect(imageChecksSupported({ ...base, image_lifecycle: true }, "job")).toBe(false);
    expect(imageChecksSupported({ ...base, job_image_lifecycle: true }, "job")).toBe(false);
    expect(imageChecksSupported({ ...base, image_lifecycle: true, job_image_lifecycle: true }, "job")).toBe(true);
    expect(imageChecksSupported(null, "job")).toBe(false);
    expect(imageChecksSupported(undefined, "service")).toBe(false);
  });

  it("describes job statuses without suggesting a redeploy", () => {
    expect(imageStatus("update_available").description).toMatch(/Pull images and redeploy/);
    expect(imageStatus("update_available", "job").description).not.toMatch(/redeploy/);
    expect(imageStatus("pinned", "job")).toEqual(imageStatus("pinned"));
  });
});

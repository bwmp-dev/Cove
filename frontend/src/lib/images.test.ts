import { describe, expect, it } from "vitest";
import type { ImageStatus, Operation } from "../api/types";
import { imageRefreshKey, imageStatus, imageSummary, releaseLink, shortDigest } from "./images";
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
});

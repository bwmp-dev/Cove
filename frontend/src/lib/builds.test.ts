import { describe, expect, it } from "vitest";
import type { Operation, Workload } from "../api/types";
import { buildHistory, repositoryError, supportsBuildPublish } from "./builds";
import { activeOperation, latestByWorkload, latestDeploymentOperation } from "./operations";

function operation(id: string, action: Operation["action"], state: Operation["state"], build = false): Operation {
  return {
    id,
    workload_id: "w1",
    action,
    state,
    created_at: "2026-11-22T10:00:00Z",
    result: build ? { id, workload: "w1", action, state, created_at: "2026-11-22T10:00:00Z", build: { images: [{ service: "web", image_id: "sha256:1", reference: "ghcr.io/team/app@sha256:1" }] } } : null,
  };
}

describe("build repository", () => {
  it("accepts lowercase repository paths", () => {
    expect(repositoryError("team/app")).toBeNull();
    expect(repositoryError("team/my-app_v2.web")).toBeNull();
    expect(repositoryError("a".repeat(128))).toBeNull();
  });

  it("rejects tags, digests, uppercase, bad separators and long paths", () => {
    expect(repositoryError("")).toMatch(/Enter a repository/);
    expect(repositoryError("team/app:latest")).toMatch(/without a registry host, tag or digest/);
    expect(repositoryError("team/app@sha256:abc")).toMatch(/without a registry host, tag or digest/);
    expect(repositoryError("Team/App")).toMatch(/lowercase/);
    expect(repositoryError("team//app")).toMatch(/lowercase/);
    expect(repositoryError("/team")).toMatch(/lowercase/);
    expect(repositoryError("a".repeat(129))).toMatch(/at most 128/);
  });

  it("is offered for services and jobs but not managed previews", () => {
    expect(supportsBuildPublish({ kind: "job" } as Workload)).toBe(true);
    expect(supportsBuildPublish({ kind: "service", preview_source_id: "", environment_preview_id: "" } as Workload)).toBe(true);
    expect(supportsBuildPublish({ kind: "service", preview_source_id: "w0" } as Workload)).toBe(false);
    expect(supportsBuildPublish({ kind: "job", environment_preview_id: "p1" } as Workload)).toBe(false);
  });
});

describe("build history", () => {
  it("keeps the latest success while a newer attempt runs or fails", () => {
    const history = buildHistory([operation("failed", "build", "failed"), operation("ok", "build", "succeeded", true), operation("deploy", "deploy", "succeeded")], "w1");
    expect(history.attempt?.id).toBe("failed");
    expect(history.success?.id).toBe("ok");
  });

  it("does not treat a success without verified images as published", () => {
    expect(buildHistory([operation("ok", "build", "succeeded")], "w1").success).toBeUndefined();
  });
});

describe("builds in deployment summaries", () => {
  const operations = [operation("build", "build", "succeeded", true), operation("deploy", "deploy", "succeeded")];

  it("do not replace the latest deployment", () => {
    expect(latestDeploymentOperation(operations, "w1")?.id).toBe("deploy");
    expect(latestByWorkload(operations).get("w1")?.id).toBe("deploy");
  });

  it("still count as the active operation", () => {
    expect(activeOperation([operation("running", "build", "running"), ...operations], "w1")?.id).toBe("running");
  });
});

import { describe, expect, it } from "vitest";
import type { EnvironmentSuspension, Operation, SuspendedResource } from "../api/types";
import { actionLabels, activeOperation, latestByWorkload, latestDeploymentOperation } from "./operations";
import { environmentHold, holdBlocks, holdReason, type HeldAction } from "./suspension";

const resource = (overrides: Partial<SuspendedResource> = {}): SuspendedResource => ({ id: "w1", name: "web", kind: "service", node_id: "n1", supported: true, phase: "active", resume_wanted: false, ...overrides });
const state = (paused: boolean, item: SuspendedResource): EnvironmentSuspension => ({ paused, phase: item.phase, resources: [item] });
const all: HeldAction[] = ["deploy", "start", "restart", "rollback", "rollout_promote", "restore", "drill", "restore_copy", "promote_release", "placement"];

describe("environmentHold", () => {
  it("does not hold project resources", () => {
    expect(environmentHold({ resourceId: "w1", read: "success", environmentPaused: true })).toEqual({ kind: "none" });
  });

  it("treats the requested pause as a hold before the worker confirms it", () => {
    const hold = environmentHold({ resourceId: "w1", environmentId: "e1", suspension: state(true, resource({ phase: "pausing" })), read: "success" });
    expect(hold.kind).toBe("paused");
    expect(all.every((action) => holdBlocks(hold, action))).toBe(true);
  });

  it("keeps blocking start and replace actions while a resume is pending", () => {
    const hold = environmentHold({ resourceId: "w1", environmentId: "e1", environmentPaused: true, suspension: state(false, resource({ phase: "resuming", operation_id: "op" })), read: "success" });
    expect(hold.kind).toBe("pending");
    expect(holdBlocks(hold, "deploy")).toBe(true);
    expect(holdReason(hold, "deploy", "Deploying")).toBe("The environment is resuming. Deploying is available when the resume finishes.");
  });

  it("allows Deploy, Start, Restart and Restore to repair a failed resume, and nothing else", () => {
    const hold = environmentHold({ resourceId: "w1", environmentId: "e1", suspension: state(false, resource({ phase: "error", error: "health check timed out" })), read: "success" });
    expect(hold).toMatchObject({ kind: "failed", repairable: true });
    expect(all.filter((action) => !holdBlocks(hold, action))).toEqual(["deploy", "start", "restart", "restore"]);
  });

  it("does not offer repairs for a failed pause", () => {
    const hold = environmentHold({ resourceId: "w1", environmentId: "e1", suspension: state(true, resource({ phase: "error", error: "worker unreachable" })), read: "success" });
    expect(hold).toMatchObject({ kind: "failed", paused: true, repairable: false });
    expect(holdBlocks(hold, "start")).toBe(true);
  });

  it("fails closed while the state is loading or unreadable, even with stale active data", () => {
    const stale = state(false, resource());
    const unknown = environmentHold({ resourceId: "w1", environmentId: "e1", read: "error" });
    expect(unknown).toEqual({ kind: "unknown" });
    expect(environmentHold({ resourceId: "w1", environmentId: "e1", suspension: stale, read: "error" })).toEqual({ kind: "unknown" });
    expect(environmentHold({ resourceId: "w1", environmentId: "e1", read: "pending" })).toEqual({ kind: "loading" });
    expect(all.every((action) => holdBlocks(unknown, action))).toBe(true);
    expect(holdReason(unknown, "deploy", "Deploying")).toBe("Cove cannot read whether the environment is paused. Deploying is available once it can be read.");
    expect(environmentHold({ resourceId: "w1", environmentId: "e1", environmentPaused: true, read: "error" }).kind).toBe("paused");
  });

  it("prefers the environment read over the resource's cached flag", () => {
    expect(environmentHold({ resourceId: "w1", environmentId: "e1", environmentPaused: true, suspension: state(false, resource()), read: "success" })).toEqual({ kind: "none" });
  });
});

describe("environment pause operations", () => {
  const operation = (id: string, action: Operation["action"], state: Operation["state"] = "succeeded"): Operation => ({ id, workload_id: "w1", action, state, result: null, created_at: "2026-11-22T10:00:00Z" });
  const operations = [operation("resume", "resume"), operation("suspend", "suspend"), operation("deploy", "deploy")];

  it("do not replace the latest deployment summary", () => {
    expect(latestDeploymentOperation(operations, "w1")?.id).toBe("deploy");
    expect(latestByWorkload(operations).get("w1")?.id).toBe("deploy");
  });

  it("still show as the active operation with their own labels", () => {
    expect(activeOperation([operation("pausing", "suspend", "running"), ...operations], "w1")?.id).toBe("pausing");
    expect(actionLabels.suspend).toBe("Pause environment resource");
    expect(actionLabels.resume).toBe("Resume environment resource");
  });
});

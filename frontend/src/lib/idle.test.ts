import { describe, expect, it } from "vitest";
import type { IdleRuntime, Operation, ResourceSuspension, WorkloadRuntime } from "../api/types";
import { actionLabels, activeOperation, latestByWorkload, latestDeploymentOperation } from "./operations";
import { formatSeconds, idleBlocks, idleDraft, idleHold, idlePhase, idleReason, sameIdleDraft, validateIdle } from "./idle";

// Test fixture: an idle helper report shaped like the worker's runtime response.
const report = (phase: string, extra: Partial<IdleRuntime> = {}): WorkloadRuntime => ({ containers: [], idle: { enabled: true, after_seconds: 300, phase, active_requests: 0, ...extra } });
const idleSuspension = (phase: ResourceSuspension["phase"]): ResourceSuspension => ({ operation: "episode", scope: "w1", reason: "idle", phase, captured: 1, previously_running: 1 });

describe("idlePhase", () => {
  it("keeps unrecognised and missing phases unknown", () => {
    expect(idlePhase("sleeping")).toBe("sleeping");
    expect(idlePhase("hibernating")).toBe("unknown");
    expect(idlePhase("toString")).toBe("unknown");
    expect(idlePhase(undefined)).toBe("unknown");
  });
});

describe("idleHold", () => {
  it("does not hold awake apps, apps without a policy or a blocked gateway", () => {
    expect(idleHold({ runtime: report("awake"), read: "success" })).toEqual({ kind: "none" });
    expect(idleHold({ runtime: report("blocked"), read: "success" })).toEqual({ kind: "none" });
    expect(idleHold({ runtime: { containers: [], idle: null }, read: "success" })).toEqual({ kind: "none" });
  });

  it("maps sleeping, waking and failed wake phases", () => {
    expect(idleHold({ runtime: report("sleeping"), read: "success" })).toEqual({ kind: "asleep", phase: "sleeping" });
    expect(idleHold({ runtime: report("wake_failed"), read: "success" })).toEqual({ kind: "asleep", phase: "wake_failed" });
    expect(idleHold({ runtime: report("sleeping_pending"), read: "success" })).toEqual({ kind: "sleeping" });
    expect(idleHold({ runtime: report("quiescing"), read: "success" })).toEqual({ kind: "sleeping" });
    expect(idleHold({ runtime: report("wake_queued"), read: "success" })).toEqual({ kind: "waking" });
    expect(idleHold({ runtime: report("waking"), read: "success" })).toEqual({ kind: "waking" });
    expect(idleHold({ runtime: report("unknown"), read: "success" })).toEqual({ kind: "unknown" });
    expect(idleHold({ runtime: report("future-phase"), read: "success" })).toEqual({ kind: "unknown" });
  });

  it("holds a manual or environment hold only while the idle snapshot exists", () => {
    expect(idleHold({ runtime: report("paused"), read: "success" })).toEqual({ kind: "none" });
    expect(idleHold({ runtime: { ...report("paused"), suspension: idleSuspension("paused") }, read: "success" })).toEqual({ kind: "asleep", phase: "sleeping" });
  });

  it("uses the worker's idle suspension when no helper status is reported", () => {
    expect(idleHold({ runtime: { containers: [], suspension: idleSuspension("paused") }, read: "success" })).toEqual({ kind: "asleep", phase: "sleeping" });
    expect(idleHold({ runtime: { containers: [], suspension: idleSuspension("pausing") }, read: "success" })).toEqual({ kind: "sleeping" });
    expect(idleHold({ runtime: { containers: [], suspension: idleSuspension("resuming") }, read: "success" })).toEqual({ kind: "waking" });
    expect(idleHold({ runtime: { containers: [], suspension: { ...idleSuspension("paused"), reason: "environment" } }, read: "success" })).toEqual({ kind: "none" });
    expect(idleHold({ runtime: { containers: [], suspension: { ...idleSuspension("paused"), reason: "manual_stop" } }, read: "success" })).toEqual({ kind: "none" });
    expect(idleHold({ runtime: { ...report("paused"), suspension: { ...idleSuspension("paused"), reason: "manual_stop" } }, read: "success" })).toEqual({ kind: "none" });
  });

  it("treats a failed or pending read as unknown only when the saved policy is on", () => {
    expect(idleHold({ policyEnabled: true, read: "error" })).toEqual({ kind: "unknown" });
    expect(idleHold({ policyEnabled: true, runtime: report("awake"), read: "error" })).toEqual({ kind: "unknown" });
    expect(idleHold({ policyEnabled: true, read: "pending" })).toEqual({ kind: "loading" });
    expect(idleHold({ policyEnabled: false, read: "error" })).toEqual({ kind: "none" });
    expect(idleHold({ read: "error" })).toEqual({ kind: "none" });
  });
});

describe("idleBlocks and idleReason", () => {
  it("lets Start and Restart wake a sleeping app but holds replacements", () => {
    const asleep = { kind: "asleep", phase: "sleeping" } as const;
    expect(idleBlocks(asleep, "start")).toBe(false);
    expect(idleBlocks(asleep, "restart")).toBe(false);
    for (const action of ["deploy", "rollback", "promote_release"] as const) expect(idleBlocks(asleep, action)).toBe(true);
    expect(idleReason(asleep, "deploy", "Deploying")).toBe("The app is asleep. Deploying is available after it wakes: use Start or Wake now first.");
    expect(idleReason(asleep, "start", "Starting")).toBeNull();
  });

  it("holds every action while sleep or wake is in progress", () => {
    expect(idleBlocks({ kind: "sleeping" }, "start")).toBe(true);
    expect(idleBlocks({ kind: "waking" }, "restart")).toBe(true);
    expect(idleReason({ kind: "waking" }, "rollback", "Rolling back")).toBe("The app is waking. Rolling back is available when it is awake.");
  });

  it("keeps Start available as a repair when the state is unknown", () => {
    expect(idleBlocks({ kind: "unknown" }, "start")).toBe(false);
    expect(idleBlocks({ kind: "unknown" }, "deploy")).toBe(true);
    expect(idleReason({ kind: "unknown" }, "deploy", "Deploying")).toMatch(/cannot read whether the app is asleep/);
    expect(idleBlocks({ kind: "none" }, "deploy")).toBe(false);
  });
});

describe("validateIdle", () => {
  it("sends exactly enabled, after_seconds and request_driven", () => {
    expect(validateIdle({ enabled: true, after: " 120 ", requestDriven: true })).toEqual({ errors: {}, policy: { enabled: true, after_seconds: 120, request_driven: true } });
  });

  it("requires the request-driven assertion only to turn sleep on", () => {
    expect(validateIdle({ enabled: true, after: "300", requestDriven: false }).errors.requestDriven).toMatch(/Confirm/);
    expect(validateIdle({ enabled: false, after: "300", requestDriven: false }).policy).toEqual({ enabled: false, after_seconds: 300, request_driven: false });
  });

  it("enforces whole seconds from 60 to 86400", () => {
    for (const after of ["59", "86401", "90.5", "", "1e3", "-60"]) expect(validateIdle({ enabled: false, after, requestDriven: false }).errors.after).toBeDefined();
    expect(validateIdle({ enabled: false, after: "60", requestDriven: false }).policy?.after_seconds).toBe(60);
    expect(validateIdle({ enabled: false, after: "86400", requestDriven: false }).policy?.after_seconds).toBe(86400);
  });

  it("defaults a zero timeout to 300 seconds and compares drafts by value", () => {
    expect(idleDraft({ enabled: false, after_seconds: 0, request_driven: false }).after).toBe("300");
    expect(sameIdleDraft({ enabled: true, after: "300 ", requestDriven: true }, { enabled: true, after: "300", requestDriven: true })).toBe(true);
  });
});

it("formats timeouts in the largest whole unit", () => {
  expect(formatSeconds(300)).toBe("5 minutes");
  expect(formatSeconds(60)).toBe("1 minute");
  expect(formatSeconds(7200)).toBe("2 hours");
  expect(formatSeconds(90)).toBe("90 seconds");
});

describe("idle policy operations", () => {
  // Test fixtures: operation rows as the list endpoint returns them, newest first.
  const operations: Operation[] = [
    { id: "policy", workload_id: "w1", action: "idle_policy", state: "running", result: null, created_at: "2026-11-22T12:01:00Z" },
    { id: "deploy", workload_id: "w1", action: "deploy", state: "succeeded", result: null, created_at: "2026-11-22T12:00:00Z" },
  ];

  it("labels policy changes and keeps them out of deployment summaries while still active", () => {
    expect(actionLabels.idle_policy).toBe("Change idle sleep policy");
    expect(latestDeploymentOperation(operations, "w1")?.id).toBe("deploy");
    expect(latestByWorkload(operations).get("w1")?.id).toBe("deploy");
    expect(activeOperation(operations, "w1")?.id).toBe("policy");
  });
});

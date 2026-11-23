import { describe, expect, it } from "vitest";
import { autoscaleDraft, autoscaleStatus, candidateServices, cooldownEnds, sameAutoscaleDraft, sampleIsStale, scaleChanges, validateAutoscale } from "./autoscale";

const limits = { web: { cpus: 0.5, memory_mb: 256 }, worker: { cpus: 1, memory_mb: 512 }, cache: { cpus: 0, memory_mb: 128 } };

describe("autoscaleStatus", () => {
  it("never presents an unrecognised status as healthy", () => {
    expect(autoscaleStatus("within_target")).toMatchObject({ tone: "ok", label: "No replica change" });
    expect(autoscaleStatus("future_state")).toMatchObject({ tone: "warn", label: "Unknown status: future_state" });
    expect(autoscaleStatus("")).toMatchObject({ tone: "warn", label: "Unknown status" });
    expect(autoscaleStatus("toString").label).toBe("Unknown status: toString");
  });
});

describe("samples", () => {
  const now = Date.parse("2026-11-22T12:00:00Z");
  it("treats samples older than 90 seconds, from the future or with unreadable times as stale", () => {
    expect(sampleIsStale({ sampled_at: "2026-11-22T12:00:05Z", generation: "g", replicas: {}, cpu_percent: {} }, now)).toBe(true);
    expect(sampleIsStale({ sampled_at: "2026-11-22T11:58:31Z", generation: "g", replicas: {}, cpu_percent: {} }, now)).toBe(false);
    expect(sampleIsStale({ sampled_at: "2026-11-22T11:58:29Z", generation: "g", replicas: {}, cpu_percent: {} }, now)).toBe(true);
    expect(sampleIsStale({ sampled_at: "not a time", generation: "g", replicas: {}, cpu_percent: {} }, now)).toBe(true);
  });

  it("ends the cooldown 5 minutes after the last queued attempt", () => {
    expect(cooldownEnds("2026-11-22T12:00:00Z")).toBe("2026-11-22T12:05:00.000Z");
    expect(cooldownEnds(null)).toBeNull();
  });
});

describe("drafts", () => {
  it("lists services with limits plus saved ones, and defaults unselected bounds", () => {
    const policy = { enabled: true, services: { legacy: { min_replicas: 2, max_replicas: 3, target_cpu_percent: 60 } } };
    const services = candidateServices(limits, policy);
    expect(services).toEqual(["cache", "legacy", "web", "worker"]);
    const draft = autoscaleDraft(policy, services);
    expect(draft.services.legacy).toEqual({ selected: true, min: "2", max: "3", target: "60" });
    expect(draft.services.web).toEqual({ selected: false, min: "1", max: "4", target: "70" });
  });

  it("ignores edits to unselected services when comparing", () => {
    const saved = autoscaleDraft({ enabled: false, services: {} }, ["web"]);
    expect(sameAutoscaleDraft(saved, { ...saved, services: { web: { selected: false, min: "3", max: "9", target: "50" } } })).toBe(true);
    expect(sameAutoscaleDraft(saved, { ...saved, services: { web: { selected: true, min: "1", max: "4", target: "70" } } })).toBe(false);
  });
});

describe("validateAutoscale", () => {
  const draft = (services: Record<string, { selected: boolean; min: string; max: string; target: string }>, enabled = true) => ({ enabled, services });

  it("builds an explicit policy with every field for selected services only", () => {
    const result = validateAutoscale(draft({ web: { selected: true, min: "2", max: "6", target: "65" }, worker: { selected: false, min: "x", max: "", target: "" } }), { limits });
    expect(result.policy).toEqual({ enabled: true, services: { web: { min_replicas: 2, max_replicas: 6, target_cpu_percent: 65 } } });
  });

  it("reports bounds, ordering and target errors beside each field", () => {
    const { errors, policy } = validateAutoscale(draft({ web: { selected: true, min: "0", max: "17", target: "95" }, worker: { selected: true, min: "5", max: "3", target: "9" } }), { limits });
    expect(policy).toBeUndefined();
    expect(errors["web.min"]).toMatch(/1 to 16/);
    expect(errors["web.max"]).toMatch(/1 to 16/);
    expect(errors["web.target"]).toMatch(/10 to 90/);
    expect(errors["worker.max"]).toMatch(/at least as large as the minimum/);
    expect(errors["worker.target"]).toMatch(/10 to 90/);
  });

  it("requires a selected service and explicit CPU and memory limits when enabled", () => {
    expect(validateAutoscale(draft({ web: { selected: false, min: "1", max: "4", target: "70" } }), { limits }).errors.form).toMatch(/at least one service/);
    expect(validateAutoscale(draft({ cache: { selected: true, min: "1", max: "4", target: "70" } }), { limits }).errors["cache.limits"]).toMatch(/CPU and memory limits/);
    // A disabled policy can keep services whose limits were removed, so turning scaling off is never blocked by them.
    expect(validateAutoscale(draft({ cache: { selected: true, min: "1", max: "4", target: "70" } }, false), { limits }).policy).toEqual({ enabled: false, services: { cache: { min_replicas: 1, max_replicas: 4, target_cpu_percent: 70 } } });
    expect(validateAutoscale(draft({}, false), { limits }).policy).toEqual({ enabled: false, services: {} });
  });

  it("counts fixed replica counts and maximum replicas against the 64 replica budget", () => {
    const many = Object.fromEntries(Array.from({ length: 4 }, (_, index) => [`s${index}`, { selected: true, min: "1", max: "16", target: "70" }]));
    const allLimits = Object.fromEntries(Object.keys(many).map((name) => [name, { cpus: 1, memory_mb: 64 }]));
    expect(validateAutoscale(draft(many), { limits: allLimits }).policy).toBeDefined();
    const result = validateAutoscale(draft(many), { limits: allLimits, manualReplicas: { other: 1 } });
    expect(result.errors.form).toMatch(/add up to 65/);
    // A fixed count above the maximum is the one reserved.
    expect(validateAutoscale(draft({ s0: { selected: true, min: "1", max: "2", target: "70" } }), { limits: allLimits, manualReplicas: { s0: 8 } }).policy).toBeDefined();
  });
});

describe("scaleChanges", () => {
  it("lists before and after counts with the triggering CPU", () => {
    expect(scaleChanges({ generation: "g1", before: { web: 2, worker: 1 }, replicas: { web: 3 }, cpu_percent: { web: 104.25, worker: 10 } })).toEqual([{ service: "web", before: 2, after: 3, cpu: 104.25 }]);
  });
});

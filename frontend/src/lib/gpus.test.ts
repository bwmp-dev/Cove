import { describe, expect, it } from "vitest";
import type { GPUInventory } from "../api/gpus";
import { composeServiceNames, deviceOptions, deviceState, gpuRows, isGPUOnly, mergeCPULimits, mergeGPUAssignments, validateGPURows, type GPURow, type Limits } from "./gpus";

const uuid = "GPU-11111111-2222-3333-4444-555555555555";
const cdi = "nvidia.com/gpu=0";

const inventory: GPUInventory = {
  checked_at: "2026-11-22T10:00:00Z",
  devices: [
    { id: uuid, name: "NVIDIA L4", driver: "nvidia", memory_mb: 23034, assignable: true, notice: "" },
    { id: cdi, name: "CDI GPU 0", driver: "cdi", memory_mb: null, assignable: false, notice: "Spec is not root-owned." },
  ],
  limited: false,
  notices: [],
};

const row = (service: string, mode: GPURow["mode"], selected: string[] = []): GPURow => ({ key: service, service, mode, selected });

describe("GPU rows", () => {
  it("reads the three modes and treats null like omitted", () => {
    const limits: Limits = {
      web: { cpus: 1, memory_mb: 512 },
      nulled: { cpus: 2, memory_mb: 0, gpus: null },
      cleared: { cpus: 0, memory_mb: 256, gpus: [] },
      trainer: { cpus: 0, memory_mb: 0, gpus: [uuid, cdi] },
    };
    expect(gpuRows(limits)).toEqual([row("cleared", "none"), row("trainer", "select", [uuid, cdi])]);
  });

  it("hides only entries that carry nothing but an explicit GPU setting from the CPU form", () => {
    expect(isGPUOnly({ cpus: 0, memory_mb: 0, gpus: [] })).toBe(true);
    expect(isGPUOnly({ cpus: 0, memory_mb: 0, gpus: null })).toBe(false);
    expect(isGPUOnly({ cpus: 0, memory_mb: 0 })).toBe(false);
    expect(isGPUOnly({ cpus: 1, memory_mb: 0, gpus: [uuid] })).toBe(false);
  });
});

describe("saving CPU and memory", () => {
  it("keeps every saved gpus value exactly, including null and []", () => {
    const saved: Limits = { web: { cpus: 1, memory_mb: 0, gpus: null }, api: { cpus: 1, memory_mb: 0, gpus: [] }, ml: { cpus: 1, memory_mb: 0, gpus: [uuid] }, plain: { cpus: 1, memory_mb: 0 } };
    const merged = mergeCPULimits(saved, { web: { cpus: 2, memory_mb: 0 }, api: { cpus: 2, memory_mb: 64 }, ml: { cpus: 0, memory_mb: 128 }, plain: { cpus: 3, memory_mb: 0 } });
    expect(merged).toEqual({ web: { cpus: 2, memory_mb: 0, gpus: null }, api: { cpus: 2, memory_mb: 64, gpus: [] }, ml: { cpus: 0, memory_mb: 128, gpus: [uuid] }, plain: { cpus: 3, memory_mb: 0 } });
    expect(merged.plain).not.toHaveProperty("gpus");
  });

  it("keeps an explicit GPU setting when its CPU row is removed or hidden, and drops inherited ones", () => {
    const saved: Limits = { ml: { cpus: 1, memory_mb: 0, gpus: [uuid] }, gpuOnly: { cpus: 0, memory_mb: 0, gpus: [] }, web: { cpus: 1, memory_mb: 0, gpus: null } };
    expect(mergeCPULimits(saved, {})).toEqual({ ml: { cpus: 0, memory_mb: 0, gpus: [uuid] }, gpuOnly: { cpus: 0, memory_mb: 0, gpus: [] } });
  });
});

describe("saving GPU assignments", () => {
  const saved: Limits = { web: { cpus: 1.5, memory_mb: 512 }, ml: { cpus: 4, memory_mb: 8192, gpus: [uuid] }, old: { cpus: 0, memory_mb: 0, gpus: [cdi] }, nulled: { cpus: 1, memory_mb: 0, gpus: null } };

  it("serializes inherit as omitted, none as [] and select as the chosen IDs without touching CPU or memory", () => {
    const merged = mergeGPUAssignments(saved, [row("web", "none"), row("ml", "select", [uuid, cdi]), row("old", "select", [cdi])]);
    expect(merged).toEqual({
      web: { cpus: 1.5, memory_mb: 512, gpus: [] },
      ml: { cpus: 4, memory_mb: 8192, gpus: [uuid, cdi] },
      old: { cpus: 0, memory_mb: 0, gpus: [cdi] },
      nulled: { cpus: 1, memory_mb: 0, gpus: null },
    });
  });

  it("returns removed rows to inheriting Compose and drops entries left empty", () => {
    const merged = mergeGPUAssignments(saved, [row("ml", "inherit")]);
    expect(merged).toEqual({ web: { cpus: 1.5, memory_mb: 512 }, ml: { cpus: 4, memory_mb: 8192 }, nulled: { cpus: 1, memory_mb: 0, gpus: null } });
    expect(merged.ml).not.toHaveProperty("gpus");
  });

  it("adds a GPU-only entry for a service without CPU or memory limits", () => {
    expect(mergeGPUAssignments(undefined, [row("worker", "none")])).toEqual({ worker: { cpus: 0, memory_mb: 0, gpus: [] } });
    expect(mergeGPUAssignments(undefined, [row("worker", "inherit")])).toEqual({});
  });

  it("never converts a saved null to []", () => {
    expect(mergeGPUAssignments(saved, []).nulled).toEqual({ cpus: 1, memory_mb: 0, gpus: null });
  });
});

describe("validation", () => {
  it("rejects bad or duplicate services, services missing from Compose and empty or oversized selections", () => {
    const many = Array.from({ length: 17 }, (_, index) => `vendor.com/gpu=${index}`);
    const errors = validateGPURows(
      [
        { key: "a", service: "", mode: "none", selected: [] },
        { key: "b", service: "web", mode: "select", selected: [] },
        { key: "c", service: "web", mode: "none", selected: [] },
        { key: "d", service: "ghost", mode: "select", selected: many },
        { key: "e", service: "api", mode: "select", selected: [uuid] },
      ],
      ["web", "api"],
    );
    expect(errors).toEqual({
      a: { service: "Enter the Compose service name." },
      b: { selection: "Select at least one device, or choose No standard GPU requests." },
      c: { service: "Each service can appear only once." },
      d: { service: "This service is not in the saved Compose file.", selection: "Select at most 16 devices." },
    });
  });

  it("does not check service names against Compose it could not read", () => {
    expect(validateGPURows([row("anything", "none")], null)).toEqual({});
  });
});

describe("device state", () => {
  it("distinguishes unchecked, missing, unassignable and available devices", () => {
    expect(deviceState(uuid, undefined).kind).toBe("unchecked");
    expect(deviceState("GPU-99999999-2222-3333-4444-555555555555", inventory).kind).toBe("missing");
    expect(deviceState(cdi, inventory).kind).toBe("unassignable");
    expect(deviceState(uuid, inventory).kind).toBe("available");
  });

  it("lists observed devices first and keeps saved devices the inspection did not observe", () => {
    const stale = "vendor.com/gpu=gone";
    expect(deviceOptions([stale, uuid], [], inventory)).toEqual([uuid, cdi, stale]);
    expect(deviceOptions([stale], [], undefined)).toEqual([stale]);
  });

  it("keeps cleared saved devices listed so they can be kept again", () => {
    const stale = "vendor.com/gpu=gone";
    expect(deviceOptions([], [stale], inventory)).toEqual([uuid, cdi, stale]);
    expect(deviceOptions([uuid], [stale, uuid], undefined)).toEqual([stale, uuid]);
  });
});

describe("Compose service names", () => {
  it("reads inline services and returns null when it cannot", () => {
    expect(composeServiceNames("services:\n  web:\n    image: nginx\n  trainer:\n    image: cuda\n")).toEqual(["web", "trainer"]);
    expect(composeServiceNames("")).toBeNull();
    expect(composeServiceNames("services: [")).toBeNull();
    expect(composeServiceNames("name: x\n")).toBeNull();
  });
});

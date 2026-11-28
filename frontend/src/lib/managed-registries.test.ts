import { expect, it } from "vitest";
import type { ManagedRegistry } from "../api/managed-registries";
import type { Node, Operation } from "../api/types";
import { editable, hostnameError, isLive, nameError, nodeProblem, recordDeletable, registryPollInterval, registryState, validateRetention } from "./managed-registries";
import { actionLabels, isHousekeeping, latestByWorkload, latestDeploymentOperation } from "./operations";

const base: ManagedRegistry = {
  id: "m1",
  name: "Team registry",
  node_id: "n1",
  project_id: "p1",
  hostname: "registry.example.test",
  state: "ready",
  connection_id: "c1",
  operation_id: "",
  operation_state: "",
  last_error: "",
  retention: { keep_per_repository: 10, minimum_age_days: 30 },
  storage: { hard_cap_verified: false, reason: "" },
  image: "",
  data_retained: false,
};
const registry = (extra: Partial<ManagedRegistry>) => ({ ...base, ...extra });

it("labels unknown states without treating them as ready or removed", () => {
  expect(registryState("ready")).toEqual({ tone: "ok", label: "Ready" });
  expect(registryState("future")).toEqual({ tone: "neutral", label: "Unknown state (future)" });
  expect(registryState("")).toEqual({ tone: "neutral", label: "Unknown state" });
  expect(isLive(registry({ state: "future" }))).toBe(true);
  expect(isLive(registry({ state: "removed" }))).toBe(false);
  expect(editable(registry({ state: "future" }))).toBe(false);
});

it("polls every 3 seconds while a registry or its operation is working and every 30 seconds otherwise", () => {
  expect(registryPollInterval(undefined)).toBe(30_000);
  expect(registryPollInterval([registry({})])).toBe(30_000);
  for (const state of ["provisioning", "maintenance", "removing"]) expect(registryPollInterval([registry({ state })])).toBe(3_000);
  for (const operation_state of ["queued", "running"]) expect(registryPollInterval([registry({ operation_state })])).toBe(3_000);
  expect(registryPollInterval([registry({ operation_state: "interrupted" })])).toBe(30_000);
});

it("allows retention changes only while ready or failed without an active operation", () => {
  expect(editable(registry({}))).toBe(true);
  expect(editable(registry({ state: "failed" }))).toBe(true);
  expect(editable(registry({ operation_state: "queued" }))).toBe(false);
  for (const state of ["maintenance", "removing", "provisioning", "removed"]) expect(editable(registry({ state }))).toBe(false);
});

it("allows deleting the record only of a removed registry without retained data or an active operation", () => {
  expect(recordDeletable(registry({ state: "removed" }))).toBe(true);
  expect(recordDeletable(registry({ state: "removed", data_retained: true }))).toBe(false);
  for (const operation_state of ["queued", "running"]) expect(recordDeletable(registry({ state: "removed", operation_state }))).toBe(false);
  for (const state of ["ready", "failed", "removing", "future"]) expect(recordDeletable(registry({ state }))).toBe(false);
});

it("validates retention bounds and names like the API", () => {
  expect(validateRetention({ keep: "1", age: "3650" })).toEqual({});
  expect(validateRetention({ keep: "100", age: "1" })).toEqual({});
  expect(Object.keys(validateRetention({ keep: "0", age: "3651" }))).toEqual(["keep", "age"]);
  expect(Object.keys(validateRetention({ keep: "2.5", age: "" }))).toEqual(["keep", "age"]);
  expect(nameError("   ")).toBe("Enter a name.");
  expect(nameError("é".repeat(60))).toBeUndefined();
  expect(nameError("é".repeat(61))).toBe("Use at most 120 characters.");
  expect(hostnameError("registry.example.test")).toBeUndefined();
  expect(hostnameError("-bad.example.test")).toBe("Enter a complete DNS hostname, such as registry.example.com.");
});

it("treats missing or unreported capabilities as unsupported", () => {
  const node = { id: "n1", name: "a", state: "ready", capabilities: { managed_registry: true, file_secrets: true } } as Node;
  expect(nodeProblem(node)).toBeNull();
  expect(nodeProblem({ ...node, capabilities: null })).toBe("capabilities not reported");
  expect(nodeProblem({ ...node, capabilities: { file_secrets: true } } as Node)).toBe("no managed registry support");
  expect(nodeProblem({ ...node, capabilities: { managed_registry: true } } as Node)).toBe("no secret file support");
  expect(nodeProblem({ ...node, state: "enrolled" })).toBe("not ready");
});

it("labels registry maintenance and leaves it out of deployment summaries", () => {
  const maintenance = { id: "o2", workload_id: "m1", action: "registry_maintenance", state: "succeeded", result: null, created_at: "2026-11-02T00:00:00Z" } as Operation;
  const deploy = { id: "o1", workload_id: "m1", action: "deploy", state: "succeeded", result: null, created_at: "2026-11-01T00:00:00Z" } as Operation;
  expect(actionLabels.registry_maintenance).toBe("Registry maintenance");
  expect(isHousekeeping(maintenance)).toBe(true);
  expect(isHousekeeping(deploy)).toBe(false);
  expect(latestDeploymentOperation([maintenance, deploy], "m1")).toBe(deploy);
  expect(latestByWorkload([maintenance, deploy]).get("m1")).toBe(deploy);
});

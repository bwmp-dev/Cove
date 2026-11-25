import { expect, it } from "vitest";
import { parseBuildRecipeSettings } from "../api/build-recipes";
import type { Operation, OperationBuildRecipe } from "../api/types";
import { commandError, composeServices, formatCPU, formatGB, formatMemory, pathError, recipePayload, recordedEngine, recordedPhaseMemory, savedRecipeBlocker } from "./build-recipes";

it.each([".", "site", "apps/web/dist", "a.b/c-d_e"])("accepts the clean relative path %s", (value) => {
  expect(pathError(value, "directory")).toBeNull();
});

it.each(["", "/srv", "../x", "a/../b", "a/..", "a//b", "a/", "./a", "a/./b", "a\\b", "c:x", "a,b", 'a"b', "a\nb", "a\rb", "a\0b", "x".repeat(513)])("refuses the path %j", (value) => {
  expect(pathError(value, "directory")).not.toBeNull();
});

it("counts command limits in UTF-8 bytes", () => {
  expect(commandError("x".repeat(8192))).toBeNull();
  expect(commandError("é".repeat(4096))).toBeNull();
  expect(commandError("é".repeat(4096) + "x")).toMatch(/8192 bytes/);
});

it("sends only the fields that belong to the engine", () => {
  const draft = { engine: "railpack", service: "web", directory: "app", output: "dist", build_command: "make", start_command: "run" };
  expect(recipePayload(draft)).toEqual({ engine: "railpack", service: "web", directory: "app", output: "", build_command: "make", start_command: "run" });
  expect(recipePayload({ ...draft, engine: "static" })).toEqual({ engine: "static", service: "web", directory: "app", output: "dist", build_command: "", start_command: "" });
  expect(recipePayload({ ...draft, engine: "compose" })).toEqual({ engine: "compose", service: "", directory: "", output: "", build_command: "", start_command: "" });
  expect(recipePayload({ ...draft, engine: "buildpacks" })).toEqual({ engine: "buildpacks", service: "web", directory: "app", output: "", build_command: "", start_command: "" });
});

it("lists the saved Compose services and nothing for an unreadable file", () => {
  expect(composeServices("services:\n  web: {}\n  api:\n    image: x\n")).toEqual(["web", "api"]);
  expect(composeServices("services: [")).toEqual([]);
  expect(composeServices("")).toEqual([]);
});

const valid = {
  recipe: { engine: "compose", service: "", directory: "", output: "", build_command: "", start_command: "" },
  can_edit: false,
  source_ready: true,
  supported: true,
  reason: "",
  build_node_id: "n1",
  active_operation_id: "",
  build_command_set: false,
  start_command_set: false,
  railpack_version: "0.43.1",
  pinned_images: [],
};

it("rejects responses with missing or mistyped fields", () => {
  expect(parseBuildRecipeSettings(valid).recipe.engine).toBe("compose");
  expect(() => parseBuildRecipeSettings({ ...valid, recipe: { ...valid.recipe, output: undefined } })).toThrow();
  expect(() => parseBuildRecipeSettings({ ...valid, recipe: { ...valid.recipe, engine: "" } })).toThrow();
  expect(() => parseBuildRecipeSettings({ ...valid, supported: "true" })).toThrow();
  expect(() => parseBuildRecipeSettings({ ...valid, pinned_images: [1] })).toThrow();
  expect(() => parseBuildRecipeSettings({ ...valid, reason: undefined })).toThrow();
  expect(parseBuildRecipeSettings({ ...valid, recipe: { ...valid.recipe, engine: "future" } }).recipe.engine).toBe("future");
});

function buildOperation(recipe?: OperationBuildRecipe): Operation {
  return { id: "o", workload_id: "w", action: "build", state: "failed", created_at: "", result: { id: "o", workload: "w", action: "build", state: "failed", created_at: "", ...(recipe ? { build_recipe: recipe } : {}) } };
}

it("describes the recorded engine and never assumes Compose", () => {
  expect(recordedEngine(buildOperation())).toBe("Build engine not recorded");
  expect(recordedEngine(buildOperation({ engine: "static", service: "web" }))).toBe("Static output for web");
  expect(recordedEngine(buildOperation({ engine: "railpack", service: "web", version: "0.43.1" }))).toBe("Railpack 0.43.1 for web");
  expect(recordedEngine(buildOperation({ engine: "compose" }))).toBe("Existing Compose / Dockerfile");
  expect(recordedEngine(buildOperation({ engine: "mystery" }))).toBe("Unknown engine (mystery)");
});

const buildpacks = {
  supported: true,
  reason: "",
  lifecycle_version: "0.21.29",
  builder_version: "0.0.230",
  platform_api: "0.13",
  pinned_images: ["lifecycle@sha256:a", "builder@sha256:b", "run@sha256:c"],
  work_storage: "disk" as const,
  work_hard_cap: false as const,
  work_target_bytes: 3221225472,
  minimum_phase_memory_bytes: 2147483648,
  maximum_phase_memory_bytes: 4294967296,
  memory_safety_bytes: 2147483648,
  required_available_memory_bytes: 4294967296,
  required_docker_disk_bytes: 25769803776,
  required_worker_disk_bytes: 10737418240,
  quota_memory_mb: 4352,
  quota_cpu: 2.5,
  builder_download_bytes: 1446326381,
  builder_unpacked_bytes: 5108638067,
  experimental: true,
};

it("reads the buildpacks object and treats a missing or malformed one as unknown without failing the read", () => {
  expect(parseBuildRecipeSettings({ ...valid, buildpacks }).buildpacks).toEqual(buildpacks);
  expect(parseBuildRecipeSettings(valid).buildpacks).toBeUndefined();
  expect(parseBuildRecipeSettings({ ...valid, buildpacks: null }).buildpacks).toBeUndefined();
  expect(parseBuildRecipeSettings({ ...valid, buildpacks: { ...buildpacks, supported: "true" } }).buildpacks).toBeUndefined();
  expect(parseBuildRecipeSettings({ ...valid, buildpacks: { ...buildpacks, work_target_bytes: "3GiB" } }).buildpacks).toBeUndefined();
  expect(parseBuildRecipeSettings({ ...valid, buildpacks: { ...buildpacks, quota_memory_mb: -1 } }).buildpacks).toBeUndefined();
  expect(parseBuildRecipeSettings({ ...valid, buildpacks: { ...buildpacks, pinned_images: [1] } }).buildpacks).toBeUndefined();
  const withoutReason: Partial<typeof buildpacks> = { ...buildpacks };
  delete withoutReason.reason;
  expect(parseBuildRecipeSettings({ ...valid, buildpacks: withoutReason }).buildpacks).toBeUndefined();
  expect(parseBuildRecipeSettings({ ...valid, buildpacks: { ...buildpacks, work_target_bytes: "3GiB" } }).recipe.engine).toBe("compose");
});

it("accepts a fractional CPU reservation and any positive safe integer sizes the server reports", () => {
  const other = { ...buildpacks, quota_cpu: 0.75, work_target_bytes: 1, minimum_phase_memory_bytes: 3 * 1024 ** 3, maximum_phase_memory_bytes: 3 * 1024 ** 3, quota_memory_mb: 1 };
  expect(parseBuildRecipeSettings({ ...valid, buildpacks: other }).buildpacks).toEqual(other);
});

it.each([
  ["a memory-backed work volume", { work_storage: "tmpfs" }],
  ["an unknown work storage mode", { work_storage: "volume" }],
  ["a missing work storage mode", { work_storage: undefined }],
  ["a hard storage cap", { work_hard_cap: true }],
  ["a missing hard-cap flag", { work_hard_cap: undefined }],
  ["a non-boolean hard-cap flag", { work_hard_cap: "false" }],
  ["the old fixed phase memory field only", { minimum_phase_memory_bytes: undefined, maximum_phase_memory_bytes: undefined, phase_memory_bytes: 4294967296 }],
  ["a minimum phase memory above the maximum", { minimum_phase_memory_bytes: 4294967296, maximum_phase_memory_bytes: 2147483648 }],
  ["a zero safety headroom", { memory_safety_bytes: 0 }],
  ["a fractional byte count", { work_target_bytes: 3221225472.5 }],
  ["an unsafe byte count", { required_docker_disk_bytes: 2 ** 53 }],
  ["a missing unpacked size", { builder_unpacked_bytes: undefined }],
  ["a zero CPU reservation", { quota_cpu: 0 }],
  ["a negative CPU reservation", { quota_cpu: -2.5 }],
  ["an infinite CPU reservation", { quota_cpu: Infinity }],
  ["a CPU reservation as text", { quota_cpu: "2.5" }],
  ["a missing CPU reservation", { quota_cpu: undefined }],
])("treats Cloud Native Buildpacks eligibility as unknown for %s, leaving the rest of the read usable", (_case, change) => {
  const read = parseBuildRecipeSettings({ ...valid, buildpacks: { ...buildpacks, ...change } });
  expect(read.buildpacks).toBeUndefined();
  expect(read.recipe.engine).toBe("compose");
  expect(savedRecipeBlocker({ ...read, recipe: { ...valid.recipe, engine: "buildpacks", service: "web", directory: "." } })).toMatch(/did not report Cloud Native Buildpacks eligibility/);
});

it("formats memory exactly in binary units and CPUs with fractions", () => {
  expect(formatMemory(4294967296)).toBe("4 GiB");
  expect(formatMemory(25769803776)).toBe("24 GiB");
  expect(formatMemory(4352 * 1024 ** 2)).toBe("4,352 MiB");
  expect(formatMemory(2430 * 1024 ** 2)).toBe("2,430 MiB");
  expect(formatMemory(1000)).toBe("1,000 bytes");
  expect(formatCPU(2.5)).toBe("2.5 CPUs");
  expect(formatCPU(1)).toBe("1 CPU");
  expect(formatCPU(2)).toBe("2 CPUs");
});

it("describes the phase memory limit a Cloud Native Buildpacks build recorded and never assumes one", () => {
  const cnb = { engine: "buildpacks", service: "web" };
  expect(recordedPhaseMemory(buildOperation(cnb))).toBe("Phase memory not assigned");
  expect(recordedPhaseMemory(buildOperation({ ...cnb, phase_memory_bytes: 2430 * 1024 ** 2, admission_available_memory_bytes: 4430 * 1024 ** 2 }))).toBe("Phase memory limit 2,430 MiB (from 4,430 MiB available at admission)");
  expect(recordedPhaseMemory(buildOperation({ ...cnb, phase_memory_bytes: 2147483648 }))).toBe("Phase memory limit 2 GiB");
  expect(recordedPhaseMemory(buildOperation({ ...cnb, phase_memory_bytes: 3221225472, admission_available_memory_bytes: -1 }))).toBe("Phase memory limit 3 GiB (available memory at admission unavailable)");
  for (const invalid of [0, -1, 1.5, 2 ** 53, Number.NaN]) expect(recordedPhaseMemory(buildOperation({ ...cnb, phase_memory_bytes: invalid }))).toBe("Phase memory limit unavailable");
  // Recorded by whatever produced the JSON, so the type does not guarantee a number.
  expect(recordedPhaseMemory(buildOperation({ ...cnb, phase_memory_bytes: "4GiB" as unknown as number }))).toBe("Phase memory limit unavailable");
  expect(recordedPhaseMemory(buildOperation({ engine: "railpack", phase_memory_bytes: 4294967296 }))).toBeUndefined();
  expect(recordedPhaseMemory(buildOperation())).toBeUndefined();
});

it("blocks a saved Cloud Native Buildpacks recipe unless the server confirms it can build", () => {
  const saved = { ...parseBuildRecipeSettings(valid), recipe: { ...valid.recipe, engine: "buildpacks", service: "web", directory: "." } };
  expect(savedRecipeBlocker({ ...saved, buildpacks })).toBeUndefined();
  // The generic flag governs static output and Railpack only.
  expect(savedRecipeBlocker({ ...saved, supported: false, buildpacks })).toBeUndefined();
  expect(savedRecipeBlocker(saved)).toMatch(/did not report Cloud Native Buildpacks eligibility/);
  expect(savedRecipeBlocker({ ...saved, buildpacks: { ...buildpacks, supported: false, reason: "the build node needs 9 GiB of available memory" } })).toBe("the build node needs 9 GiB of available memory");
  expect(savedRecipeBlocker({ ...saved, buildpacks: { ...buildpacks, supported: false } })).toMatch(/cannot run Cloud Native Buildpacks/);
  expect(savedRecipeBlocker({ ...saved, reason: "the selected service is absent from saved Compose", buildpacks })).toBe("the selected service is absent from saved Compose");
  expect(savedRecipeBlocker({ ...saved, recipe: valid.recipe })).toBeUndefined();
});

it("formats download sizes as decimal gigabytes", () => {
  expect(formatGB(1446326381)).toBe("1.45 GB");
});

it("describes a recorded Cloud Native Buildpacks build with its versions", () => {
  expect(recordedEngine(buildOperation({ engine: "buildpacks", service: "web", version: "0.21.29", builder_version: "0.0.230", platform_api: "0.13", pinned_images: ["a", "b", "c"] }))).toBe(
    "Cloud Native Buildpacks (lifecycle 0.21.29, builder 0.0.230, platform API 0.13) for web",
  );
  expect(recordedEngine(buildOperation({ engine: "buildpacks", service: "web" }))).toBe("Cloud Native Buildpacks for web");
});

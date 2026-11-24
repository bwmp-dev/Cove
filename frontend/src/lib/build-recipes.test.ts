import { expect, it } from "vitest";
import { parseBuildRecipeSettings } from "../api/build-recipes";
import type { Operation } from "../api/types";
import { commandError, composeServices, formatCPU, formatGB, formatMB, formatMemory, pathError, recipeErrors, recipePayload, recordedEngine, recordedPhaseMemory, recordedPlannerChecksum, savedRecipeBlocker } from "./build-recipes";

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
  expect(recipePayload({ ...draft, engine: "nixpacks" })).toEqual({ engine: "nixpacks", service: "web", directory: "app", output: "", build_command: "make", start_command: "run" });
});

it("validates Nixpacks like Railpack: optional commands up to 8192 bytes, for services and jobs", () => {
  const draft = { engine: "nixpacks", service: "web", directory: ".", output: "", build_command: "", start_command: "" };
  expect(recipeErrors(draft, false)).toEqual({});
  expect(recipeErrors(draft, true)).toEqual({});
  expect(recipeErrors({ ...draft, build_command: "x".repeat(8192), start_command: "é".repeat(4096) }, true)).toEqual({});
  expect(recipeErrors({ ...draft, build_command: "x".repeat(8193), start_command: "é".repeat(4097) }, false)).toEqual({ build_command: "Use at most 8192 bytes (currently 8193).", start_command: "Use at most 8192 bytes (currently 8194)." });
  expect(recipeErrors({ ...draft, service: "", directory: "../x" }, false)).toEqual({ service: "Choose a Compose service.", directory: "Paths cannot contain .. segments." });
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

function buildOperation(recipe?: NonNullable<Operation["result"]>["build_recipe"]): Operation {
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

const nixpacks = {
  supported: true,
  reason: "",
  version: "1.41.0",
  planner_sha256: `sha256:${"0f".repeat(32)}`,
  planner_download_bytes: 3717141,
  base_image: `ghcr.io/railwayapp/nixpacks@sha256:${"d4".repeat(32)}`,
  base_download_bytes: 138966568,
  maintenance: true as const,
  proxy_supported: false as const,
  planner_memory_bytes: 536870912,
  planner_cpu: 1,
  planner_timeout_seconds: 60,
  planner_output_bytes: 134217728,
};

it("reads the nixpacks object and treats a missing, malformed or unrecognised one as unknown without failing the read", () => {
  expect(parseBuildRecipeSettings({ ...valid, nixpacks }).nixpacks).toEqual(nixpacks);
  expect(parseBuildRecipeSettings(valid).nixpacks).toBeUndefined();
  for (const broken of [
    null,
    [],
    { ...nixpacks, supported: "true" },
    { ...nixpacks, reason: undefined },
    { ...nixpacks, version: "" },
    { ...nixpacks, maintenance: false },
    { ...nixpacks, maintenance: undefined },
    { ...nixpacks, proxy_supported: true },
    { ...nixpacks, proxy_supported: "false" },
    { ...nixpacks, planner_sha256: "sha256:ABC" },
    { ...nixpacks, planner_sha256: "0f".repeat(32) },
    { ...nixpacks, base_image: "ghcr.io/railwayapp/nixpacks:ubuntu-1745885067" },
    { ...nixpacks, base_image: `nixpacks@sha256:${"D4".repeat(32)}` },
    { ...nixpacks, planner_download_bytes: -1 },
    { ...nixpacks, base_download_bytes: 1.5 },
    { ...nixpacks, planner_memory_bytes: 0 },
    { ...nixpacks, planner_cpu: 0 },
    { ...nixpacks, planner_timeout_seconds: "60" },
    { ...nixpacks, planner_output_bytes: undefined },
  ]) {
    const read = parseBuildRecipeSettings({ ...valid, nixpacks: broken });
    expect(read.nixpacks).toBeUndefined();
    expect(read.recipe.engine).toBe("compose");
  }
});

it("accepts unpinned metadata only when Nixpacks is not supported", () => {
  const unknownArch = { ...nixpacks, supported: false, reason: "the pinned Nixpacks planner requires a Linux amd64 or arm64 build node", planner_sha256: "", planner_download_bytes: 0, base_download_bytes: 0 };
  expect(parseBuildRecipeSettings({ ...valid, nixpacks: unknownArch }).nixpacks).toEqual(unknownArch);
  expect(parseBuildRecipeSettings({ ...valid, nixpacks: { ...unknownArch, supported: true } }).nixpacks).toBeUndefined();
  expect(parseBuildRecipeSettings({ ...valid, nixpacks: { ...nixpacks, planner_download_bytes: 0 } }).nixpacks).toBeUndefined();
  expect(parseBuildRecipeSettings({ ...valid, nixpacks: { ...nixpacks, base_download_bytes: 0 } }).nixpacks).toBeUndefined();
  const other = { ...nixpacks, planner_cpu: 0.5, planner_sha256: `sha256:${"91".repeat(32)}`, planner_download_bytes: 4111227, base_download_bytes: 135771180 };
  expect(parseBuildRecipeSettings({ ...valid, nixpacks: other }).nixpacks).toEqual(other);
});

it("blocks a saved Nixpacks recipe unless the server confirms it can build, with the top-level reason first", () => {
  const saved = { ...parseBuildRecipeSettings(valid), recipe: { ...valid.recipe, engine: "nixpacks", service: "web", directory: "." } };
  expect(savedRecipeBlocker({ ...saved, nixpacks })).toBeUndefined();
  expect(savedRecipeBlocker(saved)).toMatch(/did not report usable Nixpacks eligibility/);
  expect(savedRecipeBlocker({ ...saved, nixpacks: { ...nixpacks, supported: false, reason: "upgrade and verify the effective build worker before using Nixpacks" } })).toBe(
    "upgrade and verify the effective build worker before using Nixpacks",
  );
  expect(savedRecipeBlocker({ ...saved, nixpacks: { ...nixpacks, supported: false } })).toMatch(/cannot run Nixpacks/);
  expect(savedRecipeBlocker({ ...saved, reason: "Nixpacks is unavailable while an outbound build proxy is configured", nixpacks })).toBe("Nixpacks is unavailable while an outbound build proxy is configured");
});

it("describes a recorded Nixpacks build with its version and the recorded planner archive checksum only", () => {
  const digest = `sha256:${"0f".repeat(32)}`;
  const recorded = buildOperation({ engine: "nixpacks", service: "web", version: "1.41.0", pinned_images: [nixpacks.base_image], planner_sha256: digest });
  expect(recordedEngine(recorded)).toBe("Nixpacks 1.41.0 for web");
  expect(recordedPlannerChecksum(recorded)).toBe(`Planner archive checksum ${digest}`);
  expect(recordedPlannerChecksum(buildOperation({ engine: "nixpacks", service: "web", version: "1.41.0" }))).toBe("Planner archive checksum not recorded");
  expect(recordedPlannerChecksum(buildOperation({ engine: "nixpacks", planner_sha256: "" }))).toBe("Planner archive checksum not recorded");
  expect(recordedPlannerChecksum(buildOperation({ engine: "nixpacks", planner_sha256: "md5:abc" }))).toBe("Planner archive checksum unavailable");
  expect(recordedPlannerChecksum(buildOperation({ engine: "railpack", planner_sha256: digest }))).toBeUndefined();
  expect(recordedPlannerChecksum(buildOperation())).toBeUndefined();
});

it("formats small downloads as decimal megabytes", () => {
  expect(formatMB(3717141)).toBe("3.7 MB");
  expect(formatMB(4111227)).toBe("4.1 MB");
});

import { expect, it } from "vitest";
import { parseBuildRecipeSettings } from "../api/build-recipes";
import type { Operation, OperationBuildRecipe } from "../api/types";
import { commandError, composeServices, pathError, recipePayload, recordedEngine } from "./build-recipes";

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

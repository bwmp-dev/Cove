import { isMap, isScalar, parseDocument } from "yaml";
import type { BuildRecipe } from "../api/build-recipes";
import type { Operation } from "../api/types";
import { workerResult } from "./operations";
import { byteLength } from "./workload-form";

export type RecipeEngine = "compose" | "static" | "railpack";

export const engineLabels: Record<RecipeEngine, string> = {
  compose: "Existing Compose / Dockerfile",
  static: "Static output",
  railpack: "Railpack",
};

export const commandMaxBytes = 8192;
export const pathMaxBytes = 512;

export const railpackDisclosure =
  "Railpack plans and builds with default build networking and downloads metadata, language toolchains and packages from upstream services. Cove pins the Railpack frontend, builder and runtime images, but those three pins do not pin toolchains or packages, so Railpack builds are neither reproducible nor fully pinned.";

export function isRecipeEngine(value: string): value is RecipeEngine {
  return value === "compose" || value === "static" || value === "railpack";
}

export function engineLabel(engine: string) {
  return isRecipeEngine(engine) ? engineLabels[engine] : `Unknown engine (${engine})`;
}

export const emptyRecipe: BuildRecipe = { engine: "compose", service: "", directory: "", output: "", build_command: "", start_command: "" };

// The API refuses fields that do not belong to the engine, so the payload clears them instead of sending what the draft still holds.
export function recipePayload(draft: BuildRecipe): BuildRecipe {
  if (draft.engine === "compose") return emptyRecipe;
  if (draft.engine === "static") return { ...emptyRecipe, engine: "static", service: draft.service, directory: draft.directory, output: draft.output };
  return { ...emptyRecipe, engine: draft.engine, service: draft.service, directory: draft.directory, build_command: draft.build_command, start_command: draft.start_command };
}

export function sameRecipe(a: BuildRecipe, b: BuildRecipe) {
  return a.engine === b.engine && a.service === b.service && a.directory === b.directory && a.output === b.output && a.build_command === b.build_command && a.start_command === b.start_command;
}

const forbiddenPathCharacters = /[\\:\0\r\n,"]/;

// Mirrors the server's clean relative path rule; "." means the checkout itself (or the selected directory for an output).
export function pathError(value: string, label: string) {
  if (value === "") return `Enter the ${label}.`;
  if (byteLength(value) > pathMaxBytes) return `Use at most ${pathMaxBytes} bytes.`;
  if (forbiddenPathCharacters.test(value)) return "Remove backslashes, colons, commas, double quotes and line breaks.";
  if (value.startsWith("/")) return "Use a path relative to the checkout, without a leading slash.";
  if (value === ".") return null;
  const segments = value.split("/");
  if (segments.includes("..")) return "Paths cannot contain .. segments.";
  if (segments.some((segment) => segment === "" || segment === ".")) return "Use a clean path without empty or . segments or a trailing slash.";
  return null;
}

export function commandError(value: string) {
  const bytes = byteLength(value);
  return bytes > commandMaxBytes ? `Use at most ${commandMaxBytes} bytes (currently ${bytes}).` : null;
}

export type RecipeErrors = Partial<Record<"service" | "directory" | "output" | "build_command" | "start_command", string>>;

export function recipeErrors(draft: BuildRecipe, isJob: boolean): RecipeErrors {
  const errors: RecipeErrors = {};
  if (draft.engine === "compose") return errors;
  if (draft.service === "") errors.service = "Choose a Compose service.";
  const directory = pathError(draft.directory, "source directory");
  if (directory) errors.directory = directory;
  if (draft.engine === "static") {
    if (isJob) errors.service = "Static output serves files from a long-running service, so jobs cannot use it.";
    const output = pathError(draft.output, "output directory");
    if (output) errors.output = output;
  }
  if (draft.engine === "railpack") {
    const build = commandError(draft.build_command);
    if (build) errors.build_command = build;
    const start = commandError(draft.start_command);
    if (start) errors.start_command = start;
  }
  return errors;
}

// Service names from the saved Compose file. An unreadable file yields none and the saved choice is still shown.
export function composeServices(compose: string) {
  const document = parseDocument(compose, { logLevel: "silent" });
  if (document.errors.length > 0) return [];
  const services = document.get("services");
  if (!isMap(services)) return [];
  return services.items.flatMap((item) => (isScalar(item.key) && typeof item.key.value === "string" ? [item.key.value] : []));
}

// What the build recorded, never the saved setting: older builds have no record and are not assumed to be Compose.
export function recordedEngine(operation: Operation) {
  const recipe = workerResult(operation)?.build_recipe;
  if (!recipe?.engine) return "Build engine not recorded";
  const version = recipe.engine === "railpack" && recipe.version ? ` ${recipe.version}` : "";
  const service = recipe.service ? ` for ${recipe.service}` : "";
  return `${engineLabel(recipe.engine)}${version}${service}`;
}

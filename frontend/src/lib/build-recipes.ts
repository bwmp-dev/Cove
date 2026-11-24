import { isMap, isScalar, parseDocument } from "yaml";
import type { BuildRecipe, BuildRecipeSettings } from "../api/build-recipes";
import type { Operation } from "../api/types";
import { byteLength } from "./workload-form";

export type RecipeEngine = "compose" | "static" | "railpack" | "buildpacks" | "nixpacks";

export const recipeEngines: readonly RecipeEngine[] = ["compose", "static", "railpack", "buildpacks", "nixpacks"];

export const engineLabels: Record<RecipeEngine, string> = {
  compose: "Existing Compose / Dockerfile",
  static: "Static output",
  railpack: "Railpack",
  buildpacks: "Cloud Native Buildpacks",
  nixpacks: "Nixpacks",
};

export const commandMaxBytes = 8192;
export const pathMaxBytes = 512;

export const railpackDisclosure =
  "Railpack plans and builds with default build networking and downloads metadata, language toolchains and packages from upstream services. Cove pins the Railpack frontend, builder and runtime images, but those three pins do not pin toolchains or packages, so Railpack builds are neither reproducible nor fully pinned.";

export function isRecipeEngine(value: string): value is RecipeEngine {
  return (recipeEngines as readonly string[]).includes(value);
}

export function engineLabel(engine: string) {
  return isRecipeEngine(engine) ? engineLabels[engine] : `Unknown engine (${engine})`;
}

export const emptyRecipe: BuildRecipe = { engine: "compose", service: "", directory: "", output: "", build_command: "", start_command: "" };

// The API refuses fields that do not belong to the engine, so the payload clears them instead of sending what the draft still holds.
export function recipePayload(draft: BuildRecipe): BuildRecipe {
  if (draft.engine === "compose") return emptyRecipe;
  if (draft.engine === "static") return { ...emptyRecipe, engine: "static", service: draft.service, directory: draft.directory, output: draft.output };
  if (draft.engine === "buildpacks") return { ...emptyRecipe, engine: "buildpacks", service: draft.service, directory: draft.directory };
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
  if (draft.engine === "railpack" || draft.engine === "nixpacks") {
    const build = commandError(draft.build_command);
    if (build) errors.build_command = build;
    const start = commandError(draft.start_command);
    if (start) errors.start_command = start;
  }
  return errors;
}

// Why the saved recipe cannot build now, or nothing when it can. Unknown Cloud Native Buildpacks or Nixpacks eligibility counts as unavailable.
export function savedRecipeBlocker(settings: BuildRecipeSettings) {
  const engine = settings.recipe.engine;
  if (engine === "compose") return undefined;
  if (!isRecipeEngine(engine)) return "This panel does not recognise the saved engine.";
  if (engine === "buildpacks") {
    if (settings.reason) return settings.reason;
    if (!settings.buildpacks) return "This server did not report Cloud Native Buildpacks eligibility, so it is treated as unavailable.";
    if (!settings.buildpacks.supported) return settings.buildpacks.reason || "The build node cannot run Cloud Native Buildpacks builds.";
    return undefined;
  }
  if (engine === "nixpacks") {
    if (settings.reason) return settings.reason;
    if (!settings.nixpacks) return "This server did not report usable Nixpacks eligibility, so it is treated as unavailable.";
    if (!settings.nixpacks.supported) return settings.nixpacks.reason || "The build node cannot run Nixpacks builds.";
    return undefined;
  }
  if (!settings.supported || settings.reason) return settings.reason || "The build worker does not support this recipe.";
  return undefined;
}

// Decimal gigabytes, for download sizes that registries report in bytes.
export function formatGB(bytes: number) {
  return `${(bytes / 1e9).toFixed(2)} GB`;
}

// Decimal megabytes, for downloads too small to read in gigabytes.
export function formatMB(bytes: number) {
  return `${(bytes / 1e6).toFixed(1)} MB`;
}

const mib = 1024 ** 2;
const gib = 1024 ** 3;

// Exact binary memory sizes: whole GiB, else whole MiB (the unit phase limits are rounded to), else bytes.
export function formatMemory(bytes: number) {
  if (bytes % gib === 0) return `${(bytes / gib).toLocaleString("en-US")} GiB`;
  if (bytes % mib === 0) return `${(bytes / mib).toLocaleString("en-US")} MiB`;
  return `${bytes.toLocaleString("en-US")} bytes`;
}

export function formatCPU(cpus: number) {
  return `${cpus.toLocaleString("en-US", { maximumFractionDigits: 3 })} ${cpus === 1 ? "CPU" : "CPUs"}`;
}

const recordedBytes = (value: unknown): value is number => typeof value === "number" && Number.isSafeInteger(value) && value > 0;

// The phase memory limit this Cloud Native Buildpacks build recorded at admission. Never a default: absent means not yet assigned.
export function recordedPhaseMemory(operation: Operation) {
  const recipe = operation.result?.build_recipe;
  if (recipe?.engine !== "buildpacks") return undefined;
  const limit: unknown = recipe.phase_memory_bytes;
  if (limit === undefined) return "Phase memory not assigned";
  if (!recordedBytes(limit)) return "Phase memory limit unavailable";
  const available: unknown = recipe.admission_available_memory_bytes;
  if (available === undefined) return `Phase memory limit ${formatMemory(limit)}`;
  if (!recordedBytes(available)) return `Phase memory limit ${formatMemory(limit)} (available memory at admission unavailable)`;
  return `Phase memory limit ${formatMemory(limit)} (from ${formatMemory(available)} available at admission)`;
}

const recordedDigest = /^sha256:[0-9a-f]{64}$/;

// The planner release archive checksum this Nixpacks build recorded. Never taken from today's settings: absent means not recorded.
export function recordedPlannerChecksum(operation: Operation) {
  const recipe = operation.result?.build_recipe;
  if (recipe?.engine !== "nixpacks") return undefined;
  const checksum: unknown = recipe.planner_sha256;
  if (checksum === undefined || checksum === "") return "Planner archive checksum not recorded";
  if (typeof checksum !== "string" || !recordedDigest.test(checksum)) return "Planner archive checksum unavailable";
  return `Planner archive checksum ${checksum}`;
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
  const recipe = operation.result?.build_recipe;
  if (!recipe?.engine) return "Build engine not recorded";
  const version = (recipe.engine === "railpack" || recipe.engine === "nixpacks") && recipe.version ? ` ${recipe.version}` : "";
  const service = recipe.service ? ` for ${recipe.service}` : "";
  if (recipe.engine === "buildpacks") {
    const parts = [recipe.version && `lifecycle ${recipe.version}`, recipe.builder_version && `builder ${recipe.builder_version}`, recipe.platform_api && `platform API ${recipe.platform_api}`].filter(Boolean);
    return `${engineLabels.buildpacks}${parts.length > 0 ? ` (${parts.join(", ")})` : ""}${service}`;
  }
  return `${engineLabel(recipe.engine)}${version}${service}`;
}

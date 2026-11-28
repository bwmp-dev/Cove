import { queryOptions, useMutation, useQueryClient } from "@tanstack/react-query";
import { request } from "./client";
import { keys } from "./queries";

// Kept as a plain string so an engine this panel does not know stays visible instead of being read as Compose.
export interface BuildRecipe {
  engine: string;
  service: string;
  directory: string;
  output: string;
  build_command: string;
  start_command: string;
}

// Cloud Native Buildpacks eligibility and resource needs, as the server reports them. The panel never derives these itself.
export interface BuildpacksSettings {
  supported: boolean;
  reason: string;
  lifecycle_version: string;
  builder_version: string;
  platform_api: string;
  pinned_images: string[];
  // Operation-owned disk state with a monitored target, not a hard storage cap. Other modes are not recognised.
  work_storage: "disk";
  work_hard_cap: false;
  work_target_bytes: number;
  // Each build's phase limit is set at admission from available memory minus the safety headroom, clamped to this range.
  minimum_phase_memory_bytes: number;
  maximum_phase_memory_bytes: number;
  memory_safety_bytes: number;
  required_available_memory_bytes: number;
  required_docker_disk_bytes: number;
  required_worker_disk_bytes: number;
  // The most a build reserves against team budgets, helpers included.
  quota_memory_mb: number;
  quota_cpu: number;
  builder_download_bytes: number;
  builder_unpacked_bytes: number;
  experimental: boolean;
}

// Nixpacks eligibility, pins and planner limits, as the server reports them for the effective build node's architecture.
export interface NixpacksSettings {
  supported: boolean;
  reason: string;
  version: string;
  // Checksum of the pinned release archive. Empty, with zero download sizes, when the node's architecture has no pinned planner.
  planner_sha256: string;
  planner_download_bytes: number;
  base_image: string;
  base_download_bytes: number;
  // Fixed by the contract: the panel only describes a maintenance-mode engine without outbound proxy support.
  maintenance: true;
  proxy_supported: false;
  planner_memory_bytes: number;
  planner_cpu: number;
  planner_timeout_seconds: number;
  planner_output_bytes: number;
}

export interface BuildRecipeSettings {
  recipe: BuildRecipe;
  can_edit: boolean;
  source_ready: boolean;
  supported: boolean;
  reason: string;
  build_node_id: string;
  active_operation_id: string;
  // Readers without edit access get empty command strings, so these say whether an override is saved.
  build_command_set: boolean;
  start_command_set: boolean;
  railpack_version: string;
  pinned_images: string[];
  // Missing from older servers or malformed: Cloud Native Buildpacks eligibility is then unknown and treated as unavailable.
  buildpacks?: BuildpacksSettings;
  // Missing from older servers or malformed: Nixpacks eligibility is then unknown and treated as unavailable.
  nixpacks?: NixpacksSettings;
}

const path = (workloadId: string) => `/api/workloads/${encodeURIComponent(workloadId)}/build-recipe`;

export const buildRecipeKey = (workloadId: string) => [...keys.workload(workloadId), "build-recipe"] as const;

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}

const incomplete = () => new Error("The build recipe settings were incomplete.");

function text(record: Record<string, unknown>, key: string) {
  const value = record[key];
  if (typeof value !== "string") throw incomplete();
  return value;
}

function flag(record: Record<string, unknown>, key: string) {
  const value = record[key];
  if (typeof value !== "boolean") throw incomplete();
  return value;
}

function strings(value: unknown) {
  if (!Array.isArray(value)) throw incomplete();
  return value.map((item) => {
    if (typeof item !== "string") throw incomplete();
    return item;
  });
}

function size(record: Record<string, unknown>, key: string) {
  const value = record[key];
  if (typeof value !== "number" || !Number.isSafeInteger(value) || value <= 0) throw incomplete();
  return value;
}

function positive(record: Record<string, unknown>, key: string) {
  const value = record[key];
  if (typeof value !== "number" || !Number.isFinite(value) || value <= 0) throw incomplete();
  return value;
}

// Unlike the rest of the read, an unusable buildpacks object leaves the older engines usable; it only makes this engine's eligibility unknown.
function parseBuildpacks(value: unknown): BuildpacksSettings | undefined {
  if (!isRecord(value)) return undefined;
  if (value.work_storage !== "disk" || value.work_hard_cap !== false) return undefined;
  try {
    const minimum = size(value, "minimum_phase_memory_bytes");
    const maximum = size(value, "maximum_phase_memory_bytes");
    if (minimum > maximum) return undefined;
    return {
      supported: flag(value, "supported"),
      reason: text(value, "reason"),
      lifecycle_version: text(value, "lifecycle_version"),
      builder_version: text(value, "builder_version"),
      platform_api: text(value, "platform_api"),
      pinned_images: strings(value.pinned_images),
      work_storage: "disk",
      work_hard_cap: false,
      work_target_bytes: size(value, "work_target_bytes"),
      minimum_phase_memory_bytes: minimum,
      maximum_phase_memory_bytes: maximum,
      memory_safety_bytes: size(value, "memory_safety_bytes"),
      required_available_memory_bytes: size(value, "required_available_memory_bytes"),
      required_docker_disk_bytes: size(value, "required_docker_disk_bytes"),
      required_worker_disk_bytes: size(value, "required_worker_disk_bytes"),
      quota_memory_mb: size(value, "quota_memory_mb"),
      quota_cpu: positive(value, "quota_cpu"),
      builder_download_bytes: size(value, "builder_download_bytes"),
      builder_unpacked_bytes: size(value, "builder_unpacked_bytes"),
      experimental: flag(value, "experimental"),
    };
  } catch {
    return undefined;
  }
}

function count(record: Record<string, unknown>, key: string) {
  const value = record[key];
  if (typeof value !== "number" || !Number.isSafeInteger(value) || value < 0) throw incomplete();
  return value;
}

const plannerDigest = /^sha256:[0-9a-f]{64}$/;
const pinnedImage = /^[^\s@]+@sha256:[0-9a-f]{64}$/;

// As with buildpacks, an unusable object only makes Nixpacks eligibility unknown. A supported status must carry a real pin, checksum and sizes.
function parseNixpacks(value: unknown): NixpacksSettings | undefined {
  if (!isRecord(value)) return undefined;
  if (value.maintenance !== true || value.proxy_supported !== false) return undefined;
  try {
    const settings: NixpacksSettings = {
      supported: flag(value, "supported"),
      reason: text(value, "reason"),
      version: text(value, "version"),
      planner_sha256: text(value, "planner_sha256"),
      planner_download_bytes: count(value, "planner_download_bytes"),
      base_image: text(value, "base_image"),
      base_download_bytes: count(value, "base_download_bytes"),
      maintenance: true,
      proxy_supported: false,
      planner_memory_bytes: size(value, "planner_memory_bytes"),
      planner_cpu: positive(value, "planner_cpu"),
      planner_timeout_seconds: size(value, "planner_timeout_seconds"),
      planner_output_bytes: size(value, "planner_output_bytes"),
    };
    if (settings.version === "" || !pinnedImage.test(settings.base_image)) return undefined;
    if (settings.planner_sha256 !== "" && !plannerDigest.test(settings.planner_sha256)) return undefined;
    const pinned = settings.planner_sha256 !== "" && settings.planner_download_bytes > 0 && settings.base_download_bytes > 0;
    if (settings.supported && !pinned) return undefined;
    return settings;
  } catch {
    return undefined;
  }
}

// A response missing any field cannot say what the next build does, so it fails the read rather than defaulting to Compose.
export function parseBuildRecipeSettings(data: unknown): BuildRecipeSettings {
  if (!isRecord(data) || !isRecord(data.recipe)) throw incomplete();
  const recipe = data.recipe;
  const engine = text(recipe, "engine");
  if (engine === "") throw incomplete();
  const buildpacks = parseBuildpacks(data.buildpacks);
  const nixpacks = parseNixpacks(data.nixpacks);
  return {
    recipe: {
      engine,
      service: text(recipe, "service"),
      directory: text(recipe, "directory"),
      output: text(recipe, "output"),
      build_command: text(recipe, "build_command"),
      start_command: text(recipe, "start_command"),
    },
    can_edit: flag(data, "can_edit"),
    source_ready: flag(data, "source_ready"),
    supported: flag(data, "supported"),
    reason: text(data, "reason"),
    build_node_id: text(data, "build_node_id"),
    active_operation_id: text(data, "active_operation_id"),
    build_command_set: flag(data, "build_command_set"),
    start_command_set: flag(data, "start_command_set"),
    railpack_version: text(data, "railpack_version"),
    pinned_images: strings(data.pinned_images),
    ...(buildpacks ? { buildpacks } : {}),
    ...(nixpacks ? { nixpacks } : {}),
  };
}

export const buildRecipeQuery = (workloadId: string) =>
  queryOptions({
    queryKey: buildRecipeKey(workloadId),
    queryFn: async ({ signal }) => parseBuildRecipeSettings(await request<unknown>("GET", path(workloadId), undefined, signal)),
    refetchInterval: (query) => (query.state.status === "error" ? false : 30_000),
    retry: false,
  });

export function useSaveBuildRecipe(workloadId: string) {
  const client = useQueryClient();
  return useMutation({
    mutationFn: (recipe: BuildRecipe) =>
      request<{ ok: boolean }>("PUT", path(workloadId), {
        engine: recipe.engine,
        service: recipe.service,
        directory: recipe.directory,
        output: recipe.output,
        build_command: recipe.build_command,
        start_command: recipe.start_command,
      }),
    // The saved recipe is written to the cache straight away so the form shows it while the refetch (not awaited) runs.
    onSuccess: (_, recipe) => {
      client.setQueryData<BuildRecipeSettings>(buildRecipeKey(workloadId), (current) =>
        current ? { ...current, recipe, build_command_set: recipe.build_command !== "", start_command_set: recipe.start_command !== "" } : current,
      );
    },
    onSettled: () => {
      void client.invalidateQueries({ queryKey: buildRecipeKey(workloadId), exact: true });
      void client.invalidateQueries({ queryKey: keys.operations });
    },
  });
}

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

// A response missing any field cannot say what the next build does, so it fails the read rather than defaulting to Compose.
export function parseBuildRecipeSettings(data: unknown): BuildRecipeSettings {
  if (!isRecord(data) || !isRecord(data.recipe)) throw incomplete();
  const recipe = data.recipe;
  const engine = text(recipe, "engine");
  if (engine === "") throw incomplete();
  const pinned = data.pinned_images;
  if (!Array.isArray(pinned)) throw incomplete();
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
    pinned_images: pinned.map((image) => {
      if (typeof image !== "string") throw incomplete();
      return image;
    }),
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

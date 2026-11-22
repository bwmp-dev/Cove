import type { EnvironmentPreview, PlannedResource, PreviewPlan, PreviewRecipe, PreviewResourcePolicy, PreviewStage, PreviewState, RecipeView, VariableMode } from "../api/previews";
import type { Tone } from "./nodes";

export const previewStates: Record<PreviewState, { tone: Tone; label: string }> = {
  awaiting_approval: { tone: "warn", label: "Awaiting approval" },
  creating: { tone: "info", label: "Creating" },
  ready: { tone: "ok", label: "Ready" },
  updating: { tone: "info", label: "Updating" },
  failed: { tone: "bad", label: "Failed" },
  deleting: { tone: "info", label: "Deleting" },
};

export const stageLabels: Record<PreviewStage, string> = {
  approval: "Waiting for an editor to approve",
  plan: "Pinning production revisions",
  data: "Copying production data",
  connect: "Connecting preview resources",
  deploy: "Deploying",
  publish: "Publishing protected routes",
  ready: "Running",
  remove: "Removing",
};

export function shortRevision(revision: string) {
  return revision ? revision.slice(0, 7) : "";
}

// Sets the editable fields from the saved recipe. The plan's environment wins when the saved recipe has none yet.
export function draftFromView(view: RecipeView, environmentId: string): PreviewRecipe {
  const recipe = structuredClone(view.recipe);
  if (environmentId) recipe.base_environment_id = environmentId;
  recipe.resources ??= {};
  recipe.repositories ??= [];
  return recipe;
}

export function updatePolicy(recipe: PreviewRecipe, id: string, update: (policy: PreviewResourcePolicy) => PreviewResourcePolicy): PreviewRecipe {
  return { ...recipe, resources: { ...recipe.resources, [id]: update({ ...recipe.resources[id] }) } };
}

export function setVariable(recipe: PreviewRecipe, id: string, key: string, mode: VariableMode | "", value?: string): PreviewRecipe {
  return updatePolicy(recipe, id, (policy) => {
    const variables = { ...policy.variables };
    if (mode === "") {
      delete variables[key];
    } else {
      const previous = variables[key];
      // Keep the saved override marker so leaving the value empty keeps the server's value.
      variables[key] = { mode, ...(mode === "override" ? { value: value ?? previous?.value ?? "", has_value: previous?.has_value } : {}) };
    }
    return { ...policy, variables };
  });
}

export function setOmitted(recipe: PreviewRecipe, id: string, key: string, omitted: boolean): PreviewRecipe {
  return updatePolicy(recipe, id, (policy) => {
    const external = { ...policy.external };
    if (omitted) external[key] = "omit";
    else delete external[key];
    return { ...policy, external };
  });
}

// The request body: override values are only sent when typed, and the has_value marker stays client-side.
export function recipeForSave(recipe: PreviewRecipe): PreviewRecipe {
  const resources: PreviewRecipe["resources"] = {};
  for (const [id, policy] of Object.entries(recipe.resources)) {
    const variables: NonNullable<PreviewResourcePolicy["variables"]> = {};
    for (const [key, variable] of Object.entries(policy.variables ?? {})) {
      variables[key] = variable.mode === "override" && variable.value ? { mode: "override", value: variable.value } : { mode: variable.mode };
    }
    resources[id] = { ...policy, variables };
  }
  return { ...recipe, resources };
}

export function unresolved(plan: PreviewPlan | null | undefined) {
  return (plan?.resources ?? []).filter((resource) => resource.included).reduce((total, resource) => total + resource.blockers.length, 0);
}

export function missingOverrides(recipe: PreviewRecipe) {
  const missing: string[] = [];
  for (const policy of Object.values(recipe.resources)) {
    for (const [key, variable] of Object.entries(policy.variables ?? {})) {
      if (variable.mode === "override" && !variable.value && !variable.has_value) missing.push(key);
    }
  }
  return missing;
}

export function resourceSource(resource: PlannedResource) {
  if (resource.kind === "job") return "Job: disabled unless its schedule is enabled";
  if (resource.follows_branch) return "Matching branch, otherwise the deployed production commit";
  return "Deployed production revision, pinned when the preview is created";
}

export function previewName(preview: Pick<EnvironmentPreview, "slug">) {
  return preview.slug;
}

export function branchSummary(preview: EnvironmentPreview) {
  const present = preview.branches.filter((branch) => branch.head !== "").length;
  return `${present} of ${preview.branches.length} ${preview.branches.length === 1 ? "branch" : "branches"} present`;
}

export function formatBytes(bytes: number) {
  if (bytes < 1024) return `${bytes} B`;
  const units = ["KiB", "MiB", "GiB", "TiB"];
  let value = bytes / 1024;
  let unit = 0;
  while (value >= 1024 && unit < units.length - 1) {
    value /= 1024;
    unit++;
  }
  return `${value.toFixed(value < 10 ? 1 : 0)} ${units[unit]}`;
}

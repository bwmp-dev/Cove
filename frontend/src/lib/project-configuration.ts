import { stringify } from "yaml";
import type { Project } from "../api/projects";
import type { ConfigurationDraft, ProjectConfiguration, ProjectConfigurationManifest, WorkloadDrift } from "../api/project-configuration";
import type { WorkloadSummary } from "../api/types";

export const manifestLimitBytes = 2 * 1024 * 1024;

export function manifestYaml(manifest: ProjectConfigurationManifest) {
  return stringify(manifest, { lineWidth: 0 });
}

// Diff values arrive as JSON-encoded strings. Show strings bare and everything else as compact JSON.
export function formatDiffValue(raw: string) {
  if (raw === "") return "Not set";
  let value: unknown;
  try {
    value = JSON.parse(raw);
  } catch {
    return raw;
  }
  if (value === null || value === "") return "Not set";
  if (typeof value === "string") return value;
  if (Array.isArray(value) && value.length === 0) return "None";
  return JSON.stringify(value);
}

export type DraftState = "applied" | "stale" | "ready";

// A draft saved against an older configuration is refused at apply time; say so before anyone tries.
export function draftState(draft: ConfigurationDraft, currentVersion: string | undefined): DraftState {
  if (draft.applied_at) return "applied";
  if (currentVersion !== undefined && draft.base_version !== currentVersion) return "stale";
  return "ready";
}

export function newestFirst(drafts: readonly ConfigurationDraft[]) {
  return drafts.toSorted((a, b) => Date.parse(b.created_at) - Date.parse(a.created_at));
}

export type DriftResult = "no-baseline" | "busy" | "suspended" | "clean" | "drifted";

// Only a recorded baseline with a completed inspection and no changes counts as clean. A busy worker
// reports baseline:false because it skipped loading one, so busy must be checked first.
export function driftResult(drift: WorkloadDrift): DriftResult {
  if (drift.deferred === "suspended") return "suspended";
  if (drift.busy) return "busy";
  if (!drift.baseline) return "no-baseline";
  return (drift.changes ?? []).length === 0 ? "clean" : "drifted";
}

// Editable persistent resources of this project that the manifest declares with a Git source. Resource names
// are not unique and manifest keys can differ from workload IDs, so matching goes only through resource_ids.
export function gitOpsCandidates(project: Project, resources: readonly WorkloadSummary[], configuration: ProjectConfiguration) {
  const ids = configuration.resource_ids ?? {};
  const withSource = new Set((configuration.manifest.resources ?? []).filter((resource) => resource.configuration.source).flatMap((resource) => (ids[resource.key] ? [ids[resource.key]] : [])));
  const previews = new Set(project.environments.filter((environment) => environment.preview_id).map((environment) => environment.id));
  return resources.filter((resource) => withSource.has(resource.id) && resource.project_id === project.id && !(resource.environment_id && previews.has(resource.environment_id)) && resource.permissions.includes("edit"));
}

export function exportFilename(serverName: string | null, project: string, environment: string | undefined) {
  if (serverName) return serverName;
  const slug = [project, environment].filter(Boolean).join("-").toLowerCase().replace(/[^a-z0-9]+/g, "-").replace(/^-|-$/g, "");
  return `${slug || "project"}-configuration.zip`;
}

export function shortRevision(revision: string) {
  return /^[0-9a-f]{12,}$/i.test(revision) ? revision.slice(0, 7) : revision;
}

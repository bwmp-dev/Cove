import { adoptionSelectionLimit, plannedLifetimeMs, type AdoptionStatus, type AdoptionSummary, type DiscoveredProject } from "../api/adoptions";
import type { Tone } from "./nodes";
import { resourceKey, suggestKey } from "./migration-imports";

export interface AdoptionDraft {
  selected: boolean;
  key: string;
  name: string;
}

export type AdoptionDraftErrors = Partial<Record<"key" | "name", string>>;

export const adoptionStatuses: Record<AdoptionStatus, { tone: Tone; label: string }> = {
  planned: { tone: "neutral", label: "Reviewed, not started" },
  applying: { tone: "warn", label: "Pending" },
  complete: { tone: "ok", label: "Adopted" },
};

export function isBlocked(project: DiscoveredProject) {
  return (project.blockers ?? []).length > 0;
}

// Suggests a key when a project is first selected, avoiding keys already in the destination and in other selections.
export function selectProject(drafts: Record<string, AdoptionDraft>, project: string, selected: boolean, existingKeys: ReadonlySet<string>): Record<string, AdoptionDraft> {
  const current = drafts[project] ?? { selected: false, key: "", name: "" };
  if (!selected || current.key !== "") return { ...drafts, [project]: { ...current, selected } };
  const taken = new Set(existingKeys);
  for (const [other, draft] of Object.entries(drafts)) {
    if (other !== project && draft.selected) taken.add(draft.key);
  }
  return { ...drafts, [project]: { ...current, selected, key: suggestKey(project, taken) } };
}

export function selectedProjects(drafts: Record<string, AdoptionDraft>) {
  return Object.entries(drafts)
    .filter(([, draft]) => draft.selected)
    .map(([project]) => project);
}

export function adoptionDraftErrors(drafts: Record<string, AdoptionDraft>, existingKeys: ReadonlySet<string>): Record<string, AdoptionDraftErrors> {
  const counts = new Map<string, number>();
  for (const draft of Object.values(drafts)) {
    if (draft.selected) counts.set(draft.key, (counts.get(draft.key) ?? 0) + 1);
  }
  const result: Record<string, AdoptionDraftErrors> = {};
  for (const [project, draft] of Object.entries(drafts)) {
    if (!draft.selected) continue;
    const errors: AdoptionDraftErrors = {};
    if (!resourceKey.test(draft.key)) errors.key = "Use 1–128 letters, digits, dots, dashes or underscores, starting with a letter or digit.";
    else if (existingKeys.has(draft.key)) errors.key = "This key is already used in the destination project. Adoption only creates new resources.";
    else if ((counts.get(draft.key) ?? 0) > 1) errors.key = "Each adopted project needs its own key.";
    if (draft.name.trim().length > 120) errors.name = "Use at most 120 characters.";
    if (Object.keys(errors).length > 0) result[project] = errors;
  }
  return result;
}

export function selectionError(count: number) {
  if (count === 0) return "Choose at least one Compose project without blockers.";
  if (count > adoptionSelectionLimit) return `Choose at most ${adoptionSelectionLimit} projects per review.`;
  return "";
}

export function plannedExpiry(summary: AdoptionSummary) {
  return new Date(Date.parse(summary.created_at) + plannedLifetimeMs).toISOString();
}

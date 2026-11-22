import type { Environment, Project } from "../api/projects";
import type { WorkloadSummary } from "../api/types";

export interface Placement {
  project: Project;
  environment?: Environment;
}

export function findPlacement(projects: Project[] | undefined, environmentId: string | undefined, projectId?: string): Placement | undefined {
  const project = projects?.find((item) => projectId ? item.id === projectId : item.environments.some((environment) => environment.id === environmentId));
  if (!project) return undefined;
  return { project, environment: project.environments.find((item) => item.id === environmentId) };
}

// Environments people create. Preview environments are created and removed by their preview.
export function productionEnvironments(project: Project) {
  return project.environments.filter((environment) => !environment.preview_id);
}

// Comparing needs read access to both environments, not to the project.
export function canCompareEnvironments(project: Project) {
  return productionEnvironments(project).filter((environment) => environment.permissions.includes("read")).length >= 2;
}

export function previewEnvironments(project: Project) {
  return project.environments.filter((environment) => environment.preview_id);
}

export function resourcesIn(resources: WorkloadSummary[], projectId: string) {
  return resources.filter((resource) => resource.project_id === projectId);
}

export function nodeNames(resources: WorkloadSummary[]) {
  return [...new Set(resources.map((resource) => resource.node))].sort((a, b) => a.localeCompare(b));
}

export function countLabel(count: number, singular: string, plural = `${singular}s`) {
  return `${count} ${count === 1 ? singular : plural}`;
}

export const suggestedEnvironments = ["production", "staging", "development"] as const;

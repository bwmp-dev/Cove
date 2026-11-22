import type { Project } from "../api/projects";
import type { Grant } from "../api/types";

export type GrantRow = Grant & { key: string };

export function toGrantRows(grants: Grant[]): GrantRow[] {
  return grants.map((grant) => ({ ...grant, key: crypto.randomUUID() }));
}

export function fromGrantRows(rows: GrantRow[]): Grant[] {
  return rows.map(({ scope, scope_id, role }) => ({ scope, scope_id, role }));
}

export function describeScope(grant: Grant, projects: Project[] | undefined, teamName: string) {
  if (grant.scope === "team") return `All of ${teamName}`;
  for (const project of projects ?? []) {
    if (grant.scope === "project" && project.id === grant.scope_id) return project.name;
    const environment = project.environments.find((item) => item.id === grant.scope_id);
    if (grant.scope === "environment" && environment) return `${project.name} / ${environment.name}`;
  }
  return grant.scope === "project" ? "Project no longer available" : "Environment no longer available";
}

export const scopeValue = (grant: Pick<Grant, "scope" | "scope_id">) => `${grant.scope}:${grant.scope_id}`;

export function parseScope(value: string): Pick<Grant, "scope" | "scope_id"> {
  const separator = value.indexOf(":");
  return { scope: value.slice(0, separator) as Grant["scope"], scope_id: value.slice(separator + 1) };
}


import type { Session } from "../api/queries";
import type { GrantRole, Permission, TeamMembership, TeamRole } from "../api/types";

export function can(permissions: readonly Permission[] | undefined, needed: Permission) {
  return permissions?.includes(needed) ?? false;
}

// Instance administrators can work in a team they are not a member of; they hold full access there.
export function currentTeam(session: Session | null | undefined): TeamMembership | undefined {
  if (!session) return undefined;
  const membership = session.teams.find((team) => team.id === session.team_id);
  if (membership || !session.is_admin || !session.team_id) return membership;
  return { id: session.team_id, name: session.team_id === "default" ? "Default team" : "Selected team", role: "admin", grants: [] };
}

export function managesTeam(team: TeamMembership | undefined) {
  return team?.role === "owner" || team?.role === "admin";
}

// Team credentials and project creation both require team-wide editing rights.
export function canManageIntegrations(session: Session | null | undefined) {
  const team = currentTeam(session);
  if (session?.is_admin || managesTeam(team)) return true;
  return team?.grants.some((grant) => grant.scope === "team" && grant.role === "editor") ?? false;
}

export const teamRoleLabels: Record<TeamRole, string> = { owner: "Owner", admin: "Admin", member: "Member" };

export const grantRoleLabels: Record<GrantRole, string> = { viewer: "Viewer", operator: "Operator", editor: "Editor" };

export const grantRoleDescriptions: Record<GrantRole, string> = {
  viewer: "Read resources, logs and operations.",
  operator: "Viewer access plus deploy, start, stop, restart, rollback, backup and cancel.",
  editor: "Operator access plus creating, configuring, restoring and deleting resources.",
};

export const teamRoleDescriptions: Record<TeamRole, string> = {
  owner: "Full access to the team, its projects and its members.",
  admin: "Full access to the team, its projects and its members.",
  member: "Access limited to the grants below.",
};

export const canCreateProjects = canManageIntegrations;

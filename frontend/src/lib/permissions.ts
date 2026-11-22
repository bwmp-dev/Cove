import type { Session } from "../api/queries";
import type { GrantRole, Permission, TeamMembership, TeamPermission, TeamRole } from "../api/types";

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

export function hasTeamPermission(session: Session | null | undefined, needed: TeamPermission) {
  return session?.permissions.includes(needed) ?? false;
}

// The full built-in editor set: legacy editor-level checks require every one of these.
export const editorPermissions: readonly TeamPermission[] = ["view", "operate", "read_secrets", "edit_configuration", "manage_backups"];

// Team credentials and project creation both require team-wide editor-level access, which a custom role only
// reaches with the complete editor set.
export function canManageIntegrations(session: Session | null | undefined) {
  if (session?.is_admin || managesTeam(currentTeam(session))) return true;
  return editorPermissions.every((permission) => hasTeamPermission(session, permission));
}

// Owners, admins and instance administrators manage every member; a team-wide manage_members grant delegates
// managing ordinary members only.
export type MemberManagement = "full" | "delegated" | "none";

export function memberManagement(session: Session | null | undefined): MemberManagement {
  if (session?.is_admin || managesTeam(currentTeam(session))) return "full";
  return hasTeamPermission(session, "manage_members") ? "delegated" : "none";
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

export function isBuiltInRole(role: string): role is GrantRole {
  return Object.hasOwn(grantRoleLabels, role);
}

// Custom role names are only readable by team owners, admins and team-wide member managers, so other people see a generic label.
export function roleLabel(role: string, customRoles?: readonly { id: string; name: string }[]) {
  if (isBuiltInRole(role)) return grantRoleLabels[role];
  return customRoles?.find((item) => item.id === role)?.name ?? "Custom role";
}

export const permissionLabels: Record<string, { label: string; description: string }> = {
  view: { label: "View", description: "Read resources, logs and operations. Every role includes it." },
  read_secrets: { label: "Read secrets", description: "Read environment variables, credentials and other secret configuration." },
  operate: { label: "Operate", description: "Deploy, start, stop, restart, roll back and cancel operations." },
  edit_configuration: { label: "Edit configuration", description: "Create, configure and delete resources." },
  manage_backups: { label: "Manage backups", description: "Run backups and change backup policies. Restores and drills also need Edit configuration." },
  manage_members: { label: "Manage members", description: "Invite and edit members and their grants. Applies only to team-wide grants, and never to owners or admins." },
};

export function permissionLabel(permission: string) {
  return permissionLabels[permission]?.label ?? permission;
}

// Keeps permissions in catalogue order and never drops view, which the API requires on every role.
export function setPermission(permissions: readonly string[], permission: string, enabled: boolean, catalogue: readonly string[]) {
  if (permission === "view") enabled = true;
  const next = new Set(permissions);
  next.add("view");
  if (enabled) next.add(permission);
  else next.delete(permission);
  return catalogue.filter((item) => next.has(item));
}

export interface RoleDraftErrors {
  name?: string;
  permissions?: string;
}

export function validateRole(name: string, permissions: readonly string[], catalogue: readonly string[]): RoleDraftErrors {
  const errors: RoleDraftErrors = {};
  const trimmed = name.trim();
  if (trimmed === "") errors.name = "Enter a role name.";
  else if (isBuiltInRole(trimmed.toLowerCase())) errors.name = "Choose a name that differs from the built-in roles.";
  if (!permissions.includes("view")) errors.permissions = "Every role includes View.";
  else if (permissions.some((item) => !catalogue.includes(item))) errors.permissions = "Remove permissions this instance does not recognise.";
  return errors;
}

// Some workflows check for the complete editor set, so a partial custom role must not be presented as an editor.
export function roleCaveats(permissions: readonly string[], editor: readonly string[]) {
  const caveats: string[] = [];
  if (permissions.includes("manage_backups") && !permissions.includes("edit_configuration")) {
    caveats.push("Can run backups but not restores or drills, which also need Edit configuration.");
  }
  const missing = editor.filter((item) => !permissions.includes(item));
  if (permissions.includes("edit_configuration") && missing.length > 0) {
    caveats.push(`Not a full editor: workflows that need editor access also require ${missing.map(permissionLabel).join(", ")}.`);
  }
  return caveats;
}

export function sameRolePermissions(left: readonly string[], right: readonly string[]) {
  return left.length === right.length && left.every((item) => right.includes(item));
}

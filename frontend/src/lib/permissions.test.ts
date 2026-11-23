import { describe, expect, it } from "vitest";
import type { Session } from "../api/queries";
import { backupRights, can, canCreateProjects, canManageIntegrations, canManageTeamDestinations, currentTeam, managesTeam, memberManagement, roleCaveats, roleLabel, setPermission, validateRole } from "./permissions";

function session(overrides: Partial<Session> = {}): Session {
  return {
    user: "Ada",
    user_id: "u1",
    email: "ada@example.com",
    name: "Ada",
    is_admin: false,
    has_password: true,
    github_linked: false,
    team_id: "t1",
    teams: [{ id: "t1", name: "Core", role: "member", grants: [] }],
    permissions: ["view"],
    ...overrides,
  };
}

describe("permissions", () => {
  it("checks explicit resource permissions", () => {
    expect(can(["read", "operate"], "operate")).toBe(true);
    expect(can(["read"], "edit")).toBe(false);
    expect(can(undefined, "read")).toBe(false);
  });

  it("resolves the selected team", () => {
    const value = session({ team_id: "t2", teams: [{ id: "t1", name: "Core", role: "member", grants: [] }, { id: "t2", name: "Ops", role: "admin", grants: [] }] });
    expect(currentTeam(value)?.name).toBe("Ops");
    expect(managesTeam(currentTeam(value))).toBe(true);
  });

  it("lets team-wide Edit configuration create projects without the rest of the editor set", () => {
    expect(canCreateProjects(session())).toBe(false);
    expect(canCreateProjects(session({ permissions: ["view", "edit_configuration"] }))).toBe(true);
    expect(canCreateProjects(session({ permissions: ["view", "operate", "read_secrets", "manage_backups"] }))).toBe(false);
    expect(canCreateProjects(session({ permissions: ["view", "operate", "read_secrets", "edit_configuration", "manage_backups"] }))).toBe(true);
    expect(canCreateProjects(session({ teams: [{ id: "t1", name: "Core", role: "admin", grants: [] }], permissions: [] }))).toBe(true);
    expect(canCreateProjects(session({ is_admin: true }))).toBe(true);
    // Team credentials stay with the full editor set.
    expect(canManageIntegrations(session({ permissions: ["view", "edit_configuration"] }))).toBe(false);
  });

  it("keeps notification channels and log forwarders with team admins who can edit and read secrets", () => {
    const admin = { teams: [{ id: "t1", name: "Core", role: "admin" as const, grants: [] }] };
    expect(canManageTeamDestinations(session({ ...admin, permissions: ["view", "edit_configuration", "read_secrets"] }))).toBe(true);
    expect(canManageTeamDestinations(session({ ...admin, permissions: ["view", "edit_configuration"] }))).toBe(false);
    expect(canManageTeamDestinations(session({ ...admin, permissions: ["view", "read_secrets"] }))).toBe(false);
    // A member with both rights is still not a team administrator.
    expect(canManageTeamDestinations(session({ permissions: ["view", "edit_configuration", "read_secrets"] }))).toBe(false);
    expect(canManageTeamDestinations(session({ is_admin: true, teams: [], permissions: ["view", "edit_configuration", "read_secrets"] }))).toBe(true);
  });

  it("delegates member management only through the team-wide permission", () => {
    expect(memberManagement(session())).toBe("none");
    expect(memberManagement(session({ permissions: ["view", "manage_members"] }))).toBe("delegated");
    expect(memberManagement(session({ teams: [{ id: "t1", name: "Core", role: "owner", grants: [] }] }))).toBe("full");
    expect(memberManagement(session({ is_admin: true, teams: [] }))).toBe("full");
  });
});

describe("custom roles", () => {
  const catalogue = ["view", "read_secrets", "operate", "edit_configuration", "manage_backups", "manage_members"];
  const editor = ["view", "operate", "read_secrets", "edit_configuration", "manage_backups"];

  it("labels built-in roles, known custom roles and unreadable custom roles", () => {
    expect(roleLabel("operator")).toBe("Operator");
    expect(roleLabel("r1", [{ id: "r1", name: "Release manager" }])).toBe("Release manager");
    expect(roleLabel("r2")).toBe("Custom role");
    expect(roleLabel("toString")).toBe("Custom role");
  });

  it("keeps view on every role and permissions in catalogue order", () => {
    expect(setPermission(["view"], "manage_members", true, catalogue)).toEqual(["view", "manage_members"]);
    expect(setPermission(["view", "manage_members"], "operate", true, catalogue)).toEqual(["view", "operate", "manage_members"]);
    expect(setPermission(["view", "operate"], "view", false, catalogue)).toEqual(["view", "operate"]);
    expect(setPermission([], "operate", false, catalogue)).toEqual(["view"]);
  });

  it("validates names and permissions before saving", () => {
    expect(validateRole("Deployers", ["view", "operate"], catalogue)).toEqual({});
    expect(validateRole(" ", ["view"], catalogue).name).toBeDefined();
    expect(validateRole("Editor", ["view"], catalogue).name).toBeDefined();
    expect(validateRole("Ops", ["operate"], catalogue).permissions).toBeDefined();
    expect(validateRole("Ops", ["view", "root"], catalogue).permissions).toBeDefined();
  });

  it("never presents a partial role as a full editor", () => {
    expect(roleCaveats(["view", "edit_configuration"], editor).some((caveat) => caveat.startsWith("Not a full editor"))).toBe(true);
    expect(roleCaveats(editor, editor)).toEqual([]);
    expect(roleCaveats(["view", "manage_backups"], editor)).toEqual(["Can run backups but not restores or drills, which also need Edit configuration."]);
  });

  it("warns that backup policy changes need Read secrets", () => {
    expect(roleCaveats(["view", "edit_configuration", "manage_backups"], editor)).toContain("Cannot change backup policies, which also need Read secrets because the destination and encryption control where data is exported.");
    expect(roleCaveats(["view", "read_secrets", "edit_configuration", "manage_backups"], editor).some((caveat) => caveat.startsWith("Cannot change backup policies"))).toBe(false);
  });
});

describe("backupRights", () => {
  it("requires Manage backups, Edit configuration and Read secrets to configure a policy", () => {
    expect(backupRights(["view", "manage_backups", "edit_configuration"])).toEqual({ backup: true, configure: false, restore: true, download: false, import: false });
    expect(backupRights(["view", "manage_backups", "edit_configuration", "read_secrets"])).toEqual({ backup: true, configure: true, restore: true, download: true, import: true });
    expect(backupRights(["view", "manage_backups", "read_secrets"])).toEqual({ backup: true, configure: false, restore: false, download: true, import: false });
    expect(backupRights(["view", "edit_configuration", "read_secrets"])).toEqual({ backup: false, configure: false, restore: false, download: false, import: false });
  });
});

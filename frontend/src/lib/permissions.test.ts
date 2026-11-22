import { describe, expect, it } from "vitest";
import type { Session } from "../api/queries";
import { can, canCreateProjects, currentTeam, managesTeam } from "./permissions";

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

  it("limits project creation to team managers, team editors and instance administrators", () => {
    expect(canCreateProjects(session())).toBe(false);
    expect(canCreateProjects(session({ teams: [{ id: "t1", name: "Core", role: "member", grants: [{ scope: "project", scope_id: "p1", role: "editor" }] }] }))).toBe(false);
    expect(canCreateProjects(session({ teams: [{ id: "t1", name: "Core", role: "member", grants: [{ scope: "team", scope_id: "t1", role: "editor" }] }] }))).toBe(true);
    expect(canCreateProjects(session({ is_admin: true }))).toBe(true);
  });
});

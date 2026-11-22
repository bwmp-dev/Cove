import { describe, expect, it } from "vitest";
import type { Project } from "../api/projects";
import { describeScope, fromGrantRows, parseScope, scopeValue, toGrantRows } from "./grants";

const projects: Project[] = [{ id: "p1", name: "Shop", permissions: [], environments: [{ id: "e1", name: "production", permissions: [] }] }];

describe("grants", () => {
  it("names each scope for people rather than by ID", () => {
    expect(describeScope({ scope: "team", scope_id: "t1", role: "viewer" }, projects, "Core")).toBe("All of Core");
    expect(describeScope({ scope: "project", scope_id: "p1", role: "editor" }, projects, "Core")).toBe("Shop");
    expect(describeScope({ scope: "environment", scope_id: "e1", role: "operator" }, projects, "Core")).toBe("Shop / production");
    expect(describeScope({ scope: "environment", scope_id: "gone", role: "viewer" }, projects, "Core")).toBe("Environment no longer available");
  });

  it("round-trips scope values and strips editor row keys", () => {
    expect(parseScope(scopeValue({ scope: "environment", scope_id: "e1" }))).toEqual({ scope: "environment", scope_id: "e1" });
    const grants = [{ scope: "project" as const, scope_id: "p1", role: "viewer" as const }];
    expect(fromGrantRows(toGrantRows(grants))).toEqual(grants);
  });
});

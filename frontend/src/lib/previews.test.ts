import { describe, expect, it } from "vitest";
import type { PreviewRecipe } from "../api/previews";
import { formatBytes, missingOverrides, recipeForSave, setOmitted, setVariable, unresolved } from "./previews";

const recipe: PreviewRecipe = {
  enabled: true,
  base_environment_id: "prod",
  branch_pattern: "staging/*",
  repositories: [1],
  domain_template: "",
  access: "private",
  ttl_hours: 72,
  max_previews: 3,
  require_approval: true,
  resources: { api: { variables: { STRIPE_KEY: { mode: "override", has_value: true } } } },
};

describe("preview recipe drafts", () => {
  it("keeps saved overrides without resending their value", () => {
    const body = recipeForSave(recipe);
    expect(body.resources.api?.variables?.STRIPE_KEY).toEqual({ mode: "override" });
    expect(missingOverrides(recipe)).toEqual([]);
  });

  it("sends typed override values and drops the client-side marker", () => {
    const next = setVariable(recipe, "api", "STRIPE_KEY", "override", "sk_test");
    expect(recipeForSave(next).resources.api?.variables?.STRIPE_KEY).toEqual({ mode: "override", value: "sk_test" });
    expect(recipe.resources.api?.variables?.STRIPE_KEY?.value).toBeUndefined();
  });

  it("reports overrides that still need a value", () => {
    const next = setVariable(recipe, "web", "SESSION_SECRET", "override");
    expect(missingOverrides(next)).toEqual(["SESSION_SECRET"]);
    expect(missingOverrides(setVariable(next, "web", "SESSION_SECRET", "copy"))).toEqual([]);
  });

  it("toggles omitted dependencies", () => {
    const omitted = setOmitted(recipe, "web", "connection:1", true);
    expect(omitted.resources.web?.external).toEqual({ "connection:1": "omit" });
    expect(setOmitted(omitted, "web", "connection:1", false).resources.web?.external).toEqual({});
  });

  it("counts blockers only for included resources", () => {
    const blocker = { code: "x", message: "m", resolution: "r" };
    const base = { kind: "service" as const, follows_branch: false, source_kind: "git" as const, services: [], volumes: [], variables: [], dependencies: [], routes: [], notes: [] };
    expect(
      unresolved({
        resources: [
          { ...base, id: "a", name: "a", included: true, blockers: [blocker, blocker] },
          { ...base, id: "b", name: "b", included: false, blockers: [blocker] },
        ],
        repositories: [],
        blocked: true,
        example_hostnames: [],
        dns: "",
      }),
    ).toBe(2);
  });

  it("formats sizes", () => {
    expect(formatBytes(512)).toBe("512 B");
    expect(formatBytes(300_000)).toBe("293 KiB");
    expect(formatBytes(5 * 1024 ** 3)).toBe("5.0 GiB");
  });
});

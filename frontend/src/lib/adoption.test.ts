import { expect, it } from "vitest";
import { adoptionDraftErrors, plannedExpiry, selectedProjects, selectionError, selectProject } from "./adoption";

it("suggests unused keys when projects are selected and keeps edited keys", () => {
  const existing = new Set(["shop"]);
  let drafts = selectProject({}, "shop", true, existing);
  drafts = selectProject(drafts, "Shop", true, existing);
  expect(drafts.shop?.key).toBe("shop-2");
  expect(drafts.Shop?.key).toBe("shop-3");
  drafts = selectProject({ ...drafts, shop: { ...drafts.shop!, key: "custom" } }, "shop", false, existing);
  drafts = selectProject(drafts, "shop", true, existing);
  expect(drafts.shop?.key).toBe("custom");
  expect(selectedProjects(drafts)).toEqual(["shop", "Shop"]);
});

it("rejects invalid, existing and duplicate keys only for selected projects", () => {
  const errors = adoptionDraftErrors(
    {
      a: { selected: true, key: "-bad", name: "" },
      b: { selected: true, key: "web", name: "" },
      c: { selected: true, key: "dup", name: "x".repeat(121) },
      d: { selected: true, key: "dup", name: "" },
      e: { selected: false, key: "-bad", name: "" },
    },
    new Set(["web"]),
  );
  expect(errors.a?.key).toMatch(/letters, digits/);
  expect(errors.b?.key).toMatch(/already used/);
  expect(errors.c).toEqual({ key: "Each adopted project needs its own key.", name: "Use at most 120 characters." });
  expect(errors.d?.key).toMatch(/own key/);
  expect(errors.e).toBeUndefined();
});

it("limits a review to 1-16 projects and expires planned reviews after 24 hours", () => {
  expect(selectionError(0)).toMatch(/at least one/);
  expect(selectionError(16)).toBe("");
  expect(selectionError(17)).toMatch(/at most 16/);
  expect(plannedExpiry({ id: "a", project_id: "p", environment_id: "", status: "planned", created_at: "2026-11-22T10:00:00.000Z" })).toBe("2026-11-23T10:00:00.000Z");
});

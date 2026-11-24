import { describe, expect, it } from "vitest";
import { ApiError } from "../api/client";
import type { PreviewImagesStatus } from "../api/preview-images";
import { credentialDraft, credentialInput, issueBlocker, parseIssuedCredential, parsePreviewImages, sameSelection, selectableResources, wasRefused } from "./preview-images";

// Fixtures only; the token is made up.
const raw = {
  configuration: { resource_ids: ["api"], recipe_version: 3, matches_scope: true, expires_at: "2026-12-24T00:00:00Z", created_at: "2026-11-24T00:00:00Z", last_received_at: null },
  resources: [
    { id: "api", name: "api", services: ["web", "worker"], registry: { id: "r1", server: "registry.example.test" }, eligible: true, reason: "" },
    { id: "db", name: "db", services: [], registry: null, eligible: false, reason: "select a team registry connection on the resource first" },
  ],
  endpoint: "/hooks/preview-images/p",
  can_manage: true,
  setup_reason: "",
  max_resources: 16,
  max_preview_keys: 1024,
  max_deliveries: 4096,
  used_preview_keys: 2,
  retained_deliveries: 5,
};

const parsed = () => parsePreviewImages(structuredClone(raw));

describe("parsePreviewImages", () => {
  it("copies only documented fields", () => {
    const status = parsePreviewImages({ ...structuredClone(raw), token: "fixture-secret", configuration: { ...raw.configuration, token: "fixture-secret" }, resources: [{ ...raw.resources[0], extra: "x" }] });
    expect(JSON.stringify(status)).not.toContain("fixture-secret");
    expect(JSON.stringify(status)).not.toContain("extra");
    expect(status.resources[0]).toEqual(raw.resources[0]);
    expect(status.configuration).toEqual(raw.configuration);
  });

  it("never enables an action from unknown flag values", () => {
    const status = parsePreviewImages({ ...structuredClone(raw), can_manage: "yes", configuration: { ...raw.configuration, matches_scope: 1 }, resources: [{ ...raw.resources[0], eligible: "true" }] });
    expect(status.can_manage).toBe(false);
    expect(status.configuration?.matches_scope).toBe(false);
    expect(status.resources[0]?.eligible).toBe(false);
  });

  it("keeps an explicit null registry for an ineligible resource", () => {
    expect(parsed().resources[1]).toEqual(raw.resources[1]);
  });

  it("accepts the no-recipe reply", () => {
    const status = parsePreviewImages({ ...structuredClone(raw), configuration: null, resources: [], can_manage: false, setup_reason: "Save a preview recipe first." });
    expect(status.configuration).toBeNull();
    expect(status.setup_reason).toBe("Save a preview recipe first.");
  });

  it.each([
    ["a non-object", "nope"],
    ["missing resources", { ...raw, resources: undefined }],
    ["a malformed configuration", { ...raw, configuration: { ...raw.configuration, expires_at: "soon" } }],
    ["a negative count", { ...raw, used_preview_keys: -1 }],
    ["a resource without services", { ...raw, resources: [{ ...raw.resources[0], services: "web" }] }],
    ["a missing configuration", { ...raw, configuration: undefined }],
    ["an incomplete registry", { ...raw, resources: [{ ...raw.resources[0], registry: { id: 7 } }] }],
    ["a registry without a server", { ...raw, resources: [{ ...raw.resources[0], registry: { id: "r1" } }] }],
    ["a non-object registry", { ...raw, resources: [{ ...raw.resources[0], registry: "registry.example.test" }] }],
    ["a missing registry", { ...raw, resources: [{ ...raw.resources[0], registry: undefined }] }],
  ])("reports %s as an error", (_name, value) => {
    expect(() => parsePreviewImages(value)).toThrow(ApiError);
  });
});

describe("issued credentials", () => {
  it("reads the one-time token and expiry", () => {
    expect(parseIssuedCredential({ token: "t", expires_at: "2026-12-24T00:00:00Z" })).toEqual({ token: "t", expires_at: "2026-12-24T00:00:00Z" });
  });

  it("treats an unreadable reply as uncertain, not refused", () => {
    let error: unknown;
    try {
      parseIssuedCredential({ ok: true });
    } catch (cause) {
      error = cause;
    }
    expect(error).toBeInstanceOf(ApiError);
    expect(wasRefused(error)).toBe(false);
    expect(wasRefused(new ApiError(409, "conflict"))).toBe(true);
    expect(wasRefused(new ApiError(400, "invalid"))).toBe(true);
    expect(wasRefused(new ApiError(503, "unavailable"))).toBe(false);
    expect(wasRefused(new ApiError(0, "offline"))).toBe(false);
  });
});

describe("credential drafts", () => {
  it("starts from the saved selection with a 30 day lifetime and sends the exact body", () => {
    const draft = credentialDraft(parsed());
    expect(draft).toEqual({ resourceIds: ["api"], days: "30" });
    expect(credentialInput({ resourceIds: ["api", "api"], days: "7" })).toEqual({ resource_ids: ["api"], days: 7 });
  });

  it("keeps saved IDs that are no longer listed visible", () => {
    const status: PreviewImagesStatus = { ...parsed(), configuration: { ...parsed().configuration!, resource_ids: ["gone", "api"] } };
    expect(selectableResources(status, credentialDraft(status)).map((row) => [row.id, row.resource?.name ?? null])).toEqual([
      ["gone", null],
      ["api", "api"],
      ["db", "db"],
    ]);
  });

  it("explains why a draft cannot be issued", () => {
    const status = parsed();
    expect(issueBlocker(status, { resourceIds: ["api"], days: "30" })).toBeUndefined();
    expect(issueBlocker(status, { resourceIds: [], days: "30" })).toBe("Choose at least one resource.");
    expect(issueBlocker(status, { resourceIds: ["api", "db"], days: "30" })).toMatch(/^Unselect db: it cannot receive CI images now/);
    expect(issueBlocker(status, { resourceIds: ["gone"], days: "30" })).toMatch(/^Unselect gone/);
    expect(issueBlocker(status, { resourceIds: ["api"], days: "0" })).toMatch(/1 to 365/);
    expect(issueBlocker(status, { resourceIds: ["api"], days: "366" })).toMatch(/1 to 365/);
    expect(issueBlocker(status, { resourceIds: ["api"], days: "2.5" })).toMatch(/1 to 365/);
    expect(issueBlocker({ ...status, max_resources: 1 }, { resourceIds: ["api", "x"], days: "30" })).toBe("Choose at most 1 resources.");
    expect(issueBlocker({ ...status, setup_reason: "Enable the preview recipe first." }, { resourceIds: ["api"], days: "30" })).toBe("Enable the preview recipe first.");
    expect(issueBlocker({ ...status, can_manage: false }, { resourceIds: ["api"], days: "30" })).toMatch(/needs edit access/);
  });

  it("compares selections without order", () => {
    expect(sameSelection(["a", "b"], ["b", "a"])).toBe(true);
    expect(sameSelection(["a"], ["a", "b"])).toBe(false);
  });
});

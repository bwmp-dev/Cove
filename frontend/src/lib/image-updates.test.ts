import { describe, expect, it } from "vitest";
import type { Workload } from "../api/types";
import { policyDraftFrom, policyInput, ruleDraft, updateState, validatePolicyDraft, windowSummary, type PolicyDraft } from "./image-updates";
import { formValues, validateWorkload } from "./workload-form";

const draft = (extra: Partial<PolicyDraft> = {}): PolicyDraft => ({ enabled: false, mode: "notify", rules: [ruleDraft({ service: "app", range: "" })], windowStart: "", windowEnd: "", ...extra });

describe("image update policy drafts", () => {
  it("starts an unsaved policy off, notify-only and without a window", () => {
    const fresh = policyDraftFrom(null);
    expect(policyInput(fresh)).toEqual({ enabled: false, mode: "notify", rules: [{ service: "", range: "" }], window_start: "", window_end: "" });
  });

  it("copies a saved policy, including services no longer in Compose", () => {
    const saved = { enabled: true, mode: "auto" as const, rules: [{ service: "gone", range: "~2.1" }], window_start: "23:00", window_end: "02:00" };
    expect(policyInput(policyDraftFrom(saved))).toEqual(saved);
  });

  it("checks only the shape of services, ranges and windows", () => {
    expect(validatePolicyDraft(draft())).toEqual({});
    expect(validatePolicyDraft(draft({ rules: [ruleDraft({ service: "app", range: ">= not semver ||" })] }))).toEqual({});
    expect(validatePolicyDraft(draft({ rules: [] })).rules).toBe("Add at least one service.");
    const long = ruleDraft({ service: "app", range: "x".repeat(201) });
    expect(validatePolicyDraft(draft({ rules: [long] }))[`${long.key}.range`]).toBe("Use at most 200 characters.");
    expect(validatePolicyDraft(draft({ windowStart: "24:00", windowEnd: "01:00" })).window).toBe("Use 24-hour HH:MM times.");
    expect(validatePolicyDraft(draft({ windowStart: "00:00", windowEnd: "00:00" })).window).toMatch(/two different times/);
    expect(validatePolicyDraft(draft({ windowStart: "", windowEnd: "00:00" })).window).toMatch(/Set both times/);
    expect(validatePolicyDraft(draft({ windowStart: "23:00", windowEnd: "00:00" }))).toEqual({});
    expect(validatePolicyDraft(draft({ windowStart: "00:00", windowEnd: "06:00" }))).toEqual({});
  });

  it("describes daily UTC windows around midnight", () => {
    expect(windowSummary("", "")).toBe("Any time of day");
    expect(windowSummary("23:00", "02:00")).toBe("23:00–02:00 UTC, continuing past midnight");
    expect(windowSummary("22:00", "00:00")).toBe("22:00–00:00 UTC");
    expect(windowSummary("00:00", "06:00")).toBe("00:00–06:00 UTC");
  });

  it("labels unrecognised result states as unknown", () => {
    expect(updateState("available").label).toBe("Update available");
    expect(updateState("current").label).toBe("Current");
    expect(updateState("something-new")).toEqual({ tone: "neutral", label: "Unknown" });
    expect(updateState(undefined).label).toBe("Unknown");
  });
});

it("never carries the image update policy in ordinary workload saves", () => {
  const workload = {
    id: "w1",
    name: "web",
    node_id: "n1",
    kind: "service",
    compose: "services:\n  app:\n    image: registry.example.test/team/app:1.2.0\n",
    source: null,
    image_update_policy: { enabled: true, mode: "auto", rules: [], window_start: "", window_end: "" },
  } as unknown as Workload;
  const input = validateWorkload(formValues(workload), "edit").input;
  expect(input).toBeDefined();
  expect(Object.keys(input ?? {}).some((key) => key.includes("image_update"))).toBe(false);
});

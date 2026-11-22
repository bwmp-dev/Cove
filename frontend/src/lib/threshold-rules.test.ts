import { describe, expect, it } from "vitest";
import { formatDuration, thresholdDeliveryProblem, thresholdRuleFromForm } from "./threshold-rules";

function form(entries: [string, string][]) {
  const data = new FormData();
  for (const [key, value] of entries) data.append(key, value);
  return data;
}

const valid: [string, string][] = [["name", " Hot CPU "], ["node_id", "node-1"], ["metric", "cpu"], ["threshold", "92.5"], ["confirmation_seconds", "300"], ["repeat_seconds", "3600"], ["channel_ids", "a"], ["channel_ids", "b"], ["channel_ids", "a"]];

describe("thresholdRuleFromForm", () => {
  it("builds the API body with numbers and distinct channels", () => {
    expect(thresholdRuleFromForm(form(valid))).toEqual({ name: "Hot CPU", node_id: "node-1", metric: "cpu", threshold: 92.5, confirmation_seconds: 300, repeat_seconds: 3600, channel_ids: ["a", "b"] });
  });

  it("rejects values outside the API limits", () => {
    const replace = (key: string, value: string) => form(valid.filter(([name]) => name !== key).concat([[key, value]]));
    expect(thresholdRuleFromForm(replace("threshold", "0"))).toMatchObject({ field: "threshold" });
    expect(thresholdRuleFromForm(replace("threshold", "100.1"))).toMatchObject({ field: "threshold" });
    expect(thresholdRuleFromForm(replace("threshold", ""))).toMatchObject({ field: "threshold" });
    expect(thresholdRuleFromForm(replace("threshold", "100"))).toMatchObject({ threshold: 100 });
    expect(thresholdRuleFromForm(replace("metric", "restarts"))).toMatchObject({ field: "metric" });
    expect(thresholdRuleFromForm(replace("confirmation_seconds", "30"))).toMatchObject({ field: "confirmation_seconds" });
    expect(thresholdRuleFromForm(replace("repeat_seconds", "604801"))).toMatchObject({ field: "repeat_seconds" });
    expect(thresholdRuleFromForm(replace("name", "x".repeat(121)))).toMatchObject({ field: "name" });
    expect(thresholdRuleFromForm(form(valid.filter(([name]) => name !== "channel_ids")))).toEqual({ field: "channel_ids", message: "Select at least one channel." });
    expect(thresholdRuleFromForm(form([...valid.filter(([name]) => name !== "channel_ids"), ...Array.from({ length: 33 }, (_, index): [string, string] => ["channel_ids", `c${index}`])]))).toMatchObject({ field: "channel_ids" });
  });
});

describe("thresholdDeliveryProblem", () => {
  const base = { provider: "slack", enabled: true, success: false, failure: false };
  it("accepts legacy health channels and channels selecting both threshold events", () => {
    expect(thresholdDeliveryProblem({ ...base, health: true })).toBeUndefined();
    expect(thresholdDeliveryProblem({ ...base, events: ["threshold.alert", "threshold.recovered"] })).toBeUndefined();
  });

  it("explains why a channel would not deliver", () => {
    expect(thresholdDeliveryProblem({ ...base, health: false })).toBe("This channel does not include health alerts.");
    expect(thresholdDeliveryProblem({ ...base, health: true, enabled: false })).toBe("Notifications are disabled on this channel.");
    expect(thresholdDeliveryProblem({ ...base, events: ["threshold.alert"] })).toContain("not recovery notices");
    expect(thresholdDeliveryProblem({ ...base, events: ["backup.failed"] })).toBe("This channel does not select threshold events.");
    expect(thresholdDeliveryProblem({ ...base, provider: "email", owner: true, health: true })).toContain("skip node events");
  });
});

it("formats durations in the largest whole unit", () => {
  expect([60, 300, 3600, 7200, 86400, 604800, 90].map(formatDuration)).toEqual(["1 minute", "5 minutes", "1 hour", "2 hours", "1 day", "7 days", "90 seconds"]);
});

import { describe, expect, it } from "vitest";
import { formatDuration, thresholdCondition, thresholdDeliveryProblem, thresholdRuleFromForm } from "./threshold-rules";

function form(entries: [string, string][]) {
  const data = new FormData();
  for (const [key, value] of entries) data.append(key, value);
  return data;
}

const valid: [string, string][] = [["name", " Hot CPU "], ["node_id", "node-1"], ["metric", "cpu"], ["threshold", "92.5"], ["confirmation_seconds", "300"], ["repeat_seconds", "3600"], ["channel_ids", "a"], ["channel_ids", "b"], ["channel_ids", "a"]];
const replaceIn = (entries: [string, string][], key: string, value: string) => form(entries.filter(([name]) => name !== key).concat([[key, value]]));

describe("thresholdRuleFromForm", () => {
  it("builds the API body with numbers and distinct channels", () => {
    expect(thresholdRuleFromForm(form(valid))).toEqual({ name: "Hot CPU", node_id: "node-1", workload_id: "", domain: "", mount_path: "", metric: "cpu", threshold: 92.5, confirmation_seconds: 300, repeat_seconds: 3600, channel_ids: ["a", "b"] });
  });

  it("rejects values outside the API limits", () => {
    const replace = (key: string, value: string) => replaceIn(valid, key, value);
    expect(thresholdRuleFromForm(replace("threshold", "0"))).toMatchObject({ field: "threshold" });
    expect(thresholdRuleFromForm(replace("threshold", "100.1"))).toMatchObject({ field: "threshold" });
    expect(thresholdRuleFromForm(replace("threshold", ""))).toMatchObject({ field: "threshold" });
    expect(thresholdRuleFromForm(replace("threshold", "100"))).toMatchObject({ threshold: 100 });
    expect(thresholdRuleFromForm(replace("metric", "load"))).toMatchObject({ field: "metric" });
    expect(thresholdRuleFromForm(replace("node_id", ""))).toMatchObject({ field: "node_id" });
    expect(thresholdRuleFromForm(replace("confirmation_seconds", "30"))).toMatchObject({ field: "confirmation_seconds" });
    expect(thresholdRuleFromForm(replace("repeat_seconds", "604801"))).toMatchObject({ field: "repeat_seconds" });
    expect(thresholdRuleFromForm(replace("name", "x".repeat(121)))).toMatchObject({ field: "name" });
    expect(thresholdRuleFromForm(form(valid.filter(([name]) => name !== "channel_ids")))).toEqual({ field: "channel_ids", message: "Select at least one channel." });
    expect(thresholdRuleFromForm(form([...valid.filter(([name]) => name !== "channel_ids"), ...Array.from({ length: 33 }, (_, index): [string, string] => ["channel_ids", `c${index}`])]))).toMatchObject({ field: "channel_ids" });
  });

  const certificate: [string, string][] = [["name", "Cert"], ["metric", "certificate"], ["workload_id", "w1"], ["domain", "app.example.test"], ["threshold", "14"], ["confirmation_seconds", "300"], ["repeat_seconds", "86400"], ["channel_ids", "a"]];

  it("targets a service and configured domain for certificate rules, leaving the node to the API", () => {
    expect(thresholdRuleFromForm(form([...certificate, ["node_id", "ignored"]]))).toEqual({ name: "Cert", node_id: "", workload_id: "w1", domain: "app.example.test", mount_path: "", metric: "certificate", threshold: 14, confirmation_seconds: 300, repeat_seconds: 86400, channel_ids: ["a"] });
    expect(thresholdRuleFromForm(replaceIn(certificate, "workload_id", ""))).toEqual({ field: "workload_id", message: "Select a service." });
    expect(thresholdRuleFromForm(replaceIn(certificate, "domain", ""))).toEqual({ field: "domain", message: "Select a domain." });
    expect(thresholdRuleFromForm(replaceIn(certificate, "threshold", "3650"))).toMatchObject({ threshold: 3650 });
    expect(thresholdRuleFromForm(replaceIn(certificate, "threshold", "3650.5"))).toMatchObject({ field: "threshold", message: "Enter a number of days above 0 and at most 3650." });
    expect(thresholdRuleFromForm(replaceIn(certificate, "threshold", "0"))).toMatchObject({ field: "threshold" });
  });

  it("accepts whole event counts from 1 to 1000 for container rules without a domain", () => {
    const restarts = certificate.filter(([key]) => key !== "metric").concat([["metric", "restarts"]]);
    expect(thresholdRuleFromForm(replaceIn(restarts, "threshold", "3"))).toMatchObject({ metric: "restarts", workload_id: "w1", domain: "", threshold: 3 });
    expect(thresholdRuleFromForm(replaceIn(restarts, "threshold", "1000"))).toMatchObject({ threshold: 1000 });
    for (const value of ["0", "1.5", "1001"]) expect(thresholdRuleFromForm(replaceIn(restarts, "threshold", value))).toMatchObject({ field: "threshold", message: "Enter a whole number from 1 to 1000." });
  });

  it("requires a domain and whole consecutive failures for uptime rules", () => {
    const uptime = certificate.filter(([key]) => key !== "metric").concat([["metric", "uptime"]]);
    expect(thresholdRuleFromForm(replaceIn(uptime, "threshold", "3"))).toMatchObject({ metric: "uptime", workload_id: "w1", domain: "app.example.test", node_id: "", threshold: 3 });
    expect(thresholdRuleFromForm(replaceIn(uptime, "domain", ""))).toEqual({ field: "domain", message: "Select a domain." });
    for (const value of ["0", "2.5", "1001"]) expect(thresholdRuleFromForm(replaceIn(uptime, "threshold", value))).toMatchObject({ field: "threshold", message: "Enter a whole number from 1 to 1000." });
  });
});

it("describes each rule in its own unit", () => {
  expect(thresholdCondition({ metric: "memory", threshold: 92.5, domain: "", confirmation_seconds: 300 })).toBe("Memory used at or above 92.5% for 5 minutes");
  expect(thresholdCondition({ metric: "certificate", threshold: 1, domain: "app.example.test", confirmation_seconds: 600 })).toBe("Certificate expiry for app.example.test at or below 1 day for 10 minutes");
  expect(thresholdCondition({ metric: "oom", threshold: 2, domain: "", confirmation_seconds: 60 })).toBe("Out-of-memory kills at or above 2 in ten minutes for 1 minute");
  expect(thresholdCondition({ metric: "uptime", threshold: 3, domain: "app.example.test", confirmation_seconds: 120 })).toBe("app.example.test failing 3 uptime checks in a row for 2 minutes");
});

describe("thresholdDeliveryProblem", () => {
  const base = { provider: "slack", enabled: true, success: false, failure: false };
  it("accepts legacy health channels and channels selecting both threshold events", () => {
    expect(thresholdDeliveryProblem({ ...base, health: true }, "node")).toBeUndefined();
    expect(thresholdDeliveryProblem({ ...base, events: ["threshold.alert", "threshold.recovered"] }, "node")).toBeUndefined();
  });

  it("explains why a channel would not deliver", () => {
    expect(thresholdDeliveryProblem({ ...base, health: false }, "node")).toBe("This channel does not include health alerts.");
    expect(thresholdDeliveryProblem({ ...base, health: true, enabled: false }, "node")).toBe("Notifications are disabled on this channel.");
    expect(thresholdDeliveryProblem({ ...base, events: ["threshold.alert"] }, "node")).toContain("not recovery notices");
    expect(thresholdDeliveryProblem({ ...base, events: ["backup.failed"] }, "resource")).toBe("This channel does not select threshold events.");
  });

  it("flags resource owner emails only for node rules", () => {
    const owner = { ...base, provider: "email", owner: true, health: true };
    expect(thresholdDeliveryProblem(owner, "node")).toContain("skip node events");
    expect(thresholdDeliveryProblem(owner, "resource")).toBeUndefined();
    expect(thresholdDeliveryProblem({ ...owner, enabled: false }, "resource")).toBe("Notifications are disabled on this channel.");
  });
});

it("formats durations in the largest whole unit", () => {
  expect([60, 300, 3600, 7200, 86400, 604800, 90].map(formatDuration)).toEqual(["1 minute", "5 minutes", "1 hour", "2 hours", "1 day", "7 days", "90 seconds"]);
});

it("maps disk filesystem scopes to the mount path contract", () => {
  const disk = valid.filter(([name]) => name !== "metric").concat([["metric", "disk"]]);
  expect(thresholdRuleFromForm(form(disk))).toMatchObject({ metric: "disk", mount_path: "" });
  expect(thresholdRuleFromForm(form([...disk, ["mount_scope", "any"]]))).toMatchObject({ mount_path: "*" });
  expect(thresholdRuleFromForm(form([...disk, ["mount_scope", "path"], ["mount_path", "/srv/data"]]))).toMatchObject({ mount_path: "/srv/data" });
  expect(thresholdRuleFromForm(form([...disk, ["mount_scope", "path"], ["mount_path", ""]]))).toEqual({ field: "mount_path", message: "Select a mount." });
  const cpu = form([...valid, ["mount_scope", "any"]]);
  expect(thresholdRuleFromForm(cpu)).toMatchObject({ metric: "cpu", mount_path: "" });
  expect(thresholdCondition({ metric: "disk", threshold: 85, domain: "", mount_path: "*", confirmation_seconds: 300 })).toBe("Fullest mount used at or above 85% for 5 minutes");
  expect(thresholdCondition({ metric: "disk", threshold: 85, domain: "", mount_path: "/srv", confirmation_seconds: 300 })).toBe("Disk used on /srv at or above 85% for 5 minutes");
  expect(thresholdCondition({ metric: "disk", threshold: 85, domain: "", mount_path: "", confirmation_seconds: 300 })).toBe("Data disk used at or above 85% for 5 minutes");
});

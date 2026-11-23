import { describe, expect, it } from "vitest";
import { groupNotificationEvents, incidentConditions, notificationFromForm, notificationTestDescription, routingSummary, selectedIncidentConditions } from "./notifications";

function form(entries: [string, string][]) {
  const data = new FormData();
  for (const [key, value] of entries) data.append(key, value);
  return data;
}

const groups: [string, string][] = [["routing", "groups"], ["failure", "on"]];

it("groups the catalogue by subject and keeps health events last", () => {
  const groups = groupNotificationEvents(["disk_low.alert", "backup.failed", "backup.succeeded", "operation.failed", "future.thing"]);
  expect(groups.map((group) => group.title)).toEqual(["Backups", "Other operations", "future", "Health alerts"]);
  expect(groups[0]?.events).toEqual([{ value: "backup.failed", label: "Failed" }, { value: "backup.succeeded", label: "Succeeded" }]);
  expect(groups[3]?.events).toEqual([{ value: "disk_low.alert", label: "Low disk space: alert" }]);
});

it("labels the server's outcome events and keeps them out of incident conditions", () => {
  const outcomes = ["security.lockout", "security.new_device", "security.second_factor_reset", "audit_export.failed", "deployment.freeze_override", "deploy_hook.accepted", "deploy_hook.failed", "gitops.applied", "gitops.drift", "configuration.applied", "import.reviewed", "import.failed", "adoption.succeeded", "adoption.failed"];
  const groups = groupNotificationEvents(["backup.failed", ...outcomes, "threshold.alert", "threshold.recovered"]);
  expect(groups.map((group) => group.title)).toEqual(["Backups", "Account security", "Deployments and configuration", "Imports and adoption", "Health alerts"]);
  const labelled = groups.flatMap((group) => group.events).filter((event) => outcomes.includes(event.value));
  expect(labelled.map((event) => event.value)).toEqual(outcomes);
  for (const event of labelled) expect(event.label).not.toMatch(/[._]/);
  expect(groups[1]?.events[0]).toEqual({ value: "security.lockout", label: "Sign-in lockout" });
  expect(incidentConditions([...outcomes, "threshold.alert", "threshold.recovered"])).toEqual([{ value: "threshold", label: "Threshold condition" }]);
});

it("labels rollout, CPU scaling and idle sleep events and never offers them to incident providers", () => {
  const outcomes = ["rollout.staged", "rollout.promoted", "rollout.aborted", "rollout.failed", "autoscale.queued", "autoscale.succeeded", "autoscale.failed", "resource.asleep", "resource.awake"];
  const groups = groupNotificationEvents([...outcomes, "resource_unhealthy.alert", "resource_unhealthy.recovered"]);
  expect(groups.map((group) => group.title)).toEqual(["Rollouts and CPU scaling", "Idle sleep", "Health alerts"]);
  const labelled = groups.flatMap((group) => group.events).filter((event) => outcomes.includes(event.value));
  expect(labelled.map((event) => event.value)).toEqual(outcomes);
  for (const event of labelled) expect(event.label).not.toMatch(/[._]/);
  expect(groups.every((group) => group.health === (group.title === "Health alerts"))).toBe(true);
  expect(incidentConditions([...outcomes, "resource_unhealthy.alert", "resource_unhealthy.recovered"])).toEqual([{ value: "resource_unhealthy", label: "Unhealthy resource" }]);
});

it("labels directory provisioning events and never offers them to incident providers", () => {
  const outcomes = ["identity.provisioned", "identity.deprovisioned", "identity.membership_changed", "identity.sync_failed"];
  const groups = groupNotificationEvents([...outcomes, "node_unreachable.alert", "node_unreachable.recovered"]);
  expect(groups.map((group) => group.title)).toEqual(["Directory provisioning", "Health alerts"]);
  expect(groups[0]?.events).toEqual([
    { value: "identity.provisioned", label: "Account provisioned" },
    { value: "identity.deprovisioned", label: "Account deprovisioned" },
    { value: "identity.membership_changed", label: "Team access changed by a directory" },
    { value: "identity.sync_failed", label: "Directory sync failed" },
  ]);
  expect(groups[0]?.health).toBe(false);
  expect(incidentConditions([...outcomes, "node_unreachable.alert", "node_unreachable.recovered"])).toEqual([{ value: "node_unreachable", label: "Node unreachable" }]);
});

it("sends exact events without the legacy switches", () => {
  expect(notificationFromForm(form([["provider", "slack"], ["url", " https://hooks.example.com/x "], ["enabled", "on"], ["routing", "events"], ["success", "on"], ["events", "backup.failed"], ["events", "drill.failed"]]))).toEqual({
    provider: "slack", url: "https://hooks.example.com/x", enabled: true, success: false, failure: false, health: false, events: ["backup.failed", "drill.failed"],
  });
});

it("keeps legacy switches, sends email recipients and webhook signing secrets", () => {
  expect(notificationFromForm(form([["provider", "email"], ["email", " ops@example.com "], ["routing", "groups"], ["failure", "on"]]))).toEqual({
    provider: "email", url: "", email: "ops@example.com", enabled: false, success: false, failure: true, health: false, events: [],
  });
  const secret = "s".repeat(32);
  const saved = { provider: "webhook", enabled: true, success: false, failure: true };
  expect(notificationFromForm(form([["provider", "webhook"], ["url", ""], ["routing", "groups"], ["health", "on"], ["signing_secret", secret]]), saved)).toMatchObject({ provider: "webhook", url: "", health: true, signing_secret: secret });
  expect(notificationFromForm(form([["provider", "discord"], ["url", "https://discord.example.com/x"], ["routing", "groups"], ["health", "on"], ["signing_secret", secret]]))).not.toHaveProperty("signing_secret");
});

it("requires at least one event", () => {
  expect(notificationFromForm(form([["provider", "webhook"], ["routing", "events"], ["failure", "on"]]))).toEqual({ field: "events", message: "Select at least one event." });
});

describe("provider destinations", () => {
  it("sends Telegram tokens and chat IDs without a URL", () => {
    expect(notificationFromForm(form([["provider", "telegram"], ["url", "https://ignored.example.com"], ["token", " 123:abc "], ["target", " -1001 "], ...groups]))).toEqual({
      provider: "telegram", url: "", token: "123:abc", target: "-1001", enabled: false, success: false, failure: true, health: false, events: [],
    });
    expect(notificationFromForm(form([["provider", "telegram"], ["token", "123/abc"], ["target", "1"], ...groups]))).toMatchObject({ field: "token" });
    expect(notificationFromForm(form([["provider", "telegram"], ["token", "123:abc"], ...groups]))).toEqual({ field: "target", message: "Chat ID is required." });
  });

  it("makes the ntfy token optional and rejects base URLs with a query", () => {
    expect(notificationFromForm(form([["provider", "ntfy"], ["url", "https://ntfy.sh"], ["target", "alerts"], ...groups]))).toEqual({
      provider: "ntfy", url: "https://ntfy.sh", target: "alerts", enabled: false, success: false, failure: true, health: false, events: [],
    });
    expect(notificationFromForm(form([["provider", "ntfy"], ["url", "https://ntfy.sh/?x=1"], ["target", "alerts"], ...groups]))).toEqual({ field: "url", message: "Enter the base URL without a query string." });
    expect(notificationFromForm(form([["provider", "gotify"], ["url", "http://gotify.example.com"], ["token", "t"], ...groups]))).toEqual({ field: "url", message: "The URL must use HTTPS." });
  });

  it("requires Pushover token and user key unless they are saved for the same provider", () => {
    const entries: [string, string][] = [["provider", "pushover"], ["token", ""], ["user_key", ""], ...groups];
    expect(notificationFromForm(form(entries))).toEqual({ field: "token", message: "Application token is required." });
    const saved = { provider: "pushover", enabled: true, success: false, failure: true, token_set: true, user_key_set: true };
    expect(notificationFromForm(form(entries), saved)).toEqual({ provider: "pushover", url: "", enabled: false, success: false, failure: true, health: false, events: [] });
    expect(notificationFromForm(form(entries), { ...saved, provider: "gotify" })).toMatchObject({ field: "token" });
    expect(notificationFromForm(form([["provider", "pushover"], ["token", "app"], ["user_key", "user"], ...groups]))).toMatchObject({ token: "app", user_key: "user" });
  });

  it("keeps a hidden URL only for the same provider", () => {
    const entries: [string, string][] = [["provider", "matrix"], ["url", ""], ["target", "!room:example.org"], ...groups];
    const saved = { provider: "matrix", enabled: true, success: false, failure: true, token_set: true };
    expect(notificationFromForm(form(entries), saved)).toMatchObject({ provider: "matrix", url: "", target: "!room:example.org" });
    expect(notificationFromForm(form(entries), { ...saved, provider: "teams" })).toEqual({ field: "url", message: "Homeserver URL is required." });
  });
});

describe("incident providers", () => {
  const catalogue = ["backup.failed", "node_unreachable.alert", "node_unreachable.recovered", "disk_low.alert", "disk_low.recovered", "future.alert"];

  it("offers only conditions with both alert and recovery events", () => {
    expect(incidentConditions(catalogue)).toEqual([{ value: "node_unreachable", label: "Node unreachable" }, { value: "disk_low", label: "Low disk space" }]);
    expect(selectedIncidentConditions(["disk_low.alert", "disk_low.recovered"])).toEqual(["disk_low"]);
  });

  it("sends exact alert and recovery pairs and ignores result groups", () => {
    expect(notificationFromForm(form([["provider", "pagerduty"], ["token", "routing"], ["routing", "groups"], ["success", "on"], ["failure", "on"], ["events", "backup.failed"], ["incident_events", "node_unreachable"], ["incident_events", "disk_low"]]))).toEqual({
      provider: "pagerduty", url: "", token: "routing", enabled: false, success: false, failure: false, health: false, events: ["node_unreachable.alert", "node_unreachable.recovered", "disk_low.alert", "disk_low.recovered"],
    });
    expect(notificationFromForm(form([["provider", "opsgenie"], ["url", "https://api.eu.opsgenie.com"], ["token", "key"], ["failure", "on"]]))).toEqual({ field: "events", message: "Select at least one health condition." });
  });

  it("summarises conditions and warns that a test opens an incident", () => {
    const pagerduty = { provider: "pagerduty", enabled: true, success: false, failure: false, events: ["disk_low.alert", "disk_low.recovered"] };
    expect(routingSummary(pagerduty)).toBe("1 health condition");
    expect(notificationTestDescription(pagerduty)).toContain("opens a new incident");
    expect(notificationTestDescription(pagerduty)).toContain("resolve or close it in PagerDuty");
  });
});

it("summarises routing", () => {
  expect(routingSummary({ provider: "webhook", enabled: true, success: true, failure: true, health: false })).toBe("Successful operations, failed operations");
  expect(routingSummary({ provider: "webhook", enabled: true, success: false, failure: false, events: ["backup.failed"] })).toBe("1 selected event");
});

describe("resource owner email", () => {
  it("sends owner mode without a fixed recipient and keeps fixed mode unchanged", () => {
    expect(notificationFromForm(form([["provider", "email"], ["recipient", "owner"], ["email", "ignored@example.com"], ...groups]))).toEqual({
      provider: "email", url: "", email: "", owner: true, enabled: false, success: false, failure: true, health: false, events: [],
    });
    expect(notificationFromForm(form([["provider", "email"], ["recipient", "fixed"], ["email", "ops@example.com"], ...groups]))).not.toHaveProperty("owner");
  });

  it("explains that an owner channel test goes to the tester", () => {
    const description = notificationTestDescription({ provider: "email", owner: true, email: "", enabled: true, success: false, failure: true });
    expect(description).toContain("your own account, not to a resource owner");
    expect(description).toContain("no verified email address");
  });
});

it("labels paired SSH host key events for health routing and incident providers", () => {
  const catalogue = ["host_key_changed.alert", "host_key_changed.recovered"];
  expect(groupNotificationEvents(catalogue)[0]?.events).toEqual([
    { value: "host_key_changed.alert", label: "SSH host key changed: alert" },
    { value: "host_key_changed.recovered", label: "SSH host key changed: recovered" },
  ]);
  expect(incidentConditions(catalogue)).toEqual([{ value: "host_key_changed", label: "SSH host key changed" }]);
});

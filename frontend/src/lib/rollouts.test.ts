import { expect, it } from "vitest";
import type { RolloutOptions, Workload } from "../api/types";
import { resultSummary, rolloutChanged, rolloutDraft, rolloutPhase, rolloutsApply, trafficSummary, validateRollout } from "./rollouts";
import { formValues, validateWorkload } from "./workload-form";

const saved: RolloutOptions = {
  strategy: "canary",
  replicas: { api: 3, web: 2 },
  http_checks: { web: { port: 8080, path: "/healthz" } },
  parallel_volumes: false,
  drain_seconds: 10,
  timeout_seconds: 90,
  canary_percent: 25,
};

it("round-trips saved options through the draft", () => {
  const draft = rolloutDraft(saved);
  expect(rolloutChanged(draft, saved)).toBe(false);
  expect(validateRollout(draft, saved)).toEqual({ options: saved, errors: {} });
});

it("applies the control plane's defaults to missing and empty values", () => {
  expect(rolloutDraft(null)).toMatchObject({ strategy: "disabled", drain: "0", timeout: "120", canary: "10", parallelVolumes: false });
  expect(rolloutDraft({ strategy: "" as RolloutOptions["strategy"], parallel_volumes: false, drain_seconds: 0, timeout_seconds: 0, canary_percent: 0 })).toMatchObject({ strategy: "rolling", timeout: "120", canary: "10" });
  expect(rolloutChanged(rolloutDraft(null), undefined)).toBe(false);
  expect(rolloutChanged(rolloutDraft(null), { strategy: "disabled", parallel_volumes: false, drain_seconds: 0, timeout_seconds: 120, canary_percent: 10 })).toBe(false);
});

it("ignores row order, which the API returns sorted by service", () => {
  const draft = rolloutDraft(saved);
  expect(rolloutChanged({ ...draft, replicas: [...draft.replicas].reverse() }, saved)).toBe(false);
  expect(rolloutChanged({ ...draft, replicas: draft.replicas.slice(1) }, saved)).toBe(true);
});

it("keeps the saved tuning when rollouts are turned off", () => {
  const draft = { ...rolloutDraft(saved), strategy: "disabled" as const, drain: "not a number" };
  expect(rolloutChanged(draft, saved)).toBe(true);
  expect(validateRollout(draft, saved)).toEqual({ options: { ...saved, strategy: "disabled" }, errors: {} });
  expect(validateRollout(draft, null).options).toEqual({ strategy: "disabled", parallel_volumes: false, drain_seconds: 0, timeout_seconds: 120, canary_percent: 10 });
});

it("reports invalid fields beside each field", () => {
  const draft = rolloutDraft({ ...saved, strategy: "rolling" });
  const [first, second] = draft.replicas;
  const [check] = draft.checks;
  if (!first || !second || !check) throw new Error("expected rows");
  const result = validateRollout(
    {
      ...draft,
      drain: "121",
      timeout: "4",
      replicas: [{ ...first, service: "web" }, { ...second, service: "web", count: "17" }, { id: "blank", service: "", count: "1" }],
      checks: [{ ...check, port: "0", path: "//evil" }, { id: "c2", service: "-bad", port: "80", path: "/ok\n" }],
    },
    saved,
  );
  expect(result.options).toBeUndefined();
  expect(result.errors).toEqual({
    drain: "Use a whole number of seconds from 0 to 120.",
    timeout: "Use a whole number of seconds from 5 to 600.",
    [`${second.id}.service`]: "Each service can be listed once.",
    [`${second.id}.count`]: "Use a whole number from 1 to 16.",
    "blank.service": "Enter the Compose service name.",
    [`${check.id}.port`]: "Use a container port from 1 to 65535.",
    [`${check.id}.path`]: "Start with a single / and use at most 1024 characters without control characters.",
    "c2.service": "Use a Compose service name: letters, digits, dots, dashes and underscores, up to 64 characters.",
    "c2.path": "Start with a single / and use at most 1024 characters without control characters.",
  });
});

it("limits the replica total and validates the canary share only for canaries", () => {
  const rows = Array.from({ length: 5 }, (_, index) => ({ id: `r${index}`, service: `s${index}`, count: "16" }));
  expect(validateRollout({ ...rolloutDraft(saved), replicas: rows }, saved).errors.replicas).toBe("Use at most 64 replicas in total.");
  expect(validateRollout({ ...rolloutDraft(saved), canary: "100" }, saved).errors.canary).toBe("Use a whole percentage from 1 to 99.");
  expect(validateRollout({ ...rolloutDraft(saved), strategy: "blue_green", canary: "100" }, saved).options?.canary_percent).toBe(25);
});

it("leaves rollout out of unrelated section saves so the server keeps it", () => {
  const workload = { id: "w1", name: "web", node_id: "n1", kind: "service", compose: "services: {}", source: null, rollout: saved } as unknown as Workload;
  const { input } = validateWorkload(formValues(workload), "edit");
  expect(input).toBeDefined();
  expect(input && "rollout" in input).toBe(false);
});

it("applies only to services outside managed previews", () => {
  expect(rolloutsApply({ kind: "service" })).toBe(true);
  expect(rolloutsApply({ kind: "job" })).toBe(false);
  expect(rolloutsApply({ kind: "service", preview_source_id: "w0" })).toBe(false);
  expect(rolloutsApply({ kind: "service", environment_preview_id: "p1" })).toBe(false);
});

it("never presents an unknown phase as complete and describes staged traffic", () => {
  expect(rolloutPhase("verifying")).toEqual({ tone: "warn", label: "Unknown phase: verifying" });
  expect(trafficSummary({ operation: "o1", strategy: "blue_green", phase: "waiting", candidate_percent: 0 })).toBe("The candidate passed its health checks and receives no traffic yet.");
  expect(trafficSummary({ operation: "o1", strategy: "canary", phase: "waiting", candidate_percent: 25 })).toBe("The candidate receives 25% of traffic; the current release serves the rest.");
  expect(resultSummary({ operation: "o1", strategy: "canary", phase: "waiting", candidate_percent: 25 })).toBe("Canary rollout staged. Traffic moves fully only after it is promoted.");
  expect(resultSummary({ operation: "o1", strategy: "guarded", phase: "completed", candidate_percent: 100, reason: "no route" })).toBe("Fell back to guarded Compose replacement.");
});

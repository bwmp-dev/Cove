import { expect, it } from "vitest";
import type { RestartPolicy } from "../api/types";
import { actionLabels } from "./operations";
import { restartStatus, type RestartContext } from "./restart-policy";

const now = Date.parse("2026-11-22T12:00:00Z");
const policy: RestartPolicy = { enabled: true, last_queued_at: null, attempts_in_window: 0, window_started_at: null, operation_id: null, last_error: "" };
const context: RestartContext = { now, manuallyStopped: false, node: { name: "worker-1", state: "ready", supported: true }, heldRollout: false, activeAction: null, readFailed: false };

it("labels the automatic restart operation without calling it a deploy or build", () => {
  expect(actionLabels.restart_unhealthy).toBe("Restart unhealthy containers");
});

it("reports off and healthy checking states", () => {
  expect(restartStatus({ ...policy, enabled: false }, context)).toEqual({ tone: "neutral", label: "Off" });
  expect(restartStatus(policy, context)).toMatchObject({ tone: "ok", label: "Checking" });
});

it("explains why the monitor skips the resource", () => {
  expect(restartStatus(policy, { ...context, manuallyStopped: true })).toMatchObject({ label: "Paused", detail: "The resource was stopped manually. Checks resume after it is started." });
  expect(restartStatus(policy, { ...context, node: { name: "worker-1", state: "ready", supported: false } })).toMatchObject({ tone: "warn", label: "Not checking" });
  expect(restartStatus(policy, { ...context, node: { name: "worker-1", state: "unreachable", supported: true } })).toMatchObject({ label: "Paused", detail: "worker-1 is unreachable. Checks resume when it is ready." });
  expect(restartStatus(policy, { ...context, heldRollout: true })).toMatchObject({ label: "Paused", detail: "A staged rollout is waiting for promote or abort. Checks resume after it finishes." });
  expect(restartStatus(policy, { ...context, activeAction: "deploy" })).toMatchObject({ tone: "info", label: "Waiting", detail: "Checks resume after the active deploy finishes." });
});

it("applies the five-minute cooldown from the queued time", () => {
  const recent = { ...policy, attempts_in_window: 1, window_started_at: "2026-11-22T11:58:00Z", last_queued_at: "2026-11-22T11:58:00Z" };
  expect(restartStatus(recent, context)).toMatchObject({ label: "Cooling down", until: "2026-11-22T12:03:00.000Z" });
  expect(restartStatus({ ...recent, last_queued_at: "2026-11-22T11:50:00Z" }, context)).toMatchObject({ label: "Checking" });
});

it("reports the hourly limit until the oldest counted attempt leaves the window", () => {
  const limited = { ...policy, attempts_in_window: 3, window_started_at: "2026-11-22T11:20:00Z", last_queued_at: "2026-11-22T11:40:00Z" };
  expect(restartStatus(limited, context)).toMatchObject({ tone: "warn", label: "Limit reached", until: "2026-11-22T12:20:00.000Z" });
  expect(restartStatus({ ...limited, window_started_at: "2026-11-22T10:59:00Z" }, context)).toMatchObject({ label: "Checking" });
});

it("waits for the later of the hourly limit and the cooldown", () => {
  const limited = { ...policy, attempts_in_window: 3, window_started_at: "2026-11-22T11:01:00Z", last_queued_at: "2026-11-22T11:59:00Z" };
  expect(restartStatus(limited, context)).toMatchObject({ tone: "warn", label: "Limit reached", until: "2026-11-22T12:04:00.000Z" });
});

it("reports a failed health read only when no other state applies", () => {
  expect(restartStatus({ ...policy, last_error: "Could not inspect Docker health; no automatic restart was queued." }, context)).toMatchObject({ tone: "warn", label: "Health check failed" });
  expect(restartStatus({ ...policy, last_error: "x" }, { ...context, manuallyStopped: true })).toMatchObject({ label: "Paused" });
});

it("does not claim checks are running while the worker is unknown", () => {
  expect(restartStatus(policy, { ...context, node: undefined })).toEqual({ tone: "neutral", label: "Loading", detail: "Reading the worker.", loading: true });
  expect(restartStatus(policy, { ...context, node: undefined, readFailed: true })).toEqual({
    tone: "neutral",
    label: "Unknown",
    detail: "Could not confirm that checks are running because the worker could not be read.",
  });
});

it("does not claim checks are running while operations or rollouts are unread", () => {
  expect(restartStatus(policy, { ...context, activeAction: undefined, readFailed: true })).toMatchObject({ label: "Unknown", detail: "Could not confirm that checks are running because active operations could not be read." });
  expect(restartStatus(policy, { ...context, heldRollout: undefined, activeAction: undefined })).toMatchObject({ label: "Loading", detail: "Reading staged rollouts and active operations." });
  expect(restartStatus({ ...policy, last_error: "x" }, { ...context, heldRollout: undefined, readFailed: true })).toMatchObject({ label: "Unknown" });
});

it("still reports states that are known while other reads are pending", () => {
  const unknown = { ...context, node: undefined, heldRollout: undefined, activeAction: undefined, readFailed: true };
  expect(restartStatus({ ...policy, enabled: false }, unknown)).toMatchObject({ label: "Off" });
  expect(restartStatus(policy, { ...unknown, manuallyStopped: true })).toMatchObject({ label: "Paused" });
  expect(restartStatus(policy, { ...unknown, activeAction: "deploy" })).toMatchObject({ label: "Waiting" });
  expect(restartStatus({ ...policy, attempts_in_window: 1, last_queued_at: "2026-11-22T11:58:00Z", window_started_at: "2026-11-22T11:58:00Z" }, unknown)).toMatchObject({ label: "Cooling down" });
});

it("reports a sleeping app as paused instead of checking", () => {
  expect(restartStatus(policy, { ...context, idleAsleep: true })).toEqual({ tone: "neutral", label: "Paused", detail: "The app is asleep after being idle. Checks resume after it wakes." });
  expect(restartStatus(policy, { ...context, idleAsleep: true, manuallyStopped: true })).toMatchObject({ detail: "The resource was stopped manually. Checks resume after it is started." });
});

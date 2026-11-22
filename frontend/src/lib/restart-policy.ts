import type { NodeState, OperationAction, RestartPolicy } from "../api/types";
import { nodeStates, type Tone } from "./nodes";
import { actionLabels } from "./operations";

// Enforced by the control plane's health monitor; shown here so the copy matches the server.
export const restartLimits = { cooldownMinutes: 5, attemptsPerHour: 3 } as const;

const minute = 60_000;

export interface RestartContext {
  now: number;
  manuallyStopped: boolean;
  // Undefined fields are still loading or could not be read, so the monitor's next step cannot be confirmed.
  node?: { name: string; state: NodeState; supported: boolean };
  heldRollout?: boolean;
  // Null when no operation is active.
  activeAction?: OperationAction | null;
  // At least one of the reads behind the undefined fields failed rather than still loading.
  readFailed: boolean;
}

export interface RestartStatus {
  tone: Tone;
  label: string;
  detail?: string;
  // When the monitor may queue the next restart, for the cooldown and hourly limit.
  until?: string;
  loading?: boolean;
}

function later(iso: string | null, minutes: number, now: number) {
  if (!iso) return undefined;
  const time = Date.parse(iso) + minutes * minute;
  return Number.isFinite(time) && time > now ? new Date(time).toISOString() : undefined;
}

// What the monitor will do next for this resource, in the order it checks: the monitor skips stopped resources,
// unavailable workers, held staged rollouts and active operations before it applies the cooldown and the hourly limit.
export function restartStatus(policy: RestartPolicy, context: RestartContext): RestartStatus {
  if (!policy.enabled) return { tone: "neutral", label: "Off" };
  const { node } = context;
  if (context.manuallyStopped) return { tone: "neutral", label: "Paused", detail: "The resource was stopped manually. Checks resume after it is started." };
  if (node && !node.supported) return { tone: "warn", label: "Not checking", detail: `${node.name} does not report health restart support. Upgrade and verify the worker.` };
  if (node && node.state !== "ready") return { tone: "warn", label: "Paused", detail: `${node.name} is ${nodeStates[node.state].label.toLowerCase()}. Checks resume when it is ready.` };
  if (context.heldRollout) return { tone: "neutral", label: "Paused", detail: "A staged rollout is waiting for promote or abort. Checks resume after it finishes." };
  if (context.activeAction) return { tone: "info", label: "Waiting", detail: `Checks resume after the active ${actionLabels[context.activeAction].toLowerCase()} finishes.` };
  const cooldownUntil = later(policy.last_queued_at, restartLimits.cooldownMinutes, context.now);
  const limitUntil = policy.attempts_in_window >= restartLimits.attemptsPerHour ? later(policy.window_started_at, 60, context.now) : undefined;
  if (limitUntil) {
    // Both gates must pass, so the cooldown can outlast the hourly window.
    const until = cooldownUntil && Date.parse(cooldownUntil) > Date.parse(limitUntil) ? cooldownUntil : limitUntil;
    return { tone: "warn", label: "Limit reached", detail: `${restartLimits.attemptsPerHour} automatic restart attempts were queued in the last hour. The next one can be queued`, until };
  }
  if (cooldownUntil) return { tone: "info", label: "Cooling down", detail: "A restart was queued recently. The next one can be queued", until: cooldownUntil };
  const unknown = [!node && "the worker", context.heldRollout === undefined && "staged rollouts", context.activeAction === undefined && "active operations"].filter((item): item is string => Boolean(item));
  if (unknown.length > 0) {
    const list = unknown.length === 1 ? unknown[0] : `${unknown.slice(0, -1).join(", ")} and ${unknown[unknown.length - 1]}`;
    return context.readFailed
      ? { tone: "neutral", label: "Unknown", detail: `Could not confirm that checks are running because ${list} could not be read.` }
      : { tone: "neutral", label: "Loading", detail: `Reading ${list}.`, loading: true };
  }
  if (policy.last_error) return { tone: "warn", label: "Health check failed", detail: "No restart was queued. The monitor tries again on its next check." };
  return { tone: "ok", label: "Checking", detail: "The panel checks Docker health about once a minute." };
}

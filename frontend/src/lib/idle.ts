import type { IdlePhase, IdlePolicy, IdleRuntime, WorkloadRuntime } from "../api/types";
import type { Tone } from "./nodes";

// Enforced by the API and the worker; shown here so the form and copy match the server.
export const idleLimits = { minSeconds: 60, maxSeconds: 86_400, defaultSeconds: 300 } as const;

export const idlePhases: Record<IdlePhase, { tone: Tone; label: string; detail: string }> = {
  awake: { tone: "ok", label: "Awake", detail: "Serving requests. The worker puts it to sleep after the idle timeout with no active requests." },
  sleeping_pending: { tone: "info", label: "Going to sleep", detail: "The worker is stopping the containers." },
  quiescing: { tone: "info", label: "Going to sleep", detail: "New requests are held while the worker takes a snapshot of the containers to stop." },
  sleeping: { tone: "neutral", label: "Sleeping", detail: "Containers are stopped. The next authorized request wakes them." },
  wake_queued: { tone: "info", label: "Waking", detail: "A wake is queued on the worker." },
  waking: { tone: "info", label: "Waking", detail: "The worker is starting the same containers and waiting for Docker health." },
  wake_failed: { tone: "bad", label: "Wake failed", detail: "Requests return 503 until it is woken. Wake it now, or check the containers and logs." },
  paused: { tone: "neutral", label: "Held", detail: "A manual stop or an environment pause holds the app, so requests do not wake it." },
  blocked: { tone: "warn", label: "Blocked", detail: "The idle gateway is waiting for a safe configuration and holds requests until it has one." },
  unknown: { tone: "neutral", label: "Unknown", detail: "The worker could not report the idle state." },
};

export function idlePhase(phase: string | undefined) {
  return phase !== undefined && Object.hasOwn(idlePhases, phase) ? (phase as IdlePhase) : "unknown";
}

export type IdleHold =
  | { kind: "none" }
  // Containers are stopped by idle sleep; Start, Restart and Wake now wake them.
  | { kind: "asleep"; phase: "sleeping" | "wake_failed" }
  | { kind: "sleeping" }
  | { kind: "waking" }
  | { kind: "loading" }
  | { kind: "unknown" };

// A sleeping app keeps its stopped containers as the wake snapshot, so the worker rejects replacing them until it is awake.
// Failed reads discard earlier data, and while the saved policy is on an unreadable state blocks replacements too.
export function idleHold({ policyEnabled, runtime, read }: { policyEnabled?: boolean; runtime?: WorkloadRuntime; read: "pending" | "error" | "success" }): IdleHold {
  if (read !== "success" || !runtime) {
    if (!policyEnabled) return { kind: "none" };
    return read === "error" ? { kind: "unknown" } : { kind: "loading" };
  }
  const idleSuspended = runtime.suspension?.reason === "idle";
  if (!runtime.idle) {
    if (!idleSuspended) return { kind: "none" };
    return runtime.suspension?.phase === "paused" ? { kind: "asleep", phase: "sleeping" } : runtime.suspension?.phase === "resuming" ? { kind: "waking" } : { kind: "sleeping" };
  }
  switch (idlePhase(runtime.idle.phase)) {
    case "awake":
    case "blocked":
      return { kind: "none" };
    case "paused":
      // The manual stop or environment hold already blocks replacements; only the idle snapshot matters here.
      return idleSuspended ? { kind: "asleep", phase: "sleeping" } : { kind: "none" };
    case "sleeping":
      return { kind: "asleep", phase: "sleeping" };
    case "wake_failed":
      return { kind: "asleep", phase: "wake_failed" };
    case "sleeping_pending":
    case "quiescing":
      return { kind: "sleeping" };
    case "wake_queued":
    case "waking":
      return { kind: "waking" };
    default:
      return { kind: "unknown" };
  }
}

// Stop is never held. Start and Restart wake a sleeping app; replacing actions wait until it is awake.
export type IdleAction = "deploy" | "rollback" | "promote_release" | "start" | "restart";

export function idleBlocks(hold: IdleHold, action: IdleAction) {
  switch (hold.kind) {
    case "none":
      return false;
    case "asleep":
      return action !== "start" && action !== "restart";
    case "loading":
    case "unknown":
      return action !== "start" && action !== "restart";
    default:
      return true;
  }
}

export function idleReason(hold: IdleHold, action: IdleAction, what: string) {
  if (!idleBlocks(hold, action)) return null;
  switch (hold.kind) {
    case "asleep":
      return `The app is asleep. ${what} is available after it wakes: use Start or Wake now first.`;
    case "sleeping":
      return `The app is going to sleep. ${what} is available after it wakes.`;
    case "waking":
      return `The app is waking. ${what} is available when it is awake.`;
    case "loading":
      return "Checking whether the app is asleep.";
    default:
      return `Cove cannot read whether the app is asleep. ${what} is available once its idle state can be read.`;
  }
}

export interface IdleDraft {
  enabled: boolean;
  after: string;
  requestDriven: boolean;
}

export interface IdleErrors {
  after?: string;
  requestDriven?: string;
}

export function idleDraft(policy: IdlePolicy): IdleDraft {
  return { enabled: policy.enabled, after: String(policy.after_seconds || idleLimits.defaultSeconds), requestDriven: policy.request_driven };
}

export function sameIdleDraft(a: IdleDraft, b: IdleDraft) {
  return a.enabled === b.enabled && a.after.trim() === b.after.trim() && a.requestDriven === b.requestDriven;
}

export function validateIdle(draft: IdleDraft): { errors: IdleErrors; policy?: IdlePolicy } {
  const errors: IdleErrors = {};
  const text = draft.after.trim();
  const seconds = Number(text);
  if (!/^\d+$/.test(text) || seconds < idleLimits.minSeconds || seconds > idleLimits.maxSeconds) {
    errors.after = `Enter a whole number of seconds from ${idleLimits.minSeconds} to ${idleLimits.maxSeconds.toLocaleString("en-US")}.`;
  }
  if (draft.enabled && !draft.requestDriven) errors.requestDriven = "Confirm that every service is a request-driven HTTP app before turning idle sleep on.";
  if (errors.after || errors.requestDriven) return { errors };
  return { errors, policy: { enabled: draft.enabled, after_seconds: seconds, request_driven: draft.requestDriven } };
}

export function formatSeconds(seconds: number) {
  if (seconds % 3600 === 0) return `${seconds / 3600} ${seconds === 3600 ? "hour" : "hours"}`;
  if (seconds % 60 === 0) return `${seconds / 60} ${seconds === 60 ? "minute" : "minutes"}`;
  return `${seconds} seconds`;
}

// The worker reports the same idle helper state on every read; only an awake app with no requests is counting down.
export function idleIsCountingDown(idle: IdleRuntime) {
  return idlePhase(idle.phase) === "awake" && idle.active_requests === 0;
}

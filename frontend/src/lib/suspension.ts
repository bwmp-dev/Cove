import type { EnvironmentSuspension, SuspendedResource, SuspensionPhase } from "../api/types";
import type { Tone } from "./nodes";

export const environmentPhases: Record<SuspensionPhase, { tone: Tone; label: string }> = {
  active: { tone: "ok", label: "Active" },
  pausing: { tone: "info", label: "Pausing" },
  paused: { tone: "neutral", label: "Paused" },
  resuming: { tone: "info", label: "Resuming" },
  error: { tone: "bad", label: "Failed" },
};

// A resource that is not held by the environment may still be stopped for other reasons, so it is not called running.
export const resourcePhases: Record<SuspensionPhase, { tone: Tone; label: string }> = {
  ...environmentPhases,
  active: { tone: "neutral", label: "Not paused" },
};

export function isSettling(phase: SuspensionPhase) {
  return phase === "pausing" || phase === "resuming";
}

export type EnvironmentHold =
  | { kind: "none" }
  | { kind: "paused"; resource?: SuspendedResource }
  | { kind: "pending"; resource: SuspendedResource }
  | { kind: "failed"; resource: SuspendedResource; paused: boolean; repairable: boolean }
  | { kind: "loading" }
  | { kind: "unknown" };

// A failed read discards earlier data: a stale "active" must not unlock actions. The resource's own desired flag can still
// report a known pause while the environment read is unavailable.
export function environmentHold({ resourceId, environmentId, environmentPaused, suspension, read }: { resourceId: string; environmentId?: string; environmentPaused?: boolean; suspension?: EnvironmentSuspension; read: "pending" | "error" | "success" }): EnvironmentHold {
  if (!environmentId) return { kind: "none" };
  if (read !== "success" || !suspension) {
    if (environmentPaused) return { kind: "paused" };
    return read === "error" ? { kind: "unknown" } : { kind: "loading" };
  }
  const resource = suspension.resources.find((item) => item.id === resourceId);
  // The API accepts repairs only for recorded resume failures, not for failed or cancelled operations without one.
  if (resource?.phase === "error") return { kind: "failed", resource, paused: suspension.paused, repairable: !suspension.paused && Boolean(resource.error) };
  if (suspension.paused) return { kind: "paused", resource };
  if (resource && resource.phase !== "active") return { kind: "pending", resource };
  return { kind: "none" };
}

// Stop, Abort, backups, builds, image checks and route changes are never held.
export type HeldAction = "deploy" | "start" | "restart" | "rollback" | "rollout_promote" | "restore" | "drill" | "restore_copy" | "promote_release" | "placement";

// Restored copies, drills and release promotion still wait for a successful resume.
const repairActions = new Set<HeldAction>(["deploy", "start", "restart", "restore"]);

export function holdBlocks(hold: EnvironmentHold, action: HeldAction) {
  switch (hold.kind) {
    case "none":
      return false;
    case "failed":
      return !(hold.repairable && repairActions.has(action));
    default:
      return true;
  }
}

export function holdReason(hold: EnvironmentHold, action: HeldAction, what: string) {
  if (!holdBlocks(hold, action)) return null;
  switch (hold.kind) {
    case "paused":
      return `The environment is paused. ${what} is available after it resumes.`;
    case "pending":
      return `The environment is resuming. ${what} is available when the resume finishes.`;
    case "failed":
      return `The environment's last ${hold.paused ? "pause" : "resume"} failed for this resource. Retry it from the environment first.`;
    case "loading":
      return "Checking whether the environment is paused.";
    default:
      return `Cove cannot read whether the environment is paused. ${what} is available once it can be read.`;
  }
}

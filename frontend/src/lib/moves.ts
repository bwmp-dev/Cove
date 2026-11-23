import { ApiError } from "../api/client";
import type { Node } from "../api/types";
import type { Tone } from "./nodes";

export type MovePhase =
  | "queued"
  | "freezing"
  | "exporting"
  | "transferring"
  | "starting"
  | "committing"
  | "cleaning"
  | "rolling_back"
  | "discarding"
  | "succeeded"
  | "rolled_back";

interface PhaseInfo {
  label: string;
  tone: Tone;
  detail: string;
}

// rolled_back covers failed and cancelled moves; the operation records which one it was.
export const movePhases: Record<MovePhase, PhaseInfo> = {
  queued: { label: "Queued", tone: "neutral", detail: "Waiting to start. Nothing has been stopped or copied yet." },
  freezing: { label: "Stopping the source", tone: "info", detail: "Source containers are stopping with restart disabled. The resource is offline until the move finishes or rolls back." },
  exporting: { label: "Exporting", tone: "info", detail: "Packing volumes, the release, retained images and the local backup repository on the source node." },
  transferring: { label: "Transferring", tone: "info", detail: "Copying the archive to the destination node. Both nodes must stay reachable." },
  starting: { label: "Starting on destination", tone: "info", detail: "Starting the copy on the destination node and checking its health. Placement has not switched yet." },
  committing: {
    label: "Committing placement",
    tone: "info",
    detail: "Placement may already have switched to the destination. Cancelling now does not undo a committed placement.",
  },
  cleaning: {
    label: "Removing source leftovers",
    tone: "info",
    detail: "The destination is committed and runs the resource. Source leftovers are still being removed, so keep both nodes until the move succeeds.",
  },
  rolling_back: {
    label: "Rolling back",
    tone: "warn",
    detail:
      "Cove first confirms the destination copy is stopped with restart disabled. If the destination cannot be reached, the source stays stopped on purpose so two copies never write at once; the rollback continues when connectivity returns.",
  },
  discarding: { label: "Discarding destination copy", tone: "warn", detail: "Removing the incomplete copy from the destination node. Placement stays on the source." },
  succeeded: { label: "Moved", tone: "ok", detail: "The resource runs on the destination node and source leftovers were removed." },
  rolled_back: {
    label: "Rolled back",
    tone: "warn",
    detail: "The move did not complete and placement stayed on the source node. The operation shows whether it failed or was cancelled; check the resource status for whether the source is running.",
  },
};

export function isUnresolvedPhase(phase: MovePhase) {
  return phase !== "succeeded" && phase !== "rolled_back";
}

export function phaseInfo(phase: string): PhaseInfo {
  return movePhases[phase as MovePhase] ?? { label: "Unknown", tone: "warn", detail: "This move reports a phase this dashboard does not recognise. Open the operation for details." };
}

export type DestinationExclusion = "not_ready" | "needs_upgrade";

export interface Destinations {
  available: Node[];
  excluded: { node: Node; reason: DestinationExclusion }[];
}

export function supportsMoves(node: Node | undefined) {
  return Boolean(node?.capabilities?.workload_moves);
}

// Instance administrators are sent every node, so assigned is the team's node list for them; other
// members already receive only the team's nodes and pass null.
export function moveDestinations(nodes: Node[], sourceId: string, assigned: string[] | null): Destinations {
  const allowed = assigned ? new Set(assigned) : null;
  const result: Destinations = { available: [], excluded: [] };
  for (const node of nodes) {
    if (node.id === sourceId || (allowed && !allowed.has(node.id))) continue;
    if (node.state !== "ready") result.excluded.push({ node, reason: "not_ready" });
    else if (!supportsMoves(node)) result.excluded.push({ node, reason: "needs_upgrade" });
    else result.available.push(node);
  }
  return result;
}

// Client errors are definite refusals; a lost response or a server error may have queued the move.
export function ambiguousSubmission(error: unknown) {
  return !(error instanceof ApiError) || error.status === 0 || error.status >= 500;
}

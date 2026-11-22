import type { NodeState, OperationState } from "../api/types";
import { operationTones, stateLabels } from "../lib/operations";
import { nodeStates, type Tone } from "../lib/nodes";

const dotTones: Record<Tone, string> = {
  ok: "bg-ok",
  warn: "bg-warn",
  bad: "bg-bad",
  info: "bg-info",
  neutral: "bg-fg-subtle",
};

// A dot on its own is decorative; always pair it with a visible word (StatusLabel) or an accessible label.
export function StatusDot({ tone, pulse = false }: { tone: Tone; pulse?: boolean }) {
  return <span aria-hidden className={`size-2 shrink-0 rounded-full ${dotTones[tone]} ${pulse ? "animate-pulse" : ""}`} />;
}

export function StatusLabel({ tone, label, pulse = false }: { tone: Tone; label: string; pulse?: boolean }) {
  return (
    <span className={`inline-flex items-center gap-2 whitespace-nowrap text-[13px] ${tone === "neutral" ? "text-fg-muted" : "text-fg"}`}>
      <StatusDot tone={tone} pulse={pulse} />
      {label}
    </span>
  );
}

export function NodeStateLabel({ state }: { state: NodeState }) {
  const info = nodeStates[state] ?? { tone: "neutral" as const, label: state };
  return <StatusLabel tone={info.tone} label={info.label} pulse={state === "pending"} />;
}

export function OperationStateLabel({ state }: { state: OperationState }) {
  return <StatusLabel tone={operationTones[state] ?? "neutral"} label={stateLabels[state] ?? state} pulse={state === "running" || state === "queued"} />;
}

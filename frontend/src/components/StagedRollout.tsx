import { useQuery } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { ArrowUpCircle, Undo2 } from "lucide-react";
import { useId, useState } from "react";
import { errorMessage, isStatus } from "../api/client";
import { useQueueOperation } from "../api/mutations";
import { nodesQuery, workloadOperationsQuery, workloadRuntimeQuery } from "../api/queries";
import type { RolloutStatus, Workload } from "../api/types";
import { useOnline } from "../lib/hooks";
import { actionLabels, activeOperation, stateLabels } from "../lib/operations";
import { can } from "../lib/permissions";
import { rolloutPhase, rolloutsApply, rolloutStrategyLabel, trafficSummary } from "../lib/rollouts";
import { buttonClass, linkClass } from "../lib/styles";
import { ConfirmDialog } from "./ConfirmDialog";
import { StatusLabel } from "./status";
import { Button, MetaList, Notice, QueryError, Section } from "./ui";

// The staged rollout waiting for promote or abort, read from the runtime endpoint. Undefined while unknown,
// including after a failed read, so callers never act on stale state.
export function usePendingRollout(workload: Pick<Workload, "id" | "node_id" | "kind" | "preview_source_id" | "environment_preview_id">, operationActive = false) {
  const nodes = useQuery(nodesQuery);
  const node = nodes.data?.find((item) => item.id === workload.node_id);
  const runtime = useQuery({ ...workloadRuntimeQuery(workload.id, operationActive), enabled: rolloutsApply(workload) && Boolean(node?.capabilities?.rollouts) });
  const rollout = runtime.isError ? undefined : runtime.data?.rollout;
  return rollout?.phase === "waiting" ? rollout : undefined;
}

export function PendingRolloutMessage({ id, workloadId, action, className = "" }: { id?: string; workloadId: string; action: string; className?: string }) {
  return (
    <p id={id} className={`text-xs text-fg-muted ${className}`}>
      A staged rollout is waiting. Promote or abort it from the{" "}
      <Link to="/resources/$resourceId" params={{ resourceId: workloadId }} className={linkClass}>
        Overview
      </Link>{" "}
      before you {action}.
    </p>
  );
}

function RolloutDetails({ rollout }: { rollout: RolloutStatus }) {
  const phase = rolloutPhase(rollout.phase);
  const replicas = Object.entries(rollout.replicas ?? {});
  return (
    <div className="space-y-2">
      <div className="flex flex-wrap items-center gap-x-4 gap-y-1 text-[13px]">
        <StatusLabel tone={phase.tone} label={phase.label} pulse={phase.tone === "info"} />
        <MetaList className="text-fg-muted">
          <span key="strategy">{rolloutStrategyLabel(rollout.strategy)}</span>
          <Link key="operation" to="/operations/$operationId" params={{ operationId: rollout.operation }} className={linkClass}>
            View operation
          </Link>
          {rollout.release && rollout.release !== rollout.operation && (
            <Link key="release" to="/operations/$operationId" params={{ operationId: rollout.release }} className={linkClass}>
              View candidate deploy
            </Link>
          )}
        </MetaList>
      </div>
      <p className="text-[13px] text-fg-muted">{trafficSummary(rollout)}</p>
      {replicas.length > 0 && (
        <p className="text-xs text-fg-muted">
          Replicas:{" "}
          {replicas.map(([service, count], index) => (
            <span key={service}>
              {index > 0 && ", "}
              <span className="font-mono break-all text-fg">{service}</span> {count}
            </span>
          ))}
        </p>
      )}
      {rollout.reason && <p className="text-xs break-words text-fg-muted">{rollout.reason}</p>}
    </div>
  );
}

export function StagedRollout({ workload, operationActive }: { workload: Workload; operationActive: boolean }) {
  const runtime = useQuery({ ...workloadRuntimeQuery(workload.id, operationActive), enabled: rolloutsApply(workload) });
  const operations = useQuery(workloadOperationsQuery(workload.id));
  const nodes = useQuery(nodesQuery);
  const queue = useQueueOperation(workload.id);
  const online = useOnline();
  const statusId = useId();
  const [confirm, setConfirm] = useState<"rollout_promote" | "rollout_abort" | null>(null);
  const node = nodes.data?.find((item) => item.id === workload.node_id);
  // A failed operations read leaves the active operation unknown, so earlier data must not count as idle.
  const active = operations.data && !operations.isError ? activeOperation(operations.data, workload.id) : undefined;
  const canOperate = can(workload.permissions, "operate");
  const stale = runtime.isError ? runtime.data?.rollout : undefined;
  const rollout = runtime.isError ? undefined : runtime.data?.rollout;

  if (!rolloutsApply(workload)) return null;
  if (stale) {
    return (
      <Section title="Rollout">
        <Notice tone="warn" title="Rollout state is unknown">
          The last read showed a rollout in progress, but the current state could not be read from the node. Promote and abort are unavailable until it can be read again.
        </Notice>
      </Section>
    );
  }
  if (!rollout && !queue.isSuccess && !queue.isError) return null;

  const waiting = rollout?.phase === "waiting";
  const supported = Boolean(node?.capabilities?.rollouts);
  const ready = node?.state === "ready";
  const blocked = !online || !ready || !supported || Boolean(active) || queue.isPending || operations.isPending || operations.isError;
  const reason = !online
    ? "You are offline. Promote and abort are available when the connection returns."
    : operations.isError
      ? "Cove cannot tell whether another operation is running. Promote and abort are available once operations load."
      : active
      ? `${actionLabels[active.action]} ${stateLabels[active.state].toLowerCase()}. Promote and abort are available when it finishes.`
      : node && !ready
        ? `${node.name} is not ready. Promote and abort are available once the node is ready.`
        : node && !supported
          ? `${node.name} does not report rollout support. Upgrade and verify the worker to promote or abort.`
          : null;

  return (
    <Section
      title={waiting ? "Staged rollout" : "Rollout"}
      description={waiting ? "The new release is running beside the current one. It is not fully released until you promote it." : undefined}
      actions={
        waiting &&
        canOperate && (
          <div className="flex flex-wrap gap-2">
            <Button variant="danger" disabled={blocked} pending={queue.isPending && queue.variables.action === "rollout_abort"} aria-describedby={reason ? statusId : undefined} onClick={() => setConfirm("rollout_abort")}>
              <Undo2 aria-hidden className="size-3.5" />
              Abort
            </Button>
            <Button variant="primary" disabled={blocked} pending={queue.isPending && queue.variables.action === "rollout_promote"} aria-describedby={reason ? statusId : undefined} onClick={() => setConfirm("rollout_promote")}>
              <ArrowUpCircle aria-hidden className="size-3.5" />
              Promote
            </Button>
          </div>
        )
      }
    >
      <div className="space-y-3" aria-live="polite">
        {rollout ? <RolloutDetails rollout={rollout} /> : <p className="text-[13px] text-fg-muted">No rollout is pending.</p>}
        {waiting && !canOperate && <p className="text-xs text-fg-muted">Promoting or aborting needs operator access.</p>}
        {waiting && canOperate && operations.isError && (
          <QueryError what="operations" error={operations.error} onRetry={() => void operations.refetch()} retrying={operations.isFetching} />
        )}
        {waiting && canOperate && reason && (
          <p id={statusId} className="text-xs text-fg-muted">
            {reason}
          </p>
        )}
        {queue.isSuccess && (
          <Notice
            tone="success"
            title={`${actionLabels[queue.variables.action]} queued`}
            action={
              <Link to="/operations/$operationId" params={{ operationId: queue.data.id }} className={buttonClass("ghost")}>
                View operation
              </Link>
            }
          />
        )}
        {queue.isError && (
          <Notice tone={isStatus(queue.error, 409) ? "warn" : "error"} title={`Could not queue ${actionLabels[queue.variables.action].toLowerCase()}`}>
            {errorMessage(queue.error)}
          </Notice>
        )}
      </div>
      <ConfirmDialog
        open={confirm === "rollout_promote"}
        title="Promote the new release?"
        confirmLabel="Promote release"
        onCancel={() => setConfirm(null)}
        onConfirm={() => {
          setConfirm(null);
          queue.mutate({ action: "rollout_promote" });
        }}
      >
        The new release&apos;s health is checked again, then it receives all traffic and the current release drains and stops. Deployment approval, freeze, window and quota
        rules apply.
      </ConfirmDialog>
      <ConfirmDialog
        open={confirm === "rollout_abort"}
        title="Abort this rollout?"
        confirmLabel="Abort rollout"
        danger
        onCancel={() => setConfirm(null)}
        onConfirm={() => {
          setConfirm(null);
          queue.mutate({ action: "rollout_abort" });
        }}
      >
        All traffic returns to the current release and the new release&apos;s containers are removed. Deploy again to retry. Abort is allowed during a deployment freeze.
      </ConfirmDialog>
    </Section>
  );
}

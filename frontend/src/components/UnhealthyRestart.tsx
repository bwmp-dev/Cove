import { useQuery } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { useId, useState, type FormEvent } from "react";
import { errorMessage, isStatus } from "../api/client";
import { useUpdateRestartPolicy } from "../api/mutations";
import { nodesQuery, restartPolicyQuery, workloadOperationsQuery, workloadRuntimeQuery } from "../api/queries";
import type { Capabilities, Operation, RestartPolicy, Workload, WorkloadRuntime } from "../api/types";
import { useNow, useOnline } from "../lib/hooks";
import { activeOperation } from "../lib/operations";
import { can } from "../lib/permissions";
import { restartLimits, restartStatus, type RestartContext } from "../lib/restart-policy";
import { idleHold } from "../lib/idle";
import { rolloutsApply } from "../lib/rollouts";
import { linkClass } from "../lib/styles";
import { RelativeTime } from "./RelativeTime";
import { SaveBar, UnsavedChangesGuard } from "./SaveBar";
import { OperationStateLabel, StatusLabel } from "./status";
import { Notice, QueryError, Section, SettingRow, SettingRows, Spinner, SwitchField } from "./ui";

const savedStatus = "Saved. The panel applies it on its next health check; no deploy is needed.";

export function UnhealthyRestartSettings({ workload }: { workload: Workload }) {
  const id = useId();
  const online = useOnline();
  const editable = can(workload.permissions, "edit");
  const policy = useQuery(restartPolicyQuery(workload.id));
  const nodes = useQuery(nodesQuery);
  const node = nodes.data?.find((item) => item.id === workload.node_id);
  const supported = Boolean(node?.capabilities?.unhealthy_restart);
  const operations = useQuery(workloadOperationsQuery(workload.id));
  const active = operations.data ? activeOperation(operations.data, workload.id) : undefined;
  const runtime = useQuery({ ...workloadRuntimeQuery(workload.id, Boolean(active)), enabled: rolloutsApply(workload) && Boolean(node?.capabilities?.rollouts) });
  const enabled = policy.data?.enabled === true;
  const capabilities = nodes.isError ? undefined : (node?.capabilities ?? undefined);
  const context: Omit<RestartContext, "now" | "manuallyStopped"> = {
    node: node && capabilities ? { name: node.name, state: node.state, supported } : undefined,
    heldRollout: heldRollout(workload, capabilities, runtime),
    idleAsleep: runtime.data && !runtime.isError ? idleHold({ runtime: runtime.data, read: "success" }).kind !== "none" : false,
    activeAction: operations.isError || !operations.data ? undefined : (active?.action ?? null),
    readFailed: nodes.isError || (nodes.isSuccess && !capabilities) || operations.isError || runtime.isError,
  };
  const update = useUpdateRestartPolicy(workload.id);
  const [draft, setDraft] = useState<boolean | null>(null);
  const saved = policy.data?.enabled;
  const checked = draft ?? saved ?? false;
  const dirty = draft !== null && saved !== undefined && draft !== saved;
  // Turning the policy off is always allowed; turning it on needs a worker known to support it.
  const switchDisabled = !editable || saved === undefined || update.isPending || (!supported && !saved);

  function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    if (!dirty || draft === null) return;
    update.mutate(draft, { onSuccess: () => setDraft(null) });
  }

  return (
    <form onSubmit={submit} noValidate>
      <UnsavedChangesGuard dirty={dirty} />
      <Section
        width="form"
        title="Automatic health restarts"
        description="Restarts containers that Docker reports unhealthy. Saved separately from the settings above and applied without a deploy."
        footer={editable && policy.data && <SaveBar dirty={dirty} pending={update.isPending} blocked={!online} onDiscard={() => setDraft(null)} status={update.isSuccess ? savedStatus : undefined} />}
      >
        <div className="space-y-5">
          {!editable && <p className="text-xs text-fg-muted">Changing this needs editor access. Ask a team owner or admin.</p>}
          {editable && !online && <Notice tone="warn" title="You are offline">Changes can be saved when the connection returns.</Notice>}
          {(editable || enabled) && nodes.isError && <QueryError what="worker capabilities" error={nodes.error} onRetry={() => void nodes.refetch()} retrying={nodes.isFetching} />}
          {(editable || enabled) && nodes.isSuccess && !node && (
            <Notice tone="warn" title="Worker unknown">
              The worker for this resource is not in the node list, so its health restart support cannot be confirmed.
            </Notice>
          )}
          {(editable || enabled) && !nodes.isError && node && !node.capabilities && (
            <Notice tone="warn" title="Worker capabilities unknown">
              {node.name} has not reported its capabilities, so health restart support cannot be confirmed and automatic restarts cannot be turned on. Verify it from the{" "}
              <Link to="/nodes/$nodeId" params={{ nodeId: node.id }} className={linkClass}>
                node page
              </Link>
              .{saved ? " You can still turn the saved policy off." : ""}
            </Notice>
          )}
          {editable && node?.capabilities && !supported && (
            <Notice tone="warn" title="Worker upgrade needed for health restarts">
              {node.name} does not report health restart support, so automatic restarts cannot be turned on. Upgrade the worker, then verify it from the{" "}
              <Link to="/nodes/$nodeId" params={{ nodeId: node.id }} className={linkClass}>
                node page
              </Link>
              .{saved ? " You can still turn the saved policy off." : ""}
            </Notice>
          )}
          {policy.isPending ? (
            <p className="flex items-center gap-2 text-[13px] text-fg-muted">
              <Spinner />
              Loading the restart policy
            </p>
          ) : policy.isError ? (
            <QueryError what="the restart policy" error={policy.error} onRetry={() => void policy.refetch()} retrying={policy.isFetching} />
          ) : (
            <SettingRows>
              <SwitchField
                layout="row"
                id={`${id}-enabled`}
                checked={checked}
                disabled={switchDisabled}
                onChange={(event) => {
                  update.reset();
                  setDraft(event.target.checked);
                }}
                label="Restart unhealthy containers"
                description="Restarts only running containers whose Docker healthcheck reports unhealthy. Containers that are starting, exited or have no healthcheck are left alone. Images are not pulled or rebuilt, and a restart can interrupt traffic."
              />
              <PolicyStatus workload={workload} policy={policy.data} context={context} operations={operations.data} />
            </SettingRows>
          )}
          {enabled && operations.isError && <QueryError what="active operations" error={operations.error} onRetry={() => void operations.refetch()} retrying={operations.isFetching} />}
          {enabled && runtime.isError && <QueryError what="the staged rollout state" error={runtime.error} onRetry={() => void runtime.refetch()} retrying={runtime.isFetching} />}
          {policy.data?.last_error && (
            <Notice tone="warn" title="Last health check failed">
              <span className="break-words">{policy.data.last_error}</span>
            </Notice>
          )}
          <Notice title="Limits">
            <ul className="list-disc space-y-0.5 pl-4">
              <li>
                At most one automatic restart is queued every {restartLimits.cooldownMinutes} minutes, and {restartLimits.attemptsPerHour} in any rolling hour. Every queued attempt
                counts, even one that is cancelled, fails or finds nothing to restart, and turning this off does not reset the count.
              </li>
              <li>The panel checks about once a minute while it is online. Applications and routes keep running when it is offline, but nothing is restarted automatically then.</li>
              <li>Stopped resources, active operations and staged rollouts waiting for promote or abort are skipped.</li>
              <li>Turning this off cancels queued automatic restarts; one already running may finish.</li>
              <li>A restart does not fix the cause. Containers can report unhealthy again afterwards, so choose healthchecks that mean a restart helps.</li>
            </ul>
          </Notice>
          {update.isError && (
            <Notice tone={isStatus(update.error, 409) ? "warn" : "error"} title="Could not save the restart policy">
              {errorMessage(update.error)}
            </Notice>
          )}
        </div>
      </Section>
    </form>
  );
}

function heldRollout(workload: Workload, capabilities: Capabilities | undefined, runtime: { data?: WorkloadRuntime; isError: boolean }) {
  if (!rolloutsApply(workload)) return false;
  if (!capabilities) return undefined;
  if (!capabilities.rollouts) return false;
  if (runtime.isError || !runtime.data) return undefined;
  return runtime.data.rollout?.phase === "waiting";
}

function PolicyStatus({ workload, policy, context, operations }: { workload: Workload; policy: RestartPolicy; context: Omit<RestartContext, "now" | "manuallyStopped">; operations?: Operation[] }) {
  const now = useNow(15_000);
  const status = restartStatus(policy, { ...context, now, manuallyStopped: workload.manually_stopped });
  const last = policy.operation_id ? operations?.find((item) => item.id === policy.operation_id) : undefined;
  return (
    <>
      <SettingRow label="Status" description={status.detail && <StatusDetail detail={status.detail} until={status.until} />}>
        {status.loading ? (
          <span className="inline-flex items-center gap-2 text-[13px] text-fg-muted">
            <Spinner />
            {status.label}
          </span>
        ) : (
          <StatusLabel tone={status.tone} label={status.label} />
        )}
      </SettingRow>
      <SettingRow
        label="Queued attempts in the last hour"
        description={
          <>
            {policy.window_started_at && (
              <>
                Oldest counted attempt <RelativeTime value={policy.window_started_at} />.{" "}
              </>
            )}
            Cancelled, failed and no-op attempts count toward the limit.
          </>
        }
      >
        <span className="tabular-nums text-fg">
          {policy.attempts_in_window} of {restartLimits.attemptsPerHour}
        </span>
      </SettingRow>
      <SettingRow label="Last queued restart">
        {policy.last_queued_at ? (
          <>
            <span className="text-fg-muted">
              Queued <RelativeTime value={policy.last_queued_at} />
            </span>
            {last && <OperationStateLabel state={last.state} />}
            {policy.operation_id && (
              <Link to="/operations/$operationId" params={{ operationId: policy.operation_id }} className={linkClass}>
                View operation
              </Link>
            )}
          </>
        ) : (
          <span className="text-fg-muted">None queued</span>
        )}
      </SettingRow>
    </>
  );
}

function StatusDetail({ detail, until }: { detail: string; until?: string }) {
  if (!until) return detail;
  return (
    <>
      {detail} <RelativeTime value={until} />.
    </>
  );
}

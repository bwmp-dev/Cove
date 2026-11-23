import { useQuery } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { AlarmClock, RotateCw } from "lucide-react";
import { useId, useState, type FormEvent, type ReactNode } from "react";
import { errorMessage, isStatus } from "../api/client";
import { useQueueOperation, useUpdateIdlePolicy } from "../api/mutations";
import { idlePolicyQuery, nodesQuery, workloadOperationsQuery, workloadRuntimeQuery } from "../api/queries";
import type { IdlePolicy, IdleRuntime, Node, Operation, Workload } from "../api/types";
import { countLabel } from "../lib/hierarchy";
import { useOnline } from "../lib/hooks";
import { formatSeconds, idleDraft, idleHold, idleLimits, idlePhase, idlePhases, sameIdleDraft, validateIdle, type IdleDraft, type IdleErrors, type IdleHold } from "../lib/idle";
import { actionLabels, activeOperation } from "../lib/operations";
import { can } from "../lib/permissions";
import { rolloutsApply } from "../lib/rollouts";
import { describedBy, inputClass, linkClass } from "../lib/styles";
import { holdBlocks, type EnvironmentHold } from "../lib/suspension";
import { Checkbox } from "./Checkbox";
import { useEnvironmentHold } from "./EnvironmentSuspension";
import { RelativeTime } from "./RelativeTime";
import { SaveBar, UnsavedChangesGuard } from "./SaveBar";
import { OperationStateLabel, StatusLabel } from "./status";
import { Button, Field, Notice, QueryError, Section, SettingRow, SettingRows, Spinner, SwitchField } from "./ui";

type IdleResource = Pick<Workload, "kind" | "preview_source_id" | "environment_preview_id"> & { id: string };

// Operations are listed newest first, so the first idle_policy entries are the latest change and the latest applied one.
function latestPolicyChange(operations: Operation[] | undefined, workloadId: string) {
  return operations?.find((operation) => operation.workload_id === workloadId && operation.action === "idle_policy");
}

function latestApplied(operations: Operation[] | undefined, workloadId: string) {
  return operations?.find((operation) => operation.workload_id === workloadId && operation.action === "idle_policy" && operation.state === "succeeded")?.id ?? null;
}

// The saved policy and the worker's live idle state. Only services outside previews can sleep, so other resources are never held.
export function useIdleState(resource: IdleResource) {
  const applies = rolloutsApply(resource);
  const operations = useQuery(workloadOperationsQuery(resource.id));
  const active = operations.data ? activeOperation(operations.data, resource.id) : undefined;
  const policy = useQuery({ ...idlePolicyQuery(resource.id, latestApplied(operations.data, resource.id)), enabled: applies && !operations.isPending });
  const runtime = useQuery({ ...workloadRuntimeQuery(resource.id, Boolean(active)), enabled: applies });
  const hold: IdleHold = applies
    ? idleHold({ policyEnabled: policy.data?.enabled, runtime: runtime.isError ? undefined : runtime.data, read: runtime.isError ? "error" : runtime.isSuccess ? "success" : "pending" })
    : { kind: "none" };
  return { applies, hold, policy, runtime, operations, active };
}

type IdleState = ReturnType<typeof useIdleState>;

type ReadState = { isPending: boolean; isError: boolean; error: unknown };

// Waking needs confirmed operations and a confirmed ready worker that supports idle sleep, so unread, missing or old workers keep it disabled.
// A failed refetch keeps the last operations, but they may be stale, so the error wins over them.
function wakeBlocker({ online, active, operations, nodes, node, environment, manuallyStopped }: { online: boolean; active?: Operation; operations: ReadState; nodes: ReadState; node?: Node; environment: EnvironmentHold; manuallyStopped: boolean }) {
  if (!online) return "You are offline. Waking is available when the connection returns.";
  if (operations.isError) return `Could not load active operations, so Cove cannot confirm the app can be woken: ${errorMessage(operations.error)}`;
  if (active) return `Waking is available after the active ${actionLabels[active.action].toLowerCase()} finishes.`;
  if (operations.isPending) return "Checking for active operations.";
  if (nodes.isError) return `Could not load the worker, so Cove cannot confirm it can wake the app: ${errorMessage(nodes.error)}`;
  if (nodes.isPending) return "Checking the worker.";
  if (!node) return "The worker for this resource is not in the node list, so the app cannot be woken.";
  if (!node.capabilities) return `${node.name} has not reported its capabilities, so idle sleep support cannot be confirmed.`;
  if (!node.capabilities.idle_sleep) return `${node.name} does not report idle sleep support. Upgrade the worker to wake the app.`;
  if (node.state !== "ready") return `${node.name} is not ready. The worker must be online to wake the app.`;
  if (manuallyStopped) return "The resource was stopped manually, so it does not wake. Start it instead.";
  if (holdBlocks(environment, "start")) return "The environment is paused or its state is unknown, so the app cannot be woken.";
  return null;
}

// Wake now queues the existing Start operation, which starts the same sleeping containers.
// retryOperations is off where the surrounding form already offers Retry for the same operations read.
function WakeNow({ workload, idle, size = "md", retryOperations = true }: { workload: Workload; idle: IdleState; size?: "sm" | "md"; retryOperations?: boolean }) {
  const queue = useQueueOperation(workload.id);
  const online = useOnline();
  const nodes = useQuery(nodesQuery);
  const { hold: environment } = useEnvironmentHold(workload);
  const reasonId = useId();
  if (!can(workload.permissions, "operate") || idle.hold.kind !== "asleep") return null;
  const node = nodes.data?.find((item) => item.id === workload.node_id);
  const { operations } = idle;
  const reason = wakeBlocker({ online, active: idle.active, operations, nodes, node, environment, manuallyStopped: workload.manually_stopped });
  const retry = operations.isError ? (retryOperations ? operations : undefined) : nodes.isError ? nodes : undefined;
  return (
    <div className="flex min-w-0 flex-col gap-1.5 @lg:items-end">
      <Button size={size} disabled={Boolean(reason)} pending={queue.isPending} aria-describedby={reason ? reasonId : undefined} onClick={() => queue.mutate({ action: "start" })}>
        {!queue.isPending && <AlarmClock aria-hidden className="size-3.5" />}
        Wake now
      </Button>
      {reason && (
        <p id={reasonId} className="text-xs break-words text-fg-muted">
          {reason}
        </p>
      )}
      {retry && (
        <Button variant="ghost" size="sm" onClick={() => void retry.refetch()} pending={retry.isFetching}>
          {!retry.isFetching && <RotateCw aria-hidden className="size-3.5" />}
          Retry
        </Button>
      )}
      {queue.isSuccess && (
        <p role="status" className="text-xs text-fg-muted">
          Wake queued.{" "}
          <Link to="/operations/$operationId" params={{ operationId: queue.data.id }} className={linkClass}>
            View operation
          </Link>
        </p>
      )}
      {queue.isError && (
        <p role="alert" className="text-xs break-words text-bad">
          Could not queue the wake: {errorMessage(queue.error)}
        </p>
      )}
    </div>
  );
}

function holdNoticeText(hold: IdleHold) {
  switch (hold.kind) {
    case "asleep":
      return hold.phase === "wake_failed"
        ? { tone: "error" as const, title: "Waking the app failed", text: "Requests return 503 until it is woken again. Deploy, rollback and promotion are available after it wakes. Stop stays available." }
        : { tone: "info" as const, title: "App is asleep", text: "Its containers are stopped after being idle. The next authorized request wakes it, or wake it now. Deploy, rollback and promotion are available after it wakes." };
    case "sleeping":
      return { tone: "info" as const, title: "App is going to sleep", text: "The worker is stopping its containers. Deploy, start, restart, rollback and promotion are available after it wakes." };
    case "waking":
      return { tone: "info" as const, title: "App is waking", text: "The worker is starting the same containers and waiting for Docker health. Deploy, rollback and promotion are available when it is awake." };
    default:
      return null;
  }
}

export function IdleHoldNotice({ id, workload, idle }: { id?: string; workload: Workload; idle: IdleState }) {
  const { hold, runtime } = idle;
  if (hold.kind === "unknown") {
    return (
      <div id={id}>
        <Notice
          tone="warn"
          title="Idle sleep state unknown"
          action={
            <Button variant="ghost" size="sm" onClick={() => void runtime.refetch()} pending={runtime.isFetching}>
              {!runtime.isFetching && <RotateCw aria-hidden className="size-3.5" />}
              Retry
            </Button>
          }
        >
          {runtime.isError ? `${errorMessage(runtime.error)} ` : "The worker reported an idle state Cove does not recognise. "}
          Idle sleep is on, so deploy, rollback and promotion stay unavailable until Cove can confirm the app is awake. Start, Restart and Stop stay available.
        </Notice>
      </div>
    );
  }
  const notice = holdNoticeText(hold);
  if (!notice) return null;
  return (
    <div id={id}>
      <Notice tone={notice.tone} title={notice.title} action={<WakeNow workload={workload} idle={idle} size="sm" />}>
        {notice.text}
        {runtime.data?.idle?.error && <span className="block break-words">{runtime.data.idle.error}</span>}
      </Notice>
    </div>
  );
}

function NodeCapabilityNotice({ nodes, node, saved }: { nodes: { isError: boolean; isSuccess: boolean; error: unknown; refetch: () => unknown; isFetching: boolean }; node?: Node; saved?: IdlePolicy }) {
  const nodeLink = node && (
    <Link to="/nodes/$nodeId" params={{ nodeId: node.id }} className={linkClass}>
      node page
    </Link>
  );
  if (nodes.isError) return <QueryError what="worker capabilities" error={nodes.error} onRetry={() => void nodes.refetch()} retrying={nodes.isFetching} />;
  if (nodes.isSuccess && !node) {
    return (
      <Notice tone="warn" title="Worker unknown">
        The worker for this resource is not in the node list, so idle sleep support cannot be confirmed and the policy cannot be changed.
      </Notice>
    );
  }
  if (node && !node.capabilities) {
    return (
      <Notice tone="warn" title="Worker capabilities unknown">
        {node.name} has not reported its capabilities, so idle sleep support cannot be confirmed and the policy cannot be changed. Verify it from the {nodeLink}.
      </Notice>
    );
  }
  if (node?.capabilities && !node.capabilities.idle_sleep) {
    return (
      <Notice tone="warn" title="Worker upgrade needed for idle sleep">
        {node.name} does not report idle sleep support. Upgrade the worker, then verify it from the {nodeLink}.
        {saved?.enabled ? " The saved policy stays on and cannot be turned off until then, because only a supporting worker can safely wake the app and remove its idle helper." : ""}
      </Notice>
    );
  }
  if (node && node.state !== "ready") {
    return (
      <Notice tone="warn" title="The node is not ready">
        Policy changes need the worker online. A sleeping app also needs its worker online to wake; awake apps keep serving.
      </Notice>
    );
  }
  return null;
}

function saveBlocker({ online, node, operations, active, draft, manuallyStopped, environment }: { online: boolean; node?: Node; operations: { isSuccess: boolean }; active?: Operation; draft: IdleDraft; manuallyStopped: boolean; environment: EnvironmentHold }): ReactNode {
  if (!online) return "You are offline. Changes can be saved when the connection returns.";
  if (!node?.capabilities?.idle_sleep || node.state !== "ready") return "Changes need a ready worker that supports idle sleep.";
  if (!operations.isSuccess) return "Changes can be saved once active operations can be read.";
  if (active) {
    return (
      <>
        {active.action === "idle_policy" ? "The worker is applying an idle sleep policy change." : `Changes cannot be saved until the active ${actionLabels[active.action].toLowerCase()} finishes.`}{" "}
        <Link to="/operations/$operationId" params={{ operationId: active.id }} className={linkClass}>
          View operation
        </Link>
      </>
    );
  }
  if (draft.enabled && manuallyStopped) return "The resource was stopped manually. Start it before turning idle sleep on. You can still turn it off.";
  if (draft.enabled && environment.kind !== "none") {
    return environment.kind === "unknown" || environment.kind === "loading"
      ? "Cove cannot confirm the environment is running, so idle sleep cannot be turned on. You can still turn it off."
      : "The environment is paused or resuming. Turn idle sleep on after it resumes. You can still turn it off.";
  }
  return null;
}

// The node, environment and operation gates for saving the policy.
function useSaveGates(workload: Workload, idle: IdleState, values?: IdleDraft) {
  const online = useOnline();
  const nodes = useQuery(nodesQuery);
  const node = nodes.isError ? undefined : nodes.data?.find((item) => item.id === workload.node_id);
  const { hold: environment } = useEnvironmentHold(workload);
  const blocker = values ? saveBlocker({ online, node, operations: idle.operations, active: idle.active, draft: values, manuallyStopped: workload.manually_stopped, environment }) : null;
  return { nodes, node, blocker, supported: Boolean(node?.capabilities?.idle_sleep) };
}

function useIdleDraft(policy?: IdlePolicy) {
  const [draft, setDraft] = useState<IdleDraft | null>(null);
  const [errors, setErrors] = useState<IdleErrors>({});
  const saved = policy && idleDraft(policy);
  const values = draft ?? saved;
  const dirty = Boolean(draft && saved && !sameIdleDraft(draft, saved));
  return { draft, setDraft, errors, setErrors, values, dirty };
}

// While the worker applies a queued change the input stays as entered, but it is no longer an unsaved edit.
function applyState(idle: IdleState, queuedId: string | undefined) {
  const applying = idle.active?.action === "idle_policy";
  const queued = queuedId ? idle.operations.data?.find((operation) => operation.id === queuedId) : undefined;
  return { applying, status: saveStatus(applying, queued) };
}

export function IdleSleepSettings({ workload }: { workload: Workload }) {
  const id = useId();
  const editable = can(workload.permissions, "edit");
  const idle = useIdleState(workload);
  const { policy, operations } = idle;
  const update = useUpdateIdlePolicy(workload.id);
  const { draft, setDraft, errors, setErrors, values, dirty } = useIdleDraft(policy.data);
  const { nodes, node, blocker, supported } = useSaveGates(workload, idle, values);
  const change = latestPolicyChange(operations.data, workload.id);
  const locked = !editable || update.isPending || !supported;
  const { applying, status } = applyState(idle, update.data?.id);
  const unsaved = dirty && !applying;

  function edit(next: Partial<IdleDraft>) {
    if (!values) return;
    update.reset();
    setErrors({});
    setDraft({ ...values, ...next });
  }

  function discard() {
    update.reset();
    setErrors({});
    setDraft(null);
  }

  function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    if (!draft || !dirty || blocker) return;
    const result = validateIdle(draft);
    setErrors(result.errors);
    if (!result.policy) {
      const form = event.currentTarget;
      requestAnimationFrame(() => form.querySelector<HTMLElement>("[aria-invalid=true]")?.focus());
      return;
    }
    update.mutate(result.policy);
  }

  return (
    <form onSubmit={submit} noValidate>
      <UnsavedChangesGuard dirty={unsaved} />
      <Section
        width="form"
        title="Idle sleep"
        description="Stops a request-driven app after a period without requests and wakes it on the next authorized request. Saved separately and applied by the worker without a deploy."
        footer={editable && policy.data && <SaveBar dirty={unsaved} pending={update.isPending} blocked={Boolean(blocker)} onDiscard={discard} status={status} />}
      >
        <div className="space-y-5">
          {!editable && <p className="text-xs text-fg-muted">Changing this needs editor access. Ask a team owner or admin.</p>}
          {(editable || policy.data?.enabled) && <NodeCapabilityNotice nodes={nodes} node={node} saved={policy.data} />}
          {editable && operations.isError && <QueryError what="active operations" error={operations.error} onRetry={() => void operations.refetch()} retrying={operations.isFetching} />}
          <IdlePolicyBody id={id} policy={policy} values={values} errors={errors} locked={locked} change={change} onEdit={edit} />
          {editable && blocker && dirty && <p className="text-xs text-fg-muted">{blocker}</p>}
          <PolicyChangeNotices change={change} dirty={dirty} error={update.error} />
          <IdleRuntimeState workload={workload} idle={idle} />
          <IdleLimits />
        </div>
      </Section>
    </form>
  );
}

function saveStatus(applying: boolean, queued?: Operation) {
  if (applying) return "Queued. The saved policy changes only after the worker applies it.";
  return queued?.state === "succeeded" ? "Applied by the worker." : undefined;
}

type PolicyQuery = IdleState["policy"];

function IdlePolicyBody({ id, policy, values, errors, locked, change, onEdit }: { id: string; policy: PolicyQuery; values?: IdleDraft; errors: IdleErrors; locked: boolean; change?: Operation; onEdit: (next: Partial<IdleDraft>) => void }) {
  if (policy.isPending) {
    return (
      <p className="flex items-center gap-2 text-[13px] text-fg-muted">
        <Spinner />
        Loading the idle sleep policy
      </p>
    );
  }
  if (policy.isError) return <QueryError what="the idle sleep policy" error={policy.error} onRetry={() => void policy.refetch()} retrying={policy.isFetching} />;
  if (!values) return null;
  return (
    <SettingRows>
      <SwitchField
        layout="row"
        id={`${id}-enabled`}
        checked={values.enabled}
        disabled={locked}
        onChange={(event) => onEdit({ enabled: event.target.checked })}
        label="Sleep when idle"
        description="Stops every container after the idle timeout with no active requests. HTTP requests, server-sent event streams and WebSockets count as active for their whole lifetime."
      />
      <Field
        layout="row"
        id={`${id}-after`}
        label="Idle timeout"
        hint={`Seconds without requests before the app sleeps, from ${idleLimits.minSeconds} to ${idleLimits.maxSeconds.toLocaleString("en-US")}. The worker checks about every 15 seconds.`}
        error={errors.after}
      >
        <input
          id={`${id}-after`}
          className={inputClass}
          inputMode="numeric"
          value={values.after}
          disabled={locked}
          aria-invalid={errors.after ? true : undefined}
          aria-describedby={describedBy(`${id}-after`, errors.after, true)}
          onChange={(event) => onEdit({ after: event.target.value })}
        />
      </Field>
      <RequestDrivenCheckbox id={`${id}-request-driven`} checked={values.requestDriven} disabled={locked} error={errors.requestDriven} onChange={(requestDriven) => onEdit({ requestDriven })} />
      <SettingRow label="Saved policy" description="What the panel has recorded. It changes only after the worker applies a change.">
        <span className="text-fg">{policy.data.enabled ? `On, after ${formatSeconds(policy.data.after_seconds)} idle` : "Off"}</span>
      </SettingRow>
      <PolicyChangeRow change={change} />
    </SettingRows>
  );
}

function PolicyChangeNotices({ change, dirty, error }: { change?: Operation; dirty: boolean; error: unknown }) {
  return (
    <>
      {(change?.state === "failed" || change?.state === "interrupted") && (
        <Notice tone="warn" title="The last policy change was not applied">
          <span className="block break-words">{change.result?.error || "Open the operation for its log."}</span>
          The saved policy is unchanged.{dirty ? " Your changes are kept. Fix the cause, then save again." : ""}
        </Notice>
      )}
      {error !== null && (
        <Notice tone={isStatus(error, 409) ? "warn" : "error"} title="Could not queue the policy change">
          {errorMessage(error)}
        </Notice>
      )}
    </>
  );
}

function RequestDrivenCheckbox({ id, checked, disabled, error, onChange }: { id: string; checked: boolean; disabled: boolean; error?: string; onChange: (checked: boolean) => void }) {
  return (
    <div className="space-y-1.5">
      <div className="flex items-start gap-3">
        <Checkbox id={id} checked={checked} disabled={disabled} aria-invalid={error ? true : undefined} aria-describedby={error ? `${id}-error` : `${id}-hint`} onChange={(event) => onChange(event.target.checked)} />
        <div className="min-w-0">
          <label htmlFor={id} className={`block text-[13px] font-medium ${disabled ? "text-fg-muted" : "text-fg"}`}>
            Every service is a request-driven HTTP app
          </label>
          <p id={`${id}-hint`} className="mt-0.5 text-xs text-fg-muted">
            Required to turn idle sleep on. Every service must be served through a managed HTTP route and do nothing between requests: no databases, queues, schedulers or background
            work, no fixed host ports, and no private connections or shared networks. Each service needs a Docker healthcheck. Completed init containers stay completed.
          </p>
        </div>
      </div>
      {error && (
        <p id={`${id}-error`} className="text-xs text-bad">
          {error}
        </p>
      )}
    </div>
  );
}

function PolicyChangeRow({ change }: { change?: Operation }) {
  return (
    <SettingRow label="Last policy change">
      {change ? (
        <>
          <OperationStateLabel state={change.state} />
          <RelativeTime value={change.created_at} className="text-fg-muted" />
          <Link to="/operations/$operationId" params={{ operationId: change.id }} className={linkClass}>
            View operation
          </Link>
        </>
      ) : (
        <span className="text-fg-muted">None in recent history</span>
      )}
    </SettingRow>
  );
}

function IdleRuntimeState({ workload, idle }: { workload: Workload; idle: IdleState }) {
  const { runtime } = idle;
  return (
    <div className="space-y-3 border-t border-line pt-4">
      <h3 className="text-[13px] font-semibold text-fg">Current state on the worker</h3>
      {runtime.isPending ? (
        <p className="flex items-center gap-2 text-[13px] text-fg-muted">
          <Spinner />
          Reading the idle state from the worker
        </p>
      ) : runtime.isError ? (
        <>
          <StatusLabel tone="neutral" label="Unknown" />
          <QueryError what="the idle state" error={runtime.error} onRetry={() => void runtime.refetch()} retrying={runtime.isFetching} />
        </>
      ) : runtime.data.idle ? (
        <IdleRuntimeRows workload={workload} idle={idle} state={runtime.data.idle} />
      ) : (
        <p className="text-[13px] text-fg-muted">The worker has no idle policy for this resource, so it stays awake.</p>
      )}
    </div>
  );
}

function IdleRuntimeRows({ workload, idle, state }: { workload: Workload; idle: IdleState; state: IdleRuntime }) {
  const phase = idlePhases[idlePhase(state.phase)];
  return (
    <>
      <SettingRows>
        <SettingRow label="State" description={phase.detail}>
          <StatusLabel tone={phase.tone} label={phase.label} pulse={phase.tone === "info"} />
          <WakeNow workload={workload} idle={idle} size="sm" retryOperations={!can(workload.permissions, "edit")} />
        </SettingRow>
        <SettingRow label="Active requests">
          <span className="text-fg tabular-nums">{countLabel(state.active_requests, "request")}</span>
        </SettingRow>
        <SettingRow label="Idle timer reset" description="A finished request, or a start, restart or wake, resets the timer.">
          {state.last_activity ? <RelativeTime value={state.last_activity} className="text-fg-muted" /> : <span className="text-fg-muted">Not reported</span>}
        </SettingRow>
        {state.last_sleep && (
          <SettingRow label="Last sleep">
            <RelativeTime value={state.last_sleep} className="text-fg-muted" />
          </SettingRow>
        )}
        {state.last_wake && (
          <SettingRow label="Last wake">
            <RelativeTime value={state.last_wake} className="text-fg-muted" />
          </SettingRow>
        )}
      </SettingRows>
      {state.error && (
        <Notice tone="warn" title="The worker reported a problem">
          <span className="break-words">{state.error}</span>
        </Notice>
      )}
    </>
  );
}

function IdleLimits() {
  return (
    <Notice title="How idle sleep works">
      <ul className="list-disc space-y-0.5 pl-4">
        <li>The worker checks about every 15 seconds. Active HTTP requests, streams and WebSockets prevent sleep.</li>
        <li>
          Sleep stops the containers and temporarily sets their restart policy to no. Waking starts the same containers, restores their restart policies and does not rerun init
          containers. It is not a backup.
        </li>
        <li>
          The first authorized request waits up to 3 minutes while the app starts and passes its Docker healthcheck within 2 minutes. Cold starts take as long as the app needs to
          become healthy. If waking fails, requests return 503 until it is retried; each sleep gets one automatic wake.
        </li>
        <li>A manual stop or an environment pause prevents waking. Deploys, rollbacks and promotions wait until the app is awake; Start and Wake now wake it.</li>
        <li>The idle helper is capped at 128 MiB and does not depend on the panel. The worker must be online to wake an app; awake apps keep serving while the panel or worker is offline.</li>
      </ul>
    </Notice>
  );
}

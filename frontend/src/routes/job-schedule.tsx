import { formText } from "../lib/form-data";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { Play } from "lucide-react";
import { useId, useState, type FormEvent } from "react";
import { errorMessage, isStatus, request } from "../api/client";
import { useQueueOperation } from "../api/mutations";
import { useEnvironmentHold } from "../components/EnvironmentSuspension";
import { holdReason } from "../lib/suspension";
import { nodesQuery, workloadOperationsQuery } from "../api/queries";
import type { JobOverlapPolicy, JobSchedule as Schedule, JobScheduleInput, Node, Operation, Workload } from "../api/types";
import { useCapacityGate } from "../components/CapacityCheck";
import { DeployDialog } from "../components/DeployReview";
import { JobOutcome } from "../components/JobOutcome";
import { RelativeTime } from "../components/RelativeTime";
import { Select } from "../components/Select";
import { OperationStateLabel } from "../components/status";
import { Button, Field, LoadingRows, Notice, QueryError, Section } from "../components/ui";
import { formatAbsolute, formatAbsoluteInZone } from "../lib/format";
import { useOnline } from "../lib/hooks";
import {
  defaultJobTimeoutSeconds,
  formatTimeout,
  hasAdvancedExecution,
  isOverlapPolicy,
  maxJobTimeoutSeconds,
  minJobTimeoutSeconds,
  overlapPolicies,
  parseJobTimeout,
} from "../lib/jobs";
import { activeOperation, actionLabels, stateLabels, workerResult } from "../lib/operations";
import { can } from "../lib/permissions";
import { buttonClass, inputClass, linkClass } from "../lib/styles";
import { displayTimeZoneLabel, useDisplayTimeZone } from "../lib/timezone";
import { useResource } from "./workload-detail";

function NextRun({ schedule }: { schedule: Schedule }) {
  const displayZone = useDisplayTimeZone();
  if (!schedule.next_run_at) return <p className="text-[13px] text-fg-muted">Scheduling is disabled.</p>;
  const zoned = formatAbsoluteInZone(schedule.next_run_at, schedule.timezone);
  return (
    <p className="text-[13px] text-fg-muted">
      Next run: <time dateTime={schedule.next_run_at} className="text-fg">{zoned ?? `${formatAbsolute(schedule.next_run_at, displayZone)} in the display time zone`}</time> ({schedule.timezone}), <RelativeTime value={schedule.next_run_at} />
      {zoned && displayZone !== schedule.timezone && <span className="block text-xs">{formatAbsolute(schedule.next_run_at, displayZone)} in the display time zone ({displayTimeZoneLabel(displayZone)}).</span>}
    </p>
  );
}

function PendingRun({ since }: { since: string }) {
  return (
    <Notice title="A scheduled run is waiting">
      Waiting since <time dateTime={since}>{formatAbsolute(since)}</time> (<RelativeTime value={since} />) for the current operation to finish. Only one run waits; missed ticks are not replayed.
    </Notice>
  );
}

function useJobRuns(workload: Pick<Workload, "node_id">) {
  const nodes = useQuery(nodesQuery);
  const node = nodes.data?.find((item) => item.id === workload.node_id);
  return { nodes, node, supported: node?.capabilities?.job_runs === true };
}

function UpgradeNotice({ node, schedule, save }: { node: Node; schedule: Schedule; save: ReturnType<typeof useSaveSchedule> }) {
  const online = useOnline();
  const savedTimeout = schedule.timeout_seconds !== defaultJobTimeoutSeconds;
  const saved = hasAdvancedExecution(schedule.overlap_policy, schedule.timeout_seconds);
  return (
    <Notice
      tone="warn"
      title="Worker upgrade needed"
      action={
        saved && (
          <Button
            disabled={!online}
            pending={save.isPending}
            onClick={() => save.mutate({ cron: schedule.cron, timezone: schedule.timezone, overlap_policy: "skip", timeout_seconds: defaultJobTimeoutSeconds })}
          >
            Reset to defaults
          </Button>
        )
      }
    >
      {node.name} does not report job execution support, so overlap and timeout can only use Skip and {formatTimeout(defaultJobTimeoutSeconds)}. Upgrade the worker, then verify it from the{" "}
      <Link to="/nodes/$nodeId" params={{ nodeId: node.id }} className={linkClass}>
        node page
      </Link>
      .{" "}
      {saved
        ? `This job has saved execution settings the worker cannot apply, so schedule changes${savedTimeout ? " and runs" : ""} are rejected. Reset to defaults keeps the cron expression and time zone.`
        : "Cron, time zone and Run now still work."}
    </Notice>
  );
}

interface FormErrors {
  timezone?: string;
  timeout?: string;
}

function describedBy(id: string, error: string | undefined) {
  return error ? `${id}-hint ${id}-error` : `${id}-hint`;
}

function useSaveSchedule(id: string) {
  const client = useQueryClient();
  return useMutation({
    mutationFn: (input: JobScheduleInput) => request<Schedule>("PUT", `/api/workloads/${id}/schedule`, input),
    onSuccess: (data) => client.setQueryData(["job-schedule", id], data),
  });
}

function ScheduleForm({ resource, schedule, supported, save }: { resource: Workload; schedule: Schedule; supported: boolean; save: ReturnType<typeof useSaveSchedule> }) {
  const online = useOnline();
  const editable = can(resource.permissions, "edit");
  const advancedEditable = editable && supported;
  const [errors, setErrors] = useState<FormErrors>({});
  const statusId = useId();

  function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    const form = new FormData(event.currentTarget);
    const timezone = formText(form, "timezone").trim();
    const next: FormErrors = {};
    const input: JobScheduleInput = { cron: formText(form, "cron").trim(), timezone };
    if (!timezone) next.timezone = "Enter a time zone, such as UTC.";
    if (advancedEditable) {
      const overlap = formText(form, "overlap_policy");
      if (isOverlapPolicy(overlap)) input.overlap_policy = overlap;
      const timeout = parseJobTimeout(formText(form, "timeout_seconds"));
      if (timeout.error) next.timeout = timeout.error;
      else input.timeout_seconds = timeout.value;
    }
    setErrors(next);
    if (next.timezone || next.timeout) return;
    save.mutate(input);
  }

  const status = !editable ? "Changing the schedule needs edit access." : !online ? "You are offline. Save is available when the connection returns." : null;

  return (
    <form onSubmit={submit} noValidate className="space-y-4">
      <div className="grid gap-4 sm:grid-cols-2">
        <Field id="job-cron" label="Cron expression" hint="Five numeric fields: minute hour day month weekday (Sunday = 0). Supports *, lists, ranges, and steps. Example: 0 3 * * * runs at 03:00 in the schedule's time zone. Leave empty to disable.">
          <input id="job-cron" name="cron" defaultValue={schedule.cron} className={`${inputClass} font-mono`} maxLength={256} spellCheck={false} autoComplete="off" aria-describedby="job-cron-hint" disabled={!editable} />
        </Field>
        <Field id="job-timezone" label="Time zone" error={errors.timezone} hint="IANA time zone name, such as America/Los_Angeles. Jobs use UTC unless you change it. A local time skipped by a daylight saving change does not run, and a repeated one can run twice.">
          <input id="job-timezone" name="timezone" defaultValue={schedule.timezone} className={`${inputClass} font-mono`} maxLength={128} spellCheck={false} autoComplete="off" aria-invalid={errors.timezone ? true : undefined} aria-describedby={describedBy("job-timezone", errors.timezone)} disabled={!editable} />
        </Field>
        <Field id="job-overlap" label="When a run overlaps" hint="Applies to scheduled ticks. Run now and deploys are rejected while another operation is active.">
          <Select id="job-overlap" name="overlap_policy" defaultValue={schedule.overlap_policy} aria-describedby="job-overlap-hint job-overlap-options" disabled={!advancedEditable}>
            {(Object.keys(overlapPolicies) as JobOverlapPolicy[]).map((policy) => (
              <option key={policy} value={policy}>
                {overlapPolicies[policy].label}
              </option>
            ))}
          </Select>
        </Field>
        <Field id="job-timeout" label="Timeout (seconds)" error={errors.timeout} hint={`${minJobTimeoutSeconds} to ${maxJobTimeoutSeconds.toLocaleString("en-US")} (24 hours). Covers Compose startup and the run; checkout and builds have a separate 30-minute limit. Default ${defaultJobTimeoutSeconds.toLocaleString("en-US")} (${formatTimeout(defaultJobTimeoutSeconds)}).`}>
          <input
            id="job-timeout"
            name="timeout_seconds"
            type="number"
            inputMode="numeric"
            min={minJobTimeoutSeconds}
            max={maxJobTimeoutSeconds}
            step={1}
            defaultValue={schedule.timeout_seconds}
            className={`${inputClass} tabular-nums`}
            aria-invalid={errors.timeout ? true : undefined}
            aria-describedby={describedBy("job-timeout", errors.timeout)}
            disabled={!advancedEditable}
          />
        </Field>
      </div>
      <dl id="job-overlap-options" className="grid gap-x-4 gap-y-1 text-xs sm:grid-cols-[6rem_minmax(0,1fr)]">
        {(Object.keys(overlapPolicies) as JobOverlapPolicy[]).map((policy) => (
          <div key={policy} className="contents">
            <dt className="font-medium text-fg">{overlapPolicies[policy].label}</dt>
            <dd className="mb-1 text-fg-muted sm:mb-0">{overlapPolicies[policy].description}</dd>
          </div>
        ))}
      </dl>
      <p className="text-xs text-fg-muted">A timed-out or cancelled run keeps this job locked until all of its containers have stopped. Failed runs send the notifications configured for deployment failures.</p>
      <NextRun schedule={schedule} />
      {save.isError && (
        <Notice tone={isStatus(save.error, 409) ? "warn" : "error"} title="Could not save the schedule">
          {errorMessage(save.error)}
        </Notice>
      )}
      {save.isSuccess && <Notice tone="success" title="Schedule saved" />}
      <div className="flex flex-wrap items-center justify-end gap-3">
        {status && (
          <p id={statusId} className="text-xs text-fg-muted">
            {status}
          </p>
        )}
        {editable && (
          <Button type="submit" variant="primary" pending={save.isPending} disabled={!online} aria-describedby={status ? statusId : undefined}>
            Save schedule
          </Button>
        )}
      </div>
    </form>
  );
}

type LoadState = "loading" | "error" | "ready";

function loadState(query: { isPending: boolean; isError: boolean }): LoadState {
  return query.isError ? "error" : query.isPending ? "loading" : "ready";
}

interface RunState {
  online: boolean;
  operations: LoadState;
  active: Operation | undefined;
  nodes: LoadState;
  node: Node | undefined;
  needsUpgrade: boolean;
}

// Unknown operation or node state blocks the run: it may be locked or unable to reach the worker.
function runBlocker({ online, operations, active, nodes, node, needsUpgrade }: RunState): string | null {
  if (!online) return "You are offline. Run now is available when the connection returns.";
  if (operations === "error") return "Cove cannot tell whether another operation is running. Run now is available once operations load.";
  if (operations === "loading") return "Checking for active operations.";
  if (active) return `${active.action === "deploy" ? "Run" : actionLabels[active.action]} ${stateLabels[active.state].toLowerCase()}. Run now is available when it finishes.`;
  if (nodes === "error") return "Cove cannot read the node. Run now is available once the node loads.";
  if (nodes === "loading") return "Checking the node.";
  if (!node) return "Cove cannot find this job's node, so it cannot confirm the node is ready.";
  if (node.state !== "ready") return `${node.name} is not ready. Run now is available once the node is ready.`;
  if (needsUpgrade) return `${node.name} does not report job execution support, so it cannot apply the saved timeout. Upgrade and verify the worker, or reset the execution settings to their defaults.`;
  return null;
}

function QueueFeedback({ queue }: { queue: ReturnType<typeof useQueueOperation> }) {
  if (queue.isSuccess) {
    return (
      <Notice
        tone="success"
        title="Run queued"
        action={
          <Link to="/operations/$operationId" params={{ operationId: queue.data.id }} className={buttonClass("ghost")}>
            View operation
          </Link>
        }
      />
    );
  }
  if (queue.isError) {
    return (
      <Notice tone={isStatus(queue.error, 409) ? "warn" : "error"} title="Could not queue the run">
        {errorMessage(queue.error)}
      </Notice>
    );
  }
  return null;
}

function LatestRun({ operation }: { operation: Operation | undefined }) {
  if (!operation) return <p className="text-[13px] text-fg-muted">This job has not run yet.</p>;
  const job = workerResult(operation)?.job;
  return (
    <div className="space-y-1.5">
      <div className="flex flex-wrap items-center gap-x-4 gap-y-1 text-[13px]">
        <Link to="/operations/$operationId" params={{ operationId: operation.id }} className={linkClass}>
          Latest run
        </Link>
        <OperationStateLabel state={operation.state} />
        <RelativeTime value={operation.created_at} className="text-fg-muted" />
      </div>
      {job && <JobOutcome job={job} />}
    </div>
  );
}

function confirmText(resource: Workload, schedule: Schedule | undefined, supported: boolean) {
  const stopped = resource.manually_stopped ? "This job was manually stopped. Running it now also resumes its schedule. " : "";
  const timeout = schedule && supported ? `The run is stopped if it is still going after ${formatTimeout(schedule.timeout_seconds)}. ` : "";
  return `${stopped}${timeout}Deployment approval, freeze and window rules apply.`;
}

function RunNow({ resource, schedule, node, nodes, supported }: { resource: Workload; schedule: Schedule | undefined; node: Node | undefined; nodes: LoadState; supported: boolean }) {
  const operations = useQuery(workloadOperationsQuery(resource.id));
  const queue = useQueueOperation(resource.id);
  const online = useOnline();
  const [confirm, setConfirm] = useState(false);
  // A run is a manual deploy of the job: the capacity check runs first, then the configuration review asks for a reason.
  const capacity = useCapacityGate({ workloadId: resource.id, actionLabel: "Run now", onDeploy: () => setConfirm(true) });
  const statusId = useId();
  const canOperate = can(resource.permissions, "operate");
  const active = operations.data ? activeOperation(operations.data, resource.id) : undefined;
  const { hold } = useEnvironmentHold(resource);
  const reason = holdReason(hold, "deploy", "Running the job") ?? runBlocker({
    online,
    operations: loadState(operations),
    active,
    nodes,
    node,
    needsUpgrade: Boolean(schedule && schedule.timeout_seconds !== defaultJobTimeoutSeconds && !supported),
  });
  const blocked = Boolean(reason) || queue.isPending || capacity.checking;

  return (
    <Section
      width="form"
      title="Run now"
      description="Start one run immediately. The schedule is unchanged and the run appears in Operations."
      actions={
        canOperate && (
          <Button disabled={blocked} pending={queue.isPending || capacity.checking} aria-describedby={reason ? statusId : undefined} onClick={capacity.start}>
            {!queue.isPending && !capacity.checking && <Play aria-hidden className="size-3.5" />}
            Run now
          </Button>
        )
      }
    >
      <div className="space-y-3" aria-live="polite">
        {!canOperate && <p className="text-xs text-fg-muted">Running the job needs operator access.</p>}
        {canOperate && reason && (
          <p id={statusId} className="text-xs text-fg-muted">
            {reason}
            {active && (
              <>
                {" "}
                <Link to="/operations/$operationId" params={{ operationId: active.id }} className={linkClass}>
                  View operation
                </Link>
              </>
            )}
          </p>
        )}
        {operations.isError && <QueryError what="operations" error={operations.error} onRetry={() => void operations.refetch()} retrying={operations.isFetching} />}
        <QueueFeedback queue={queue} />
        {operations.data && <LatestRun operation={operations.data.find((operation) => operation.workload_id === resource.id && operation.action === "deploy")} />}
      </div>
      {capacity.dialog}
      <DeployDialog
        open={confirm}
        title={`Run ${resource.name} now?`}
        confirmLabel="Run now"
        workloadId={resource.id}
        pending={queue.isPending}
        onCancel={() => setConfirm(false)}
        onDeploy={(runReason) => queue.mutate({ action: "deploy", reason: runReason }, { onSettled: () => setConfirm(false) })}
      >
        {confirmText(resource, schedule, supported)}
      </DeployDialog>
    </Section>
  );
}

export function JobSchedule() {
  const resource = useResource();
  const schedule = useQuery({
    queryKey: ["job-schedule", resource.id],
    queryFn: ({ signal }) => request<Schedule>("GET", `/api/workloads/${resource.id}/schedule`, undefined, signal),
    refetchInterval: 30_000,
  });
  const { nodes, node, supported } = useJobRuns(resource);
  // Kept outside the form, which remounts with the saved values after a successful save.
  const save = useSaveSchedule(resource.id);
  const editable = can(resource.permissions, "edit");
  const data = schedule.data;
  const formKey = data ? `${resource.id}:${data.cron}:${data.timezone}:${data.overlap_policy}:${data.timeout_seconds}` : resource.id;
  const isJob = resource.kind === "job";

  return (
    <>
      <Section width="form" title="Scheduled job" description="Run this one-shot Compose job on a cron schedule in the time zone you choose. Each run appears in Operations. Ticks missed during a panel outage are not replayed.">
        <div className="space-y-4">
          {!isJob ? (
            <Notice>Create a one-shot job resource to schedule a command. Existing services are not restarted by this scheduler.</Notice>
          ) : (
            <>
              {resource.manually_stopped && <Notice tone="warn">This job was manually stopped. Scheduled runs are skipped until you run or deploy it again.</Notice>}
              {schedule.isError && <QueryError what="the schedule" error={schedule.error} onRetry={() => void schedule.refetch()} retrying={schedule.isFetching} />}
              {schedule.isPending && <LoadingRows label="Loading schedule" rows={2} />}
              {editable && nodes.isError && <QueryError what="worker capabilities" error={nodes.error} onRetry={() => void nodes.refetch()} retrying={nodes.isFetching} />}
              {data && editable && node && !supported && <UpgradeNotice node={node} schedule={data} save={save} />}
              {data?.pending_since && <PendingRun since={data.pending_since} />}
              {data && <ScheduleForm key={formKey} resource={resource} schedule={data} supported={supported} save={save} />}
            </>
          )}
        </div>
      </Section>
      {isJob && <RunNow resource={resource} schedule={data} node={node} nodes={loadState(nodes)} supported={supported} />}
    </>
  );
}

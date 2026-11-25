import { Activity } from "lucide-react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useState } from "react";
import { useCancelOperation, useEmergencyOverride, useRollback } from "../api/mutations";
import { errorMessage, isStatus, request } from "../api/client";
import type { Node, Operation } from "../api/types";
import { ReasonField } from "../components/ReasonField";
import { ConfirmDialog, Dialog } from "../components/ConfirmDialog";
import { useOnline } from "../lib/hooks";
import { getRouteApi, Link, useNavigate } from "@tanstack/react-router";
import { nodesQuery, operationQuery, workloadsQuery } from "../api/queries";
import { projectsQuery } from "../api/projects";
import { JobOutcome } from "../components/JobOutcome";
import { LogViewer } from "../components/LogViewer";
import { OperationResultNotice } from "../components/OperationResultNotice";
import { OperationStateLabel } from "../components/status";
import { Breadcrumbs, Button, DefinitionList, EmptyState, Field, LoadingRows, Notice, PageHeader, QueryError, Section } from "../components/ui";
import { shortId } from "../lib/format";
import { formatAbsolute, formatDuration } from "../lib/format";
import { actionLabels, durationMs, isActive, isSuspensionAction, overrideEligible, reasonError, workerResult } from "../lib/operations";
import { can } from "../lib/permissions";
import { findPlacement } from "../lib/hierarchy";
import { resultSummary } from "../lib/rollouts";
import { buttonClass, inputClass, linkClass } from "../lib/styles";
import { recordedEngine, recordedPhaseMemory, recordedPlannerChecksum } from "../lib/build-recipes";
import { PendingRolloutMessage, usePendingRollout } from "../components/StagedRollout";
import type { ManagedRegistryResult, SuspensionResult, WorkerOperation, WorkloadSummary } from "../api/types";
import { useEnvironmentHold } from "../components/EnvironmentSuspension";
import { holdReason } from "../lib/suspension";
import { useIdleState } from "../components/IdleSleep";
import { idleReason } from "../lib/idle";
import { ScaleOutcome } from "../components/CpuAutoscale";
import { ScanOutcome, ScanSource } from "../components/ImageScans";

const route = getRouteApi("/app/operations/$operationId");

function RollbackRelease({ workload, release }: { workload: WorkloadSummary; release: string }) {
  const workloadId = workload.id;
  const pendingRollout = usePendingRollout(workload);
  const idle = useIdleState(workload);
  const held = holdReason(useEnvironmentHold(workload).hold, "rollback", "Rolling back") ?? idleReason(idle.hold, "rollback", "Rolling back");
  const retained = useQuery({ queryKey: ["releases", workloadId], queryFn: ({ signal }) => request<{ releases: { id: string }[] }>("GET", `/api/workloads/${workloadId}/releases`, undefined, signal) });
  const rollback = useRollback(workloadId);
  const [confirm, setConfirm] = useState(false);
  const [reason, setReason] = useState("");
  const online = useOnline();
  const navigate = useNavigate();
  return <>
    <Button disabled={!online || rollback.isPending || Boolean(pendingRollout) || Boolean(held) || retained.isError || !retained.data?.releases.some((item) => item.id === release)} onClick={() => setConfirm(true)}>Roll back to this release</Button>
    {pendingRollout && <PendingRolloutMessage workloadId={workloadId} action="roll back" />}
    {held && <p className="text-xs text-fg-muted">{held}</p>}
    {retained.isError && <Notice tone="error">Could not verify whether this release is retained.</Notice>}
    {retained.data && !retained.data.releases.some((item) => item.id === release) && <p className="text-xs text-fg-muted">This release is no longer retained on the worker.</p>}
    {rollback.isError && <Notice tone="error" title="Could not queue rollback">{errorMessage(rollback.error)}</Notice>}
    <ConfirmDialog open={confirm} title="Roll back to this release?" confirmLabel="Roll back" danger pending={rollback.isPending} onCancel={() => setConfirm(false)} onConfirm={() => {
      if (reasonError(reason)) return;
      rollback.mutate({ release, reason }, { onSettled: () => setConfirm(false), onSuccess: ({ id }) => void navigate({ to: "/operations/$operationId", params: { operationId: id } }) });
    }}>
      <p>This restores the saved Compose configuration and local images. It does not restore database contents or volumes, undo migrations, or edit your saved resource configuration. The worker must still have this release and its images.</p>
      <div className="mt-4"><ReasonField id="rollback-reason" value={reason} onChange={setReason} /></div>
    </ConfirmDialog>
  </>;
}

// Team owners and admins can let one queued deploy or rollback skip the schedule and approval gates. Quotas,
// dependencies, locks, permissions and cancellation still apply.
function EmergencyOverride({ operation }: { operation: Operation }) {
  const override = useEmergencyOverride(operation.id);
  const online = useOnline();
  const [open, setOpen] = useState(false);
  const [reason, setReason] = useState("");
  const [attempted, setAttempted] = useState(false);
  const error = reasonError(reason, true);
  const shown = attempted ? error : reason ? reasonError(reason) : undefined;
  const close = () => { setOpen(false); setAttempted(false); };
  const ineligible = isStatus(override.error, 409);
  return <>
    <Button variant="danger" disabled={!online || override.isPending} onClick={() => { override.reset(); setOpen(true); }}>Emergency override</Button>
    <Dialog open={open} title="Override the deployment schedule?" onClose={close}>
      <form className="mt-3 space-y-4" noValidate onSubmit={(event) => {
        event.preventDefault();
        setAttempted(true);
        if (error) return;
        override.mutate(reason, { onSuccess: close });
      }}>
        <p>This lets this {(actionLabels[operation.action] ?? operation.action).toLowerCase()} start without waiting for deployment windows, freezes or approval. Quotas, dependencies, locks and cancellation still apply. Your account, the time and the reason are recorded in the audit log.</p>
        <Field id="override-reason" label="Emergency reason" hint="Required. Visible to anyone who can read this operation; do not include secrets." error={shown}>
          <textarea id="override-reason" className={inputClass} rows={3} value={reason} required aria-invalid={shown ? true : undefined} aria-describedby={shown ? "override-reason-error" : "override-reason-hint"} onChange={(event) => setReason(event.target.value)} />
        </Field>
        {override.isError && <Notice tone="error" title="Could not override">{ineligible ? "This operation is no longer eligible: it may have started, been cancelled or already been overridden." : errorMessage(override.error)}</Notice>}
        <div className="flex justify-end gap-2 border-t border-line pt-3">
          <Button variant="ghost" onClick={close} disabled={override.isPending}>Cancel</Button>
          <Button type="submit" variant="danger" pending={override.isPending}>Override and allow start</Button>
        </div>
      </form>
    </Dialog>
  </>;
}

const suspensionPhases: Record<SuspensionResult["phase"], string> = {
  pausing: "Pausing",
  paused: "Paused",
  resuming: "Resuming",
  resumed: "Resumed",
  stopped: "Left stopped",
};

const idleSuspensionPhases: Record<SuspensionResult["phase"], string> = {
  pausing: "Going to sleep",
  paused: "Asleep",
  resuming: "Waking",
  resumed: "Woke the sleeping app",
  stopped: "Left stopped",
};

// The node recorded for this build. It stays the same when the resource's build node changes later, and older control planes omit it.
function BuildNodeValue({ id, name }: { id?: string; name?: string }) {
  if (!id) return <span className="text-fg-muted">Not recorded</span>;
  if (!name) return <span className="font-mono text-xs break-all">{id}</span>;
  return (
    <Link to="/nodes/$nodeId" params={{ nodeId: id }} className={linkClass}>
      {name}
    </Link>
  );
}

const nodeTerm = (operation: Operation) => (operation.action === "build" ? "Runtime node" : "Node");

function buildNodeRows(operation: Operation, nodes: Node[] | undefined) {
  if (operation.action !== "build") return [];
  return [
    { term: "Build node", value: <BuildNodeValue id={operation.execution_node_id} name={nodes?.find((node) => node.id === operation.execution_node_id)?.name} /> },
    { term: "Build engine", value: <BuildEngineValue operation={operation} /> },
  ];
}

// The engine recorded by this build, never the current setting. Command text is not part of the record.
function BuildEngineValue({ operation }: { operation: Operation }) {
  const recipe = workerResult(operation)?.build_recipe;
  if (!recipe?.engine) return <span className="text-fg-muted">{recordedEngine(operation)}</span>;
  const memory = recordedPhaseMemory(operation);
  const checksum = recordedPlannerChecksum(operation);
  return (
    <div className="space-y-1">
      <p className="break-words">{recordedEngine(operation)}</p>
      {memory && <p className="text-xs break-words text-fg-muted">{memory}</p>}
      {checksum && <p className="text-xs break-all text-fg-muted">{checksum}</p>}
      {recipe.static_runtime && <p className="font-mono text-xs break-all text-fg-muted">{recipe.static_runtime}</p>}
      {recipe.pinned_images && recipe.pinned_images.length > 0 && (
        <ul aria-label="Pinned build images" className="space-y-0.5">
          {recipe.pinned_images.map((image) => (
            <li key={image} className="font-mono text-xs break-all text-fg-muted">
              {image}
            </li>
          ))}
        </ul>
      )}
    </div>
  );
}

const suspensionTerms: Record<SuspensionResult["reason"], string> = { idle: "Idle sleep", environment: "Environment pause", manual_stop: "Manual stop" };

// A manual Start can wake an app that idle sleep stopped, so its result is not an environment resume.
function SuspensionOutcome({ suspension }: { suspension: SuspensionResult }) {
  const idle = suspension.reason === "idle";
  const labels = idle ? idleSuspensionPhases : suspensionPhases;
  const before = idle ? "before it went to sleep" : suspension.reason === "manual_stop" ? "before the stop" : "before the pause";
  return (
    <div className="space-y-1">
      <p>{labels[suspension.phase] ?? suspension.phase}</p>
      <p className="text-xs text-fg-muted">
        {suspension.captured} {suspension.captured === 1 ? "container" : "containers"} captured, {suspension.previously_running} running {before}.
        {suspension.phase === "stopped" && (idle ? " It was stopped manually while asleep, so it was left stopped." : " It was stopped manually while paused, so the resume left it stopped.")}
      </p>
    </div>
  );
}

function imageScanRows(operation: Operation, worker: WorkerOperation | null, active: boolean, workloadId: string | undefined) {
  const source = operation.action === "image_scan" && worker ? [{ term: "Scan source", value: <ScanSource origin={worker.scan_origin} /> }] : [];
  if (worker?.image_scan) return [...source, { term: "Vulnerability scan", value: <ScanOutcome scan={worker.image_scan} workloadId={workloadId} /> }];
  if (operation.action === "image_scan" && worker && !active) return [...source, { term: "Vulnerability scan", value: <span className="text-fg-muted">No report was recorded.</span> }];
  return source;
}

const plural = (count: number, word: string) => `${count} ${count === 1 ? word : `${word}s`}`;

// Manifest counts only: reclaimed space is not measured, and a removal reports whether the data volume was kept.
function RegistryMaintenanceOutcome({ result }: { result: ManagedRegistryResult }) {
  return (
    <div className="space-y-1">
      <p>{result.removed ? (result.data_retained ? "Registry removed; its data volume was kept on the node" : "Registry removed; no data volume was kept") : `${plural(result.deleted, "manifest")} deleted of ${plural(result.manifests, "manifest")} found`}</p>
      {result.storage?.hard_cap_verified !== true && <p className="text-xs text-fg-muted">The registry storage has no verified hard cap.</p>}
    </div>
  );
}

function registryMaintenanceRows(worker: WorkerOperation | null) {
  return worker?.managed_registry ? [{ term: "Registry maintenance", value: <RegistryMaintenanceOutcome result={worker.managed_registry} /> }] : [];
}

const stateDescriptions = {
  cancelled: "Cancellation was acknowledged by the worker. Inspect the resource: changes already made to containers are not undone.",
  queued: "Waiting for the control plane to hand the operation to the worker.",
  running: "The worker is running the operation. It continues even if the SSH connection drops.",
  succeeded: "The operation finished successfully.",
  failed: "The operation finished with an error.",
  interrupted: "The worker stopped before the operation finished, for example after a restart. Review the logs and run the action again if needed.",
} as const;

export function OperationPage() {
  const { operationId } = route.useParams();
  const client = useQueryClient();
  const approve = useMutation({ mutationFn: () => request("POST", `/api/operations/${operationId}/approve`, {}), onSuccess: () => client.invalidateQueries({ queryKey: operationQuery(operationId).queryKey }) });
  const cancel = useCancelOperation(operationId);
  const [confirmCancel, setConfirmCancel] = useState(false);
  const online = useOnline();
  const operations = useQuery(operationQuery(operationId));
  const workloads = useQuery(workloadsQuery);
  const nodes = useQuery({ ...nodesQuery, enabled: operations.data?.action === "build" });
  const projects = useQuery(projectsQuery);
  const back = <Breadcrumbs><Link to="/operations" className="rounded hover:text-fg">Operations</Link>{shortId(operationId)}</Breadcrumbs>;

  if (operations.isPending) return <LoadingRows label="Loading operation" />;
  if (operations.isError && !operations.data) {
    return (
      <>
        <PageHeader icon={<Activity aria-hidden />} breadcrumbs={back} title="Operation" />
        <div className="px-4 py-5 sm:px-6">
          <QueryError what="the operation" error={operations.error} onRetry={() => void operations.refetch()} retrying={operations.isFetching} />
        </div>
      </>
    );
  }

  const operation = operations.data;
  if (!operation) {
    return (
      <>
        <PageHeader icon={<Activity aria-hidden />} breadcrumbs={back} title="Operation not found" />
        <EmptyState title="This operation could not be found">
          Check the operation link and try again.
        </EmptyState>
      </>
    );
  }

  const workload = workloads.data?.find((item) => item.id === operation.workload_id);
  const placement = workload ? findPlacement(projects.data, workload.environment_id, workload.project_id) : undefined;
  const worker = workerResult(operation);
  const duration = durationMs(operation);
  const active = isActive(operation.state);

  return (
    <>
      <PageHeader
        icon={<Activity aria-hidden />}
        breadcrumbs={
          workload ? (
            <Breadcrumbs>
              {placement ? (
                <Link key="projects" to="/projects" className="rounded hover:text-fg">Projects</Link>
              ) : (
                <Link key="resources" to="/resources" className="rounded hover:text-fg">All resources</Link>
              )}
              {placement && <Link key="project" to="/projects/$projectId" params={{ projectId: placement.project.id }} className="rounded hover:text-fg">{placement.project.name}</Link>}
              {placement?.environment && (
                <Link key="environment" to="/projects/$projectId/environments/$environmentId" params={{ projectId: placement.project.id, environmentId: placement.environment.id }} className="rounded hover:text-fg">
                  {placement.environment.name}
                </Link>
              )}
              <Link key="resource" to="/resources/$resourceId" params={{ resourceId: workload.id }} className="rounded hover:text-fg">{workload.name}</Link>
              <Link key="operations" to="/resources/$resourceId/operations" params={{ resourceId: workload.id }} className="rounded hover:text-fg">Operations</Link>
              {shortId(operationId)}
            </Breadcrumbs>
          ) : back
        }
        actions={
          active && can(workload?.permissions, "operate") && !isSuspensionAction(operation.action) && operation.can_cancel !== false ? (
            <Button disabled={!online || cancel.isPending || operation.cancel_requested || cancel.isSuccess} onClick={() => setConfirmCancel(true)}>
              {operation.cancel_requested || cancel.isSuccess ? "Cancellation requested" : "Cancel operation"}
            </Button>
          ) : undefined
        }
        title={`${actionLabels[operation.action] ?? operation.action} ${workload?.name ?? "resource"}`}
        description={
          <span className="flex flex-wrap items-center gap-x-4 gap-y-1">
            <OperationStateLabel state={operation.state} />
            <span className="font-mono text-xs">{operation.id}</span>
          </span>
        }
      />

      {operation.approval_required && !operation.approved_at && !operation.emergency_override_at && operation.state === "queued" && <div className="px-4 pt-4 sm:px-6"><Notice title="Approval required"><p>A team administrator must approve this deployment before it can start.</p>{operation.can_approve && <Button disabled={!online || operation.cancel_requested} pending={approve.isPending} onClick={() => approve.mutate()}>Approve deployment</Button>}{approve.isError && <p>{errorMessage(approve.error)}</p>}</Notice></div>}
      {operation.emergency_override_at && (
        <div className="px-4 pt-4 sm:px-6">
          <Notice tone="warn" title="Emergency override recorded">
            <p>
              Deployment windows, freezes and approval were skipped for this operation on {formatAbsolute(operation.emergency_override_at)}
              {operation.emergency_override_by ? <> by account <span className="font-mono text-xs break-all">{operation.emergency_override_by}</span></> : null}.
            </p>
            {operation.emergency_override_reason && <p className="mt-1 whitespace-pre-wrap break-words text-fg">{operation.emergency_override_reason}</p>}
          </Notice>
        </div>
      )}
      {overrideEligible(operation) && (
        <div className="px-4 pt-4 sm:px-6">
          <Notice title="Emergency override" action={<EmergencyOverride operation={operation} />}>
            If an outage requires this deployment now, a team owner or admin can let this operation skip deployment windows, freezes and approval.
          </Notice>
        </div>
      )}
      {isSuspensionAction(operation.action) && (
        <div className="px-4 pt-4 sm:px-6">
          <Notice title={operation.action === "suspend" ? "Queued by an environment pause" : "Queued by an environment resume"}>
            {placement?.environment ? (
              <>
                The control plane runs this operation for the resource's environment. Pause, resume or retry it from{" "}
                <Link to="/projects/$projectId/environments/$environmentId" params={{ projectId: placement.project.id, environmentId: placement.environment.id }} className={linkClass}>
                  {placement.environment.name}
                </Link>
                .
              </>
            ) : (
              "The control plane runs this operation for the resource's environment. Pause, resume or retry it from the environment page."
            )}
          </Notice>
        </div>
      )}
      {operations.isRefetchError && (
        <div className="px-4 pt-4 sm:px-6">
          <QueryError what="the latest status" error={operations.error} onRetry={() => void operations.refetch()} retrying={operations.isFetching} />
        </div>
      )}

      <Section title="Summary">
        <div className="space-y-4">
          {workload?.kind === "service" && can(workload.permissions, "operate") && operation.state === "succeeded" && (operation.action === "deploy" || operation.action === "rollback") && <RollbackRelease workload={workload} release={operation.id} />}
          {cancel.isError && <Notice tone="error" title="Could not request cancellation">{errorMessage(cancel.error)}</Notice>}
          {active && operation.can_cancel === false && <Notice title="Cannot be cancelled">Database maintenance stays locked until this same change succeeds. If it fails, retry it from the resource's Database page.</Notice>}
          {workload?.kind === "database" && (operation.state === "failed" || operation.state === "interrupted") && operation.reason?.startsWith("Database maintenance:") && <Notice tone="warn" title="Retry this maintenance" action={<Link to="/resources/$resourceId/database" params={{ resourceId: workload.id }} className={buttonClass("secondary")}>Open maintenance</Link>}>The change is retained. Retry the same task after fixing the reported problem; do not start a different change to recover.</Notice>}
          {active && operation.cancel_requested && <Notice tone="warn" title="Waiting for the worker">The request is saved and will be delivered when the worker is reachable.</Notice>}
          <p className="text-[13px] text-fg-muted">{stateDescriptions[operation.state]}</p>
          <OperationResultNotice operation={operation} />
          <DefinitionList
            items={[
              {
                term: "Resource",
                value: workload ? (
                  <Link to="/resources/$resourceId" params={{ resourceId: workload.id }} className={linkClass}>
                    {workload.name}
                  </Link>
                ) : (
                  <span className="font-mono text-xs">{operation.workload_id}</span>
                ),
              },
              ...(workload
                ? [
                    {
                      term: nodeTerm(operation),
                      value: (
                        <Link to="/nodes/$nodeId" params={{ nodeId: workload.node_id }} className={linkClass}>
                          {workload.node}
                        </Link>
                      ),
                    },
                  ]
                : []),
              ...buildNodeRows(operation, nodes.data),
              { term: "Action", value: actionLabels[operation.action] ?? operation.action },
              ...(operation.reason ? [{ term: "Reason", value: <span className="whitespace-pre-wrap break-words">{operation.reason}</span> }] : []),
              { term: "Queued", value: formatAbsolute(operation.created_at) },
              ...(worker?.finished_at ? [{ term: "Finished", value: formatAbsolute(worker.finished_at) }] : []),
              ...(duration !== null ? [{ term: "Duration", value: formatDuration(duration) }] : []),
              ...(worker?.job ? [{ term: "Job result", value: <JobOutcome job={worker.job} /> }] : []),
              ...(worker?.rollout
                ? [
                    {
                      term: "Rollout",
                      value: (
                        <div className="space-y-1">
                          <p>{resultSummary(worker.rollout)}</p>
                          {worker.rollout.reason && <p className="text-xs text-fg-muted">{worker.rollout.reason}</p>}
                          {worker.rollout.phase === "waiting" && workload && (
                            <p className="text-xs text-fg-muted">
                              The resource{" "}
                              <Link to="/resources/$resourceId" params={{ resourceId: workload.id }} className={linkClass}>
                                Overview
                              </Link>{" "}
                              shows whether it is still waiting.
                            </p>
                          )}
                        </div>
                      ),
                    },
                  ]
                : []),
              ...(worker?.scale ? [{ term: "CPU scaling", value: <ScaleOutcome scale={worker.scale} /> }] : []),
              ...imageScanRows(operation, worker, active, workload?.id),
              ...registryMaintenanceRows(worker),
              ...(worker?.suspension ? [{ term: suspensionTerms[worker.suspension.reason], value: <SuspensionOutcome suspension={worker.suspension} /> }] : []),
              ...(worker?.revision ? [{ term: "Git revision", value: <span className="font-mono text-xs break-all">{worker.revision}</span> }] : []),
              ...(!worker ? [{ term: "Worker status", value: <span className="text-fg-muted">Not yet acknowledged by the worker</span> }] : []),
            ]}
          />
        </div>
      </Section>

      <ConfirmDialog open={confirmCancel} title="Cancel this operation?" confirmLabel="Cancel operation" danger onCancel={() => setConfirmCancel(false)} onConfirm={() => { setConfirmCancel(false); cancel.mutate(); }}>
        This stops the deployment command when the worker acknowledges it. Changes already made to containers are not undone; check the resource afterward.
      </ConfirmDialog>

      <Section title="Logs">
        <LogViewer operationId={operation.id} active={active} />
      </Section>
    </>
  );
}

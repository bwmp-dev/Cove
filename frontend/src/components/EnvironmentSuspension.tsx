import { useQuery } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { Pause, Play, RotateCw } from "lucide-react";
import { useState, type ReactNode } from "react";
import { errorMessage, isStatus } from "../api/client";
import { environmentSuspensionQuery, useSetEnvironmentPaused, type Environment } from "../api/projects";
import { nodesQuery } from "../api/queries";
import type { EnvironmentSuspension as SuspensionState, SuspendedResource } from "../api/types";
import { useOnline } from "../lib/hooks";
import { can } from "../lib/permissions";
import { linkClass, listLinkClass } from "../lib/styles";
import { environmentHold, environmentPhases, isSettling, resourcePhases, type EnvironmentHold } from "../lib/suspension";
import { ConfirmDialog } from "./ConfirmDialog";
import { StatusLabel } from "./status";
import { Button, LoadingRows, Notice, QueryError, Section } from "./ui";

function summary(state: SuspensionState) {
  switch (state.phase) {
    case "pausing":
      return "Pause requested. Active operations are being cancelled, then workers stop the containers they captured. Earlier pause or resume operations finish first, and resources on offline workers stay pending until their worker reconnects.";
    case "paused":
      return "Paused. Captured containers are stopped with their restart policies disabled. Routes keep their hosts, TLS, authentication and security settings and return 503.";
    case "resuming":
      return "Resume requested. Workers start only the services that were running before the pause, restore their restart policies and wait up to 2 minutes for Docker health checks. Earlier pause or resume operations finish first.";
    case "error":
      return state.paused
        ? "Pausing did not finish for every resource. Retry the pause, or open each failed operation to see what happened."
        : "Resuming did not finish for every resource. Retry the resume, or repair a resource with Deploy, Start, Restart or a backup Restore on its page.";
    default:
      return state.paused ? "Pause requested. Waiting for workers to report progress." : "Resources run normally. Pausing stops them on their workers until you resume the environment.";
  }
}

function resumeNote(resource: SuspendedResource, paused: boolean) {
  if (!paused || resource.phase === "pausing") return null;
  if (resource.kind === "job") return "Jobs do not run on resume. Schedules continue from their next future time.";
  return resource.resume_wanted ? "Starts on resume" : "Stays stopped on resume: it was not running before the pause, or was stopped manually.";
}

function ResourceRow({ resource, paused, nodeName }: { resource: SuspendedResource; paused: boolean; nodeName: string }) {
  const phase = resourcePhases[resource.phase];
  const note = resumeNote(resource, paused);
  return (
    <li className="grid gap-x-4 gap-y-1 px-4 py-2.5 md:grid-cols-[minmax(0,1fr)_10rem_9rem]">
      <div className="min-w-0">
        <Link to="/resources/$resourceId" params={{ resourceId: resource.id }} className={`${listLinkClass} text-[13px] break-words`}>
          {resource.name}
        </Link>
        <p className="text-xs text-fg-muted">
          {resource.kind === "job" ? "Job" : "Service"} on{" "}
          <Link to="/nodes/$nodeId" params={{ nodeId: resource.node_id }} className="rounded hover:text-fg">
            {nodeName}
          </Link>
        </p>
        {note && <p className="text-xs text-fg-muted">{note}</p>}
        {!resource.supported && <p className="text-xs text-warn">The worker does not support pausing yet. Upgrade it, then verify it from its node page.</p>}
        {resource.error && <p className="text-xs break-words text-bad">{resource.error}</p>}
      </div>
      <div>
        <StatusLabel tone={phase.tone} label={phase.label} pulse={isSettling(resource.phase)} />
      </div>
      <div className="text-xs md:text-right">
        {resource.operation_id ? (
          <Link to="/operations/$operationId" params={{ operationId: resource.operation_id }} className={linkClass}>
            View operation
          </Link>
        ) : (
          isSettling(resource.phase) && <span className="text-fg-muted">Waiting to be queued</span>
        )}
      </div>
    </li>
  );
}

function SuspensionActions({ state, disabled, retrying, onRetry, onConfirm }: { state: SuspensionState; disabled: boolean; retrying: boolean; onRetry: () => void; onConfirm: (action: "pause" | "resume") => void }) {
  return (
    <>
      {state.phase === "error" && (
        <Button disabled={disabled} pending={retrying} onClick={onRetry}>
          {!retrying && <RotateCw aria-hidden className="size-3.5" />}
          {state.paused ? "Retry pause" : "Retry resume"}
        </Button>
      )}
      {state.paused ? (
        <Button variant="primary" disabled={disabled} onClick={() => onConfirm("resume")}>
          <Play aria-hidden className="size-3.5" />
          Resume environment
        </Button>
      ) : (
        <Button disabled={disabled} onClick={() => onConfirm("pause")}>
          <Pause aria-hidden className="size-3.5" />
          Pause environment
        </Button>
      )}
    </>
  );
}

function SuspensionDetails({
  environment,
  state,
  preview,
  canOperate,
  online,
  change,
  unsupportedNodes,
  nodeName,
}: {
  environment: Environment;
  state: SuspensionState;
  preview: boolean;
  canOperate: boolean;
  online: boolean;
  change: ReturnType<typeof useSetEnvironmentPaused>;
  unsupportedNodes: string[];
  nodeName: (id: string) => string;
}) {
  const phase = environmentPhases[state.phase];
  return (
    <>
      <div className="space-y-3 border-b border-line px-4 py-3">
        <div className="flex flex-wrap items-center gap-x-4 gap-y-1" aria-live="polite">
          <StatusLabel tone={phase.tone} label={phase.label} pulse={isSettling(state.phase)} />
          {(isSettling(state.phase) || state.phase === "error") && <span className="text-xs text-fg-muted">Requested: {state.paused ? "pause" : "resume"}</span>}
        </div>
        <p className="max-w-3xl text-[13px] text-fg-muted">{summary(state)}</p>
        {unsupportedNodes.length > 0 && (
          <Notice tone="warn" title="Some workers cannot pause resources">
            Upgrade and verify every worker in this environment before pausing or resuming it:{" "}
            {unsupportedNodes.map((id, index) => (
              <span key={id}>
                {index > 0 && ", "}
                <Link to="/nodes/$nodeId" params={{ nodeId: id }} className={linkClass}>
                  {nodeName(id)}
                </Link>
              </span>
            ))}
            .
          </Notice>
        )}
        {change.isError && (
          <Notice tone={isStatus(change.error, 409) ? "warn" : "error"} title={change.variables ? "Could not request the pause" : "Could not request the resume"}>
            <span className="break-words">{errorMessage(change.error)}</span>
          </Notice>
        )}
        {preview && <p className="text-xs text-fg-muted">Preview environments are paused, redeployed and removed through their preview, so this page shows their state only.</p>}
        {!preview && !canOperate && <p className="text-xs text-fg-muted">You can view this environment. Pausing and resuming it needs operator access.</p>}
        {canOperate && !online && <p className="text-xs text-fg-muted">Pausing and resuming are unavailable while offline.</p>}
      </div>
      {state.resources.length === 0 ? (
        <p className="px-4 py-3 text-[13px] text-fg-muted">No resources in {environment.name}. Pausing it has nothing to stop.</p>
      ) : (
        <ul aria-label={`Pause state of resources in ${environment.name}`} className="divide-y divide-line">
          {state.resources.map((resource) => (
            <ResourceRow key={resource.id} resource={resource} paused={state.paused} nodeName={nodeName(resource.node_id)} />
          ))}
        </ul>
      )}
    </>
  );
}

export function EnvironmentSuspensionSection({ environment, preview = false }: { environment: Environment; preview?: boolean }) {
  const state = useQuery(environmentSuspensionQuery(environment.id));
  const nodes = useQuery(nodesQuery);
  const change = useSetEnvironmentPaused(environment.id);
  const online = useOnline();
  const [confirm, setConfirm] = useState<"pause" | "resume" | null>(null);
  const canOperate = can(environment.permissions, "operate") && !preview;
  const data = state.isError ? undefined : state.data;
  const unsupported = data?.resources.filter((resource) => !resource.supported) ?? [];
  const unsupportedNodes = [...new Set(unsupported.map((resource) => resource.node_id))];
  const nodeName = (id: string) => nodes.data?.find((node) => node.id === id)?.name ?? "its node";
  const disabled = !online || change.isPending || unsupported.length > 0;
  const send = (paused: boolean) => {
    setConfirm(null);
    change.mutate(paused);
  };

  const actions = data && canOperate && (
    <SuspensionActions
      state={data}
      disabled={disabled}
      retrying={change.isPending && change.variables === data.paused}
      onRetry={() => send(data.paused)}
      onConfirm={setConfirm}
    />
  );

  let body: ReactNode;
  if (state.isPending) {
    body = <LoadingRows label="Loading pause state" rows={2} />;
  } else if (!data) {
    body = (
      <div className="p-3">
        <QueryError what="the environment's pause state" error={state.error} onRetry={() => void state.refetch()} retrying={state.isFetching} />
      </div>
    );
  } else {
    body = (
      <SuspensionDetails
        environment={environment}
        state={data}
        preview={preview}
        canOperate={canOperate}
        online={online}
        change={change}
        unsupportedNodes={unsupportedNodes}
        nodeName={nodeName}
      />
    );
  }

  return (
    <Section title="Pause and resume" description="Pausing stops this environment's resources on their workers. Resuming starts the services that were running before." actions={actions} flush>
      {body}
      <ConfirmDialog open={confirm === "pause"} title={`Pause ${environment.name}?`} confirmLabel="Pause environment" danger pending={change.isPending} onCancel={() => setConfirm(null)} onConfirm={() => send(true)}>
        <ul className="list-disc space-y-1.5 pl-4">
          <li>Active operations in this environment are asked to cancel. Workers finish their cleanup first, then stop the captured containers and disable their restart policies.</li>
          <li>Traffic is interrupted. Routes keep their hosts, TLS, authentication and security settings and return 503 while paused.</li>
          <li>Deploys, starts, restarts, rollbacks, promotions, restores and scheduled job runs are blocked until the environment resumes. Stop and Abort stay available.</li>
          <li>Pausing is not a backup and does not roll back application data. It can take a while, and resources on offline workers stay pending until their worker reconnects.</li>
        </ul>
      </ConfirmDialog>
      <ConfirmDialog open={confirm === "resume"} title={`Resume ${environment.name}?`} confirmLabel="Resume environment" pending={change.isPending} onCancel={() => setConfirm(null)} onConfirm={() => send(false)}>
        <ul className="list-disc space-y-1.5 pl-4">
          <li>Only services that were running before the pause start again, with their original restart policies. Manually stopped services and completed init containers stay as they are.</li>
          <li>The existing containers start; saved source or configuration changes are not deployed. Deploy afterwards to apply them.</li>
          <li>Jobs are not run. Schedules continue from their next future time, and missed pushes and schedule times are not replayed.</li>
          <li>Workers wait up to 2 minutes for Docker health checks. A failure stays visible here until you retry or repair the resource.</li>
        </ul>
      </ConfirmDialog>
    </Section>
  );
}

export function useEnvironmentHold(resource: { id: string; environment_id?: string; environment_paused?: boolean }) {
  const state = useQuery({ ...environmentSuspensionQuery(resource.environment_id ?? ""), enabled: Boolean(resource.environment_id) });
  const hold = environmentHold({
    resourceId: resource.id,
    environmentId: resource.environment_id,
    environmentPaused: resource.environment_paused,
    suspension: state.isError ? undefined : state.data,
    read: state.isError ? "error" : state.isSuccess ? "success" : "pending",
  });
  return { hold, state };
}

function failedHoldText(hold: Extract<EnvironmentHold, { kind: "failed" }>) {
  if (hold.repairable) {
    return "Retry the resume from the environment, or use Deploy, Start, Restart or a backup Restore to repair this resource. Restored copies, restore drills and release promotion wait until the resume succeeds.";
  }
  return hold.paused ? "Retry the pause from the environment. Stop stays available." : "Retry the resume from the environment.";
}

function activeHoldNotice(hold: Extract<EnvironmentHold, { kind: "paused" | "pending" }>) {
  if (hold.kind === "pending") {
    return { title: "Environment resume in progress", text: "Deploy, start, restart, rollback, promotion and restore are available when the resume finishes." };
  }
  const pausing = hold.resource?.phase === "pausing";
  return {
    title: pausing ? "Environment pause in progress" : "Environment paused",
    text: `${pausing ? "Workers are still stopping containers. " : ""}Deploy, start, restart, rollback, promotion, restore and job runs are unavailable until the environment resumes. Stop and Abort stay available.`,
  };
}

export function EnvironmentHoldNotice({ id, hold, environment, onRetry, retrying }: { id?: string; hold: EnvironmentHold; environment: ReactNode; onRetry: () => void; retrying: boolean }) {
  if (hold.kind === "none" || hold.kind === "loading") return null;
  const open = <span className="shrink-0">{environment}</span>;
  if (hold.kind === "unknown") {
    return (
      <div id={id}>
        <Notice
          tone="warn"
          title="Environment pause state unknown"
          action={
            <Button variant="ghost" size="sm" onClick={onRetry} pending={retrying}>
              {!retrying && <RotateCw aria-hidden className="size-3.5" />}
              Retry
            </Button>
          }
        >
          Cove could not read whether this resource's environment is paused or resuming, so deploy, start, restart, rollback, promotion and restore stay unavailable until it can be read. Stop and Abort stay available.
        </Notice>
      </div>
    );
  }
  if (hold.kind === "failed") {
    return (
      <div id={id}>
        <Notice tone="error" title={hold.paused ? "Pausing this resource failed" : "Resuming this resource failed"} action={open}>
          {hold.resource.error && <span className="block font-mono text-xs break-words">{hold.resource.error}</span>}
          {failedHoldText(hold)}
        </Notice>
      </div>
    );
  }
  const notice = activeHoldNotice(hold);
  return (
    <div id={id}>
      <Notice tone="info" title={notice.title} action={open}>
        {notice.text}
      </Notice>
    </div>
  );
}

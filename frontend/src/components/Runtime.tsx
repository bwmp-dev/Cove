import { useQuery } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { AlertTriangle, RotateCw } from "lucide-react";
import { errorMessage, isStatus } from "../api/client";
import { workloadLogsQuery, workloadRuntimeQuery } from "../api/queries";
import type { Container, IdleRuntime, ResourceSuspension } from "../api/types";
import { formatRelative } from "../lib/format";
import { idlePhase, idlePhases } from "../lib/idle";
import { countLabel } from "../lib/hierarchy";
import { useNow } from "../lib/hooks";
import { LogPanel } from "./LogViewer";
import { Button, MetaList, Notice, Spinner } from "./ui";
import { containerStatus, type ContainerTone as Tone } from "../lib/containers";
import { linkClass, tableHeadClass, tableRowClass } from "../lib/styles";

const toneClass: Record<Tone, string> = { ok: "bg-ok", warn: "bg-warn", bad: "bg-bad", neutral: "bg-fg-subtle" };

function ExposureWarning({ className = "" }: { className?: string }) {
  return (
    <span className={`items-start gap-1.5 font-sans text-xs text-warn ${className}`}>
      <AlertTriangle aria-hidden className="mt-px size-3.5 shrink-0" />
      <span>Published on all interfaces</span>
    </span>
  );
}

function ContainerRow({ container, kind, manuallyStopped, environmentPaused, idleSleeping }: { container: Container; kind: "service" | "job"; manuallyStopped: boolean; environmentPaused: boolean; idleSleeping: boolean }) {
  const { tone, label } = containerStatus(container, kind, { manuallyStopped, environmentPaused, idleSleeping });
  return (
    <tr className={tableRowClass}>
      <td className="py-2.5 pr-4 pl-4">
        <span className="font-mono text-xs break-all text-fg">{container.name}</span>
        <div className="mt-1 text-xs text-fg-muted">
          {container.metrics ? (
            <MetaList>
              <span key="cpu" className="whitespace-nowrap">CPU {container.metrics.cpu}</span>
              <span key="memory" className="whitespace-nowrap">Memory {container.metrics.memory}</span>
            </MetaList>
          ) : (
            "Usage unavailable"
          )}
        </div>
        <div className="mt-0.5 truncate font-mono text-xs text-fg-subtle md:hidden">{container.image}</div>
        {container.exposed_ports && <ExposureWarning className="mt-1 flex lg:hidden" />}
      </td>
      <td className="hidden max-w-0 truncate py-2.5 pr-4 font-mono text-xs text-fg-muted md:table-cell" title={container.image}>
        {container.image}
      </td>
      <td className="py-2.5 pr-4">
        <span className="inline-flex items-start gap-2 text-[13px] text-fg">
          <span aria-hidden className={`mt-1.5 size-2 shrink-0 rounded-full ${toneClass[tone]}`} />
          <span>
            <span className="whitespace-nowrap">{label}</span>
            {container.status && <span className="block text-xs text-fg-muted">{container.status}</span>}
          </span>
        </span>
      </td>
      <td className="hidden py-2.5 pr-4 font-mono text-xs text-fg-muted lg:table-cell">
        {container.ports ? <span className="break-words">{container.ports}</span> : <span className="font-sans whitespace-nowrap text-fg-subtle">None published</span>}
        {container.exposed_ports && <ExposureWarning className="mt-1 flex" />}
      </td>
    </tr>
  );
}

function suspensionText(suspension: ResourceSuspension) {
  if (suspension.reason === "idle") {
    if (suspension.phase === "resuming") return `The worker is waking this app: ${countLabel(suspension.previously_running, "container")} that ran before it went to sleep start again.`;
    return `${suspension.phase === "pausing" ? "The worker is putting this app to sleep" : "This app is asleep after being idle"}: ${countLabel(suspension.captured, "container")} captured, ${suspension.previously_running} of them running before it went to sleep. The next authorized request wakes it.`;
  }
  if (suspension.reason === "manual_stop") {
    return `This resource was stopped intentionally: ${countLabel(suspension.captured, "container")} held stopped by the worker. Requests do not wake it; Start, Restart or Deploy starts it again.`;
  }
  if (suspension.phase === "resuming") return `The worker is resuming this resource for its environment: ${countLabel(suspension.previously_running, "container")} that ran before the pause start again.`;
  return `${suspension.phase === "pausing" ? "The worker is pausing" : "The environment paused"} this resource: ${countLabel(suspension.captured, "container")} captured, ${suspension.previously_running} of them running before the pause.`;
}

function SuspensionSummary({ suspension }: { suspension: ResourceSuspension }) {
  return <p className="border-b border-line px-4 py-2 text-xs text-fg-muted">{suspensionText(suspension)}</p>;
}

function IdleSummary({ idle, now }: { idle: IdleRuntime; now: number }) {
  const phase = idlePhases[idlePhase(idle.phase)];
  return (
    <p className="border-b border-line px-4 py-2 text-xs text-fg-muted">
      Idle sleep: <span className="text-fg">{phase.label}</span>. {countLabel(idle.active_requests, "active request")}.
      {idle.last_activity && ` Idle timer reset ${formatRelative(idle.last_activity, now)}.`}
      {idle.error && <span className="block break-words text-warn">{idle.error}</span>}
    </p>
  );
}

// Only the worker's own report marks stopped containers as paused or sleeping; the requested state alone does not.
function stoppedBy(suspension: ResourceSuspension | null | undefined) {
  const stopping = suspension?.phase === "paused" || suspension?.phase === "pausing";
  return { environmentPaused: stopping && suspension.reason === "environment", idleSleeping: stopping && suspension.reason === "idle", manualStop: stopping && suspension.reason === "manual_stop" };
}

export function RuntimeContainers({ workloadId, kind, operationActive, manuallyStopped }: { workloadId: string; kind: "service" | "job"; operationActive: boolean; manuallyStopped: boolean }) {
  const runtime = useQuery(workloadRuntimeQuery(workloadId, operationActive));
  const now = useNow(5_000);

  const header = (
    <div className="flex flex-wrap items-center justify-between gap-2 border-b border-line px-4 py-2">
      <p className="flex items-center gap-2 text-xs text-fg-muted" aria-live="polite">
        {runtime.isError
          ? "Current Docker state is unknown."
          : runtime.isSuccess
            ? `Read from Docker on the node ${formatRelative(new Date(runtime.dataUpdatedAt).toISOString(), now)}.`
            : "Reading Docker state from the node."}
        {runtime.isFetching && <Spinner className="size-3 text-fg-subtle" />}
      </p>
      <Button variant="ghost" onClick={() => void runtime.refetch()} disabled={runtime.isFetching}>
        <RotateCw aria-hidden className="size-3.5" />
        Refresh
      </Button>
    </div>
  );

  if (runtime.isPending) {
    return header;
  }
  // Never show the previous container list once a read fails: it would look like current health.
  if (runtime.isError) {
    return (
      <>
        {header}
        <div className="p-3">
          <Notice tone={isStatus(runtime.error, 502) ? "warn" : "error"} title="Container state unavailable">
            {errorMessage(runtime.error)}
          </Notice>
        </div>
      </>
    );
  }
  const { containers, suspension, idle } = runtime.data;
  const { environmentPaused, idleSleeping, manualStop } = stoppedBy(suspension);
  // A worker-confirmed manual stop holds the containers even when the saved resource is not marked manually stopped.
  const stopped = manuallyStopped || manualStop;
  const exposed = containers.filter((container) => container.exposed_ports).length;
  return (
    <>
      {header}
      {idle && kind === "service" && <IdleSummary idle={idle} now={now} />}
      {suspension && <SuspensionSummary suspension={suspension} />}
      {exposed > 0 && (
        <div className="border-b border-line p-3">
          <Notice tone="warn" title={exposed === 1 ? "A container publishes ports on every interface" : `${exposed} containers publish ports on every interface`}>
            Docker accepts connections to these ports from any network the node is on. Unless public access is intended, bind them to a specific address from{" "}
            <Link to="/resources/$resourceId/networking" params={{ resourceId: workloadId }} className={linkClass}>
              Networking
            </Link>
            .
          </Notice>
        </div>
      )}
      {containers.length === 0 ? (
        <p className="px-4 py-3 text-[13px] text-fg-muted">
          {kind === "job"
            ? "No containers exist for this job on the node. Deploy runs it."
            : "No containers exist for this resource on the node. It has not been deployed, or its containers were removed."}
        </p>
      ) : (
        <table className="w-full table-fixed text-left">
          <caption className="sr-only">Containers</caption>
          <thead>
            <tr className={tableHeadClass}>
              <th scope="col" className="py-2 pr-4 pl-4 font-medium">
                Container
              </th>
              <th scope="col" className="hidden w-[22%] py-2 pr-4 font-medium md:table-cell">
                Image
              </th>
              <th scope="col" className="w-32 py-2 pr-4 font-medium sm:w-36">
                State
              </th>
              <th scope="col" className="hidden w-28 py-2 pr-4 font-medium lg:table-cell">
                Ports
              </th>
            </tr>
          </thead>
          <tbody>
            {containers.map((container) => (
              <ContainerRow key={container.id} container={container} kind={kind} manuallyStopped={stopped} environmentPaused={environmentPaused} idleSleeping={idleSleeping} />
            ))}
          </tbody>
        </table>
      )}
    </>
  );
}

export function ContainerLogs({ workloadId }: { workloadId: string }) {
  return (
    <LogPanel
      options={workloadLogsQuery(workloadId)}
      label="Container logs"
      description="Refreshed every few seconds while this tab is open."
      emptyText="The containers have not written any output."
    />
  );
}

import { useQuery } from "@tanstack/react-query";
import { RotateCw } from "lucide-react";
import { errorMessage, isStatus } from "../api/client";
import { workloadLogsQuery, workloadRuntimeQuery } from "../api/queries";
import type { Container } from "../api/types";
import { formatRelative } from "../lib/format";
import { useNow } from "../lib/hooks";
import { LogPanel } from "./LogViewer";
import { Button, MetaList, Notice, Spinner } from "./ui";
import { containerStatus, type ContainerTone as Tone } from "../lib/containers";
import { tableHeadClass, tableRowClass } from "../lib/styles";

const toneClass: Record<Tone, string> = { ok: "bg-ok", warn: "bg-warn", bad: "bg-bad", neutral: "bg-fg-subtle" };

function ContainerRow({ container, kind, manuallyStopped }: { container: Container; kind: "service" | "job"; manuallyStopped: boolean }) {
  const { tone, label } = containerStatus(container, kind, { manuallyStopped });
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
      </td>
    </tr>
  );
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
  const containers = runtime.data.containers;
  return (
    <>
      {header}
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
              <ContainerRow key={container.id} container={container} kind={kind} manuallyStopped={manuallyStopped} />
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

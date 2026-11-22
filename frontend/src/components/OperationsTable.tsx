import { Link } from "@tanstack/react-router";
import type { Operation } from "../api/types";
import { formatDuration, shortId } from "../lib/format";
import { actionLabels, durationMs, isActive } from "../lib/operations";
import { listLinkClass, tableHeadClass, tableRowClass } from "../lib/styles";
import { RelativeTime } from "./RelativeTime";
import { OperationStateLabel } from "./status";

interface OperationsTableProps {
  operations: Operation[];
  workloadNames?: Map<string, string>;
  caption: string;
}

export function OperationsTable({ operations, workloadNames, caption }: OperationsTableProps) {
  const showWorkload = workloadNames !== undefined;
  return (
    <table className="w-full text-left">
      <caption className="sr-only">{caption}</caption>
      <thead>
        <tr className={tableHeadClass}>
          <th scope="col" className="py-2 pr-4 pl-4 font-medium">
            Operation
          </th>
          {showWorkload && (
            <th scope="col" className="hidden py-2 pr-4 font-medium md:table-cell">
              Resource
            </th>
          )}
          <th scope="col" className="py-2 pr-4 font-medium">
            Status
          </th>
          <th scope="col" className="hidden py-2 pr-4 font-medium lg:table-cell">
            Revision
          </th>
          <th scope="col" className="hidden py-2 pr-4 text-right font-medium sm:table-cell">
            Duration
          </th>
          <th scope="col" className="py-2 pr-4 text-right font-medium">
            Created
          </th>
        </tr>
      </thead>
      <tbody>
        {operations.map((operation) => {
          const duration = durationMs(operation);
          const workloadName = workloadNames?.get(operation.workload_id);
          const revision = operation.result?.revision;
          return (
            <tr key={operation.id} className={tableRowClass}>
              <td className="py-2.5 pr-4 pl-4">
                <Link
                  to="/operations/$operationId"
                  params={{ operationId: operation.id }}
                  className={listLinkClass}
                >
                  {actionLabels[operation.action] ?? operation.action}
                </Link>
                <span className="ml-2 font-mono text-xs text-fg-subtle">{shortId(operation.id)}</span>
                {showWorkload && <div className="mt-0.5 truncate text-xs text-fg-muted md:hidden">{workloadName ?? "Unknown resource"}</div>}
              </td>
              {showWorkload && (
                <td className="hidden max-w-56 truncate py-2.5 pr-4 text-[13px] text-fg-muted md:table-cell">
                  {workloadName ?? <span className="font-mono text-xs text-fg-subtle">{shortId(operation.workload_id)}</span>}
                </td>
              )}
              <td className="py-2.5 pr-4">
                <OperationStateLabel state={operation.state} />
              </td>
              <td className="hidden py-2.5 pr-4 font-mono text-xs text-fg-muted lg:table-cell">
                {revision ? <span title={revision}>{revision.slice(0, 7)}</span> : <span className="font-sans text-fg-subtle">None</span>}
              </td>
              <td className="hidden py-2.5 pr-4 text-right text-[13px] whitespace-nowrap text-fg-muted tabular-nums sm:table-cell">
                {duration !== null ? (
                  formatDuration(duration)
                ) : (
                  <span className="text-fg-subtle">{operation.state === "queued" ? "Waiting to start" : isActive(operation.state) ? "In progress" : "Unknown"}</span>
                )}
              </td>
              <td className="py-2.5 pr-4 text-right text-[13px] whitespace-nowrap text-fg-muted tabular-nums">
                <RelativeTime value={operation.created_at} />
              </td>
            </tr>
          );
        })}
      </tbody>
    </table>
  );
}

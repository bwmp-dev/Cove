import { useQuery } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { RefreshCw } from "lucide-react";
import { isStatus } from "../api/client";
import { workloadDriftQuery, type DriftChange, type WorkloadDrift } from "../api/project-configuration";
import { shortId } from "../lib/format";
import type { Tone } from "../lib/nodes";
import { driftResult } from "../lib/project-configuration";
import { linkClass, tableHeadClass, tableRowClass } from "../lib/styles";
import { RelativeTime } from "./RelativeTime";
import { StatusLabel } from "./status";
import { Button, LoadingRows, Notice, QueryError, Section } from "./ui";

const title = "Runtime drift";
const description = "Compares the containers on the node with what the last successful Cove operation started. Read-only: nothing is repaired or restarted.";

const changeStates: Record<DriftChange["state"], { tone: Tone; label: string }> = {
  changed: { tone: "warn", label: "Changed" },
  added: { tone: "warn", label: "Added outside Cove" },
  missing: { tone: "bad", label: "Missing" },
};

export function RuntimeDrift({ workloadId }: { workloadId: string }) {
  const drift = useQuery(workloadDriftQuery(workloadId));
  const refresh = (
    <Button size="sm" onClick={() => void drift.refetch()} pending={drift.isFetching} disabled={drift.isFetching}>
      {!drift.isFetching && <RefreshCw aria-hidden className="size-3.5" />}
      Refresh
    </Button>
  );
  return (
    <Section title={title} description={description} actions={refresh} flush>
      {drift.isPending ? (
        <LoadingRows label="Inspecting containers" rows={2} />
      ) : drift.isError ? (
        <div className="p-4">
          {isStatus(drift.error, 409) ? (
            <Notice tone="warn" title="The worker cannot inspect drift">
              Upgrade and verify the node's worker, then refresh.
            </Notice>
          ) : isStatus(drift.error, 502) ? (
            <Notice tone="error" title="The worker could not inspect the containers">
              {drift.error.message} The containers were not checked; this is not a clean result.
            </Notice>
          ) : (
            <QueryError what="runtime drift" error={drift.error} onRetry={() => void drift.refetch()} retrying={drift.isFetching} />
          )}
        </div>
      ) : (
        <DriftReport drift={drift.data} />
      )}
    </Section>
  );
}

function DriftReport({ drift }: { drift: WorkloadDrift }) {
  const result = driftResult(drift);
  const changes = drift.changes ?? [];
  return (
    <div className="min-w-0">
      <div className="space-y-3 p-4">
        {result === "no-baseline" && (
          <Notice title="No baseline recorded yet">
            Drift is measured against the last successful deploy, start, stop or restart run by an upgraded worker. Run one of those to record a baseline.
          </Notice>
        )}
        {result === "busy" && (
          <Notice tone="warn" title="Inspection deferred">
            An operation is running on this resource, so the containers were not compared. Refresh when it finishes.
          </Notice>
        )}
        {result === "suspended" && (
          <Notice title="Inspection deferred while stopped">
            This resource is paused with its environment or asleep, so its containers were not inspected. Refresh after it resumes or wakes.
          </Notice>
        )}
        {result === "clean" && (
          <p role="status">
            <StatusLabel tone="ok" label="No drift since the last Cove operation" />
          </p>
        )}
        {result === "drifted" && (
          <p role="status">
            <StatusLabel tone="warn" label={`${changes.length} ${changes.length === 1 ? "container differs" : "containers differ"} from the baseline`} />
          </p>
        )}
        <p className="flex flex-wrap gap-x-4 gap-y-1 text-xs text-fg-muted">
          <span>
            Checked <RelativeTime value={drift.checked_at} />
          </span>
          {drift.captured_at && (
            <span>
              Baseline captured <RelativeTime value={drift.captured_at} />
            </span>
          )}
          {drift.operation && (
            <Link to="/operations/$operationId" params={{ operationId: drift.operation }} className={linkClass}>
              Baseline operation
            </Link>
          )}
        </p>
        {result === "clean" && <p className="text-xs text-fg-muted">A stopped resource can intentionally have no containers.</p>}
      </div>
      {result === "drifted" && (
        <table className="w-full table-fixed border-t border-line text-left text-[13px]">
          <caption className="sr-only">Containers that differ from the baseline</caption>
          <thead>
            <tr className={tableHeadClass}>
              <th scope="col" className="w-2/5 px-4 sm:w-1/3">
                Service
              </th>
              <th scope="col" className="hidden w-40 px-4 sm:table-cell">
                State
              </th>
              <th scope="col" className="px-4">
                What changed
              </th>
            </tr>
          </thead>
          <tbody>
            {changes.map((change) => {
              const state = changeStates[change.state];
              return (
                <tr key={`${change.container}:${change.state}`} className={`${tableRowClass} align-top`}>
                  <td className="px-4 py-2.5">
                    <p className="font-medium break-words text-fg">{change.service || "Unknown service"}</p>
                    <p className="font-mono text-xs text-fg-muted" title={change.container}>
                      {shortId(change.container)}
                    </p>
                    <div className="mt-1 sm:hidden">
                      <StatusLabel tone={state.tone} label={state.label} />
                    </div>
                  </td>
                  <td className="hidden px-4 py-2.5 sm:table-cell">
                    <StatusLabel tone={state.tone} label={state.label} />
                  </td>
                  <td className="px-4 py-2.5 text-xs">
                    {(change.fields ?? []).length > 0 ? <span className="break-words">{(change.fields ?? []).join(", ")}</span> : <span className="text-fg-muted">{change.state === "changed" ? "Not specified" : "Whole container"}</span>}
                  </td>
                </tr>
              );
            })}
          </tbody>
        </table>
      )}
    </div>
  );
}

import { useQuery, type UseQueryResult } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { RotateCw } from "lucide-react";
import { useState, type ReactNode } from "react";
import { errorMessage, isStatus } from "../api/client";
import { nodeMetricsQuery, workloadMetricsQuery, type DeployMarker, type MetricHistory, type MetricRange } from "../api/telemetry";
import { formatAbsolute } from "../lib/format";
import { actionLabels } from "../lib/operations";
import { listLinkClass } from "../lib/styles";
import { formatBinaryBytes, formatPercent, formatRate, hasReadings, isMetricRange, maxGap, nodeCharts, rangeOptions, rangeWindows, tierLabels, workloadCharts } from "../lib/telemetry";
import { MetricChart } from "./MetricChart";
import { OperationStateLabel } from "./status";
import { Select } from "./Select";
import { Button, Notice, QueryError, Spinner } from "./ui";

const shortTime = new Intl.DateTimeFormat("en", { hour: "2-digit", minute: "2-digit" });
const dayTime = new Intl.DateTimeFormat("en", { month: "short", day: "numeric", hour: "2-digit", minute: "2-digit" });
const shortDate = new Intl.DateTimeFormat("en", { month: "short", day: "numeric" });
const fullDate = new Intl.DateTimeFormat("en", { dateStyle: "medium" });

const timeFormats: Record<MetricRange, (time: number) => string> = {
  hour: (time) => shortTime.format(time),
  day: (time) => dayTime.format(time),
  week: (time) => shortDate.format(time),
  month: (time) => shortDate.format(time),
  year: (time) => fullDate.format(time),
};

const chartGrid = "grid grid-cols-1 gap-x-8 gap-y-6 @3xl:grid-cols-2";

function RangeToolbar({ id, range, onRange, query }: { id: string; range: MetricRange; onRange: (range: MetricRange) => void; query: UseQueryResult<unknown> }) {
  return (
    <div className="flex flex-wrap items-center gap-2 border-b border-line px-3 py-2.5">
      <label htmlFor={id} className="sr-only">
        Time range
      </label>
      <div className="w-full min-[360px]:w-44">
        <Select id={id} aria-label="Time range" value={range} onChange={(event) => isMetricRange(event.target.value) && onRange(event.target.value)}>
          {rangeOptions.map((option) => (
            <option key={option.value} value={option.value}>
              {option.label}
            </option>
          ))}
        </Select>
      </div>
      <p className="flex items-center gap-2 text-xs text-fg-muted" aria-live="polite">
        {query.isFetching && <Spinner className="size-3 text-fg-subtle" />}
        {query.isFetching ? "Loading samples" : query.isSuccess ? `Updated ${shortTime.format(query.dataUpdatedAt)}` : null}
      </p>
      <Button variant="ghost" size="sm" className="ml-auto" onClick={() => void query.refetch()} disabled={query.isFetching}>
        <RotateCw aria-hidden className="size-3.5" />
        Refresh
      </Button>
    </div>
  );
}

function HistoryState<T>({ query, what, empty, children }: { query: UseQueryResult<MetricHistory<T>>; what: string; empty: ReactNode; children: (history: MetricHistory<T>) => ReactNode }) {
  if (query.isPending) {
    return (
      <div role="status" aria-label={`Loading ${what}`} className={`${chartGrid} p-4`}>
        {[0, 1].map((item) => (
          <div key={item} className="space-y-2">
            <div className="h-3 w-32 animate-pulse rounded bg-raised" />
            <div className="h-32 animate-pulse rounded-md bg-raised" />
          </div>
        ))}
      </div>
    );
  }
  if (query.isError && !query.data) {
    return (
      <div className="p-3">
        {isStatus(query.error, 503) ? (
          <Notice tone="warn" title="Metric history unavailable" action={<Button variant="ghost" size="sm" onClick={() => void query.refetch()} pending={query.isFetching}>Retry</Button>}>
            {errorMessage(query.error)}
          </Notice>
        ) : (
          <QueryError what={what} error={query.error} onRetry={() => void query.refetch()} retrying={query.isFetching} />
        )}
      </div>
    );
  }
  const history = query.data;
  if (!history) return null;
  return (
    <div className="space-y-4 p-4">
      {query.isError && (
        <Notice tone="warn" title="Could not refresh">
          Showing samples loaded at {formatAbsolute(new Date(query.dataUpdatedAt).toISOString())}. {errorMessage(query.error)}
        </Notice>
      )}
      {history.limited && (
        <Notice tone="warn" title="Range truncated">
          This range has more samples than one response returns. The oldest samples are not shown; choose a shorter range to see them.
        </Notice>
      )}
      {history.points.length === 0 ? <p className="text-[13px] text-fg-muted">{empty}</p> : children(history)}
    </div>
  );
}

function tierNote(history: MetricHistory<unknown>) {
  return `Each point is the last sample recorded in its ${tierLabels[history.tier]}, not an average or peak. Gaps mean no sample was recorded.`;
}

export function NodeMetricHistory({ nodeId }: { nodeId: string }) {
  const [range, setRange] = useState<MetricRange>("hour");
  const query = useQuery(nodeMetricsQuery(nodeId, range));
  return (
    <>
      <RangeToolbar id={`node-${nodeId}-range`} range={range} onRange={setRange} query={query} />
      <HistoryState query={query} what="node metric history" empty="No samples were recorded in this range. Cove records history once the worker supports telemetry; earlier history is not backfilled.">
        {(history) => {
          const charts = nodeCharts(history);
          const end = query.dataUpdatedAt;
          const common = { start: end - rangeWindows[range], end, gap: maxGap(history.tier), formatTime: timeFormats[range] };
          return (
            <>
              <p className="text-xs text-fg-muted">{tierNote(history)}</p>
              <div className={chartGrid}>
                <MetricChart title="CPU" description="Percent of all host cores." series={charts.cpu} format={formatPercent} ceiling={100} {...common} />
                <MetricChart
                  title="Memory used"
                  description={charts.memoryTotal === undefined ? "Total minus available memory." : `Total minus available memory, of ${formatBinaryBytes(charts.memoryTotal)}.`}
                  series={charts.memory}
                  format={formatBinaryBytes}
                  ceiling={charts.memoryTotal}
                  {...common}
                />
                <MetricChart
                  title="Cove data disk used"
                  description={`Only the filesystem holding Cove data${charts.diskTotal === undefined ? "" : ` (${formatBinaryBytes(charts.diskTotal)})`}. Other mounts are not measured.`}
                  series={charts.disk}
                  format={formatBinaryBytes}
                  ceiling={charts.diskTotal}
                  {...common}
                />
                <MetricChart
                  title="Network interface traffic"
                  description="Average rate between samples, summed over the worker's non-loopback interfaces, including virtual ones. Not internet bandwidth. Counters reset when an interface or the host restarts."
                  series={charts.network}
                  format={formatRate}
                  emptyText={charts.hasNetwork ? "Not enough consecutive samples to compute a rate." : "This worker does not report network counters."}
                  {...common}
                />
              </div>
            </>
          );
        }}
      </HistoryState>
    </>
  );
}

function DeployMarkers({ markers, start }: { markers: DeployMarker[]; start: number }) {
  const shown = markers.filter((marker) => Date.parse(marker.timestamp) >= start);
  if (shown.length === 0) return <p className="text-xs text-fg-muted">No deploys or rollbacks in this range.</p>;
  return (
    <div className="min-w-0">
      <h3 className="text-[13px] font-semibold text-fg">Deploys and rollbacks</h3>
      <p className="mt-0.5 text-xs text-fg-muted">Dashed lines on the charts mark when each operation was created; red lines failed.</p>
      <ul className="mt-2 divide-y divide-line rounded-md border border-line">
        {shown.map((marker) => (
          <li key={marker.operation_id} className="flex flex-wrap items-center justify-between gap-x-4 gap-y-1 px-3 py-2 text-[13px]">
            <Link to="/operations/$operationId" params={{ operationId: marker.operation_id }} className={listLinkClass}>
              {actionLabels[marker.action] ?? marker.action}
            </Link>
            <span className="flex flex-wrap items-center gap-x-4 gap-y-1">
              <OperationStateLabel state={marker.state} />
              <time dateTime={marker.timestamp} className="text-xs text-fg-muted tabular-nums">
                {formatAbsolute(marker.timestamp)}
              </time>
            </span>
          </li>
        ))}
      </ul>
    </div>
  );
}

export function WorkloadMetricHistory({ workloadId }: { workloadId: string }) {
  const [range, setRange] = useState<MetricRange>("hour");
  const query = useQuery(workloadMetricsQuery(workloadId, range));
  return (
    <>
      <RangeToolbar id={`resource-${workloadId}-range`} range={range} onRange={setRange} query={query} />
      <HistoryState query={query} what="resource metric history" empty="No container samples were recorded in this range. Cove records history while containers run on a worker that supports telemetry; earlier history is not backfilled.">
        {(history) => {
          const charts = workloadCharts(history);
          const end = query.dataUpdatedAt;
          const start = end - rangeWindows[range];
          const common = { start, end, gap: maxGap(history.tier), formatTime: timeFormats[range], markers: history.deploy_markers };
          const usage = hasReadings(charts.cpu) || hasReadings(charts.memory) || hasReadings(charts.networkRx) || hasReadings(charts.blockRead);
          return (
            <>
              <p className="text-xs text-fg-muted">
                {tierNote(history)} One line per container name, including earlier placements on other nodes.
              </p>
              {!usage && (
                <Notice tone="info" title="No numeric usage recorded">
                  The worker did not report parsed container usage for these samples. Upgrade the worker to record CPU, memory, network and block I/O history.
                </Notice>
              )}
              <div className={chartGrid}>
                <MetricChart title="CPU" description="Percent of one core; above 100% means more than one core." series={charts.cpu} format={formatPercent} {...common} />
                <MetricChart title="Memory used" description="Container memory in use." series={charts.memory} format={formatBinaryBytes} {...common} />
                <MetricChart title="Network received" description="Average rate between samples on the container's interfaces." series={charts.networkRx} format={formatRate} {...common} />
                <MetricChart title="Network sent" description="Average rate between samples on the container's interfaces." series={charts.networkTx} format={formatRate} {...common} />
                <MetricChart title="Block I/O read" description="Average block device read rate between samples. Not disk space used." series={charts.blockRead} format={formatRate} {...common} />
                <MetricChart title="Block I/O written" description="Average block device write rate between samples. Not disk space used." series={charts.blockWritten} format={formatRate} {...common} />
              </div>
              <DeployMarkers markers={history.deploy_markers} start={start} />
            </>
          );
        }}
      </HistoryState>
    </>
  );
}

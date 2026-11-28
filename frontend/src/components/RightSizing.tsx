import { useQuery } from "@tanstack/react-query";
import { sizingQuery, type ServiceSizing } from "../api/advice";
import { isStatus } from "../api/client";
import type { ServiceLimits } from "../api/types";
import { formatAbsolute } from "../lib/format";
import { tableHeadClass, tableRowClass } from "../lib/styles";
import { formatBinaryBytes, formatPercent } from "../lib/telemetry";
import { RelativeTime } from "./RelativeTime";
import { LoadingRows, Notice, QueryError, Section } from "./ui";

function configured(limit: ServiceLimits | undefined) {
  const cpus = limit?.cpus ? `${limit.cpus} CPU` : "CPU from Compose";
  const memory = limit?.memory_mb ? `${limit.memory_mb.toLocaleString("en")} MiB` : "memory from Compose";
  return `${cpus}, ${memory}`;
}

function Suggestion({ item }: { item: ServiceSizing }) {
  if (item.suggested_cpus === null || item.suggested_memory_mb === null) return <span className="text-fg-muted">No suggestion</span>;
  return (
    <span className="tabular-nums">
      {item.suggested_cpus} CPU, {item.suggested_memory_mb.toLocaleString("en")} MiB
    </span>
  );
}

function SizingRow({ item, limit }: { item: ServiceSizing; limit: ServiceLimits | undefined }) {
  return (
    <tr className={`${tableRowClass} align-top`}>
      <td className="py-2.5 pr-4 pl-4">
        <span className="font-mono text-xs break-all text-fg">{item.service}</span>
        <p className="mt-0.5 text-xs text-fg-muted">Configured: {configured(limit)}</p>
        <p className="mt-0.5 text-xs text-fg-muted md:hidden">
          p95 {formatPercent(item.cpu_p95_percent)} CPU, {formatBinaryBytes(item.memory_p95_bytes)} memory
        </p>
        <p className="mt-0.5 text-xs text-fg-subtle">
          {item.hours} distinct {item.hours === 1 ? "hour" : "hours"}, {item.samples.toLocaleString("en")} samples, last{" "}
          <RelativeTime value={item.last_sample} />
          <span className="sr-only">
            , from {formatAbsolute(item.first_sample)} to {formatAbsolute(item.last_sample)}
          </span>
        </p>
      </td>
      <td className="hidden py-2.5 pr-4 text-right text-[13px] text-fg-muted tabular-nums md:table-cell">{formatPercent(item.cpu_p95_percent)}</td>
      <td className="hidden py-2.5 pr-4 text-right text-[13px] text-fg-muted tabular-nums md:table-cell">{formatBinaryBytes(item.memory_p95_bytes)}</td>
      <td className="py-2.5 pr-4 text-[13px]">
        <Suggestion item={item} />
        <p className="mt-0.5 text-xs text-fg-muted">{item.notice}</p>
      </td>
    </tr>
  );
}

// Suggestions are advice only. Nothing here edits the limits form above; people copy a value if they agree with it.
export function RightSizing({ workloadId, limits }: { workloadId: string; limits: Record<string, ServiceLimits> | undefined }) {
  const sizing = useQuery(sizingQuery(workloadId));
  const data = sizing.data;
  return (
    <Section
      title="Right-sizing suggestions"
      description={
        data
          ? `From the last ${data.window} of hourly samples. ${data.sampling} Suggestions add 25% headroom above the 95th percentile and need at least 20 distinct hours with a sample in the last 2 hours. CPU percentages are of one core.`
          : "Read-only suggestions from recorded hourly usage. Limits are never changed automatically."
      }
      flush
    >
      {sizing.isPending ? (
        <LoadingRows label="Loading usage history" rows={2} />
      ) : sizing.isError || !data ? (
        <div className="p-3">
          {sizing.isError && isStatus(sizing.error, 503) ? (
            <Notice tone="warn" title="Usage history unavailable">
              The control plane could not read usage history. Try again later.
            </Notice>
          ) : (
            <QueryError what="right-sizing suggestions" error={sizing.error} onRetry={() => void sizing.refetch()} retrying={sizing.isFetching} />
          )}
        </div>
      ) : data.services.length === 0 ? (
        <p className="px-4 py-4 text-[13px] text-fg-muted">
          No per-service usage history yet. Suggestions need hourly samples labelled with their Compose service, which workers record from this version on; older history and resources that have not run recently have none.
        </p>
      ) : (
        <table className="w-full table-fixed text-left">
          <caption className="sr-only">Right-sizing suggestions per Compose service</caption>
          <thead>
            <tr className={tableHeadClass}>
              <th scope="col" className="py-2 pr-4 pl-4">
                Service
              </th>
              <th scope="col" className="hidden w-28 py-2 pr-4 text-right md:table-cell">
                p95 CPU
              </th>
              <th scope="col" className="hidden w-28 py-2 pr-4 text-right md:table-cell">
                p95 memory
              </th>
              <th scope="col" className="w-[45%] py-2 pr-4 md:w-[35%]">
                Suggested limit
              </th>
            </tr>
          </thead>
          <tbody>
            {data.services.map((item) => (
              <SizingRow key={item.service} item={item} limit={limits?.[item.service]} />
            ))}
          </tbody>
        </table>
      )}
    </Section>
  );
}

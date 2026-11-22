import { useQuery } from "@tanstack/react-query";
import { useId, useState } from "react";
import { workloadTrafficQuery, type RouteTrafficSummary, type TrafficEntry, type TrafficRange, type WorkloadTraffic } from "../api/outputs";
import { formatAbsolute, formatBytes } from "../lib/format";
import { useNow } from "../lib/hooks";
import { contentKeys, formatMs, statusClass, trafficRanges } from "../lib/outputs";
import { inputClass, tableHeadClass, tableRowClass } from "../lib/styles";
import { RelativeTime } from "./RelativeTime";
import { Select } from "./Select";
import { LoadingRows, Notice, QueryError, Section } from "./ui";

// The collector runs once a minute; allow a few missed passes before calling the data stale.
const staleAfter = 5 * 60_000;

function isTrafficRange(value: string): value is TrafficRange {
  return trafficRanges.some((range) => range.value === value);
}

function RouteTable({ routes, rangeLabel }: { routes: RouteTrafficSummary[]; rangeLabel: string }) {
  return (
    <table className="w-full text-left text-[13px]">
      <caption className="sr-only">Requests per domain, {rangeLabel.toLowerCase()}</caption>
      <thead>
        <tr className={tableHeadClass}>
          <th scope="col" className="pl-4 pr-3">Domain</th>
          <th scope="col" className="pr-3 text-right">Requests</th>
          <th scope="col" className="hidden pr-3 text-right md:table-cell">2xx</th>
          <th scope="col" className="hidden pr-3 text-right md:table-cell">3xx</th>
          <th scope="col" className="hidden pr-3 text-right sm:table-cell">4xx</th>
          <th scope="col" className="pr-3 text-right">5xx</th>
          <th scope="col" className="hidden pr-3 text-right lg:table-cell">Sent</th>
          <th scope="col" className="hidden pr-4 text-right sm:table-cell">p95</th>
        </tr>
      </thead>
      <tbody>
        {routes.map((route) => (
          <tr key={route.domain} className={tableRowClass}>
            <th scope="row" className="py-2 pl-4 pr-3 font-normal text-fg">
              <span className="break-all">{route.domain}</span>
              <span className="block text-xs text-fg-muted sm:hidden">
                4xx {route.status_4xx} · p95 {formatMs(route.p95_ms)}
              </span>
            </th>
            <td className="pr-3 text-right tabular-nums">{route.requests}</td>
            <td className="hidden pr-3 text-right tabular-nums text-fg-muted md:table-cell">{route.status_2xx}</td>
            <td className="hidden pr-3 text-right tabular-nums text-fg-muted md:table-cell">{route.status_3xx}</td>
            <td className={`hidden pr-3 text-right tabular-nums sm:table-cell ${route.status_4xx > 0 ? "text-warn" : "text-fg-muted"}`}>{route.status_4xx}</td>
            <td className={`pr-3 text-right tabular-nums ${route.status_5xx > 0 ? "text-bad" : "text-fg-muted"}`}>{route.status_5xx}</td>
            <td className="hidden pr-3 text-right tabular-nums text-fg-muted lg:table-cell">{formatBytes(route.bytes)}</td>
            <td className="hidden pr-4 text-right tabular-nums text-fg-muted sm:table-cell">{formatMs(route.p95_ms)}</td>
          </tr>
        ))}
      </tbody>
    </table>
  );
}

function EntryTable({ entries }: { entries: TrafficEntry[] }) {
  return (
    <table className="w-full text-left text-[13px]">
      <caption className="sr-only">Most recent requests</caption>
      <thead>
        <tr className={tableHeadClass}>
          <th scope="col" className="pl-4 pr-3">Time</th>
          <th scope="col" className="hidden pr-3 sm:table-cell">Domain</th>
          <th scope="col" className="pr-4 text-right sm:pr-3">Status</th>
          <th scope="col" className="hidden pr-3 text-right md:table-cell">Sent</th>
          <th scope="col" className="hidden pr-3 text-right sm:table-cell">Duration</th>
          <th scope="col" className="hidden pr-4 sm:table-cell">Client</th>
        </tr>
      </thead>
      <tbody>
        {contentKeys(entries, (entry) => `${entry.timestamp}|${entry.domain}|${entry.status}|${entry.client_ip}`).map(({ item: entry, key }) => (
          <tr key={key} className={tableRowClass}>
            <td className="py-2 pl-4 pr-3 text-fg-muted">
              <time dateTime={entry.timestamp} className="whitespace-nowrap tabular-nums">{formatAbsolute(entry.timestamp)}</time>
              <span className="block break-all text-xs sm:hidden">{entry.domain}</span>
              <span className="block font-mono text-xs break-all sm:hidden">{entry.client_ip || "Unknown client"}</span>
            </td>
            <td className="hidden pr-3 break-all sm:table-cell">{entry.domain}</td>
            <td className={`pr-4 text-right tabular-nums sm:pr-3 ${statusClass(entry.status)}`}>{entry.status}</td>
            <td className="hidden pr-3 text-right tabular-nums text-fg-muted md:table-cell">{formatBytes(entry.bytes)}</td>
            <td className="hidden pr-3 text-right tabular-nums text-fg-muted sm:table-cell">{formatMs(entry.duration_ms)}</td>
            <td className="hidden pr-4 font-mono text-xs break-all text-fg-muted sm:table-cell">{entry.client_ip || "Unknown"}</td>
          </tr>
        ))}
      </tbody>
    </table>
  );
}

function isStale(data: WorkloadTraffic, now: number) {
  const collected = data.collected_at ? Date.parse(data.collected_at) : Number.NaN;
  return Number.isFinite(collected) && now - collected > staleAfter;
}

function CollectionNotices({ data, stale }: { data: WorkloadTraffic; stale: boolean }) {
  return (
    <>
      {!data.collected_at && (
        <Notice tone="warn" title="Traffic has not been collected">
          Cove has no traffic report from this service's node. An instance administrator turns on access logging under the node's Proxy configuration; until then requests are not recorded, so an empty table does not mean the service had no visitors.
        </Notice>
      )}
      {data.collected_at && !data.available && (
        <Notice tone="warn" title="Access logs are unavailable on this node">
          The node's last report had no proxy access log to read. Requests after that are missing until access logging works again.
        </Notice>
      )}
      {data.collection_error && <Notice tone="error" title="Latest collection failed">{data.collection_error}</Notice>}
      {stale && data.collected_at && (
        <Notice tone="warn" title="Traffic data is stale">
          Last collected <RelativeTime value={data.collected_at} />. Requests since then are not shown yet.
        </Notice>
      )}
      {data.limited && <Notice tone="warn" title="Some traffic is not shown">Collection or history limits were reached, so some requests or historical routes may be omitted. Counts cover retained observations only.</Notice>}
    </>
  );
}

function TrafficBody({ data, rangeLabel }: { data: WorkloadTraffic; rangeLabel: string }) {
  const now = useNow(30_000);
  const routes = data.routes ?? [];
  const entries = data.entries ?? [];
  const known = data.collected_at !== null;
  const stale = isStale(data, now);
  const noticed = !known || !data.available || data.collection_error !== "" || stale || data.limited;
  return (
    <>
      {noticed && (
        <div className="space-y-2 border-b border-line p-4">
          <CollectionNotices data={data} stale={stale} />
        </div>
      )}
      <div>
        <h3 className="px-4 pt-3 text-[13px] font-semibold text-fg">Requests by domain</h3>
        {routes.length > 0 ? (
          <RouteTable routes={routes} rangeLabel={rangeLabel} />
        ) : (
          <p className="px-4 py-3 text-[13px] text-fg-muted">{known ? `No requests recorded, ${rangeLabel.toLowerCase()}.` : "No traffic data."}</p>
        )}
      </div>
      <div className="border-t border-line">
        <h3 className="px-4 pt-3 text-[13px] font-semibold text-fg">Recent requests</h3>
        <p className="px-4 text-xs text-fg-muted">The 100 most recent requests in the range. Paths, query strings, authorization and other headers are not stored; the client address may come from X-Forwarded-For when the request passed a trusted proxy.</p>
        {entries.length > 0 ? <EntryTable entries={entries} /> : <p className="px-4 py-3 text-[13px] text-fg-muted">No recent requests.</p>}
      </div>
      <p className="border-t border-line px-4 py-3 text-xs text-fg-muted">
        {data.notice} History is kept for {data.retention_days} days.{data.collected_at && <> Last collected <RelativeTime value={data.collected_at} />.</>}
      </p>
    </>
  );
}

export function RouteTraffic({ workloadId }: { workloadId: string }) {
  const [range, setRange] = useState<TrafficRange>("day");
  const rangeId = useId();
  const traffic = useQuery(workloadTrafficQuery(workloadId, range));
  const rangeLabel = trafficRanges.find((option) => option.value === range)?.label ?? "Last 24 hours";
  return (
    <Section
      title="Traffic"
      description="Requests the node's proxy served for this service's domains, read from its access log about once a minute."
      flush
      actions={
        <div className="flex items-center gap-2">
          <label htmlFor={rangeId} className="text-xs text-fg-muted">Range</label>
          <Select id={rangeId} value={range} onChange={(event) => isTrafficRange(event.target.value) && setRange(event.target.value)} className={`${inputClass} w-40`}>
            {trafficRanges.map((option) => <option key={option.value} value={option.value}>{option.label}</option>)}
          </Select>
        </div>
      }
    >
      {traffic.isPending ? (
        <LoadingRows label="Loading traffic" rows={3} />
      ) : traffic.isError && !traffic.data ? (
        <div className="p-4">
          <QueryError what="traffic" error={traffic.error} onRetry={() => void traffic.refetch()} retrying={traffic.isFetching} />
        </div>
      ) : (
        <TrafficBody data={traffic.data} rangeLabel={rangeLabel} />
      )}
    </Section>
  );
}

import { useQuery } from "@tanstack/react-query";
import { getRouteApi } from "@tanstack/react-router";
import { useEffect } from "react";
import { errorMessage, isStatus } from "../api/client";
import { publicStatusQuery, type PublicStatusCheck } from "../api/outputs";
import { RelativeTime } from "../components/RelativeTime";
import { StatusLabel } from "../components/status";
import { Button, LoadingRows, Notice } from "../components/ui";
import { contentKeys, formatMs, formatRatio } from "../lib/outputs";
import { tableHeadClass, tableRowClass } from "../lib/styles";

const route = getRouteApi("/status/$slug");

function checkState(check: PublicStatusCheck) {
  if (check.up === true) return { tone: "ok", label: "Up" } as const;
  if (check.up === false) return { tone: "bad", label: "Down" } as const;
  return { tone: "neutral", label: "Unknown" } as const;
}

function overall(checks: PublicStatusCheck[]) {
  if (checks.length === 0) return null;
  if (checks.some((check) => check.up === false)) return { tone: "bad", label: "Some checks are failing" } as const;
  if (checks.some((check) => check.up === null)) return { tone: "neutral", label: "Some readings are unknown" } as const;
  return { tone: "ok", label: "All checks are passing" } as const;
}

function ChecksTable({ checks }: { checks: PublicStatusCheck[] }) {
  return (
    <table className="w-full text-left text-[13px]">
      <caption className="sr-only">Checks</caption>
      <thead>
        <tr className={tableHeadClass}>
          <th scope="col" className="pl-4 pr-3">Check</th>
          <th scope="col" className="pr-3">Status</th>
          <th scope="col" className="hidden pr-3 text-right sm:table-cell">Latency</th>
          <th scope="col" className="pr-4 text-right">Successful checks, 24 h</th>
        </tr>
      </thead>
      <tbody>
        {contentKeys(checks, (check) => check.label).map(({ item: check, key }) => {
          const state = checkState(check);
          return (
            <tr key={key} className={tableRowClass}>
              <th scope="row" className="py-2 pl-4 pr-3 font-normal break-words text-fg">
                {check.label}
                <span className="block text-xs text-fg-muted">
                  {check.checked_at ? <>Checked <RelativeTime value={check.checked_at} /></> : "No recent check"}
                  <span className="sm:hidden">{check.latency_ms !== null && check.up !== null && ` · ${formatMs(check.latency_ms)}`}</span>
                </span>
              </th>
              <td className="pr-3"><StatusLabel tone={state.tone} label={state.label} /></td>
              <td className="hidden pr-3 text-right tabular-nums text-fg-muted sm:table-cell">{check.latency_ms !== null && check.up !== null ? formatMs(check.latency_ms) : "Unknown"}</td>
              <td className="pr-4 text-right tabular-nums">
                {check.observed_up_ratio_24h === null || check.observed_samples_24h === 0 ? (
                  <span className="text-fg-muted">No samples</span>
                ) : (
                  <>
                    {formatRatio(check.observed_up_ratio_24h)}
                    <span className="block text-xs text-fg-muted">of {check.observed_samples_24h} {check.observed_samples_24h === 1 ? "check" : "checks"}</span>
                  </>
                )}
              </td>
            </tr>
          );
        })}
      </tbody>
    </table>
  );
}

export function PublicStatusPage() {
  const { slug } = route.useParams();
  const status = useQuery({ ...publicStatusQuery(slug), retry: false });
  const title = status.data?.title;
  useEffect(() => {
    document.title = title ? `${title} status` : "Status";
  }, [title]);

  let body;
  if (status.isPending) {
    body = <LoadingRows label="Loading status" rows={3} />;
  } else if (status.isError && !status.data) {
    body = (
      <div className="p-4">
        {isStatus(status.error, 404) ? (
          <Notice tone="warn" title="Status page not found">This status page does not exist or is not published.</Notice>
        ) : (
          <Notice tone="error" title="Status is unavailable" action={<Button onClick={() => void status.refetch()} pending={status.isFetching}>Retry</Button>}>
            {errorMessage(status.error)}
          </Notice>
        )}
      </div>
    );
  } else {
    const checks = status.data.checks ?? [];
    const summary = overall(checks);
    body = (
      <>
        {status.isError && <div className="border-b border-line p-4"><Notice tone="warn" title="Showing the last loaded status">{errorMessage(status.error)}</Notice></div>}
        {summary && <div className="border-b border-line px-4 py-3"><StatusLabel tone={summary.tone} label={summary.label} /></div>}
        {checks.length === 0 ? <p className="px-4 py-4 text-[13px] text-fg-muted">No checks are published on this page.</p> : <ChecksTable checks={checks} />}
      </>
    );
  }

  return (
    <main className="min-h-dvh bg-canvas px-4 py-10 sm:px-6">
      <div className="mx-auto w-full max-w-3xl">
        <h1 className="text-xl font-semibold break-words text-fg">{title ?? "Status"}</h1>
        {status.data && <p className="mt-1 text-xs text-fg-muted">{status.data.source} Ratios count the checks that ran, not continuous availability.</p>}
        <section aria-label="Checks" className="mt-5 min-w-0 overflow-x-auto rounded-lg border border-line bg-surface">{body}</section>
        <p className="mt-4 text-xs text-fg-subtle">Updates every minute.</p>
      </div>
    </main>
  );
}

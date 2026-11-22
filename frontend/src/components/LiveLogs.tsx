import { useQuery, useQueryClient } from "@tanstack/react-query";
import { Eraser, Pause, Play } from "lucide-react";
import { useEffect, useRef, useState } from "react";
import { adviceKeys, fetchLiveLogs, type LiveLogEntry } from "../api/advice";
import { errorMessage, isStatus } from "../api/client";
import { formatAbsolute } from "../lib/format";
import { useOnline } from "../lib/hooks";
import { clearTail, emptyTail, maxLiveEntries, mergeTail, tailContainers, type LiveTail } from "../lib/live-logs";
import { containerColour } from "../lib/telemetry";
import { RelativeTime } from "./RelativeTime";
import { ContainerLogs } from "./Runtime";
import { Select } from "./Select";
import { Button, Notice, Spinner } from "./ui";

const allContainers = "";
const pollInterval = 2_000;
const timeOfDay = new Intl.DateTimeFormat("en", { hour: "2-digit", minute: "2-digit", second: "2-digit", fractionalSecondDigits: 3, hour12: false });

function LiveRow({ entry }: { entry: LiveLogEntry }) {
  const colour = containerColour(entry.container);
  return (
    <li className="grid grid-cols-1 gap-x-3 gap-y-0.5 px-3 py-1 hover:bg-raised md:grid-cols-[6.5rem_11rem_minmax(0,1fr)]">
      <div className="flex min-w-0 flex-wrap items-center gap-x-3 gap-y-0.5 md:contents">
        <time dateTime={entry.timestamp} title={formatAbsolute(entry.timestamp)} className="font-mono text-xs whitespace-nowrap text-fg-muted tabular-nums">
          {timeOfDay.format(Date.parse(entry.timestamp))}
        </time>
        <span className="flex min-w-0 items-center gap-1.5 text-xs text-fg-muted" title={entry.container}>
          <span aria-hidden className={`size-2 shrink-0 rounded-sm ${colour.bg}`} />
          <span className="truncate font-mono">{entry.container}</span>
        </span>
      </div>
      <p className="min-w-0 font-mono text-xs leading-relaxed break-words whitespace-pre-wrap text-fg">
        <span>{entry.message}</span>
        {entry.clipped && <span className="ml-2 font-sans text-[11px] whitespace-nowrap text-warn">Line clipped at 8 KiB</span>}
      </p>
    </li>
  );
}

function PlainTailFallback({ workloadId, supported, error, retrying, onRetry }: { workloadId: string; supported?: boolean; error: unknown; retrying: boolean; onRetry: () => void }) {
  return (
    <div className="space-y-3">
      <Notice
        tone="warn"
        title="Merged live tail needs a newer worker"
        action={
          supported !== false && (
            <Button variant="ghost" size="sm" onClick={onRetry} pending={retrying}>
              Try again
            </Button>
          )
        }
      >
        {supported === false ? "This node's worker does not report structured logs" : errorMessage(error)}. Showing the plain-text tail instead; upgrade the worker and verify the node for per-container timestamps, colours and filters.
      </Notice>
      <ContainerLogs workloadId={workloadId} />
    </div>
  );
}

function tailErrorTitle(error: unknown) {
  if (isStatus(error, 502)) return "Worker unavailable";
  if (isStatus(error, 404)) return "Resource not found";
  return "Could not read live logs";
}

function TailError({ error, paused, keptLines, onRestart }: { error: unknown; paused: boolean; keptLines: boolean; onRestart: () => void }) {
  const unavailable = isStatus(error, 502);
  return (
    <Notice
      tone={unavailable ? "warn" : "error"}
      title={tailErrorTitle(error)}
      action={
        isStatus(error, 400) ? (
          <Button variant="ghost" size="sm" onClick={onRestart}>
            Restart tail
          </Button>
        ) : undefined
      }
    >
      {errorMessage(error)}
      {unavailable && !paused && " Retrying every 2 seconds."}
      {keptLines && " Lines already shown are kept."}
    </Notice>
  );
}

function tailStatus(online: boolean, paused: boolean) {
  if (!online) return "Paused while offline.";
  if (paused) return "Paused. New output is read from where the tail stopped when you resume.";
  return "Reading new output every 2 seconds while this page is visible.";
}

function TailStatus({ online, paused, fetching, sampledAt, shown, total, filtered }: { online: boolean; paused: boolean; fetching: boolean; sampledAt?: string | null; shown: number; total: number; filtered: boolean }) {
  return (
    <p className="flex flex-wrap items-center gap-x-2 gap-y-1 text-xs text-fg-muted" aria-live="polite">
      {fetching && <Spinner className="size-3 text-fg-subtle" />}
      <span>{tailStatus(online, paused)}</span>
      {sampledAt && (
        <span>
          Last read from the worker <RelativeTime value={sampledAt} />.
        </span>
      )}
      {total > 0 && (
        <span>
          {filtered ? `${shown.toLocaleString("en")} of ${total.toLocaleString("en")}` : total.toLocaleString("en")} {total === 1 ? "line" : "lines"}, oldest first.
        </span>
      )}
    </p>
  );
}

function emptyMessage(container: string, total: number, cursor: string | null) {
  if (container) return `No output from ${container} since the tail started.`;
  if (total === 0 && cursor) return "No new output. The containers have not written anything since the tail started or was cleared.";
  return "No output yet.";
}

function useLiveTail(workloadId: string, paused: boolean, supported?: boolean) {
  const client = useQueryClient();
  const key = adviceKeys.liveLogs(workloadId);
  return useQuery({
    queryKey: key,
    queryFn: async ({ signal }) => {
      const previous = client.getQueryData<LiveTail>(key) ?? emptyTail;
      return mergeTail(previous, await fetchLiveLogs(workloadId, previous.cursor, signal));
    },
    refetchInterval: (query) => (paused || isStatus(query.state.error, 409) || isStatus(query.state.error, 404) ? false : pollInterval),
    staleTime: 0,
    gcTime: 0,
    retry: false,
    refetchOnWindowFocus: false,
    enabled: supported !== false,
  });
}

// Keeps the newest line in view until someone scrolls up, and follows again once they scroll back to the bottom.
function TailLines({ entries, empty }: { entries: LiveLogEntry[]; empty: string }) {
  const listRef = useRef<HTMLOListElement>(null);
  const following = useRef(true);
  const newest = entries[entries.length - 1]?.id;

  useEffect(() => {
    const element = listRef.current;
    if (element && following.current) element.scrollTop = element.scrollHeight;
  }, [newest]);

  return (
    <ol
      ref={listRef}
      role="region"
      tabIndex={0}
      aria-label="Live log lines"
      onScroll={(event) => {
        const element = event.currentTarget;
        following.current = element.scrollHeight - element.scrollTop - element.clientHeight < 24;
      }}
      className="max-h-[32rem] min-h-32 divide-y divide-line overflow-y-auto rounded-md border border-line bg-panel"
    >
      {entries.length === 0 ? <li className="px-3 py-6 text-[13px] text-fg-subtle">{empty}</li> : entries.map((entry) => <LiveRow key={entry.id} entry={entry} />)}
    </ol>
  );
}

function TailToolbar({ id, container, containers, onContainer, canClear, onClear, paused, onPause }: { id: string; container: string; containers: string[]; onContainer: (container: string) => void; canClear: boolean; onClear: () => void; paused: boolean; onPause: () => void }) {
  return (
    <div className="flex flex-wrap items-center gap-2">
      <div className="min-w-0 flex-1 basis-40 sm:w-52 sm:flex-none sm:basis-auto">
        <label htmlFor={id} className="sr-only">
          Container
        </label>
        <Select id={id} aria-label="Container" value={container} onChange={(event) => onContainer(event.target.value)}>
          <option value={allContainers}>All containers</option>
          {containers.map((name) => (
            <option key={name} value={name}>
              {name}
            </option>
          ))}
        </Select>
      </div>
      <div className="ml-auto flex flex-wrap items-center gap-2">
        <Button variant="ghost" size="sm" onClick={onClear} disabled={!canClear}>
          <Eraser aria-hidden className="size-3.5" />
          Clear
        </Button>
        <Button size="sm" onClick={onPause} aria-pressed={paused}>
          {paused ? <Play aria-hidden className="size-3.5" /> : <Pause aria-hidden className="size-3.5" />}
          {paused ? "Resume" : "Pause"}
        </Button>
      </div>
    </div>
  );
}

function TailLimits({ limited, trimmed }: { limited: boolean; trimmed: number }) {
  return (
    <>
      {limited && (
        <Notice tone="warn" title="Some lines may be missing">
          The latest read reached the worker&apos;s limits: it catches up at most 5 minutes and 2,000 lines, and clips lines at 8 KiB. Stored log history below keeps lines Cove collected.
        </Notice>
      )}
      {trimmed > 0 && (
        <p className="text-xs text-fg-muted">
          {trimmed.toLocaleString("en")} older {trimmed === 1 ? "line was" : "lines were"} removed to keep the tail at {maxLiveEntries.toLocaleString("en")} lines.
        </p>
      )}
    </>
  );
}

// Merged output from every container of the resource, read from the worker every two seconds while the tab is
// visible, online and not paused. Lines exist only in memory and are dropped when the page closes.
// supported is false when the node is known to run a worker without structured logs, undefined while unknown.
export function LiveLogs({ workloadId, supported }: { workloadId: string; supported?: boolean }) {
  const client = useQueryClient();
  const online = useOnline();
  const [paused, setPaused] = useState(false);
  const [container, setContainer] = useState(allContainers);
  const key = adviceKeys.liveLogs(workloadId);
  const tail = useLiveTail(workloadId, paused, supported);
  const data = tail.data;
  const entries = data?.entries ?? [];
  const shown = container ? entries.filter((entry) => entry.container === container) : entries;

  if (supported === false || (isStatus(tail.error, 409) && !data)) {
    return <PlainTailFallback workloadId={workloadId} supported={supported} error={tail.error} retrying={tail.isFetching} onRetry={() => void tail.refetch()} />;
  }

  return (
    <div className="space-y-3">
      <TailToolbar
        id={`${workloadId}-live-container`}
        container={container}
        containers={tailContainers(entries, container)}
        onContainer={setContainer}
        canClear={entries.length > 0}
        onClear={() => client.setQueryData<LiveTail>(key, (current) => clearTail(current ?? emptyTail))}
        paused={paused}
        onPause={() => setPaused(!paused)}
      />

      <TailStatus online={online} paused={paused} fetching={tail.isFetching} sampledAt={data?.sampledAt} shown={shown.length} total={entries.length} filtered={container !== allContainers} />

      {!online && <Notice tone="warn" title="You are offline">The tail resumes when the connection returns. Output written meanwhile is read back up to 5 minutes.</Notice>}
      {tail.isError && <TailError error={tail.error} paused={paused} keptLines={Boolean(data)} onRestart={() => client.setQueryData<LiveTail>(key, emptyTail)} />}
      {data && <TailLimits limited={data.limited} trimmed={data.trimmed} />}

      {tail.isPending && !tail.isError ? (
        <div role="status" className="flex items-center gap-2 rounded-md border border-line bg-panel px-3 py-6 text-[13px] text-fg-muted">
          <Spinner /> {online ? "Reading the last 30 seconds of output" : "Waiting for the connection"}
        </div>
      ) : (
        data && <TailLines entries={shown} empty={emptyMessage(container, entries.length, data.cursor)} />
      )}
    </div>
  );
}

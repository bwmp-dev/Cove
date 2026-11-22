import { useMutation, useQuery } from "@tanstack/react-query";
import { ChevronLeft, ChevronRight, Download, RotateCw, Search } from "lucide-react";
import { useState, type FormEvent } from "react";
import { errorMessage, isStatus } from "../api/client";
import { downloadLogHistory, logHistoryQuery, type LogCursor, type LogEntry, type LogFilter } from "../api/telemetry";
import { formatAbsolute } from "../lib/format";
import { useOnline } from "../lib/hooks";
import { inputClass } from "../lib/styles";
import { containerColour, logContainers, logSearchTooLong, maxLogSearchBytes, type LogContainer } from "../lib/telemetry";
import { NoMatches } from "./FilterToolbar";
import { RelativeTime } from "./RelativeTime";
import { Select } from "./Select";
import { Button, Notice, QueryError, Spinner } from "./ui";

const allContainers = "";
const timeOfDay = new Intl.DateTimeFormat("en", { month: "short", day: "numeric", hour: "2-digit", minute: "2-digit", second: "2-digit", hour12: false });

function saveText(text: string, filename: string) {
  const url = URL.createObjectURL(new Blob([text], { type: "text/plain;charset=utf-8" }));
  const link = document.createElement("a");
  link.href = url;
  link.download = filename;
  link.click();
  URL.revokeObjectURL(url);
}

function LogRow({ entry }: { entry: LogEntry }) {
  const colour = containerColour(entry.container || entry.container_id);
  return (
    <li className="grid grid-cols-1 gap-x-3 gap-y-0.5 px-3 py-1.5 hover:bg-raised md:grid-cols-[8.5rem_11rem_minmax(0,1fr)]">
      <div className="flex min-w-0 flex-wrap items-center gap-x-3 gap-y-0.5 md:contents">
        <time dateTime={entry.timestamp} title={formatAbsolute(entry.timestamp)} className="font-mono text-xs whitespace-nowrap text-fg-muted tabular-nums">
          {timeOfDay.format(Date.parse(entry.timestamp))}
        </time>
        <span className="flex min-w-0 items-center gap-1.5 text-xs text-fg-muted" title={entry.container_id}>
          <span aria-hidden className={`size-2 shrink-0 rounded-sm ${colour.bg}`} />
          <span className="truncate font-mono">{entry.container || entry.container_id}</span>
        </span>
      </div>
      <p className="min-w-0 font-mono text-xs leading-relaxed break-words whitespace-pre-wrap text-fg">
        <span>{entry.message}</span>
        {entry.clipped && <span className="ml-2 font-sans text-[11px] whitespace-nowrap text-warn">Line clipped at 8 KiB</span>}
      </p>
    </li>
  );
}

export function LogHistory({ workloadId }: { workloadId: string }) {
  const online = useOnline();
  const [search, setSearch] = useState("");
  const [filter, setFilter] = useState<LogFilter>({ q: "", container: allContainers });
  const [selected, setSelected] = useState<LogContainer | null>(null);
  // Cursors of the pages opened so far; the last one is the page on screen and null is the newest page.
  const [cursors, setCursors] = useState<(LogCursor | null)[]>([null]);
  const cursor = cursors[cursors.length - 1] ?? null;
  const history = useQuery(logHistoryQuery(workloadId, filter, cursor));
  const download = useMutation({ mutationFn: () => downloadLogHistory(workloadId, filter, cursor), onSuccess: (text) => saveText(text, "cove-logs.txt"), gcTime: 0 });
  const searchError = logSearchTooLong(search) ? `Search for at most ${maxLogSearchBytes} bytes of text.` : undefined;
  const filtering = filter.q !== "" || filter.container !== allContainers;

  const apply = (next: LogFilter) => {
    setFilter(next);
    setCursors([null]);
    download.reset();
  };
  const submit = (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    if (!searchError) apply({ ...filter, q: search.trim() });
  };
  const reset = () => {
    setSearch("");
    setSelected(null);
    apply({ q: "", container: allContainers });
  };

  const data = history.data;
  const entries = data ? [...data.entries].reverse() : [];
  const containers = logContainers(data?.entries ?? [], selected);
  const page = cursors.length;
  const first = entries[0];
  const last = entries[entries.length - 1];

  return (
    <>
      <div className="flex flex-wrap items-start gap-2 border-b border-line px-3 py-2.5">
        <search aria-label="Search stored logs" className="contents">
          <form onSubmit={submit} className="flex w-full min-w-0 items-start gap-2 sm:w-auto sm:flex-1 sm:basis-72 lg:max-w-md">
            <div className="relative min-w-0 flex-1">
              <label htmlFor={`${workloadId}-log-search`} className="sr-only">
                Search log text
              </label>
              <Search aria-hidden className="pointer-events-none absolute top-1/2 left-2.5 size-3.5 -translate-y-1/2 text-fg-subtle" />
              <input
                id={`${workloadId}-log-search`}
                type="search"
                className={`${inputClass} min-h-9 pl-8 pointer-coarse:min-h-11`}
                placeholder="Search log text"
                value={search}
                onChange={(event) => setSearch(event.target.value)}
                aria-invalid={searchError ? true : undefined}
                aria-describedby={searchError ? `${workloadId}-log-search-error` : undefined}
                autoComplete="off"
                spellCheck={false}
              />
              {searchError && (
                <p id={`${workloadId}-log-search-error`} className="mt-1 text-xs text-bad">
                  {searchError}
                </p>
              )}
            </div>
            <Button type="submit" className="min-h-9" disabled={Boolean(searchError)}>
              Search
            </Button>
          </form>
          <div className="min-w-0 flex-1 basis-40 sm:w-52 sm:flex-none sm:basis-auto">
            <label htmlFor={`${workloadId}-log-container`} className="sr-only">
              Container
            </label>
            <Select
              id={`${workloadId}-log-container`}
              aria-label="Container"
              value={filter.container}
              onChange={(event) => {
                const id = event.target.value;
                const container = containers.find((item) => item.id === id) ?? null;
                setSelected(container);
                apply({ ...filter, container: container ? container.id : allContainers });
              }}
            >
              <option value={allContainers}>All containers</option>
              {containers.map((container) => (
                <option key={container.id} value={container.id}>
                  {container.name}
                </option>
              ))}
            </Select>
          </div>
        </search>
        <div className="ml-auto flex flex-wrap items-center gap-2">
          {filtering && (
            <Button variant="ghost" size="sm" onClick={reset}>
              Reset filters
            </Button>
          )}
          <Button variant="ghost" size="sm" onClick={() => void history.refetch()} disabled={history.isFetching}>
            <RotateCw aria-hidden className="size-3.5" />
            Refresh
          </Button>
          <Button size="sm" onClick={() => download.mutate()} pending={download.isPending} disabled={!online || !data || history.isPlaceholderData || data.entries.length === 0}>
            {!download.isPending && <Download aria-hidden className="size-3.5" />}
            Download this page
          </Button>
        </div>
      </div>

      <div className="space-y-2 px-3 pt-3 empty:hidden">
        {download.isError && (
          <Notice tone="error" title="Could not download this page">
            {errorMessage(download.error)}
          </Notice>
        )}
        {data && !data.collected_at && (
          <Notice title="Not collected yet">Cove has not stored logs for this resource yet. Collection runs about once a minute and needs a worker with telemetry support; upgrade and verify the node if this persists.</Notice>
        )}
        {data?.collection_error && (
          <Notice tone="warn" title="The last collection failed">
            {data.collection_error}
          </Notice>
        )}
        {data?.limited && (
          <Notice tone="warn" title="Some lines may be missing">
            The latest collection reached its limits, so busy containers can have gaps. Cove catches up at most 24 hours and keeps up to 500 lines per container from each collection.
          </Notice>
        )}
        {history.isError && data && (
          <Notice tone="warn" title="Could not refresh">
            Showing the page loaded earlier. {errorMessage(history.error)}
          </Notice>
        )}
      </div>

      {history.isPending ? (
        <div role="status" className="flex items-center gap-2 px-4 py-6 text-[13px] text-fg-muted">
          <Spinner /> Loading stored logs
        </div>
      ) : !data ? (
        <div className="p-3">
          {isStatus(history.error, 503) ? (
            <Notice tone="warn" title="Log history unavailable" action={<Button variant="ghost" size="sm" onClick={() => void history.refetch()} pending={history.isFetching}>Retry</Button>}>
              {errorMessage(history.error)}
            </Notice>
          ) : (
            <QueryError what="stored logs" error={history.error} onRetry={() => void history.refetch()} retrying={history.isFetching} />
          )}
        </div>
      ) : entries.length === 0 ? (
        filtering ? (
          <NoMatches what="log lines" scope={page > 1 ? "No older lines match the search and filters." : "No stored lines match the search and filters. Search is a case-insensitive match on the exact text."} onReset={reset} />
        ) : (
          <p className="px-4 py-6 text-[13px] text-fg-muted">
            {page > 1 ? "No older stored lines." : data.collected_at ? "No stored lines. The containers have not written output since collection started, or retention removed older lines." : "No stored lines yet."}
          </p>
        )
      ) : (
        <div className="pt-3">
          <p className="flex flex-wrap items-center gap-x-2 px-3 pb-2 text-xs text-fg-muted" aria-live="polite">
            {history.isFetching && <Spinner className="size-3 text-fg-subtle" />}
            <span>
              {page === 1 ? "Newest" : `Page ${page},`} {entries.length.toLocaleString("en")} {entries.length === 1 ? "line" : "lines"}
              {first && last && (
                <>
                  {" "}from <time dateTime={first.timestamp}>{formatAbsolute(first.timestamp)}</time> to <time dateTime={last.timestamp}>{formatAbsolute(last.timestamp)}</time>
                </>
              )}
              , oldest first.
            </span>
            {data.collected_at && (
              <span>
                Collected <RelativeTime value={data.collected_at} />.
              </span>
            )}
          </p>
          <ol
            role="region"
            tabIndex={0}
            aria-label="Stored log lines"
            aria-busy={history.isFetching || undefined}
            className="max-h-[40rem] divide-y divide-line overflow-y-auto border-t border-line"
          >
            {entries.map((entry) => (
              <LogRow key={entry.id} entry={entry} />
            ))}
          </ol>
        </div>
      )}

      {data && (page > 1 || data.next_cursor) && (
        <nav aria-label="Log history pages" className="flex flex-wrap items-center justify-end gap-2 border-t border-line px-3 py-2.5">
          {page > 2 && (
            <Button variant="ghost" size="sm" onClick={() => setCursors([null])}>
              Newest
            </Button>
          )}
          <Button size="sm" disabled={page === 1 || history.isFetching} onClick={() => setCursors(cursors.slice(0, -1))}>
            <ChevronLeft aria-hidden className="size-3.5" />
            Newer lines
          </Button>
          <Button size="sm" disabled={!data.next_cursor || history.isFetching} onClick={() => data.next_cursor && setCursors([...cursors, data.next_cursor])}>
            Older lines
            <ChevronRight aria-hidden className="size-3.5" />
          </Button>
        </nav>
      )}
    </>
  );
}

import { useQuery, type UseQueryOptions } from "@tanstack/react-query";
import { RotateCw } from "lucide-react";
import { useEffect, useRef } from "react";
import { errorMessage, isStatus } from "../api/client";
import { operationLogsQuery } from "../api/queries";
import type { OperationLogs } from "../api/types";
import { CopyButton } from "./CopyButton";
import { Button, Notice, Spinner } from "./ui";

interface LogPanelProps<TKey extends readonly unknown[]> {
  options: UseQueryOptions<OperationLogs, Error, OperationLogs, TKey>;
  label: string;
  description: string;
  emptyText: string;
}

export function LogPanel<TKey extends readonly unknown[]>({ options, label, description, emptyText }: LogPanelProps<TKey>) {
  const logs = useQuery(options);
  const ref = useRef<HTMLPreElement>(null);
  const following = useRef(true);
  const text = logs.data?.logs ?? "";

  useEffect(() => {
    const element = ref.current;
    if (element && following.current) element.scrollTop = element.scrollHeight;
  }, [text]);

  const workerUnavailable = logs.isError && isStatus(logs.error, 502);

  return (
    <div className="space-y-3">
      <div className="flex flex-wrap items-center justify-between gap-2">
        <p className="flex items-center gap-2 text-xs text-fg-muted">
          {description}
          {logs.isFetching && <Spinner className="size-3 text-fg-subtle" />}
        </p>
        <div className="flex items-center gap-2">
          {text && !logs.isError && <CopyButton text={text} label="Copy logs" variant="ghost" />}
          <Button variant="ghost" onClick={() => void logs.refetch()} disabled={logs.isFetching}>
            <RotateCw aria-hidden className="size-3.5" />
            Refresh
          </Button>
        </div>
      </div>

      {logs.isError && (
        <Notice tone={workerUnavailable ? "warn" : "error"} title={workerUnavailable ? "Worker unavailable" : "Could not load logs"}>
          {workerUnavailable
            ? "The main node could not reach the worker. Logs stay on the worker and can be read once it is reachable again."
            : errorMessage(logs.error)}
        </Notice>
      )}
      {logs.data?.error && !logs.isError && (
        <Notice tone="warn" title="The worker could not read the logs">
          {logs.data.error}
        </Notice>
      )}

      {logs.isPending ? (
        <div role="status" className="flex items-center gap-2 rounded-md border border-line bg-panel px-3 py-6 text-[13px] text-fg-muted">
          <Spinner /> Loading logs
        </div>
      ) : (
        !logs.isError && (
          <pre
            ref={ref}
            role="region"
            tabIndex={0}
            aria-label={label}
            onScroll={(event) => {
              const element = event.currentTarget;
              following.current = element.scrollHeight - element.scrollTop - element.clientHeight < 24;
            }}
            className="max-h-[32rem] min-h-32 overflow-auto rounded-md border border-line bg-panel p-3 font-mono text-xs leading-relaxed break-words whitespace-pre-wrap text-fg"
          >
            {text || <span className="text-fg-subtle">{emptyText}</span>}
          </pre>
        )
      )}
    </div>
  );
}

export function LogViewer({ operationId, active }: { operationId: string; active: boolean }) {
  return (
    <LogPanel
      options={operationLogsQuery(operationId, active)}
      label="Operation logs"
      description={active ? "Refreshing while the operation is active." : "Trailing output retained by the worker."}
      emptyText={active ? "No output yet." : "No output was recorded."}
    />
  );
}

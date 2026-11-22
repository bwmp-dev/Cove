import { useQuery } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { request } from "../api/client";
import { QueryError, Section } from "./ui";

export function NotificationHistory() {
  const history = useQuery({ queryKey: ["notification-history"], queryFn: ({ signal }) => request<{ operation_id: string; name: string; state: string; attempts: number; error: string }[]>("GET", "/api/notifications/history", undefined, signal), refetchInterval: 15000 });
  return <Section title="Recent deliveries">
    {history.isError ? <QueryError what="delivery history" error={history.error} onRetry={() => void history.refetch()} /> : history.isPending ? <p className="text-sm text-fg-muted">Loading deliveries…</p> : history.data.length === 0 ? <p className="text-sm text-fg-muted">No deliveries yet. New operation results will appear here.</p> : <ul className="divide-y divide-line">{history.data.map((entry) => <li key={`${entry.operation_id}:${entry.name}`} className="py-3 text-sm"><div className="flex flex-wrap justify-between gap-2"><Link to="/operations/$operationId" params={{ operationId: entry.operation_id }} className="font-medium hover:underline">{entry.name}</Link><span>{entry.state} · {entry.attempts} attempts</span></div>{entry.error && <p className="text-xs text-fg-muted">{entry.error}</p>}</li>)}</ul>}
  </Section>;
}

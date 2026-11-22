import { Bell } from "lucide-react";
import { useQuery } from "@tanstack/react-query";
import { request } from "../api/client";
import { NotificationHistory } from "../components/NotificationHistory";
import { Connections } from "../components/Connections";
import { Notice, PageHeader, QueryError, Section, SectionGrid } from "../components/ui";
import { formatAbsolute } from "../lib/format";

type HealthAlert = { kind: string; subject: string; message: string; first_seen: string; checked_at: string; active: boolean };
function HealthAlerts() {
  const query = useQuery({ queryKey: ["health-alerts"], queryFn: () => request<HealthAlert[]>("GET", "/api/notifications/health"), refetchInterval: 30_000 });
  return <Section title="Health alerts" description="Failures are confirmed for two minutes and repeat at most daily. A scheduled backup is overdue after two backup intervals without success. Enable health notifications on a channel to receive alerts and recovery notices.">
    {query.isPending ? <p>Checking health alerts…</p> : query.isError ? <QueryError what="health alerts" error={query.error} onRetry={() => void query.refetch()} retrying={query.isFetching} /> : query.data.length === 0 ? <p className="text-sm text-fg-muted">No reported health alerts. Unavailable telemetry is not proof that a resource is healthy.</p> : <ul className="space-y-3">{query.data.map((alert) => <li key={`${alert.kind}:${alert.subject}`}><Notice tone="warn" title={alert.active ? "Active alert" : "Confirming failure"}>{alert.message}<p className="mt-1 text-xs">Last checked {formatAbsolute(alert.checked_at)}</p></Notice></li>)}</ul>}
  </Section>;
}
export function NotificationsPage() { return <><PageHeader icon={<Bell aria-hidden />} title="Notification settings" description="Choose operation results and health events for your team's webhooks." /><SectionGrid><Connections kind="notifications" /><HealthAlerts /><HealthDeliveryHistory /><NotificationHistory /></SectionGrid></>; }

function HealthDeliveryHistory() {
  const query = useQuery({ queryKey: ["health-deliveries"], queryFn: () => request<{ id: string; name: string; event: string; state: string; attempts: number; error: string; created_at: string }[]>("GET", "/api/notifications/health-history"), refetchInterval: 30_000 });
  return <Section title="Health notification history" description="Latest 50 deliveries; completed history is kept for 30 days.">
    {query.isPending ? <p>Loading deliveries…</p> : query.isError ? <QueryError what="health deliveries" error={query.error} onRetry={() => void query.refetch()} retrying={query.isFetching} /> : <ul className="divide-y divide-line">{query.data.length === 0 && <li className="py-2 text-sm text-fg-muted">No health notifications yet.</li>}{query.data.map((delivery) => <li key={delivery.id} className="py-2 text-sm"><p>{delivery.name} · {delivery.event} · {delivery.state}</p><p className="text-xs text-fg-muted">{formatAbsolute(delivery.created_at)} · {delivery.attempts} attempts{delivery.error ? ` · ${delivery.error}` : ""}</p></li>)}</ul>}
  </Section>;
}

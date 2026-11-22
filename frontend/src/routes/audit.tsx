import { ScrollText } from "lucide-react";
import { useQuery } from "@tanstack/react-query";
import { useState } from "react";
import { request } from "../api/client";
import { FilterSelect, FilterToolbar, NoMatches } from "../components/FilterToolbar";
import { RelativeTime } from "../components/RelativeTime";
import { StatusLabel } from "../components/status";
import { EmptyState, LoadingRows, PageHeader, QueryError, Section } from "../components/ui";
import type { Tone } from "../lib/nodes";
import { managesTeam } from "../lib/permissions";
import { useCurrentTeam, useSession } from "../lib/session";
import { tableHeadClass, tableRowClass } from "../lib/styles";

interface Event {
  id: number;
  actor: string;
  action: string;
  subject: string;
  status: number;
  created_at: string;
}

const results = {
  succeeded: { tone: "ok", label: "Succeeded" },
  rejected: { tone: "warn", label: "Rejected" },
  failed: { tone: "bad", label: "Failed" },
} as const satisfies Record<string, { tone: Tone; label: string }>;

type Result = keyof typeof results;

function result(status: number): Result {
  if (status >= 500) return "failed";
  if (status >= 400) return "rejected";
  return "succeeded";
}

function Events() {
  const events = useQuery({ queryKey: ["audit"], queryFn: ({ signal }) => request<Event[]>("GET", "/api/audit", undefined, signal), refetchInterval: 15000 });
  const [search, setSearch] = useState("");
  const [outcome, setOutcome] = useState("");
  if (events.isPending) return <LoadingRows label="Loading audit events" />;
  if (events.isError) {
    return (
      <div className="p-3">
        <QueryError what="audit events" error={events.error} onRetry={() => void events.refetch()} retrying={events.isFetching} />
      </div>
    );
  }
  if (events.data.length === 0) return <p className="px-4 py-3 text-[13px] text-fg-muted">No actions have been recorded yet.</p>;
  const query = search.trim().toLowerCase();
  const filtering = Boolean(query || outcome);
  const filtered = events.data.filter((event) => {
    if (outcome && result(event.status) !== outcome) return false;
    return !query || [event.actor, event.action, event.subject].some((value) => value.toLowerCase().includes(query));
  });
  const reset = () => {
    setSearch("");
    setOutcome("");
  };
  return (
    <>
      <FilterToolbar
        label="Filter audit events"
        search={search}
        onSearch={setSearch}
        searchPlaceholder="Search actor, action or subject"
        active={filtering}
        onReset={reset}
        summary={filtering ? `${filtered.length} of ${events.data.length} events` : `${events.data.length} events`}
      >
        <FilterSelect id="audit-result" label="Result" value={outcome} onChange={setOutcome}>
          <option value="">All results</option>
          {Object.entries(results).map(([value, { label }]) => (
            <option key={value} value={value}>
              {label}
            </option>
          ))}
        </FilterSelect>
      </FilterToolbar>
      {filtered.length === 0 ? (
        <NoMatches what="events" scope="None of the 100 most recent events match the search and filters." onReset={reset} />
      ) : (
        <table className="w-full table-fixed text-left">
          <caption className="sr-only">Audit events</caption>
          <thead>
            <tr className={tableHeadClass}>
              <th scope="col" className="pr-4 pl-4">
                Action
              </th>
              <th scope="col" className="hidden w-[28%] pr-4 md:table-cell">
                Actor
              </th>
              <th scope="col" className="hidden w-36 pr-4 sm:table-cell">
                Result
              </th>
              <th scope="col" className="w-28 pr-4 text-right">
                When
              </th>
            </tr>
          </thead>
          <tbody>
            {filtered.map((event) => {
              const { tone, label } = results[result(event.status)];
              const status = <StatusLabel tone={tone} label={`${label} · ${event.status}`} />;
              return (
                <tr key={event.id} className={tableRowClass}>
                  <td className="py-2 pr-4 pl-4">
                    <p className="font-mono text-xs break-all text-fg">{event.action}</p>
                    {event.subject && <p className="mt-0.5 text-xs break-all text-fg-muted">{event.subject}</p>}
                    <p className="mt-0.5 text-xs break-all text-fg-muted md:hidden">{event.actor}</p>
                    <p className="mt-1 sm:hidden">{status}</p>
                  </td>
                  <td className="hidden py-2 pr-4 text-[13px] break-all text-fg-muted md:table-cell">{event.actor}</td>
                  <td className="hidden py-2 pr-4 sm:table-cell">{status}</td>
                  <td className="py-2 pr-4 text-right text-[13px] text-fg-muted tabular-nums">
                    <RelativeTime value={event.created_at} />
                  </td>
                </tr>
              );
            })}
          </tbody>
        </table>
      )}
    </>
  );
}

export function AuditPage() {
  const session = useSession();
  const team = useCurrentTeam();
  const allowed = managesTeam(team) || Boolean(session?.is_admin);
  return (
    <>
      <PageHeader icon={<ScrollText aria-hidden />} title="Audit log" description="The 100 most recent recorded actions in this team. Secret values are excluded." />
      {allowed ? (
        <Section title="Activity" description="Search and filters apply to the 100 most recent events." flush>
          <Events />
        </Section>
      ) : (
        <EmptyState title="Team owners and admins only">Ask a team owner or admin if you need to review recorded actions.</EmptyState>
      )}
    </>
  );
}

import { useQuery } from "@tanstack/react-query";
import { RotateCw, Search } from "lucide-react";
import { workloadObservationsQuery } from "../api/queries";
import type { Node, Workload, WorkloadObservations } from "../api/types";
import { formatAbsolute } from "../lib/format";
import { useOnline } from "../lib/hooks";
import { formatDays } from "../lib/threshold-rules";
import { isTransportRoute } from "../lib/route-transports";
import { RelativeTime } from "./RelativeTime";
import { Button, DefinitionList, LoadingRows, MetaList, Notice, QueryError, Section, Subsection } from "./ui";

const description = "Raw values the worker reports for certificate and container alert rules. They are read only when you inspect, before Cove removes events that happened during its own operations, so they are not alert counts.";

function certificateRows(observations: WorkloadObservations, workload: Workload) {
  const expiries = new Map(observations.certificates.map((certificate) => [certificate.domain, certificate.expires_at]));
  const domains = new Set([...(workload.routes ?? []).filter((route) => !isTransportRoute(route)).map((route) => route.domain), ...expiries.keys()]);
  return [...domains].sort().map((domain) => ({ domain, expiresAt: expiries.get(domain) ?? null }));
}

function CertificateExpiry({ expiresAt, sampledAt }: { expiresAt: string | null; sampledAt: string }) {
  if (expiresAt === null) return <span className="text-warn">Unknown. The certificate could not be read, so certificate rules skip this domain.</span>;
  const days = (Date.parse(expiresAt) - Date.parse(sampledAt)) / 86_400_000;
  return (
    <MetaList>
      <span>
        {days > 0 ? "Expires" : "Expired"} <time dateTime={expiresAt}>{formatAbsolute(expiresAt)}</time>
      </span>
      <span className={days > 0 ? undefined : "text-bad"}>{days > 0 ? `${formatDays(Math.floor(days * 10) / 10)} remaining when inspected` : "Expired when inspected"}</span>
    </MetaList>
  );
}

function Observations({ observations, workload }: { observations: WorkloadObservations; workload: Workload }) {
  const certificates = certificateRows(observations, workload);
  return (
    <div className="divide-y divide-line">
      <Subsection>
        <p className="text-xs text-fg-muted">
          Inspected <RelativeTime value={observations.sampled_at} /> ({formatAbsolute(observations.sampled_at)})
        </p>
      </Subsection>
      <Subsection title="Docker events in the ten minutes before inspection">
        {observations.events_available ? (
          <DefinitionList
            compact
            items={[
              { term: "Automatic restarts", value: <span className="tabular-nums">{observations.restart_events}</span> },
              { term: "Out-of-memory", value: <span className="tabular-nums">{observations.oom_events}</span> },
              { term: "Exits", value: <span className="tabular-nums">{observations.crash_events}</span> },
            ]}
          />
        ) : (
          <Notice tone="warn" title="Event counts unknown">Docker events could not be read or exceeded the bounded backlog Cove reads, so container rules skip this sample instead of counting it as zero.</Notice>
        )}
      </Subsection>
      <Subsection title="Current container state">
        {observations.containers.length === 0 ? (
          <p className="text-[13px] text-fg-muted">No containers were found for this service. It may not be deployed yet.</p>
        ) : (
          <ul className="divide-y divide-line" aria-label="Inspected containers">
            {observations.containers.map((container) => {
              const counted = !container.expected_completed && !container.manual_stop && !container.exit_unknown;
              return (
                <li key={container.id} className="space-y-0.5 py-2 first:pt-0 last:pb-0">
                  <p className="break-all font-mono text-xs text-fg" title={container.id}>{container.id.slice(0, 12)}</p>
                  <MetaList className="text-xs text-fg-muted">
                    <span className="tabular-nums">Docker restart count {container.restart_count}</span>
                    <span className={container.exited && counted ? "text-bad" : undefined}>{container.exited ? `Exited with code ${container.exit_code}` : "Not exited"}</span>
                    <span className={container.oom_killed && counted ? "text-bad" : undefined}>{container.oom_killed ? "Stopped by an out-of-memory kill" : "No out-of-memory kill"}</span>
                    {container.expected_completed && <span>One-shot container expected to complete, excluded from alerts</span>}
                    {container.manual_stop && <span>Manual stop (excluded)</span>}
                    {container.exit_unknown && <span className="text-warn">Exit cause unknown (not counted)</span>}
                  </MetaList>
                </li>
              );
            })}
          </ul>
        )}
      </Subsection>
      <Subsection title="Certificates for configured domains">
        {certificates.length === 0 ? (
          <p className="text-[13px] text-fg-muted">This service has no configured domains.</p>
        ) : (
          <ul className="divide-y divide-line" aria-label="Inspected certificates">
            {certificates.map((certificate) => (
              <li key={certificate.domain} className="space-y-0.5 py-2 first:pt-0 last:pb-0">
                <p className="break-all text-[13px] text-fg">{certificate.domain}</p>
                <div className="text-xs text-fg-muted"><CertificateExpiry expiresAt={certificate.expiresAt} sampledAt={observations.sampled_at} /></div>
              </li>
            ))}
          </ul>
        )}
      </Subsection>
    </div>
  );
}

export function AlertObservations({ workload, node }: { workload: Workload; node: Node | undefined }) {
  const online = useOnline();
  const observations = useQuery({ ...workloadObservationsQuery(workload.id), enabled: false });
  const unsupported = node !== undefined && !node.capabilities?.alert_observations;
  const inspected = observations.data !== undefined || observations.isError;
  const action = (
    <Button onClick={() => void observations.refetch()} pending={observations.isFetching} disabled={!online || unsupported}>
      {!observations.isFetching && (inspected ? <RotateCw aria-hidden className="size-3.5" /> : <Search aria-hidden className="size-3.5" />)}
      {inspected ? "Inspect again" : "Inspect"}
    </Button>
  );

  let body;
  if (unsupported) {
    body = <div className="p-4"><Notice tone="warn" title="Worker upgrade needed">{node.name} has not reported support for alert observations. Upgrade the worker on that node and verify it again.</Notice></div>;
  } else if (observations.isError) {
    body = <div className="p-4"><QueryError what="alert observations" error={observations.error} onRetry={() => void observations.refetch()} retrying={observations.isFetching} /></div>;
  } else if (observations.isFetching && !observations.data) {
    body = <LoadingRows label="Inspecting the service" rows={3} />;
  } else if (observations.data) {
    body = <Observations observations={observations.data} workload={workload} />;
  } else {
    body = <p className="p-4 text-[13px] text-fg-muted">Not inspected yet. Inspecting reads current container state, recent Docker events and certificate expiry from the worker without changing anything.</p>;
  }

  return (
    <Section title="Alert observations" description={description} actions={action} flush>
      {body}
      <p className="border-t border-line px-4 py-3 text-xs text-fg-muted">
        Rules skip values that are unknown instead of treating them as zero. Docker keeps a finite event backlog, so counts cover at most the last ten minutes, and a container that exited before then without a recorded cause is shown as exit cause unknown and neither triggers nor clears crash alerts. Stops, kills and restarts made through Docker are treated as manual stops. Alerts ignore events during Cove operations such as deploys, restarts and stops, stay quiet while the service is manually stopped, and exclude completed one-shot containers and jobs.
      </p>
    </Section>
  );
}

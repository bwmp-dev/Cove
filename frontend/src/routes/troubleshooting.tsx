import { useQuery } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { useState } from "react";
import { errorMessage, request } from "../api/client";
import { nodesQuery, workloadOperationsQuery, workloadRuntimeQuery } from "../api/queries";
import { ContainerLogs } from "../components/Runtime";
import { LogViewer } from "../components/LogViewer";
import { Button, Notice, Section } from "../components/ui";
import { containerStatus } from "../lib/containers";
import { actionLabels, isActive, stateLabels } from "../lib/operations";
import { useResource } from "./workload-detail";

export function Troubleshooting() {
 const resource = useResource();
 const nodes = useQuery(nodesQuery);
 const operations = useQuery(workloadOperationsQuery(resource.id));
 const latest = operations.isError ? undefined : operations.data?.find(item => item.workload_id === resource.id);
 const runtime = useQuery(workloadRuntimeQuery(resource.id, Boolean(latest && isActive(latest.state))));
 const backups = useQuery({ queryKey: ["backups", resource.id], queryFn: ({ signal }) => request<{ backups: { id: string; created_at: string; verified_at?: string }[] }>("GET", `/api/workloads/${resource.id}/backups`, undefined, signal) });
 const [logs, setLogs] = useState(false);
 const node = nodes.isError ? undefined : nodes.data?.find(item => item.id === resource.node_id);
 const latestBackup = backups.isError ? undefined : backups.data?.backups.toSorted((a, b) => Date.parse(b.created_at) - Date.parse(a.created_at))[0];
 return <div className="space-y-5">
  <Section title="Troubleshooting" description="Current observations and the last recorded operation. These checks do not execute commands or change the resource.">
   <ul className="divide-y divide-line text-sm">
    <li className="space-y-1 py-3"><p className="font-medium">Intended state</p><p className="text-fg-muted">{resource.manually_stopped ? "Manually stopped. Automatic deployment and scheduled jobs must not start it." : "No manual stop is recorded."}</p></li>
    <li className="space-y-1 py-3"><p className="font-medium">Worker connectivity</p><p className="text-fg-muted">{node ? `${node.name}: ${node.state}. Last checked ${node.last_checked_at ? new Date(node.last_checked_at).toLocaleString() : "never"}.` : nodes.isError ? errorMessage(nodes.error) : "Node status is unavailable."}</p>{node && node.state !== "ready" && <p>Verify the node connection before retrying a deployment.</p>}</li>
    <li className="space-y-1 py-3"><p className="font-medium">Containers</p>{runtime.isError ? <Notice tone="error">Current container state unavailable: {errorMessage(runtime.error)}</Notice> : !runtime.data ? <p className="text-fg-muted">Reading current container state…</p> : runtime.data.containers.length === 0 ? <p>No containers found. Check deployment output and whether the resource has been deployed.</p> : <ul className="space-y-2">{runtime.data.containers.map(container => { const status = containerStatus(container, resource.kind === "job" ? "job" : "service", { manuallyStopped: resource.manually_stopped || (runtime.data.suspension?.reason === "manual_stop" && runtime.data.suspension.phase === "paused"), idleSleeping: runtime.data.suspension?.reason === "idle" && runtime.data.suspension.phase !== "resuming" }); return <li key={container.id} className="break-words"><span className="font-mono text-xs">{container.name}</span>: {status.label} — {container.status}{status.tone === "bad" && <p className="text-bad">Inspect the container logs and its Compose healthcheck. A successful deploy does not prove the application is currently healthy.</p>}</li>; })}</ul>}</li>
    <li className="space-y-1 py-3"><p className="font-medium">Latest operation</p>{operations.isError ? <Notice tone="error">{errorMessage(operations.error)}</Notice> : latest ? <><Link to="/operations/$operationId" params={{ operationId: latest.id }} className="underline">{actionLabels[latest.action]}: {stateLabels[latest.state].toLowerCase()}</Link>{latest.result?.error && <p className="break-words text-bad">{latest.result.error}</p>}</> : <p className="text-fg-muted">No operation recorded.</p>}</li>
    <li className="space-y-1 py-3"><p className="font-medium">Recovery point</p>{backups.isError ? <p className="text-fg-muted">Backup status unavailable: {errorMessage(backups.error)}</p> : latestBackup ? <p>Latest backup: {new Date(latestBackup.created_at).toLocaleString()}. {latestBackup.verified_at ? `Restore verified ${new Date(latestBackup.verified_at).toLocaleString()}.` : "This backup has no successful restore drill recorded."}</p> : <p className="text-fg-muted">No backup snapshot available. Check the Backups page before making data changes.</p>}</li>
   </ul>
   <div className="flex flex-wrap gap-4 border-t border-line pt-3 text-sm"><Link to="/resources/$resourceId/preflight" params={{ resourceId: resource.id }} className="underline">Run deployment preflight</Link><Link to="/resources/$resourceId/configuration-history" params={{ resourceId: resource.id }} className="underline">Inspect configuration changes</Link><Link to="/resources/$resourceId/domains" params={{ resourceId: resource.id }} className="underline">Inspect domains</Link></div>
  </Section>
  <Button onClick={() => setLogs(!logs)}>{logs ? "Hide diagnostic logs" : "Show diagnostic logs"}</Button>
  {logs && <><Section title="Current container logs"><ContainerLogs workloadId={resource.id} /></Section>{latest && <Section title="Latest operation logs"><LogViewer operationId={latest.id} active={isActive(latest.state)} /></Section>}</>}
 </div>;
}

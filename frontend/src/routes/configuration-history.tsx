import { useQuery } from "@tanstack/react-query";
import { request, errorMessage } from "../api/client";
import { Notice, Section } from "../components/ui";
import { useResource } from "./workload-detail";
import { formatAbsolute } from "../lib/format";
import { ReleaseComparison } from "../components/ReleaseComparison";
interface Revision { diffs: { field: string; before: string; after: string }[]; id: string; created_at: string; changes: string[]; baseline: boolean }
export function ConfigurationHistory() {
  const resource = useResource();
  const history = useQuery({ queryKey: ["configuration-history", resource.id], queryFn: ({ signal }) => request<Revision[]>("GET", `/api/workloads/${resource.id}/configuration-history`, undefined, signal), refetchInterval: 30_000 });
  return <>
  <ReleaseComparison workloadId={resource.id} />
  <Section title="Change history" description="The latest 100 configuration revisions. Changed setting names are shown; secret values are never included. This history does not roll back databases or stored data.">
    {history.isPending && <p>Loading history...</p>}
    {history.error && <Notice title="Could not load history" tone="error">{errorMessage(history.error)}</Notice>}
    {history.data && <ol className="divide-y divide-line">{history.data.map((revision) => <li key={revision.id} className="space-y-1 py-3">
      <p className="text-sm font-medium">{revision.baseline ? "Earliest retained configuration" : "Configuration updated"}</p>
      <time className="text-xs text-fg-muted" dateTime={revision.created_at}>{formatAbsolute(revision.created_at)}</time>
      <p className="break-words text-xs text-fg-muted">{revision.baseline ? "Comparison starts with this revision." : revision.changes.length ? revision.changes.join(", ") : "No setting values changed."}</p>
      {!revision.baseline && revision.diffs.map((diff) => <details key={diff.field} className="text-xs">
        <summary className="cursor-pointer py-1">{diff.field}</summary>
        <div className="grid gap-2 sm:grid-cols-2"><div><p className="text-fg-muted">Before</p><pre className="overflow-auto whitespace-pre-wrap break-all rounded border border-line p-2">{diff.before || "Not set"}</pre></div><div><p className="text-fg-muted">After</p><pre className="overflow-auto whitespace-pre-wrap break-all rounded border border-line p-2">{diff.after || "Not set"}</pre></div></div>
      </details>)}
    </li>)}</ol>}
  </Section>
  </>;
}

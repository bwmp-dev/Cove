import { useQuery } from "@tanstack/react-query";
import { ArrowRight, GitCompare } from "lucide-react";
import { useState } from "react";
import { errorMessage, isStatus, request } from "../api/client";
import { workloadOperationsQuery } from "../api/queries";
import { formatAbsolute, shortId } from "../lib/format";
import { activeOperation } from "../lib/operations";
import { tableHeadClass, tableRowClass } from "../lib/styles";
import { Select } from "./Select";
import { StatusLabel } from "./status";
import { Button, Field, LoadingRows, Notice, Section, Subsection } from "./ui";

// Workers before release outcomes were reported send only id and created_at.
interface RetainedRelease { id: string; created_at: string; state?: string; revision?: string }

// Only successful releases can be compared; a release without a reported state comes from an older worker
// and stays selectable so the API can explain that the worker needs upgrading.
function comparable(release: RetainedRelease) {
  return !release.state || release.state === "succeeded";
}

function releaseLabel(release: RetainedRelease) {
  const name = release.revision ? `commit ${release.revision.slice(0, 7)}` : `release ${shortId(release.id)}`;
  return `${formatAbsolute(release.created_at)} · ${name}`;
}

interface ImageChange { service: string; from: string; to: string; changed: boolean }

export interface ReleaseDiff {
  from: string;
  to: string;
  // Changed or unchanged indicators only: the API never returns values, secrets or Compose text.
  configuration: { field: string; changed: boolean }[];
  release: {
    from_revision: string;
    to_revision: string;
    images: ImageChange[] | null;
    commits: { sha: string; subject: string }[] | null;
    commits_complete: boolean;
    commit_message: string;
  };
}

const fieldLabels: Record<string, string> = {
  compose: "Compose file",
  source: "Git source settings",
  "source.credentials": "Git access token",
  limits: "Resource limits",
  routes: "Domains and routes",
  hooks: "Deployment hooks",
  file_secrets: "File secrets",
  private_network: "Private network",
  registry_credentials: "Registry credentials",
  backup_credentials_and_configuration: "Backup credentials and configuration",
  networks: "Docker networks",
  guarded_deploy: "Guarded deployment",
};

function configurationLabel(field: string) {
  if (field.startsWith("variables.")) return `Variable ${field.slice("variables.".length)}`;
  return fieldLabels[field] ?? field;
}

function Revision({ value }: { value: string }) {
  return value ? <span className="font-mono text-xs break-all text-fg" title={value}>{value.slice(0, 12)}</span> : <span className="text-fg-subtle">Not from Git</span>;
}

function Digest({ value }: { value: string }) {
  return value ? <span className="font-mono text-xs break-all">{value}</span> : <span className="text-fg-subtle">Not present</span>;
}

function diffError(error: unknown) {
  if (isStatus(error, 409)) return "These releases cannot be compared. Both must be successful deployments or rollbacks that are still retained, and the worker must be upgraded to a version that supports comparisons.";
  if (isStatus(error, 502)) return "The worker could not compare these releases. Check that it is reachable, no operation is running and the repository is accessible, then try again.";
  return errorMessage(error);
}

function DiffResult({ diff }: { diff: ReleaseDiff }) {
  const changed = diff.configuration.filter((item) => item.changed);
  const unchanged = diff.configuration.filter((item) => !item.changed);
  const images = diff.release.images ?? [];
  const commits = diff.release.commits ?? [];
  return (
    <div className="divide-y divide-line border-t border-line">
      <Subsection title="Git revision" description="The pinned commit each release deployed.">
        <p className="flex flex-wrap items-center gap-2 text-[13px]">
          <Revision value={diff.release.from_revision} />
          <ArrowRight aria-label="to" className="size-3.5 text-fg-subtle" />
          <Revision value={diff.release.to_revision} />
        </p>
        {diff.release.commit_message && <p className="text-xs whitespace-pre-wrap break-words text-fg-muted">{diff.release.commit_message}</p>}
      </Subsection>
      <Subsection title="Commits" description={commits.length ? `${commits.length} commit${commits.length === 1 ? "" : "s"} between the two revisions.` : undefined}>
        {!diff.release.commits_complete && (
          <Notice tone="warn" title="Commit history is incomplete">
            The worker could not list every commit between these revisions, for example because the history is shallow, the repository was unreachable or there are more than 100 commits. The list below may be partial.
          </Notice>
        )}
        {commits.length ? (
          <ul aria-label="Commits" className="divide-y divide-line rounded-md border border-line">
            {commits.map((commit) => (
              <li key={commit.sha} className="flex min-w-0 gap-3 px-3 py-2 text-[13px]">
                <span className="shrink-0 font-mono text-xs text-fg-muted" title={commit.sha}>{commit.sha.slice(0, 7)}</span>
                <span className="min-w-0 break-words text-fg">{commit.subject}</span>
              </li>
            ))}
          </ul>
        ) : (
          <p className="text-[13px] text-fg-muted">{diff.release.commits_complete ? "No commits between these revisions." : "No commits could be listed."}</p>
        )}
      </Subsection>
      <Subsection title="Images" description="Image references each service ran, as recorded by the worker.">
        {images.length ? (
          <div className="overflow-x-auto">
            <table className="w-full min-w-0 table-fixed text-left">
              <caption className="sr-only">Service images</caption>
              <thead>
                <tr className={tableHeadClass}>
                  <th scope="col" className="w-28 pr-3 sm:w-36">Service</th>
                  <th scope="col" className="pr-3">Image</th>
                  <th scope="col" className="w-24 text-right sm:w-28">Change</th>
                </tr>
              </thead>
              <tbody>
                {images.map((image) => (
                  <tr key={image.service} className={tableRowClass}>
                    <td className="py-2 pr-3 align-top text-[13px] break-words text-fg">{image.service}</td>
                    <td className="space-y-1 py-2 pr-3 align-top text-xs text-fg-muted">
                      {image.changed ? (
                        <>
                          <p><span className="text-fg-subtle">From </span><Digest value={image.from} /></p>
                          <p><span className="text-fg-subtle">To </span><Digest value={image.to} /></p>
                        </>
                      ) : (
                        <Digest value={image.to} />
                      )}
                    </td>
                    <td className="py-2 text-right align-top"><StatusLabel tone={image.changed ? "warn" : "neutral"} label={image.changed ? "Changed" : "Same"} /></td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        ) : (
          <p className="text-[13px] text-fg-muted">The worker reported no service images for these releases.</p>
        )}
      </Subsection>
      <Subsection title="Configuration" description="Which saved settings differ. Values, secrets and Compose text are never shown.">
        {changed.length ? (
          <ul aria-label="Changed settings" className="divide-y divide-line rounded-md border border-line">
            {changed.map((item) => (
              <li key={item.field} className="flex min-w-0 items-center justify-between gap-3 px-3 py-2 text-[13px]">
                <span className="min-w-0 break-words text-fg">{configurationLabel(item.field)}</span>
                <StatusLabel tone="warn" label="Changed" />
              </li>
            ))}
          </ul>
        ) : (
          <p className="text-[13px] text-fg-muted">No saved settings differ between these releases.</p>
        )}
        {unchanged.length > 0 && (
          <details className="text-[13px]">
            <summary className="cursor-pointer rounded text-xs text-fg-muted hover:text-fg">{unchanged.length} unchanged setting{unchanged.length === 1 ? "" : "s"}</summary>
            <ul aria-label="Unchanged settings" className="mt-2 grid gap-x-6 gap-y-1 text-xs text-fg-muted sm:grid-cols-2">
              {unchanged.map((item) => <li key={item.field} className="break-words">{configurationLabel(item.field)}</li>)}
            </ul>
          </details>
        )}
      </Subsection>
    </div>
  );
}

function noComparisonMessage(comparableCount: number, retainedCount: number) {
  if (comparableCount === 1) return "Only one successful release is retained. Deploy again to compare releases.";
  if (retainedCount) return "No successful releases are retained. Successful deployments and rollbacks appear here.";
  return "No releases are retained yet. Successful deployments and rollbacks appear here.";
}

function CompareForm({ workloadId, list, busy }: { workloadId: string; list: RetainedRelease[]; busy: boolean }) {
  const [from, setFrom] = useState("");
  const [to, setTo] = useState("");
  const [compared, setCompared] = useState<{ from: string; to: string } | null>(null);
  const diff = useQuery({
    queryKey: ["workloads", workloadId, "release-diff", compared?.from, compared?.to],
    queryFn: ({ signal }) => request<ReleaseDiff>("GET", `/api/workloads/${encodeURIComponent(workloadId)}/release-diff?from=${encodeURIComponent(compared?.from ?? "")}&to=${encodeURIComponent(compared?.to ?? "")}`, undefined, signal),
    enabled: compared !== null,
    retry: false,
    staleTime: 60_000,
  });
  const unreported = list.some((release) => !release.state);
  const options = (other: string) => list.map((release) => <option key={release.id} value={release.id} disabled={release.id === other}>{releaseLabel(release)}</option>);
  const ready = Boolean(from && to && from !== to);

  return (
    <>
      <form
        className="space-y-3 p-4"
        onSubmit={(event) => {
          event.preventDefault();
          if (ready && !busy) setCompared({ from, to });
        }}
      >
        <div className="grid gap-4 sm:grid-cols-2">
          <Field id="release-diff-from" label="From release">
            <Select id="release-diff-from" value={from} onChange={(event) => setFrom(event.target.value)}>
              <option value="">Select a release</option>
              {options(to)}
            </Select>
          </Field>
          <Field id="release-diff-to" label="To release">
            <Select id="release-diff-to" value={to} onChange={(event) => setTo(event.target.value)}>
              <option value="">Select a release</option>
              {options(from)}
            </Select>
          </Field>
        </div>
        {unreported && <p className="text-xs text-fg-muted">The worker did not report whether some releases succeeded, so they are listed without a check. Upgrade the worker to list only successful releases.</p>}
        <div className="flex flex-wrap items-center justify-end gap-x-3 gap-y-2">
          {busy && <p id="release-diff-busy" className="mr-auto text-xs text-fg-muted">An operation is running on this resource. Compare when it finishes.</p>}
          <Button type="submit" disabled={!ready || busy} aria-describedby={busy ? "release-diff-busy" : undefined} pending={diff.isFetching}>
            {!diff.isFetching && <GitCompare aria-hidden className="size-3.5" />}
            Compare
          </Button>
        </div>
      </form>
      {compared && diff.isPending && <div className="border-t border-line"><LoadingRows label="Comparing releases" rows={3} /></div>}
      {compared && diff.isError && (
        <div className="border-t border-line p-4">
          <Notice tone="error" title="Could not compare releases" action={<Button variant="ghost" size="sm" disabled={busy} onClick={() => void diff.refetch()}>Retry</Button>}>{diffError(diff.error)}</Notice>
        </div>
      )}
      {diff.data && <DiffResult diff={diff.data} />}
    </>
  );
}

export function ReleaseComparison({ workloadId }: { workloadId: string }) {
  const releases = useQuery({ queryKey: ["releases", workloadId], queryFn: ({ signal }) => request<{ releases: RetainedRelease[] | null }>("GET", `/api/workloads/${encodeURIComponent(workloadId)}/releases`, undefined, signal) });
  const operations = useQuery(workloadOperationsQuery(workloadId));
  const busy = operations.data ? Boolean(activeOperation(operations.data, workloadId)) : false;
  const retained = releases.data?.releases ?? [];
  // Newest first, so the default reads "older → newer".
  const list = retained.filter(comparable).sort((a, b) => Date.parse(b.created_at) - Date.parse(a.created_at));

  return (
    <Section title="Compare releases" description="Compare any two successful releases still retained on the worker: pinned Git revisions, commits, service images and which settings changed." flush>
      {releases.isPending ? (
        <LoadingRows label="Loading retained releases" rows={2} />
      ) : releases.isError ? (
        <div className="p-4"><Notice tone="error" title="Could not load retained releases">{errorMessage(releases.error)}</Notice></div>
      ) : list.length < 2 ? (
        <p className="p-4 text-[13px] text-fg-muted">{noComparisonMessage(list.length, retained.length)}</p>
      ) : (
        <CompareForm workloadId={workloadId} list={list} busy={busy} />
      )}
    </Section>
  );
}

import { useQuery } from "@tanstack/react-query";
import { getRouteApi, Link } from "@tanstack/react-router";
import { GitBranch, KeyRound, Plus } from "lucide-react";
import { useId, useState, type FormEvent } from "react";
import { errorMessage } from "../api/client";
import { projectsQuery, type Project } from "../api/projects";
import { fetchPreviewCredentials, previewListQuery, previewQuery, recipeQuery, useRequestPreview, type EnvironmentPreview, type PreviewResource } from "../api/previews";
import { CopyButton } from "../components/CopyButton";
import { Dialog } from "../components/ConfirmDialog";
import { PreviewActions } from "../components/PreviewActions";
import { PreviewRecipeEditor } from "../components/PreviewRecipe";
import { RelativeTime } from "../components/RelativeTime";
import { StatusLabel } from "../components/status";
import { Button, DefinitionList, Field, LoadingRows, Notice, QueryError, Section } from "../components/ui";
import { useOnline } from "../lib/hooks";
import { can } from "../lib/permissions";
import { previewStates, shortRevision, stageLabels } from "../lib/previews";
import { inputClass, listLinkClass, tableHeadClass, tableRowClass } from "../lib/styles";

const layoutRoute = getRouteApi("/app/projects/$projectId");

export function ProjectPreviews() {
  const { projectId } = layoutRoute.useParams();
  const projects = useQuery(projectsQuery);
  const project = projects.data?.find((item) => item.id === projectId);
  if (!project) return null;
  return (
    <>
      <PreviewList project={project} />
      {can(project.permissions, "edit") ? (
        <PreviewRecipeEditor project={project} />
      ) : (
        <div className="px-4 pt-5 sm:px-6">
          <Notice>Project editors configure how previews are created.</Notice>
        </div>
      )}
    </>
  );
}

function PreviewList({ project }: { project: Project }) {
  const list = useQuery(previewListQuery(project.id));
  const [requesting, setRequesting] = useState(false);
  const editable = can(project.permissions, "edit");
  return (
    <Section
      title="Environment previews"
      description="One preview per branch name across your repositories, each with its own copy of the production environment and its data."
      flush
      actions={
        editable && (
          <Button onClick={() => setRequesting(true)}>
            <Plus aria-hidden className="size-3.5" />
            New preview
          </Button>
        )
      }
    >
      {list.isPending ? (
        <LoadingRows label="Loading previews" rows={2} />
      ) : list.isError ? (
        <div className="p-4">
          <QueryError what="previews" error={list.error} onRetry={() => void list.refetch()} retrying={list.isFetching} />
        </div>
      ) : (
        <>
          {list.data.waiting.length > 0 && (
            <div className="border-b border-line p-4">
              <Notice tone="warn" title="Branches waiting for a preview">
                <ul className="space-y-0.5">
                  {list.data.waiting.map((branch) => (
                    <li key={`${branch.repository_id}:${branch.branch}`}>
                      <span className="break-all font-mono text-xs text-fg">{branch.branch}</span> · {branch.reason}
                    </li>
                  ))}
                </ul>
              </Notice>
            </div>
          )}
          {list.data.previews.length === 0 ? (
            <p className="px-4 py-3 text-[13px] text-fg-muted">No previews yet. Push a branch that matches the recipe, or create one from an existing branch.</p>
          ) : (
            <ul aria-label={`Previews in ${project.name}`} className="divide-y divide-line">
              {list.data.previews.map((preview) => (
                <PreviewRow key={preview.id} preview={preview} projectId={project.id} />
              ))}
            </ul>
          )}
        </>
      )}
      <Dialog open={requesting} title="New preview" onClose={() => setRequesting(false)}>
        <RequestPreview projectId={project.id} onClose={() => setRequesting(false)} />
      </Dialog>
    </Section>
  );
}

function PreviewRow({ preview, projectId }: { preview: EnvironmentPreview; projectId: string }) {
  const state = previewStates[preview.state];
  return (
    <li className="flex flex-wrap items-start gap-x-6 gap-y-2 px-4 py-3 hover:bg-raised">
      <div className="min-w-0 flex-1 basis-64">
        <div className="flex flex-wrap items-center gap-x-3 gap-y-1">
          {preview.environment_id ? (
            <Link to="/projects/$projectId/environments/$environmentId" params={{ projectId, environmentId: preview.environment_id }} className={`${listLinkClass} break-all text-[13px]`}>
              {preview.slug}
            </Link>
          ) : (
            <span className="break-all text-[13px] font-medium text-fg">{preview.slug}</span>
          )}
          <StatusLabel tone={state.tone} label={state.label} pulse={state.tone === "info"} />
          {preview.pinned && <span className="text-xs text-fg-muted">Kept</span>}
        </div>
        <ul className="mt-1 space-y-0.5 text-xs text-fg-muted">
          {preview.branches.map((branch) => (
            <li key={branch.repository_id} className="flex min-w-0 flex-wrap items-center gap-x-1.5">
              <GitBranch aria-hidden className="size-3 shrink-0 text-fg-subtle" />
              <span className="break-all font-mono">{branch.repository}</span>
              {branch.head ? (
                <span className="font-mono text-fg">{shortRevision(branch.head)}</span>
              ) : (
                <span>branch deleted; runs the pinned production revision</span>
              )}
            </li>
          ))}
        </ul>
        {preview.state !== "ready" && (
          <p className={`mt-1 text-xs ${preview.state === "failed" ? "text-bad" : "text-fg-muted"}`}>{preview.detail || stageLabels[preview.stage]}</p>
        )}
      </div>
      <div className="flex flex-wrap items-center gap-x-4 gap-y-2 text-xs text-fg-muted">
        <span>{preview.state === "deleting" ? `Removing: ${preview.delete_reason}` : preview.expires_at ? <>Removed if inactive <RelativeTime value={preview.expires_at} /></> : "Kept until deleted"}</span>
        <PreviewActions preview={preview} size="sm" />
      </div>
    </li>
  );
}

function RequestPreview({ projectId, onClose }: { projectId: string; onClose: () => void }) {
  const id = useId();
  const online = useOnline();
  const create = useRequestPreview(projectId);
  const recipe = useQuery(recipeQuery(projectId, ""));
  const [suffix, setSuffix] = useState("");
  const prefix = recipe.data?.recipe.branch_pattern.replace(/\*$/, "") ?? "";
  function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    create.mutate(suffix.trim(), { onSuccess: onClose });
  }
  return (
    <form onSubmit={submit} className="space-y-4">
      <Field id={id} label="Branch" hint={`The branch must already exist in at least one selected repository. Creating it here counts as approval.`}>
        <div className="flex min-w-0 items-center rounded-md border border-line-input bg-panel focus-within:border-info focus-within:ring-1 focus-within:ring-info">
          <span className="shrink-0 pl-2.5 font-mono text-[13px] text-fg-muted">{prefix}</span>
          <input id={id} className={`${inputClass} border-0 pl-0.5 font-mono focus:ring-0`} value={suffix} onChange={(event) => setSuffix(event.target.value)} required maxLength={100} autoComplete="off" spellCheck={false} aria-describedby={`${id}-hint`} />
        </div>
      </Field>
      {create.isError && (
        <Notice tone="error" title="Could not request the preview">
          {errorMessage(create.error)}
        </Notice>
      )}
      <div className="flex flex-wrap justify-end gap-2">
        <Button variant="ghost" onClick={onClose} disabled={create.isPending}>
          Cancel
        </Button>
        <Button type="submit" variant="primary" disabled={!online || suffix.trim() === ""} pending={create.isPending}>
          Create preview
        </Button>
      </div>
    </form>
  );
}

// Shown at the top of a preview environment's page: what runs, where its data came from and how to open it.
export function PreviewStatus({ previewId }: { previewId: string }) {
  const preview = useQuery(previewQuery(previewId));
  if (preview.isPending) {
    return (
      <Section title="Preview" flush>
        <LoadingRows label="Loading the preview" rows={2} />
      </Section>
    );
  }
  if (preview.isError) {
    return (
      <div className="px-4 pt-5 sm:px-6">
        <QueryError what="the preview" error={preview.error} onRetry={() => void preview.refetch()} retrying={preview.isFetching} />
      </div>
    );
  }
  const data = preview.data;
  const state = previewStates[data.state];
  const routes = (data.resources ?? []).flatMap((resource) => resource.routes.map((route) => ({ route, published: resource.published })));
  return (
    <Section title={`Preview ${data.slug}`} description="Copied from the production environment when it was created. Production is only read." actions={<PreviewActions preview={data} />} flush>
      <div className="divide-y divide-line">
        <div className="p-4">
          {data.state === "failed" && (
            <div className="mb-4">
              <Notice tone="error" title={`Failed while ${stageLabels[data.stage].toLowerCase()}`}>
                {data.detail}
              </Notice>
            </div>
          )}
          <DefinitionList
            items={[
              { term: "Status", value: <span className="inline-flex flex-wrap items-center gap-x-3 gap-y-1"><StatusLabel tone={state.tone} label={state.label} pulse={state.tone === "info"} />{data.state !== "ready" && data.state !== "failed" && <span className="text-xs text-fg-muted">{stageLabels[data.stage]}</span>}</span> },
              {
                term: "Branches",
                value: (
                  <ul className="space-y-0.5">
                    {data.branches.map((branch) => (
                      <li key={branch.repository_id} className="break-all">
                        <span className="font-mono text-xs">{branch.branch}</span> <span className="text-fg-muted">in</span> <span className="font-mono text-xs">{branch.repository}</span>{" "}
                        {branch.head ? <span className="font-mono text-xs text-fg-muted">{shortRevision(branch.head)}</span> : <span className="text-xs text-fg-muted">(deleted)</span>}
                      </li>
                    ))}
                  </ul>
                ),
              },
              { term: "Removal", value: data.state === "deleting" ? `Removing: ${data.delete_reason}` : data.expires_at ? <>Removed if inactive <RelativeTime value={data.expires_at} /></> : "Kept until deleted" },
              {
                term: "Access",
                value: routes.length === 0 ? <span className="text-fg-muted">No public routes</span> : (
                  <div className="space-y-1.5">
                    <ul className="space-y-0.5">
                      {routes.map(({ route, published }) => (
                        <li key={route} className="break-all">
                          {published ? (
                            <a href={`https://${route}`} target="_blank" rel="noreferrer" className={`${listLinkClass} font-mono text-xs`}>
                              {route}
                            </a>
                          ) : (
                            <span className="font-mono text-xs text-fg-muted">{route}</span>
                          )}
                          {!published && <span className="text-xs text-fg-muted"> · published once every resource is healthy</span>}
                        </li>
                      ))}
                    </ul>
                    {data.access === "private" ? can(data.permissions, "edit") ? <Credentials previewId={data.id} /> : <p className="text-xs text-fg-muted">Password protected. Ask a project editor for the credentials.</p> : <p className="text-xs text-warn">Public: anyone with the address can open it.</p>}
                  </div>
                ),
              },
            ]}
          />
        </div>
        <PreviewResources resources={data.resources ?? []} />
      </div>
    </Section>
  );
}

function Credentials({ previewId }: { previewId: string }) {
  const [credentials, setCredentials] = useState<{ username: string; password: string } | null>(null);
  const [error, setError] = useState("");
  const [pending, setPending] = useState(false);
  async function reveal() {
    setPending(true);
    setError("");
    try {
      setCredentials(await fetchPreviewCredentials(previewId));
    } catch (cause) {
      setError(errorMessage(cause));
    } finally {
      setPending(false);
    }
  }
  if (!credentials) {
    return (
      <div className="flex flex-wrap items-center gap-2">
        <Button size="sm" onClick={() => void reveal()} pending={pending}>
          <KeyRound aria-hidden className="size-3.5" />
          Show credentials
        </Button>
        {error && <span role="alert" className="text-xs text-bad">{error}</span>}
      </div>
    );
  }
  return (
    <div className="space-y-2">
      <p className="text-xs text-fg-muted">
        Username <span className="font-mono text-fg">{credentials.username}</span> · password <span className="break-all font-mono text-fg">{credentials.password}</span>
      </p>
      <div className="flex flex-wrap gap-2">
        <CopyButton text={credentials.password} label="Copy password" />
        <Button variant="ghost" onClick={() => setCredentials(null)}>
          Hide
        </Button>
      </div>
    </div>
  );
}

function PreviewResources({ resources }: { resources: PreviewResource[] }) {
  if (resources.length === 0) {
    return <p className="px-4 py-3 text-[13px] text-fg-muted">Resources are created once production revisions are pinned.</p>;
  }
  return (
    <table className="w-full table-fixed text-[13px]">
      <caption className="sr-only">Preview resources, what they run and where their data came from</caption>
      <thead>
        <tr className={`${tableHeadClass} text-left`}>
          <th scope="col" className="pl-4">Resource</th>
          <th scope="col" className="hidden md:table-cell">Runs</th>
          <th scope="col" className="hidden pr-4 lg:table-cell">Data</th>
        </tr>
      </thead>
      <tbody>
        {resources.map((resource) => (
          <tr key={resource.base_id} className={`${tableRowClass} align-top`}>
            <td className="py-2.5 pl-4 pr-3">
              <Link to="/resources/$resourceId" params={{ resourceId: resource.resource_id }} className={`${listLinkClass} break-words`}>
                {resource.name}
              </Link>
              <p className="text-xs text-fg-muted">{resource.kind === "job" ? "Job, disabled unless scheduled" : resource.deployed ? "Deployed" : "Waiting to deploy"}</p>
              <p className="text-xs text-fg-muted md:hidden"><RunsLabel resource={resource} /></p>
              <p className="text-xs text-fg-muted lg:hidden"><DataLabel resource={resource} /></p>
            </td>
            <td className="hidden py-2.5 pr-3 text-xs text-fg-muted md:table-cell"><RunsLabel resource={resource} /></td>
            <td className="hidden py-2.5 pr-4 text-xs text-fg-muted lg:table-cell"><DataLabel resource={resource} /></td>
          </tr>
        ))}
      </tbody>
    </table>
  );
}

function RunsLabel({ resource }: { resource: PreviewResource }) {
  if (resource.source === "disabled") return <>Not running</>;
  return (
    <>
      {resource.source === "branch" ? "Branch " : "Production "}
      {resource.revision ? <span className="font-mono text-fg">{shortRevision(resource.revision)}</span> : <span>release {shortRevision(resource.baseline_release)}</span>}
      {resource.source === "baseline" && resource.baseline_deployed_at && (
        <>
          {", deployed "}
          <RelativeTime value={resource.baseline_deployed_at} />
        </>
      )}
    </>
  );
}

function DataLabel({ resource }: { resource: PreviewResource }) {
  const seeded = resource.data_report?.seeded;
  const copies = (resource.data_report?.volumes ?? []).filter((volume) => volume.mode === "copy");
  if (resource.data_state === "none") return <>No stored data</>;
  if (resource.data_state === "failed") return <span className="text-bad">Copy failed</span>;
  if (resource.data_state !== "ready") return <>Copying from production…</>;
  return (
    <span className="block space-y-0.5">
      {resource.snapshot_at && (
        <span className="block">
          Database snapshot <RelativeTime value={resource.snapshot_at} />
          {seeded?.verified && seeded.rows !== undefined && `, ${seeded.tables ?? 0} tables and ${seeded.rows} rows verified`}
        </span>
      )}
      {copies.length > 0 && <span className="block">{copies.length} {copies.length === 1 ? "volume" : "volumes"} copied, digests verified</span>}
      {!resource.snapshot_at && copies.length === 0 && "Empty storage"}
    </span>
  );
}


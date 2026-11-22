import { Boxes } from "lucide-react";
import { useState } from "react";
import { TemplateBrowser } from "../components/TemplateBrowser";
import type { ResourceDraft } from "../lib/templates";
import { useQuery } from "@tanstack/react-query";
import { getRouteApi, Link, useNavigate } from "@tanstack/react-router";
import { errorMessage } from "../api/client";
import { useCreateWorkload } from "../api/mutations";
import { projectsQuery } from "../api/projects";
import { nodesQuery } from "../api/queries";
import { WorkloadForm, type ProviderLink } from "../components/WorkloadForm";
import type { WorkloadInput } from "../api/types";
import { Breadcrumbs, Button, LoadingRows, Notice, PageHeader, QueryError } from "../components/ui";
import { saveProviderSource } from "../api/git-providers";
import { findPlacement, type Placement } from "../lib/hierarchy";
import { buttonClass } from "../lib/styles";

const route = getRouteApi("/app/resources/new");

function NewResourceCrumbs({ placement }: { placement: Placement | undefined }) {
  if (!placement) {
    return (
      <Breadcrumbs>
        <Link to="/resources" className="rounded hover:text-fg">
          All resources
        </Link>
        New resource
      </Breadcrumbs>
    );
  }
  return (
    <Breadcrumbs>
      <Link to="/projects" className="rounded hover:text-fg">
        Projects
      </Link>
      <Link to="/projects/$projectId" params={{ projectId: placement.project.id }} className="rounded hover:text-fg">
        {placement.project.name}
      </Link>
      {placement.environment && <Link
        to="/projects/$projectId/environments/$environmentId"
        params={{ projectId: placement.project.id, environmentId: placement.environment.id }}
        className="rounded hover:text-fg"
      >
        {placement.environment.name}
      </Link>}
      New resource
    </Breadcrumbs>
  );
}

export function NewWorkloadPage() {
  const search = route.useSearch();
  const [selection, setSelection] = useState<{ draft?: ResourceDraft } | null>(null);
  // The resource exists once created, so a failed provider connection is reported instead of resubmitting the form.
  const [linking, setLinking] = useState(false);
  const [unlinked, setUnlinked] = useState<{ id: string; message: string } | null>(null);
  const nodes = useQuery(nodesQuery);
  const projects = useQuery(projectsQuery);
  const create = useCreateWorkload();
  const navigate = useNavigate();
  const placement = findPlacement(projects.data, search.environment, search.project);

  async function finish(id: string, input: WorkloadInput, provider?: ProviderLink) {
    if (provider && input.source) {
      setLinking(true);
      try {
        await saveProviderSource(id, { ...provider, branch: input.source.branch, compose_path: input.source.compose_path, ...(input.source.inline_compose ? { inline_compose: true } : {}) });
      } catch (error) {
        setUnlinked({ id, message: errorMessage(error) });
        return;
      } finally {
        setLinking(false);
      }
    }
    void navigate({ to: "/resources/$resourceId", params: { resourceId: id } });
  }

  const breadcrumbs = <NewResourceCrumbs placement={placement} />;
  const cancel = placement?.environment ? (
    <Link
      to="/projects/$projectId/environments/$environmentId"
      params={{ projectId: placement.project.id, environmentId: placement.environment.id }}
      className={buttonClass("ghost")}
    >
      Cancel
    </Link>
  ) : (
    <Link to="/resources" className={buttonClass("ghost")}>
      Cancel
    </Link>
  );

  const failed = nodes.isError && !nodes.data ? nodes : projects.isError && !projects.data ? projects : null;

  return (
    <>
      <PageHeader icon={<Boxes aria-hidden />} breadcrumbs={breadcrumbs} title="New resource" description="Saving does not deploy. Deploy from the resource page when ready." />
      {nodes.isPending || projects.isPending ? (
        <LoadingRows label="Loading nodes and projects" rows={2} />
      ) : failed ? (
        <div className="px-4 py-5 sm:px-6">
          <QueryError what={failed === nodes ? "nodes" : "projects"} error={failed.error} onRetry={() => void failed.refetch()} retrying={failed.isFetching} />
        </div>
      ) : unlinked ? (
        <div className="max-w-3xl px-4 py-5 sm:px-6">
          <Notice
            tone="warn"
            title="Resource created, but the Git provider was not connected"
            action={
              <Link to="/resources/$resourceId/source" params={{ resourceId: unlinked.id }} className={buttonClass("secondary")}>
                Open Source settings
              </Link>
            }
          >
            {unlinked.message} Connect the repository again from the resource's Source page.
          </Notice>
        </div>
      ) : selection === null ? (
        <TemplateBrowser onUse={(draft) => setSelection({ draft })} onCustom={() => setSelection({})} />
      ) : (
        <>
        <div className="px-4 pt-4 sm:px-6"><Button variant="ghost" disabled={create.isPending} onClick={() => { create.reset(); setSelection(null); }}>Back to resource browser</Button></div>
        <WorkloadForm
          draft={selection.draft}
          nodes={nodes.data ?? []}
          projects={projects.data ?? []}
          initialEnvironmentId={placement?.environment?.id ?? ""}
          initialProjectId={placement?.project.id ?? ""}
          pending={create.isPending || linking}
          serverError={create.isError ? errorMessage(create.error) : null}
          submitLabel="Create resource"
          cancel={cancel}
          onSubmit={(input, provider) => create.mutate(input, { onSuccess: ({ id }) => void finish(id, input, provider) })}
        />
        </>
      )}
    </>
  );
}

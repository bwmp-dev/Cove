import { Boxes } from "lucide-react";
import { useRef, useState } from "react";
import { TemplateBrowser } from "../components/TemplateBrowser";
import type { ResourceDraft } from "../api/templates";
import { useQuery } from "@tanstack/react-query";
import { getRouteApi, Link, useNavigate } from "@tanstack/react-router";
import { errorMessage } from "../api/client";
import { useCreateWorkload } from "../api/mutations";
import { projectsQuery } from "../api/projects";
import { nodesQuery } from "../api/queries";
import { WorkloadForm, type ProviderLink } from "../components/WorkloadForm";
import { DatabaseForm } from "../components/DatabaseForm";
import type { WorkloadInput } from "../api/types";
import { Breadcrumbs, Button, LoadingRows, Notice, PageHeader, QueryError } from "../components/ui";
import { saveProviderSource, type ProviderSourceInput } from "../api/git-providers";
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
  const [selection, setSelection] = useState<{ draft?: ResourceDraft; database?: true } | null>(null);
  // The resource exists once created, so a failed provider connection is reported instead of resubmitting the form.
  const [linking, setLinking] = useState(false);
  const [unlinked, setUnlinked] = useState<{ id: string; message: string } | null>(null);
  const nodes = useQuery(nodesQuery);
  const projects = useQuery(projectsQuery);
  // The input of the creation in progress; held outside the mutation so routes, typed header values, forward-auth URLs
  // and the source token never become mutation variables.
  const submitted = useRef<WorkloadInput | null>(null);
  const create = useCreateWorkload(() => {
    if (!submitted.current) throw new Error("No resource to create.");
    return submitted.current;
  });
  const navigate = useNavigate();
  const placement = findPlacement(projects.data, search.environment, search.project);

  function submit(input: WorkloadInput, provider?: ProviderLink) {
    const link = provider && input.source ? { ...provider, branch: input.source.branch, compose_path: input.source.compose_path, ...(input.source.inline_compose ? { inline_compose: true } : {}) } : undefined;
    submitted.current = input;
    create.mutate(undefined, {
      onSuccess: ({ id }) => void finish(id, link),
      onSettled: () => {
        submitted.current = null;
      },
    });
  }

  async function finish(id: string, link?: ProviderSourceInput) {
    if (link) {
      setLinking(true);
      try {
        await saveProviderSource(id, link);
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
        <TemplateBrowser onUse={(draft) => setSelection({ draft })} onCustom={() => setSelection({})} onDatabase={() => setSelection({ database: true })} />
      ) : selection.database ? (
        <>
          <div className="px-4 pt-4 sm:px-6"><Button variant="ghost" onClick={() => setSelection(null)}>Back to resource browser</Button></div>
          <DatabaseForm
            nodes={nodes.data ?? []}
            projects={projects.data ?? []}
            initialEnvironmentId={placement?.environment?.id ?? ""}
            initialProjectId={placement?.project.id ?? ""}
            cancel={cancel}
            onCreated={(id) => void navigate({ to: "/resources/$resourceId", params: { resourceId: id } })}
          />
        </>
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
          onSubmit={submit}
        />
        </>
      )}
    </>
  );
}

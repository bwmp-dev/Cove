import { SharedVariables } from "../components/SharedVariables";
import { DeploymentPolicy } from "../components/DeploymentPolicy";
import { DependencyMap } from "../components/DependencyMap";
import { EnvironmentResources } from "../components/PrivateConnections";
import { useQuery } from "@tanstack/react-query";
import { getRouteApi, Link, Outlet, useNavigate, useParams } from "@tanstack/react-router";
import { FileCog, Folder, GitBranch, Layers, Plus } from "lucide-react";
import { createContext, Fragment, use, useId, useMemo, useState, type FormEvent, type ReactNode } from "react";
import { errorMessage } from "../api/client";
import { projectsQuery, useCreateEnvironment, useDeleteEnvironment, useDeleteProject, type Environment, type Project } from "../api/projects";
import { workloadsQuery } from "../api/queries";
import type { WorkloadSummary } from "../api/types";
import { ActionMenu, type MenuAction } from "../components/ActionMenu";
import { DeploymentPlan } from "../components/DeploymentPlan";
import { CloneForm } from "../components/Clone";
import { ConfirmDialog, Dialog } from "../components/ConfirmDialog";
import { PlacementRow } from "../components/EnvironmentList";
import { Notes } from "../components/Notes";
import { PinButton } from "../components/PinButton";
import { ProjectCosts } from "../components/ProjectCosts";
import { StatusPageSettings } from "../components/StatusPageSettings";
import { Breadcrumbs, Button, EmptyState, Field, LoadingRows, MetaList, Notice, PageHeader, QueryError, Section, SectionGrid, TabNav } from "../components/ui";
import { canCompareEnvironments, canExportConfiguration, countLabel, nodeNames, previewEnvironments, productionEnvironments, resourcesIn, suggestedEnvironments } from "../lib/hierarchy";
import { useOnline } from "../lib/hooks";
import { can, canCreateProjects } from "../lib/permissions";
import { useSession } from "../lib/session";
import { buttonClass, inputClass, tabLinkClass } from "../lib/styles";
import { PreviewStatus } from "./project-previews";

interface ProjectContextValue {
  project: Project;
  resources: WorkloadSummary[];
}

const ProjectContext = createContext<ProjectContextValue | null>(null);

export function useProject() {
  const value = use(ProjectContext);
  if (!value) throw new Error("Project pages must render inside the project layout.");
  return value;
}

const layoutRoute = getRouteApi("/app/projects/$projectId");
const environmentRoute = getRouteApi("/app/projects/$projectId/environments/$environmentId");

const offline = "Unavailable while offline.";

function NewResourceLink({ projectId, environmentId, variant = "primary" }: { projectId: string; environmentId?: string; variant?: "primary" | "secondary" }) {
  return (
    <Link to="/resources/new" search={{ project: projectId, environment: environmentId }} className={buttonClass(variant)}>
      <Plus aria-hidden className="size-3.5" />
      New resource
    </Link>
  );
}

export function ProjectLayout() {
  const { projectId } = layoutRoute.useParams();
  const { environmentId } = useParams({ strict: false });
  const projects = useQuery(projectsQuery);
  const workloads = useQuery(workloadsQuery);
  const crumbsRoot = <Link to="/projects" className="rounded hover:text-fg">Projects</Link>;

  if (projects.isPending || workloads.isPending) return <LoadingRows label="Loading project" />;
  if (projects.isError || workloads.isError) {
    const failed = projects.isError ? projects : workloads;
    return (
      <div className="px-4 pt-5 sm:px-6">
        <QueryError what={projects.isError ? "the project" : "resources"} error={failed.error} onRetry={() => void failed.refetch()} retrying={failed.isFetching} />
      </div>
    );
  }
  const project = projects.data.find((item) => item.id === projectId);
  if (!project) {
    return (
      <>
        <PageHeader icon={<Folder aria-hidden />} breadcrumbs={<Breadcrumbs>{crumbsRoot}Not found</Breadcrumbs>} title="Project not found" />
        <EmptyState title="This project does not exist">It may have been deleted, or the link is incorrect.</EmptyState>
      </>
    );
  }

  return <ProjectFrame project={project} workloads={workloads.data} environmentId={environmentId} crumbsRoot={crumbsRoot} />;
}

function CreateEnvironmentForm({ project, onClose }: { project: Project; onClose: () => void }) {
  const id = useId();
  const online = useOnline();
  const navigate = useNavigate();
  const create = useCreateEnvironment();
  const [name, setName] = useState("");
  const existing = new Set(project.environments.map((environment) => environment.name));
  const suggestions = suggestedEnvironments.filter((suggestion) => !existing.has(suggestion));

  function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    const trimmed = name.trim();
    if (trimmed === "") return;
    create.mutate(
      { projectId: project.id, name: trimmed },
      {
        onSuccess: ({ id: environmentId }) => {
          onClose();
          void navigate({ to: "/projects/$projectId/environments/$environmentId", params: { projectId: project.id, environmentId } });
        },
      },
    );
  }

  return (
    <form onSubmit={submit} className="space-y-3">
      <Field id={`${id}-environment`} label="Environment name" hint={suggestions.length > 0 ? `For example ${suggestions.join(", ")}.` : undefined}>
        <input
          id={`${id}-environment`}
          className={inputClass}
          list={`${id}-suggestions`}
          value={name}
          onChange={(event) => setName(event.target.value)}
          required
          maxLength={120}
          autoComplete="off"
          aria-describedby={suggestions.length > 0 ? `${id}-environment-hint` : undefined}
        />
      </Field>
      <datalist id={`${id}-suggestions`}>
        {suggestions.map((suggestion) => (
          <option key={suggestion} value={suggestion} />
        ))}
      </datalist>
      {create.isError && (
        <Notice tone="error" title="Could not create the environment">
          {errorMessage(create.error)}
        </Notice>
      )}
      <div className="flex flex-wrap gap-2">
        <Button type="submit" variant="primary" disabled={!online || name.trim() === ""} pending={create.isPending}>
          Create environment
        </Button>
        <Button variant="ghost" onClick={onClose} disabled={create.isPending}>
          Cancel
        </Button>
      </div>
    </form>
  );
}

function deleteReason(resources: number, online: boolean) {
  if (resources > 0) return `Move or delete its ${countLabel(resources, "resource")} first.`;
  return online ? undefined : offline;
}

function ProjectActions({ project, resources }: { project: Project; resources: number }) {
  const online = useOnline();
  const navigate = useNavigate();
  const deletion = useDeleteProject();
  const canClone = canCreateProjects(useSession());
  const [dialog, setDialog] = useState<"clone" | "delete" | null>(null);
  const close = () => {
    deletion.reset();
    setDialog(null);
  };
  const actions: MenuAction[] = [
    ...(canClone ? [{ label: "Clone project", onSelect: () => setDialog("clone"), disabledReason: online ? undefined : offline }] : []),
    { label: "Delete project", danger: true, onSelect: () => setDialog("delete"), disabledReason: deleteReason(resources, online) },
  ];
  return (
    <>
      <ActionMenu label="Project actions" actions={actions} />
      <Dialog open={dialog === "clone"} title={`Clone ${project.name}`} onClose={close}>
        <CloneForm kind="project" id={project.id} name={project.name} onCancel={close} />
      </Dialog>
      <ConfirmDialog
        open={dialog === "delete"}
        title={`Delete ${project.name}?`}
        confirmLabel="Delete project"
        danger
        pending={deletion.isPending}
        onCancel={close}
        onConfirm={() => deletion.mutate(project.id, { onSuccess: () => void navigate({ to: "/projects" }) })}
      >
        <p>The project and its empty environments are removed. Nodes are not affected.</p>
        {deletion.isError && (
          <div className="mt-3">
            <Notice tone="error" title="Could not delete the project">
              {errorMessage(deletion.error)}
            </Notice>
          </div>
        )}
      </ConfirmDialog>
    </>
  );
}

function EnvironmentActions({ project, environment, resources }: { project: Project; environment: Environment; resources: number }) {
  const online = useOnline();
  const navigate = useNavigate();
  const deletion = useDeleteEnvironment();
  const [dialog, setDialog] = useState<"clone" | "delete" | null>(null);
  const close = () => {
    deletion.reset();
    setDialog(null);
  };
  return (
    <>
      <ActionMenu
        label={`${environment.name} environment actions`}
        actions={[
          { label: "Clone environment", onSelect: () => setDialog("clone"), disabledReason: online ? undefined : offline },
          { label: "Delete environment", danger: true, onSelect: () => setDialog("delete"), disabledReason: deleteReason(resources, online) },
        ]}
      />
      <Dialog open={dialog === "clone"} title={`Clone ${environment.name}`} onClose={close}>
        <CloneForm kind="environment" id={environment.id} name={environment.name} projectId={project.id} onCancel={close} />
      </Dialog>
      <ConfirmDialog
        open={dialog === "delete"}
        title={`Delete ${environment.name}?`}
        confirmLabel="Delete environment"
        danger
        pending={deletion.isPending}
        onCancel={close}
        onConfirm={() =>
          deletion.mutate(environment.id, {
            onSuccess: () => {
              setDialog(null);
              void navigate({ to: "/projects/$projectId", params: { projectId: project.id } });
            },
          })
        }
      >
        <p>The empty {environment.name} environment is removed from {project.name}. Nodes are not affected.</p>
        {deletion.isError && (
          <div className="mt-3">
            <Notice tone="error" title="Could not delete the environment">
              {errorMessage(deletion.error)}
            </Notice>
          </div>
        )}
      </ConfirmDialog>
    </>
  );
}

function ProjectFrame({ project, workloads, environmentId, crumbsRoot }: { project: Project; workloads: WorkloadSummary[]; environmentId: string | undefined; crumbsRoot: ReactNode }) {
  const context = useMemo(() => ({ project, resources: resourcesIn(workloads, project.id) }), [project, workloads]);
  const { resources } = context;
  const [creating, setCreating] = useState(false);
  const current = project.environments.find((environment) => environment.id === environmentId);
  const environments = productionEnvironments(project);
  const previews = previewEnvironments(project);
  const nodes = nodeNames(resources);
  const canEditProject = can(project.permissions, "edit");
  // Preview environments hold exactly the copies their recipe creates.
  const canCreate = current ? can(current.permissions, "edit") && !current.preview_id : canEditProject;
  const previewsLink = (
    <Link to="/projects/$projectId/previews" params={{ projectId: project.id }} className="rounded hover:text-fg">
      Previews
    </Link>
  );

  return (
    <ProjectContext value={context}>
      <PageHeader
        breadcrumbs={
          <Breadcrumbs>
            {crumbsRoot}
            {current ? (
              <Link to="/projects/$projectId" params={{ projectId: project.id }} className="rounded hover:text-fg">
                {project.name}
              </Link>
            ) : (
              project.name
            )}
            {current?.preview_id && previewsLink}
            {current && current.name}
          </Breadcrumbs>
        }
        icon={<Folder className="size-4" />}
        title={project.name}
        description={
          <MetaList>
            <span key="environments">{countLabel(environments.length, "environment")}</span>
            {previews.length > 0 && <span key="previews">{countLabel(previews.length, "preview")}</span>}
            <span key="resources">{countLabel(resources.length, "resource")}</span>
            <span key="nodes">{countLabel(nodes.length, "node")} in use</span>
          </MetaList>
        }
        actions={
            <>
              {current ? <PinButton kind="environment" id={current.id} name={current.name} /> : <PinButton kind="project" id={project.id} name={project.name} />}
              {canEditProject && (
                <Button onClick={() => setCreating(true)}>
                  <Layers aria-hidden className="size-3.5" />
                  New environment
                </Button>
              )}
              {canCreate && <NewResourceLink projectId={project.id} environmentId={current?.id} />}
              {canEditProject && <DeploymentPlan projectId={project.id} resources={resources} />}
              {canEditProject && <ProjectActions project={project} resources={resources.length} />}
            </>
        }
        tabs={<ProjectTabs project={project} resources={resources} inPreview={!!current?.preview_id} />}
      />
      <Dialog open={creating} title={`New environment in ${project.name}`} onClose={() => setCreating(false)}>
        <CreateEnvironmentForm project={project} onClose={() => setCreating(false)} />
      </Dialog>
      <Outlet />
    </ProjectContext>
  );
}

function ProjectTabs({ project, resources, inPreview }: { project: Project; resources: WorkloadSummary[]; inPreview: boolean }) {
  const previews = previewEnvironments(project);
  return (
    <TabNav label="Project sections">
      <Link to="/projects/$projectId" params={{ projectId: project.id }} activeOptions={{ exact: true }} className={tabLinkClass}>
        Overview
      </Link>
      {productionEnvironments(project).map((environment) => (
        <Link key={environment.id} to="/projects/$projectId/environments/$environmentId" params={{ projectId: project.id, environmentId: environment.id }} className={tabLinkClass}>
          <Layers aria-hidden className="size-3.5" />
          {environment.name}
          <span className="text-xs font-normal text-fg-subtle tabular-nums">{resources.filter((resource) => resource.environment_id === environment.id).length}</span>
        </Link>
      ))}
      <Link
        to="/projects/$projectId/previews"
        params={{ projectId: project.id }}
        // A preview environment's page belongs to the Previews tab.
        className={`${tabLinkClass} aria-[current=page]:border-fg aria-[current=page]:font-medium aria-[current=page]:text-fg`}
        aria-current={inPreview ? "page" : undefined}
      >
        <GitBranch aria-hidden className="size-3.5" />
        Previews
        {previews.length > 0 && <span className="text-xs font-normal text-fg-subtle tabular-nums">{previews.length}</span>}
      </Link>
      {(can(project.permissions, "read") || canCompareEnvironments(project) || canExportConfiguration(project)) && (
        <Link to="/projects/$projectId/configuration" params={{ projectId: project.id }} className={tabLinkClass}>
          <FileCog aria-hidden className="size-3.5" />
          Configuration
        </Link>
      )}
    </TabNav>
  );
}

export function ProjectOverview() {
  const { project, resources } = useProject();
  const shared = resources.filter((resource) => !resource.environment_id);
  const editable = can(project.permissions, "edit");

  return (
    <>
      <Section title="Environments" description="Each environment holds its own resources, which can run on different nodes." flush>
        {productionEnvironments(project).length === 0 ? (
          <p className="px-4 py-3 text-[13px] text-fg-muted">Environments are optional. Add one to separate production from staging or development.</p>
        ) : (
          <ul aria-label={`Environments in ${project.name}`} className="divide-y divide-line">
            {productionEnvironments(project).map((environment) => {
              const inEnvironment = resources.filter((resource) => resource.environment_id === environment.id);
              return (
                <PlacementRow
                  key={environment.id}
                  projectId={project.id}
                  environmentId={environment.id}
                  name={environment.name}
                  resources={inEnvironment}
                  actions={editable && <EnvironmentActions project={project} environment={environment} resources={inEnvironment.length} />}
                />
              );
            })}
          </ul>
        )}
      </Section>
      {can(project.permissions, "read") && (
        <EnvironmentResources
          projectId={project.id}
          title="Project resources"
          description="Shared across environments. Connect them to environment resources with private connections."
          resources={shared}
          canCreate={false}
          empty={
            <div className="flex flex-wrap items-center justify-between gap-3 px-4 py-3">
              <p className="text-[13px] text-fg-muted">No project resources yet.</p>
              {editable && <NewResourceLink projectId={project.id} variant="secondary" />}
            </div>
          }
        />
      )}
      {editable && (
        <SectionGrid>
          <SharedVariables scope="project" id={project.id} collapsible />
          <DeploymentPolicy scope="project" id={project.id} />
        </SectionGrid>
      )}
      {can(project.permissions, "read") && <ProjectCosts projectId={project.id} />}
      {editable && <StatusPageSettings projectId={project.id} />}
      {can(project.permissions, "read") && <DependencyMap projectId={project.id} />}
      {can(project.permissions, "read") && <Notes key={project.id} scope="project" id={project.id} canEdit={editable} />}
    </>
  );
}

export function EnvironmentPage() {
  const { environmentId } = environmentRoute.useParams();
  const { project, resources } = useProject();
  const environment = project.environments.find((item) => item.id === environmentId);
  if (!environment) {
    return <EmptyState title="This environment does not exist">It may have been deleted, or the link is incorrect.</EmptyState>;
  }
  const inEnvironment = resources.filter((resource) => resource.environment_id === environment.id);
  if (environment.preview_id) {
    return (
      <Fragment key={environment.id}>
        <PreviewStatus previewId={environment.preview_id} />
        <EnvironmentResources
          projectId={project.id}
          environmentId={environment.id}
          title="Preview resources"
          description="The preview's own copies and the private connections and shared networks between them. Nothing here connects to production."
          resources={inEnvironment}
          canCreate={false}
          managedConnections
          empty={<p className="px-4 py-3 text-[13px] text-fg-muted">Resources appear once the preview pins production revisions.</p>}
        />
      </Fragment>
    );
  }
  // One key for the whole page resets every section's local state on a switch. Keying the
  // siblings individually with the same value made React keep the old section mounted each time.
  return (
    <Fragment key={environment.id}>
      <EnvironmentResources
        projectId={project.id}
        environmentId={environment.id}
        title={`Resources in ${environment.name}`}
        description="Compose services and jobs, where each runs, and the private connections between them."
        resources={inEnvironment}
        canCreate={false}
        actions={can(project.permissions, "edit") && <EnvironmentActions project={project} environment={environment} resources={inEnvironment.length} />}
        empty={
          <div className="flex flex-wrap items-center justify-between gap-3 px-4 py-3">
            <p className="text-[13px] text-fg-muted">No resources in {environment.name} yet.</p>
            {can(environment.permissions, "edit") && <NewResourceLink projectId={project.id} environmentId={environment.id} variant="secondary" />}
          </div>
        }
      />
      {can(environment.permissions, "edit") && (
        <SectionGrid>
          <SharedVariables scope="environment" id={environment.id} collapsible />
          <DeploymentPolicy scope="environment" id={environment.id} />
        </SectionGrid>
      )}
      {can(project.permissions, "read") && <ProjectCosts projectId={project.id} environmentId={environment.id} />}
    </Fragment>
  );
}

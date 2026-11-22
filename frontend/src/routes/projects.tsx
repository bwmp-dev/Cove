import { useQuery } from "@tanstack/react-query";
import { Link, useNavigate } from "@tanstack/react-router";
import { Folder, Plus } from "lucide-react";
import { useId, useState, type FormEvent, type ReactNode } from "react";
import { errorMessage } from "../api/client";
import { projectsQuery, useCreateEnvironment, useCreateProject, type Project } from "../api/projects";
import { nodesQuery, workloadsQuery } from "../api/queries";
import type { WorkloadSummary } from "../api/types";
import { Checkbox } from "../components/Checkbox";
import { PlacementRow } from "../components/EnvironmentList";
import { Button, EmptyState, Field, LoadingRows, Notice, PageHeader, QueryError, Section } from "../components/ui";
import { countLabel, resourcesIn, productionEnvironments, previewEnvironments } from "../lib/hierarchy";
import { useOnline } from "../lib/hooks";
import { canCreateProjects } from "../lib/permissions";
import { useSession } from "../lib/session";
import { buttonClass, inputClass, linkClass } from "../lib/styles";

function CreateProject({ onClose }: { onClose: () => void }) {
  const id = useId();
  const online = useOnline();
  const navigate = useNavigate();
  const project = useCreateProject();
  const environment = useCreateEnvironment();
  const [name, setName] = useState("");
  const [withProduction, setWithProduction] = useState(true);

  function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    const trimmed = name.trim();
    if (trimmed === "") return;
    project.mutate(trimmed, {
      onSuccess: ({ id: projectId }) => {
        if (!withProduction) {
          void navigate({ to: "/projects/$projectId", params: { projectId } });
          return;
        }
        environment.mutate(
          { projectId, name: "production" },
          {
            onSuccess: ({ id: environmentId }) =>
              void navigate({ to: "/projects/$projectId/environments/$environmentId", params: { projectId, environmentId } }),
          },
        );
      },
    });
  }

  const createdId = project.data?.id;
  return (
    <Section width="form" title="New project" description="A project groups the resources of one product or system. Add environments to separate production from staging or development.">
      {createdId && environment.isError ? (
        <Notice
          tone="warn"
          title="Project created without an environment"
          action={
            <Link to="/projects/$projectId" params={{ projectId: createdId }} className={buttonClass("secondary")}>
              Open project
            </Link>
          }
        >
          The production environment could not be created: {errorMessage(environment.error)} Add it from the project page.
        </Notice>
      ) : (
        <form className="max-w-lg space-y-4" onSubmit={submit}>
          <Field id={`${id}-name`} label="Project name">
            <input
              id={`${id}-name`}
              className={inputClass}
              value={name}
              onChange={(event) => setName(event.target.value)}
              required
              pattern=".*\S.*"
              title="Enter a project name"
              maxLength={120}
              autoComplete="off"
            />
          </Field>
          <label className="flex items-start gap-2.5 text-[13px] text-fg">
            <Checkbox checked={withProduction} onChange={(event) => setWithProduction(event.target.checked)} />
            <span>
              Create a production environment
              <span className="block text-xs text-fg-muted">More environments can be added later.</span>
            </span>
          </label>
          {project.isError && (
            <Notice tone="error" title="Could not create the project">
              {errorMessage(project.error)}
            </Notice>
          )}
          <div className="flex flex-wrap gap-2">
            <Button type="submit" variant="primary" disabled={!online} pending={project.isPending || environment.isPending}>
              Create project
            </Button>
            <Button variant="ghost" onClick={onClose} disabled={project.isPending || environment.isPending}>
              Cancel
            </Button>
          </div>
        </form>
      )}
    </Section>
  );
}

function ProjectGroup({ project, resources }: { project: Project; resources: WorkloadSummary[] }) {
  const inProject = resourcesIn(resources, project.id);
  const shared = inProject.filter((resource) => !resource.environment_id);
  const environments = productionEnvironments(project);
  const previews = previewEnvironments(project).length;
  return (
    <li>
      <div className="flex flex-wrap items-baseline justify-between gap-x-3 gap-y-0.5 bg-panel px-3 py-2">
        <h2 id={`project-${project.id}`} className="min-w-0 max-w-full">
          <Link to="/projects/$projectId" params={{ projectId: project.id }} className="inline-flex max-w-full items-center gap-2 rounded text-sm font-semibold text-fg hover:underline hover:decoration-fg-subtle hover:underline-offset-4">
            <Folder aria-hidden className="size-4 shrink-0 text-fg-muted" />
            <span className="min-w-0 break-words">{project.name}</span>
          </Link>
        </h2>
        <p className="text-xs text-fg-muted tabular-nums">
          {countLabel(environments.length, "environment")}
          {previews > 0 && ` · ${countLabel(previews, "preview")}`} · {countLabel(inProject.length, "resource")}
        </p>
      </div>
      {environments.length === 0 && shared.length === 0 ? (
        <p className="border-t border-line px-3 py-2.5 text-[13px] text-fg-muted sm:pl-9">
          No environments yet.{" "}
          <Link to="/projects/$projectId" params={{ projectId: project.id }} className={linkClass}>
            Open {project.name}
          </Link>
        </p>
      ) : (
        <ul aria-label={`Environments in ${project.name}`} className="divide-y divide-line border-t border-line">
          {shared.length > 0 && <PlacementRow projectId={project.id} name="Project resources" resources={shared} indent />}
          {environments.map((environment) => (
            <PlacementRow
              key={environment.id}
              projectId={project.id}
              environmentId={environment.id}
              name={environment.name}
              resources={inProject.filter((resource) => resource.environment_id === environment.id)}
              indent
            />
          ))}
        </ul>
      )}
    </li>
  );
}

function ProjectList({ projects, resources, creating, allowed, newProject }: { projects: Project[]; resources: WorkloadSummary[]; creating: boolean; allowed: boolean; newProject: ReactNode }) {
  if (projects.length === 0) {
    if (creating) return null;
    return (
      <EmptyState title="No projects yet" action={newProject}>
        Projects hold resources directly or within optional environments such as production and staging. Each resource runs on one
        node, so an environment can span several nodes.
        {!allowed && " Ask a team owner or admin to create a project or grant you access to one."}
      </EmptyState>
    );
  }
  return (
    <div className="px-4 pt-5 sm:px-6">
      <ul aria-label="Projects" className="divide-y divide-line-strong overflow-hidden rounded-lg border border-line bg-surface">
        {projects.map((project) => (
          <ProjectGroup key={project.id} project={project} resources={resources} />
        ))}
      </ul>
    </div>
  );
}

export function ProjectsPage() {
  const projects = useQuery(projectsQuery);
  const workloads = useQuery(workloadsQuery);
  const nodes = useQuery(nodesQuery);
  const [creating, setCreating] = useState(false);
  const allowed = canCreateProjects(useSession());
  const noReadyNode = nodes.data !== undefined && !nodes.data.some((node) => node.state === "ready");

  const newProject = allowed && (
    <Button variant="primary" onClick={() => setCreating(true)} disabled={creating}>
      <Plus aria-hidden className="size-3.5" />
      New project
    </Button>
  );

  let body;
  if (projects.isPending || workloads.isPending) {
    body = <LoadingRows label="Loading projects" />;
  } else if (projects.isError || workloads.isError) {
    const failed = projects.isError ? projects : workloads;
    body = (
      <div className="px-4 pt-5 sm:px-6">
        <QueryError what={projects.isError ? "projects" : "resources"} error={failed.error} onRetry={() => void failed.refetch()} retrying={failed.isFetching} />
      </div>
    );
  } else {
    body = <ProjectList projects={projects.data} resources={workloads.data} creating={creating} allowed={allowed} newProject={newProject} />;
  }

  return (
    <>
      <PageHeader icon={<Folder aria-hidden />} title="Projects" description="Every resource belongs to a project. Use environments to separate production from staging." actions={newProject} />
      {noReadyNode && (
        <div className="px-4 pt-5 sm:px-6">
          <Notice
            tone="warn"
            title="No ready nodes"
            action={
              <Link to="/nodes" className={buttonClass("secondary")}>
                Open nodes
              </Link>
            }
          >
            Resources run on a node that has passed SSH verification with Docker Compose available.
          </Notice>
        </div>
      )}
      {creating && <CreateProject onClose={() => setCreating(false)} />}
      {body}
    </>
  );
}

import { useQuery } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { Plus, Boxes, Tags } from "lucide-react";
import { useMemo, useState } from "react";
import { projectsQuery } from "../api/projects";
import { nodesQuery, operationsQuery, workloadsQuery } from "../api/queries";
import type { OperationState } from "../api/types";
import { FilterSelect, FilterToolbar, NoMatches } from "../components/FilterToolbar";
import { ResourceTable } from "../components/ResourceTable";
import { EmptyState, LoadingRows, PageHeader, QueryError, Section } from "../components/ui";
import { countLabel } from "../lib/hierarchy";
import { isActive, latestByWorkload, resourceTypeLabel } from "../lib/operations";
import { buttonClass } from "../lib/styles";
import { can, canCreateProjects } from "../lib/permissions";
import { useSession } from "../lib/session";

function NewResourceLink() {
  return (
    <Link to="/resources/new" className={buttonClass("primary")}>
      <Plus aria-hidden className="size-3.5" />
      New resource
    </Link>
  );
}

const lastOperationFilters = { active: "Queued or running", succeeded: "Succeeded", failed: "Failed or interrupted", cancelled: "Cancelled", none: "None recent" } as const;

function lastOperationFilter(state: OperationState | undefined): keyof typeof lastOperationFilters {
  if (!state) return "none";
  if (isActive(state)) return "active";
  if (state === "failed" || state === "interrupted") return "failed";
  return state;
}

function useInventory() {
  const projects = useQuery(projectsQuery);
  const workloads = useQuery(workloadsQuery);
  return { projects, workloads };
}

export function WorkloadsPage() {
  const { projects, workloads } = useInventory();
  const nodes = useQuery(nodesQuery);
  const operations = useQuery(operationsQuery);
  const latest = useMemo(() => latestByWorkload(operations.data ?? []), [operations.data]);
  const [placement, setPlacement] = useState("");
  const [search, setSearch] = useState("");
  const [status, setStatus] = useState("");
  const [node, setNode] = useState("");
  const [type, setType] = useState("");
  const hasReadyNode = nodes.data?.some((node) => node.state === "ready");
  const session = useSession();
  const canCreate = canCreateProjects(session) || (projects.data?.some((project) => can(project.permissions, "edit") || project.environments.some((environment) => can(environment.permissions, "edit"))) ?? false);

  let body;
  if (projects.isPending || workloads.isPending) {
    body = <LoadingRows label="Loading resources" />;
  } else if (projects.isError || workloads.isError) {
    const failed = projects.isError ? projects : workloads;
    body = (
      <div className="px-4 pt-5 sm:px-6">
        <QueryError what={projects.isError ? "projects" : "resources"} error={failed.error} onRetry={() => void failed.refetch()} retrying={failed.isFetching} />
      </div>
    );
  } else if (workloads.data.length === 0) {
    body = (
      <EmptyState
        title="No resources yet"
        action={
          hasReadyNode === false ? (
            <Link to="/nodes" className={buttonClass("primary")}>
              Set up a node
            </Link>
          ) : (
            canCreate && <NewResourceLink />
          )
        }
      >
        {hasReadyNode === false
          ? "Resources run on a ready node. Add a node and verify it before creating a resource."
          : "Create a resource from a template, a managed database, a Compose file or a Git repository, then deploy it to a ready node."}
      </EmptyState>
    );
  } else {
    const query = search.trim().toLowerCase();
    const filtering = Boolean(query || placement || status || node || type);
    const typeOptions = [...new Set(workloads.data.map((resource) => resource.kind || "service"))].sort();
    const nodeOptions = [...new Map(workloads.data.map((resource) => [resource.node_id, resource.node])).entries()].sort((a, b) => a[1].localeCompare(b[1]));
    const filtered = workloads.data.filter((resource) => {
      if (query && !resource.name.toLowerCase().includes(query)) return false;
      if (node && resource.node_id !== node) return false;
      if (type && (resource.kind || "service") !== type) return false;
      if (status && lastOperationFilter(latest.get(resource.id)?.state) !== status) return false;
      if (placement.startsWith("project:")) return resource.project_id === placement.slice(8);
      if (placement.startsWith("direct:")) return resource.project_id === placement.slice(7) && !resource.environment_id;
      return !placement || resource.environment_id === placement;
    });
    const reset = () => {
      setSearch("");
      setPlacement("");
      setStatus("");
      setNode("");
      setType("");
    };
    body = (
      <Section title="Resources" description="Last operation is drawn from the 100 most recent operations." flush>
        <FilterToolbar
          label="Filter resources"
          search={search}
          onSearch={setSearch}
          searchPlaceholder="Search by name"
          active={filtering}
          onReset={reset}
          summary={filtering ? `${filtered.length} of ${countLabel(workloads.data.length, "resource")}` : countLabel(workloads.data.length, "resource")}
        >
          <FilterSelect id="resource-placement" label="Project and environment" value={placement} onChange={setPlacement}>
            <option value="">All projects</option>
            {projects.data.map((project) => (
              <optgroup key={project.id} label={project.name}>
                <option value={`project:${project.id}`}>{project.name} / All resources</option>
                {can(project.permissions, "read") && <option value={`direct:${project.id}`}>{project.name} / Project resources</option>}
                {project.environments.map((environment) => (
                  <option key={environment.id} value={environment.id}>
                    {project.name} / {environment.name}
                  </option>
                ))}
              </optgroup>
            ))}
          </FilterSelect>
          <FilterSelect id="resource-status" label="Last operation" value={status} onChange={setStatus}>
            <option value="">Any last operation</option>
            {Object.entries(lastOperationFilters).map(([value, label]) => (
              <option key={value} value={value}>
                {label}
              </option>
            ))}
          </FilterSelect>
          {typeOptions.length > 1 && (
            <FilterSelect id="resource-type" label="Type" value={type} onChange={setType}>
              <option value="">All types</option>
              {typeOptions.map((value) => (
                <option key={value} value={value}>
                  {resourceTypeLabel(value)}
                </option>
              ))}
            </FilterSelect>
          )}
          {nodeOptions.length > 1 && (
            <FilterSelect id="resource-node" label="Node" value={node} onChange={setNode}>
              <option value="">All nodes</option>
              {nodeOptions.map(([id, name]) => (
                <option key={id} value={id}>
                  {name}
                </option>
              ))}
            </FilterSelect>
          )}
        </FilterToolbar>
        {filtered.length === 0 ? <NoMatches what="resources" onReset={reset} /> : <ResourceTable resources={filtered} projects={projects.data} caption="Resources" />}
      </Section>
    );
  }

  return (
    <>
      <PageHeader icon={<Boxes aria-hidden />} title="All resources" description="Every resource you can access, with its environment and node." actions={
          <>
            <Link to="/resources/organize" className={buttonClass("secondary")}>
              <Tags aria-hidden className="size-3.5" />
              Organize
            </Link>
            {canCreate && <NewResourceLink />}
          </>
        } />
      {body}
    </>
  );
}

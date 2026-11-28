import { useQuery } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { ResourceIcon } from "./ResourceIcon";
import { useMemo } from "react";
import { operationsQuery } from "../api/queries";
import type { Project } from "../api/projects";
import type { WorkloadSummary } from "../api/types";
import { findPlacement } from "../lib/hierarchy";
import { actionLabels, latestByWorkload, resourceTypeLabel } from "../lib/operations";
import { listLinkClass, tableHeadClass, tableRowClass } from "../lib/styles";
import { RelativeTime } from "./RelativeTime";
import { OperationStateLabel } from "./status";
import { QueryError } from "./ui";

interface ResourceTableProps {
  resources: WorkloadSummary[];
  caption: string;
  // When provided, a column shows each resource's project and environment.
  projects?: Project[];
  // Offers a direct link to each resource's placement settings.
  assignAction?: boolean;
}

function Placement({ resource, projects }: { resource: WorkloadSummary; projects: Project[] }) {
  const placement = findPlacement(projects, resource.environment_id, resource.project_id);
  if (!placement) return <span className="text-fg-subtle">Project unavailable</span>;
  if (!placement.environment) return <Link to="/projects/$projectId" params={{ projectId: placement.project.id }} className="rounded hover:text-fg">{placement.project.name} / Project resources</Link>;
  return (
    <Link
      to="/projects/$projectId/environments/$environmentId"
      params={{ projectId: placement.project.id, environmentId: placement.environment.id }}
      className="rounded hover:text-fg"
    >
      {placement.project.name} <span className="text-fg-subtle">/</span> {placement.environment.name}
    </Link>
  );
}

export function ResourceTable({ resources, caption, projects, assignAction = false }: ResourceTableProps) {
  const operations = useQuery(operationsQuery);
  const latest = useMemo(() => latestByWorkload(operations.data ?? []), [operations.data]);

  return (
    <>
      {operations.isError && (
        <div className="border-b border-line p-3">
          <QueryError what="recent operations" error={operations.error} onRetry={() => void operations.refetch()} retrying={operations.isFetching} />
        </div>
      )}
      <table className="w-full text-left">
        <caption className="sr-only">{caption}</caption>
        <thead>
          <tr className={tableHeadClass}>
            <th scope="col" className="py-2 pr-4 pl-4 font-medium">
              Resource
            </th>
            {projects && (
              <th scope="col" className="hidden py-2 pr-4 font-medium lg:table-cell">
                Project / environment
              </th>
            )}
            <th scope="col" className="hidden py-2 pr-4 font-medium sm:table-cell">
              Node
            </th>
            <th scope="col" className="py-2 pr-4 font-medium">
              Last operation
            </th>
            <th scope="col" className={`py-2 pr-4 text-right font-medium ${assignAction ? "" : "hidden md:table-cell"}`}>
              {assignAction ? <span className="sr-only">Actions</span> : "When"}
            </th>
          </tr>
        </thead>
        <tbody>
          {resources.map((resource) => {
            const operation = latest.get(resource.id);
            return (
              <tr key={resource.id} className={tableRowClass}>
                <td className="py-2.5 pr-4 pl-4">
                  <div className="flex min-w-0 items-center gap-3">
                    <ResourceIcon kind={resource.kind} className="hidden size-4 shrink-0 text-fg-subtle sm:block" />
                    <div className="min-w-0">
                      <Link to="/resources/$resourceId" params={{ resourceId: resource.id }} className={`${listLinkClass} break-words`}>
                        {resource.name}
                      </Link>
                      <p className="text-xs text-fg-subtle">
                        {resourceTypeLabel(resource.kind)}
                        <span className="sm:hidden"> · {resource.node}</span>
                      </p>
                      {projects && (
                        <p className="mt-0.5 text-xs text-fg-muted lg:hidden">
                          <Placement resource={resource} projects={projects} />
                        </p>
                      )}
                    </div>
                  </div>
                </td>
                {projects && (
                  <td className="hidden py-2.5 pr-4 text-[13px] text-fg-muted lg:table-cell">
                    <Placement resource={resource} projects={projects} />
                  </td>
                )}
                <td className="hidden py-2.5 pr-4 text-[13px] text-fg-muted sm:table-cell">
                  <Link to="/nodes/$nodeId" params={{ nodeId: resource.node_id }} className="rounded hover:text-fg">
                    {resource.node}
                  </Link>
                </td>
                <td className="py-2.5 pr-4">
                  {operation ? (
                    <>
                      <Link
                        to="/operations/$operationId"
                        params={{ operationId: operation.id }}
                        className="inline-flex flex-wrap items-center gap-x-2 rounded hover:underline hover:decoration-fg-subtle hover:underline-offset-4"
                      >
                        <span className="text-[13px] text-fg-muted">{actionLabels[operation.action]}</span>
                        <OperationStateLabel state={operation.state} />
                      </Link>
                      {!assignAction && <RelativeTime value={operation.created_at} className="block text-xs text-fg-muted md:hidden" />}
                    </>
                  ) : operations.isPending ? (
                    <span className="text-xs text-fg-subtle">Loading</span>
                  ) : (
                    <span className="text-[13px] text-fg-subtle">None recent</span>
                  )}
                </td>
                <td className={`py-2.5 pr-4 text-right text-[13px] whitespace-nowrap text-fg-muted tabular-nums ${assignAction ? "" : "hidden md:table-cell"}`}>
                  {assignAction ? (
                    <Link to="/resources/$resourceId/settings" params={{ resourceId: resource.id }} className="rounded text-fg hover:underline hover:underline-offset-4">
                      Assign<span className="sr-only"> {resource.name}</span>
                    </Link>
                  ) : (
                    operation && <RelativeTime value={operation.created_at} />
                  )}
                </td>
              </tr>
            );
          })}
        </tbody>
      </table>
    </>
  );
}

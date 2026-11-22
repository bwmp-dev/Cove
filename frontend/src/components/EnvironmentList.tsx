import { Link } from "@tanstack/react-router";
import { Boxes, ChevronRight, Layers } from "lucide-react";
import type { ReactNode } from "react";
import type { WorkloadSummary } from "../api/types";
import { countLabel, nodeNames } from "../lib/hierarchy";

function nodesText(resources: WorkloadSummary[]) {
  const nodes = nodeNames(resources);
  if (nodes.length === 0) return "";
  return nodes.length === 1 ? nodes[0] : `${countLabel(nodes.length, "node")}: ${nodes.join(", ")}`;
}

// One compact row for an environment, or for the project-level resources when environmentId is omitted.
// From tablet width the name, resources, nodes and count line up in columns across rows.
export function PlacementRow({ projectId, environmentId, name, resources, actions, indent = false }: {
  projectId: string;
  environmentId?: string;
  name: string;
  resources: WorkloadSummary[];
  actions?: ReactNode;
  indent?: boolean;
}) {
  const Icon = environmentId ? Layers : Boxes;
  const names = resources.map((resource) => resource.name).join(", ");
  const nodes = nodesText(resources);
  const content = (
    <>
      <Icon aria-hidden className="mt-0.5 size-3.5 shrink-0 text-fg-subtle" />
      <span className="grid min-w-0 flex-1 grid-cols-[minmax(0,1fr)_auto] items-baseline gap-x-4 gap-y-0.5 md:grid-cols-[minmax(8rem,14rem)_minmax(0,1fr)_minmax(0,11rem)_6.5rem]">
        <span title={name} className="truncate text-[13px] font-medium text-fg group-hover:underline group-hover:decoration-fg-subtle group-hover:underline-offset-4">{name}</span>
        <span className="text-right text-xs text-fg-muted tabular-nums md:order-last">{countLabel(resources.length, "resource")}</span>
        <span className="col-span-2 truncate text-xs text-fg-muted md:col-span-1">{names || "No resources"}</span>
        <span className={`col-span-2 truncate text-xs text-fg-subtle md:col-span-1 ${nodes ? "" : "hidden md:block"}`}>{nodes}</span>
      </span>
      <ChevronRight aria-hidden className="mt-0.5 size-3.5 shrink-0 text-fg-subtle" />
    </>
  );
  const className = `group flex min-w-0 flex-1 items-start gap-2.5 rounded py-2.5 pl-3 ${indent ? "sm:pl-9" : ""} ${actions ? "" : "pr-3"}`;
  return (
    <li className="flex min-w-0 items-center gap-1 hover:bg-raised">
      {environmentId ? (
        <Link to="/projects/$projectId/environments/$environmentId" params={{ projectId, environmentId }} className={className}>
          {content}
        </Link>
      ) : (
        <Link to="/projects/$projectId" params={{ projectId }} className={className}>
          {content}
        </Link>
      )}
      {actions && <div className="shrink-0 pr-2">{actions}</div>}
    </li>
  );
}

import { useQuery } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { ArrowRight, ArrowLeftRight } from "lucide-react";
import { errorMessage, request } from "../api/client";
import { Notice, Section } from "./ui";

interface Edge { source: string; source_name: string; target: string; target_name: string; kind: "variable" | "network" | "order" | "connection"; label: string }

export function DependencyMap({ projectId }: { projectId: string }) {
 const edges = useQuery({ queryKey: ["dependencies", projectId], queryFn: ({ signal }) => request<Edge[]>("GET", `/api/projects/${projectId}/dependencies`, undefined, signal) });
 return <Section title="Dependencies" description="Explicit variable bindings, private connections, shared networks, and deployment order. Relationships involving resources you cannot view are omitted. Network access does not imply deployment order.">
  {edges.isError ? <Notice tone="error">{errorMessage(edges.error)}</Notice> : !edges.data ? <p className="text-sm text-fg-muted">Loading dependencies…</p> : edges.data.length === 0 ? <p className="text-sm text-fg-muted">No explicit dependencies configured.</p> : <ul className="divide-y divide-line">{edges.data.map((edge, index) => <li key={index} className="grid grid-cols-[1fr_auto_1fr] items-center gap-3 py-3 text-sm">
   <Link to="/resources/$resourceId" params={{ resourceId: edge.source }} className="min-w-0 break-words text-fg hover:underline">{edge.source_name}</Link>
   <div className="flex max-w-40 flex-col items-center gap-1 text-center text-xs text-fg-muted">{edge.kind === "network" ? <ArrowLeftRight aria-hidden className="size-4" /> : <ArrowRight aria-hidden className="size-4" />}<span className="break-all">{edge.kind === "variable" ? `Uses ${edge.label}` : edge.kind === "order" ? "Deploys after" : edge.kind === "connection" ? `Connects to ${edge.label}` : `Shares ${edge.label}`}</span></div>
   <Link to="/resources/$resourceId" params={{ resourceId: edge.target }} className="min-w-0 break-words text-fg hover:underline">{edge.target_name}</Link>
  </li>)}</ul>}
 </Section>;
}

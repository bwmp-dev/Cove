import { useQuery, type UseQueryResult } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { ArrowRight, Cable, Lock, Network, Plus } from "lucide-react";
import { ResourceIcon } from "./ResourceIcon";
import { useId, useMemo, useRef, useState, type FormEvent, type PointerEvent as ReactPointerEvent, type ReactNode } from "react";
import { errorMessage } from "../api/client";
import { connectionGraphQuery, useCreateConnection, useRemoveConnection, useRetryConnection, useUpdateConnection } from "../api/private-connections";
import { workloadQuery, workloadsQuery } from "../api/queries";
import { databaseEndpoints } from "../lib/databases";
import type { WorkloadSummary } from "../api/types";
import {
  aliasPattern,
  connectionStates,
  edgePath,
  endpointLabel,
  layoutGraph,
  NODE_HEIGHT,
  NODE_WIDTH,
  resourceStatus,
  separateLabels,
  suggestedAlias,
  validAlias,
  type ConnectionEndpoint,
  type ConnectionGraph,
  type GraphResource,
  type PrivateConnection,
} from "../lib/connections";
import { useOnline } from "../lib/hooks";
import { can } from "../lib/permissions";
import { buttonClass, inputClass, listLinkClass } from "../lib/styles";
import { ActionMenu } from "./ActionMenu";
import { ConfirmDialog, Dialog } from "./ConfirmDialog";
import { ResourceTable } from "./ResourceTable";
import { Select } from "./Select";
import { StatusDot, StatusLabel } from "./status";
import { Button, Field, LoadingRows, Notice, QueryError, Section } from "./ui";

type View = "graph" | "list";
type EdgeTone = "fg-subtle" | "bad" | "info" | "warn";

const strokeClasses: Record<EdgeTone, string> = { "fg-subtle": "stroke-fg-subtle", bad: "stroke-bad", info: "stroke-info", warn: "stroke-warn" };
const fillClasses: Record<EdgeTone, string> = { "fg-subtle": "fill-fg-subtle", bad: "fill-bad", info: "fill-info", warn: "fill-warn" };

function edgeTone(connection: PrivateConnection): EdgeTone {
  if (connection.state === "error") return "bad";
  if (connection.state === "pending" || connection.state === "revoking") return "info";
  return "fg-subtle";
}

interface GraphNode {
  id: string;
  resource?: GraphResource;
  external: boolean;
}

interface Selection {
  kind: "connection" | "resource";
  id: string;
}

interface Draft {
  connection?: PrivateConnection;
  source?: string;
  target?: string;
}

// Sections without connections keep the resource list; the graph opens by default where it has
// something to show and room to show it.
function automaticView(connections: number): View {
  const wide = typeof window !== "undefined" && window.matchMedia?.("(min-width: 768px)").matches;
  return connections > 0 && wide ? "graph" : "list";
}

function ViewToggle({ view, onChange }: { view: View; onChange: (view: View) => void }) {
  const item = (value: View, label: string) => (
    <button
      type="button"
      aria-pressed={view === value}
      onClick={() => onChange(value)}
      className="h-8 px-3 text-[13px] font-medium text-fg-muted transition-colors first:rounded-l-md last:rounded-r-md hover:bg-raised hover:text-fg aria-pressed:bg-hover aria-pressed:text-fg pointer-coarse:min-h-10"
    >
      {label}
    </button>
  );
  return (
    <div role="group" aria-label="Resource view" className="inline-flex divide-x divide-line-strong rounded-md border border-line-strong">
      {item("graph", "Graph")}
      {item("list", "List")}
    </div>
  );
}

// The resources, connections and shared networks shown for one environment, or for a project's
// resources outside environments.
function scopeGraph(graph: ConnectionGraph, projectId: string, environmentId: string | undefined) {
  const inScope = (resource: GraphResource) => resource.project_id === projectId && (environmentId ? resource.environment_id === environmentId : resource.environment_id === "");
  const byId = new Map(graph.resources.map((resource) => [resource.id, resource]));
  const local = graph.resources.filter(inScope);
  const localIds = new Set(local.map((resource) => resource.id));
  const connections = graph.connections.filter((connection) => (connection.source.id && localIds.has(connection.source.id)) || (connection.target.id && localIds.has(connection.target.id)));
  const nodes: GraphNode[] = local.map((resource) => ({ id: resource.id, resource, external: false }));
  const seen = new Set(localIds);
  const endpointId = (connection: PrivateConnection, role: "source" | "target") => connection[role].id ?? `restricted-${connection.id}-${role}`;
  for (const connection of connections) {
    for (const role of ["source", "target"] as const) {
      const id = endpointId(connection, role);
      if (seen.has(id)) continue;
      seen.add(id);
      nodes.push({ id, resource: byId.get(id), external: true });
    }
  }
  const shared = graph.shared_networks.flatMap((network) => {
    const members = network.resources.filter((id) => seen.has(id));
    return members.flatMap((a, index) => members.slice(index + 1).map((b) => ({ name: network.name, a, b })));
  });
  return { nodes, connections, shared, endpointId, local };
}

export function EnvironmentResources({ projectId, environmentId, title, description, resources, empty, actions, canCreate, managedConnections = false }: {
  projectId: string;
  environmentId?: string;
  title: string;
  description: string;
  resources: WorkloadSummary[];
  empty: ReactNode;
  actions?: ReactNode;
  canCreate: boolean;
  // Preview environments copy their connections from production; they cannot be edited here.
  managedConnections?: boolean;
}) {
  const [chosenView, setView] = useState<View | null>(null);
  const graph = useQuery(connectionGraphQuery(projectId, environmentId));
  const workloads = useQuery(workloadsQuery);
  const [draft, setDraft] = useState<Draft | null>(null);
  const online = useOnline();
  const editable = (workloads.data ?? []).filter((workload) => can(workload.permissions, "edit"));
  const canConnect = !managedConnections && editable.length > 1;
  const scoped = useMemo(() => (graph.data ? scopeGraph(graph.data, projectId, environmentId) : null), [graph.data, projectId, environmentId]);
  const view = chosenView ?? automaticView(scoped?.connections.length ?? 0);

  return (
    <Section
      title={title}
      description={description}
      flush
      actions={
        <>
          {(resources.length > 0 || (scoped?.connections.length ?? 0) > 0) && <ViewToggle view={view} onChange={setView} />}
          {canConnect && (
            <Button onClick={() => setDraft({})} disabled={!online} title={online ? undefined : "Unavailable while offline."}>
              <Cable aria-hidden className="size-3.5" />
              New connection
            </Button>
          )}
          {canCreate && (
            <Link to="/resources/new" search={{ project: projectId, environment: environmentId }} className={buttonClass("secondary")}>
              <Plus aria-hidden className="size-3.5" />
              New resource
            </Link>
          )}
          {actions}
        </>
      }
    >
      <ResourcesBody
        view={view}
        title={title}
        resources={resources}
        empty={empty}
        graph={graph}
        scoped={scoped}
        onDraft={setDraft}
        canConnect={canConnect && online}
        editableIds={new Set(editable.map((workload) => workload.id))}
      />
      <Dialog open={draft !== null} title={draft?.connection ? "Edit connection" : "New private connection"} onClose={() => setDraft(null)}>
        {draft && <ConnectionForm draft={draft} graph={graph.data} candidates={editable} onClose={() => setDraft(null)} />}
      </Dialog>
    </Section>
  );
}

type Scoped = ReturnType<typeof scopeGraph>;

function ResourcesBody({ view, title, resources, empty, graph, scoped, onDraft, canConnect, editableIds }: {
  view: View;
  title: string;
  resources: WorkloadSummary[];
  empty: ReactNode;
  graph: UseQueryResult<ConnectionGraph>;
  scoped: Scoped | null;
  onDraft: (draft: Draft) => void;
  canConnect: boolean;
  editableIds: Set<string>;
}) {
  if (resources.length === 0 && (!scoped || scoped.connections.length === 0)) return empty;
  const failure = graph.isError && (
    <div className={view === "list" ? "border-t border-line p-3" : "p-3"}>
      <QueryError what={view === "list" ? "private connections" : "the resource graph"} error={graph.error} onRetry={() => void graph.refetch()} retrying={graph.isFetching} />
    </div>
  );
  if (view === "list") {
    return (
      <>
        <ResourceTable resources={resources} caption={title} />
        {failure || (scoped && <ConnectionList graph={scoped} onEdit={(connection) => onDraft({ connection })} sharedNetworks={scoped.shared} names={new Map(graph.data?.resources.map((r) => [r.id, r.name]))} />)}
      </>
    );
  }
  if (failure) return failure;
  if (!scoped || !graph.data) return <LoadingRows label="Loading the resource graph" rows={3} />;
  return <GraphView graph={scoped} all={graph.data} onConnect={(source, target) => onDraft({ source, target })} onEdit={(connection) => onDraft({ connection })} canConnect={canConnect} editableIds={editableIds} />;
}

function nodeTitle(node: GraphNode) {
  return node.resource?.name ?? "Resource you cannot view";
}

function GraphView({ graph, all, onConnect, onEdit, canConnect, editableIds }: {
  graph: Scoped;
  all: ConnectionGraph;
  onConnect: (source?: string, target?: string) => void;
  onEdit: (connection: PrivateConnection) => void;
  canConnect: boolean;
  editableIds: Set<string>;
}) {
  const [selection, setSelection] = useState<Selection | null>(null);
  const [connectFrom, setConnectFrom] = useState<string | null>(null);
  const [drag, setDrag] = useState<{ from: string; x: number; y: number; moved: boolean } | null>(null);
  const canvas = useRef<HTMLDivElement>(null);
  const markerId = useId().replace(/:/g, "");
  const { edges, layout, labels } = useMemo(() => {
    const edges = graph.connections.map((connection) => ({ connection, source: graph.endpointId(connection, "source"), target: graph.endpointId(connection, "target") }));
    const layout = layoutGraph(graph.nodes.map((node) => node.id), edges);
    const pairCount = new Map<string, number>();
    const placed = edges.flatMap(({ connection, source, target }) => {
      const from = layout.nodes.get(source);
      const to = layout.nodes.get(target);
      if (!from || !to) return [];
      const pair = `${source}/${target}`;
      const index = pairCount.get(pair) ?? 0;
      pairCount.set(pair, index + 1);
      return [{ connection, ...edgePath(from, to, index * 10).label }];
    });
    const separated = separateLabels(placed);
    return { edges, layout, labels: placed.map((label, index) => ({ ...label, ...separated[index]! })) };
  }, [graph]);
  const pairs = new Map<string, number>();
  const selectedConnection = selection?.kind === "connection" ? graph.connections.find((connection) => connection.id === selection.id) : undefined;
  const selectedNode = selection?.kind === "resource" ? graph.nodes.find((node) => node.id === selection.id) : undefined;
  const connectable = (node: GraphNode) => canConnect && Boolean(node.resource) && editableIds.has(node.id);

  function point(event: ReactPointerEvent) {
    const box = canvas.current?.getBoundingClientRect();
    return { x: event.clientX - (box?.left ?? 0) + (canvas.current?.scrollLeft ?? 0), y: event.clientY - (box?.top ?? 0) + (canvas.current?.scrollTop ?? 0) };
  }

  function choose(node: GraphNode) {
    if (connectFrom) {
      if (node.id !== connectFrom && node.resource && editableIds.has(node.id)) {
        onConnect(connectFrom, node.id);
      }
      setConnectFrom(null);
      return;
    }
    setSelection({ kind: "resource", id: node.id });
  }

  return (
    <div className="divide-y divide-line">
      <div className="space-y-1 px-4 py-3 text-xs text-fg-muted">
        <p>
          Each connection lets one source service open TCP connections to one target port through its private hostname. Replies flow back over the same connection; the target cannot start connections to the source unless you add one in that direction. Public domains and published ports are not affected.
        </p>
        {canConnect && <p>Drag from a resource's connect handle to another resource, or use New connection.</p>}
      </div>
      <section aria-label="Connection legend" className="px-4 py-3">
        <h3 className="mb-2 text-xs font-medium text-fg">Connection legend</h3>
        <dl className="grid gap-3 text-xs sm:grid-cols-2 xl:grid-cols-3">
          <div>
            <dt className="flex items-center gap-2 font-medium text-fg">
              <ArrowRight aria-hidden="true" className="h-4 w-8 shrink-0 text-fg-muted" />
              Solid arrow: private connection
            </dt>
            <dd className="mt-1 text-fg-muted">One direction, one target port. Replies are allowed.</dd>
          </div>
          <div>
            <dt className="flex items-center gap-2 font-medium text-fg">
              <span aria-hidden="true" className="w-8 shrink-0 border-t-2 border-dashed border-warn" />
              Yellow dashed line: shared network
            </dt>
            <dd className="mt-1 text-fg-muted">Both directions, all listening ports. Bypasses private connection restrictions.</dd>
          </div>
          <div>
            <dt className="flex items-center gap-2 font-medium text-fg">
              <span aria-hidden="true" className="flex w-8 shrink-0 items-center text-fg-muted">
                <span className="grow border-t-2 border-dashed border-current" />
                <ArrowRight className="size-4 shrink-0" />
              </span>
              Dashed arrow: change pending
            </dt>
            <dd className="mt-1 text-fg-muted">Being applied or revoked. Check the connection status.</dd>
          </div>
        </dl>
      </section>
      {graph.shared.length > 0 && (
        <div className="px-4 py-3">
          <Notice tone="warn" title="Shared networks give broader access">
            Yellow dashed lines are shared networks: resources on them reach each other on every listening port, in both directions, regardless of the connections shown here.
          </Notice>
        </div>
      )}
      {connectFrom && (
        <div className="flex flex-wrap items-center justify-between gap-2 border-b border-info-line bg-info-tint px-4 py-2 text-[13px] text-fg" role="status">
          <span>Choose the target for {nodeTitle(graph.nodes.find((node) => node.id === connectFrom) ?? { id: "", external: false })}.</span>
          <Button variant="ghost" onClick={() => setConnectFrom(null)}>
            Cancel
          </Button>
        </div>
      )}
      <div ref={canvas} className="relative overflow-auto" style={{ maxHeight: "36rem" }}>
        <div className="relative" style={{ width: layout.width + 64, height: layout.height }}>
          <svg aria-hidden className="pointer-events-none absolute inset-0" width={layout.width + 64} height={layout.height}>
            <defs>
              {(["fg-subtle", "bad", "info", "warn"] as const).map((tone) => (
                <marker key={tone} id={`${markerId}-${tone}`} viewBox="0 0 10 10" refX="9" refY="5" markerWidth="7" markerHeight="7" orient="auto-start-reverse">
                  <path d="M 0 0 L 10 5 L 0 10 z" className={fillClasses[tone]} />
                </marker>
              ))}
            </defs>
            {graph.shared.map(({ name, a, b }) => {
              const from = layout.nodes.get(a);
              const to = layout.nodes.get(b);
              if (!from || !to) return null;
              const path = edgePath(from.x <= to.x ? from : to, from.x <= to.x ? to : from, -18);
              return <path key={`${name}-${a}-${b}`} d={path.d} fill="none" strokeDasharray="5 5" strokeWidth={1.5} className="stroke-warn" />;
            })}
            {edges.map(({ connection, source, target }) => {
              const from = layout.nodes.get(source);
              const to = layout.nodes.get(target);
              if (!from || !to) return null;
              const pair = `${source}/${target}`;
              const index = pairs.get(pair) ?? 0;
              pairs.set(pair, index + 1);
              const tone = edgeTone(connection);
              const selected = selection?.kind === "connection" && selection.id === connection.id;
              return (
                <path
                  key={connection.id}
                  d={edgePath(from, to, index * 10).d}
                  fill="none"
                  strokeWidth={selected ? 2.5 : 1.5}
                  strokeDasharray={connection.state === "applied" || connection.state === "error" ? undefined : "4 3"}
                  markerEnd={`url(#${markerId}-${tone})`}
                  className={selected ? strokeClasses.info : strokeClasses[tone]}
                />
              );
            })}
            {drag && layout.nodes.get(drag.from) && (
              <line x1={layout.nodes.get(drag.from)!.x + NODE_WIDTH} y1={layout.nodes.get(drag.from)!.y + NODE_HEIGHT / 2} x2={drag.x} y2={drag.y} strokeDasharray="4 3" strokeWidth={1.5} className="stroke-info" />
            )}
          </svg>
          {labels.map(({ connection, x, y }) => {
            const state = connectionStates[connection.state];
            const selected = selection?.kind === "connection" && selection.id === connection.id;
            const endpoint = `${connection.alias ?? "port"}:${connection.port}`;
            return (
              <button
                key={connection.id}
                type="button"
                aria-pressed={selected}
                aria-label={`${endpointLabel(connection.source)} to ${endpointLabel(connection.target)}, ${connection.alias ? `${connection.alias} port ${connection.port}` : `port ${connection.port}`}, ${state.label}`}
                onClick={() => setSelection({ kind: "connection", id: connection.id })}
                className={`absolute z-10 flex max-w-[168px] -translate-x-1/2 -translate-y-1/2 items-center gap-1.5 rounded-md border px-1.5 py-0.5 font-mono text-[11px] whitespace-nowrap transition-colors pointer-coarse:min-h-10 ${selected ? "border-info bg-info-tint text-fg" : "border-line-strong bg-panel text-fg-muted hover:text-fg"}`}
                style={{ left: x, top: y }}
              >
                <StatusDot tone={state.tone} />
                <span className="truncate">{endpoint}</span>
              </button>
            );
          })}
          {graph.nodes.map((node) => {
            const position = layout.nodes.get(node.id);
            if (!position) return null;
            const selected = selection?.kind === "resource" && selection.id === node.id;
            const status = node.resource ? resourceStatus(node.resource) : null;
            const choosing = connectFrom !== null && connectFrom !== node.id;
            return (
              <div key={node.id} data-node-id={node.id} className="absolute" style={{ left: position.x, top: position.y, width: NODE_WIDTH, height: NODE_HEIGHT }}>
                <button
                  type="button"
                  aria-pressed={selected}
                  onClick={() => choose(node)}
                  aria-label={choosing ? `Connect to ${nodeTitle(node)}` : undefined}
                  className={`flex size-full min-w-0 flex-col justify-center gap-1 rounded-md border px-3 text-left transition-colors ${selected ? "border-info bg-info-tint" : choosing ? "border-info-line bg-panel hover:bg-info-tint" : node.external ? "border-dashed border-line-strong bg-panel hover:bg-raised" : "border-line-strong bg-panel hover:bg-raised"}`}
                >
                  <span className="flex min-w-0 items-center gap-2 pr-5">
                    {node.resource ? <ResourceIcon kind={node.resource.kind} className="size-3.5 shrink-0 text-fg-subtle" /> : <Lock aria-hidden className="size-3.5 shrink-0 text-fg-subtle" />}
                    <span className="truncate text-[13px] font-medium text-fg" title={nodeTitle(node)}>
                      {nodeTitle(node)}
                    </span>
                  </span>
                  {node.resource ? (
                    <span className="flex min-w-0 items-center gap-1.5 text-xs text-fg-muted">
                      <StatusDot tone={status!.tone} />
                      <span className="truncate" title={`${status!.label} · ${node.resource.node}`}>
                        {status!.label} · {node.resource.node}
                      </span>
                    </span>
                  ) : (
                    <span className="text-xs text-fg-subtle">Outside your access</span>
                  )}
                  {node.external && node.resource && (
                    <span className="truncate text-[11px] text-fg-subtle" title={`${node.resource.project}${node.resource.environment ? ` / ${node.resource.environment}` : ""}`}>
                      {node.resource.project}
                      {node.resource.environment ? ` / ${node.resource.environment}` : " resources"}
                    </span>
                  )}
                </button>
                {connectable(node) && !connectFrom && (
                  <button
                    type="button"
                    aria-label={`Connect from ${nodeTitle(node)}`}
                    title={`Connect from ${nodeTitle(node)}`}
                    className="absolute top-1/2 -right-3 flex size-6 -translate-y-1/2 touch-none items-center justify-center rounded-md border border-line-strong bg-raised text-fg-muted hover:bg-hover hover:text-fg pointer-coarse:size-9"
                    onPointerDown={(event) => {
                      event.currentTarget.setPointerCapture(event.pointerId);
                      setDrag({ from: node.id, ...point(event), moved: false });
                    }}
                    onPointerMove={(event) => {
                      if (drag) setDrag({ ...drag, ...point(event), moved: true });
                    }}
                    onPointerUp={(event) => {
                      if (!drag?.moved) return;
                      const target = document.elementFromPoint(event.clientX, event.clientY)?.closest<HTMLElement>("[data-node-id]")?.dataset.nodeId;
                      setDrag(null);
                      if (target && target !== node.id && editableIds.has(target)) onConnect(node.id, target);
                    }}
                    onPointerCancel={() => setDrag(null)}
                    onClick={() => {
                      if (drag?.moved) return;
                      setDrag(null);
                      setConnectFrom(node.id);
                    }}
                  >
                    <ArrowRight aria-hidden className="size-3.5" />
                  </button>
                )}
              </div>
            );
          })}
        </div>
      </div>
      {selectedConnection && <ConnectionDetail connection={selectedConnection} onEdit={() => onEdit(selectedConnection)} onClose={() => setSelection(null)} />}
      {selectedNode && <ResourceDetail node={selectedNode} graph={graph} all={all} onSelect={(id) => setSelection({ kind: "connection", id })} />}
    </div>
  );
}

function ResourceDetail({ node, graph, all, onSelect }: { node: GraphNode; graph: Scoped; all: ConnectionGraph; onSelect: (id: string) => void }) {
  const outgoing = graph.connections.filter((connection) => connection.source.id === node.id);
  const incoming = graph.connections.filter((connection) => connection.target.id === node.id);
  const networks = all.shared_networks.filter((network) => network.resources.includes(node.id));
  const resource = node.resource;
  return (
    <div className="space-y-3 p-4">
      <div className="flex flex-wrap items-baseline justify-between gap-2">
        <h3 className="min-w-0 text-[13px] font-semibold break-words text-fg">
          {resource ? (
            <Link to="/resources/$resourceId" params={{ resourceId: resource.id }} className={listLinkClass}>
              {resource.name}
            </Link>
          ) : (
            "Resource you cannot view"
          )}
        </h3>
        {resource && <StatusLabel {...resourceStatus(resource)} />}
      </div>
      {resource && (
        <dl className="grid gap-x-6 gap-y-1 text-xs sm:grid-cols-[max-content_1fr]">
          <dt className="text-fg-subtle">Node</dt>
          <dd className="text-fg-muted">{resource.node}</dd>
          <dt className="text-fg-subtle">Placement</dt>
          <dd className="text-fg-muted">
            {resource.project} / {resource.environment || "Project resources"}
          </dd>
          <dt className="text-fg-subtle">Compose services</dt>
          <dd className="font-mono break-all text-fg-muted">{resource.services.length > 0 ? resource.services.join(", ") : "Defined in its repository"}</dd>
        </dl>
      )}
      <ConnectionLinks title="Connects to" connections={outgoing} other="target" onSelect={onSelect} empty="No outgoing connections." />
      <ConnectionLinks title="Accepts from" connections={incoming} other="source" onSelect={onSelect} empty="No incoming connections. Only its own Compose services and any shared networks can reach it privately." />
      {networks.length > 0 && (
        <Notice tone="warn" title="Also on shared networks">
          {networks.map((network) => network.name).join(", ")}: every resource on these networks reaches this one on all ports.
        </Notice>
      )}
      {resource?.tailscale_service && <p className="text-xs text-fg-muted">Service {resource.tailscale_service} uses Tailscale access and cannot use private connections.</p>}
    </div>
  );
}

function ConnectionLinks({ title, connections, other, onSelect, empty }: { title: string; connections: PrivateConnection[]; other: "source" | "target"; onSelect: (id: string) => void; empty: string }) {
  return (
    <div>
      <h4 className="text-[11px] font-medium text-fg-subtle">{title}</h4>
      {connections.length === 0 ? (
        <p className="text-xs text-fg-muted">{empty}</p>
      ) : (
        <ul className="mt-1 space-y-1">
          {connections.map((connection) => (
            <li key={connection.id}>
              <button type="button" onClick={() => onSelect(connection.id)} className="inline-flex min-w-0 items-center gap-2 rounded text-left text-[13px] text-fg hover:underline">
                <StatusLabel {...connectionStates[connection.state]} />
                <span className="min-w-0 break-words">
                  {endpointLabel(connection[other])} <span className="font-mono text-xs text-fg-muted">{connection.alias ?? "port"}:{connection.port}</span>
                </span>
              </button>
            </li>
          ))}
        </ul>
      )}
    </div>
  );
}

function ConnectionSides({ connection }: { connection: PrivateConnection }) {
  if (connection.sides.length === 0) return null;
  return (
    <dl className="grid gap-x-6 gap-y-1 text-xs sm:grid-cols-[max-content_1fr]">
      {connection.sides.map((side) => (
        <div key={side.role} className="contents">
          <dt className="text-fg-subtle">{side.role === "source" ? "Source node" : "Target node"}</dt>
          <dd className="text-fg-muted">
            {sideLabels[side.state]}
            {side.detail && side.detail !== connection.detail ? ` — ${side.detail}` : ""}
          </dd>
        </div>
      ))}
    </dl>
  );
}

const sideLabels = { applied: "Applied", error: "Error", pending: "Waiting" } as const;

function ConnectionActions({ connection, onEdit, onRemove, onClose }: { connection: PrivateConnection; onEdit: () => void; onRemove: () => void; onClose?: () => void }) {
  const retry = useRetryConnection();
  const online = useOnline();
  const retryable = connection.can_retry && (connection.state === "error" || connection.state === "pending");
  return (
    <div className="space-y-2">
      <div className="flex flex-wrap gap-2">
        {retryable && (
          <Button onClick={() => retry.mutate(connection.id)} pending={retry.isPending} disabled={!online}>
            Retry
          </Button>
        )}
        {connection.can_edit && (
          <Button onClick={onEdit} disabled={!online}>
            Edit
          </Button>
        )}
        {connection.can_remove && (
          <Button variant="danger" onClick={onRemove} disabled={!online}>
            Remove
          </Button>
        )}
        {onClose && (
          <Button variant="ghost" onClick={onClose}>
            Close
          </Button>
        )}
      </div>
      {retry.isError && (
        <Notice tone="error" title="Could not retry">
          {errorMessage(retry.error)}
        </Notice>
      )}
    </div>
  );
}

export function ConnectionDetail({ connection, onEdit, onClose }: { connection: PrivateConnection; onEdit: () => void; onClose?: () => void }) {
  const remove = useRemoveConnection();
  const [confirming, setConfirming] = useState(false);
  const state = connectionStates[connection.state];
  return (
    <div className="space-y-3 p-4">
      <div className="flex flex-wrap items-start justify-between gap-2">
        <div className="min-w-0 space-y-1">
          <h3 className="flex min-w-0 flex-wrap items-center gap-x-2 text-[13px] font-semibold text-fg">
            <span className="break-words">{endpointLabel(connection.source)}</span>
            <ArrowRight aria-label="to" className="size-3.5 shrink-0 text-fg-subtle" />
            <span className="break-words">{endpointLabel(connection.target)}</span>
          </h3>
          <p className="font-mono text-xs break-all text-fg-muted">
            {connection.alias ? `${connection.alias}:${connection.port}` : `Port ${connection.port}`} · TCP
          </p>
        </div>
        <ConnectionActions connection={connection} onEdit={onEdit} onRemove={() => setConfirming(true)} onClose={onClose} />
      </div>
      <div className="space-y-1">
        <StatusLabel tone={state.tone} label={state.label} pulse={connection.state === "pending" || connection.state === "revoking"} />
        {connection.detail && <p className="text-xs text-fg-muted">{connection.detail}</p>}
      </div>
      <ConnectionSides connection={connection} />
      <p className="text-xs text-fg-muted">
        {connection.cross_node ? "Traffic between the two nodes is encrypted with mutual TLS between their Cove link gateways." : "Both resources run on the same node; traffic stays on that node."} Only the source service can use this hostname, and only for this port.
      </p>
      {connection.broader_access.length > 0 && (
        <Notice tone="warn" title="Broader access exists">
          These resources are also connected by {connection.broader_access.join(", ")}, which allows every port in both directions. This connection does not restrict that access.
        </Notice>
      )}
      {remove.isError && (
        <Notice tone="error" title="Could not remove the connection">
          {errorMessage(remove.error)}
        </Notice>
      )}
      <ConfirmDialog
        open={confirming}
        title="Remove this connection?"
        confirmLabel="Remove connection"
        danger
        pending={remove.isPending}
        onCancel={() => setConfirming(false)}
        onConfirm={() => remove.mutate(connection.id, { onSuccess: () => setConfirming(false) })}
      >
        <p>
          {endpointLabel(connection.source)} loses access to {endpointLabel(connection.target)}
          {connection.alias ? ` at ${connection.alias}:${connection.port}` : ""}. Open connections are closed as soon as either node applies the change.
        </p>
      </ConfirmDialog>
    </div>
  );
}

function ConnectionList({ graph, onEdit, sharedNetworks, names }: { graph: Scoped; onEdit: (connection: PrivateConnection) => void; sharedNetworks: Scoped["shared"]; names: Map<string, string> }) {
  const [open, setOpen] = useState<string | null>(null);
  const remove = useRemoveConnection();
  const [removing, setRemoving] = useState<PrivateConnection | null>(null);
  const networks = [...new Set(sharedNetworks.map((link) => link.name))];
  return (
    <div className="border-t border-line">
      <h3 className="flex items-center gap-2 px-4 pt-3 pb-2 text-[13px] font-semibold text-fg">
        <Cable aria-hidden className="size-3.5 text-fg-subtle" />
        Private connections
      </h3>
      {graph.connections.length === 0 ? (
        <p className="px-4 pb-3 text-[13px] text-fg-muted">No private connections. Resources without connections keep their existing networking.</p>
      ) : (
        <ul aria-label="Private connections" className="divide-y divide-line border-t border-line">
          {graph.connections.map((connection) => {
            const state = connectionStates[connection.state];
            return (
              <li key={connection.id}>
                <div className="flex flex-wrap items-center gap-x-4 gap-y-1 px-4 py-2.5 hover:bg-raised">
                  <button type="button" aria-expanded={open === connection.id} onClick={() => setOpen(open === connection.id ? null : connection.id)} className="min-w-0 flex-1 basis-64 rounded text-left">
                    <span className="flex min-w-0 flex-wrap items-center gap-x-2 text-[13px] text-fg">
                      <span className="break-words">{endpointLabel(connection.source)}</span>
                      <ArrowRight aria-label="to" className="size-3.5 shrink-0 text-fg-subtle" />
                      <span className="break-words">{endpointLabel(connection.target)}</span>
                    </span>
                    <span className="block font-mono text-xs break-all text-fg-muted">{connection.alias ? `${connection.alias}:${connection.port}` : `Port ${connection.port}`} · TCP</span>
                  </button>
                  <div className="flex shrink-0 items-center gap-3">
                  <StatusLabel tone={state.tone} label={state.label} pulse={connection.state === "pending" || connection.state === "revoking"} />
                  {(connection.can_edit || connection.can_remove) && (
                    <ActionMenu
                      label={`Actions for ${endpointLabel(connection.source)} to ${endpointLabel(connection.target)}`}
                      actions={[
                        ...(connection.can_edit ? [{ label: "Edit connection", onSelect: () => onEdit(connection) }] : []),
                        ...(connection.can_remove ? [{ label: "Remove connection", danger: true, onSelect: () => setRemoving(connection) }] : []),
                      ]}
                    />
                  )}
                  </div>
                </div>
                {open === connection.id && (
                  <div className="border-t border-line bg-panel">
                    <ConnectionDetail connection={connection} onEdit={() => onEdit(connection)} />
                  </div>
                )}
              </li>
            );
          })}
        </ul>
      )}
      {networks.length > 0 && (
        <div className="border-t border-line px-4 py-3">
          <Notice tone="warn" title="Shared networks give broader access">
            <ul className="space-y-0.5">
              {networks.map((network) => (
                <li key={network}>
                  <Network aria-hidden className="mr-1.5 inline size-3.5 align-[-2px]" />
                  {network}:{" "}
                  {[...new Set(sharedNetworks.filter((link) => link.name === network).flatMap((link) => [link.a, link.b]))].map((id) => names.get(id) ?? "a resource you cannot view").join(", ")} reach each other on every port.
                </li>
              ))}
            </ul>
          </Notice>
        </div>
      )}
      <ConfirmDialog
        open={removing !== null}
        title="Remove this connection?"
        confirmLabel="Remove connection"
        danger
        pending={remove.isPending}
        onCancel={() => setRemoving(null)}
        onConfirm={() => removing && remove.mutate(removing.id, { onSuccess: () => setRemoving(null) })}
      >
        {removing && (
          <p>
            {endpointLabel(removing.source)} loses access to {endpointLabel(removing.target)}. Open connections are closed as soon as either node applies the change.
          </p>
        )}
        {remove.isError && (
          <div className="mt-3">
            <Notice tone="error" title="Could not remove the connection">
              {errorMessage(remove.error)}
            </Notice>
          </div>
        )}
      </ConfirmDialog>
    </div>
  );
}

function EndpointField({ id, label, placeholder, existing, value, options, onChange }: {
  id: string;
  label: string;
  placeholder: string;
  existing?: ConnectionEndpoint;
  value: string;
  options: WorkloadSummary[];
  onChange: (value: string) => void;
}) {
  return (
    <Field id={id} label={label}>
      {existing ? (
        <p id={id} className="py-1.5 text-[13px] break-words">
          {endpointLabel(existing)}
        </p>
      ) : (
        <Select id={id} value={value} required onChange={(event) => onChange(event.target.value)}>
          <option value="">{placeholder}</option>
          {options.map((option) => (
            <option key={option.id} value={option.id}>
              {option.name}
              {option.environment ? ` (${option.environment})` : ""}
            </option>
          ))}
        </Select>
      )}
    </Field>
  );
}

function ConnectionForm({ draft, graph, candidates, onClose }: { draft: Draft; graph?: ConnectionGraph; candidates: WorkloadSummary[]; onClose: () => void }) {
  const id = useId();
  const online = useOnline();
  const create = useCreateConnection();
  const update = useUpdateConnection();
  const existing = draft.connection;
  const [source, setSource] = useState(existing?.source.id ?? draft.source ?? "");
  const [target, setTarget] = useState(existing?.target.id ?? draft.target ?? "");
  const services = (resourceId: string) => graph?.resources.find((resource) => resource.id === resourceId)?.services ?? [];
  const only = (resourceId: string) => (services(resourceId).length === 1 ? (services(resourceId)[0] ?? "") : "");
  const [sourceService, setSourceService] = useState(existing?.source.service ?? only(source));
  const [targetService, setTargetService] = useState(existing?.target.service ?? only(target));
  const [port, setPort] = useState(existing ? String(existing.port) : "");
  const nameOf = (resourceId: string) => candidates.find((candidate) => candidate.id === resourceId)?.name ?? graph?.resources.find((resource) => resource.id === resourceId)?.name ?? "";
  const [alias, setAlias] = useState(existing?.alias ?? (target ? suggestedAlias(nameOf(target)) : ""));
  const aliasEdited = useRef(Boolean(existing));
  const mutation = existing ? update : create;
  const targets = candidates.filter((candidate) => candidate.kind !== "job" && candidate.id !== source);
  // A managed database accepts connections only to its own service, native port and generated hostname.
  const targetIsDatabase = candidates.find((candidate) => candidate.id === target)?.kind === "database";
  const targetDetail = useQuery({ ...workloadQuery(target), enabled: Boolean(target) && targetIsDatabase });
  const database = targetIsDatabase ? targetDetail.data?.database : undefined;
  const endpoints = database ? databaseEndpoints(database) : [];
  const endpoint = endpoints.find((item) => String(item.port) === port) ?? endpoints[0];
  const effectiveTargetService = endpoint ? endpoint.service : targetService;
  const effectiveAlias = endpoint ? endpoint.alias : alias;
  const effectivePort = endpoint ? String(endpoint.port) : port;
  const portNumber = Number(effectivePort);
  const aliasError = effectiveAlias !== "" && !validAlias(effectiveAlias) ? "Use a lowercase DNS name such as postgres.internal. Names starting with cove- are reserved." : undefined;
  const valid = source && target && sourceService && effectiveTargetService && Number.isInteger(portNumber) && portNumber >= 1 && portNumber <= 65535 && validAlias(effectiveAlias) && (!targetIsDatabase || database);

  function submit(event: FormEvent) {
    event.preventDefault();
    if (!valid || mutation.isPending) return;
    const input = { source_service: sourceService.trim(), target_service: effectiveTargetService.trim(), port: portNumber, alias: effectiveAlias.trim() };
    if (existing) update.mutate({ id: existing.id, ...input }, { onSuccess: onClose });
    else create.mutate({ source_id: source, target_id: target, ...input }, { onSuccess: onClose });
  }

  const serviceField = (role: "source" | "target", resourceId: string, value: string, onChange: (value: string) => void) => {
    const known = services(resourceId);
    const fieldId = `${id}-${role}-service`;
    return (
      <Field id={fieldId} label={role === "source" ? "Source service" : "Target service"} hint={known.length === 0 && resourceId ? "Enter the Compose service name from the repository." : undefined}>
        {known.length > 0 ? (
          <Select id={fieldId} value={value} required onChange={(event) => onChange(event.target.value)}>
            <option value="">Select service</option>
            {known.map((service) => (
              <option key={service} value={service}>
                {service}
              </option>
            ))}
          </Select>
        ) : (
          <input id={fieldId} className={inputClass} value={value} required maxLength={64} onChange={(event) => onChange(event.target.value)} aria-describedby={resourceId ? `${fieldId}-hint` : undefined} />
        )}
      </Field>
    );
  };

  return (
    <form onSubmit={submit} className="space-y-4 text-fg">
      <fieldset disabled={mutation.isPending} className="space-y-4">
        <div className="grid gap-4 sm:grid-cols-2">
          <EndpointField
            id={`${id}-source`}
            label="Source"
            placeholder="Select resource"
            existing={existing?.source}
            value={source}
            options={candidates}
            onChange={(value) => {
              setSource(value);
              setSourceService(only(value));
            }}
          />
          <EndpointField
            id={`${id}-target`}
            label="Target"
            placeholder="Select service resource"
            existing={existing?.target}
            value={target}
            options={targets}
            onChange={(value) => {
              setTarget(value);
              setTargetService(only(value));
              if (!aliasEdited.current) setAlias(value ? suggestedAlias(nameOf(value)) : "");
            }}
          />
          {serviceField("source", source, sourceService, setSourceService)}
          {database ? (
            <>
              <Field id={`${id}-target-service`} label="Target service" hint="Set by the managed database.">
                <input id={`${id}-target-service`} className={`${inputClass} font-mono`} value={effectiveTargetService} readOnly aria-describedby={`${id}-target-service-hint`} />
              </Field>
              {endpoints.length > 1 ? (
                <Field id={`${id}-port`} label="Endpoint" hint="The native protocol, or another endpoint the database offers.">
                  <Select id={`${id}-port`} value={effectivePort} onChange={(event) => setPort(event.target.value)}>
                    {endpoints.map((item) => (
                      <option key={item.key} value={String(item.port)}>
                        {item.label}
                      </option>
                    ))}
                  </Select>
                </Field>
              ) : (
                <Field id={`${id}-port`} label="Target port" hint="The database's native port.">
                  <input id={`${id}-port`} className={inputClass} value={effectivePort} readOnly aria-describedby={`${id}-port-hint`} />
                </Field>
              )}
              <Field id={`${id}-alias`} label="Private hostname" hint={endpoint?.key === "pooler" ? "The pooler's generated hostname, used by its DB_POOL_HOST and DB_POOL_URL exports." : "The database's hostname, used by its DB_HOST and DB_URL exports."}>
                <input id={`${id}-alias`} className={`${inputClass} font-mono`} value={effectiveAlias} readOnly aria-describedby={`${id}-alias-hint`} />
              </Field>
            </>
          ) : targetIsDatabase ? (
            <p className="text-xs text-fg-muted sm:col-span-2">{targetDetail.isError ? `Could not read the database: ${errorMessage(targetDetail.error)}` : "Reading the database's connection details"}</p>
          ) : (
            <>
          {serviceField("target", target, targetService, setTargetService)}
          <Field id={`${id}-port`} label="Target port" hint="The container port the target service listens on.">
            <input id={`${id}-port`} className={inputClass} inputMode="numeric" pattern="[0-9]*" value={port} required onChange={(event) => setPort(event.target.value.replace(/\D/g, "").slice(0, 5))} aria-describedby={`${id}-port-hint`} />
          </Field>
          <Field id={`${id}-alias`} label="Private hostname" error={aliasError} hint="Only the source resolves this name.">
            <input
              id={`${id}-alias`}
              className={`${inputClass} font-mono`}
              value={alias}
              required
              maxLength={253}
              pattern={aliasPattern}
              aria-invalid={aliasError ? true : undefined}
              aria-describedby={aliasError ? `${id}-alias-error` : `${id}-alias-hint`}
              onChange={(event) => {
                setAlias(event.target.value.toLowerCase());
                aliasEdited.current = true;
              }}
            />
          </Field>
            </>
          )}
        </div>
        <p className="text-xs text-fg-muted">
          Protocol: TCP. UDP is not forwarded. The source connects to <span className="font-mono">{effectiveAlias || "hostname"}:{effectivePort || "port"}</span>; the target cannot start connections back. Other resources, including other sources of the same target, do not gain access. Both resources need edit access.
        </p>
      </fieldset>
      {mutation.isError && (
        <Notice tone="error" title="Could not save the connection">
          {errorMessage(mutation.error)}
        </Notice>
      )}
      <div className="flex flex-wrap gap-2">
        <Button type="submit" variant="primary" pending={mutation.isPending} disabled={!online || !valid}>
          {existing ? "Save connection" : "Create connection"}
        </Button>
        <Button variant="ghost" onClick={onClose} disabled={mutation.isPending}>
          Cancel
        </Button>
      </div>
    </form>
  );
}

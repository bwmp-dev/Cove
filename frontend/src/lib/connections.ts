import type { Permission } from "../api/types";
import type { Tone } from "./nodes";
import { operationTones } from "./operations";

export type ConnectionState = "applied" | "pending" | "error" | "revoking";

export interface GraphResource {
  id: string;
  name: string;
  kind: "service" | "job";
  project_id: string;
  project: string;
  environment_id: string;
  environment: string;
  node_id: string;
  node: string;
  node_state: string;
  manually_stopped: boolean;
  last_action: string;
  last_state: string;
  services: string[];
  permissions: Permission[];
  private_links: boolean;
  tailscale_service?: string;
}

export interface ConnectionEndpoint {
  id?: string;
  name?: string;
  service?: string;
  node_id?: string;
  restricted: boolean;
}

export interface ConnectionSide {
  role: "source" | "target";
  state: "applied" | "pending" | "error";
  detail?: string;
}

export interface PrivateConnection {
  id: string;
  source: ConnectionEndpoint;
  target: ConnectionEndpoint;
  port: number;
  alias?: string;
  protocol: "tcp";
  cross_node: boolean;
  state: ConnectionState;
  detail?: string;
  sides: ConnectionSide[];
  broader_access: string[];
  can_edit: boolean;
  can_remove: boolean;
  can_retry: boolean;
  updated_at: string;
}

export interface SharedNetwork {
  name: string;
  resources: string[];
}

export interface ConnectionGraph {
  resources: GraphResource[];
  connections: PrivateConnection[];
  shared_networks: SharedNetwork[];
}

export interface ConnectionInput {
  source_id: string;
  source_service: string;
  target_id: string;
  target_service: string;
  port: number;
  alias: string;
}

export const connectionStates: Record<ConnectionState, { tone: Tone; label: string }> = {
  applied: { tone: "ok", label: "Active" },
  pending: { tone: "info", label: "Applying" },
  error: { tone: "bad", label: "Error" },
  revoking: { tone: "info", label: "Revoking" },
};

export function resourceStatus(resource: Pick<GraphResource, "manually_stopped" | "last_action" | "last_state">): { tone: Tone; label: string } {
  if (resource.manually_stopped) return { tone: "neutral", label: "Stopped" };
  switch (resource.last_state) {
    case "queued":
    case "running":
      return { tone: "info", label: "Operation in progress" };
    case "succeeded":
      return { tone: "ok", label: "Running" };
    case "":
      return { tone: "neutral", label: "Not deployed" };
    case "failed":
    case "interrupted":
    case "cancelled":
      return { tone: operationTones[resource.last_state], label: `Last ${resource.last_action || "operation"} ${resource.last_state}` };
  }
  return { tone: "warn", label: `Last ${resource.last_action || "operation"} ${resource.last_state}` };
}

// A private hostname suggestion derived from the target's name, such as "postgres.internal".
export function suggestedAlias(name: string) {
  const slug = name
    .toLowerCase()
    .replace(/[^a-z0-9]+/g, "-")
    .replace(/^-+|-+$/g, "")
    .slice(0, 50)
    .replace(/-+$/, "");
  return `${slug.startsWith("cove-") || slug === "" ? `app-${slug}`.replace(/-$/, "") : slug}.internal`;
}

export const aliasPattern = "[a-z0-9]([a-z0-9\\-]*[a-z0-9])?(\\.[a-z0-9]([a-z0-9\\-]*[a-z0-9])?)*";

export function validAlias(alias: string) {
  return alias.length <= 253 && new RegExp(`^${aliasPattern}$`).test(alias) && alias !== "localhost" && !alias.startsWith("cove-");
}

export function endpointLabel(endpoint: ConnectionEndpoint) {
  if (endpoint.restricted) return "Resource you cannot view";
  return endpoint.service ? `${endpoint.name} · ${endpoint.service}` : (endpoint.name ?? "");
}

export const NODE_WIDTH = 208;
export const NODE_HEIGHT = 72;
// Wide enough for a hostname:port label between columns.
const COLUMN_GAP = 184;
const ROW_GAP = 28;
const PADDING = 16;

export interface GraphNodeLayout {
  id: string;
  x: number;
  y: number;
}

// Places nodes in columns by their longest incoming path, so connections mostly point right. Cycles
// (for example two connections in opposite directions) are cut at the node count.
export function layoutGraph(nodeIds: string[], edges: { source: string; target: string }[]) {
  const rank = new Map(nodeIds.map((id) => [id, 0]));
  const connected = new Set<string>();
  for (const edge of edges) {
    if (rank.has(edge.source) && rank.has(edge.target) && edge.source !== edge.target) {
      connected.add(edge.source);
      connected.add(edge.target);
    }
  }
  const limit = Math.max(nodeIds.length - 1, 0);
  for (let pass = 0; pass < nodeIds.length; pass++) {
    let changed = false;
    for (const edge of edges) {
      const from = rank.get(edge.source);
      const to = rank.get(edge.target);
      if (from === undefined || to === undefined || edge.source === edge.target) continue;
      const next = Math.min(from + 1, limit);
      if (next > to) {
        rank.set(edge.target, next);
        changed = true;
      }
    }
    if (!changed) break;
  }
  const columns: string[][] = [];
  const ordered = [...nodeIds.filter((id) => connected.has(id)), ...nodeIds.filter((id) => !connected.has(id))];
  for (const id of ordered) {
    const column = rank.get(id) ?? 0;
    (columns[column] ??= []).push(id);
  }
  const nodes = new Map<string, GraphNodeLayout>();
  let height = 0;
  columns.forEach((column, index) => {
    column.forEach((id, row) => {
      const y = PADDING + row * (NODE_HEIGHT + ROW_GAP);
      nodes.set(id, { id, x: PADDING + index * (NODE_WIDTH + COLUMN_GAP), y });
      height = Math.max(height, y + NODE_HEIGHT + PADDING);
    });
  });
  const width = PADDING * 2 + Math.max(columns.length, 1) * NODE_WIDTH + Math.max(columns.length - 1, 0) * COLUMN_GAP;
  return { nodes, width, height: Math.max(height, NODE_HEIGHT + PADDING * 2) };
}

// A curve from the source's right edge to the target's left edge. Connections that point backwards
// or within one column bend around so they never overlap a node's text.
export function edgePath(source: GraphNodeLayout, target: GraphNodeLayout, offset = 0) {
  const sy = source.y + NODE_HEIGHT / 2 + offset;
  const ty = target.y + NODE_HEIGHT / 2 + offset;
  if (target.x > source.x) {
    const sx = source.x + NODE_WIDTH;
    const tx = target.x;
    const bend = Math.max((tx - sx) / 2, 40);
    return { d: `M ${sx} ${sy} C ${sx + bend} ${sy}, ${tx - bend} ${ty}, ${tx} ${ty}`, label: { x: (sx + tx) / 2, y: (sy + ty) / 2 } };
  }
  const sx = source.x + NODE_WIDTH;
  const tx = target.x + NODE_WIDTH;
  const reach = Math.max(sx, tx) + 56 + Math.abs(offset);
  return { d: `M ${sx} ${sy} C ${reach} ${sy}, ${reach} ${ty}, ${tx} ${ty}`, label: { x: reach - 14, y: (sy + ty) / 2 } };
}

// Moves edge labels apart so every label stays visible and clickable. Labels closer than the label
// width horizontally and the gap vertically are pushed down in order.
export function separateLabels(points: { x: number; y: number }[], gap = 26, width = 168) {
  const placed: { x: number; y: number }[] = points.map((point) => ({ ...point }));
  const order = placed.map((_, index) => index).sort((a, b) => placed[a]!.y - placed[b]!.y || placed[a]!.x - placed[b]!.x);
  for (let i = 0; i < order.length; i++) {
    const current = placed[order[i]!]!;
    for (let j = 0; j < i; j++) {
      const other = placed[order[j]!]!;
      if (Math.abs(other.x - current.x) < width && Math.abs(other.y - current.y) < gap) {
        current.y = other.y + gap;
        j = -1;
      }
    }
  }
  return placed;
}

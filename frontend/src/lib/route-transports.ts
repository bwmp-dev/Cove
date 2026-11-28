import type { Listener } from "../api/ingress-listeners";
import type { Route, RoutePolicy, RouteVisibility } from "../api/types";
import { listenerName, protocolLabels } from "./ingress-listeners";
import { isWildcardHostname } from "./certificates";
import { canonical, cidrErrors, cidrList, isComposeService, isPrivateVisibility, listenerBindings, routeLimits, upstreamErrors, visibilityNames, type UpstreamRow } from "./workload-form";

export type TransportProtocol = "tcp" | "udp";

// ipAllowList is kept while a row is switched to UDP but only sent for TCP.
export interface TransportRow {
  id: string;
  protocol: TransportProtocol;
  service: string;
  port: number;
  // Empty until people choose; the API requires an explicit visibility.
  visibility: "" | RouteVisibility;
  listener: string;
  ipAllowList: string;
  upstreams: UpstreamRow[];
}

export const transportProtocol = 6;

export function isTransportRoute(route: Pick<Route, "protocol">): boolean {
  return route.protocol === "tcp" || route.protocol === "udp";
}

// Hostnames reachable from the internet. Raw routes have no hostname, and private hostnames are served only on a LAN
// or Tailscale listener, so neither may become a link, uptime target or public status row.
export function isPublicHTTPRoute(route: Pick<Route, "protocol" | "policy">): boolean {
  return !isTransportRoute(route) && !isPrivateVisibility(route.policy?.visibility);
}

// Public hostnames whose certificate the worker requests from Let's Encrypt over HTTP-01. Wildcards, stored certificates
// and the proxy default are excluded, matching the worker's automatic certificate retries.
export function isPublicACMERoute(route: Pick<Route, "protocol" | "domain" | "policy">): boolean {
  const source = route.policy?.certificate_source;
  return isPublicHTTPRoute(route) && !isWildcardHostname(route.domain) && !route.policy?.certificate_id && (!source || source === "acme");
}

// A wildcard is a pattern, not a URL.
export function isLinkableRoute(route: Pick<Route, "protocol" | "domain" | "policy">): boolean {
  return isPublicHTTPRoute(route) && !isWildcardHostname(route.domain);
}

export function splitRoutes(routes: Route[] | null | undefined) {
  const list = routes ?? [];
  return { http: list.filter((route) => !isTransportRoute(route)), transport: list.filter(isTransportRoute) };
}

export function emptyTransport(): TransportRow {
  return { id: crypto.randomUUID(), protocol: "tcp", service: "", port: NaN, visibility: "", listener: "", ipAllowList: "", upstreams: [] };
}

export function transportRows(routes: Route[]): TransportRow[] {
  return routes.filter(isTransportRoute).map((route) => ({
    id: crypto.randomUUID(),
    protocol: route.protocol === "udp" ? "udp" : "tcp",
    service: route.service,
    port: route.port,
    visibility: route.policy?.visibility ?? "",
    listener: route.policy?.listener ?? "",
    ipAllowList: (route.policy?.ip_allow_list ?? []).join("\n"),
    upstreams: (route.policy?.upstreams ?? []).map((target) => ({ id: crypto.randomUUID(), ...target })),
  }));
}

export function transportOf(row: TransportRow): Route {
  const policy: RoutePolicy = { listener: row.listener };
  if (row.visibility) policy.visibility = row.visibility;
  const cidrs = cidrList(row.ipAllowList);
  if (row.protocol === "tcp" && cidrs.length > 0) policy.ip_allow_list = cidrs;
  if (row.upstreams.length > 0) policy.upstreams = row.upstreams.map((target) => ({ service: target.service.trim(), port: target.port }));
  return { protocol: row.protocol, domain: "", service: row.service.trim(), port: row.port, policy };
}

export function sameTransports(rows: TransportRow[], saved: Route[]) {
  return JSON.stringify(canonical(rows.map(transportOf))) === JSON.stringify(canonical(saved));
}

function sameAsSaved(route: Route, saved: Route[]) {
  const value = JSON.stringify(canonical(route));
  return saved.some((item) => JSON.stringify(canonical(item)) === value);
}

export interface TransportContext {
  // undefined while unknown or unreadable, which allows only saved routes unchanged.
  routeProtocol: number | undefined;
  // undefined while unknown, which leaves listener checks to the API.
  listeners: Listener[] | undefined;
  saved: Route[];
  httpCount: number;
}

// Each listener carries one raw route, which the control plane also enforces across resources. Saved routes are kept
// unchanged when the worker is older or its route protocol is unknown, for the control plane to judge.
export function validateTransports(rows: TransportRow[], context: TransportContext): { routes?: Route[]; errors: Record<string, string> } {
  const errors: Record<string, string> = {};
  const { routeProtocol, listeners, saved, httpCount } = context;
  if (rows.length + httpCount > routeLimits.routes) errors.transports = `A resource can have at most ${routeLimits.routes} routes, HTTP, TCP and UDP together.`;
  const claimed = new Set<string>();
  const routes = rows.map((row) => {
    const route = transportOf(row);
    if ((routeProtocol === undefined || routeProtocol < transportProtocol) && !sameAsSaved(route, saved)) {
      errors[row.id] = routeProtocol === undefined ? "TCP and UDP routes can be changed once the worker capabilities of this node are known. Remove the route or try again later." : "TCP and UDP routes need a newer worker on this node. Upgrade it and verify the node again.";
      return route;
    }
    const service = row.service.trim();
    if (!isComposeService(service)) errors[row.id] = "Enter the Compose service name.";
    else if (!Number.isInteger(row.port) || row.port < 1 || row.port > 65535) errors[row.id] = "Use a container port from 1 to 65535.";
    if (!row.visibility) errors[`${row.id}.visibility`] = "Choose who can connect: Public, LAN only or Tailscale only.";
    const label = protocolLabels[row.protocol];
    if (row.listener === "") errors[`${row.id}.listener`] = `Choose the ${label} listener that receives this traffic.`;
    else if (!listenerName.test(row.listener)) errors[`${row.id}.listener`] = "This listener ID is not valid. Choose a listener from the node.";
    else if (claimed.has(row.listener)) errors[`${row.id}.listener`] = `Listener ${row.listener} already carries another route here. Each listener carries one TCP or UDP route.`;
    else if (listeners) {
      const match = listeners.find((item) => item.id === row.listener);
      if (!match) errors[`${row.id}.listener`] = `Listener ${row.listener} is not configured on this node. Choose another listener.`;
      else if (match.protocol !== row.protocol) errors[`${row.id}.listener`] = `Listener ${row.listener} carries ${protocolLabels[match.protocol]}. Choose a ${label} listener.`;
      else if (row.visibility && match.visibility !== row.visibility) errors[`${row.id}.listener`] = `Listener ${row.listener} has ${visibilityNames[match.visibility]} visibility. Choose a ${visibilityNames[row.visibility]} ${label} listener or change the visibility.`;
    }
    claimed.add(row.listener);
    if (row.protocol === "tcp") cidrErrors(row.id, row.ipAllowList, errors);
    upstreamErrors(row.id, service, row.port, row.upstreams, errors);
    return route;
  });
  return Object.keys(errors).length > 0 ? { errors } : { routes, errors };
}

// Listener and visibility errors shown after a save attempt are re-checked as people edit, and errors of removed rows
// are dropped.
export function refreshTransportErrors(errors: Record<string, string>, previous: TransportRow[], rows: TransportRow[], context: TransportContext) {
  const ids = new Set(rows.map((row) => row.id));
  const removed = new Set(previous.filter((row) => !ids.has(row.id)).map((row) => row.id));
  const owner = (key: string) => key.split(".")[0] ?? "";
  const next = Object.fromEntries(Object.entries(errors).filter(([key]) => !removed.has(owner(key))));
  const shown = Object.keys(next).filter((key) => key === "transports" || ((key.endsWith(".listener") || key.endsWith(".visibility")) && ids.has(owner(key))));
  if (shown.length === 0) return removed.size > 0 ? next : errors;
  const current = validateTransports(rows, context).errors;
  for (const key of shown) {
    if (current[key]) next[key] = current[key];
    else delete next[key];
  }
  return next;
}

export function transportSummary(route: Route, list?: Listener[]) {
  const protocol = route.protocol === "udp" ? "UDP" : "TCP";
  const visibility = route.policy?.visibility;
  const id = route.policy?.listener ?? "";
  const listener = list?.find((item) => item.id === id);
  const parts = [protocol, visibility ? (visibility === "public" ? "Public" : `${visibilityNames[visibility]} only`) : "visibility not set", id ? `listener ${id}${listener ? ` (${listenerBindings(listener)})` : ""}` : "no listener"];
  return parts.join(" · ");
}

export function transportTargets(route: Route) {
  return [{ service: route.service, port: route.port }, ...(route.policy?.upstreams ?? [])].map((target) => `${target.service}:${target.port}`).join(", ");
}

import type { GitSourceInput, Route, RoutePolicy, Workload, WorkloadInput, WorkloadKind } from "../api/types";
import type { ResourceDraft } from "./templates";

export interface EnvRow {
  id: string;
  key: string;
  value: string;
}

export type SourceKind = "compose" | "git";

export interface WorkloadFormValues {
  name: string;
  nodeId: string;
  kind: WorkloadKind;
  jobService: string;
  sourceKind: SourceKind;
  compose: string;
  repository: string;
  branch: string;
  composePath: string;
  inlineCompose?: boolean;
  watchPaths?: string;
  token: string;
  // null keeps the saved environment unchanged (edit only).
  environment: EnvRow[] | null;
}

export type WorkloadFormErrors = Partial<Record<"name" | "nodeId" | "jobService" | "compose" | "repository" | "branch" | "composePath" | "watchPaths" | "token" | "environment" | "form", string>> & {
  rows?: Record<string, string>;
};

const environmentKey = /^[A-Za-z_][A-Za-z0-9_]*$/;
const MiB = 1024 * 1024;
const encoder = new TextEncoder();

export function byteLength(text: string) {
  return encoder.encode(text).length;
}

export function composeLimit() {
  return MiB;
}

export function validateEnvironmentKey(key: string): string | null {
  if (key === "") return "Enter a name.";
  if (!environmentKey.test(key)) return "Use letters, digits, and underscores, not starting with a digit.";
  if (key === "PATH" || key.startsWith("DOCKER_") || key.startsWith("COMPOSE_") || key.startsWith("LD_")) {
    return "PATH and DOCKER_, COMPOSE_, LD_ prefixes are reserved.";
  }
  return null;
}

export function validateRepository(value: string): string | null {
  if (value === "") return "Enter the repository URL.";
  let url: URL;
  try {
    url = new URL(value);
  } catch {
    return "Enter a valid URL.";
  }
  if (url.protocol !== "https:" || url.host === "") return "Use an HTTPS URL.";
  if (url.username || url.password) return "Remove credentials from the URL; use the access token field instead.";
  if (url.search || url.hash) return "Remove the query string or fragment.";
  return null;
}

export function validateBranch(value: string): string | null {
  if (value === "") return "Enter a branch.";
  if (value.length > 200 || value.startsWith("-") || /[\s~^:?*[\\]/.test(value) || value.includes("..")) {
    return "Enter a valid Git branch name.";
  }
  return null;
}

export function validateComposePath(value: string): string | null {
  if (value === "") return "Enter the Compose file path.";
  if (value.startsWith("/") || value.includes("\\") || value.includes(":")) return "Use a path relative to the repository root.";
  const segments = value.split("/").filter((segment) => segment !== "" && segment !== ".");
  let depth = 0;
  for (const segment of segments) {
    depth += segment === ".." ? -1 : 1;
    if (depth < 0) return "The path must stay inside the repository.";
  }
  if (depth === 0) return "Enter a file path.";
  return null;
}

// Parses KEY=VALUE lines from a pasted .env file. Returns null when the text does not look like one.
export function parseDotenv(text: string): { key: string; value: string }[] | null {
  const entries: { key: string; value: string }[] = [];
  for (const raw of text.split(/\r?\n/)) {
    const line = raw.trim();
    if (line === "" || line.startsWith("#")) continue;
    const match = /^(?:export\s+)?([^=\s]+)\s*=\s*(.*)$/.exec(line);
    if (!match) return null;
    let value = match[2] ?? "";
    const quote = value.charAt(0);
    if ((quote === '"' || quote === "'") && value.length >= 2 && value.endsWith(quote)) {
      value = value.slice(1, -1);
      if (quote === '"') value = value.replace(/\\n/g, "\n").replace(/\\"/g, '"');
    }
    entries.push({ key: match[1] ?? "", value });
  }
  return entries.length > 0 ? entries : null;
}

export function validateWorkload(values: WorkloadFormValues, mode: "create" | "edit"): { input?: WorkloadInput; errors: WorkloadFormErrors } {
  const errors: WorkloadFormErrors = {};
  const name = values.name.trim();
  if (name === "") errors.name = "Enter a name.";
  else if (name.length > 120) errors.name = "Use at most 120 characters.";
  if (mode === "create" && values.nodeId === "") errors.nodeId = "Select a ready node.";
  const jobService = values.jobService.trim();
  if (values.kind === "job" && !/^[A-Za-z0-9_-]{1,64}$/.test(jobService)) {
    errors.jobService = "Enter the Compose service name whose exit code decides the result.";
  }

  let source: GitSourceInput | null = null;
  if (values.sourceKind === "compose" || values.inlineCompose) {
    if (values.compose.trim() === "") errors.compose = "Enter a Compose file.";
    else if (byteLength(values.compose) > MiB) errors.compose = "The Compose file must not exceed 1 MiB.";
  }
  if (values.sourceKind === "git") {
    const repository = values.repository.trim();
    const branch = values.branch.trim();
    const composePath = values.inlineCompose ? "" : values.composePath.trim();
    const repositoryError = validateRepository(repository);
    const branchError = validateBranch(branch);
    const pathError = values.inlineCompose ? null : validateComposePath(composePath);
    if (repositoryError) errors.repository = repositoryError;
    if (branchError) errors.branch = branchError;
    if (pathError) errors.composePath = pathError;
    if (values.token.length > 8192 || /[\r\n\0]/.test(values.token)) errors.token = "Enter a single-line token.";
    const watchPaths = (values.watchPaths ?? "").split(/\r?\n/).map((p) => p.trim()).filter(Boolean);
    if (watchPaths.length > 64 || watchPaths.some((p) => p.length > 256 || p === "!" || p.startsWith("/") || p.startsWith("!/") || (p.includes("\\") || [...p].some((c) => c.charCodeAt(0) < 32)) || p.replace(/^!/, "").split("/").some((part) => part === ".." || part === "."))) errors.watchPaths = "Watch paths must be relative patterns, at most 64 patterns of 256 characters.";
    source = { repository, branch, compose_path: composePath, watch_paths: watchPaths };
    if (values.inlineCompose) source.inline_compose = true;
    if (values.token !== "") source.token = values.token;
  }

  let environment: Record<string, string> | undefined;
  if (values.environment) {
    const rows: Record<string, string> = {};
    const seen = new Set<string>();
    environment = {};
    for (const row of values.environment) {
      const keyError = validateEnvironmentKey(row.key);
      if (keyError) rows[row.id] = keyError;
      else if (seen.has(row.key)) rows[row.id] = "Duplicate name.";
      else if (row.value.length > 65536 || row.value.includes("\0")) rows[row.id] = "Values are limited to 64 KiB of text.";
      seen.add(row.key);
      environment[row.key] = row.value;
    }
    if (Object.keys(rows).length > 0) {
      errors.rows = rows;
      errors.environment = "Fix the highlighted variables.";
    }
  }

  if (Object.keys(errors).length > 0) return { errors };

  const input: WorkloadInput = {
    name,
    node_id: values.nodeId,
    kind: values.kind,
    compose: values.sourceKind === "compose" || values.inlineCompose ? values.compose : "",
    source,
  };
  if (values.kind === "job") input.job_service = jobService;
  if (environment) input.environment = environment;
  if (byteLength(JSON.stringify(input)) > 2 * MiB - 1024) {
    return { errors: { form: "The configuration exceeds the 2 MiB request limit." } };
  }
  return { input, errors };
}

let nextRowId = 0;

export function createRow(key = "", value = ""): EnvRow {
  nextRowId += 1;
  return { id: `env-${nextRowId}`, key, value };
}

export function formValues(workload: Workload | undefined, draft?: ResourceDraft): WorkloadFormValues {
  return {
    name: workload?.name ?? draft?.name ?? "",
    nodeId: workload?.node_id ?? "",
    kind: workload?.kind === "job" ? "job" : "service",
    jobService: workload?.job_service ?? "",
    sourceKind: workload?.source ? "git" : "compose",
    compose: workload?.compose ?? draft?.compose ?? "",
    repository: workload?.source?.repository ?? "",
    branch: workload?.source?.branch ?? "main",
    composePath: workload?.source?.compose_path ?? "compose.yaml",
    inlineCompose: workload?.source?.inline_compose ?? false,
    watchPaths: workload?.source?.watch_paths?.join("\n") ?? "",
    token: "",
    environment: workload ? null : Object.entries(draft?.environment ?? {}).map(([key, value]) => createRow(key, value)),
  };
}

export interface HeaderRow {
  id: string;
  name: string;
  value: string;
}

export interface UpstreamRow {
  id: string;
  service: string;
  port: number;
}

// Editable form of a route policy. Header maps and upstreams become rows so each one has its own field and error.
export interface PolicyDraft {
  pathPrefix: string;
  requestHeaders: HeaderRow[];
  responseHeaders: HeaderRow[];
  securityHeaders: boolean;
  ipAllowList: string;
  redirectHost: string;
  redirectPermanent: boolean;
  // Empty keeps an omitted value omitted; the worker treats it like "acme".
  certificateSource: "" | "acme" | "default";
  upstreams: UpstreamRow[];
  sticky: boolean;
}

export interface RouteRow {
  id: string;
  domain: string;
  service: string;
  port: number;
  auth?: string;
  policy?: PolicyDraft;
}

export const routeLimits = { headers: 32, cidrs: 64, upstreams: 15 } as const;

export function emptyPolicy(): PolicyDraft {
  return { pathPrefix: "", requestHeaders: [], responseHeaders: [], securityHeaders: false, ipAllowList: "", redirectHost: "", redirectPermanent: false, certificateSource: "", upstreams: [], sticky: false };
}

function headerRows(headers: Record<string, string> | undefined): HeaderRow[] {
  return Object.entries(headers ?? {}).map(([name, value]) => ({ id: crypto.randomUUID(), name, value }));
}

function policyDraft(policy: RoutePolicy): PolicyDraft {
  return {
    pathPrefix: policy.path_prefix ?? "",
    requestHeaders: headerRows(policy.request_headers),
    responseHeaders: headerRows(policy.response_headers),
    securityHeaders: policy.security_headers ?? false,
    ipAllowList: (policy.ip_allow_list ?? []).join("\n"),
    redirectHost: policy.redirect_host ?? "",
    redirectPermanent: policy.redirect_permanent ?? false,
    certificateSource: policy.certificate_source ?? "",
    upstreams: (policy.upstreams ?? []).map((target) => ({ id: crypto.randomUUID(), ...target })),
    sticky: policy.sticky ?? false,
  };
}

export function routeRows(routes: Route[] | undefined): RouteRow[] {
  return (routes ?? []).map(({ policy, ...route }) => ({ ...route, id: crypto.randomUUID(), ...(policy ? { policy: policyDraft(policy) } : {}) }));
}

export function cidrList(text: string) {
  return text.split(/[\s,]+/).filter(Boolean);
}

function headerMap(rows: HeaderRow[]) {
  return Object.fromEntries(rows.map((row) => [row.name.trim(), row.value]));
}

// Leaves out empty settings so a route without any policy keeps the legacy shape every worker accepts.
function policyOf(draft: PolicyDraft): RoutePolicy | undefined {
  const policy: RoutePolicy = {};
  const path = draft.pathPrefix.trim();
  const redirect = draft.redirectHost.trim().toLowerCase();
  const cidrs = cidrList(draft.ipAllowList);
  if (path) policy.path_prefix = path;
  if (draft.requestHeaders.length > 0) policy.request_headers = headerMap(draft.requestHeaders);
  if (draft.responseHeaders.length > 0) policy.response_headers = headerMap(draft.responseHeaders);
  if (draft.securityHeaders) policy.security_headers = true;
  if (cidrs.length > 0) policy.ip_allow_list = cidrs;
  if (redirect) {
    policy.redirect_host = redirect;
    if (draft.redirectPermanent) policy.redirect_permanent = true;
  }
  if (draft.certificateSource) policy.certificate_source = draft.certificateSource;
  if (draft.upstreams.length > 0) policy.upstreams = draft.upstreams.map((target) => ({ service: target.service.trim(), port: target.port }));
  if (draft.sticky) policy.sticky = true;
  return Object.keys(policy).length > 0 ? policy : undefined;
}

function routeOf(row: RouteRow): Route {
  const route: Route = { domain: row.domain.trim().toLowerCase(), service: row.service.trim(), port: row.port };
  if (row.auth) route.auth = row.auth;
  const policy = row.policy && policyOf(row.policy);
  if (policy) route.policy = policy;
  return route;
}

export function routesUsePolicies(rows: RouteRow[]) {
  return rows.some((row) => row.policy !== undefined && policyOf(row.policy) !== undefined);
}

const domainLabel = /^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$/;
const composeService = /^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,63}$/;
const routePath = /^\/[a-zA-Z0-9/_.~%+@:-]*$/;
const headerName = /^[!#$%&'*+.^_`|~0-9A-Za-z-]+$/;
// Matches the worker, which rejects every control character in header values.
function hasControlCharacter(value: string): boolean {
  return [...value].some((c) => {
    const code = c.charCodeAt(0);
    return code < 32 || code === 127;
  });
}
const proxyHeaders = new Set(["host", "forwarded", "connection", "upgrade", "transfer-encoding", "content-length", "te", "trailer", "proxy-authorization", "proxy-authenticate"]);
const presetHeaders = new Set(["strict-transport-security", "x-content-type-options", "x-frame-options", "referrer-policy"]);

function isHostname(value: string) {
  return value.length <= 253 && value.includes(".") && value.split(".").every((label) => domainLabel.test(label));
}

function isPrefixLength(bits: string, max: number) {
  return /^(0|[1-9][0-9]{0,2})$/.test(bits) && Number(bits) <= max;
}

function isIPv4(value: string) {
  const parts = value.split(".");
  return parts.length === 4 && parts.every((part) => /^(0|[1-9][0-9]{0,2})$/.test(part) && Number(part) <= 255);
}

function isIPv6(value: string) {
  if (!value.includes(":") || !/^[0-9a-fA-F:.]+$/.test(value)) return false;
  const tail = value.lastIndexOf(":");
  const last = value.slice(tail + 1);
  const embedded = last.includes(".");
  if (embedded && !isIPv4(last)) return false;
  const groups = embedded ? `${value.slice(0, tail + 1)}0:0` : value;
  const halves = groups.split("::");
  if (halves.length > 2) return false;
  const parts = halves.map((half) => (half === "" ? [] : half.split(":")));
  if (parts.some((list) => list.some((group) => !/^[0-9a-fA-F]{1,4}$/.test(group)))) return false;
  const count = parts.reduce((total, list) => total + list.length, 0);
  return halves.length === 2 ? count < 8 : count === 8;
}

// Accepts the CIDR forms Go's netip.ParsePrefix accepts: no zones and no leading zeros.
export function isCIDR(value: string) {
  const slash = value.indexOf("/");
  if (slash < 0) return false;
  const address = value.slice(0, slash);
  const bits = value.slice(slash + 1);
  if (isIPv4(address)) return isPrefixLength(bits, 32);
  return isIPv6(address) && isPrefixLength(bits, 128);
}

function headerErrors(rows: HeaderRow[], key: string, preset: boolean, errors: Record<string, string>) {
  const seen = new Set<string>();
  for (const row of rows) {
    const name = row.name.trim();
    const lower = name.toLowerCase();
    const field = `${key}.${row.id}`;
    if (name === "") errors[field] = "Enter a header name.";
    else if (name.length > 128 || !headerName.test(name)) errors[field] = "Use a header name of up to 128 characters without spaces or separators.";
    else if (seen.has(lower)) errors[field] = "Each header can be set once.";
    else if (proxyHeaders.has(lower) || lower.startsWith("x-forwarded-")) errors[field] = "The proxy controls this header.";
    else if (preset && presetHeaders.has(lower)) errors[field] = "The security headers preset sets this header. Turn the preset off to set it here.";
    else if (row.value.length > 4096 || hasControlCharacter(row.value)) errors[field] = "Use a value of at most 4096 characters without tabs, line breaks or other control characters.";
    seen.add(lower);
  }
}

function policyErrors(row: RouteRow, domain: string, service: string, errors: Record<string, string>) {
  const draft = row.policy;
  if (!draft) return;
  const path = draft.pathPrefix.trim();
  if (path && (path.length > 1024 || !routePath.test(path))) errors[`${row.id}.path`] = "Start with / and use a URL path without a query string, spaces or wildcards.";
  const redirect = draft.redirectHost.trim().toLowerCase();
  if (redirect && (!isHostname(redirect) || redirect === domain)) errors[`${row.id}.redirect`] = "Use a DNS hostname different from this route's hostname.";
  for (const [key, rows] of [["request", draft.requestHeaders], ["response", draft.responseHeaders]] as const) {
    if (rows.length > routeLimits.headers) errors[`${row.id}.${key}`] = `Use at most ${routeLimits.headers} headers.`;
    headerErrors(rows, `${row.id}.${key}`, draft.securityHeaders, errors);
  }
  const cidrs = cidrList(draft.ipAllowList);
  const invalid = cidrs.find((cidr) => !isCIDR(cidr));
  if (cidrs.length > routeLimits.cidrs) errors[`${row.id}.cidr`] = `Use at most ${routeLimits.cidrs} ranges.`;
  else if (invalid) errors[`${row.id}.cidr`] = `${invalid} is not an IPv4 or IPv6 CIDR range such as 10.0.0.0/8.`;
  if (draft.upstreams.length > routeLimits.upstreams) errors[`${row.id}.upstreams`] = `Use at most ${routeLimits.upstreams} additional upstreams.`;
  const targets = new Set([`${service}:${row.port}`]);
  for (const target of draft.upstreams) {
    const name = target.service.trim();
    const field = `${row.id}.upstream.${target.id}`;
    if (!composeService.test(name)) errors[field] = "Enter the Compose service name.";
    else if (!Number.isInteger(target.port) || target.port < 1 || target.port > 65535) errors[field] = "Use a container port from 1 to 65535.";
    else if (targets.has(`${name}:${target.port}`)) errors[field] = "This service and port is already a target of the route.";
    targets.add(`${name}:${target.port}`);
  }
}

// Mirrors the worker's route validation so mistakes are reported next to the field before saving. Hostname, service
// and port errors are keyed by the row id; policy errors by "<row id>.<field>".
export function validateRoutes(rows: RouteRow[]): { routes?: Route[]; errors: Record<string, string> } {
  const errors: Record<string, string> = {};
  const seen = new Set<string>();
  const routes = rows.map((row) => {
    const route = routeOf(row);
    const { domain, service } = route;
    const path = route.policy?.path_prefix ?? "";
    const claim = `${domain} ${path}`;
    if (domain === "") errors[row.id] = "Enter a hostname.";
    else if (!isHostname(domain)) errors[row.id] = "Use a DNS hostname such as app.example.com, without a scheme, port, path or wildcard.";
    else if (seen.has(claim)) errors[row.id] = path ? "Each hostname and path prefix can be routed once." : "Each hostname can be routed once.";
    else if (!composeService.test(service)) errors[row.id] = "Enter the Compose service name.";
    else if (!Number.isInteger(row.port) || row.port < 1 || row.port > 65535) errors[row.id] = "Use a container port from 1 to 65535.";
    seen.add(claim);
    policyErrors(row, domain, service, errors);
    return route;
  });
  return Object.keys(errors).length > 0 ? { errors } : { routes, errors };
}

function canonical(value: unknown): unknown {
  if (Array.isArray(value)) return value.map(canonical);
  if (value && typeof value === "object") {
    return Object.fromEntries(
      Object.entries(value)
        .sort(([a], [b]) => (a < b ? -1 : a > b ? 1 : 0))
        .map(([key, item]) => [key, canonical(item)]),
    );
  }
  return value;
}

export function sameRoutes(rows: RouteRow[], routes: Route[] | undefined) {
  return JSON.stringify(canonical(rows.map(routeOf))) === JSON.stringify(canonical(routes ?? []));
}

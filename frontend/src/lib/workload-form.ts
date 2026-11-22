import type { GitSourceInput, Route, Workload, WorkloadInput, WorkloadKind } from "../api/types";
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

export type RouteRow = Route & { id: string };

export function routeRows(routes: Route[] | undefined): RouteRow[] {
  return (routes ?? []).map((route) => ({ ...route, id: crypto.randomUUID() }));
}

const domainLabel = /^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$/;
const composeService = /^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,63}$/;

// Mirrors the worker's route validation so mistakes are reported next to the field before saving.
export function validateRoutes(rows: RouteRow[]): { routes?: Route[]; errors: Record<string, string> } {
  const errors: Record<string, string> = {};
  const seen = new Set<string>();
  const routes = rows.map((row) => {
    const domain = row.domain.trim().toLowerCase();
    const service = row.service.trim();
    if (domain === "") errors[row.id] = "Enter a hostname.";
    else if (domain.length > 253 || !domain.includes(".") || !domain.split(".").every((label) => domainLabel.test(label))) {
      errors[row.id] = "Use a DNS hostname such as app.example.com, without a scheme, port, path or wildcard.";
    } else if (seen.has(domain)) errors[row.id] = "Each hostname can be routed once.";
    else if (!composeService.test(service)) errors[row.id] = "Enter the Compose service name.";
    else if (!Number.isInteger(row.port) || row.port < 1 || row.port > 65535) errors[row.id] = "Use a container port from 1 to 65535.";
    seen.add(domain);
    return { domain, service, port: row.port };
  });
  return Object.keys(errors).length > 0 ? { errors } : { routes, errors };
}

export function sameRoutes(rows: RouteRow[], routes: Route[] | undefined) {
  const saved = routes ?? [];
  return rows.length === saved.length && rows.every((row, index) => row.domain.trim().toLowerCase() === saved[index]?.domain && row.service.trim() === saved[index]?.service && row.port === saved[index]?.port);
}

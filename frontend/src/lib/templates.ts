import type { CatalogueTemplate, DraftRequest, TemplateEntry, TemplateVariable } from "../api/templates";

const dnsLabel = /^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$/;

export function validHostname(value: string) {
  return value.length <= 253 && value.includes(".") && value.split(".").every((label) => dnsLabel.test(label));
}

function httpsURL(value: string) {
  try {
    const url = new URL(value);
    return url.protocol === "https:" && url.hostname !== "" && !url.username && !url.password && !url.hash;
  } catch {
    return false;
  }
}

// Generated variables start empty: the server creates a fresh secret when the value is left blank.
export function initialValues(template: CatalogueTemplate): Record<string, string> {
  return Object.fromEntries(template.variables.map((variable) => [variable.key, variable.generated ? "" : variable.default]));
}

// Mirrors the server's typed checks so mistakes show beside the field; the server stays authoritative.
export function variableError(variable: TemplateVariable, value: string): string | null {
  if (value.includes("\0") || value.length > 65536) return "Use at most 64 KiB of text.";
  if (variable.generated) {
    return value === "" || /^[a-f0-9]{64}$/.test(value) ? null : "Use exactly 64 lowercase hexadecimal characters, or leave empty to generate one.";
  }
  if (value.trim() === "") return variable.required ? "Enter a value." : null;
  switch (variable.type) {
    case "integer":
      return /^[+-]?\d+$/.test(value) ? null : "Enter a whole number.";
    case "boolean":
      return value === "true" || value === "false" ? null : "Choose true or false.";
    case "hostname":
      return validHostname(value) ? null : "Use a lowercase DNS hostname such as app.example.com.";
    case "url":
      return httpsURL(value) ? null : "Use an HTTPS URL without credentials or a fragment.";
    case "email":
      return /^[^\s@<>]+@[^\s@<>]+\.[^\s@<>]+$/.test(value) ? null : "Enter an email address.";
    default:
      return null;
  }
}

export function hostnameError(value: string): string | null {
  const hostname = value.trim().toLowerCase();
  if (hostname === "") return "Enter the hostname for the HTTPS route.";
  return validHostname(hostname) ? null : "Use a DNS hostname such as app.example.com, without a scheme, port, path or wildcard.";
}

export function validateDraft(entry: TemplateEntry, values: Record<string, string>, hostname: string, acknowledged: boolean) {
  const errors: Record<string, string> = {};
  for (const variable of entry.template.variables) {
    const problem = variableError(variable, values[variable.key] ?? "");
    if (problem) errors[variable.key] = problem;
  }
  if (entry.template.web?.route) {
    const problem = hostnameError(hostname);
    if (problem) errors.hostname = problem;
  }
  if ((entry.template.capabilities ?? []).length > 0 && !acknowledged) errors.acknowledge = "Confirm that you reviewed the host access before continuing.";
  return errors;
}

// Unedited generated secrets are omitted so the server creates them; nothing secret is generated in the browser.
export function draftRequest(entry: TemplateEntry, values: Record<string, string>, hostname: string, acknowledged: boolean): DraftRequest {
  const supplied: Record<string, string> = {};
  for (const variable of entry.template.variables) {
    const value = values[variable.key] ?? "";
    if (variable.generated && value === "") continue;
    supplied[variable.key] = value;
  }
  return {
    origin: entry.origin,
    repository_id: entry.repository_id,
    id: entry.template.id,
    revision: entry.revision,
    values: supplied,
    hostname: entry.template.web?.route ? hostname.trim().toLowerCase() : "",
    acknowledge_host_access: acknowledged,
  };
}

export const capabilityLabels: Record<string, string> = {
  privileged: "Privileged containers (full access to the host kernel and devices)",
  host_network: "Host network namespace",
  host_pid: "Host process namespace",
  host_ipc: "Host IPC namespace",
  host_userns: "Host user namespace",
  host_uts: "Host UTS namespace",
  host_cgroup: "Host cgroup namespace",
  host_mount: "Bind mounts from the worker's filesystem",
  docker_socket: "Docker socket (can control every container on the worker, equivalent to root)",
  devices: "Host devices or GPUs",
  capabilities: "Added Linux capabilities",
  security_options: "Changed security options, sysctls or cgroup parent",
};

export function capabilityLabel(capability: string) {
  return capabilityLabels[capability] ?? capability;
}

export function originLabel(entry: Pick<TemplateEntry, "origin">) {
  return entry.origin === "builtin" ? "Built-in" : entry.origin === "team" ? "Team template" : "Template source";
}

export function shortRevision(revision: string) {
  return revision.length > 12 ? revision.slice(0, 12) : revision;
}

export const templateIdPattern = /^[a-z0-9][a-z0-9-]{0,63}$/;

export function suggestTemplateId(name: string) {
  return name.toLowerCase().replace(/[^a-z0-9]+/g, "-").replace(/^-+|-+$/g, "").slice(0, 64).replace(/-+$/, "");
}

// Template sources must be public HTTPS endpoints on the default port; private hosts and credentials are refused by design.
export function sourceURLError(value: string): string | null {
  if (value.trim() === "") return "Enter the source URL.";
  let url: URL;
  try {
    url = new URL(value.trim());
  } catch {
    return "Enter a valid URL.";
  }
  if (url.protocol !== "https:" || url.hostname === "") return "Use an HTTPS URL.";
  if (url.username || url.password) return "Remove credentials. Template sources must be public.";
  if (url.search || url.hash) return "Remove the query string or fragment.";
  if (url.port !== "" && url.port !== "443") return "Use the default HTTPS port.";
  return null;
}

export function httpsLinkError(value: string, what: string): string | null {
  if (value.trim() === "") return `Enter the ${what} link.`;
  return httpsURL(value.trim()) ? null : "Use an HTTPS URL without credentials or a fragment.";
}

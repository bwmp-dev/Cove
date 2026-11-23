import type { ApiError } from "../api/client";

export interface Remediation {
  fix: string;
  link?: { label: string; href: string };
}

interface Rule {
  matches: (message: string, error: ApiError) => boolean;
  remediation: (resourceId: string | null, error: ApiError) => Remediation;
}

// Resource IDs are opaque text; only plain path-safe segments are linked so a crafted address cannot shape the link.
const resourcePath = /^\/resources\/([A-Za-z0-9_-]{1,128})(?:\/|$)/;

function troubleshooting(resourceId: string | null, fallback: Remediation["link"]): Remediation["link"] {
  return resourceId ? { label: "Open troubleshooting", href: `/resources/${resourceId}/troubleshooting` } : fallback;
}

// Ordered from the most to the least specific. Messages are the control plane's own short sentences
// (internal/control); anything not listed here keeps its original text without a suggestion.
const rules: Rule[] = [
  {
    matches: (message) => message.startsWith("file access failed; check target ownership"),
    remediation: (resourceId) => ({
      fix: "Reload the folder or file so you work on its current revision and owner, then try again. If it keeps failing, check the worker's tools and the resource's containers.",
      link: troubleshooting(resourceId, { label: "Open nodes", href: "/nodes" }),
    }),
  },
  {
    matches: (message) => message.startsWith("snapshot inspection unavailable"),
    remediation: (resourceId) => ({
      fix: "Check that the worker has the filesystem snapshot tools and privileges it needs and that the resource is deployed, or choose the stopped volume snapshot.",
      link: troubleshooting(resourceId, { label: "Open nodes", href: "/nodes" }),
    }),
  },
  {
    matches: (message) => /^wait for active operations and upgrade and verify|^verify an upgraded worker and wait for active operations/.test(message),
    remediation: () => ({ fix: "Wait for running operations to finish. If it still fails, upgrade the worker from its node page and verify the node.", link: { label: "Open nodes", href: "/nodes" } }),
  },
  {
    matches: (message) => /^backup transfer (slots|readers) are busy/.test(message),
    remediation: () => ({ fix: "Other backup downloads or imports are using every transfer slot. Try again once one finishes." }),
  },
  {
    matches: (message) => message === "dump exceeds the configured backup transfer limit",
    remediation: () => ({ fix: "Choose a smaller dump, or ask an instance administrator to raise the backup transfer limit in Settings." }),
  },
  {
    matches: (message) => message === "finish the pending panel recovery action first" || message === "another panel recovery action is pending",
    remediation: () => ({ fix: "Wait for the pending panel recovery action to finish; Panel recovery in Settings shows its progress." }),
  },
  {
    matches: (message) => message.startsWith("upgrade and verify "),
    remediation: () => ({ fix: "Upgrade the worker from its node page, then verify the node.", link: { label: "Open nodes", href: "/nodes" } }),
  },
  {
    matches: (message) => /wait for the active operation|wait for active operations before|already has an active operation|worker or resource is busy/.test(message),
    remediation: (resourceId) => ({
      fix: "Wait for the running operation to finish, then try again.",
      link: resourceId ? { label: "Open resource operations", href: `/resources/${resourceId}/operations` } : { label: "Open operations", href: "/operations" },
    }),
  },
  {
    matches: (message) => message === "node is not assigned to this team",
    remediation: () => ({ fix: "Ask an instance administrator to assign the node to this team." }),
  },
  {
    matches: (message) => message.startsWith("quota exceeded"),
    remediation: () => ({ fix: "Compare current usage with the team quota before retrying.", link: { label: "Open team settings", href: "/team" } }),
  },
  {
    matches: (message) => /^cloudflare (could not be reached|connection unavailable)/.test(message),
    remediation: () => ({ fix: "Check the Cloudflare connection and its API token permissions.", link: { label: "Open integrations", href: "/integrations" } }),
  },
  {
    matches: (message) => /^github .*unavailable|^could not start github authorization/.test(message),
    remediation: () => ({ fix: "Check the GitHub connection.", link: { label: "Open integrations", href: "/integrations" } }),
  },
  {
    matches: (message, error) => error.status === 502 || /^(worker unavailable|worker rejected request|invalid worker response|worker response too large)/.test(message),
    remediation: (resourceId) => ({ fix: "Check that the node is connected and its worker is up to date.", link: troubleshooting(resourceId, { label: "Open nodes", href: "/nodes" }) }),
  },
  {
    matches: (message) => message === "origin rejected",
    remediation: () => ({ fix: "Open Cove at its configured public address and try again." }),
  },
  {
    matches: (message) => message === "database unavailable",
    remediation: () => ({ fix: "The control plane could not reach its database. Try again shortly; if this continues, check the control plane service." }),
  },
  {
    matches: (_message, error) => error.status === 429 && error.retryAfter !== null,
    remediation: (_resourceId, error) => ({ fix: `Try again in ${error.retryAfter} ${error.retryAfter === 1 ? "second" : "seconds"}.` }),
  },
  {
    matches: (_message, error) => error.status === 401,
    remediation: () => ({ fix: "Sign in again to continue.", link: { label: "Sign in", href: "/login" } }),
  },
  {
    matches: (message, error) => error.status === 403 || message === "permission denied" || message === "team administrator required",
    remediation: () => ({ fix: "Ask a team administrator for access to this action." }),
  },
];

// Checked by shape rather than instanceof so the shared error states do not need the API client module at runtime.
function isApiError(error: unknown): error is ApiError {
  return error instanceof Error && error.name === "ApiError" && typeof (error as Partial<ApiError>).status === "number";
}

export function remediationFor(error: unknown, pathname = ""): Remediation | null {
  if (!isApiError(error) || error.status === 0) return null;
  const message = error.message.trim().toLowerCase();
  const resourceId = resourcePath.exec(pathname)?.[1] ?? null;
  const rule = rules.find((item) => item.matches(message, error));
  return rule ? rule.remediation(resourceId, error) : null;
}

import type { RegistryWebhookFilter, RegistryWebhookProvider, RegistryWebhookStatus } from "../api/registry-webhook";

// Mirrors the control plane's limits so mistakes are caught before saving. The control plane stays authoritative.
export const maxWebhookFilters = 32;
const repositoryPath = /^[a-z0-9]+(?:[._/-][a-z0-9]+)*$/;
const tagName = /^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$/;

export const providers: Record<RegistryWebhookProvider, { label: string; servers?: string[] }> = {
  distribution: { label: "Distribution registry notifications" },
  docker_hub: { label: "Docker Hub", servers: ["docker.io", "index.docker.io"] },
  ghcr: { label: "GitHub Container Registry", servers: ["ghcr.io"] },
};

export function defaultProvider(server: string | undefined): RegistryWebhookProvider {
  if (server && providers.ghcr.servers?.includes(server)) return "ghcr";
  if (server && providers.docker_hub.servers?.includes(server)) return "docker_hub";
  return "distribution";
}

export function providerError(provider: RegistryWebhookProvider, server: string | undefined) {
  const servers = providers[provider].servers;
  if (!servers || (server && servers.includes(server))) return undefined;
  return `${providers[provider].label} needs the resource's registry to be ${servers.join(" or ")}${server ? `, not ${server}` : ""}.`;
}

// Docker Hub webhooks are configured by URL only, so the token travels as a query parameter. Built only for the
// one-time reveal; never cached or navigated to.
export function dockerHubSecretURL(endpoint: string, token: string) {
  return `${endpoint}?secret=${encodeURIComponent(token)}`;
}

let counter = 0;

export interface FilterDraft {
  key: string;
  repository: string;
  tag: string;
}

export interface WebhookDraft {
  provider: RegistryWebhookProvider;
  enabled: boolean;
  filters: FilterDraft[];
}

export function filterDraft(filter?: RegistryWebhookFilter): FilterDraft {
  return { key: `filter-${++counter}`, repository: filter?.repository ?? "", tag: filter?.tag ?? "" };
}

// A new webhook starts disabled, so automatic deploys are always switched on deliberately.
export function webhookDraftFrom(status: RegistryWebhookStatus): WebhookDraft {
  const saved = status.configuration;
  return {
    provider: saved?.provider ?? defaultProvider(status.registry?.server),
    enabled: saved?.enabled ?? false,
    filters: saved && saved.filters.length > 0 ? saved.filters.map(filterDraft) : [filterDraft()],
  };
}

export function webhookInput(draft: WebhookDraft) {
  return { provider: draft.provider, enabled: draft.enabled, filters: draft.filters.map((filter) => ({ repository: filter.repository.trim(), tag: filter.tag.trim() })) };
}

export function webhookDraftKey(draft: WebhookDraft) {
  return JSON.stringify(webhookInput(draft));
}

export type FilterErrors = Record<string, string>;

// Errors are keyed by `${key}.repository`, `${key}.tag`, "filters" and "provider".
export function validateWebhookDraft(draft: WebhookDraft, server: string | undefined): FilterErrors {
  const errors: FilterErrors = {};
  const provider = providerError(draft.provider, server);
  if (provider) errors.provider = provider;
  if (draft.filters.length === 0) errors.filters = "Add at least one repository filter.";
  if (draft.filters.length > maxWebhookFilters) errors.filters = `Use at most ${maxWebhookFilters} filters.`;
  const seen = new Set<string>();
  for (const filter of draft.filters) {
    const repository = filter.repository.trim();
    const tag = filter.tag.trim();
    if (!repository) errors[`${filter.key}.repository`] = "Enter a repository path.";
    else if (repository.length > 200 || !repositoryPath.test(repository)) errors[`${filter.key}.repository`] = "Use a lowercase repository path of at most 200 characters, without a registry host or tag.";
    if (!tag) errors[`${filter.key}.tag`] = "Enter a tag, or * for every tag.";
    else if (tag !== "*" && !tagName.test(tag)) errors[`${filter.key}.tag`] = "Use a tag of at most 128 letters, digits, dots, dashes or underscores, or * for every tag.";
    const identity = `${repository}:${tag}`;
    if (repository && tag && seen.has(identity)) errors[`${filter.key}.tag`] = "This repository and tag are already listed.";
    seen.add(identity);
  }
  return errors;
}

// Saving issues a new token when the provider or the resource's registry changed since the last save.
export function saveRotates(status: RegistryWebhookStatus, draft: WebhookDraft) {
  const saved = status.configuration;
  return saved !== null && (!status.matches_saved_registry || saved.provider !== draft.provider);
}

export interface WebhookProblem {
  tone: "warn" | "info";
  title: string;
  text: string;
}

// Why the saved webhook cannot deploy pushes right now, if anything.
export function webhookProblem(status: RegistryWebhookStatus): WebhookProblem | null {
  const saved = status.configuration;
  if (!saved) return null;
  if (!status.registry) return { tone: "warn", title: "No connected registry", text: `This webhook was saved for ${saved.registry_server}, but the resource no longer uses a connected registry, so pushes are ignored. Remove the webhook, or select a registry and save to issue a new secret.` };
  if (!status.matches_saved_registry) return { tone: "warn", title: "Registry changed", text: `This webhook was saved for ${saved.registry_server}, but the resource now uses ${status.registry.name} (${status.registry.server}). Pushes are ignored until you save, which issues a new secret.` };
  if (!status.worker_supported) return { tone: "warn", title: "Worker upgrade needed", text: "The worker does not report image lifecycle support, so pushes cannot be deployed. Upgrade and verify the worker. You can still disable or remove the webhook." };
  if (!saved.enabled) return { tone: "info", title: "Automatic deploys are off", text: "Pushes are not deployed while the webhook is disabled." };
  return null;
}

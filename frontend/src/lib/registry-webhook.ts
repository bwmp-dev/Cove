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

// Why the draft cannot be saved with automatic deploys on, if it is on.
export function enabledError(draft: WebhookDraft, status: RegistryWebhookStatus, canOperate: boolean) {
  if (!draft.enabled) return undefined;
  if (!status.worker_supported) return "Upgrade and verify the worker before turning on automatic deploys. You can save the webhook with them off.";
  if (!canOperate) return "Saving with automatic deploys on needs Operate access as well as Edit configuration. Turn them off to save your changes.";
  return undefined;
}

// Saving issues a new token when the provider or the resource's registry changed since the last save.
export function saveRotates(status: RegistryWebhookStatus, draft: WebhookDraft) {
  const saved = status.configuration;
  return saved !== null && (!status.matches_saved_registry || saved.provider !== draft.provider);
}

export function webhookSaveState(status: RegistryWebhookStatus, saved: WebhookDraft, draft: WebhookDraft, canOperate: boolean) {
  const existing = status.configuration;
  const rotates = saveRotates(status, draft);
  const changed = webhookDraftKey(draft) !== webhookDraftKey(saved);
  // Ownership is only taken over by a deliberate save, so an unchanged form can still be saved to claim it.
  const claims = status.ownership_required && canOperate && draft.enabled;
  return {
    rotates,
    changed,
    claims,
    dirty: !existing || rotates || changed || claims,
    confirmsEnable: draft.enabled && (!existing?.enabled || status.ownership_required),
  };
}

// Why the saved webhook's secret cannot be rotated right now, if anything.
export function rotateBlockedReason(status: RegistryWebhookStatus, canOperate: boolean, blocked: string | undefined) {
  if (blocked) return blocked;
  if (!canOperate) return "Rotating the secret needs Edit configuration and Operate access.";
  if (!status.registry || !status.matches_saved_registry) return "Save the current registry selection first";
  return undefined;
}

export interface WebhookProblem {
  tone: "warn" | "info";
  title: string;
  text: string;
}

// Saving and rotating make the actor the webhook's owner. Turning automatic deploys on and rotating need Edit
// configuration and Operate; turning them off and removing the webhook need Edit configuration only.
export interface WebhookAccess {
  canEdit: boolean;
  canOperate: boolean;
}

function ownershipText({ canEdit, canOperate }: WebhookAccess) {
  const cause = "No one currently authorised owns this webhook: whoever last saved it or rotated its secret no longer holds Edit configuration and Operate access, for example after losing access to this resource, no longer meeting the multi-factor authentication requirement, or being locked, deactivated or deprovisioned. Pushes are not deployed, and ownership is never taken over automatically. If that access is restored, pushes deploy again without a new owner, unless the account was deactivated or deprovisioned.";
  if (canEdit && canOperate) return `${cause} Save the webhook or rotate its secret to take ownership. This cancels the previous owner’s pending webhook deploys, and later pushes deploy with your access.`;
  if (canEdit) return `${cause} Taking ownership needs Edit configuration and Operate access. You can turn automatic deploys off or remove the webhook.`;
  return `${cause} Someone with Edit configuration and Operate access can save it or rotate its secret to take ownership.`;
}

// Why the saved webhook cannot deploy pushes right now, if anything.
export function webhookProblem(status: RegistryWebhookStatus, access: WebhookAccess): WebhookProblem | null {
  const saved = status.configuration;
  if (!saved) return null;
  if (!status.registry) return { tone: "warn", title: "No connected registry", text: `This webhook was saved for ${saved.registry_server}, but the resource no longer uses a connected registry, so pushes are ignored. Remove the webhook, or select a registry and save to issue a new secret.` };
  if (!status.matches_saved_registry) return { tone: "warn", title: "Registry changed", text: `This webhook was saved for ${saved.registry_server}, but the resource now uses ${status.registry.name} (${status.registry.server}). Pushes are ignored until you save, which issues a new secret.` };
  if (!status.worker_supported) return { tone: "warn", title: "Worker upgrade needed", text: "The worker does not report image lifecycle support, so pushes cannot be deployed. Upgrade and verify the worker. You can still disable or remove the webhook." };
  // Reported while disabled too, so turning automatic deploys back on is known to need a new owner.
  if (status.ownership_required) return { tone: "warn", title: saved.enabled ? "Automatic deploys suspended" : "No authorised owner", text: ownershipText(access) };
  if (!saved.enabled) return { tone: "info", title: "Automatic deploys are off", text: "Pushes are not deployed while the webhook is disabled." };
  return null;
}

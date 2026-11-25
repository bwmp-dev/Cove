import { ApiError } from "../api/client";
import { isApplicationPath } from "./base-path";
import type { PreviewImagesConfiguration, PreviewImagesInput, PreviewImagesResource, PreviewImagesStatus } from "../api/preview-images";

export const defaultCredentialDays = 30;
export const maxCredentialDays = 365;

export interface IssuedCredential {
  token: string;
  expires_at: string;
}

const unrecognised = "The server returned CI image preview settings this panel does not recognise. Reload the page after updating the panel.";

function invalid(): never {
  throw new ApiError(200, unrecognised);
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}

function text(value: unknown) {
  return typeof value === "string" ? value : invalid();
}

// The server reports the hook as a logical panel path; the page adds the origin and any path prefix.
function applicationPath(value: unknown) {
  return typeof value === "string" && isApplicationPath(value) ? value : invalid();
}

function texts(value: unknown) {
  return Array.isArray(value) ? value.map(text) : invalid();
}

function count(value: unknown) {
  return typeof value === "number" && Number.isSafeInteger(value) && value >= 0 ? value : invalid();
}

function timestamp(value: unknown) {
  return typeof value === "string" && !Number.isNaN(Date.parse(value)) ? value : invalid();
}

function parseConfiguration(value: unknown): PreviewImagesConfiguration | null {
  if (value === null) return null;
  if (!isRecord(value)) return invalid();
  return {
    resource_ids: texts(value.resource_ids),
    recipe_version: count(value.recipe_version),
    // Anything but an explicit true is treated as a stale scope.
    matches_scope: value.matches_scope === true,
    expires_at: timestamp(value.expires_at),
    created_at: timestamp(value.created_at),
    last_received_at: value.last_received_at === null || value.last_received_at === undefined ? null : timestamp(value.last_received_at),
  };
}

// Null means no team registry is selected; anything else must be a complete registry or the read is invalid.
function parseRegistry(value: unknown): PreviewImagesResource["registry"] {
  if (value === null) return null;
  if (!isRecord(value)) return invalid();
  return { id: text(value.id), server: text(value.server) };
}

function parseResource(value: unknown): PreviewImagesResource {
  if (!isRecord(value)) return invalid();
  return {
    id: text(value.id),
    name: text(value.name),
    services: texts(value.services),
    registry: parseRegistry(value.registry),
    eligible: value.eligible === true,
    reason: typeof value.reason === "string" ? value.reason : "",
  };
}

// Copies only the documented fields, so nothing unexpected reaches the query cache. Unknown or malformed values never
// enable an action: flags must be exactly true, and a malformed structure is reported as an error.
export function parsePreviewImages(value: unknown): PreviewImagesStatus {
  // A missing configuration must not read as "no credential"; only an explicit null means none.
  if (!isRecord(value) || !Array.isArray(value.resources) || !("configuration" in value)) return invalid();
  return {
    configuration: parseConfiguration(value.configuration),
    resources: value.resources.map(parseResource),
    endpoint: applicationPath(value.endpoint),
    can_manage: value.can_manage === true,
    setup_reason: typeof value.setup_reason === "string" ? value.setup_reason : "",
    max_resources: count(value.max_resources),
    max_preview_keys: count(value.max_preview_keys),
    max_deliveries: count(value.max_deliveries),
    used_preview_keys: count(value.used_preview_keys),
    retained_deliveries: count(value.retained_deliveries),
  };
}

export function parseIssuedCredential(value: unknown): IssuedCredential {
  if (isRecord(value) && typeof value.token === "string" && value.token !== "" && typeof value.expires_at === "string") {
    return { token: value.token, expires_at: value.expires_at };
  }
  throw new ApiError(201, "The reply could not be read. A new credential may have been issued and the previous one revoked.");
}

// 4xx replies are refusals that changed nothing. Anything else (no reply, a server error or an unreadable reply) leaves
// it unknown whether the credential was issued, replaced or revoked.
export function wasRefused(error: unknown) {
  return error instanceof ApiError && error.status >= 400 && error.status < 500;
}

export interface CredentialDraft {
  resourceIds: string[];
  days: string;
}

// Starts from the saved selection, including resources that are no longer eligible or listed, so none is dropped silently.
export function credentialDraft(status: PreviewImagesStatus): CredentialDraft {
  return { resourceIds: status.configuration?.resource_ids ?? [], days: String(defaultCredentialDays) };
}

export function credentialInput(draft: CredentialDraft): PreviewImagesInput {
  return { resource_ids: [...new Set(draft.resourceIds)], days: Number(draft.days) };
}

export interface SelectedResource {
  id: string;
  resource: PreviewImagesResource | null;
}

// Saved IDs that are not in the listed resources come first, so they stay visible.
export function selectableResources(status: PreviewImagesStatus, draft: CredentialDraft): SelectedResource[] {
  const listed = new Set(status.resources.map((resource) => resource.id));
  const missing = [...new Set([...(status.configuration?.resource_ids ?? []), ...draft.resourceIds])].filter((id) => !listed.has(id));
  return [...missing.map((id) => ({ id, resource: null })), ...status.resources.map((resource) => ({ id: resource.id, resource }))];
}

export function daysError(days: string) {
  const value = Number(days);
  return /^\d+$/.test(days.trim()) && value >= 1 && value <= maxCredentialDays ? undefined : `Enter a whole number of days from 1 to ${maxCredentialDays}.`;
}

// Why the draft cannot be issued, or undefined when it can. The server stays authoritative.
export function issueBlocker(status: PreviewImagesStatus, draft: CredentialDraft): string | undefined {
  if (!status.can_manage) return "Issuing a credential needs edit access to the project and its production environment.";
  if (status.setup_reason) return status.setup_reason;
  const selected = new Set(draft.resourceIds);
  if (selected.size === 0) return "Choose at least one resource.";
  if (selected.size > status.max_resources) return `Choose at most ${status.max_resources} resources.`;
  const byId = new Map(status.resources.map((resource) => [resource.id, resource]));
  const unavailable = [...selected].filter((id) => byId.get(id)?.eligible !== true).map((id) => byId.get(id)?.name ?? id);
  if (unavailable.length > 0) return `Unselect ${unavailable.join(", ")}: ${unavailable.length === 1 ? "it cannot" : "they cannot"} receive CI images now.`;
  return daysError(draft.days);
}

export function sameSelection(a: string[], b: string[]) {
  const left = [...new Set(a)].sort();
  const right = [...new Set(b)].sort();
  return left.length === right.length && left.every((id, index) => id === right[index]);
}

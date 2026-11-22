export const baseScopes = ["openid", "email", "profile"];

// RFC 6749 scope-token characters; anything else is refused by the API as invalid scopes.
const scopeToken = /^[\x21\x23-\x5B\x5D-\x7E]+$/;

export function parseScopes(value: string) {
  return [...new Set(value.split(/[\s,]+/).filter(Boolean))];
}

// The API always requests the base scopes, so the editor only shows and sends the extra ones.
export function extraScopes(scopes: readonly string[]) {
  return scopes.filter((scope) => !baseScopes.includes(scope));
}

function isHTTPS(value: string) {
  try {
    const url = new URL(value);
    return url.protocol === "https:" && url.hostname !== "";
  } catch {
    return false;
  }
}

export interface ProviderDraft {
  display_name: string;
  issuer: string;
  client_id: string;
  scopes: string;
}

export type ProviderErrors = Partial<Record<keyof ProviderDraft, string>>;

export function validateProvider(draft: ProviderDraft): ProviderErrors {
  const errors: ProviderErrors = {};
  if (draft.display_name.trim() === "") errors.display_name = "Enter the name shown on the sign-in button.";
  if (!isHTTPS(draft.issuer.trim())) errors.issuer = "Enter the provider's HTTPS issuer URL.";
  if (draft.client_id.trim() === "") errors.client_id = "Enter the client ID from the provider.";
  if (parseScopes(draft.scopes).some((scope) => !scopeToken.test(scope))) errors.scopes = "Scopes cannot contain quotes or backslashes.";
  return errors;
}

export interface AuditExportDraft {
  webhook_url: string;
  webhook_secret: string;
  syslog_address: string;
  retention_days: string;
}

export type AuditExportErrors = Partial<Record<keyof AuditExportDraft, string>>;

const byteLength = (value: string) => new TextEncoder().encode(value).length;

export function validateAuditExport(draft: AuditExportDraft, secretConfigured: boolean): AuditExportErrors {
  const errors: AuditExportErrors = {};
  const webhook = draft.webhook_url.trim();
  if (webhook !== "" && !isHTTPS(webhook)) errors.webhook_url = "Use an HTTPS URL, or leave it empty to turn the webhook off.";
  if (webhook !== "" && draft.webhook_secret === "" && !secretConfigured) errors.webhook_secret = "Enter a signing secret of at least 32 bytes.";
  if (draft.webhook_secret !== "" && byteLength(draft.webhook_secret) < 32) errors.webhook_secret = "The signing secret must be at least 32 bytes.";
  const syslog = draft.syslog_address.trim();
  if (syslog !== "") {
    const match = /^(.+):(\d{1,5})$/.exec(syslog);
    const port = match ? Number(match[2]) : 0;
    if (!match || port < 1 || port > 65535 || /\s/.test(match[1] ?? "")) errors.syslog_address = "Use host:port, for example logs.example.com:6514.";
  }
  const days = Number(draft.retention_days);
  if (!Number.isInteger(days) || days < 1 || days > 3650) errors.retention_days = "Enter a whole number of days from 1 to 3650.";
  return errors;
}

export function generateSecret() {
  const bytes = crypto.getRandomValues(new Uint8Array(32));
  return Array.from(bytes, (byte) => byte.toString(16).padStart(2, "0")).join("");
}

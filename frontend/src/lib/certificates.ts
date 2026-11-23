import type { CertificateZone, RouteCertificate } from "../api/types";
import type { Tone } from "./nodes";

export const certificateLimits = { name: 80, chainBytes: 65_536, keyBytes: 16_384, perResource: 32 } as const;

export type CertificateStatus = "valid" | "expiring" | "expired" | "not_yet_valid" | "unknown";

const encoder = new TextEncoder();
const byteLength = (text: string) => encoder.encode(text).length;
const day = 86_400_000;
const expiringWindow = 30 * day;

export const certificateStatuses: Record<CertificateStatus, { tone: Tone; label: string }> = {
  valid: { tone: "ok", label: "Valid" },
  expiring: { tone: "warn", label: "Expires soon" },
  expired: { tone: "bad", label: "Expired" },
  not_yet_valid: { tone: "warn", label: "Not yet valid" },
  unknown: { tone: "neutral", label: "Unknown validity" },
};

// Describes the validity window only. Whether clients trust the issuer is not known here: uploads may be self-signed
// or issued by a private CA such as Cloudflare Origin CA.
export function certificateStatus(certificate: Pick<RouteCertificate, "not_before" | "expires_at">, now: number): CertificateStatus {
  const start = Date.parse(certificate.not_before);
  const end = Date.parse(certificate.expires_at);
  if (!Number.isFinite(start) || !Number.isFinite(end)) return "unknown";
  if (now < start) return "not_yet_valid";
  if (now >= end) return "expired";
  if (end - now <= expiringWindow) return "expiring";
  return "valid";
}

const dnsLabel = /^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$/;

function isDNSName(name: string) {
  const base = name.startsWith("*.") ? name.slice(2) : name;
  return base !== "" && base.split(".").every((label) => dnsLabel.test(label));
}

function isIPAddress(name: string) {
  return /^[0-9.]+$/.test(name) || name.includes(":");
}

// Whether a wildcard route hostname such as *.example.com matches name: exactly one more label, never the apex, deeper
// names or another wildcard.
export function wildcardCovers(pattern: string, name: string) {
  if (!pattern.startsWith("*.") || name.startsWith("*.") || !name.endsWith(pattern.slice(1))) return false;
  return dnsLabel.test(name.slice(0, -(pattern.length - 1)));
}

// Names the shared proxy certificate store would register for this certificate, mirroring the control plane: every
// DNS subject alternative name, plus the common name when it is a DNS name or an IP address.
function registeredNames(certificate: Pick<RouteCertificate, "common_name" | "dns_names">) {
  const names = [...certificate.dns_names];
  if (certificate.common_name) names.push(certificate.common_name);
  return names.map((name) => name.toLowerCase().replace(/\.+$/, ""));
}

// Returns why a certificate cannot be selected for routes on these hostnames, or null when the rule is met. A wildcard
// route owns names one label below it; a wildcard name in the certificate needs the same wildcard route. The control
// plane makes the final decision; this only reports a mistake before saving.
export function ownershipProblem(certificate: Pick<RouteCertificate, "common_name" | "dns_names" | "ip_addresses">, hostnames: string[]): string | null {
  const names = registeredNames(certificate);
  if ((certificate.ip_addresses?.length ?? 0) > 0 || names.some(isIPAddress)) return "This certificate names an IP address, which routes cannot use.";
  const outside = names.filter((name) => isDNSName(name) && !hostnames.some((host) => host === name || wildcardCovers(host, name)));
  if (outside.length === 0) return null;
  const unique = [...new Set(outside)];
  return `This certificate also names ${unique.join(", ")}, which ${unique.length === 1 ? "is" : "are"} not a hostname of this resource's routes. Add ${unique.length === 1 ? "a route for it" : "routes for them"} or use a certificate without ${unique.length === 1 ? "it" : "them"}.`;
}

export interface CertificateDraft {
  name: string;
  chain: string;
  key: string;
}

export type CertificateDraftErrors = Partial<Record<keyof CertificateDraft, string>>;

function hasControlCharacter(value: string) {
  return [...value].some((c) => {
    const code = c.charCodeAt(0);
    return code < 32 || code === 127;
  });
}

// Catches empty, oversized and obviously wrong input. Chain order, key match and key strength are checked by the API.
export function validateCertificateDraft(draft: CertificateDraft): CertificateDraftErrors {
  const errors: CertificateDraftErrors = {};
  const name = draft.name.trim();
  if (name === "") errors.name = "Enter a name.";
  else if (name.length > certificateLimits.name) errors.name = `Use at most ${certificateLimits.name} characters.`;
  else if (hasControlCharacter(name)) errors.name = "Remove line breaks and other control characters.";
  if (draft.chain.trim() === "") errors.chain = "Paste the PEM certificate chain or choose a file.";
  else if (byteLength(draft.chain) > certificateLimits.chainBytes) errors.chain = "Keep the certificate chain within 64 KiB.";
  else if (!draft.chain.includes("-----BEGIN CERTIFICATE-----")) errors.chain = "Use PEM certificates that start with -----BEGIN CERTIFICATE-----.";
  else if (/-----BEGIN [A-Z ]*PRIVATE KEY-----/.test(draft.chain)) errors.chain = "Remove the private key from the certificate chain and paste it below.";
  if (draft.key.trim() === "") errors.key = "Paste the PEM private key or choose a file.";
  else if (byteLength(draft.key) > certificateLimits.keyBytes) errors.key = "Keep the private key within 16 KiB.";
  else if (draft.key.includes("ENCRYPTED")) errors.key = "Use an unencrypted private key. Remove its passphrase first.";
  else if (!/-----BEGIN (?:RSA |EC )?PRIVATE KEY-----/.test(draft.key)) errors.key = "Use a PEM private key. Its first line names a private key.";
  return errors;
}

export interface CertificateSourceInfo {
  label: string;
  trust: string;
}

// The provenance of a stored version. It says nothing about whether routes currently serve it.
export function certificateSource(source: string | undefined): CertificateSourceInfo {
  if (source === undefined || source === "uploaded") return { label: "Uploaded", trust: "Trusted by whichever clients trust the certificate authority that signed it." };
  if (source === "cloudflare_origin") return { label: "Cloudflare Origin CA", trust: "Trusted by Cloudflare's edge only. Browsers do not trust Origin CA certificates." };
  if (source === "acme_dns") return { label: "ACME DNS-01", trust: "Trusted by clients that trust the certificate authority the panel operator configured. Browsers do not trust a private or test authority." };
  return { label: "Other source", trust: "Cove does not recognise how this version was created." };
}

export const issuerLimits = { perResource: 8, nameBytes: 60, hostnames: 32 } as const;

// Validity periods Cloudflare Origin CA offers, in days.
export const originValidityDays = [7, 30, 90, 365, 730, 1095, 5475] as const;

export function validityLabel(days: number) {
  if (days % 365 === 0) return days === 365 ? "1 year" : `${days / 365} years`;
  return days === 1 ? "1 day" : `${days} days`;
}

const issuerPhases: Record<string, { tone: Tone; label: string }> = {
  requesting: { tone: "info", label: "Requesting" },
  reconciling: { tone: "warn", label: "Confirming the request" },
  applying: { tone: "info", label: "Updating routes" },
  issued: { tone: "ok", label: "Issued" },
  failed: { tone: "bad", label: "Failed" },
};

// Unknown phases from a newer panel are shown as they are, never as complete.
export function issuerPhase(phase: string) {
  return issuerPhases[phase] ?? { tone: "neutral" as Tone, label: phase ? `Unknown state (${phase})` : "Unknown state" };
}

export function issuerWorking(phase: string) {
  return phase === "requesting" || phase === "reconciling" || phase === "applying";
}

export function issuerRetryable(phase: string) {
  return phase === "failed" || phase === "reconciling";
}

export type IssuerProvider = "cloudflare_origin" | "acme_dns";

const issuerProviders: Record<string, string> = { cloudflare_origin: "Cloudflare Origin CA", acme_dns: "ACME DNS-01" };

export function issuerProviderLabel(provider: string) {
  return issuerProviders[provider] ?? (provider ? `Unknown provider (${provider})` : "Unknown provider");
}

const challengePhases: Record<string, { tone: Tone; label: string }> = {
  pending: { tone: "info", label: "Waiting to publish" },
  presenting: { tone: "info", label: "Publishing" },
  present: { tone: "info", label: "Published" },
  blocked: { tone: "warn", label: "Refused by the DNS provider" },
  cleanup: { tone: "info", label: "Removing" },
};

// Unknown phases from a newer panel are shown as they are, never as resolved.
export function challengePhase(phase: string) {
  return challengePhases[phase] ?? { tone: "neutral" as Tone, label: phase ? `Unknown state (${phase})` : "Unknown state" };
}

// Blocked records wait for an issuer Retry; every other known phase is reconciled by the panel on its own.
export function challengeWorking(phase: string) {
  return phase === "pending" || phase === "presenting" || phase === "present" || phase === "cleanup";
}

export type AuthorityTerms = { kind: "none" } | { kind: "text"; text: string } | { kind: "link"; href: string } | { kind: "unreadable" };

// How to show an authority's terms. Plain-text terms (the panel decodes data:text/plain URIs) are shown only as text,
// and only an HTTPS URL ever becomes a link, so a data: or script URI can never be opened from the dashboard.
export function authorityTerms(termsUrl: string | undefined, termsText: string | undefined): AuthorityTerms {
  if (termsText) return { kind: "text", text: termsText };
  if (!termsUrl) return { kind: "none" };
  try {
    const url = new URL(termsUrl);
    if (url.protocol === "https:" && url.hostname !== "" && url.username === "" && url.password === "") return { kind: "link", href: url.href };
  } catch {
    // Not an absolute URL: fall through to the unreadable state.
  }
  return { kind: "unreadable" };
}

export interface IssuerDraft {
  name: string;
  zoneId: string;
  hostnames: string;
  validityDays: number;
  autoRenew: boolean;
}

export type IssuerDraftErrors = Partial<Record<"name" | "zoneId" | "hostnames" | "terms", string>>;

export function hostnameList(text: string) {
  return text
    .split(/[\s,]+/)
    .map((name) => name.trim().toLowerCase())
    .filter(Boolean);
}

function isCompleteName(name: string) {
  return name.length <= 253 && name.includes(".") && name.split(".").every((label) => dnsLabel.test(label));
}

// Mirrors the panel's zone-share check: an exact entry or a "*.zone" pattern covers a hostname, but a wildcard share
// never covers its own apex, and a wildcard request needs a wildcard share.
export function shareAllows(patterns: string[], name: string) {
  if (!name.startsWith("*.")) return patterns.some((pattern) => pattern === name || (pattern.startsWith("*.") && name.endsWith(pattern.slice(1))));
  const base = name.slice(2);
  return patterns.some((pattern) => pattern.startsWith("*.") && (base === pattern.slice(2) || base.endsWith(pattern.slice(1))));
}

// Catches mistakes before sending. The panel checks the current zone state and grants again on every request.
export function validateIssuerDraft(draft: IssuerDraft, zone: CertificateZone | undefined): IssuerDraftErrors {
  const errors: IssuerDraftErrors = {};
  const name = draft.name.trim();
  if (name === "") errors.name = "Enter a name.";
  else if (byteLength(name) > issuerLimits.nameBytes) errors.name = `Use at most ${issuerLimits.nameBytes} bytes. Accented letters and symbols count as more than one.`;
  else if (hasControlCharacter(name)) errors.name = "Remove line breaks and other control characters.";
  if (!zone) errors.zoneId = "Choose a Cloudflare zone.";
  const hostnames = hostnameList(draft.hostnames);
  const invalid = hostnames.find((host) => !isCompleteName(host.startsWith("*.") ? host.slice(2) : host));
  const duplicate = hostnames.find((host, index) => hostnames.indexOf(host) !== index);
  if (hostnames.length === 0) errors.hostnames = "Enter at least one hostname.";
  else if (hostnames.length > issuerLimits.hostnames) errors.hostnames = `Use at most ${issuerLimits.hostnames} hostnames.`;
  else if (invalid) errors.hostnames = `${invalid} is not a complete DNS hostname. Use names such as app.example.com or *.example.com.`;
  else if (duplicate) errors.hostnames = `${duplicate} is listed more than once.`;
  else if (zone) {
    const outside = hostnames.find((host) => {
      const base = host.startsWith("*.") ? host.slice(2) : host;
      return base !== zone.domain && !base.endsWith(`.${zone.domain}`);
    });
    const unshared = hostnames.find((host) => !shareAllows(zone.domains, host));
    if (outside) errors.hostnames = `${outside} is not in the ${zone.domain} zone.`;
    else if (unshared) errors.hostnames = `${unshared} is not shared with this team. Ask the owner of the Cloudflare connection to share it.`;
  }
  return errors;
}

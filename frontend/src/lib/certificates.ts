import type { RouteCertificate } from "../api/types";
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

// Names the shared proxy certificate store would register for this certificate, mirroring the control plane: every
// DNS subject alternative name, plus the common name when it is a DNS name or an IP address.
function registeredNames(certificate: Pick<RouteCertificate, "common_name" | "dns_names">) {
  const names = [...certificate.dns_names];
  if (certificate.common_name) names.push(certificate.common_name);
  return names.map((name) => name.toLowerCase().replace(/\.+$/, ""));
}

// Returns why a certificate cannot be selected for routes on these hostnames, or null when the rule is met. The
// control plane makes the final decision; this only reports a mistake before saving.
export function ownershipProblem(certificate: Pick<RouteCertificate, "common_name" | "dns_names" | "ip_addresses">, hostnames: string[]): string | null {
  const names = registeredNames(certificate);
  if ((certificate.ip_addresses?.length ?? 0) > 0 || names.some(isIPAddress)) return "This certificate names an IP address, which routes cannot use.";
  const owned = new Set(hostnames);
  const outside = names.filter((name) => isDNSName(name) && !owned.has(name));
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

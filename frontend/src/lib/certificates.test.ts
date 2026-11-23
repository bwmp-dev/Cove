import { describe, expect, it } from "vitest";
import { certificateSource, certificateStatus, issuerPhase, issuerRetryable, issuerWorking, ownershipProblem, shareAllows, validateCertificateDraft, validateIssuerDraft, validityLabel, wildcardCovers, type IssuerDraft } from "./certificates";

const now = Date.parse("2026-06-01T00:00:00Z");
const span = (notBefore: string, expiresAt: string) => ({ not_before: notBefore, expires_at: expiresAt });

describe("certificateStatus", () => {
  it("reports the validity window from the certificate's own dates", () => {
    expect(certificateStatus(span("2026-01-01T00:00:00Z", "2027-01-01T00:00:00Z"), now)).toBe("valid");
    expect(certificateStatus(span("2026-01-01T00:00:00Z", "2026-06-20T00:00:00Z"), now)).toBe("expiring");
    expect(certificateStatus(span("2025-01-01T00:00:00Z", "2026-06-01T00:00:00Z"), now)).toBe("expired");
    expect(certificateStatus(span("2026-07-01T00:00:00Z", "2027-07-01T00:00:00Z"), now)).toBe("not_yet_valid");
    expect(certificateStatus(span("not a date", "2027-07-01T00:00:00Z"), now)).toBe("unknown");
  });
});

describe("ownershipProblem", () => {
  const hosts = ["app.example.com", "www.example.com"];

  it("accepts certificates whose every DNS name and common name are route hostnames", () => {
    expect(ownershipProblem({ dns_names: ["app.example.com"], common_name: "app.example.com" }, hosts)).toBeNull();
    expect(ownershipProblem({ dns_names: ["app.example.com", "www.example.com"], common_name: "Example Origin" }, hosts)).toBeNull();
    expect(ownershipProblem({ dns_names: ["APP.example.com."] }, hosts)).toBeNull();
  });

  it("refuses partial ownership even when one name matches", () => {
    expect(ownershipProblem({ dns_names: ["app.example.com", "api.example.com"] }, hosts)).toMatch(/also names api\.example\.com/);
    expect(ownershipProblem({ dns_names: ["app.example.com"], common_name: "other.example.net" }, hosts)).toMatch(/other\.example\.net/);
    expect(ownershipProblem({ dns_names: ["*.example.com"] }, hosts)).toMatch(/\*\.example\.com/);
  });

  it("refuses IP address names", () => {
    expect(ownershipProblem({ dns_names: ["app.example.com"], ip_addresses: ["192.0.2.1"] }, hosts)).toMatch(/IP address/);
    expect(ownershipProblem({ dns_names: ["app.example.com"], common_name: "192.0.2.1" }, hosts)).toMatch(/IP address/);
  });
});

describe("validateCertificateDraft", () => {
  const chain = "-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----\n";
  const key = "-----BEGIN PRIVATE KEY-----\nMIIE\n-----END PRIVATE KEY-----\n";

  it("accepts PEM input and leaves deeper checks to the API", () => {
    expect(validateCertificateDraft({ name: "Origin 2026", chain, key })).toEqual({});
    expect(validateCertificateDraft({ name: "Legacy", chain, key: key.replaceAll("PRIVATE KEY", "EC PRIVATE KEY") })).toEqual({});
  });

  it("reports empty, oversized and misplaced material next to its field", () => {
    expect(Object.keys(validateCertificateDraft({ name: " ", chain: "", key: "" })).sort()).toEqual(["chain", "key", "name"]);
    expect(validateCertificateDraft({ name: "a".repeat(81), chain, key }).name).toMatch(/80/);
    expect(validateCertificateDraft({ name: "two\nlines", chain, key }).name).toMatch(/control/);
    expect(validateCertificateDraft({ name: "x", chain: chain + "a".repeat(65_536), key }).chain).toMatch(/64 KiB/);
    expect(validateCertificateDraft({ name: "x", chain: chain + key, key }).chain).toMatch(/private key/);
    expect(validateCertificateDraft({ name: "x", chain, key: "-----BEGIN ENCRYPTED PRIVATE KEY-----" }).key).toMatch(/unencrypted/);
    expect(validateCertificateDraft({ name: "x", chain, key: chain }).key).toMatch(/PEM private key/);
  });
});

describe("wildcard route ownership", () => {
  it("matches exactly one label below a wildcard route, not the apex, deeper names or another wildcard", () => {
    expect(wildcardCovers("*.example.com", "app.example.com")).toBe(true);
    expect(wildcardCovers("*.example.com", "example.com")).toBe(false);
    expect(wildcardCovers("*.example.com", "a.b.example.com")).toBe(false);
    expect(wildcardCovers("*.example.com", "*.example.com")).toBe(false);
    expect(wildcardCovers("app.example.com", "app.example.com")).toBe(false);
  });

  it("lets a wildcard route own one-label names and requires the wildcard route for a wildcard name", () => {
    expect(ownershipProblem({ dns_names: ["*.example.com", "app.example.com"] }, ["*.example.com"])).toBeNull();
    expect(ownershipProblem({ dns_names: ["example.com"] }, ["*.example.com"])).toMatch(/also names example\.com/);
    expect(ownershipProblem({ dns_names: ["a.b.example.com"] }, ["*.example.com"])).toMatch(/a\.b\.example\.com/);
    expect(ownershipProblem({ dns_names: ["*.example.com"] }, ["app.example.com"])).toMatch(/\*\.example\.com/);
  });
});

describe("certificate sources and issuer phases", () => {
  it("labels uploads, Origin CA versions and unknown sources without claiming browser trust for Origin CA", () => {
    expect(certificateSource(undefined).label).toBe("Uploaded");
    expect(certificateSource("uploaded").label).toBe("Uploaded");
    expect(certificateSource("cloudflare_origin")).toEqual({ label: "Cloudflare Origin CA", trust: expect.stringMatching(/Browsers do not trust/) as unknown });
    expect(certificateSource("future_ca").label).toBe("Other source");
  });

  it("never shows an unknown phase as complete and offers Retry only after failure or an uncertain request", () => {
    expect(issuerPhase("issued")).toEqual({ tone: "ok", label: "Issued" });
    expect(issuerPhase("failed").tone).toBe("bad");
    expect(issuerPhase("revoking")).toEqual({ tone: "neutral", label: "Unknown state (revoking)" });
    expect(["requesting", "reconciling", "applying"].every(issuerWorking)).toBe(true);
    expect(issuerWorking("issued")).toBe(false);
    expect(["failed", "reconciling"].every(issuerRetryable)).toBe(true);
    expect(["requesting", "applying", "issued", "unknown"].some(issuerRetryable)).toBe(false);
  });

  it("names validity periods", () => {
    expect([7, 90, 365, 1095, 5475].map(validityLabel)).toEqual(["7 days", "90 days", "1 year", "3 years", "15 years"]);
  });
});

describe("validateIssuerDraft", () => {
  const zone = { zone_id: "z".repeat(32), domain: "example.com", domains: ["example.com", "*.example.com"] };
  const draft = (patch: Partial<IssuerDraft>): IssuerDraft => ({ name: "Edge origin", zoneId: zone.zone_id, hostnames: "example.com\n*.example.com", validityDays: 365, autoRenew: true, ...patch });

  it("accepts shared exact and wildcard names in the zone", () => {
    expect(validateIssuerDraft(draft({}), zone)).toEqual({});
    expect(validateIssuerDraft(draft({ hostnames: "APP.example.com, api.example.com" }), zone)).toEqual({});
  });

  it("limits the name to 60 bytes without control characters", () => {
    expect(validateIssuerDraft(draft({ name: "  " }), zone).name).toMatch(/Enter a name/);
    expect(validateIssuerDraft(draft({ name: "a".repeat(60) }), zone).name).toBeUndefined();
    expect(validateIssuerDraft(draft({ name: "é".repeat(31) }), zone).name).toMatch(/60 bytes/);
    expect(validateIssuerDraft(draft({ name: "two\tparts" }), zone).name).toMatch(/control/);
  });

  it("refuses missing zones, bad, duplicate, too many, outside and unshared names", () => {
    expect(validateIssuerDraft(draft({ zoneId: "" }), undefined).zoneId).toMatch(/zone/);
    expect(validateIssuerDraft(draft({ hostnames: "" }), zone).hostnames).toMatch(/at least one/);
    expect(validateIssuerDraft(draft({ hostnames: "*.com" }), zone).hostnames).toMatch(/complete DNS hostname/);
    expect(validateIssuerDraft(draft({ hostnames: "a.*.example.com" }), zone).hostnames).toMatch(/complete DNS hostname/);
    expect(validateIssuerDraft(draft({ hostnames: "app.example.com app.example.com" }), zone).hostnames).toMatch(/more than once/);
    expect(validateIssuerDraft(draft({ hostnames: Array.from({ length: 33 }, (_, i) => `h${i}.example.com`).join("\n") }), zone).hostnames).toMatch(/32/);
    expect(validateIssuerDraft(draft({ hostnames: "app.example.net" }), zone).hostnames).toMatch(/not in the example\.com zone/);
    expect(validateIssuerDraft(draft({ hostnames: "app.example.com" }), { ...zone, domains: ["www.example.com"] }).hostnames).toMatch(/not shared/);
  });

  it("mirrors the zone share: a wildcard share never covers its apex, and an exact share never covers a wildcard", () => {
    expect(shareAllows(["*.example.com"], "app.example.com")).toBe(true);
    expect(shareAllows(["*.example.com"], "example.com")).toBe(false);
    expect(shareAllows(["*.example.com"], "*.example.com")).toBe(true);
    expect(shareAllows(["*.example.com"], "*.dev.example.com")).toBe(true);
    expect(shareAllows(["app.example.com"], "*.example.com")).toBe(false);
    expect(shareAllows(["example.com"], "example.com")).toBe(true);
  });
});

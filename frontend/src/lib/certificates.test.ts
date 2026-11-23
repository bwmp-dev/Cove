import { describe, expect, it } from "vitest";
import { certificateStatus, ownershipProblem, validateCertificateDraft } from "./certificates";

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

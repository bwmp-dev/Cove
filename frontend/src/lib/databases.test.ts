import { describe, expect, it } from "vitest";
import type { DatabaseResource } from "../api/types";
import { databaseEndpoints, extensionNeedsVariant, isSecretDatabaseExport, normalisePem, validatePublicAccess, type PublicAccessDraft } from "./databases";

const certificate = "-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----\n";
const key = "-----BEGIN EC PRIVATE KEY-----\nMHc\n-----END EC PRIVATE KEY-----\n";
const draft = (changes: Partial<PublicAccessDraft> = {}): PublicAccessDraft => ({ hostname: "db.example.com", bind_address: "203.0.113.10", port: "5432", certificate, private_key: key, ca_certificate: "", ...changes });
const postgres: DatabaseResource = { engine: "postgresql", version: "18.7-alpine", template_revision: "r", service: "postgres", image: "postgres:18.7-alpine", port: 5432, host: "db-abc.internal" };

describe("validatePublicAccess", () => {
  it("accepts a DNS hostname, worker address, unprivileged port and PEM material", () => {
    expect(validatePublicAccess(draft())).toEqual({});
    expect(validatePublicAccess(draft({ bind_address: "::" }))).toEqual({});
    expect(validatePublicAccess(draft({ bind_address: "2001:db8::10", ca_certificate: certificate }))).toEqual({});
  });

  it("rejects IP hostnames, reserved names and unsupported bind addresses", () => {
    expect(validatePublicAccess(draft({ hostname: "203.0.113.10" })).hostname).toBeDefined();
    expect(validatePublicAccess(draft({ hostname: "cove-db.example.com" })).hostname).toBeDefined();
    expect(validatePublicAccess(draft({ hostname: "db..example.com" })).hostname).toBeDefined();
    expect(validatePublicAccess(draft({ bind_address: "224.0.0.1" })).bind_address).toBeDefined();
    expect(validatePublicAccess(draft({ bind_address: "169.254.0.1" })).bind_address).toBeDefined();
    expect(validatePublicAccess(draft({ bind_address: "fe80::1" })).bind_address).toBeDefined();
    expect(validatePublicAccess(draft({ bind_address: "db.example.com" })).bind_address).toBeDefined();
  });

  it("enforces the port range and PEM size limits", () => {
    expect(validatePublicAccess(draft({ port: "443" })).port).toBeDefined();
    expect(validatePublicAccess(draft({ port: "65536" })).port).toBeDefined();
    expect(validatePublicAccess(draft({ port: "1024" })).port).toBeUndefined();
    const longKey = `-----BEGIN PRIVATE KEY-----\n${"A".repeat(16400)}\n-----END PRIVATE KEY-----`;
    expect(validatePublicAccess(draft({ private_key: longKey })).private_key).toMatch(/16 KiB/);
    expect(validatePublicAccess(draft({ certificate: "not a certificate" })).certificate).toBeDefined();
    expect(validatePublicAccess(draft({ ca_certificate: "junk" })).ca_certificate).toBeDefined();
  });
});

describe("normalisePem", () => {
  it("restores line breaks a password input removed", () => {
    const body = "A".repeat(70);
    expect(normalisePem(`-----BEGIN PRIVATE KEY-----${body}-----END PRIVATE KEY-----`)).toBe(`-----BEGIN PRIVATE KEY-----\n${"A".repeat(64)}\nAAAAAA\n-----END PRIVATE KEY-----\n`);
  });

  it("leaves unrecognised input for server validation", () => {
    expect(normalisePem("  something else  ")).toBe("something else");
  });
});

describe("database endpoints", () => {
  it("offers the pooler and ClickHouse HTTP only where they exist", () => {
    expect(databaseEndpoints(postgres).map((item) => item.key)).toEqual(["native"]);
    expect(databaseEndpoints({ ...postgres, pooler: true, pooler_host: "pool-abc.internal" })[1]).toMatchObject({ service: "pgbouncer", port: 6432, alias: "pool-abc.internal" });
    expect(databaseEndpoints({ ...postgres, pooler: false, pooler_host: "" }).map((item) => item.key)).toEqual(["native"]);
    expect(databaseEndpoints({ ...postgres, engine: "clickhouse", service: "clickhouse", port: 9000 }).map((item) => item.port)).toEqual([9000, 8123]);
  });

  it("requires the matching image variant for vector and PostGIS", () => {
    expect(extensionNeedsVariant("vector", "")).toBe(true);
    expect(extensionNeedsVariant("vector", "vector")).toBe(false);
    expect(extensionNeedsVariant("postgis", "vector")).toBe(true);
    expect(extensionNeedsVariant("pg_trgm", undefined)).toBe(false);
  });
});

it("treats only database passwords and connection URLs as secret exports", () => {
  for (const key of ["DB_PASSWORD", "DB_URL", "DB_POOL_PASSWORD", "DB_POOL_URL", "DB_PUBLIC_URL"]) expect(isSecretDatabaseExport(key)).toBe(true);
  for (const key of ["DB_HOST", "DB_PORT", "DB_USER", "DB_NAME", "DB_POOL_HOST", "DB_POOL_PORT", "DB_POOL_USER", "DB_POOL_NAME", "DB_PUBLIC_HOST", "DB_PUBLIC_PORT", "DB_TLS_CA", "POSTGRES_PASSWORD", "CALLBACK_URL"]) {
    expect(isSecretDatabaseExport(key)).toBe(false);
  }
});

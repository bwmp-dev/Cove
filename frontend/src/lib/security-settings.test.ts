import { describe, expect, it } from "vitest";
import { extraScopes, generateSecret, parseScopes, validateAuditExport, validateProvider } from "./security-settings";

describe("identity provider form", () => {
  const valid = { display_name: "Corp", issuer: "https://login.example.com", client_id: "cove", scopes: "groups" };

  it("accepts a complete HTTPS provider", () => {
    expect(validateProvider(valid)).toEqual({});
  });

  it("requires HTTPS issuers, a name and a client ID", () => {
    expect(Object.keys(validateProvider({ ...valid, issuer: "http://login.example.com", display_name: " ", client_id: "" })).sort()).toEqual(["client_id", "display_name", "issuer"]);
  });

  it("splits scopes and hides the ones always requested", () => {
    expect(parseScopes(" groups, offline_access groups ")).toEqual(["groups", "offline_access"]);
    expect(extraScopes(["openid", "email", "profile", "groups"])).toEqual(["groups"]);
    expect(validateProvider({ ...valid, scopes: 'bad"scope' }).scopes).toBeDefined();
  });
});

describe("audit export form", () => {
  const empty = { webhook_url: "", webhook_secret: "", syslog_address: "", retention_days: "365" };

  it("allows both sinks to be off", () => {
    expect(validateAuditExport(empty, false)).toEqual({});
  });

  it("requires a long enough secret for a new webhook but keeps a saved one", () => {
    const webhook = { ...empty, webhook_url: "https://siem.example.com/hook" };
    expect(validateAuditExport(webhook, false).webhook_secret).toBeDefined();
    expect(validateAuditExport(webhook, true)).toEqual({});
    expect(validateAuditExport({ ...webhook, webhook_secret: "short" }, true).webhook_secret).toMatch(/32 bytes/);
    expect(validateAuditExport({ ...webhook, webhook_secret: generateSecret() }, false)).toEqual({});
    expect(validateAuditExport({ ...webhook, webhook_url: "http://siem.example.com" }, true).webhook_url).toBeDefined();
  });

  it("checks syslog host:port and the retention range", () => {
    expect(validateAuditExport({ ...empty, syslog_address: "logs.example.com:6514" }, false)).toEqual({});
    expect(validateAuditExport({ ...empty, syslog_address: "logs.example.com" }, false).syslog_address).toBeDefined();
    expect(validateAuditExport({ ...empty, syslog_address: "logs:70000" }, false).syslog_address).toBeDefined();
    expect(validateAuditExport({ ...empty, retention_days: "0" }, false).retention_days).toBeDefined();
    expect(validateAuditExport({ ...empty, retention_days: "3651" }, false).retention_days).toBeDefined();
    expect(validateAuditExport({ ...empty, retention_days: "1.5" }, false).retention_days).toBeDefined();
  });

  it("generates 32 random bytes as hex", () => {
    expect(generateSecret()).toMatch(/^[0-9a-f]{64}$/);
    expect(generateSecret()).not.toBe(generateSecret());
  });
});

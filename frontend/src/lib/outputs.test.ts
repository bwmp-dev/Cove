import { describe, expect, it } from "vitest";
import type { UptimeCheck } from "../api/outputs";
import { forwarderEndpointProblem, parseTrustedProxies, trustedProxyProblem, uptimeCheckFromForm, uptimeState } from "./outputs";

function form(entries: Record<string, string>) {
  const data = new FormData();
  for (const [key, value] of Object.entries(entries)) data.append(key, value);
  return data;
}

describe("trusted proxies", () => {
  it("accepts IPv4 and IPv6 CIDR ranges and removes duplicates", () => {
    expect(parseTrustedProxies("203.0.113.0/24\n2001:db8::/32, 203.0.113.0/24\n\n10.0.0.1/32")).toEqual({ proxies: ["203.0.113.0/24", "2001:db8::/32", "10.0.0.1/32"] });
    expect(parseTrustedProxies("")).toEqual({ proxies: [] });
  });

  it("refuses all-address ranges, bare addresses and invalid prefixes", () => {
    expect(trustedProxyProblem("0.0.0.0/0")).toContain("trusts every address");
    expect(trustedProxyProblem("::/0")).toContain("trusts every address");
    expect(trustedProxyProblem("203.0.113.5")).toContain("CIDR notation");
    expect(trustedProxyProblem("203.0.113.0/33")).toContain("above 32");
    expect(trustedProxyProblem("2001:db8::/129")).toContain("above 128");
    expect(trustedProxyProblem("256.1.1.1/8")).toContain("not a valid");
    expect(trustedProxyProblem("example.com/24")).toContain("not a valid");
  });

  it("caps the list at 32 ranges", () => {
    const many = Array.from({ length: 33 }, (_, index) => `10.0.${index}.0/24`).join("\n");
    expect(parseTrustedProxies(many)).toEqual({ error: "Enter at most 32 ranges." });
  });
});

describe("uptimeCheckFromForm", () => {
  const valid = { domain: "app.example.test", path: "/healthz", interval_seconds: "300", timeout_seconds: "5", expected_min: "200", expected_max: "399", public_label: " API ", enabled: "on" };

  it("builds the API body", () => {
    expect(uptimeCheckFromForm(form(valid))).toEqual({ domain: "app.example.test", path: "/healthz", enabled: true, interval_seconds: 300, timeout_seconds: 5, expected_min: 200, expected_max: 399, public_label: "API" });
    const disabled = form(valid);
    disabled.delete("enabled");
    expect(uptimeCheckFromForm(disabled)).toMatchObject({ enabled: false });
  });

  it("enforces the API limits", () => {
    const field = (patch: Record<string, string>) => {
      const result = uptimeCheckFromForm(form({ ...valid, ...patch }));
      return "field" in result ? result.field : undefined;
    };
    expect(field({ path: "healthz" })).toBe("path");
    expect(field({ path: "//evil.example" })).toBe("path");
    expect(field({ path: "/a?b=1" })).toBe("path");
    expect(field({ path: "/a#b" })).toBe("path");
    expect(field({ path: `/${"a".repeat(2048)}` })).toBe("path");
    expect(field({ interval_seconds: "59" })).toBe("interval_seconds");
    expect(field({ interval_seconds: "86401" })).toBe("interval_seconds");
    expect(field({ interval_seconds: "60.5" })).toBe("interval_seconds");
    expect(field({ timeout_seconds: "11" })).toBe("timeout_seconds");
    expect(field({ expected_min: "99" })).toBe("expected_min");
    expect(field({ expected_max: "199" })).toBe("expected_max");
    expect(field({ expected_max: "600" })).toBe("expected_max");
    expect(field({ public_label: "x".repeat(121) })).toBe("public_label");
    expect(field({ domain: "" })).toBe("domain");
  });
});

describe("uptimeState", () => {
  const now = Date.parse("2026-11-22T12:00:00Z");
  const check: UptimeCheck = { id: "u1", domain: "a", path: "/", enabled: true, interval_seconds: 60, timeout_seconds: 5, expected_min: 200, expected_max: 399, public_label: "", checked_at: "2026-11-22T11:59:00Z", up: true, status: 200, latency_ms: 40, error: "", consecutive_failures: 0 };

  it("reports fresh results, and stale or missing ones as unknown", () => {
    expect(uptimeState(check, now)).toEqual({ tone: "ok", label: "Up" });
    expect(uptimeState({ ...check, up: false }, now)).toEqual({ tone: "bad", label: "Down" });
    expect(uptimeState({ ...check, checked_at: "2026-11-22T11:57:54Z" }, now).label).toBe("Unknown (stale)");
    expect(uptimeState({ ...check, checked_at: "2026-11-22T11:57:56Z" }, now).label).toBe("Up");
    expect(uptimeState({ ...check, checked_at: null, up: null }, now).label).toBe("Not checked yet");
    expect(uptimeState({ ...check, enabled: false }, now).label).toBe("Disabled");
  });
});

describe("forwarderEndpointProblem", () => {
  it("requires full HTTPS URLs without credentials or fragments for HTTP receivers", () => {
    expect(forwarderEndpointProblem("loki", "https://logs.example.test/loki/api/v1/push")).toBeUndefined();
    expect(forwarderEndpointProblem("http", "http://logs.example.test/in")).toBe("Enter a full https:// URL.");
    expect(forwarderEndpointProblem("axiom", "https://user:pass@api.example.test/ingest")).toContain("Remove the credentials");
    expect(forwarderEndpointProblem("datadog", "https://intake.example.test/api/v2/logs#x")).toContain("fragment");
    expect(forwarderEndpointProblem("otlp", "logs.example.test")).toBe("Enter a full https:// URL.");
  });

  it("accepts only tls://host:port for syslog", () => {
    expect(forwarderEndpointProblem("syslog", "tls://syslog.example.test:6514")).toBeUndefined();
    expect(forwarderEndpointProblem("syslog", "tls://syslog.example.test")).toContain("tls://hostname:port");
    expect(forwarderEndpointProblem("syslog", "tcp://syslog.example.test:514")).toContain("tls://hostname:port");
    expect(forwarderEndpointProblem("syslog", "tls://syslog.example.test:6514/path")).toContain("tls://hostname:port");
    expect(forwarderEndpointProblem("syslog", "tls://user@syslog.example.test:6514")).toContain("tls://hostname:port");
  });
});

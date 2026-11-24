import { describe, expect, it } from "vitest";
import { normalTunnelStatus, tunnelPollInterval, type TunnelStatus, type TunnelStatusReply } from "../api/cloudflare-tunnel";
import { controlPhase, dnsPhase, draftFrom, edgeState, imageError, tunnelRemains, validateTunnelDraft } from "./cloudflare-tunnel";

const pin = "docker.io/cloudflare/cloudflared:2026.11.1@sha256:e613aadbc2d9511785d2e67a86b0c8ac18dd63a3f9af0d06faf04cbfdcb35f2d";

// Fixture status, not a real control plane reply.
function status(extra: Partial<TunnelStatus> = {}): TunnelStatus {
  return normalTunnelStatus({
    configuration: null,
    phase: "disabled",
    cleanup_requested: false,
    last_error: "",
    next_check_at: null,
    worker: null,
    worker_error: null,
    zones: [{ id: "z1", domain: "example.com", connection_kind: "token" }, { id: "z2", domain: "example.org", connection_kind: "oauth" }],
    available_hostnames: ["app.example.com"],
    dns: [],
    default_image: pin,
    ...extra,
  });
}

describe("normalTunnelStatus", () => {
  it("turns Go's null lists into arrays", () => {
    const reply: TunnelStatusReply = {
      configuration: { zone_id: "z1", hostnames: null, image: pin },
      phase: "active",
      cleanup_requested: false,
      last_error: "",
      next_check_at: null,
      worker: { image: pin, hostnames: null, phase: "active", running: true, connected: true, withheld: null },
      worker_error: "",
      zones: null,
      available_hostnames: null,
      dns: null,
      default_image: pin,
    };
    const normal = normalTunnelStatus(reply);
    expect(normal.configuration?.hostnames).toEqual([]);
    expect(normal.worker?.hostnames).toEqual([]);
    expect(normal).toMatchObject({ zones: [], available_hostnames: [], dns: [], worker_error: null });
  });
});

describe("imageError", () => {
  it("accepts only the official image pinned by a numeric release and digest", () => {
    expect(imageError(pin)).toBeUndefined();
    expect(imageError(pin.replace("docker.io/", ""))).toBeUndefined();
    expect(imageError(`cloudflare/cloudflared:2026.11.1-rc.1@sha256:${"a".repeat(64)}`)).toBeUndefined();
    expect(imageError("cloudflare/cloudflared:2026.11.1")).toMatch(/Pin a release version and digest/);
    expect(imageError(`cloudflare/cloudflared:2026.11@sha256:${"a".repeat(64)}`)).toMatch(/Pin a release version and digest/);
    expect(imageError(`cloudflare/cloudflared:2026.11.1@sha256:${"A".repeat(64)}`)).toMatch(/Pin a release version and digest/);
    expect(imageError(`ghcr.io/cloudflare/cloudflared:2026.11.1@sha256:${"a".repeat(64)}`)).toBe("Use the official cloudflare/cloudflared image.");
    expect(imageError(`cloudflare/cloudflared-fork:2026.11.1@sha256:${"a".repeat(64)}`)).toBe("Use the official cloudflare/cloudflared image.");
    expect(imageError(`cloudflare/cloudflared:latest@sha256:${"a".repeat(64)}`)).toBe("Pin a release version such as 2026.11.1, not latest.");
    expect(imageError(" ")).toBe("Enter the connector image.");
  });
});

describe("edgeState", () => {
  const worker = { image: pin, hostnames: [], phase: "active", running: true, connected: true, withheld: null };

  it("reports connected only from the worker's own report", () => {
    expect(edgeState(status({ phase: "active", worker })).label).toBe("Connected to the Cloudflare edge");
    expect(edgeState(status({ phase: "active", worker: { ...worker, connected: false } })).label).toBe("Running, not connected");
    expect(edgeState(status({ phase: "active", worker: { ...worker, running: false, connected: false } })).label).toBe("Not running");
  });

  it("is unknown when the worker read failed, even if a stale report says connected", () => {
    expect(edgeState(status({ phase: "active", worker, worker_error: "worker unreachable" })).label).toBe("Unknown");
    expect(edgeState(status({ phase: "active" })).label).toBe("Unknown");
  });
});

describe("tunnelPollInterval", () => {
  it("polls quickly while work or cleanup is in progress", () => {
    expect(tunnelPollInterval(status({ phase: "connecting" }))).toBe(3_000);
    expect(tunnelPollInterval(status({ phase: "blocked", cleanup_requested: true }))).toBe(3_000);
    expect(tunnelPollInterval(status({ dns: [{ hostname: "app.example.com", phase: "present", cleanup_requested: true, last_error: "" }] }))).toBe(3_000);
    expect(tunnelPollInterval(status({ phase: "active" }))).toBe(30_000);
    expect(tunnelPollInterval(status({ phase: "blocked" }))).toBe(30_000);
  });
});

describe("tunnel state", () => {
  it("keeps unknown phases visible as unknown", () => {
    expect(controlPhase("rerouting")).toEqual({ tone: "warn", label: "Unknown state (rerouting)", active: false });
  });

  it("labels the control plane's DNS states and keeps others unknown", () => {
    expect(dnsPhase("pending", false, "").label).toBe("Pending");
    expect(dnsPhase("presenting", false, "")).toEqual({ tone: "info", label: "Creating, confirming ownership", active: true });
    expect(dnsPhase("present", false, "")).toEqual({ tone: "ok", label: "Present", active: false });
    expect(dnsPhase("cleanup", true, "").label).toBe("Removing");
    expect(dnsPhase("present", true, "")).toEqual({ tone: "info", label: "Removal pending", active: true });
    expect(dnsPhase("present", true, "DNS delete failed.")).toMatchObject({ tone: "bad", label: "Removal failed" });
    expect(dnsPhase("presenting", false, "Existing record refused.").tone).toBe("warn");
    expect(dnsPhase("active", false, "").label).toBe("Unknown state (active)");
  });

  it("polls quickly while a DNS record's create outcome is unconfirmed", () => {
    expect(tunnelPollInterval(status({ phase: "active", dns: [{ hostname: "app.example.com", phase: "presenting", cleanup_requested: false, last_error: "" }] }))).toBe(3_000);
    expect(tunnelPollInterval(status({ phase: "active", dns: [{ hostname: "app.example.com", phase: "present", cleanup_requested: false, last_error: "" }] }))).toBe(30_000);
  });

  it("treats remaining cleanup rows as a tunnel that still exists", () => {
    expect(tunnelRemains(status())).toBe(false);
    expect(tunnelRemains(status({ dns: [{ hostname: "app.example.com", phase: "cleanup", cleanup_requested: true, last_error: "" }] }))).toBe(true);
  });
});

describe("validateTunnelDraft", () => {
  it("refuses OAuth zones, other zones' hostnames and changing the zone of an existing tunnel", () => {
    const fresh = status();
    expect(draftFrom(fresh)).toEqual({ zoneId: "z1", hostnames: [], image: pin });
    expect(validateTunnelDraft({ zoneId: "z2", hostnames: ["a.example.org"], image: pin }, fresh).zone).toMatch(/OAuth/);
    expect(validateTunnelDraft({ zoneId: "z1", hostnames: ["a.example.org"], image: pin }, fresh).hostnames).toBe("Every hostname must be in example.com.");
    expect(validateTunnelDraft({ zoneId: "z1", hostnames: [], image: pin }, fresh).hostnames).toBe("Choose at least one hostname.");
    expect(validateTunnelDraft({ zoneId: "z1", hostnames: ["app.example.com"], image: pin }, fresh)).toEqual({});
    const existing = status({ configuration: { zone_id: "z3", hostnames: ["x.example.net"], image: pin }, zones: [...fresh.zones, { id: "z3", domain: "example.net", connection_kind: "token" }] });
    expect(validateTunnelDraft({ zoneId: "z1", hostnames: ["app.example.com"], image: pin }, existing).zone).toMatch(/Disable the tunnel/);
  });
});

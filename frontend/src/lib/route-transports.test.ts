import { describe, expect, it } from "vitest";
import type { Listener } from "../api/ingress-listeners";
import type { Route } from "../api/types";
import { emptyTransport, isLinkableRoute, isPublicHTTPRoute, refreshTransportErrors, sameTransports, splitRoutes, transportRows, transportSummary, validateTransports, type TransportContext, type TransportRow } from "./route-transports";

// API-shaped fixtures; not product data.
const listeners: Listener[] = [
  { id: "postgres", protocol: "tcp", visibility: "public", bindings: [{ address: "0.0.0.0", port: 5432 }] },
  { id: "dns", protocol: "udp", visibility: "lan", bindings: [{ address: "192.168.1.2", port: 5353 }] },
  { id: "web", protocol: "https", visibility: "lan", bindings: [{ address: "192.168.1.2", port: 8443 }] },
];
const tcp: Route = { protocol: "tcp", domain: "", service: "db", port: 5432, policy: { listener: "postgres", visibility: "public", ip_allow_list: ["192.168.0.0/16"], upstreams: [{ service: "db2", port: 5432 }] } };
const udp: Route = { protocol: "udp", domain: "", service: "resolver", port: 53, policy: { listener: "dns", visibility: "lan" } };
const context = (overrides: Partial<TransportContext> = {}): TransportContext => ({ routeProtocol: 6, listeners, saved: [], httpCount: 0, ...overrides });
const row = (patch: Partial<TransportRow>): TransportRow => ({ ...emptyTransport(), ...patch });

describe("TCP and UDP routes", () => {
  it("round-trips the exact raw TCP request", () => {
    const rows = transportRows([tcp]);
    expect(sameTransports(rows, [tcp])).toBe(true);
    expect(validateTransports(rows, context({ saved: [tcp] }))).toEqual({ routes: [tcp], errors: {} });
  });

  it("splits mixed routes and drops nothing", () => {
    const http: Route = { domain: "app.example.com", service: "web", port: 80 };
    expect(splitRoutes([udp, http, tcp])).toEqual({ http: [http], transport: [udp, tcp] });
    expect(splitRoutes(null)).toEqual({ http: [], transport: [] });
  });

  it("never sends an allowlist for UDP, even after switching from TCP", () => {
    const [draft] = transportRows([tcp]);
    if (!draft) throw new Error("missing row");
    const result = validateTransports([{ ...draft, protocol: "udp", listener: "dns", visibility: "lan" }], context());
    expect(result.routes).toEqual([{ protocol: "udp", domain: "", service: "db", port: 5432, policy: { listener: "dns", visibility: "lan", upstreams: [{ service: "db2", port: 5432 }] } }]);
  });

  it("requires an explicit visibility and a matching listener", () => {
    const first = row({ service: "db", port: 5432, listener: "" });
    const missingChoices = validateTransports([first], context()).errors;
    expect(Object.keys(missingChoices).sort()).toEqual([`${first.id}.listener`, `${first.id}.visibility`]);
    expect(missingChoices[`${first.id}.visibility`]).toMatch(/Choose who can connect/);
    expect(missingChoices[`${first.id}.listener`]).toBe("Choose the TCP listener that receives this traffic.");
    const wrongProtocol = row({ service: "db", port: 5432, visibility: "lan", listener: "dns" });
    expect(validateTransports([wrongProtocol], context()).errors[`${wrongProtocol.id}.listener`]).toBe("Listener dns carries UDP. Choose a TCP listener.");
    const wrongVisibility = row({ service: "db", port: 5432, visibility: "tailscale", listener: "postgres" });
    expect(validateTransports([wrongVisibility], context()).errors[`${wrongVisibility.id}.listener`]).toMatch(/has Public visibility\. Choose a Tailscale TCP listener/);
    const https = row({ service: "db", port: 5432, visibility: "lan", listener: "web" });
    expect(validateTransports([https], context()).errors[`${https.id}.listener`]).toMatch(/carries HTTPS\. Choose a TCP listener/);
    const missing = row({ service: "db", port: 5432, visibility: "public", listener: "gone" });
    expect(validateTransports([missing], context()).errors[`${missing.id}.listener`]).toMatch(/not configured on this node/);
    expect(validateTransports([missing], context({ listeners: undefined })).errors).toEqual({});
  });

  it("allows one route per listener", () => {
    const a = row({ service: "db", port: 5432, visibility: "public", listener: "postgres" });
    const b = row({ service: "db2", port: 5432, visibility: "public", listener: "postgres" });
    const errors = validateTransports([a, b], context()).errors;
    expect(errors).toEqual({ [`${b.id}.listener`]: "Listener postgres already carries another route here. Each listener carries one TCP or UDP route." });
    expect(refreshTransportErrors(errors, [a, b], [a], context())).toEqual({});
    expect(refreshTransportErrors({ ...errors, other: "HTTP error" }, [a, b], [a, { ...b }], context())).toEqual({ ...errors, other: "HTTP error" });
  });

  it("checks service, port, source ranges and upstreams like HTTP routes", () => {
    const bad = row({ service: "bad name", port: 70000, visibility: "public", listener: "postgres", ipAllowList: "10.0.0.0/33", upstreams: [{ id: "u1", service: "bad name", port: 5432 }, { id: "u2", service: "db", port: 5432 }] });
    const errors = validateTransports([bad], context()).errors;
    expect(errors[bad.id]).toBe("Enter the Compose service name.");
    expect(errors[`${bad.id}.cidr`]).toMatch(/10\.0\.0\.0\/33 is not an IPv4 or IPv6 CIDR/);
    expect(errors[`${bad.id}.upstream.u1`]).toBe("Enter the Compose service name.");
    const duplicate = row({ service: "db", port: 5432, visibility: "public", listener: "postgres", upstreams: [{ id: "u3", service: "db", port: 5432 }] });
    expect(validateTransports([duplicate], context()).errors[`${duplicate.id}.upstream.u3`]).toBe("This service and port is already a target of the route.");
  });

  it("counts HTTP and raw routes together against the route limit", () => {
    const one = row({ service: "db", port: 5432, visibility: "public", listener: "postgres" });
    expect(validateTransports([one], context({ httpCount: 32 })).errors.transports).toMatch(/at most 32 routes/);
    expect(validateTransports([one], context({ httpCount: 31 })).errors).toEqual({});
  });

  it.each([
    [5, /need a newer worker/],
    [undefined, /once the worker capabilities of this node are known/],
  ])("rejects new or changed raw routes on route protocol %s but keeps saved ones for the API to judge", (routeProtocol, message) => {
    expect(validateTransports(transportRows([tcp]), context({ routeProtocol, saved: [tcp] })).errors).toEqual({});
    const added = row({ service: "db", port: 5432, visibility: "public", listener: "postgres" });
    expect(validateTransports([added], context({ routeProtocol })).errors[added.id]).toMatch(message);
    const [changed] = transportRows([tcp]);
    if (!changed) throw new Error("missing row");
    const edited = { ...changed, ipAllowList: "10.0.0.0/8" };
    expect(validateTransports([edited], context({ routeProtocol, saved: [tcp] })).errors[edited.id]).toMatch(message);
    expect(validateTransports([], context({ routeProtocol, saved: [tcp] }))).toEqual({ routes: [], errors: {} });
  });

  it("summarises a raw route without inventing a hostname", () => {
    expect(transportSummary(tcp, listeners)).toBe("TCP · Public · listener postgres (0.0.0.0:5432)");
    expect(transportSummary(udp)).toBe("UDP · LAN only · listener dns");
  });
});

describe("public links", () => {
  const http = (policy?: Route["policy"], domain = "app.example.com"): Route => ({ domain, service: "web", port: 80, policy });

  it("links only public exact HTTP hostnames", () => {
    expect(isLinkableRoute(http())).toBe(true);
    expect(isLinkableRoute(http({ visibility: "public" }))).toBe(true);
    expect(isLinkableRoute(http(undefined, "*.example.com"))).toBe(false);
    expect(isLinkableRoute(http({ visibility: "lan", listener: "web" }))).toBe(false);
    expect(isLinkableRoute(http({ visibility: "tailscale", listener: "tailnet" }))).toBe(false);
    expect(isLinkableRoute(tcp)).toBe(false);
    expect(isLinkableRoute(udp)).toBe(false);
  });

  it("keeps public wildcards as uptime targets but never private or raw routes", () => {
    expect(isPublicHTTPRoute(http(undefined, "*.example.com"))).toBe(true);
    expect(isPublicHTTPRoute(http({ visibility: "lan", listener: "web" }, "*.example.com"))).toBe(false);
    expect(isPublicHTTPRoute({ ...tcp, policy: { ...tcp.policy, visibility: "public" } })).toBe(false);
  });
});

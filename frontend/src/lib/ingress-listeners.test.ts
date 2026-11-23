import { describe, expect, it } from "vitest";
import type { ListenerStatus } from "../api/ingress-listeners";
import { bindingDraft, draftFrom, listenerDraft, parseAddress, phaseInfo, publicChange, settingsFrom, settingsKey, validateDraft, type ListenerDraftSettings } from "./ingress-listeners";

const actual = { http: [{ address: "0.0.0.0", port: 80 }, { address: "::", port: 80 }], https: [{ address: "0.0.0.0", port: 443 }, { address: "::", port: 443 }] };

function status(extra: Partial<ListenerStatus> = {}): ListenerStatus {
  return { settings: { http3: false, listeners: [] }, ports: null, proxy_present: true, running: true, public_bindings: actual, ...extra };
}

function draft(extra: Partial<ListenerDraftSettings> = {}): ListenerDraftSettings {
  return { ...draftFrom(status()), ...extra };
}

function listener(id: string, protocol: "https" | "tcp" | "udp", visibility: "public" | "lan" | "tailscale", ...bindings: [string, number][]) {
  return listenerDraft({ id, protocol, visibility, bindings: bindings.map(([address, port]) => ({ address, port })) });
}

const messages = (value: ListenerDraftSettings) => Object.values(validateDraft(value, actual));

describe("parseAddress", () => {
  it("accepts IPv4 and IPv6 literals and rejects the rest", () => {
    expect(parseAddress("192.168.1.20")?.v4).toBe(true);
    expect(parseAddress("fd7a:115c:a1e0::1")?.v4).toBe(false);
    expect(parseAddress("::")?.bytes.every((byte) => byte === 0)).toBe(true);
    expect(parseAddress("::ffff:10.0.0.1")?.bytes.slice(10)).toEqual([0xff, 0xff, 10, 0, 0, 1]);
    for (const value of ["192.168.1", "192.168.01.1", "256.1.1.1", "1:2:3:4:5:6:7:8:9", "1::2::3", "[::1]", "fe80::1%eth0", "host.example"]) expect(parseAddress(value)).toBeNull();
  });
});

describe("draft round trip", () => {
  it("omits public bindings while they are retained and sends them once set explicitly", () => {
    const retained = draftFrom(status());
    expect(settingsFrom(retained)).toEqual({ http3: false, listeners: [] });
    expect(settingsFrom({ ...retained, publicMode: "explicit" }).public).toEqual(actual);
  });

  it("starts explicit lists from saved intent and ignores ordering when comparing", () => {
    const saved = { public: { http: [], https: [{ address: "203.0.113.5", port: 443 }] }, http3: true, listeners: [{ id: "db", protocol: "tcp" as const, visibility: "lan" as const, bindings: [{ address: "10.0.0.5", port: 5432 }, { address: "10.0.0.6", port: 5432 }] }] };
    const value = draftFrom(status({ settings: saved }));
    expect(value.publicMode).toBe("explicit");
    const reordered = { ...value, listeners: value.listeners.map((item) => ({ ...item, bindings: [...item.bindings].reverse() })) };
    expect(settingsKey(settingsFrom(reordered))).toBe(settingsKey(saved));
  });
});

describe("validateDraft", () => {
  it("accepts a private LAN, Tailscale and loopback public listener", () => {
    expect(messages(draft({ listeners: [listener("db", "tcp", "lan", ["192.168.1.20", 5432]), listener("tail", "https", "tailscale", ["100.101.102.103", 8443]), listener("local", "tcp", "public", ["127.0.0.1", 18080])] }))).toEqual([]);
  });

  it("rejects names, visibility and addresses the worker refuses", () => {
    const errors = messages(
      draft({
        listeners: [
          listener("Bad Name", "tcp", "lan", ["10.0.0.1", 1]),
          listener("dup", "https", "public", ["0.0.0.0", 8443]),
          listener("dup", "tcp", "lan", ["100.64.0.1", 2]),
          listener("wild", "udp", "tailscale", ["0.0.0.0", 3]),
          listener("mapped", "tcp", "public", ["::ffff:10.0.0.1", 4]),
          listener("multi", "udp", "public", ["239.1.1.1", 5]),
          listener("port", "tcp", "public", ["127.0.0.1", 70000]),
        ],
      }),
    );
    expect(errors).toEqual(
      expect.arrayContaining([
        expect.stringContaining("lowercase letters"),
        expect.stringContaining("Public HTTPS uses the public bindings"),
        "Another listener already uses this name.",
        expect.stringContaining("not a Tailscale address"),
        expect.stringContaining("Tailscale address, in 100.64.0.0/10"),
        expect.stringContaining("IPv4-mapped"),
        "Multicast addresses cannot be bound.",
        "Enter a port from 1 to 65535.",
      ]),
    );
  });

  it("reports wildcard overlaps with retained public bindings, but not across transports", () => {
    expect(messages(draft({ listeners: [listener("lan-https", "https", "lan", ["192.168.1.20", 443])] }))).toEqual([expect.stringContaining("Overlaps the current public HTTPS binding")]);
    expect(messages(draft({ listeners: [listener("quic", "udp", "lan", ["192.168.1.20", 443])] }))).toEqual([]);
    expect(messages(draft({ http3: true, listeners: [listener("quic", "udp", "lan", ["192.168.1.20", 443])] }))).toEqual([expect.stringContaining("HTTP/3 UDP binding")]);
  });

  it("requires a single advertised HTTPS port for HTTP/3", () => {
    const explicit = draft({ publicMode: "explicit", http3: true, https: [bindingDraft({ address: "0.0.0.0", port: 443 }), bindingDraft({ address: "::", port: 8443 })] });
    expect(validateDraft(explicit, actual).http3).toContain("same host port");
    expect(validateDraft({ ...explicit, https: [] }, actual).http3).toContain("at least one public HTTPS binding");
  });

  it("limits bindings and requires at least one per listener", () => {
    const empty = listener("empty", "tcp", "lan");
    empty.bindings = [];
    expect(messages(draft({ listeners: [empty] }))).toEqual(["Add at least one binding."]);
    const many = draft({ publicMode: "explicit", http: Array.from({ length: 9 }, (_, index) => bindingDraft({ address: "127.0.0.1", port: 8000 + index })) });
    expect(validateDraft(many, actual).http).toBe("At most 8 HTTP bindings.");
  });
});

describe("publicChange", () => {
  it("warns about removed publications only in explicit mode", () => {
    expect(publicChange(draft(), actual).changed).toBe(false);
    const removed = publicChange(draft({ publicMode: "explicit", http: [], https: [] }), actual);
    expect(removed).toMatchObject({ removesAll: true, removesHttp: true, removesHttps: true, changed: true });
    expect(publicChange(draft({ publicMode: "explicit" }), actual).changed).toBe(false);
  });
});

describe("phaseInfo", () => {
  it("never reports an unknown or uncommitted phase as applied", () => {
    expect(phaseInfo("completed", true).label).toBe("Applied");
    expect(phaseInfo("completed", false).tone).toBe("warn");
    expect(phaseInfo("mystery", true)).toMatchObject({ tone: "warn", active: false, label: "Unknown state (mystery)" });
    expect(phaseInfo("restoring", false).active).toBe(true);
  });
});

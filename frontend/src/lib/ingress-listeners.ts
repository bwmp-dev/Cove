import type { Listener, ListenerBinding, ListenerProtocol, ListenerSettings, ListenerStatus, ListenerVisibility, PublicBindings } from "../api/ingress-listeners";

// Mirrors the worker's limits so obvious mistakes are caught before a plan is requested. The worker stays authoritative.
export const maxListeners = 32;
export const maxBindings = 8;
export const listenerName = /^[a-z0-9][a-z0-9_-]{0,31}$/;

export const protocolLabels: Record<ListenerProtocol, string> = { https: "HTTPS", tcp: "TCP", udp: "UDP" };
export const visibilityLabels: Record<ListenerVisibility, string> = { public: "Public", lan: "Private LAN", tailscale: "Tailscale" };

export function entrypointName(id: string) {
  return `cove-listener-${id}`;
}

interface Address {
  v4: boolean;
  bytes: number[];
}

function parseIPv4(value: string): number[] | null {
  const parts = value.split(".");
  if (parts.length !== 4) return null;
  const bytes: number[] = [];
  for (const part of parts) {
    if (!/^(?:0|[1-9]\d{0,2})$/.test(part)) return null;
    const byte = Number(part);
    if (byte > 255) return null;
    bytes.push(byte);
  }
  return bytes;
}

function parseGroups(value: string): number[] | null {
  if (value === "") return [];
  const groups = value.split(":");
  const words: number[] = [];
  for (const [index, group] of groups.entries()) {
    if (index === groups.length - 1 && group.includes(".")) {
      const v4 = parseIPv4(group);
      if (!v4) return null;
      words.push(((v4[0] ?? 0) << 8) | (v4[1] ?? 0), ((v4[2] ?? 0) << 8) | (v4[3] ?? 0));
    } else {
      if (!/^[0-9a-fA-F]{1,4}$/.test(group)) return null;
      words.push(parseInt(group, 16));
    }
  }
  return words;
}

function parseIPv6(value: string): number[] | null {
  const halves = value.split("::");
  if (halves.length > 2) return null;
  const head = parseGroups(halves[0] ?? "");
  const tail = halves.length === 2 ? parseGroups(halves[1] ?? "") : [];
  if (!head || !tail) return null;
  const missing = 8 - head.length - tail.length;
  if (halves.length === 1 ? missing !== 0 : missing < 1) return null;
  const words = [...head, ...Array<number>(Math.max(missing, 0)).fill(0), ...tail];
  return words.flatMap((word) => [word >> 8, word & 0xff]);
}

export function parseAddress(value: string): Address | null {
  if (value.includes(":")) {
    const bytes = parseIPv6(value);
    return bytes ? { v4: false, bytes } : null;
  }
  const bytes = parseIPv4(value);
  return bytes ? { v4: true, bytes } : null;
}

const unspecified = (address: Address) => address.bytes.every((byte) => byte === 0);
const mappedV4 = (address: Address) => !address.v4 && address.bytes.slice(0, 10).every((byte) => byte === 0) && address.bytes[10] === 0xff && address.bytes[11] === 0xff;
const multicast = ({ v4, bytes: [first = 0] }: Address) => (v4 ? first >= 224 && first <= 239 : first === 0xff);
function tailscale({ v4, bytes }: Address) {
  const [a = 0, b = 0, c = 0, d = 0, e = 0, f = 0] = bytes;
  return v4 ? a === 100 && (b & 0xc0) === 64 : a === 0xfd && b === 0x7a && c === 0x11 && d === 0x5c && e === 0xa1 && f === 0xe0;
}
function privateRange({ v4, bytes }: Address) {
  const [a = 0, b = 0] = bytes;
  return v4 ? a === 10 || (a === 172 && (b & 0xf0) === 16) || (a === 192 && b === 168) : (a & 0xfe) === 0xfc;
}

export function formatBinding({ address, port }: ListenerBinding) {
  return address.includes(":") ? `[${address}]:${port}` : `${address}:${port}`;
}

let counter = 0;
const nextKey = () => `draft-${++counter}`;

export interface BindingDraft {
  key: string;
  address: string;
  port: string;
}

export interface ListenerDraft {
  key: string;
  id: string;
  protocol: ListenerProtocol;
  visibility: ListenerVisibility;
  bindings: BindingDraft[];
}

export interface ListenerDraftSettings {
  publicMode: "retain" | "explicit";
  http: BindingDraft[];
  https: BindingDraft[];
  http3: boolean;
  listeners: ListenerDraft[];
}

export function bindingDraft(binding?: ListenerBinding): BindingDraft {
  return { key: nextKey(), address: binding?.address ?? "", port: binding ? String(binding.port) : "" };
}

export function listenerDraft(listener?: Listener): ListenerDraft {
  return {
    key: nextKey(),
    id: listener?.id ?? "",
    protocol: listener?.protocol ?? "tcp",
    visibility: listener?.visibility ?? "lan",
    bindings: listener ? listener.bindings.map(bindingDraft) : [bindingDraft()],
  };
}

const noBindings: PublicBindings = { http: [], https: [] };

// Explicit public lists start from the saved intent, or from the proxy's actual publications so switching modes
// never silently removes a binding.
export function draftFrom(status: ListenerStatus): ListenerDraftSettings {
  const publicStart = status.settings.public ?? status.public_bindings ?? noBindings;
  return {
    publicMode: status.settings.public ? "explicit" : "retain",
    http: (publicStart.http ?? []).map(bindingDraft),
    https: (publicStart.https ?? []).map(bindingDraft),
    http3: status.settings.http3,
    listeners: (status.settings.listeners ?? []).map(listenerDraft),
  };
}

const binding = (draft: BindingDraft): ListenerBinding => ({ address: draft.address.trim(), port: Number(draft.port.trim()) });

export function settingsFrom(draft: ListenerDraftSettings): ListenerSettings {
  return {
    ...(draft.publicMode === "explicit" ? { public: { http: draft.http.map(binding), https: draft.https.map(binding) } } : {}),
    http3: draft.http3,
    listeners: draft.listeners.map((listener) => ({ id: listener.id.trim(), protocol: listener.protocol, visibility: listener.visibility, bindings: listener.bindings.map(binding) })),
  };
}

// A stable comparison key: order-insensitive like the worker's canonical form, so reordering is not a change.
export function settingsKey(settings: ListenerSettings) {
  const sorted = (items: ListenerBinding[] = []) => items.map(formatBinding).sort();
  return JSON.stringify({
    public: settings.public ? { http: sorted(settings.public.http), https: sorted(settings.public.https) } : null,
    http3: settings.http3,
    listeners: [...(settings.listeners ?? [])].sort((a, b) => a.id.localeCompare(b.id)).map((listener) => [listener.id, listener.protocol, listener.visibility, sorted(listener.bindings)]),
  });
}

export type FieldErrors = Record<string, string>;

interface Reservation {
  address: Address;
  port: number;
  udp: boolean;
  label: string;
}

function samePublication(a: Reservation, b: Reservation) {
  if (a.port !== b.port || a.udp !== b.udp || a.address.v4 !== b.address.v4) return false;
  return unspecified(a.address) || unspecified(b.address) || a.address.bytes.every((byte, index) => byte === b.address.bytes[index]);
}

// Errors are keyed by the draft key of the binding or listener they belong to, plus "http", "https" and "http3".
export function validateDraft(draft: ListenerDraftSettings, actualPublic?: PublicBindings): FieldErrors {
  const errors: FieldErrors = {};
  const reserved: Reservation[] = [];

  function checkBindings(bindings: BindingDraft[], visibility: ListenerVisibility, udp: boolean, label: string) {
    for (const item of bindings) {
      const address = parseAddress(item.address.trim());
      const portText = item.port.trim();
      const port = Number(portText);
      if (!/^\d+$/.test(portText) || port < 1 || port > 65535) errors[`${item.key}.port`] = "Enter a port from 1 to 65535.";
      if (!item.address.trim()) {
        errors[`${item.key}.address`] = "Enter an IPv4 or IPv6 address.";
        continue;
      }
      if (!address) {
        errors[`${item.key}.address`] = item.address.includes("%") ? "Interface zones are not supported." : "Enter an IPv4 or IPv6 literal, without brackets or a port.";
        continue;
      }
      if (mappedV4(address)) errors[`${item.key}.address`] = "Use the IPv4 address instead of its IPv4-mapped IPv6 form.";
      else if (multicast(address)) errors[`${item.key}.address`] = "Multicast addresses cannot be bound.";
      else if (visibility === "lan" && (unspecified(address) || !privateRange(address) || tailscale(address))) errors[`${item.key}.address`] = "Use an address this node owns in 10.0.0.0/8, 172.16.0.0/12, 192.168.0.0/16 or fc00::/7, not a Tailscale address.";
      else if (visibility === "tailscale" && !tailscale(address)) errors[`${item.key}.address`] = "Use this node's Tailscale address, in 100.64.0.0/10 or fd7a:115c:a1e0::/48.";
      if (errors[`${item.key}.address`] || errors[`${item.key}.port`]) continue;
      const reservation = { address, port, udp, label };
      const overlap = reserved.find((existing) => samePublication(existing, reservation));
      if (overlap) errors[`${item.key}.address`] = `Overlaps ${overlap.label} on ${udp ? "UDP" : "TCP"} port ${port}. A wildcard address overlaps every address of its family.`;
      else reserved.push(reservation);
    }
  }

  if (draft.publicMode === "explicit") {
    if (draft.http.length > maxBindings) errors.http = `At most ${maxBindings} HTTP bindings.`;
    if (draft.https.length > maxBindings) errors.https = `At most ${maxBindings} HTTPS bindings.`;
    checkBindings(draft.http, "public", false, "a public HTTP binding");
    checkBindings(draft.https, "public", false, "a public HTTPS binding");
  }
  const https = draft.publicMode === "explicit" ? draft.https.map(binding) : actualPublic?.https;
  const http3Ready = draft.http3 && https !== undefined && https.length > 0 && https.every((item) => item.port === https[0]?.port);
  if (draft.http3 && https?.length === 0) errors.http3 = "HTTP/3 needs at least one public HTTPS binding.";
  else if (draft.http3 && https && !http3Ready) errors.http3 = "HTTP/3 advertises one port, so every public HTTPS binding must use the same host port.";
  if (http3Ready && draft.publicMode === "explicit") checkBindings(draft.https, "public", true, "the HTTP/3 UDP binding");
  // Retained publications still occupy their ports, so listeners are checked against them too.
  if (draft.publicMode === "retain" && actualPublic) {
    const retain = (items: ListenerBinding[], udp: boolean, label: string) => {
      for (const item of items) {
        const address = parseAddress(item.address);
        if (address) reserved.push({ address, port: item.port, udp, label });
      }
    };
    retain(actualPublic.http, false, "the current public HTTP binding");
    retain(actualPublic.https, false, "the current public HTTPS binding");
    if (http3Ready) retain(actualPublic.https, true, "the HTTP/3 UDP binding");
  }

  const names = new Set<string>();
  for (const listener of draft.listeners) {
    const id = listener.id.trim();
    if (!id) errors[`${listener.key}.id`] = "Enter a name.";
    else if (!listenerName.test(id)) errors[`${listener.key}.id`] = "Use up to 32 lowercase letters, digits, dashes or underscores, starting with a letter or digit.";
    else if (names.has(id)) errors[`${listener.key}.id`] = "Another listener already uses this name.";
    names.add(id);
    if (listener.protocol === "https" && listener.visibility === "public") errors[`${listener.key}.visibility`] = "Public HTTPS uses the public bindings above. Choose Private LAN or Tailscale.";
    if (listener.bindings.length === 0) errors[`${listener.key}.bindings`] = "Add at least one binding.";
    else if (listener.bindings.length > maxBindings) errors[`${listener.key}.bindings`] = `At most ${maxBindings} bindings.`;
    checkBindings(listener.bindings, listener.visibility, listener.protocol === "udp", `listener ${id || "without a name"}`);
  }
  return errors;
}

// Public publications the draft would remove or change, so the form can warn before panel access is affected.
export function publicChange(draft: ListenerDraftSettings, actual?: PublicBindings) {
  if (draft.publicMode === "retain") return { removesAll: false, removesHttp: false, removesHttps: false, changed: false };
  const http = draft.http.map(binding);
  const https = draft.https.map(binding);
  const sorted = (items: ListenerBinding[]) => items.map(formatBinding).sort().join(",");
  return {
    removesAll: http.length === 0 && https.length === 0,
    removesHttp: http.length === 0 && (actual?.http.length ?? 0) > 0,
    removesHttps: https.length === 0 && (actual?.https.length ?? 0) > 0,
    changed: !actual || sorted(http) !== sorted(actual.http) || sorted(https) !== sorted(actual.https),
  };
}

export type ChangeTone = "info" | "warn" | "error" | "success";

export function phaseInfo(phase: string, committed: boolean): { tone: ChangeTone; label: string; active: boolean } {
  switch (phase) {
    case "validating":
      return { tone: "info", label: "Validating in an isolated proxy", active: true };
    case "applying":
      return { tone: "info", label: "Recreating the proxy", active: true };
    case "restoring":
      return { tone: "warn", label: "Restoring the previous proxy", active: true };
    case "cleanup":
      return { tone: "info", label: committed ? "Saved, cleaning up" : "Cleaning up", active: true };
    case "completed":
      return committed ? { tone: "success", label: "Applied", active: false } : { tone: "warn", label: "Finished without saving", active: false };
    case "failed":
      return { tone: "error", label: committed ? "Failed after saving" : "Failed, previous proxy kept", active: false };
    case "interrupted":
      // Also reported when a failed change was rolled back to the previous proxy.
      return { tone: "warn", label: "Not completed, interrupted or restored", active: false };
    default:
      return { tone: "warn", label: `Unknown state (${phase || "none"})`, active: false };
  }
}

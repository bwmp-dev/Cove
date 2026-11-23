import type { ForwarderKind, LogForwarderInput, UptimeCheck, UptimeCheckInput } from "../api/outputs";
import type { Tone } from "./nodes";

const encoder = new TextEncoder();
const byteLength = (value: string) => encoder.encode(value).length;
const hasControl = (value: string) => /\p{Cc}/u.test(value);

export const maxTrustedProxies = 32;

function validIPv4(value: string) {
  const parts = value.split(".");
  return parts.length === 4 && parts.every((part) => /^\d{1,3}$/.test(part) && Number(part) <= 255 && (part === "0" || !part.startsWith("0")));
}

function validIPv6(value: string) {
  if (!/^[0-9a-fA-F:.]+$/.test(value) || !value.includes(":")) return false;
  const halves = value.split("::");
  if (halves.length > 2) return false;
  const groups = halves.flatMap((half) => (half === "" ? [] : half.split(":")));
  const last = groups.at(-1);
  const embedded = last?.includes(".") ? validIPv4(last) : false;
  if (last?.includes(".") && !embedded) return false;
  const hex = embedded ? groups.slice(0, -1) : groups;
  if (!hex.every((group) => /^[0-9a-fA-F]{1,4}$/.test(group))) return false;
  const count = hex.length + (embedded ? 2 : 0);
  return halves.length === 2 ? count < 8 : count === 8;
}

// Mirrors the worker's netip.ParsePrefix check: an address with a prefix length, never an all-address range.
export function trustedProxyProblem(value: string): string | undefined {
  const [address, bits, extra] = value.split("/");
  if (address === undefined || bits === undefined || extra !== undefined || !/^\d{1,3}$/.test(bits)) return `${value} is not in CIDR notation, such as 203.0.113.0/24.`;
  const max = validIPv4(address) ? 32 : validIPv6(address) ? 128 : 0;
  if (max === 0) return `${value} is not a valid IP address range.`;
  const length = Number(bits);
  if (length === 0) return `${value} trusts every address. Enter only the ranges your own proxies or load balancers use.`;
  if (length > max) return `${value} has a prefix length above ${max}.`;
  return undefined;
}

export function parseTrustedProxies(text: string): { proxies: string[] } | { error: string } {
  const proxies = [...new Set(text.split(/[\s,]+/).map((line) => line.trim()).filter(Boolean))];
  if (proxies.length > maxTrustedProxies) return { error: `Enter at most ${maxTrustedProxies} ranges.` };
  for (const proxy of proxies) {
    const problem = trustedProxyProblem(proxy);
    if (problem) return { error: problem };
  }
  return { proxies };
}

export function sameProxies(a: readonly string[] | null | undefined, b: readonly string[] | null | undefined) {
  return (a ?? []).join(",") === (b ?? []).join(",");
}

// Rows without an identifier get content keys; identical rows are told apart by their occurrence count.
export function contentKeys<T>(items: readonly T[], keyOf: (item: T) => string) {
  const seen = new Map<string, number>();
  return items.map((item) => {
    const base = keyOf(item);
    const count = seen.get(base) ?? 0;
    seen.set(base, count + 1);
    return { item, key: `${base}#${count}` };
  });
}

export function statusClass(status: number) {
  if (status >= 500) return "text-bad";
  if (status >= 400) return "text-warn";
  return "text-fg";
}

export function formatMs(value: number) {
  if (value >= 1000) return `${(value / 1000).toFixed(value >= 10_000 ? 0 : 1)} s`;
  return `${Math.round(value)} ms`;
}

export const trafficRanges = [
  { value: "hour", label: "Last hour" },
  { value: "day", label: "Last 24 hours" },
  { value: "week", label: "Last 7 days" },
] as const;

// Readings older than two intervals plus the timeout are stale; the public page reports them as unknown too.
export function uptimeStale(check: Pick<UptimeCheck, "checked_at" | "interval_seconds" | "timeout_seconds">, now: number) {
  if (!check.checked_at) return true;
  const checked = Date.parse(check.checked_at);
  return !Number.isFinite(checked) || now - checked > (check.interval_seconds * 2 + check.timeout_seconds) * 1000;
}

export function uptimeState(check: UptimeCheck, now: number): { tone: Tone; label: string } {
  if (!check.enabled) return { tone: "neutral", label: "Disabled" };
  if (!check.checked_at) return { tone: "neutral", label: "Not checked yet" };
  if (uptimeStale(check, now)) return { tone: "warn", label: "Unknown (stale)" };
  if (check.up === true) return { tone: "ok", label: "Up" };
  if (check.up === false) return { tone: "bad", label: "Down" };
  return { tone: "neutral", label: "Unknown" };
}

export const uptimeDefaults: Omit<UptimeCheckInput, "domain"> = { path: "/", enabled: true, interval_seconds: 300, timeout_seconds: 5, expected_min: 200, expected_max: 399, public_label: "" };

export type UptimeField = "domain" | "path" | "interval_seconds" | "timeout_seconds" | "expected_min" | "expected_max" | "public_label";

export function uptimeCheckFromForm(data: FormData): UptimeCheckInput | { field: UptimeField; message: string } {
  const text = (key: string) => {
    const value = data.get(key);
    return typeof value === "string" ? value : "";
  };
  const whole = (key: string) => (/^\d+$/.test(text(key).trim()) ? Number(text(key).trim()) : Number.NaN);
  const domain = text("domain");
  if (domain === "") return { field: "domain", message: "Select a domain." };
  const path = text("path").trim();
  if (!path.startsWith("/") || path.startsWith("//") || /[?#\s]/.test(path) || hasControl(path) || byteLength(path) > 2048) return { field: "path", message: "Enter a path that starts with a single /, without spaces, a query or a fragment, at most 2048 bytes." };
  const interval = whole("interval_seconds");
  if (!(interval >= 60 && interval <= 86400)) return { field: "interval_seconds", message: "Enter a whole number of seconds from 60 to 86400." };
  const timeout = whole("timeout_seconds");
  if (!(timeout >= 1 && timeout <= 10)) return { field: "timeout_seconds", message: "Enter a whole number of seconds from 1 to 10." };
  const min = whole("expected_min");
  if (!(min >= 100 && min <= 599)) return { field: "expected_min", message: "Enter a status code from 100 to 599." };
  const max = whole("expected_max");
  if (!(max >= min && max <= 599)) return { field: "expected_max", message: `Enter a status code from ${min} to 599.` };
  const label = text("public_label").trim();
  if (byteLength(label) > 120 || hasControl(label)) return { field: "public_label", message: "Enter at most 120 bytes on one line." };
  return { domain, path, enabled: data.has("enabled"), interval_seconds: interval, timeout_seconds: timeout, expected_min: min, expected_max: max, public_label: label };
}

export function formatInterval(seconds: number) {
  if (seconds % 3600 === 0) return `${seconds / 3600} h`;
  if (seconds % 60 === 0) return `${seconds / 60} min`;
  return `${seconds} s`;
}

export function formatRatio(ratio: number) {
  const percent = ratio * 100;
  return `${percent === 100 || percent === 0 ? percent.toFixed(0) : percent.toFixed(2)}%`;
}

interface ForwarderKindSpec {
  label: string;
  endpointLabel: string;
  endpointHint: string;
  endpointPlaceholder: string;
  token: "bearer" | "api-key" | "none";
  tokenLabel: string;
  tokenHint: string;
  username: boolean;
}

const httpTokenHint = "Sent as a Bearer token, or as the Basic password when a username is set.";

export const forwarderKinds: Record<ForwarderKind, ForwarderKindSpec> = {
  loki: { label: "Grafana Loki", endpointLabel: "Push URL", endpointHint: "The full HTTPS push URL, such as https://logs.example.com/loki/api/v1/push.", endpointPlaceholder: "https://logs.example.com/loki/api/v1/push", token: "bearer", tokenLabel: "Token or password", tokenHint: httpTokenHint, username: true },
  otlp: { label: "OpenTelemetry (OTLP/HTTP JSON)", endpointLabel: "Logs URL", endpointHint: "The full HTTPS OTLP logs URL, usually ending in /v1/logs. gRPC is not supported.", endpointPlaceholder: "https://otel.example.com/v1/logs", token: "bearer", tokenLabel: "Token or password", tokenHint: httpTokenHint, username: true },
  http: { label: "Generic HTTPS", endpointLabel: "Ingestion URL", endpointHint: "Receives JSON {logs:[{workload,container,timestamp,message}]} in a POST.", endpointPlaceholder: "https://ingest.example.com/cove", token: "bearer", tokenLabel: "Token or password", tokenHint: httpTokenHint, username: true },
  axiom: { label: "Axiom", endpointLabel: "Dataset ingest URL", endpointHint: "The full dataset ingest URL, such as https://api.axiom.co/v1/datasets/<dataset>/ingest.", endpointPlaceholder: "https://api.axiom.co/v1/datasets/cove/ingest", token: "bearer", tokenLabel: "API token", tokenHint: "Sent as a Bearer token.", username: false },
  datadog: { label: "Datadog", endpointLabel: "Logs intake URL", endpointHint: "Your site's HTTPS logs intake URL, such as https://http-intake.logs.datadoghq.eu/api/v2/logs.", endpointPlaceholder: "https://http-intake.logs.datadoghq.com/api/v2/logs", token: "api-key", tokenLabel: "API key", tokenHint: "Sent in the DD-API-KEY header.", username: false },
  syslog: { label: "Syslog over TLS", endpointLabel: "Receiver address", endpointHint: "tls://hostname:port. Plain TCP and UDP are not supported, and paths or credentials are not allowed.", endpointPlaceholder: "tls://syslog.example.com:6514", token: "none", tokenLabel: "", tokenHint: "", username: false },
};

export const forwarderKindOrder: ForwarderKind[] = ["loki", "otlp", "http", "axiom", "datadog", "syslog"];

export function isForwarderKind(value: string): value is ForwarderKind {
  return value in forwarderKinds;
}

export function forwarderEndpointProblem(kind: ForwarderKind, endpoint: string): string | undefined {
  if (byteLength(endpoint) > 4096 || hasControl(endpoint)) return "Enter at most 4096 bytes on one line.";
  let url: URL;
  try {
    url = new URL(endpoint);
  } catch {
    return kind === "syslog" ? "Enter tls://hostname:port." : "Enter a full https:// URL.";
  }
  if (kind === "syslog") {
    const port = Number(url.port);
    if (url.protocol !== "tls:" || url.hostname === "" || !(port >= 1 && port <= 65535) || url.username || url.password || (url.pathname !== "" && url.pathname !== "/") || url.search || url.hash) return "Enter tls://hostname:port with a port from 1 to 65535 and nothing else.";
    return undefined;
  }
  if (url.protocol !== "https:" || url.hostname === "") return "Enter a full https:// URL.";
  if (url.username || url.password) return "Remove the credentials from the URL and use the token field instead.";
  if (endpoint.includes("#")) return "Remove the fragment (#…) from the URL.";
  return undefined;
}

export type ForwarderField = "name" | "endpoint" | "token" | "username";

const multiLine = (value: string, limit: number) => byteLength(value) > limit || /[\r\n]/.test(value);

// saved says which values the API keeps when their field is left blank; it only applies while the kind is unchanged.
export function forwarderFromForm(data: FormData, kind: ForwarderKind, saved: { endpoint: boolean; token: boolean }): LogForwarderInput | { field: ForwarderField; message: string } {
  const spec = forwarderKinds[kind];
  const text = (key: string) => {
    const value = data.get(key);
    return typeof value === "string" ? value.trim() : "";
  };
  const name = text("name");
  if (name === "" || byteLength(name) > 120) return { field: "name", message: "Enter a name of at most 120 bytes." };
  const endpoint = text("endpoint");
  if (endpoint === "" && !saved.endpoint) return { field: "endpoint", message: `Enter the ${spec.endpointLabel.toLowerCase()}.` };
  const endpointProblem = endpoint === "" ? undefined : forwarderEndpointProblem(kind, endpoint);
  if (endpointProblem) return { field: "endpoint", message: endpointProblem };
  const token = spec.token === "none" ? "" : text("token");
  if (multiLine(token, 4096)) return { field: "token", message: "Enter at most 4096 bytes on one line." };
  if (spec.token === "api-key" && token === "" && !saved.token) return { field: "token", message: "Enter the API key." };
  const username = spec.username ? text("username") : "";
  if (multiLine(username, 256)) return { field: "username", message: "Enter at most 256 bytes on one line." };
  return { name, kind, endpoint, token, username, enabled: data.has("enabled") };
}

import type { DatabaseEngine, DatabaseVersion } from "../api/databases";
import type { DatabaseEngineId, DatabaseResource } from "../api/types";

const engineNames: Record<DatabaseEngineId, string> = {
  postgresql: "PostgreSQL",
  mysql: "MySQL",
  mariadb: "MariaDB",
  mongodb: "MongoDB",
  redis: "Redis",
  valkey: "Valkey",
  clickhouse: "ClickHouse",
};

export function engineName(engine: string) {
  return engineNames[engine as DatabaseEngineId] ?? engine;
}

// Client-facing details that differ between engines. Each sentence mirrors how the managed template is configured.
export function engineNotes(engine: string): string | null {
  switch (engine) {
    case "mongodb":
      return "Users authenticate against the admin database (authSource=admin). The application database app is created on its first write.";
    case "redis":
    case "valkey":
      return "Clients use the ACL user default and database 0.";
    case "clickhouse":
      return "DB_URL uses the native protocol on port 9000. A private connection may also target the HTTP interface on port 8123.";
    case "mysql":
    case "mariadb":
      return "Rotation changes the application account. The separate root credential is retained and not exported.";
    case "postgresql":
      return "The exported account is the initial administrative account. Create least-privilege roles for applications that need narrower access.";
    default:
      return null;
  }
}

export const exportLabels: Record<string, string> = {
  DB_HOST: "Private hostname",
  DB_PORT: "Native port",
  DB_USER: "User",
  DB_PASSWORD: "Password",
  DB_NAME: "Database",
  DB_URL: "Connection URL",
  DB_POOL_HOST: "Pooler hostname",
  DB_POOL_PORT: "Pooler port",
  DB_POOL_USER: "Pooler user",
  DB_POOL_PASSWORD: "Pooler password",
  DB_POOL_NAME: "Pooler database",
  DB_POOL_URL: "Pooler connection URL",
  DB_PUBLIC_HOST: "Public TLS hostname",
  DB_PUBLIC_PORT: "Public TLS port",
  DB_PUBLIC_URL: "Public TLS connection URL",
  DB_TLS_CA: "TLS CA certificate (public)",
};

export function isDatabaseExport(key: string) {
  return Object.hasOwn(exportLabels, key);
}

export interface DatabaseEndpoint {
  key: "native" | "http" | "pooler";
  label: string;
  service: string;
  port: number;
  alias: string;
}

export const poolerService = "pgbouncer";
export const poolerPort = 6432;

// Targets a private connection may use for this database. The control plane rejects anything else.
export function databaseEndpoints(database: DatabaseResource): DatabaseEndpoint[] {
  const endpoints: DatabaseEndpoint[] = [{ key: "native", label: `Native protocol (${database.port})`, service: database.service, port: database.port, alias: database.host }];
  if (database.engine === "clickhouse") endpoints.push({ key: "http", label: "HTTP interface (8123)", service: database.service, port: 8123, alias: database.host });
  if (database.pooler && database.pooler_host) endpoints.push({ key: "pooler", label: `PgBouncer pooler (${poolerPort})`, service: poolerService, port: poolerPort, alias: database.pooler_host });
  return endpoints;
}

export interface AdminTool {
  id: string;
  name: string;
  port: number;
  localPort: number;
}

// Container ports are fixed by the worker's helper definitions; local ports are only a suggestion for the CLI command.
const adminTools: Record<string, AdminTool> = {
  adminer: { id: "adminer", name: "Adminer", port: 8080, localPort: 18080 },
  "mongo-express": { id: "mongo-express", name: "Mongo Express", port: 8081, localPort: 18081 },
  redisinsight: { id: "redisinsight", name: "Redis Insight", port: 5540, localPort: 15540 },
};

export function adminTool(id: string | undefined): AdminTool | undefined {
  return id ? adminTools[id] : undefined;
}

export const extensionNotes: Record<string, string> = {
  pg_stat_statements: "Adds the library to shared_preload_libraries and restarts PostgreSQL. Clients are disconnected while it restarts.",
  vector: "Needs the pgvector image variant. On another image, create a copy with the vector target instead.",
  postgis: "Needs the PostGIS image variant. Disabling also removes postgis_topology, postgis_tiger_geocoder and fuzzystrmatch in the same transaction.",
};

// Enabling these on a different image is refused by the control plane; a copy to the matching variant is the path.
export function extensionNeedsVariant(extension: string, variant: string | undefined) {
  if (extension === "vector") return variant !== "vector";
  if (extension === "postgis") return variant !== "postgis";
  return false;
}

// "unknown" means the recorded settings operation no longer exists, so whether the saved settings applied is not known.
export type SettingsState = "queued" | "running" | "succeeded" | "failed" | "interrupted" | "cancelled" | "unknown";

export function settingsSettled(state: SettingsState | undefined) {
  return state === undefined || state === "succeeded";
}

export function settingsFailed(state: SettingsState | undefined) {
  return state === "failed" || state === "interrupted" || state === "cancelled";
}

export interface PublicAccessDraft {
  hostname: string;
  bind_address: string;
  port: string;
  certificate: string;
  private_key: string;
  ca_certificate: string;
}

export type PublicAccessErrors = Partial<Record<keyof PublicAccessDraft, string>>;

const dnsLabel = /^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$/;
const ipv4 = /^(25[0-5]|2[0-4]\d|1\d\d|[1-9]?\d)(\.(25[0-5]|2[0-4]\d|1\d\d|[1-9]?\d)){3}$/;

function validHostname(value: string) {
  if (value.length > 253 || ipv4.test(value) || value.includes(":") || value === "localhost" || value.startsWith("cove-")) return false;
  const labels = value.split(".");
  return labels.every((label) => dnsLabel.test(label)) && !/^\d+$/.test(labels.at(-1) ?? "");
}

function validBindAddress(value: string) {
  if (ipv4.test(value)) return !value.startsWith("169.254.") && Number(value.split(".")[0]) < 224;
  if (!/^[0-9a-f:.]+$/i.test(value) || !value.includes(":")) return false;
  try {
    // The URL parser normalises valid IPv6 literals and rejects malformed ones.
    new URL(`http://[${value}]/`);
    return !/^fe[89ab]/i.test(value) && !/^ff/i.test(value);
  } catch {
    return false;
  }
}

const pemCertificate = /-----BEGIN CERTIFICATE-----[\s\S]+?-----END CERTIFICATE-----/;
const pemKey = /-----BEGIN ([A-Z ]+ )?PRIVATE KEY-----[\s\S]+?-----END ([A-Z ]+ )?PRIVATE KEY-----/;
const bytes = (value: string) => new TextEncoder().encode(value).length;

// Format checks only. The control plane verifies the key pair, hostname, validity window and chain.
export function validatePublicAccess(draft: PublicAccessDraft): PublicAccessErrors {
  const errors: PublicAccessErrors = {};
  const hostname = draft.hostname.trim().toLowerCase();
  if (!hostname) errors.hostname = "Enter the DNS name clients connect to.";
  else if (!validHostname(hostname)) errors.hostname = "Use a DNS name such as db.example.com, not an IP address. Names starting with cove- are reserved.";
  const bind = draft.bind_address.trim();
  if (!bind) errors.bind_address = "Enter the worker address to listen on.";
  else if (!validBindAddress(bind)) errors.bind_address = "Use an IPv4 or IPv6 address of the worker, or 0.0.0.0 or ::. Multicast and link-local addresses are not accepted.";
  const port = Number(draft.port);
  if (!/^\d+$/.test(draft.port.trim()) || port < 1024 || port > 65535) errors.port = "Use a port between 1024 and 65535.";
  if (!draft.certificate.trim()) errors.certificate = "Paste the PEM server certificate.";
  else if (!pemCertificate.test(draft.certificate)) errors.certificate = "Paste a PEM certificate beginning with -----BEGIN CERTIFICATE-----.";
  else if (bytes(draft.certificate) > 32768) errors.certificate = "The certificate must be at most 32 KiB.";
  if (!draft.private_key.trim()) errors.private_key = "Paste the PEM private key for the certificate.";
  else if (!pemKey.test(draft.private_key)) errors.private_key = "Paste a PEM private key.";
  else if (bytes(draft.private_key) > 16384) errors.private_key = "The private key must be at most 16 KiB.";
  if (draft.ca_certificate.trim()) {
    if (!pemCertificate.test(draft.ca_certificate)) errors.ca_certificate = "Paste PEM CA certificates, including the root.";
    else if (bytes(draft.ca_certificate) > 65536) errors.ca_certificate = "The CA bundle must be at most 64 KiB.";
  }
  return errors;
}

// Password inputs drop line breaks on paste; PEM decoders need them, so rebuild the armour and 64-column body.
export function normalisePem(value: string) {
  const match = /^\s*(-----BEGIN [A-Z0-9 ]+-----)\s*([A-Za-z0-9+/=\s]+?)\s*(-----END [A-Z0-9 ]+-----)\s*$/.exec(value);
  if (!match) return value.trim();
  const [, begin = "", armoured = "", end = ""] = match;
  const body = armoured.replace(/\s+/g, "");
  return `${begin}\n${body.match(/.{1,64}/g)?.join("\n") ?? ""}\n${end}\n`;
}

export function engineDefaultPublicPort(engine: string, port: number) {
  if (engine === "redis" || engine === "valkey") return 6380;
  if (engine === "clickhouse") return 9440;
  return port;
}

export const variantNames: Record<string, string> = { vector: "pgvector", postgis: "PostGIS" };

export function versionLabel(engine: string, target: Pick<DatabaseVersion, "version" | "variant">) {
  const variant = variantNames[target.variant];
  return `${engineName(engine)} ${target.version}${variant ? ` (${variant})` : ""}`;
}

// The current default first, then the catalogue's other pinned versions with their own images and warnings.
export function versionOptions(engine: DatabaseEngine): DatabaseVersion[] {
  const details = engine.version_details ?? [];
  const others = engine.versions
    .filter((value) => value !== engine.version)
    .map((value) => details.find((item) => item.version === value) ?? { version: value, image: engine.image.replace(engine.version, value), variant: "" });
  return [{ version: engine.version, image: engine.image, variant: "" }, ...others];
}

// Engines the worker can inspect for copy compatibility; key-value and analytical engines are not inspected.
export const inspectableEngines = new Set(["postgresql", "mysql", "mariadb", "mongodb"]);

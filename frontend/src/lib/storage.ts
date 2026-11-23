import type { Storage } from "../api/connections";

export const storageProtocols = [
  { value: "s3", label: "S3" },
  { value: "b2", label: "Backblaze B2" },
  { value: "sftp", label: "SFTP" },
  { value: "ftps", label: "FTPS" },
  { value: "webdav", label: "WebDAV" },
] as const;

export function storageProtocolLabel(protocol: string) {
  return storageProtocols.find((item) => item.value === protocol)?.label ?? protocol.toUpperCase();
}

export function storageSummary(storage: Storage) {
  return [storageProtocolLabel(storage.protocol), storage.endpoint, storage.path].filter(Boolean).join(" · ");
}

// Mirrors the worker's B2 path rules so people see the problem beside the field instead of a 400.
export function b2PathError(path: string) {
  if (path === "") return "Enter the bucket name, optionally followed by a path.";
  if (path.startsWith("/")) return "Start with the bucket name, without a leading slash.";
  if (path.includes("..")) return "Remove “..” from the path.";
  if (/[\\:\r\n]/.test(path)) return "Remove backslashes, colons and line breaks from the path.";
  return undefined;
}

// B2 uses Backblaze's native API, so it never carries an endpoint, CA or fields from other protocols.
export function storageFromForm(protocol: string, text: (key: string) => string): Storage {
  if (protocol === "b2") return { protocol, endpoint: "", path: text("path").trim(), access_key: text("access_key").trim(), secret_key: text("secret_key") };
  return { ca_certificate: text("ca_certificate"), protocol, endpoint: text("endpoint").trim(), path: text("path").trim(), username: text("username"), password: text("password"), access_key: text("access_key"), secret_key: text("secret_key"), region: text("region"), host_key: text("host_key").trim(), tls_mode: text("tls_mode") };
}

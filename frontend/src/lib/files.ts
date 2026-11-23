import type { FileEntry, FileTarget } from "../api/files";

export const maxFileBytes = 32768;
export const maxEntries = 200;
const maxPathBytes = 1024;

// The control plane refuses these container paths; showing them as folders would only lead to an error.
const blockedContainerPaths = ["/proc", "/sys", "/dev", "/run/secrets"];

export const containerTools = "sh, timeout, stat, base64, head, sha256sum, mktemp, cat, truncate, ln, tr, rm and rmdir";

// SHA-256 of zero bytes: the revision of a file Cove has just created empty.
export const emptyRevision = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855";

const encoder = new TextEncoder();

// Matches Go's unicode.IsControl (C0, DEL and C1), which the worker uses to refuse paths and hide listed names.
function isControlCharacter(character: string) {
  const code = character.charCodeAt(0);
  return code < 0x20 || (code >= 0x7f && code <= 0x9f);
}

export function utf8Length(text: string) {
  return encoder.encode(text).length;
}

export function joinPath(directory: string, name: string) {
  return directory === "/" ? `/${name}` : `${directory}/${name}`;
}

export function parentPath(path: string) {
  const index = path.lastIndexOf("/");
  return index <= 0 ? "/" : path.slice(0, index);
}

export function baseName(path: string) {
  return path.slice(path.lastIndexOf("/") + 1);
}

export function pathSegments(path: string): { name: string; path: string }[] {
  if (path === "/") return [];
  const parts = path.slice(1).split("/");
  return parts.map((name, index) => ({ name, path: `/${parts.slice(0, index + 1).join("/")}` }));
}

export function isBlockedPath(kind: FileTarget["kind"], path: string) {
  return kind === "container" && blockedContainerPaths.some((blocked) => path === blocked || path.startsWith(`${blocked}/`));
}

// Mirrors the server's path rules for a single new name, so people see the problem before a request is sent.
export function fileNameError(name: string, directory: string, kind: FileTarget["kind"]): string | undefined {
  if (name === "") return "Enter a file name.";
  if (name === "." || name === "..") return "Choose a name other than . or ..";
  if (name.includes("/")) return "Enter a name without slashes. Open the folder first to create a file inside it.";
  if ([...name].some((character) => character === "\\" || isControlCharacter(character))) return "Remove backslashes, tabs, line breaks and other control characters.";
  const path = joinPath(directory, name);
  if (utf8Length(path) > maxPathBytes) return `The full path must be at most ${maxPathBytes} bytes.`;
  if (isBlockedPath(kind, path)) return "Special filesystems and secret mounts cannot be changed.";
  return undefined;
}

export function sortEntries(entries: FileEntry[]) {
  const order: Record<FileEntry["kind"], number> = { directory: 0, file: 1, link: 2, other: 3 };
  return [...entries].sort((a, b) => order[a.kind] - order[b.kind] || a.name.localeCompare(b.name));
}

export function decodeBase64(data: string | null): Uint8Array<ArrayBuffer> {
  if (!data) return new Uint8Array();
  return Uint8Array.from(atob(data), (character) => character.charCodeAt(0));
}

export function encodeBase64(bytes: Uint8Array) {
  let binary = "";
  for (let start = 0; start < bytes.length; start += 8192) {
    binary += String.fromCharCode(...bytes.subarray(start, start + 8192));
  }
  return btoa(binary);
}

export function encodeText(text: string) {
  return encodeBase64(encoder.encode(text));
}

// Returns null for bytes that are not valid UTF-8 or contain NUL, which are downloaded instead of edited as text.
export function decodeText(bytes: Uint8Array): string | null {
  if (bytes.includes(0)) return null;
  try {
    return new TextDecoder("utf-8", { fatal: true, ignoreBOM: true }).decode(bytes);
  } catch {
    return null;
  }
}

export async function revisionOf(bytes: Uint8Array<ArrayBuffer>) {
  const digest = new Uint8Array(await crypto.subtle.digest("SHA-256", bytes));
  return Array.from(digest, (byte) => byte.toString(16).padStart(2, "0")).join("");
}

export function targetKey(target: Pick<FileTarget, "kind" | "id">) {
  return `${target.kind}:${target.id}`;
}

export function targetAvailable(target: FileTarget) {
  return target.kind === "volume" || target.state === "running";
}

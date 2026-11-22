// The control plane can serve the dashboard under a path prefix (COVE_PUBLIC_URL with a path). One build serves
// any prefix: the server injects <base href> for assets and this meta element for code that builds addresses.
// Router paths stay logical (unprefixed); only addresses handed to the browser or fetch carry the prefix.
// The prefix may itself look like an application path (/api, /projects), so logical paths are always prefixed and
// only real browser addresses are ever stripped.
const segmentPattern = /^[A-Za-z0-9_-]+$/;

export function parseBasePath(value: string | null | undefined): string {
  if (!value || value === "/") return "";
  if (!value.startsWith("/") || value.endsWith("/")) return "";
  return value.slice(1).split("/").every((segment) => segmentPattern.test(segment)) ? value : "";
}

export const basePath = parseBasePath(typeof document === "undefined" ? null : document.querySelector('meta[name="cove-base-path"]')?.getAttribute("content"));

// Browsers drop tabs and newlines from URLs, so "/<tab>/evil.example" would become scheme-relative after this check.
function hasControlCharacter(path: string) {
  return [...path].some((character) => character <= "\u001f" || character === "\u007f");
}

export function isApplicationPath(path: string) {
  return path.startsWith("/") && !path.startsWith("//") && !path.includes("\\") && !hasControlCharacter(path);
}

function hasBasePath(path: string, base: string) {
  return path === base || path.startsWith(`${base}/`) || path.startsWith(`${base}?`) || path.startsWith(`${base}#`);
}

// Takes a logical path. TanStack's navigate({ href }) and redirect({ href }) also expect this browser address: the
// router strips one leading prefix from them, which would turn a logical /projects under the /projects prefix into /.
export function withBasePath(path: string, base = basePath): string {
  if (!isApplicationPath(path)) throw new Error("Only same-origin application paths can be addressed.");
  return `${base}${path}`;
}

// Takes an address the browser reports, such as window.location.pathname.

export function stripBasePath(path: string, base = basePath): string {
  if (!base || !hasBasePath(path, base)) return path;
  const rest = path.slice(base.length);
  return rest === "" || !rest.startsWith("/") ? `/${rest}` : rest;
}

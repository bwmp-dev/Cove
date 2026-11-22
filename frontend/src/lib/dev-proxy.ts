import { parseBasePath } from "./base-path";

export interface DevPublicURL {
  origin: string;
  basePath: string;
}

// COVE_DEV_PUBLIC_URL mirrors the control plane's COVE_PUBLIC_URL. The backend compares Origin with the scheme and
// host only, and serves /api and /auth under the URL's path, so the dev proxy needs the two parts separately.
export function parseDevPublicURL(value: string | undefined): DevPublicURL | null {
  if (!value) return null;
  if (!URL.canParse(value)) throw new Error(`COVE_DEV_PUBLIC_URL is not a valid URL: ${value}`);
  const url = new URL(value);
  const path = url.pathname.replace(/\/$/, "");
  const basePath = parseBasePath(path);
  if (basePath !== path || url.search || url.hash || url.username || url.password) {
    throw new Error(`COVE_DEV_PUBLIC_URL must be the control plane's COVE_PUBLIC_URL with an optional path of letters, numbers, underscores and hyphens: ${value}`);
  }
  return { origin: url.origin, basePath };
}

export function devProxyPath(path: string, target: DevPublicURL | null): string {
  return target ? `${target.basePath}${path}` : path;
}

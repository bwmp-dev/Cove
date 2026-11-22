// One-time tokens arrive in the URL fragment so they never reach server logs. They are moved into memory
// and removed from the address bar before the application renders.
const tokenPaths = new Set(["/verify", "/reset-password", "/invite"]);

let captured: { path: string; token: string } | null = null;

export function captureFragmentToken(location: Pick<Location, "pathname" | "search" | "hash">, history: Pick<History, "replaceState" | "state">) {
  if (!tokenPaths.has(location.pathname) || location.hash.length <= 1) return;
  const fragment = location.hash.slice(1);
  const token = fragment.includes("=") ? new URLSearchParams(fragment).get("token") : fragment;
  history.replaceState(history.state, "", location.pathname + location.search);
  if (token) captured = { path: location.pathname, token };
}

export function fragmentToken(path: string) {
  return captured?.path === path ? captured.token : null;
}

export function forgetFragmentToken() {
  captured = null;
}

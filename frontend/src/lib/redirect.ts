import { isApplicationPath } from "./base-path";

// Only same-origin application paths are accepted as post-login destinations. The router records them as logical
// paths (without the panel prefix), so they are returned unchanged and callers add the prefix with withBasePath.
export function safeRedirect(value: string | undefined): string {
  if (!value || !isApplicationPath(value)) {
    return "/dashboard";
  }
  try {
    const url = new URL(value, "https://cove.invalid");
    if (url.origin !== "https://cove.invalid" || url.pathname.startsWith("//") || url.pathname.startsWith("/login")) {
      return "/dashboard";
    }
    return url.pathname + url.search + url.hash;
  } catch {
    return "/dashboard";
  }
}

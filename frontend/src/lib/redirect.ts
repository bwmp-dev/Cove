// Only same-origin application paths are accepted as post-login destinations.
export function safeRedirect(value: string | undefined): string {
  if (!value || !value.startsWith("/") || value.startsWith("//") || value.includes("\\")) {
    return "/dashboard";
  }
  try {
    const url = new URL(value, "https://cove.invalid");
    if (url.origin !== "https://cove.invalid" || url.pathname.startsWith("/login")) {
      return "/dashboard";
    }
    return url.pathname + url.search + url.hash;
  } catch {
    return "/dashboard";
  }
}

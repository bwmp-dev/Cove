export const maxSearchBytes = 200;

export function searchBytes(text: string) {
  return new TextEncoder().encode(text).length;
}

const pages = new Set(["/dashboard", "/projects", "/resources", "/operations", "/nodes", "/integrations", "/registries", "/storage", "/team", "/tokens", "/account", "/notifications", "/audit", "/admin", "/terminal", "/users", "/api-docs"]);
const segment = "[A-Za-z0-9_-]{1,128}";
const entityPaths = [
  new RegExp(`^/projects/${segment}$`),
  new RegExp(`^/projects/${segment}/environments/${segment}$`),
  new RegExp(`^/resources/${segment}$`),
  new RegExp(`^/resources/${segment}/(logs|metrics|troubleshooting|backups|files|database|object-storage)$`),
  new RegExp(`^/nodes/${segment}$`),
  new RegExp(`^/operations/${segment}$`),
];

// Search results carry server-built paths. Only known in-app destinations are followed, so a result
// can never become an external redirect or reach a route the palette does not expect.
export function isSearchPath(path: string) {
  return pages.has(path) || entityPaths.some((pattern) => pattern.test(path));
}

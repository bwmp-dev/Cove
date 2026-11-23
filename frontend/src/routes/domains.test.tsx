import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { ReactNode } from "react";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { request } from "../api/client";
import type { Node, Route, Workload } from "../api/types";
import { ResourceDomains } from "./resource-config";

let resource: Workload;

vi.mock("@tanstack/react-router", () => ({ Link: ({ children }: { children: ReactNode }) => <a href="/">{children}</a>, useBlocker: () => ({ status: "idle" }) }));
vi.mock("../api/client", async (importOriginal) => ({ ...(await importOriginal<typeof import("../api/client")>()), request: vi.fn() }));
vi.mock("./workload-detail", () => ({ useResource: () => resource }));
beforeEach(() => vi.stubGlobal("ResizeObserver", class { observe = vi.fn(); disconnect = vi.fn(); }));
afterEach(() => {
  vi.clearAllMocks();
  vi.unstubAllGlobals();
});

// Test fixtures below are API-shaped responses for a single resource; they are not product data.
const saved: Route = {
  domain: "app.example.com",
  service: "web",
  port: 8080,
  policy: {
    maintenance: { enabled: false, html: "<h1>Back soon</h1>" },
    error_pages: { "502": "<h1>Unavailable</h1>" },
    forward_auth: { service: "auth", port: 9091, response_headers: ["Remote-User"] },
  },
};

const workload = {
  id: "w1",
  name: "web",
  node_id: "n1",
  kind: "service",
  compose: "services:\n  web:\n    image: nginx\n",
  source: null,
  environment_keys: [],
  has_source_token: false,
  permissions: ["read", "operate", "edit"],
  manually_stopped: false,
  deployed: true,
  project_id: "p1",
  guarded_deploy: false,
  routes: [saved],
} as Workload;

function node(routeProtocol?: number): Node {
  return { id: "n1", name: "worker-1", address: "", port: 22, state: "ready", mode: "ssh", last_checked_at: null, capabilities: { protocol: 1, os: "linux", arch: "amd64", docker: "27", compose: "2", ready: true, route_policies: true, ...(routeProtocol === undefined ? {} : { route_protocol: routeProtocol }) } };
}

function renderPage(nodes: Node[]) {
  resource = workload;
  vi.mocked(request).mockImplementation((method: string, path: string) => {
    if (path === "/api/nodes") return Promise.resolve(nodes);
    if (path.startsWith("/api/operations")) return Promise.resolve([]);
    if (method === "PUT" && path === "/api/workloads/w1/routes") return Promise.resolve({ state: "unchanged" });
    return Promise.reject(new Error(`unexpected ${method} ${path}`));
  });
  render(
    <QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}>
      <ResourceDomains />
    </QueryClientProvider>,
  );
}

const puts = () => vi.mocked(request).mock.calls.filter(([method, path]) => method === "PUT" && path === "/api/workloads/w1/routes");

it("saves an unrelated change with every saved route extension unchanged", async () => {
  renderPage([node(2)]);
  await userEvent.click(await screen.findByRole("button", { name: /Advanced routing/ }));
  await screen.findByRole("group", { name: "Forward-auth" });
  await userEvent.click(screen.getByRole("switch", { name: "Sticky sessions" }));
  await userEvent.click(screen.getByRole("button", { name: "Save and apply" }));
  await waitFor(() => expect(puts()).toHaveLength(1));
  expect(puts()[0]?.[2]).toEqual({ routes: [{ ...saved, policy: { ...saved.policy, sticky: true } }] });
});

it("lets an older worker's route drop its saved extensions with explicit nulls", async () => {
  renderPage([node(1)]);
  expect(await screen.findByText("Maintenance, error pages and forward-auth need a newer worker")).toBeInTheDocument();
  await userEvent.click(screen.getByRole("button", { name: /Advanced routing/ }));
  await userEvent.click(await screen.findByRole("button", { name: "Remove forward-auth" }));
  await userEvent.click(screen.getByRole("button", { name: "Remove maintenance settings" }));
  await userEvent.click(screen.getByRole("button", { name: "Remove error page 502" }));
  await userEvent.click(screen.getByRole("button", { name: "Save and apply" }));
  await waitFor(() => expect(puts()).toHaveLength(1));
  expect(puts()[0]?.[2]).toEqual({ routes: [{ domain: "app.example.com", service: "web", port: 8080, policy: { maintenance: null, error_pages: null, forward_auth: null } }] });
});

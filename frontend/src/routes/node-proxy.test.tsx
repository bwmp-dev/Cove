import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { request } from "../api/client";
import type { ListenerStatus } from "../api/ingress-listeners";
import type { ProxyConfig } from "../api/proxy";
import type { Node } from "../api/types";
import { NodeProxyPage } from "./node-proxy";

vi.mock("../api/client", async (original) => ({ ...(await original<typeof import("../api/client")>()), request: vi.fn() }));
vi.mock("../lib/session", () => ({ useSession: () => ({ is_admin: true }) }));
vi.mock("@tanstack/react-router", () => ({
  getRouteApi: () => ({ useParams: () => ({ nodeId: "n1" }) }),
  useBlocker: () => ({ status: "idle" }),
  Link: ({ children }: { children: React.ReactNode }) => <span>{children}</span>,
}));
beforeEach(() => vi.stubGlobal("ResizeObserver", class { observe = vi.fn(); disconnect = vi.fn(); }));
afterEach(() => {
  vi.clearAllMocks();
  vi.unstubAllGlobals();
});

const node: Node = { id: "n1", name: "worker-1", address: "", port: 22, state: "ready", mode: "ssh", last_checked_at: null, capabilities: { protocol: 1, os: "linux", arch: "amd64", docker: "27", compose: "2", ready: true, ingress_exposures: true } };
const listenerStatus: ListenerStatus = { settings: { http3: false, listeners: [] }, ports: null, proxy_present: true, running: true, public_bindings: { http: [], https: [] } };
const config: ProxyConfig = { available: true, running: true, ports: [], entrypoints: [], resolvers: [], files: [] };

it("keeps an unsaved listener draft when the proxy configuration finishes loading after it", async () => {
  let loadConfig: (value: ProxyConfig) => void = () => undefined;
  vi.mocked(request).mockImplementation((_method: string, path: string) => {
    if (path === "/api/nodes") return Promise.resolve([node]);
    if (path === "/api/nodes/n1/ingress-listeners") return Promise.resolve(listenerStatus);
    if (path === "/api/nodes/n1/ingress-config") return new Promise((resolve) => (loadConfig = resolve));
    if (path === "/api/workloads") return Promise.resolve([]);
    return Promise.reject(new Error(`Unexpected request ${path}`));
  });
  render(
    <QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}>
      <NodeProxyPage />
    </QueryClientProvider>,
  );

  fireEvent.click(await screen.findByRole("button", { name: "Add listener" }));
  fireEvent.change(screen.getByLabelText("Name"), { target: { value: "local-db" } });
  expect(screen.getByRole("status", { name: "Loading proxy configuration" })).toBeInTheDocument();

  loadConfig(config);
  expect(await screen.findByRole("heading", { name: "Dynamic configuration files" })).toBeInTheDocument();
  expect(screen.getByLabelText("Name")).toHaveValue("local-db");
  expect(screen.getByText("Unsaved changes. Review a plan to apply them.")).toBeInTheDocument();
});

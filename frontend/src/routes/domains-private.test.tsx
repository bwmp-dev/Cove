import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { ReactNode } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { ApiError, request } from "../api/client";
import { routeListenersKey, type Listener } from "../api/ingress-listeners";
import type { Node, Route, Workload } from "../api/types";
import { ResourceDomains } from "./resource-config";

let resource: Workload;

vi.mock("@tanstack/react-router", () => ({ Link: ({ children }: { children: ReactNode }) => <a href="/">{children}</a>, useBlocker: () => ({ status: "idle" }), useRouter: () => undefined }));
vi.mock("../api/client", async (importOriginal) => ({ ...(await importOriginal<typeof import("../api/client")>()), request: vi.fn() }));
vi.mock("./workload-detail", () => ({ useResource: () => resource }));
beforeEach(() => vi.stubGlobal("ResizeObserver", class { observe = vi.fn(); disconnect = vi.fn(); }));
afterEach(() => {
  vi.clearAllMocks();
  vi.unstubAllGlobals();
});

// API-shaped fixtures for one resource and node; not product data.
const privateRoute: Route = { domain: "app.example.com", service: "web", port: 8080, policy: { visibility: "lan", listener: "office", certificate_source: "default" } };
const publicRoute: Route = { domain: "app.example.com", service: "web", port: 8080 };
const listeners: Listener[] = [
  { id: "office", protocol: "https", visibility: "lan", bindings: [{ address: "192.168.10.2", port: 8443 }] },
  { id: "lab", protocol: "https", visibility: "lan", bindings: [{ address: "192.168.20.2", port: 9443 }] },
  { id: "tailnet", protocol: "https", visibility: "tailscale", bindings: [{ address: "100.100.1.2", port: 8443 }] },
];

function node(routeProtocol: number): Node {
  return { id: "n1", name: "worker-1", address: "", port: 22, state: "ready", mode: "ssh", last_checked_at: null, capabilities: { protocol: 1, os: "linux", arch: "amd64", docker: "27", compose: "2", ready: true, route_policies: true, route_protocol: routeProtocol } };
}

function renderPage({ routes, routeProtocol = 5, listed = () => Promise.resolve({ listeners }), permissions = ["read", "operate", "edit"] }: { routes: Route[]; routeProtocol?: number; listed?: () => Promise<unknown>; permissions?: string[] }) {
  resource = { id: "w1", name: "web", node_id: "n1", kind: "service", compose: "services:\n  web:\n    image: nginx\n", source: null, environment_keys: [], has_source_token: false, permissions, manually_stopped: false, deployed: true, project_id: "p1", guarded_deploy: false, routes } as Workload;
  vi.mocked(request).mockImplementation((method: string, path: string) => {
    if (path === "/api/nodes") return Promise.resolve([node(routeProtocol)]);
    if (path.startsWith("/api/operations")) return Promise.resolve([]);
    if (path === "/api/workloads/w1/route-listeners") return listed();
    if (path === "/api/workloads/w1/uptime") return Promise.resolve([]);
    if (path === "/api/workloads/w1/certificates" || path === "/api/workloads/w1/certificate-issuers" || path === "/api/workloads/w1/certificate-zones") return Promise.resolve([]);
    if (path.startsWith("/api/workloads/w1/certificate-challenges")) return Promise.resolve({ items: [], more: false });
    if (method === "PUT" && path === "/api/workloads/w1/routes") return Promise.resolve({ state: "unchanged" });
    return Promise.reject(new Error(`unexpected ${method} ${path}`));
  });
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(
    <QueryClientProvider client={client}>
      <ResourceDomains />
    </QueryClientProvider>,
  );
  return client;
}

const puts = () => vi.mocked(request).mock.calls.filter(([method, path]) => method === "PUT" && path === "/api/workloads/w1/routes");
const listenerCalls = () => vi.mocked(request).mock.calls.filter(([, path]) => path === "/api/workloads/w1/route-listeners");

async function chooseOption(combobox: HTMLElement, name: string | RegExp) {
  await userEvent.click(combobox);
  await userEvent.click(await screen.findByRole("option", { name }));
}

async function save() {
  await userEvent.click(screen.getByRole("button", { name: "Save and apply" }));
  await waitFor(() => expect(puts()).toHaveLength(1));
  return puts()[0]?.[2];
}

describe("private routes on Domains", () => {
  it("shows the saved listener and keeps privacy on an unrelated edit", async () => {
    renderPage({ routes: [privateRoute] });
    expect(await screen.findByText(/LAN only via office \(192\.168\.10\.2:8443\)/)).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: /Advanced routing/ }));
    expect(screen.getByRole("combobox", { name: "Visibility" })).toHaveTextContent("LAN only");
    expect(screen.getByRole("combobox", { name: "Listener" })).toHaveTextContent("office · 192.168.10.2:8443");
    await userEvent.click(screen.getByRole("switch", { name: "Sticky sessions" }));
    expect(await save()).toEqual({ routes: [{ ...privateRoute, policy: { ...privateRoute.policy, sticky: true } }] });
  });

  it("switches a private hostname to Public with an explicit public visibility and null listener", async () => {
    renderPage({ routes: [privateRoute] });
    await userEvent.click(await screen.findByRole("button", { name: /Advanced routing/ }));
    const exposure = /A guessed private hostname can reveal certificate names on public HTTPS; HTTP access remains denied\. Use wildcard or non-identifying certificate names for sensitive hosts, and a private CA to avoid public certificate logs\./;
    expect(screen.getByRole("combobox", { name: "Certificate" })).toHaveAccessibleDescription(exposure);
    await chooseOption(screen.getByRole("combobox", { name: "Visibility" }), "Public");
    expect(screen.queryByRole("combobox", { name: "Listener" })).not.toBeInTheDocument();
    expect(screen.getByRole("combobox", { name: "Certificate" })).not.toHaveAccessibleDescription(exposure);
    expect(screen.queryByText(exposure)).not.toBeInTheDocument();
    expect(await save()).toEqual({ routes: [{ ...publicRoute, policy: { certificate_source: "default", visibility: "public", listener: null } }] });
  });

  it("makes a public route private with the chosen listener and certificate", async () => {
    renderPage({ routes: [publicRoute] });
    await userEvent.click(await screen.findByRole("button", { name: /Advanced routing/ }));
    const visibility = screen.getByRole("combobox", { name: "Visibility" });
    await waitFor(() => expect(listenerCalls()).toHaveLength(1));
    await chooseOption(visibility, "LAN only");
    const listener = screen.getByRole("combobox", { name: "Listener" });
    expect(listener).toHaveTextContent("Choose a listener");
    await userEvent.click(listener);
    expect(screen.queryByRole("option", { name: /tailnet/ })).not.toBeInTheDocument();
    await userEvent.click(screen.getByRole("option", { name: "lab · 192.168.20.2:9443" }));
    await userEvent.click(screen.getByRole("button", { name: "Save and apply" }));
    expect(await screen.findByText(/Private routes cannot use Let's Encrypt/)).toBeInTheDocument();
    expect(puts()).toHaveLength(0);
    await chooseOption(screen.getByRole("combobox", { name: "Certificate" }), "Proxy default certificate");
    expect(screen.queryByText(/Private routes cannot use Let's Encrypt/)).not.toBeInTheDocument();
    expect(await save()).toEqual({ routes: [{ ...publicRoute, policy: { visibility: "lan", listener: "lab", certificate_source: "default" } }] });
  });

  it("keeps the saved listener visible when the list cannot be read, with retry and no automatic change", async () => {
    renderPage({ routes: [privateRoute], listed: () => Promise.reject(new ApiError(502, "route listeners unavailable; verify or upgrade the worker")) });
    await userEvent.click(await screen.findByRole("button", { name: /Advanced routing/ }));
    expect(await screen.findByText("Could not load route listeners")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Retry" })).toBeInTheDocument();
    expect(screen.getByRole("combobox", { name: "Listener" })).toHaveTextContent("office (saved)");
    await userEvent.click(screen.getByRole("switch", { name: "Sticky sessions" }));
    expect(await save()).toEqual({ routes: [{ ...privateRoute, policy: { ...privateRoute.policy, sticky: true } }] });
  });

  it("treats listeners as unknown when a refetch fails after a successful read", async () => {
    let fail = false;
    const client = renderPage({ routes: [privateRoute], listed: () => (fail ? Promise.reject(new ApiError(502, "route listeners unavailable; verify or upgrade the worker")) : Promise.resolve({ listeners })) });
    await userEvent.click(await screen.findByRole("button", { name: /Advanced routing/ }));
    expect(await screen.findByRole("combobox", { name: "Listener" })).toHaveTextContent("office · 192.168.10.2:8443");
    fail = true;
    await client.refetchQueries({ queryKey: routeListenersKey("w1") });
    expect(await screen.findByText("Could not load route listeners")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Retry" })).toBeInTheDocument();
    const listener = screen.getByRole("combobox", { name: "Listener" });
    expect(listener).toHaveTextContent("office (saved)");
    expect(listener).toBeDisabled();
    expect(screen.queryByText(/Bound to/)).not.toBeInTheDocument();
    const visibility = screen.getByRole("combobox", { name: "Visibility" });
    expect(visibility).toHaveTextContent("LAN only");
    await userEvent.click(visibility);
    expect(screen.getByRole("option", { name: "Tailscale only" })).toHaveAttribute("aria-disabled", "true");
    await userEvent.keyboard("{Escape}");
    await userEvent.click(screen.getByRole("switch", { name: "Sticky sessions" }));
    expect(await save()).toEqual({ routes: [{ ...privateRoute, policy: { ...privateRoute.policy, sticky: true } }] });
  });

  it("explains loading and offers Retry on a public route until the listeners can be read", async () => {
    let attempt = 0;
    let finishFirst: (value: unknown) => void = () => undefined;
    renderPage({
      routes: [publicRoute],
      listed: () => {
        attempt += 1;
        if (attempt === 1) return new Promise((_, reject) => { finishFirst = reject; });
        return Promise.resolve({ listeners });
      },
    });
    await userEvent.click(await screen.findByRole("button", { name: /Advanced routing/ }));
    const visibility = screen.getByRole("combobox", { name: "Visibility" });
    expect(visibility).toHaveAccessibleDescription(/Reading the listeners of worker-1/);
    await userEvent.click(visibility);
    expect(screen.getByRole("option", { name: "LAN only" })).toHaveAttribute("aria-disabled", "true");
    await userEvent.keyboard("{Escape}");

    finishFirst(new ApiError(502, "route listeners unavailable; verify or upgrade the worker"));
    expect(await screen.findByText("Could not load route listeners")).toBeInTheDocument();
    expect(visibility).toHaveAccessibleDescription(/could not be read\. Retry below.*Could not load route listeners/);
    await userEvent.click(visibility);
    expect(screen.getByRole("option", { name: "LAN only" })).toHaveAttribute("aria-disabled", "true");
    await userEvent.keyboard("{Escape}");

    await userEvent.click(screen.getByRole("button", { name: "Retry" }));
    await waitFor(() => expect(screen.queryByText("Could not load route listeners")).not.toBeInTheDocument());
    expect(listenerCalls()).toHaveLength(2);
    await chooseOption(visibility, "LAN only");
    await chooseOption(screen.getByRole("combobox", { name: "Listener" }), "office · 192.168.10.2:8443");
    await chooseOption(screen.getByRole("combobox", { name: "Certificate" }), "Proxy default certificate");
    expect(await save()).toEqual({ routes: [{ ...publicRoute, policy: { visibility: "lan", listener: "office", certificate_source: "default" } }] });
  });

  it("shows one listener error on a saved private route", async () => {
    renderPage({ routes: [privateRoute], listed: () => Promise.reject(new ApiError(502, "route listeners unavailable; verify or upgrade the worker")) });
    await userEvent.click(await screen.findByRole("button", { name: /Advanced routing/ }));
    expect(await screen.findAllByText("Could not load route listeners")).toHaveLength(1);
  });

  it("flags a saved listener that the node no longer has", async () => {
    renderPage({ routes: [{ ...privateRoute, policy: { ...privateRoute.policy, listener: "gone" } }] });
    await userEvent.click(await screen.findByRole("button", { name: /Advanced routing/ }));
    expect(await screen.findByRole("combobox", { name: "Listener" })).toHaveTextContent("gone (unavailable)");
    expect(screen.getByText(/is not a LAN HTTPS listener on this node any more/)).toBeInTheDocument();
    await userEvent.click(screen.getByRole("switch", { name: "Sticky sessions" }));
    await userEvent.click(screen.getByRole("button", { name: "Save and apply" }));
    expect(await screen.findByText(/Listener gone is not configured on this node/)).toBeInTheDocument();
    expect(puts()).toHaveLength(0);
  });

  it("explains the upgrade on an older worker without reading listeners and keeps the saved value", async () => {
    renderPage({ routes: [privateRoute], routeProtocol: 4 });
    expect(await screen.findByText("Private routes need a newer worker")).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: /Advanced routing/ }));
    await userEvent.click(screen.getByRole("combobox", { name: "Visibility" }));
    expect(screen.getByRole("option", { name: "Tailscale only" })).toHaveAttribute("aria-disabled", "true");
    await userEvent.keyboard("{Escape}");
    expect(screen.getByRole("combobox", { name: "Listener" })).toHaveTextContent("office (saved)");
    expect(listenerCalls()).toHaveLength(0);
    await userEvent.click(screen.getByRole("switch", { name: "Sticky sessions" }));
    expect(await save()).toEqual({ routes: [{ ...privateRoute, policy: { ...privateRoute.policy, sticky: true } }] });
  });

  it("never turns a private hostname into an external link for viewers", async () => {
    renderPage({ routes: [privateRoute, { domain: "www.example.com", service: "web", port: 8080 }], permissions: ["read"] });
    const list = await screen.findByRole("list", { name: "Routes" });
    expect(within(list).getByText("app.example.com").closest("a")).toBeNull();
    expect(within(list).getByRole("link", { name: "www.example.com" })).toHaveAttribute("href", "https://www.example.com");
  });

  it("summarises privacy for viewers who cannot edit", async () => {
    renderPage({ routes: [privateRoute], permissions: ["read"] });
    const list = await screen.findByRole("list", { name: "Routes" });
    expect(await within(list).findByText(/LAN only via office \(192\.168\.10\.2:8443\), proxy default certificate/)).toBeInTheDocument();
  });
});

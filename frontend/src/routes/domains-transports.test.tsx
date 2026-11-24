import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { ReactNode } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { ApiError, request } from "../api/client";
import { nodesQuery } from "../api/queries";
import type { Listener } from "../api/ingress-listeners";
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
const httpRoute: Route = { domain: "app.example.com", service: "web", port: 8080 };
const tcpRoute: Route = { protocol: "tcp", domain: "", service: "db", port: 5432, policy: { listener: "postgres", visibility: "public", ip_allow_list: ["192.168.0.0/16"], upstreams: [{ service: "db2", port: 5432 }] } };
const udpRoute: Route = { protocol: "udp", domain: "", service: "resolver", port: 53, policy: { listener: "dns", visibility: "lan" } };
const listeners: Listener[] = [
  { id: "postgres", protocol: "tcp", visibility: "public", bindings: [{ address: "0.0.0.0", port: 15432 }] },
  { id: "redis", protocol: "tcp", visibility: "public", bindings: [{ address: "0.0.0.0", port: 16379 }] },
  { id: "dns", protocol: "udp", visibility: "lan", bindings: [{ address: "192.168.1.2", port: 5353 }] },
  { id: "office", protocol: "https", visibility: "lan", bindings: [{ address: "192.168.1.2", port: 8443 }] },
];

function node(routeProtocol: number): Node {
  return { id: "n1", name: "worker-1", address: "", port: 22, state: "ready", mode: "ssh", last_checked_at: null, capabilities: { protocol: 1, os: "linux", arch: "amd64", docker: "27", compose: "2", ready: true, route_policies: true, route_protocol: routeProtocol } };
}

function renderPage({ routes, routeProtocol = 6, nodes = () => Promise.resolve([node(routeProtocol)]), listed = () => Promise.resolve({ listeners }), saved = () => Promise.resolve({ state: "unchanged" }), permissions = ["read", "operate", "edit"] }: { routes: Route[] | null; routeProtocol?: number; nodes?: () => Promise<unknown>; listed?: () => Promise<unknown>; saved?: () => Promise<unknown>; permissions?: string[] }) {
  resource = { id: "w1", name: "web", node_id: "n1", kind: "service", compose: "services:\n  web:\n    image: nginx\n", source: null, environment_keys: [], has_source_token: false, permissions, manually_stopped: false, deployed: true, project_id: "p1", guarded_deploy: false, routes } as Workload;
  vi.mocked(request).mockImplementation((method: string, path: string) => {
    if (path === "/api/nodes") return nodes();
    if (path.startsWith("/api/operations")) return Promise.resolve([]);
    if (path === "/api/workloads/w1/route-listeners") return listed();
    if (path === "/api/workloads/w1/uptime") return Promise.resolve([]);
    if (path === "/api/workloads/w1/certificates" || path === "/api/workloads/w1/certificate-issuers" || path === "/api/workloads/w1/certificate-zones") return Promise.resolve([]);
    if (path.startsWith("/api/workloads/w1/certificate-challenges")) return Promise.resolve({ items: [], more: false });
    if (method === "PUT" && path === "/api/workloads/w1/routes") return saved();
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

// The first nodes read reports route protocol 6, later reads fail until recovered.
function failingNodes() {
  const state = { calls: 0, recovered: false };
  const read = () => {
    state.calls += 1;
    return state.calls === 1 || state.recovered ? Promise.resolve([node(6)]) : Promise.reject(new ApiError(502, "control plane unavailable"));
  };
  return { state, read };
}

async function failNodeRefetch(client: QueryClient) {
  await screen.findByRole("button", { name: "Save and apply" });
  await waitFor(() => expect(screen.getByRole("button", { name: "Add TCP or UDP route" })).toBeEnabled());
  await client.refetchQueries({ queryKey: nodesQuery.queryKey });
  expect(await screen.findByText("Could not load worker capabilities")).toBeInTheDocument();
}

const puts = () => vi.mocked(request).mock.calls.filter(([method, path]) => method === "PUT" && path === "/api/workloads/w1/routes");
const rawList = () => screen.getByRole("list", { name: "TCP and UDP routes" });

async function chooseOption(combobox: HTMLElement, name: string | RegExp) {
  await userEvent.click(combobox);
  await userEvent.click(await screen.findByRole("option", { name }));
}

async function save() {
  await userEvent.click(screen.getByRole("button", { name: "Save and apply" }));
  await waitFor(() => expect(puts()).toHaveLength(1));
  return puts()[0]?.[2];
}

async function addTcpRoute() {
  await userEvent.click(await screen.findByRole("button", { name: "Add TCP or UDP route" }));
  const row = within(rawList()).getAllByRole("listitem").at(-1);
  if (!row) throw new Error("missing row");
  return row;
}

describe("TCP and UDP routes on Domains", () => {
  it("adds a TCP route with the exact raw request and keeps HTTP routes unchanged", async () => {
    renderPage({ routes: [httpRoute] });
    const row = await addTcpRoute();
    await waitFor(() => expect(within(row).getByRole("combobox", { name: "Visibility" })).toBeEnabled());
    await userEvent.type(within(row).getByLabelText("Compose service"), "db");
    await userEvent.type(within(row).getByLabelText("Container port"), "5432");
    const listener = within(row).getByRole("combobox", { name: "Listener" });
    expect(listener).toHaveTextContent("Choose a visibility first");
    expect(listener).toBeDisabled();
    await chooseOption(within(row).getByRole("combobox", { name: "Visibility" }), "Public");
    await userEvent.click(listener);
    expect(screen.queryByRole("option", { name: /dns/ })).not.toBeInTheDocument();
    expect(screen.queryByRole("option", { name: /office/ })).not.toBeInTheDocument();
    await userEvent.click(screen.getByRole("option", { name: "postgres · 0.0.0.0:15432" }));
    expect(within(row).getByText("0.0.0.0:15432")).toBeInTheDocument();
    await userEvent.type(within(row).getByLabelText(/Allowed source IP ranges/), "192.168.0.0/16");
    await userEvent.click(within(row).getByRole("button", { name: "Add upstream" }));
    const upstream = within(within(row).getByRole("list", { name: "Additional upstreams" })).getByLabelText("Compose service");
    await userEvent.type(upstream, "db2");
    const upstreamPort = within(within(row).getByRole("list", { name: "Additional upstreams" })).getByLabelText("Container port");
    await userEvent.clear(upstreamPort);
    await userEvent.type(upstreamPort, "5432");
    expect(await save()).toEqual({ routes: [httpRoute, tcpRoute] });
  });

  it("hides HTTP-only and allowlist fields for UDP and sends none of them", async () => {
    renderPage({ routes: null });
    const row = await addTcpRoute();
    await waitFor(() => expect(within(row).getByRole("combobox", { name: "Visibility" })).toBeEnabled());
    await chooseOption(within(row).getByRole("combobox", { name: "Protocol" }), "UDP");
    expect(within(row).queryByLabelText(/Allowed source IP ranges/)).not.toBeInTheDocument();
    expect(within(row).queryByLabelText(/Hostname/)).not.toBeInTheDocument();
    expect(within(row).queryByText(/Path prefix|Certificate|Sticky|timeout/i)).not.toBeInTheDocument();
    expect(within(row).getByText(/UDP routes have no source IP allowlist, authentication or TLS/)).toBeInTheDocument();
    await userEvent.type(within(row).getByLabelText("Compose service"), "resolver");
    await userEvent.type(within(row).getByLabelText("Container port"), "53");
    await chooseOption(within(row).getByRole("combobox", { name: "Visibility" }), "LAN only");
    await chooseOption(within(row).getByRole("combobox", { name: "Listener" }), "dns · 192.168.1.2:5353");
    expect(await save()).toEqual({ routes: [udpRoute] });
  });

  it("keeps mixed saved routes unchanged on an unrelated HTTP edit, HTTP routes first", async () => {
    renderPage({ routes: [tcpRoute, httpRoute, udpRoute] });
    const save_ = await screen.findByRole("button", { name: "Save and apply" });
    expect(save_).toBeDisabled();
    expect(within(rawList()).getAllByRole("listitem").filter((item) => item.parentElement === rawList())).toHaveLength(2);
    const port = within(screen.getByRole("list", { name: "Routes" })).getByLabelText("Container port");
    await userEvent.clear(port);
    await userEvent.type(port, "8081");
    expect(await save()).toEqual({ routes: [{ ...httpRoute, port: 8081 }, tcpRoute, udpRoute] });
  });

  it("keeps an explicit HTTP protocol saved through the API unchanged and in the request", async () => {
    const explicit: Route = { ...httpRoute, protocol: "http" };
    renderPage({ routes: [explicit, tcpRoute] });
    expect(await screen.findByRole("button", { name: "Save and apply" })).toBeDisabled();
    const port = within(screen.getByRole("list", { name: "Routes" })).getByLabelText("Container port");
    await userEvent.clear(port);
    await userEvent.type(port, "8081");
    expect(await save()).toEqual({ routes: [{ ...explicit, port: 8081 }, tcpRoute] });
  });

  it("reports a listener used twice and a mismatched listener before saving", async () => {
    renderPage({ routes: [tcpRoute] });
    const row = await addTcpRoute();
    await waitFor(() => expect(within(row).getByRole("combobox", { name: "Visibility" })).toBeEnabled());
    await userEvent.type(within(row).getByLabelText("Compose service"), "cache");
    await userEvent.type(within(row).getByLabelText("Container port"), "6379");
    await chooseOption(within(row).getByRole("combobox", { name: "Visibility" }), "Public");
    await userEvent.click(within(row).getByRole("combobox", { name: "Listener" }));
    expect(screen.getByRole("option", { name: /postgres .*used by another route here/ })).toHaveAttribute("aria-disabled", "true");
    await userEvent.keyboard("{Escape}");
    await userEvent.click(screen.getByRole("button", { name: "Save and apply" }));
    expect(await within(row).findByText("Choose the TCP listener that receives this traffic.")).toBeInTheDocument();
    expect(puts()).toHaveLength(0);
    await chooseOption(within(row).getByRole("combobox", { name: "Listener" }), "redis · 0.0.0.0:16379");
    expect(within(row).queryByText("Choose the TCP listener that receives this traffic.")).not.toBeInTheDocument();
  });

  it("flags a saved listener that no longer matches and keeps it selected", async () => {
    renderPage({ routes: [{ ...tcpRoute, policy: { ...tcpRoute.policy, listener: "gone" } }] });
    const listener = await within(await screen.findByRole("list", { name: "TCP and UDP routes" })).findByRole("combobox", { name: "Listener" });
    await waitFor(() => expect(listener).toHaveTextContent("gone (unavailable)"));
    expect(screen.getByText(/is not a Public TCP listener on this node/)).toBeInTheDocument();
  });

  it("keeps the typed draft when the server refuses a listener claimed elsewhere", async () => {
    renderPage({ routes: null, saved: () => Promise.reject(new ApiError(409, "a selected transport listener already belongs to another resource or pending operation")) });
    const row = await addTcpRoute();
    await waitFor(() => expect(within(row).getByRole("combobox", { name: "Visibility" })).toBeEnabled());
    await userEvent.type(within(row).getByLabelText("Compose service"), "db");
    await userEvent.type(within(row).getByLabelText("Container port"), "5432");
    await chooseOption(within(row).getByRole("combobox", { name: "Visibility" }), "Public");
    await chooseOption(within(row).getByRole("combobox", { name: "Listener" }), "postgres · 0.0.0.0:15432");
    await save();
    expect(await screen.findByText(/already belongs to another resource or pending operation/)).toBeInTheDocument();
    expect(within(row).getByLabelText("Compose service")).toHaveValue("db");
    expect(within(row).getByRole("combobox", { name: "Listener" })).toHaveTextContent("postgres · 0.0.0.0:15432");
  });

  it("keeps the saved listener visible with Retry when listeners cannot be read", async () => {
    renderPage({ routes: [tcpRoute], listed: () => Promise.reject(new ApiError(502, "worker unavailable")) });
    expect(await screen.findByText("Could not load route listeners")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Retry" })).toBeInTheDocument();
    const listener = within(rawList()).getByRole("combobox", { name: "Listener" });
    expect(listener).toHaveTextContent("postgres (saved)");
    expect(listener).toBeDisabled();
    await userEvent.type(within(rawList()).getByLabelText(/Allowed source IP ranges/), "\n10.0.0.0/8");
    expect(await save()).toEqual({ routes: [{ ...tcpRoute, policy: { ...tcpRoute.policy, ip_allow_list: ["192.168.0.0/16", "10.0.0.0/8"] } }] });
  });

  it("explains the upgrade on an older worker, offers no new raw routes and still allows removal", async () => {
    renderPage({ routes: [httpRoute, tcpRoute], routeProtocol: 5 });
    expect(await screen.findByText("TCP and UDP routes need a newer worker")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Add TCP or UDP route" })).toBeDisabled();
    expect(within(rawList()).getByLabelText("Compose service")).toBeDisabled();
    await userEvent.click(screen.getByRole("button", { name: "Remove TCP route postgres" }));
    expect(await save()).toEqual({ routes: [httpRoute] });
  });

  it("waits for the node before offering raw routes", async () => {
    renderPage({ routes: null });
    expect(screen.getByText(/Checking whether the worker on its node supports TCP and UDP routes/)).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Add TCP or UDP route" })).toBeDisabled();
    await waitFor(() => expect(screen.getByRole("button", { name: "Add TCP or UDP route" })).toBeEnabled());
    expect(screen.queryByText(/Checking whether the worker/)).not.toBeInTheDocument();
  });

  it("keeps saved raw routes locked but removable while the node is unresolved", async () => {
    renderPage({ routes: [httpRoute, tcpRoute], nodes: () => new Promise(() => undefined) });
    expect(await screen.findByText(/Checking whether the worker on its node supports TCP and UDP routes/)).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Add TCP or UDP route" })).toBeDisabled();
    expect(within(rawList()).getByLabelText("Compose service")).toBeDisabled();
    expect(within(rawList()).getByRole("combobox", { name: "Visibility" })).toBeDisabled();
    expect(within(rawList()).getByLabelText(/Allowed source IP ranges/)).toBeDisabled();
    await userEvent.click(screen.getByRole("button", { name: "Remove TCP route postgres" }));
    expect(await save()).toEqual({ routes: [httpRoute] });
  });

  it("does not trust cached capabilities after a failed node refetch and recovers with Retry", async () => {
    const nodes = failingNodes();
    const client = renderPage({ routes: [tcpRoute], nodes: nodes.read });
    await failNodeRefetch(client);
    expect(screen.queryByText(/Checking whether the worker/)).not.toBeInTheDocument();
    expect(screen.getByText(/can be added or changed once the worker capabilities of worker-1 can be read/)).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Add TCP or UDP route" })).toBeDisabled();
    expect(within(rawList()).getByLabelText("Compose service")).toBeDisabled();
    expect(within(rawList()).getByRole("combobox", { name: "Listener" })).toBeDisabled();
    expect(screen.getByRole("button", { name: "Remove TCP route postgres" })).toBeEnabled();
    nodes.state.recovered = true;
    await userEvent.click(screen.getByRole("button", { name: "Retry" }));
    await waitFor(() => expect(screen.getByRole("button", { name: "Add TCP or UDP route" })).toBeEnabled());
    expect(screen.queryByText("Could not load worker capabilities")).not.toBeInTheDocument();
    expect(within(rawList()).getAllByLabelText("Compose service")[0]).toBeEnabled();
    expect(within(rawList()).getByRole("list", { name: "Additional upstreams" })).toBeInTheDocument();
  });

  it("lists raw routes for viewers without links or hostnames", async () => {
    renderPage({ routes: [httpRoute, tcpRoute, udpRoute], permissions: ["read"] });
    const list = await screen.findByRole("list", { name: "TCP and UDP routes" });
    expect(await within(list).findByText("TCP · Public · listener postgres (0.0.0.0:15432)")).toBeInTheDocument();
    expect(within(list).getByText("db:5432, db2:5432")).toBeInTheDocument();
    expect(within(list).getByText("UDP · LAN only · listener dns (192.168.1.2:5353)")).toBeInTheDocument();
    expect(within(list).queryByRole("link")).not.toBeInTheDocument();
    const links = within(screen.getByRole("list", { name: "Routes" })).getAllByRole("link");
    expect(links.map((link) => link.getAttribute("href"))).toEqual(["https://app.example.com"]);
  });
});

describe("HTTP upstream timeouts on Domains", () => {
  async function openAdvanced() {
    await userEvent.click(await screen.findByRole("button", { name: /Advanced routing/ }));
  }

  it("saves positive overrides only", async () => {
    renderPage({ routes: [httpRoute] });
    await openAdvanced();
    await waitFor(() => expect(screen.getByLabelText(/Upstream connection timeout/)).toBeEnabled());
    expect(screen.getByText(/WebSocket and other HTTP upgrades work automatically; no Upgrade headers need to be set/)).toBeInTheDocument();
    expect(screen.getByLabelText(/Idle pooled connection timeout/)).toHaveAccessibleDescription(/does not limit active WebSocket connections/);
    await userEvent.type(screen.getByLabelText(/Upstream connection timeout/), "5");
    await userEvent.type(screen.getByLabelText(/Response header timeout/), "0");
    await userEvent.type(screen.getByLabelText(/Idle pooled connection timeout/), "120");
    expect(await save()).toEqual({ routes: [{ ...httpRoute, policy: { timeouts: { dial_seconds: 5, idle_connection_seconds: 120 } } }] });
  });

  it("clears saved timeouts with null", async () => {
    renderPage({ routes: [{ ...httpRoute, policy: { timeouts: { response_header_seconds: 30 } } }] });
    await openAdvanced();
    const header = screen.getByLabelText(/Response header timeout/);
    expect(header).toHaveValue(30);
    await userEvent.clear(header);
    expect(await save()).toEqual({ routes: [{ ...httpRoute, policy: { timeouts: null } }] });
  });

  it("disables new timeouts on an older worker and keeps saved ones clearable", async () => {
    renderPage({ routes: [{ ...httpRoute, policy: { timeouts: { dial_seconds: 5 } } }], routeProtocol: 5 });
    expect(await screen.findByText("Upstream timeouts need a newer worker")).toBeInTheDocument();
    await openAdvanced();
    expect(screen.getByLabelText(/Response header timeout/)).toBeDisabled();
    const dial = screen.getByLabelText(/Upstream connection timeout/);
    expect(dial).toBeEnabled();
    await userEvent.clear(dial);
    expect(await save()).toEqual({ routes: [{ ...httpRoute, policy: { timeouts: null } }] });
  });

  it("allows only clearing saved timeouts after a failed node refetch", async () => {
    const nodes = failingNodes();
    const client = renderPage({ routes: [{ ...httpRoute, policy: { timeouts: { dial_seconds: 5 } } }], nodes: nodes.read });
    await failNodeRefetch(client);
    await openAdvanced();
    expect(screen.getByText(/New timeouts can be set once the worker capabilities of the node are known/)).toBeInTheDocument();
    expect(screen.getByLabelText(/Response header timeout/)).toBeDisabled();
    const dial = screen.getByLabelText(/Upstream connection timeout/);
    await userEvent.clear(dial);
    expect(dial).toBeDisabled();
    expect(await save()).toEqual({ routes: [{ ...httpRoute, policy: { timeouts: null } }] });
  });

  it("refuses a changed saved timeout after a failed node refetch", async () => {
    const nodes = failingNodes();
    const client = renderPage({ routes: [{ ...httpRoute, policy: { timeouts: { dial_seconds: 5 } } }], nodes: nodes.read });
    await failNodeRefetch(client);
    await openAdvanced();
    await userEvent.type(screen.getByLabelText(/Upstream connection timeout/), "0");
    await userEvent.click(screen.getByRole("button", { name: "Save and apply" }));
    expect(await screen.findByText(/Upstream timeouts can be set once the worker capabilities of the node are known/)).toBeInTheDocument();
    expect(puts()).toHaveLength(0);
  });
});

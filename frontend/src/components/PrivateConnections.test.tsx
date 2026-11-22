import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import type { ReactNode } from "react";
import { afterEach, expect, it, vi } from "vitest";
import { request } from "../api/client";
import type { ConnectionGraph, GraphResource, PrivateConnection } from "../lib/connections";
import { EnvironmentResources } from "./PrivateConnections";

vi.mock("@tanstack/react-router", () => ({ Link: ({ children, className }: { children: ReactNode; className?: string }) => <a href="#resource" className={className}>{children}</a> }));
vi.mock("../api/client", () => ({ request: vi.fn(), errorMessage: (error: unknown) => String(error) }));
afterEach(() => vi.clearAllMocks());
// jsdom does not implement modal dialogs.
HTMLDialogElement.prototype.showModal = function (this: HTMLDialogElement) {
  this.open = true;
};
HTMLDialogElement.prototype.close = function (this: HTMLDialogElement) {
  this.open = false;
};

const resource = (id: string, name: string, environment = "e1", node = "One"): GraphResource => ({
  id, name, kind: "service", project_id: "p", project: "Shop", environment_id: environment, environment: environment === "e1" ? "production" : "staging",
  node_id: node, node, node_state: "ready", manually_stopped: false, last_action: "deploy", last_state: "succeeded", services: ["app"], permissions: ["read", "operate", "edit"], private_links: true,
});

const connection = (id: string, source: string, target: string, overrides: Partial<PrivateConnection> = {}): PrivateConnection => ({
  id, source: { id: source, name: source.toUpperCase(), service: "app", restricted: false }, target: { id: target, name: target.toUpperCase(), service: "app", restricted: false },
  port: 5432, alias: `${target}.internal`, protocol: "tcp", cross_node: true, state: "applied", sides: [{ role: "source", state: "applied" }, { role: "target", state: "applied" }],
  broader_access: [], can_edit: true, can_remove: true, can_retry: true, updated_at: "2026-11-21T00:00:00Z", ...overrides,
});

const graph: ConnectionGraph = {
  resources: [resource("a", "A"), resource("db", "DB", "e1", "Two"), resource("d", "D", "e1", "Two"), resource("c", "C", "e2")],
  connections: [
    connection("a-db", "a", "db"),
    connection("d-c", "d", "c", { state: "error", detail: "The target has no containers yet.", broader_access: ["shared network legacy"] }),
    connection("x-db", "x", "db", { source: { restricted: true }, alias: undefined, can_edit: false, can_remove: true }),
  ],
  shared_networks: [{ name: "legacy", resources: ["d", "c"] }],
};

function renderGraph() {
  vi.mocked(request).mockImplementation((method: string, path: string) => Promise.resolve(respond(method, path)));
  function respond(method: string, path: string): unknown {
    if (path.startsWith("/api/projects/p/connections")) return graph;
    if (path === "/api/workloads") return [
      { id: "a", name: "A", kind: "service", permissions: ["read", "operate", "edit"], project_id: "p", environment_id: "e1", node_id: "One", node: "One" },
      { id: "db", name: "DB", kind: "service", permissions: ["read", "operate", "edit"], project_id: "p", environment_id: "e1", node_id: "Two", node: "Two" },
    ];
    if (path === "/api/operations") return [];
    if (method === "POST") return { id: "new" };
    throw new Error("unexpected " + path);
  }
  render(
    <QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}>
      <EnvironmentResources projectId="p" environmentId="e1" title="Resources in production" description="" resources={[]} empty={<p>Empty</p>} canCreate={false} />
    </QueryClientProvider>,
  );
}

it("lists directional connections with restricted endpoints and broader shared-network access", async () => {
  renderGraph();
  const list = await screen.findByRole("list", { name: "Private connections" });
  expect(within(list).getByText("db.internal:5432 · TCP")).toBeInTheDocument();
  expect(within(list).getByText("Resource you cannot view")).toBeInTheDocument();
  expect(within(list).getByText("Error")).toBeInTheDocument();
  expect(screen.getByText(/reach each other on every port/)).toBeInTheDocument();
  expect(request).toHaveBeenCalledWith("GET", "/api/projects/p/connections?environment=e1", undefined, expect.anything());
});

it("shows resource nodes and opens the editor for a selected connection", async () => {
  renderGraph();
  fireEvent.click(await screen.findByRole("button", { name: "Graph" }));
  const edge = await screen.findByRole("button", { name: /D · app to C · app, c.internal port 5432, Error/ });
  expect(screen.getByText("Shared networks give broader access")).toBeInTheDocument();
  fireEvent.click(edge);
  expect(await screen.findByText("Broader access exists")).toBeInTheDocument();
  expect(screen.getByText("The target has no containers yet.")).toBeInTheDocument();
  expect(screen.getByRole("button", { name: "Retry" })).toBeInTheDocument();
  expect(screen.getAllByText(/Running · Two/).length).toBeGreaterThan(0);
});

it("creates a connection with the keyboard alternative to dragging", async () => {
  renderGraph();
  fireEvent.click(await screen.findByRole("button", { name: "Graph" }));
  fireEvent.click(await screen.findByRole("button", { name: "Connect from A" }));
  expect(screen.getByRole("status")).toHaveTextContent("Choose the target for A");
  fireEvent.click(screen.getByRole("button", { name: "Connect to DB" }));
  const dialog = await screen.findByRole("dialog", { hidden: true });
  expect(within(dialog).getByLabelText("Private hostname", { selector: "input" })).toHaveValue("db.internal");
  fireEvent.change(within(dialog).getByLabelText("Target port", { selector: "input" }), { target: { value: "5432" } });
  fireEvent.click(within(dialog).getByRole("button", { name: "Create connection", hidden: true }));
  await waitFor(() =>
    expect(request).toHaveBeenCalledWith("POST", "/api/private-connections", { source_id: "a", target_id: "db", source_service: "app", target_service: "app", port: 5432, alias: "db.internal" }),
  );
});

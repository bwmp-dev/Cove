import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { request } from "../api/client";
import type { Connection } from "../api/connections";
import type { Session } from "../api/queries";
import type { Node } from "../api/types";
import { Connections } from "./Connections";

vi.mock("../api/client", () => ({ request: vi.fn(), errorMessage: (error: Error) => error.message }));
// A test may replace the default instance administrator session.
const sessionOverride = vi.hoisted(() => ({ current: null as Session | null }));
vi.mock("../lib/session", () => ({ useSession: () => sessionOverride.current ?? ({ is_admin: true, team_id: "one", teams: [{ id: "one", name: "Team", role: "owner", grants: [] }], permissions: ["view", "read_secrets", "operate", "edit_configuration", "manage_backups", "manage_members"] }) }));
beforeEach(() => vi.stubGlobal("ResizeObserver", class { observe = vi.fn(); disconnect = vi.fn(); }));
afterEach(() => {
  sessionOverride.current = null;
  vi.resetAllMocks();
  vi.unstubAllGlobals();
});

const b2: Connection = { id: "c1", name: "Offsite", storage: { protocol: "b2", endpoint: "", path: "cove/prod", access_key: "0012ab" } };
const node = (id: string, capabilities: { backup_retention: boolean; backblaze_b2: boolean }): Node => ({ id, name: id, address: "", port: 22, state: "ready", mode: "ssh", last_checked_at: null, capabilities: { infrastructure: true, ...capabilities, protocol: 1, os: "linux", arch: "amd64", docker: "", compose: "", ready: true } });

function renderConnections(connections: Connection[]) {
  vi.mocked(request).mockImplementation((method: string, path: string) => {
    if (method !== "GET") return Promise.resolve({ ok: true });
    if (path === "/api/nodes") return Promise.resolve([node("upgraded", { backup_retention: true, backblaze_b2: true }), node("retention-only", { backup_retention: true, backblaze_b2: false }), node("legacy", { backup_retention: false, backblaze_b2: false })]);
    return Promise.resolve(connections);
  });
  render(<QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}><Connections kind="storage-locations" /></QueryClientProvider>);
}

it("creates a native B2 location with key labels and no endpoint or CA", async () => {
  const user = userEvent.setup();
  renderConnections([]);
  await user.click(await screen.findByRole("button", { name: "Add storage location" }));
  await user.click(screen.getByRole("combobox", { name: "Protocol" }));
  await user.click(screen.getByRole("option", { name: "Backblaze B2" }));
  expect(screen.queryByLabelText("Endpoint")).not.toBeInTheDocument();
  expect(screen.queryByLabelText(/Private CA certificate/)).not.toBeInTheDocument();
  expect(screen.getByText("Requires upgraded workers")).toBeInTheDocument();
  await user.type(screen.getByLabelText("Name"), "Offsite");
  await user.type(screen.getByLabelText("Bucket and path"), "/cove");
  await user.type(screen.getByLabelText("Application key ID"), "0012ab");
  await user.type(screen.getByLabelText("Application key"), "K001secret");
  await user.click(screen.getByRole("button", { name: "Save" }));
  expect(screen.getByText("Start with the bucket name, without a leading slash.")).toBeInTheDocument();
  expect(screen.getByLabelText("Bucket and path")).toHaveFocus();
  expect(request).not.toHaveBeenCalledWith("POST", expect.anything(), expect.anything());

  await user.clear(screen.getByLabelText("Bucket and path"));
  await user.type(screen.getByLabelText("Bucket and path"), "cove/prod");
  await user.click(screen.getByRole("button", { name: "Save" }));
  await waitFor(() => expect(request).toHaveBeenCalledWith("POST", "/api/storage-locations", { name: "Offsite", storage: { protocol: "b2", endpoint: "", path: "cove/prod", access_key: "0012ab", secret_key: "K001secret" } }));
});

it("leaves the application key blank when editing and keeps it optional", async () => {
  renderConnections([b2]);
  expect(await screen.findByText("Backblaze B2 · cove/prod")).toBeInTheDocument();
  fireEvent.click(screen.getByRole("button", { name: "Edit" }));
  expect(screen.getByLabelText("Application key ID")).toHaveValue("0012ab");
  expect(screen.getByLabelText("Application key")).toHaveValue("");
  expect(screen.getByLabelText("Application key")).not.toBeRequired();
  fireEvent.click(screen.getByRole("button", { name: "Save" }));
  await waitFor(() => expect(request).toHaveBeenCalledWith("PUT", "/api/storage-locations/c1", { name: "Offsite", storage: { protocol: "b2", endpoint: "", path: "cove/prod", access_key: "0012ab", secret_key: "" } }));
});

it("only offers upgraded workers for B2 connection tests", async () => {
  const user = userEvent.setup();
  renderConnections([b2]);
  await user.click(await screen.findByRole("button", { name: "Test" }));
  expect(await screen.findByText(/Only upgraded workers are listed/)).toBeInTheDocument();
  await user.click(screen.getByRole("combobox", { name: "Node" }));
  expect(screen.getByRole("option", { name: "upgraded" })).toBeInTheDocument();
  expect(screen.queryByRole("option", { name: "retention-only" })).not.toBeInTheDocument();
  expect(screen.queryByRole("option", { name: "legacy" })).not.toBeInTheDocument();
});

// Fixtures only: no registry is contacted.
const session: Session = { user: "admin", user_id: "u1", email: "admin@example.test", name: "Admin", is_admin: false, has_password: true, github_linked: false, team_id: "t1", teams: [{ id: "t1", name: "Team", role: "admin", grants: [] }], permissions: ["view", "read_secrets", "operate", "edit_configuration", "manage_backups", "manage_members"] };
const registryConnections: Connection[] = [
  { id: "c1", name: "Team registry (m1abcdef)", managed_registry_id: "m1", registry: { server: "registry.example.test", username: "cove_registry" } },
  { id: "c2", name: "GitHub packages", registry: { server: "ghcr.io", username: "team" } },
];

it("keeps Test but hides Edit and Delete on the connection generated for a managed registry", async () => {
  vi.mocked(request).mockImplementation((method: string, path: string) => (method === "GET" && path === "/api/registries" ? Promise.resolve(registryConnections) : Promise.reject(new Error(`unexpected request ${method} ${path}`))));
  sessionOverride.current = session;
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(
    <QueryClientProvider client={client}>
      <Connections kind="registries" />
    </QueryClientProvider>,
  );
  const managed = (await screen.findByRole("heading", { name: "Team registry (m1abcdef)" })).closest("li");
  const ordinary = screen.getByRole("heading", { name: "GitHub packages" }).closest("li");
  if (!managed || !ordinary) throw new Error("connection rows missing");
  expect(within(managed).getByText(/Managed by Cove/)).toBeInTheDocument();
  expect(within(managed).getByRole("link", { name: "View managed registry" })).toHaveAttribute("href", "#managed-registry-m1");
  expect(within(managed).getByRole("button", { name: "Test" })).toBeInTheDocument();
  expect(within(managed).queryByRole("button", { name: "Edit" })).not.toBeInTheDocument();
  expect(within(managed).queryByRole("button", { name: "Delete" })).not.toBeInTheDocument();
  expect(within(ordinary).getByRole("button", { name: "Edit" })).toBeInTheDocument();
  expect(within(ordinary).getByRole("button", { name: "Delete" })).toBeInTheDocument();
  expect(within(ordinary).queryByText(/Managed by Cove/)).not.toBeInTheDocument();
});

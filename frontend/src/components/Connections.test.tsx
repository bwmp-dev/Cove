import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, within } from "@testing-library/react";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { request } from "../api/client";
import type { Connection } from "../api/connections";
import { keys, type Session } from "../api/queries";
import { Connections } from "./Connections";

vi.mock("../api/client", async (original) => ({ ...(await original<typeof import("../api/client")>()), request: vi.fn() }));
beforeEach(() => vi.stubGlobal("ResizeObserver", class { observe = vi.fn(); disconnect = vi.fn(); }));
afterEach(() => {
  vi.clearAllMocks();
  vi.unstubAllGlobals();
});

// Fixtures only: no registry is contacted.
const session: Session = { user: "admin", user_id: "u1", email: "admin@example.test", name: "Admin", is_admin: false, has_password: true, github_linked: false, team_id: "t1", teams: [{ id: "t1", name: "Team", role: "admin", grants: [] }] };
const connections: Connection[] = [
  { id: "c1", name: "Team registry (m1abcdef)", managed_registry_id: "m1", registry: { server: "registry.example.test", username: "cove_registry" } },
  { id: "c2", name: "GitHub packages", registry: { server: "ghcr.io", username: "team" } },
];

it("keeps Test but hides Edit and Delete on the connection generated for a managed registry", async () => {
  vi.mocked(request).mockImplementation((method: string, path: string) => (method === "GET" && path === "/api/registries" ? Promise.resolve(connections) : Promise.reject(new Error(`unexpected request ${method} ${path}`))));
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  client.setQueryData(keys.session, session);
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

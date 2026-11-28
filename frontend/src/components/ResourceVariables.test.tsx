import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { request } from "../api/client";
import type { WorkloadSummary } from "../api/types";
import { editorAccess, editWithoutSecrets, secretsWithoutEdit, viewerAccess } from "../test/permissions";
import { ResourceVariables } from "./ResourceVariables";

vi.mock("../api/client", async (original) => ({ ...(await original<typeof import("../api/client")>()), request: vi.fn() }));
beforeEach(() => vi.stubGlobal("ResizeObserver", class { observe = vi.fn(); disconnect = vi.fn(); }));
afterEach(() => {
  vi.clearAllMocks();
  vi.unstubAllGlobals();
});

const summary = (id: string, name: string, permissions: WorkloadSummary["permissions"], kind: WorkloadSummary["kind"] = "service"): WorkloadSummary => ({ id, name, project: "Shop", project_id: "p", node_id: "n", node: "One", kind, permissions });

it("offers binding sources by read_secrets on the source, not by edit access", async () => {
  const user = userEvent.setup();
  vi.mocked(request).mockImplementation((method: string, path: string) => {
    if (path === "/api/workloads") return Promise.resolve([summary("app", "app", editorAccess), summary("db", "database", secretsWithoutEdit), summary("cache", "cache", editWithoutSecrets)]);
    if (path === "/api/workloads/app/variable-bindings") return Promise.resolve([]);
    if (path === "/api/workloads/db") return Promise.resolve({ id: "db", environment_keys: ["PASSWORD"] });
    return Promise.reject(new Error(`unexpected ${method} ${path}`));
  });
  render(
    <QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}>
      <ResourceVariables workloadId="app" />
    </QueryClientProvider>,
  );
  expect(await screen.findByText("No resource bindings.")).toBeInTheDocument();
  await user.click(screen.getByRole("combobox", { name: "Source resource" }));
  expect(await screen.findByRole("option", { name: "Shop / database" })).toBeInTheDocument();
  expect(screen.queryByRole("option", { name: "Shop / cache" })).not.toBeInTheDocument();
  expect(screen.queryByRole("option", { name: "Shop / app" })).not.toBeInTheDocument();
  await user.click(screen.getByRole("option", { name: "Shop / database" }));
  await waitFor(() => expect(request).toHaveBeenCalledWith("GET", "/api/workloads/db", undefined, expect.anything()));
});

const databaseKeys = ["DB_HOST", "DB_PORT", "DB_USER", "DB_PASSWORD", "DB_NAME", "DB_URL", "DB_POOL_HOST", "DB_POOL_USER", "DB_POOL_PASSWORD", "DB_POOL_URL", "DB_PUBLIC_HOST", "DB_PUBLIC_PORT", "DB_PUBLIC_URL", "DB_TLS_CA"];

function renderWithSources(sources: WorkloadSummary[]) {
  vi.mocked(request).mockImplementation((method: string, path: string) => {
    if (path === "/api/workloads") return Promise.resolve(sources);
    if (path === "/api/workloads/app/variable-bindings") return Promise.resolve([]);
    if (method === "PUT" && path === "/api/workloads/app/variable-bindings") return Promise.resolve(null);
    if (path === "/api/workloads/db") return Promise.resolve({ id: "db", environment_keys: databaseKeys, database: { engine: "postgresql", service: "db", host: "db.internal", port: 5432 } });
    return Promise.reject(new Error(`unexpected ${method} ${path}`));
  });
  render(
    <QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}>
      <ResourceVariables workloadId="app" />
    </QueryClientProvider>,
  );
}

it("lets a custom role with only View on a database bind its public metadata, never its passwords or URLs", async () => {
  const user = userEvent.setup();
  renderWithSources([summary("app", "app", editWithoutSecrets), summary("db", "orders", viewerAccess, "database"), summary("svc", "worker", viewerAccess)]);
  await user.click(await screen.findByRole("combobox", { name: "Source resource" }));
  expect(screen.queryByRole("option", { name: "Shop / worker" })).not.toBeInTheDocument();
  await user.click(await screen.findByRole("option", { name: "Shop / orders" }));
  await user.type(screen.getByRole("textbox", { name: "Binding name" }), "ORDERS_HOST");
  const key = screen.getByRole("combobox", { name: "Source variable" });
  await waitFor(() => expect(key).toBeEnabled());
  expect(key).toHaveAccessibleDescription("Passwords and connection URLs need Read secrets on this database.");
  await user.click(key);
  const offered = screen.getAllByRole("option").map((option) => option.textContent?.split(" ")[0]);
  expect(offered).toEqual(expect.arrayContaining(["DB_HOST", "DB_PORT", "DB_USER", "DB_NAME", "DB_POOL_HOST", "DB_POOL_USER", "DB_PUBLIC_HOST", "DB_PUBLIC_PORT", "DB_TLS_CA"]));
  for (const secret of ["DB_PASSWORD", "DB_URL", "DB_POOL_PASSWORD", "DB_POOL_URL", "DB_PUBLIC_URL"]) expect(offered).not.toContain(secret);
  await user.click(screen.getByRole("option", { name: "DB_PUBLIC_HOST (Public TLS hostname)" }));
  await user.click(screen.getByRole("button", { name: "Save binding" }));
  await waitFor(() => expect(request).toHaveBeenCalledWith("PUT", "/api/workloads/app/variable-bindings", { name: "ORDERS_HOST", source_id: "db", source_key: "DB_PUBLIC_HOST" }));
});

it("offers database passwords and URLs once the source grants Read secrets", async () => {
  const user = userEvent.setup();
  renderWithSources([summary("app", "app", editorAccess), summary("db", "orders", secretsWithoutEdit, "database")]);
  await user.click(await screen.findByRole("combobox", { name: "Source resource" }));
  await user.click(await screen.findByRole("option", { name: "Shop / orders" }));
  const key = screen.getByRole("combobox", { name: "Source variable" });
  await waitFor(() => expect(key).toBeEnabled());
  expect(screen.queryByText("Passwords and connection URLs need Read secrets on this database.")).not.toBeInTheDocument();
  await user.click(key);
  expect(screen.getByRole("option", { name: "DB_PASSWORD (Password, secret)" })).toBeInTheDocument();
  expect(screen.getByRole("option", { name: "DB_POOL_URL (Pooler connection URL, secret)" })).toBeInTheDocument();
  expect(screen.getByRole("option", { name: "DB_HOST (Private hostname)" })).toBeInTheDocument();
});

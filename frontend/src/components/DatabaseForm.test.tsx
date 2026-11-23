import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { ApiError, request } from "../api/client";
import type { Project } from "../api/projects";
import type { Node } from "../api/types";
import { DatabaseForm } from "./DatabaseForm";

vi.mock("@tanstack/react-router", () => ({ Link: ({ children }: { children: unknown }) => children }));
vi.mock("../api/client", async (original) => ({ ...(await original<typeof import("../api/client")>()), request: vi.fn() }));
beforeEach(() => vi.stubGlobal("ResizeObserver", class { observe = vi.fn(); disconnect = vi.fn(); }));
afterEach(() => {
  vi.clearAllMocks();
  vi.unstubAllGlobals();
});

const engines = {
  engines: [
    { engine: "postgresql", name: "PostgreSQL", version: "18.7-alpine", versions: ["18.7-alpine", "17.12-alpine", "18.6-postgis3.6"], version_details: [{ version: "17.12-alpine", image: "postgres:17.12-alpine", variant: "" }, { version: "18.6-postgis3.6", image: "postgis/postgis:18-3.6@sha256:abc", variant: "postgis", warning: "Linux amd64 only. This pinned image contains PostgreSQL 18.6." }], image: "postgres:18.7-alpine", service: "postgres", port: 5432, template_id: "postgres", template_revision: "rev-1", logical_backup: true, minimum: { cpus: 0.5, memory_mb: 256, disk_mb: 1024 } },
    { engine: "redis", name: "Redis", version: "8.10.3-alpine", versions: ["8.10.3-alpine"], image: "redis:8.10.3-alpine", service: "redis", port: 6379, template_id: "redis", template_revision: "rev-1", logical_backup: false },
  ],
};
const nodes = [{ id: "node-1", name: "worker", state: "ready" }] as Node[];
const projects = [{ id: "project-1", name: "Shop", permissions: ["read", "operate", "edit"], environments: [] }] as unknown as Project[];

function renderForm(onCreated = vi.fn()) {
  render(
    <QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}>
      <DatabaseForm nodes={nodes} projects={projects} initialProjectId="project-1" initialEnvironmentId="" cancel={null} onCreated={onCreated} />
    </QueryClientProvider>,
  );
  return onCreated;
}

it("creates a typed database with the pinned catalogue revision and no Compose or credentials", async () => {
  vi.mocked(request).mockImplementation((method: string, path: string) => Promise.resolve(method === "GET" && path === "/api/database-engines" ? engines : { id: "db-1" }));
  const user = userEvent.setup();
  const onCreated = renderForm();
  await user.click(await screen.findByRole("radio", { name: /PostgreSQL/ }));
  await user.type(screen.getByLabelText("Name"), "orders");
  await user.click(screen.getByRole("combobox", { name: "Node" }));
  await user.click(screen.getByRole("option", { name: "worker" }));
  await user.click(screen.getByRole("radio", { name: /17\.12-alpine/ }));
  await user.click(screen.getByRole("button", { name: "Create database" }));
  await waitFor(() => expect(onCreated).toHaveBeenCalledWith("db-1"));
  expect(request).toHaveBeenCalledWith("POST", "/api/workloads", {
    name: "orders",
    node_id: "node-1",
    project_id: "project-1",
    kind: "database",
    database: { engine: "postgresql", version: "17.12-alpine", template_revision: "rev-1" },
  });
});

it("reloads the catalogue when the pinned revision is stale", async () => {
  let loads = 0;
  vi.mocked(request).mockImplementation((method: string) => {
    if (method === "GET") {
      loads += 1;
      return Promise.resolve(engines);
    }
    return Promise.reject(new ApiError(409, "Database template changed; select its current pinned version"));
  });
  const user = userEvent.setup();
  renderForm();
  await user.click(await screen.findByRole("radio", { name: /Redis/ }));
  expect(screen.queryByRole("group", { name: "Version" })).not.toBeInTheDocument();
  await user.type(screen.getByLabelText("Name"), "cache");
  await user.click(screen.getByRole("combobox", { name: "Node" }));
  await user.click(screen.getByRole("option", { name: "worker" }));
  await user.click(screen.getByRole("button", { name: "Create database" }));
  await screen.findByText(/The engine list has been reloaded/);
  await waitFor(() => expect(loads).toBe(2));
});

it("shows each variant's warning before it is chosen and the pinned image after", async () => {
  vi.mocked(request).mockImplementation(() => Promise.resolve(engines));
  const user = userEvent.setup();
  renderForm();
  await user.click(await screen.findByRole("radio", { name: /PostgreSQL/ }));
  expect(screen.getByText("Linux amd64 only. This pinned image contains PostgreSQL 18.6.")).toBeInTheDocument();
  expect(screen.getByRole("radio", { name: /18\.7-alpine \(current\)/ })).toBeChecked();
  await user.click(screen.getByRole("radio", { name: /18\.6-postgis3\.6 · PostGIS/ }));
  expect(screen.getByText("postgis/postgis:18-3.6@sha256:abc")).toBeInTheDocument();
  expect(screen.getByText("Needs at least 0.5 CPU, 256 MiB memory, and 1024 MiB disk.")).toBeInTheDocument();
});

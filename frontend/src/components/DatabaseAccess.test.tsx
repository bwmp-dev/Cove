import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, expect, it, vi } from "vitest";
import { request } from "../api/client";
import type { DatabaseResource } from "../api/types";
import { DatabaseAccess } from "./DatabaseAccess";

vi.mock("@tanstack/react-router", () => ({ Link: ({ children }: { children: unknown }) => children }));
vi.mock("../api/client", async (original) => ({ ...(await original<typeof import("../api/client")>()), request: vi.fn() }));
afterEach(() => vi.clearAllMocks());

const key = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIFixtureKeyOnly test";

function mockApi(tools: string[]) {
  vi.mocked(request).mockImplementation((method: string, path: string) => {
    if (path === "/api/workloads/abc/database") return Promise.resolve({ admin_tools: tools });
    if (path.startsWith("/api/operations")) return Promise.resolve([]);
    if (method === "GET" && path === "/api/workloads/abc/database-access") return Promise.resolve([]);
    if (method === "POST") return Promise.resolve({ id: "lease", expires_at: "2026-11-22T12:15:00Z", host: "worker.example", ssh_port: 22, host_key: "ssh-ed25519 AAAAhost" });
    return Promise.reject(new Error(`Unexpected ${method} ${path}`));
  });
}

function renderAccess(database: DatabaseResource) {
  render(
    <QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}>
      <DatabaseAccess id="abc" database={database} />
    </QueryClientProvider>,
  );
}

it("opens an Adminer lease on the database service and explains the local login", async () => {
  mockApi(["adminer"]);
  const user = userEvent.setup();
  renderAccess({ engine: "mariadb", version: "11.8.9", template_revision: "r", service: "mariadb", image: "mariadb:11.8.9", port: 3306, host: "db-abc.internal" });
  await user.click(await screen.findByRole("radio", { name: "Adminer in the browser" }));
  await user.type(screen.getByLabelText("Ed25519 public key"), key);
  await user.click(screen.getByRole("button", { name: "Open Adminer access" }));
  await waitFor(() => expect(request).toHaveBeenCalledWith("POST", "/api/workloads/abc/database-access", { service: "mariadb", port: 8080, public_key: key, tool: "adminer" }));
  expect(await screen.findByText("http://127.0.0.1:18080")).toBeInTheDocument();
  expect(screen.getByRole("region", { name: "Local database connection command" })).toHaveTextContent("--local-port 18080");
});

it("offers only the native tunnel for engines without an admin tool", async () => {
  mockApi([]);
  const user = userEvent.setup();
  renderAccess({ engine: "clickhouse", version: "25", template_revision: "r", service: "clickhouse", image: "clickhouse:25", port: 9000, host: "db-abc.internal" });
  await screen.findByText(/A browser admin tool is not offered/);
  expect(screen.queryByRole("radio", { name: "Adminer in the browser" })).not.toBeInTheDocument();
  await user.type(screen.getByLabelText("Ed25519 public key"), key);
  await user.click(screen.getByRole("button", { name: "Open temporary access" }));
  await waitFor(() => expect(request).toHaveBeenCalledWith("POST", "/api/workloads/abc/database-access", { service: "clickhouse", port: 9000, public_key: key }));
});

it("opens a Mongo Express lease on its helper port and explains Basic authentication", async () => {
  mockApi(["mongo-express"]);
  const user = userEvent.setup();
  renderAccess({ engine: "mongodb", version: "8.0.38-noble", template_revision: "r", service: "mongo", image: "mongo:8.0.38-noble", port: 27017, host: "db-abc.internal" });
  await user.click(await screen.findByRole("radio", { name: "Mongo Express in the browser" }));
  await user.type(screen.getByLabelText("Ed25519 public key"), key);
  await user.click(screen.getByRole("button", { name: "Open Mongo Express access" }));
  await waitFor(() => expect(request).toHaveBeenCalledWith("POST", "/api/workloads/abc/database-access", { service: "mongo", port: 8081, public_key: key, tool: "mongo-express" }));
  expect(await screen.findByText("http://127.0.0.1:18081")).toBeInTheDocument();
  expect(screen.getByText(/asks for a user name and password/)).toBeInTheDocument();
});

it("opens a Redis Insight lease and leaves its license acceptance to the person", async () => {
  mockApi(["redisinsight"]);
  const user = userEvent.setup();
  renderAccess({ engine: "valkey", version: "9", template_revision: "r", service: "valkey", image: "valkey:9", port: 6379, host: "db-abc.internal" });
  await user.click(await screen.findByRole("radio", { name: "Redis Insight in the browser" }));
  await user.type(screen.getByLabelText("Ed25519 public key"), key);
  await user.click(screen.getByRole("button", { name: "Open Redis Insight access" }));
  await waitFor(() => expect(request).toHaveBeenCalledWith("POST", "/api/workloads/abc/database-access", { service: "valkey", port: 5540, public_key: key, tool: "redisinsight" }));
  expect(await screen.findByText(/Cove does not accept them for you/)).toBeInTheDocument();
});

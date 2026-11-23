import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen } from "@testing-library/react";
import { afterEach, expect, it, vi } from "vitest";
import { request } from "../api/client";
import type { DatabaseResource, Workload } from "../api/types";
import { ResourceLimits } from "./resource-config";

const resource = vi.hoisted((): { current: unknown } => ({ current: null }));
vi.mock("./workload-detail", () => ({ useResource: () => resource.current }));
vi.mock("@tanstack/react-router", () => ({ Link: ({ children }: { children: unknown }) => children, useBlocker: () => ({ status: "idle" }) }));
vi.mock("../api/client", async (original) => ({ ...(await original<typeof import("../api/client")>()), request: vi.fn() }));
vi.mock("../components/GPUs", () => ({ ResourceGPUs: () => <p>GPU assignment editor</p> }));
vi.mock("../components/RightSizing", () => ({ RightSizing: () => null }));
afterEach(() => vi.resetAllMocks());

const database: DatabaseResource = { engine: "postgresql", version: "17", template_revision: "r", service: "db", image: "postgres:17", port: 5432, host: "db.internal" };
const base: Workload = { id: "w1", name: "orders", node_id: "n1", project_id: "p1", kind: "service", compose: "", environment_keys: [], source: null, has_source_token: false, manually_stopped: false, deployed: true, permissions: ["read", "operate", "edit"], limits: { db: { cpus: 1, memory_mb: 512 } } };

function renderLimits(workload: Workload) {
  resource.current = workload;
  vi.mocked(request).mockResolvedValue([]);
  render(<QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}><ResourceLimits /></QueryClientProvider>);
}

it("hides GPU assignments on managed databases but keeps their CPU and memory limits", async () => {
  renderLimits({ ...base, kind: "database", database });
  expect(await screen.findByDisplayValue("512")).toBeInTheDocument();
  expect(screen.queryByText("GPU assignment editor")).not.toBeInTheDocument();
});

it("keeps GPU assignments for other services", async () => {
  renderLimits(base);
  expect(await screen.findByDisplayValue("512")).toBeInTheDocument();
  expect(screen.getByText("GPU assignment editor")).toBeInTheDocument();
});

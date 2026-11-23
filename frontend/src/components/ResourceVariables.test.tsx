import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { request } from "../api/client";
import type { WorkloadSummary } from "../api/types";
import { editorAccess, editWithoutSecrets, secretsWithoutEdit } from "../test/permissions";
import { ResourceVariables } from "./ResourceVariables";

vi.mock("../api/client", async (original) => ({ ...(await original<typeof import("../api/client")>()), request: vi.fn() }));
beforeEach(() => vi.stubGlobal("ResizeObserver", class { observe = vi.fn(); disconnect = vi.fn(); }));
afterEach(() => {
  vi.clearAllMocks();
  vi.unstubAllGlobals();
});

const summary = (id: string, name: string, permissions: WorkloadSummary["permissions"]): WorkloadSummary => ({ id, name, project: "Shop", project_id: "p", node_id: "n", node: "One", kind: "service", permissions });

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

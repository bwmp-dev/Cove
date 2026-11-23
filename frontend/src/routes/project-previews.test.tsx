import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { ReactNode } from "react";
import { afterEach, expect, it, vi } from "vitest";
import { request } from "../api/client";
import type { EnvironmentPreview, PreviewResource } from "../api/previews";
import type { Permission } from "../api/types";
import { editWithoutSecrets, secretsWithoutEdit } from "../test/permissions";
import { PreviewStatus } from "./project-previews";

vi.mock("../api/client", async (original) => ({ ...(await original<typeof import("../api/client")>()), request: vi.fn() }));
vi.mock("@tanstack/react-router", () => ({ getRouteApi: () => ({}), Link: ({ children }: { children: ReactNode }) => <a href="#link">{children}</a> }));
vi.mock("../components/PreviewActions", () => ({ PreviewActions: () => null }));
afterEach(() => vi.clearAllMocks());

const resource: PreviewResource = {
  base_id: "w1", resource_id: "pw1", name: "web", kind: "service", source: "branch", revision: "4f1c2e9a", baseline_revision: "", baseline_release: "", baseline_deployed_at: null,
  deployed: true, data_state: "none", snapshot_at: null, data_report: null, routes: ["web-pr.example.test"], published: true, operation: "",
};

// Environment-preview views report the base environment's exact catalogue permissions.
function preview(permissions: Permission[]): EnvironmentPreview {
  return {
    id: "pv", project_id: "p", environment_id: "pv-env", base_environment_id: "prod", suffix: "demo", slug: "demo", state: "ready", stage: "ready", detail: "", pinned: false, access: "private",
    delete_reason: "", last_activity_at: "2026-11-22T10:00:00Z", expires_at: null, created_at: "2026-11-22T09:00:00Z", branches: [], resources: [resource], permissions,
  };
}

function setup(permissions: Permission[]) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  vi.mocked(request).mockImplementation((method: string, path: string) => {
    if (path === "/api/environment-previews/pv") return Promise.resolve(preview(permissions));
    if (path === "/api/environment-previews/pv/credentials") return Promise.resolve({ username: "preview", password: "pr3view-secret" });
    return Promise.reject(new Error(`unexpected ${method} ${path}`));
  });
  render(
    <QueryClientProvider client={client}>
      <PreviewStatus previewId="pv" />
    </QueryClientProvider>,
  );
  return client;
}

it("does not offer credentials to a base-environment editor without read_secrets", async () => {
  setup(editWithoutSecrets);
  expect(await screen.findByText(/Ask someone who can read secrets in the base environment/)).toBeInTheDocument();
  expect(screen.queryByRole("button", { name: "Show credentials" })).not.toBeInTheDocument();
  expect(request).not.toHaveBeenCalledWith("GET", "/api/environment-previews/pv/credentials");
});

it("reveals credentials on request to read_secrets without edit and keeps them out of the query cache", async () => {
  const user = userEvent.setup();
  const client = setup(secretsWithoutEdit);
  await user.click(await screen.findByRole("button", { name: "Show credentials" }));
  expect(await screen.findByText("pr3view-secret")).toBeInTheDocument();
  expect(request).toHaveBeenCalledWith("GET", "/api/environment-previews/pv/credentials");
  expect(JSON.stringify(client.getQueryCache().getAll().map((query) => query.state.data))).not.toContain("pr3view-secret");
  await user.click(screen.getByRole("button", { name: "Hide" }));
  expect(screen.queryByText("pr3view-secret")).not.toBeInTheDocument();
});

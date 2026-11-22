import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, waitFor } from "@testing-library/react";
import type { ReactNode } from "react";
import { afterEach, expect, it, vi } from "vitest";
import { request } from "../api/client";
import { EnvironmentPage, ProjectLayout } from "./project";

const route = vi.hoisted(() => ({ environmentId: "e1" }));

vi.mock("@tanstack/react-router", () => ({
  Link: ({ children, className }: { children: ReactNode; className?: string }) => (
    <a href="#link" className={className}>
      {children}
    </a>
  ),
  Outlet: () => <EnvironmentPage />,
  getRouteApi: () => ({ useParams: () => ({ projectId: "p", environmentId: route.environmentId }) }),
  useParams: () => ({ environmentId: route.environmentId }),
  useNavigate: () => () => undefined,
}));
vi.mock("../api/client", () => ({ request: vi.fn(), errorMessage: (error: unknown) => String(error) }));
vi.mock("../components/SharedVariables", () => ({ SharedVariables: ({ id }: { id: string }) => <section aria-label={`Shared variables ${id}`} /> }));
vi.mock("../components/DeploymentPolicy", () => ({ DeploymentPolicy: ({ id }: { id: string }) => <section aria-label={`Deployment policy ${id}`} /> }));
afterEach(() => vi.clearAllMocks());

const permissions = ["read", "operate", "edit"];
const project = {
  id: "p",
  name: "Shop",
  permissions,
  environments: [
    { id: "e1", name: "production", permissions },
    { id: "e2", name: "staging", permissions },
  ],
};
const workload = (id: string, environment: string) => ({ id, name: id, kind: "service", permissions, project_id: "p", environment_id: environment, node_id: "n", node: "One" });

it("replaces the environment sections instead of accumulating them when switching environments", async () => {
  vi.mocked(request).mockImplementation((_method: string, path: string) => {
    if (path === "/api/projects") return Promise.resolve([project]);
    if (path === "/api/workloads") return Promise.resolve([workload("web", "e1"), workload("db", "e1"), workload("api", "e2"), workload("cache", "e2")]);
    if (path === "/api/operations") return Promise.resolve([]);
    if (path === "/api/session") return Promise.resolve({ teams: [], is_admin: false });
    if (path === "/api/nodes") return Promise.resolve([]);
    if (path.endsWith("/suspension")) return Promise.resolve({ paused: false, phase: "active", resources: [] });
    if (path.startsWith("/api/projects/p/connections")) return Promise.resolve({ resources: [], connections: [], shared_networks: [] });
    return Promise.reject(new Error(`unexpected ${path}`));
  });
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const view = () => (
    <QueryClientProvider client={client}>
      <ProjectLayout />
    </QueryClientProvider>
  );
  const { rerender } = render(view());
  await screen.findByRole("heading", { name: "Resources in production" });

  for (const environmentId of ["e2", "e1", "e2", "e1", "e2"]) {
    route.environmentId = environmentId;
    rerender(view());
  }

  await waitFor(() => expect(screen.getAllByRole("heading", { name: /^Resources in / })).toHaveLength(1));
  expect(screen.getByRole("heading", { name: "Resources in staging" })).toBeInTheDocument();
  expect(screen.getAllByRole("region", { name: /^Shared variables / })).toHaveLength(1);
  expect(screen.getByRole("region", { name: "Shared variables e2" })).toBeInTheDocument();
  expect(screen.queryByRole("link", { name: "web" })).not.toBeInTheDocument();
  expect(screen.getByRole("link", { name: "api" })).toBeInTheDocument();
});

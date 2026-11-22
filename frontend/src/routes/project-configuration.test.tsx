import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen } from "@testing-library/react";
import type { ReactNode } from "react";
import { afterEach, expect, it, vi } from "vitest";
import { request } from "../api/client";
import type { Project } from "../api/projects";
import { ProjectConfiguration } from "./project-configuration";

const context = vi.hoisted(() => ({ project: null as unknown as Project }));

vi.mock("./project", () => ({ useProject: () => ({ project: context.project, resources: [] }) }));
vi.mock("../lib/session", () => ({ useSession: () => ({ email: "owner@example.test", teams: [], is_admin: false }) }));
vi.mock("../api/client", async (original) => ({ ...(await original<typeof import("../api/client")>()), request: vi.fn() }));
vi.mock("@tanstack/react-router", () => ({ Link: ({ children }: { children: ReactNode }) => <a href="#link">{children}</a> }));
afterEach(() => vi.clearAllMocks());

function setup(project: Project) {
  context.project = project;
  vi.mocked(request).mockImplementation((method: string, path: string) => {
    if (path.startsWith("/api/projects/p/configuration/compare")) return Promise.resolve({ from: "Production", to: "Staging", resources: [], variable_changes: [], policy_changed: false });
    return Promise.reject(new Error(`unexpected ${method} ${path}`));
  });
  render(
    <QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}>
      <ProjectConfiguration />
    </QueryClientProvider>,
  );
}

it("lets people with read access to two environments compare them without project read", async () => {
  setup({ id: "p", name: "Shop", permissions: [], environments: [{ id: "e1", name: "Production", permissions: ["read"] }, { id: "e2", name: "Staging", permissions: ["read"] }] });
  expect(await screen.findByText("Compare environments")).toBeInTheDocument();
  expect(screen.getByText(/need read access to the project/)).toBeInTheDocument();
  expect(screen.queryByText("Current configuration")).not.toBeInTheDocument();
  expect(screen.queryByText("GitOps")).not.toBeInTheDocument();
  expect(screen.queryByText(/Drafts/)).not.toBeInTheDocument();
  const paths = vi.mocked(request).mock.calls.map(([, path]) => path);
  expect(paths.length).toBeGreaterThan(0);
  expect(paths.every((path) => path.startsWith("/api/projects/p/configuration/compare"))).toBe(true);
});

it("keeps configuration closed to people who can read fewer than two environments and not the project", () => {
  setup({ id: "p", name: "Shop", permissions: [], environments: [{ id: "e1", name: "Production", permissions: ["read"] }, { id: "e2", name: "Staging", permissions: [] }] });
  expect(screen.getByText("Configuration needs read access")).toBeInTheDocument();
  expect(screen.queryByText("Compare environments")).not.toBeInTheDocument();
  expect(request).not.toHaveBeenCalled();
});

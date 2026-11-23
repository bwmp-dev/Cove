import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen } from "@testing-library/react";
import type { ReactNode } from "react";
import { afterEach, expect, it, vi } from "vitest";
import { request } from "../api/client";
import type { Project } from "../api/projects";
import { editWithoutSecrets, secretsWithoutEdit } from "../test/permissions";
import { ProjectConfiguration } from "./project-configuration";

const context = vi.hoisted(() => ({ project: null as unknown as Project }));

vi.mock("./project", () => ({ useProject: () => ({ project: context.project, resources: [] }) }));
vi.mock("../lib/session", () => ({ useSession: () => ({ email: "owner@example.test", teams: [], is_admin: false }) }));
vi.mock("../api/client", async (original) => ({ ...(await original<typeof import("../api/client")>()), request: vi.fn() }));
vi.mock("@tanstack/react-router", () => ({ Link: ({ children }: { children: ReactNode }) => <a href="#link">{children}</a>, useBlocker: () => ({ status: "idle" }) }));
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

it("offers an environment export to read_secrets on one environment without project read", () => {
  setup({ id: "p", name: "Shop", permissions: [], environments: [{ id: "e1", name: "Production", permissions: secretsWithoutEdit }, { id: "e2", name: "Staging", permissions: [] }] });
  expect(screen.getByText("Export with secrets")).toBeInTheDocument();
  expect(screen.getByText(/export the environments whose secrets you can read/)).toBeInTheDocument();
  expect(screen.queryByText("Compare environments")).not.toBeInTheDocument();
  expect(screen.queryByText("Configuration needs read access")).not.toBeInTheDocument();
  expect(request).not.toHaveBeenCalled();
});

it("hides export from a project editor without read_secrets and explains why", () => {
  setup({ id: "p", name: "Shop", permissions: editWithoutSecrets, environments: [{ id: "e1", name: "Production", permissions: editWithoutSecrets }] });
  expect(screen.queryByText("Export with secrets")).not.toBeInTheDocument();
  expect(screen.getByText(/Exporting with secrets needs permission to read secrets/)).toBeInTheDocument();
  expect(screen.queryByText(/need editor access/)).not.toBeInTheDocument();
});

it("lets read_secrets without edit export while drafts and GitOps changes stay with editors", () => {
  setup({ id: "p", name: "Shop", permissions: secretsWithoutEdit, environments: [{ id: "e1", name: "Production", permissions: secretsWithoutEdit }] });
  expect(screen.getByText("Export with secrets")).toBeInTheDocument();
  expect(screen.getByText(/Saving drafts, importing and changing GitOps settings need editor access to the project\./)).toBeInTheDocument();
});

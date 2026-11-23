import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { ReactNode } from "react";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { request } from "../api/client";
import type { Project } from "../api/projects";
import type { ResourceDraft } from "../api/templates";
import type { Node, WorkloadInput } from "../api/types";
import { WorkloadForm, type ProviderLink } from "./WorkloadForm";

vi.mock("../api/client", () => ({ request: vi.fn(), errorMessage: String }));
vi.mock("@tanstack/react-router", () => ({ Link: ({ children }: { children: ReactNode }) => <a href="#link">{children}</a> }));
beforeEach(() => vi.stubGlobal("ResizeObserver", class { observe = vi.fn(); disconnect = vi.fn(); }));
afterEach(() => {
  vi.clearAllMocks();
  vi.unstubAllGlobals();
});

it("creates a Git resource from a provider repository and hands back the connection to save", async () => {
  const user = userEvent.setup();
  vi.mocked(request).mockImplementation((_method: string, path: string) => {
    if (path === "/api/git/connections") return Promise.resolve([{ id: 3, name: "Codeberg", kind: "gitea", url: "https://codeberg.org/api/v1", workspace: "", webhook_url: "https://cove.example/hooks/git/3" }]);
    if (path.endsWith("/repositories")) return Promise.resolve([{ id: 9, full_name: "acme/site", clone_url: "https://codeberg.org/acme/site.git", default_branch: "trunk", installation_id: 3 }]);
    if (path.endsWith("/branches")) return Promise.resolve([{ ref: "refs/heads/trunk" }]);
    return Promise.reject(new Error(path));
  });
  const onSubmit = vi.fn<(input: WorkloadInput, provider?: ProviderLink) => void>();
  render(
    <QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}>
      <WorkloadForm
        nodes={[{ id: "n1", name: "node", address: "10.0.0.1", port: 22, state: "ready", capabilities: null, mode: "ssh", last_checked_at: null }]}
        projects={[{ id: "p1", name: "Site", permissions: ["read", "operate", "edit", "view", "read_secrets", "edit_configuration", "manage_backups"], environments: [] }]}
        initialProjectId="p1"
        initialEnvironmentId=""
        pending={false}
        serverError={null}
        submitLabel="Create resource"
        cancel={null}
        onSubmit={onSubmit}
      />
    </QueryClientProvider>,
  );

  await user.type(screen.getByLabelText("Name"), "site");
  await user.click(screen.getByRole("combobox", { name: "Node" }));
  await user.click(screen.getByRole("option", { name: "node" }));
  await user.click(screen.getByRole("radio", { name: /Git repository/ }));
  await user.click(screen.getByRole("radio", { name: /Git provider/ }));
  expect(screen.queryByLabelText("Repository URL")).not.toBeInTheDocument();
  expect(screen.queryByLabelText("Access token")).not.toBeInTheDocument();

  await user.click(screen.getByRole("button", { name: "Create resource" }));
  expect(screen.getByText("Select a connection, repository and branch.")).toBeInTheDocument();
  expect(onSubmit).not.toHaveBeenCalled();

  await user.click(await screen.findByRole("combobox", { name: "Connection" }));
  await user.click(screen.getByRole("option", { name: "Codeberg (Gitea or Forgejo)" }));
  await user.click(await screen.findByRole("combobox", { name: "Repository" }));
  await user.click(screen.getByRole("option", { name: "acme/site" }));
  await waitFor(() => expect(screen.getByRole("combobox", { name: "Branch" })).toBeEnabled());
  await user.click(screen.getByRole("switch", { name: "Deploy pushes automatically" }));
  await user.click(screen.getByRole("button", { name: "Create resource" }));

  expect(onSubmit).toHaveBeenCalledOnce();
  const [input, provider] = onSubmit.mock.calls[0] ?? [];
  expect(input?.project_id).toBe("p1");
  expect(input?.source).toMatchObject({ repository: "https://codeberg.org/acme/site.git", branch: "trunk", compose_path: "compose.yaml" });
  expect(input?.source).not.toHaveProperty("token");
  expect(provider).toEqual({ connection_id: 3, repository_id: 9, automatic: false, reporting: false });
});

const node: Node = { id: "n1", name: "worker", address: "10.0.0.2", port: 22, state: "ready", capabilities: null, mode: "ssh", last_checked_at: null };
const project: Project = { id: "p1", name: "Apps", permissions: ["read", "operate", "edit", "view", "read_secrets", "edit_configuration", "manage_backups"], environments: [] };
const draft: ResourceDraft = {
  name: "garage",
  compose: "services:\n  garage:\n    image: dxflrs/garage:v2.5.0\n",
  environment: { RPC_SECRET: "a".repeat(64) },
  routes: [],
  file_secrets: [{ name: "rpc", variable: "RPC_SECRET", services: ["garage"], build_services: [] }],
};

it("keeps template file secrets in the create request", async () => {
  const user = userEvent.setup();
  const onSubmit = vi.fn();
  render(
    <QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}>
      <WorkloadForm draft={draft} nodes={[node]} projects={[project]} initialProjectId="p1" initialEnvironmentId="" pending={false} serverError={null} submitLabel="Create resource" cancel={null} onSubmit={onSubmit} />
    </QueryClientProvider>,
  );
  expect(screen.getByText(/mounts these variables as files/)).toBeInTheDocument();
  await user.click(screen.getByRole("combobox", { name: "Node" }));
  await user.click(screen.getByRole("option", { name: "worker" }));
  await user.click(screen.getByRole("button", { name: "Create resource" }));
  await waitFor(() => expect(onSubmit).toHaveBeenCalledTimes(1));
  expect(onSubmit.mock.calls[0]?.[0]).toMatchObject({ name: "garage", node_id: "n1", project_id: "p1", file_secrets: draft.file_secrets, environment: draft.environment });
});

import { QueryClient, QueryClientProvider, useQuery } from "@tanstack/react-query";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { ApiError, request } from "../api/client";
import type { ImportDiscovery } from "../api/migration-imports";
import { projectConfigurationQuery } from "../api/project-configuration";
import type { Project } from "../api/projects";
import type { Node } from "../api/types";
import { CoolifyImport } from "./CoolifyImport";

vi.mock("../api/client", async (original) => ({ ...(await original<typeof import("../api/client")>()), request: vi.fn() }));
vi.mock("@tanstack/react-router", () => ({ useBlocker: () => ({ status: "idle" }) }));
beforeEach(() => vi.stubGlobal("ResizeObserver", class { observe = vi.fn(); disconnect = vi.fn(); }));
afterEach(() => {
  vi.clearAllMocks();
  vi.unstubAllGlobals();
});
HTMLDialogElement.prototype.showModal = function (this: HTMLDialogElement) {
  this.open = true;
};
HTMLDialogElement.prototype.close = function (this: HTMLDialogElement) {
  this.open = false;
};

const project: Project = { id: "p", name: "Shop", permissions: ["read", "edit"], environments: [{ id: "e1", name: "Production", permissions: ["read", "edit"] }] };
const capabilities = { protocol: 1, os: "linux", arch: "amd64", docker: "27", compose: "2", ready: true };
const nodes: Node[] = [
  { id: "n1", name: "edge-1", address: "", port: 22, state: "ready", mode: "ssh", last_checked_at: null, capabilities: { ...capabilities, imports: true } },
  { id: "n2", name: "old-1", address: "", port: 22, state: "ready", mode: "ssh", last_checked_at: null, capabilities },
];
const items = [
  { kind: "service", resource: { uuid: "svc-1", name: "web", docker_compose_raw: "services: {}" }, environment_variables: [{ key: "TOKEN", value: "hunter2" }] },
  { kind: "application", resource: { uuid: "app-1", name: "api", build_pack: "nixpacks" } },
];
const envelope = JSON.stringify({ items });
const discovery: ImportDiscovery = {
  id: "imp1",
  candidates: [
    { id: "svc-1", name: "web", kind: "service", blockers: [] },
    { id: "app-1", name: "api", kind: "application", blockers: ["Only Docker Compose applications and services are supported."] },
  ],
  warnings: ["Import does not copy data or change DNS."],
};
const dryRun = { draft_id: "d9", base_version: "v1", changes: [{ resource: "web-2", action: "create", fields: ["compose", "environment"], diffs: [{ field: "node_id", before: "", after: '"n1"' }] }], warnings: ["Verify the source manager's automation is paused."] };

function Harness() {
  const configuration = useQuery(projectConfigurationQuery("p"));
  return <CoolifyImport project={project} configuration={configuration} online />;
}

function setup(handlers: Record<string, (body?: unknown) => Promise<unknown>> = {}) {
  vi.mocked(request).mockImplementation((method: string, path: string, body?: unknown) => {
    const key = `${method} ${path}`;
    if (handlers[key]) return handlers[key](body);
    if (key === "GET /api/projects/p/configuration") return Promise.resolve({ version: "v1", manifest: { api_version: "cove/v1", project: "Shop", resources: [{ key: "web", configuration: { name: "web" } }] }, resource_ids: {} });
    if (key === "GET /api/projects/p/imports") return Promise.resolve([]);
    if (key === "GET /api/nodes") return Promise.resolve(nodes);
    if (key === "POST /api/projects/p/imports/coolify") return Promise.resolve(discovery);
    if (key === "POST /api/projects/p/imports/imp1/dry-run") return Promise.resolve(dryRun);
    if (key === "POST /api/projects/p/configuration/drafts/d9/apply") return Promise.resolve({ id: "d9", operation_ids: null, state: "saved" });
    if (key === "GET /api/projects/p/configuration/drafts") return Promise.resolve([]);
    return Promise.reject(new Error(key));
  });
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(
    <QueryClientProvider client={client}>
      <Harness />
    </QueryClientProvider>,
  );
  return client;
}

function cacheText(client: QueryClient) {
  return JSON.stringify([client.getQueryCache().getAll().map((query) => query.state.data), client.getMutationCache().getAll().map((mutation) => [mutation.state.variables, mutation.state.data])]);
}

async function discover(user: ReturnType<typeof userEvent.setup>) {
  await user.click(screen.getByLabelText("Export envelope"));
  await user.paste(envelope);
  await user.click(screen.getByRole("button", { name: "Discover resources" }));
  return screen.findByRole("list", { name: "Discovered resources" });
}

it("discovers, maps, dry-runs and applies an import without keeping source secrets", async () => {
  const user = userEvent.setup();
  const client = setup();
  expect(await screen.findByText(/No active imports/)).toBeInTheDocument();
  const list = await discover(user);

  expect(request).toHaveBeenCalledWith("POST", "/api/projects/p/imports/coolify", { items });
  expect(screen.getByLabelText("Export envelope")).toHaveValue("");
  expect(within(list).getByText(/Only Docker Compose applications/)).toBeInTheDocument();
  expect(within(list).queryByRole("checkbox", { name: "Import api" })).not.toBeInTheDocument();
  expect(within(list).getByRole("checkbox", { name: "Import web" })).toBeChecked();
  expect(within(list).getByLabelText("Resource key")).toHaveValue("web-2");
  expect(screen.getByText("Import does not copy data or change DNS.")).toBeInTheDocument();

  await user.click(screen.getByRole("button", { name: "Run dry-run" }));
  expect(await screen.findByText("Choose a node.")).toBeInTheDocument();
  await user.click(within(list).getByRole("combobox", { name: "Node" }));
  expect(screen.getByRole("option", { name: "old-1 (needs worker upgrade)" })).toHaveAttribute("aria-disabled", "true");
  await user.click(screen.getByRole("option", { name: "edge-1" }));
  await user.click(screen.getByRole("button", { name: "Run dry-run" }));

  expect(await screen.findByRole("list", { name: "Changes in the import draft" })).toBeInTheDocument();
  expect(request).toHaveBeenCalledWith("POST", "/api/projects/p/imports/imp1/dry-run", { mappings: [{ source_id: "svc-1", key: "web-2", name: "web", environment: "", node_id: "n1" }] });
  expect(screen.getByText("Verify the source manager's automation is paused.")).toBeInTheDocument();
  const apply = screen.getByRole("button", { name: "Apply import draft" });
  expect(apply).toBeDisabled();
  await user.click(screen.getByRole("checkbox", { name: /only saves new configuration/ }));
  await user.click(apply);

  expect(await screen.findByText("New configuration saved")).toBeInTheDocument();
  expect(screen.getByText(/Nothing was deployed/)).toBeInTheDocument();
  expect(request).toHaveBeenCalledWith("POST", "/api/projects/p/configuration/drafts/d9/apply");
  expect(cacheText(client)).not.toContain("hunter2");
});

it("keeps the envelope for correction when discovery is refused and validates it locally first", async () => {
  const user = userEvent.setup();
  setup({ "POST /api/projects/p/imports/coolify": () => Promise.reject(new ApiError(400, "exports require unique resource UUIDs")) });
  await user.click(await screen.findByLabelText("Export envelope"));
  await user.paste("{");
  await user.click(screen.getByRole("button", { name: "Discover resources" }));
  expect(screen.getByText("The envelope is not valid JSON.")).toBeInTheDocument();
  expect(request).not.toHaveBeenCalledWith("POST", "/api/projects/p/imports/coolify", expect.anything());

  await user.clear(screen.getByLabelText("Export envelope"));
  await user.paste(envelope);
  await user.click(screen.getByRole("button", { name: "Discover resources" }));
  expect(await screen.findByText("exports require unique resource UUIDs")).toBeInTheDocument();
  expect(screen.getByLabelText("Export envelope")).toHaveValue(envelope);
});

it("explains a refused dry-run and lets a reviewed draft be discarded", async () => {
  const user = userEvent.setup();
  let attempts = 0;
  const removeDraft = vi.fn(() => Promise.resolve({ ok: true }));
  setup({
    "POST /api/projects/p/imports/imp1/dry-run": () => (attempts++ === 0 ? Promise.reject(new ApiError(409, "upgrade and verify the selected worker before importing")) : Promise.resolve(dryRun)),
    "DELETE /api/projects/p/configuration/drafts/d9": removeDraft,
  });
  const list = await discover(user);
  await user.click(within(list).getByRole("combobox", { name: "Node" }));
  await user.click(screen.getByRole("option", { name: "edge-1" }));
  await user.click(screen.getByRole("button", { name: "Run dry-run" }));
  expect(await screen.findByText(/upgrade and verify the selected worker/)).toBeInTheDocument();
  expect(screen.getByText(/Nothing was staged/)).toBeInTheDocument();

  await user.click(screen.getByRole("button", { name: "Run dry-run" }));
  expect(within(list).getByLabelText("Resource key")).toBeDisabled();
  await user.click(await screen.findByRole("button", { name: "Discard and edit mapping" }));
  await waitFor(() => expect(removeDraft).toHaveBeenCalled());
  expect(await screen.findByRole("button", { name: "Run dry-run" })).toBeInTheDocument();
  expect(within(list).getByLabelText("Resource key")).toBeEnabled();
});

it("resumes a saved import and reports an expired one", async () => {
  const user = userEvent.setup();
  setup({
    "GET /api/projects/p/imports": () => Promise.resolve([{ id: "old", created_at: "2026-11-22T08:00:00Z", expires_at: "2026-11-23T08:00:00Z" }]),
    "GET /api/projects/p/imports/old": () => Promise.reject(new ApiError(404, "Not Found")),
  });
  const sessions = await screen.findByRole("list", { name: "Discovered imports" });
  await user.click(within(sessions).getByRole("button", { name: "Review" }));
  expect(await screen.findByText("This import is no longer available")).toBeInTheDocument();
});

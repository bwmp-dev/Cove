import { QueryClient, QueryClientProvider, useQuery } from "@tanstack/react-query";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { ReactNode } from "react";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { ApiError, request } from "../api/client";
import type { Project } from "../api/projects";
import { projectConfigurationQuery, type GitOpsStatus, type ProjectConfiguration } from "../api/project-configuration";
import type { WorkloadSummary } from "../api/types";
import { GitOpsSource } from "./GitOpsSource";

vi.mock("../api/client", async (original) => ({ ...(await original<typeof import("../api/client")>()), request: vi.fn() }));
vi.mock("@tanstack/react-router", () => ({ Link: ({ children }: { children: ReactNode }) => <a href="#link">{children}</a> }));
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

const permissions: Project["permissions"] = ["read", "operate", "edit"];
const project: Project = { id: "p", name: "Shop", permissions, environments: [{ id: "e1", name: "Production", permissions }] };
const resources: WorkloadSummary[] = [{ id: "w1", name: "web", project_id: "p", environment_id: "e1", environment: "Production", node_id: "n", node: "One", kind: "service", permissions }];
const connected: GitOpsStatus = { connected: true, source: { workload_id: "w1", path: "cove.yaml", automatic: false }, last_revision: "0123456789abcdef0123", last_error: "", draft_id: "", synced_at: "2026-11-22T09:00:00Z", drift: [{ resource: "web", action: "update", fields: ["limits"], diffs: null }] };

const configuration: ProjectConfiguration = { version: "v1", manifest: { api_version: "cove/v1", project: "Shop", resources: [{ key: "web", environment: "Production", configuration: { name: "web", source: { repository: "r" } } }] }, resource_ids: { web: "w1" } };

function Harness(props: { editable: boolean; individualAccount: boolean; resources: WorkloadSummary[] }) {
  const query = useQuery(projectConfigurationQuery("p"));
  return <GitOpsSource project={project} resources={props.resources} configuration={query} editable={props.editable} online individualAccount={props.individualAccount} />;
}

function setup(status: GitOpsStatus, options: { editable?: boolean; individualAccount?: boolean; resources?: WorkloadSummary[]; handlers?: Record<string, (body?: unknown) => Promise<unknown>> } = {}) {
  vi.mocked(request).mockImplementation((method: string, path: string, body?: unknown) => {
    const key = `${method} ${path}`;
    if (options.handlers?.[key]) return options.handlers[key](body);
    if (key === "GET /api/projects/p/configuration/gitops") return Promise.resolve(status);
    if (key === "GET /api/projects/p/configuration") return Promise.resolve(configuration);
    return Promise.reject(new Error(key));
  });
  render(
    <QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}>
      <Harness editable={options.editable ?? true} individualAccount={options.individualAccount ?? true} resources={options.resources ?? resources} />
    </QueryClientProvider>,
  );
}

it("connects a source with automatic apply off unless explicitly enabled", async () => {
  const user = userEvent.setup();
  const save = vi.fn((body?: unknown) => Promise.resolve({ ...connected, source: body }));
  setup({ connected: false }, { handlers: { "PUT /api/projects/p/configuration/gitops": save } });
  const automatic = await screen.findByRole("switch", { name: "Apply changes automatically" });
  expect(automatic).not.toBeChecked();
  expect(screen.getByLabelText("Path in repository")).toHaveValue("cove.yaml");
  await user.click(screen.getByRole("combobox", { name: "Repository of" }));
  await user.click(screen.getByRole("option", { name: "web (Production)" }));
  await user.click(screen.getByRole("button", { name: "Connect" }));
  await waitFor(() => expect(save).toHaveBeenCalledWith({ workload_id: "w1", path: "cove.yaml", automatic: false }));
  expect(await screen.findByText("Manual review")).toBeInTheDocument();
});

it("does not offer automatic apply to shared administrator sessions", async () => {
  setup({ connected: false }, { individualAccount: false });
  expect(await screen.findByRole("switch", { name: "Apply changes automatically" })).toBeDisabled();
  expect(screen.getByText(/individual account with a verified email/)).toBeInTheDocument();
});

it("shows sync state read-only to viewers", async () => {
  setup(connected, { editable: false });
  expect(await screen.findByText("Manual review")).toBeInTheDocument();
  expect(screen.getByText("0123456")).toBeInTheDocument();
  expect(screen.getByRole("list", { name: "Differences from cove.yaml" })).toHaveTextContent("limits");
  expect(screen.queryByRole("button", { name: "Sync cove.yaml" })).not.toBeInTheDocument();
  expect(screen.queryByRole("button", { name: "Disconnect" })).not.toBeInTheDocument();
});

it("stages a pending draft on sync and reports failures", async () => {
  const user = userEvent.setup();
  const sync = vi
    .fn()
    .mockResolvedValueOnce({ state: "pending", draft_id: "d1", changes: [{ resource: "web", action: "update", fields: ["routes"], diffs: null }], revision: "fedcba9876543210" })
    .mockRejectedValueOnce(new ApiError(502, "The repository could not be fetched."));
  setup(connected, { handlers: { "POST /api/projects/p/configuration/gitops/sync": sync } });
  await user.click(await screen.findByRole("button", { name: "Sync cove.yaml" }));
  expect(await screen.findByText("Draft staged")).toBeInTheDocument();
  expect(screen.getByRole("list", { name: "Changes from cove.yaml" })).toHaveTextContent("routes");
  await user.click(screen.getByRole("button", { name: "Sync cove.yaml" }));
  expect(await screen.findByText("The repository could not be fetched.")).toBeInTheDocument();
});

it("disconnects after confirming that resources stay unchanged", async () => {
  const user = userEvent.setup();
  const remove = vi.fn(() => Promise.resolve({ ok: true }));
  setup(connected, { handlers: { "DELETE /api/projects/p/configuration/gitops": remove } });
  await user.click(await screen.findByRole("button", { name: "Disconnect" }));
  const dialog = screen.getByRole("dialog", { name: "Disconnect GitOps?" });
  expect(within(dialog).getByText(/resources stay as they are/)).toBeInTheDocument();
  await user.click(within(dialog).getByRole("button", { name: "Disconnect" }));
  await waitFor(() => expect(remove).toHaveBeenCalled());
});

it("offers only the resource the manifest maps to a Git source, even when names repeat", async () => {
  const user = userEvent.setup();
  const twin: WorkloadSummary = { ...resources[0]!, id: "w2" };
  setup({ connected: false }, { resources: [twin, ...resources] });
  await user.click(await screen.findByRole("combobox", { name: "Repository of" }));
  expect(screen.getAllByRole("option", { name: "web (Production)" })).toHaveLength(1);
  await user.click(screen.getByRole("option", { name: "web (Production)" }));
  expect(screen.getByRole("combobox", { name: "Repository of" })).toHaveTextContent("web (Production)");
  expect(document.querySelector<HTMLSelectElement>("select")?.value).toBe("w1");
});

it("offers no Git resources while the configuration is loading or unavailable", async () => {
  let fail: (error: Error) => void = () => undefined;
  setup({ connected: false }, { handlers: { "GET /api/projects/p/configuration": () => new Promise((_, reject) => (fail = reject)) } });
  expect(await screen.findByText("Loading the resources with a Git source…")).toBeInTheDocument();
  expect(screen.queryByRole("combobox", { name: "Repository of" })).not.toBeInTheDocument();
  expect(screen.getByRole("button", { name: "Connect" })).toBeDisabled();
  fail(new ApiError(503, "Configuration storage is unavailable."));
  expect(await screen.findByText("Could not load the resources with a Git source")).toBeInTheDocument();
  expect(screen.queryByRole("combobox", { name: "Repository of" })).not.toBeInTheDocument();
  expect(screen.queryByText("No Git resource to sync from")).not.toBeInTheDocument();
  expect(screen.getByRole("button", { name: "Connect" })).toBeDisabled();
});

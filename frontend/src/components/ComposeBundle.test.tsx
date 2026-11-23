import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, expect, it, vi } from "vitest";
import { request } from "../api/client";
import type { ComposeBundleSummary } from "../api/compose-bundle";
import type { Workload } from "../api/types";
import { can } from "../lib/permissions";
import { editorAccess, editWithoutSecrets } from "../test/permissions";
import { ComposeBundleSection } from "./ComposeBundle";

vi.mock("../api/client", () => ({ request: vi.fn(), errorMessage: (error: unknown) => (error instanceof Error ? error.message : String(error)) }));
vi.mock("@tanstack/react-router", () => ({ useBlocker: () => ({ status: "idle" }) }));
afterEach(() => vi.clearAllMocks());
HTMLDialogElement.prototype.showModal = function (this: HTMLDialogElement) {
  this.open = true;
};
HTMLDialogElement.prototype.close = function (this: HTMLDialogElement) {
  this.open = false;
};

const secret = "DATABASE_PASSWORD=hunter2\n";
const inline: Workload = { id: "w1", name: "web", node_id: "n1", project_id: "p1", permissions: editorAccess, manually_stopped: false, deployed: false, compose: "services: {}", environment_keys: [], source: null, has_source_token: false };
const git: Workload = { ...inline, source: { repository: "https://github.com/acme/app.git", branch: "main", compose_path: "deploy/compose.yml" } };
const savedInline: ComposeBundleSummary = { enabled: true, available: true, files: ["production.yaml", "metrics.yaml"], assets: ["base.env"], paths: [], profiles: ["worker"] };
const exported = { bundle: { files: [{ path: "production.yaml", content: "services:\n  web: {}\n" }, { path: "metrics.yaml", content: "services:\n  metrics: {}\n" }], assets: { "base.env": secret }, profiles: ["worker"] } };

function setup(workload: Workload, summary: ComposeBundleSummary, options: { editable?: boolean; temporary?: boolean; blocked?: boolean; put?: () => Promise<unknown> } = {}) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  vi.mocked(request).mockImplementation((method: string, path: string, body?: unknown) => {
    if (method === "GET" && path === "/api/workloads/w1/compose-bundle") return Promise.resolve(summary);
    if (method === "POST" && path === "/api/workloads/w1/compose-bundle/export") return Promise.resolve(exported);
    if (method === "PUT" && path === "/api/workloads/w1/compose-bundle") {
      if (options.put) return options.put();
      const bundle = (body as { bundle: { files?: { path: string }[]; paths?: string[]; profiles?: string[]; assets?: Record<string, string> } | null }).bundle;
      return Promise.resolve({ enabled: bundle !== null, available: true, files: bundle?.files?.map((file) => file.path) ?? [], assets: Object.keys(bundle?.assets ?? {}), paths: bundle?.paths ?? [], profiles: bundle?.profiles ?? [] });
    }
    return Promise.reject(new Error(`${method} ${path}`));
  });
  const view = render(
    <QueryClientProvider client={client}>
      <ComposeBundleSection workload={workload} editable={options.editable ?? true} readable={can(workload.permissions, "read_secrets")} temporary={options.temporary ?? false} blocked={options.blocked ?? false} blockers={null} />
    </QueryClientProvider>,
  );
  return { client, ...view };
}

function cacheText(client: QueryClient) {
  return JSON.stringify([client.getQueryCache().getAll().map((query) => query.state.data), client.getMutationCache().getAll().map((mutation) => [mutation.state.variables, mutation.state.data])]);
}

it("shows saved metadata read-only to viewers without reading contents", async () => {
  setup(inline, savedInline, { editable: false });
  expect(await screen.findByText("production.yaml")).toBeInTheDocument();
  expect(screen.getByText("compose.yaml")).toBeInTheDocument();
  expect(screen.getByText("base.env")).toBeInTheDocument();
  expect(screen.getByText("worker")).toBeInTheDocument();
  expect(screen.queryByRole("button", { name: /files and profiles/ })).not.toBeInTheDocument();
  expect(screen.queryByRole("button", { name: "Clear selection" })).not.toBeInTheDocument();
  expect(request).toHaveBeenCalledTimes(1);
});

it("does not query preview resources and explains where to edit", () => {
  setup(inline, savedInline, { temporary: true });
  expect(screen.getByText(/Preview resources deploy the single Compose file/)).toBeInTheDocument();
  expect(request).not.toHaveBeenCalled();
});

it("loads saved contents only after confirmation, saves the new order and leaves no contents cached", async () => {
  const user = userEvent.setup();
  const { client } = setup(inline, savedInline);
  await user.click(await screen.findByRole("button", { name: "Edit files and profiles" }));
  expect(screen.getByRole("dialog", { name: "Load saved Compose files?" })).toBeInTheDocument();
  expect(request).toHaveBeenCalledTimes(1);
  await user.click(screen.getByRole("button", { name: "Load saved files" }));
  expect(request).toHaveBeenCalledWith("POST", "/api/workloads/w1/compose-bundle/export");

  expect(await screen.findByLabelText("Contents of ancillary file 1")).toHaveValue(secret);
  await user.click(screen.getByRole("button", { name: "Move production.yaml down" }));
  expect(screen.getByText("Moved production.yaml to position 3.")).toBeInTheDocument();
  expect(screen.getByLabelText("Path of override 2")).toHaveValue("metrics.yaml");
  await user.click(screen.getByRole("button", { name: "Save files and profiles" }));

  await waitFor(() =>
    expect(request).toHaveBeenCalledWith("PUT", "/api/workloads/w1/compose-bundle", {
      bundle: {
        files: [exported.bundle.files[1], exported.bundle.files[0]],
        assets: { "base.env": secret },
        paths: [],
        profiles: ["worker"],
      },
    }),
  );
  expect(await screen.findByText("Saved. Changes apply on the next deploy.")).toBeInTheDocument();
  expect(screen.getAllByText(/^(metrics|production)\.yaml$/).map((node) => node.textContent)).toEqual(["metrics.yaml", "production.yaml"]);
  expect(screen.queryByText(/hunter2/)).not.toBeInTheDocument();
  await waitFor(() => expect(client.getMutationCache().getAll()).toHaveLength(0));
  expect(cacheText(client)).not.toContain("hunter2");
});

it("clears loaded contents when editing is cancelled", async () => {
  const user = userEvent.setup();
  const { client } = setup(inline, savedInline);
  await user.click(await screen.findByRole("button", { name: "Edit files and profiles" }));
  await user.click(screen.getByRole("button", { name: "Load saved files" }));
  expect(await screen.findByLabelText("Contents of ancillary file 1")).toHaveValue(secret);
  await user.click(screen.getByRole("button", { name: "Cancel" }));

  expect(screen.queryByLabelText("Contents of ancillary file 1")).not.toBeInTheDocument();
  await waitFor(() => expect(client.getMutationCache().getAll()).toHaveLength(0));
  expect(cacheText(client)).not.toContain("hunter2");
  await user.click(screen.getByRole("button", { name: "Edit files and profiles" }));
  expect(screen.getByRole("dialog", { name: "Load saved Compose files?" })).toBeInTheDocument();
});

it("replaces saved files without reading them only with a warning", async () => {
  const user = userEvent.setup();
  setup(inline, savedInline);
  await user.click(await screen.findByRole("button", { name: "Edit files and profiles" }));
  await user.click(screen.getByRole("button", { name: "Start over" }));
  expect(screen.getByText("Saved files were not loaded")).toBeInTheDocument();
  expect(screen.queryByLabelText(/Path of override/)).not.toBeInTheDocument();
  await user.click(screen.getByRole("button", { name: "Add override file" }));
  await user.type(screen.getByLabelText("Path of override 2"), "compose.yaml");
  await user.click(screen.getByLabelText("Contents of override 2"));
  await user.paste("services: {}");
  await user.click(screen.getByRole("button", { name: "Save files and profiles" }));
  expect(screen.getByText(/compose.yaml is the primary Compose file/)).toBeInTheDocument();
  await waitFor(() => expect(screen.getByLabelText("Path of override 2")).toHaveFocus());
  expect(request).toHaveBeenCalledTimes(1);

  await user.clear(screen.getByLabelText("Path of override 2"));
  await user.type(screen.getByLabelText("Path of override 2"), "prod.yaml");
  await user.click(screen.getByRole("button", { name: "Save files and profiles" }));
  await waitFor(() => expect(request).toHaveBeenCalledWith("PUT", "/api/workloads/w1/compose-bundle", { bundle: { files: [{ path: "prod.yaml", content: "services: {}" }], assets: {}, paths: [], profiles: ["worker"] } }));
  expect(request).not.toHaveBeenCalledWith("POST", expect.anything());
});

it("lets an editor without read_secrets start over but never load saved contents", async () => {
  const user = userEvent.setup();
  const { client } = setup({ ...inline, permissions: editWithoutSecrets }, savedInline);
  await user.click(await screen.findByRole("button", { name: "Edit files and profiles" }));
  const dialog = screen.getByRole("dialog", { name: "Replace saved Compose files?" });
  expect(within(dialog).queryByRole("button", { name: "Load saved files" })).not.toBeInTheDocument();
  expect(within(dialog).getByText(/your access does not include reading them/)).toBeInTheDocument();
  await user.click(within(dialog).getByRole("button", { name: "Start over" }));
  expect(screen.getByText("Saved files were not loaded")).toBeInTheDocument();
  expect(request).not.toHaveBeenCalledWith("POST", "/api/workloads/w1/compose-bundle/export");
  expect(cacheText(client)).not.toContain("hunter2");
});

it("keeps unsaved edits and shows the server reason when saving is refused", async () => {
  const user = userEvent.setup();
  setup(git, { enabled: false, available: true, files: [], assets: [], paths: [], profiles: [] }, { put: () => Promise.reject(new Error("Disable pull-request previews before selecting Compose files or profiles")) });
  await user.click(await screen.findByRole("button", { name: "Select files and profiles" }));
  expect(screen.queryByText(/Contents of/)).not.toBeInTheDocument();
  await user.click(screen.getByRole("button", { name: "Add override path" }));
  await user.type(screen.getByLabelText("Path of override 2"), "deploy/prod.yml");
  await user.type(screen.getByLabelText(/Profiles/), "worker");
  await user.click(screen.getByRole("button", { name: "Save files and profiles" }));

  expect(await screen.findByText("Disable pull-request previews before selecting Compose files or profiles")).toBeInTheDocument();
  expect(request).toHaveBeenCalledWith("PUT", "/api/workloads/w1/compose-bundle", { bundle: { files: [], assets: {}, paths: ["deploy/prod.yml"], profiles: ["worker"] } });
  expect(screen.getByLabelText("Path of override 2")).toHaveValue("deploy/prod.yml");
});

it("offers profiles only for Compose managed in Cove", async () => {
  const user = userEvent.setup();
  setup({ ...git, source: { ...git.source!, inline_compose: true } }, { enabled: true, available: true, files: [], assets: [], paths: [], profiles: ["worker"] });
  await user.click(await screen.findByRole("button", { name: "Edit files and profiles" }));
  expect(screen.queryByRole("button", { name: /Add override/ })).not.toBeInTheDocument();
  expect(screen.getByLabelText(/Profiles/)).toHaveValue("worker");
});

it("blocks selecting on old workers but still lets editors clear after confirming", async () => {
  const user = userEvent.setup();
  setup(git, { enabled: true, available: false, files: [], assets: [], paths: ["deploy/prod.yml"], profiles: [] });
  expect(await screen.findByText("Upgrade the worker to select files and profiles")).toBeInTheDocument();
  expect(screen.getByRole("button", { name: "Edit files and profiles" })).toBeDisabled();
  await user.click(screen.getByRole("button", { name: "Clear selection" }));
  const dialog = screen.getByRole("dialog", { name: "Clear Compose files and profiles?" });
  expect(request).toHaveBeenCalledTimes(1);
  await user.click(within(dialog).getByRole("button", { name: "Clear selection" }));
  await waitFor(() => expect(request).toHaveBeenCalledWith("PUT", "/api/workloads/w1/compose-bundle", { bundle: null }));
  expect(await screen.findByText(/Selection cleared/)).toBeInTheDocument();
  expect(screen.queryByRole("button", { name: "Clear selection" })).not.toBeInTheDocument();
});

it("disables changes while an operation is active or offline", async () => {
  setup(inline, savedInline, { blocked: true });
  expect(await screen.findByRole("button", { name: "Edit files and profiles" })).toBeDisabled();
  expect(screen.getByRole("button", { name: "Clear selection" })).toBeDisabled();
});

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { ReactNode } from "react";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import type { BuildNodeSettings } from "../api/build-nodes";
import { ApiError, request } from "../api/client";
import type { Operation, Permission, Workload } from "../api/types";
import { BuildNodeSetting } from "./BuildNode";

vi.mock("../api/client", async (original) => ({ ...(await original<typeof import("../api/client")>()), request: vi.fn() }));
vi.mock("./SaveBar", async (original) => ({ ...(await original<typeof import("./SaveBar")>()), UnsavedChangesGuard: () => null }));
vi.mock("@tanstack/react-router", () => ({ Link: ({ children }: { children: ReactNode }) => <a href="/">{children}</a> }));

beforeEach(() => vi.stubGlobal("ResizeObserver", class { observe = vi.fn(); disconnect = vi.fn(); }));
afterEach(() => {
  vi.clearAllMocks();
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
  window.dispatchEvent(new Event("online"));
});

const editor: Permission[] = ["read", "operate", "edit"];
const workload = (permissions: Permission[] = editor) => ({ id: "w1", name: "app", node_id: "n1", kind: "service", permissions, manually_stopped: false, deployed: true }) as Workload;

const runtime = { id: "n1", name: "runtime-1", eligible: true, reason: "" };
const builder = { id: "n2", name: "builder-2", eligible: true, reason: "" };
const arm = { id: "n3", name: "arm-3", eligible: false, reason: "build and runtime nodes must report the same Linux architecture" };

function settings(overrides: Partial<BuildNodeSettings> = {}): BuildNodeSettings {
  return { node_id: "", runtime_node_id: "n1", effective_node_id: "n1", nodes: [runtime, builder, arm], ...overrides };
}

function running(): Operation {
  return { id: "op1", workload_id: "w1", action: "deploy", state: "running", created_at: "2026-11-22T10:00:00Z", result: null };
}

interface Serve {
  read?: () => Promise<unknown>;
  operations?: () => Promise<unknown>;
  save?: () => Promise<unknown>;
}

function serve({ read = () => Promise.resolve(settings()), operations = () => Promise.resolve([]), save = () => Promise.resolve({ ok: true }) }: Serve = {}) {
  vi.mocked(request).mockImplementation((method: string, path: string) => {
    if (path === "/api/workloads/w1/build-node") return method === "PUT" ? save() : read();
    if (path.startsWith("/api/operations")) return operations();
    return Promise.reject(new Error(`Unexpected request ${method} ${path}`));
  });
}

function renderSetting(target = workload()) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(
    <QueryClientProvider client={client}>
      <BuildNodeSetting workload={target} />
    </QueryClientProvider>,
  );
  return client;
}

async function choose(user: ReturnType<typeof userEvent.setup>, name: string) {
  await user.click(screen.getByRole("combobox", { name: "Build node" }));
  await user.click(screen.getByRole("option", { name }));
}

const saveButton = () => screen.getByRole("button", { name: "Save build node" });

it("saves the exact node_id body and refreshes the setting and operations", async () => {
  const user = userEvent.setup();
  serve();
  const client = renderSetting();
  const invalidate = vi.spyOn(client, "invalidateQueries");
  const control = await screen.findByRole("combobox", { name: "Build node" });
  expect(control).toHaveTextContent("Runtime node (runtime-1)");
  await waitFor(() => expect(screen.getByRole("status")).toHaveTextContent("No unsaved changes"));

  await choose(user, "builder-2");
  expect(screen.getByRole("status")).toHaveTextContent("Unsaved changes");
  await user.click(saveButton());
  await waitFor(() => expect(request).toHaveBeenCalledWith("PUT", "/api/workloads/w1/build-node", { node_id: "n2" }));
  expect(await screen.findByText("Build node saved.")).toBeInTheDocument();
  expect(invalidate).toHaveBeenCalledWith({ queryKey: ["workloads", "w1", "build-node"], exact: true });
  expect(invalidate).toHaveBeenCalledWith({ queryKey: ["operations"] });
});

it("clears a designated node by saving an empty node_id", async () => {
  const user = userEvent.setup();
  serve({ read: () => Promise.resolve(settings({ node_id: "n2", effective_node_id: "n2" })) });
  renderSetting();
  expect(await screen.findByRole("combobox", { name: "Build node" })).toHaveTextContent("builder-2");
  await choose(user, "Runtime node (runtime-1)");
  await waitFor(() => expect(saveButton()).toBeEnabled());
  await user.click(saveButton());
  await waitFor(() => expect(request).toHaveBeenCalledWith("PUT", "/api/workloads/w1/build-node", { node_id: "" }));
});

it("offers only eligible nodes and explains the others", async () => {
  const user = userEvent.setup();
  serve();
  renderSetting();
  await user.click(await screen.findByRole("combobox", { name: "Build node" }));
  expect(screen.getByRole("option", { name: "arm-3 (unavailable)" })).toHaveAttribute("aria-disabled", "true");
  const reasons = screen.getByRole("list", { name: "Nodes that cannot build" });
  expect(within(reasons).getByText(/same Linux architecture/)).toBeInTheDocument();
});

it("says when no other node can build", async () => {
  serve({ read: () => Promise.resolve(settings({ nodes: [runtime, arm] })) });
  renderSetting();
  expect(await screen.findByText("No other node available to this team can build for this resource.")).toBeInTheDocument();
});

it("keeps a revoked saved node selected instead of resetting it", async () => {
  serve({ read: () => Promise.resolve(settings({ node_id: "n9", effective_node_id: "n9" })) });
  renderSetting();
  expect(await screen.findByRole("combobox", { name: "Build node" })).toHaveTextContent("n9 (no longer listed)");
  expect(screen.getByText("The saved build node n9 is unavailable")).toBeInTheDocument();
  expect(screen.getByText(/not changed automatically/)).toBeInTheDocument();
  expect(saveButton()).toBeDisabled();
});

it("shows why a saved node lost eligibility", async () => {
  serve({ read: () => Promise.resolve(settings({ node_id: "n3", effective_node_id: "n3" })) });
  renderSetting();
  expect(await screen.findByText("The saved build node arm-3 is unavailable")).toBeInTheDocument();
  expect(screen.getByRole("combobox", { name: "Build node" })).toHaveTextContent("arm-3 (unavailable)");
});

it("keeps the draft when the server refuses the change", async () => {
  const user = userEvent.setup();
  serve({ save: () => Promise.reject(new ApiError(409, "wait for active operations before changing the build node")) });
  renderSetting();
  await screen.findByRole("combobox", { name: "Build node" });
  await choose(user, "builder-2");
  await user.click(saveButton());
  expect(await screen.findByText("The build node was not saved")).toBeInTheDocument();
  expect(screen.getByText("wait for active operations before changing the build node")).toBeInTheDocument();
  expect(screen.getByRole("combobox", { name: "Build node" })).toHaveTextContent("builder-2");
  expect(screen.getByRole("status")).toHaveTextContent("Unsaved changes");
});

it("blocks saving while an operation is active", async () => {
  const user = userEvent.setup();
  serve({ operations: () => Promise.resolve([running()]) });
  renderSetting();
  expect(await screen.findByText("An operation is in progress")).toBeInTheDocument();
  await choose(user, "builder-2");
  expect(saveButton()).toBeDisabled();
});

it("blocks saving when operations cannot be read", async () => {
  const user = userEvent.setup();
  serve({ operations: () => Promise.reject(new Error("down")) });
  renderSetting();
  expect(await screen.findByText("Operations could not be read")).toBeInTheDocument();
  await choose(user, "builder-2");
  expect(saveButton()).toBeDisabled();
});

it("blocks saving while offline", async () => {
  const user = userEvent.setup();
  serve();
  renderSetting();
  await screen.findByRole("combobox", { name: "Build node" });
  await choose(user, "builder-2");
  vi.spyOn(navigator, "onLine", "get").mockReturnValue(false);
  act(() => void window.dispatchEvent(new Event("offline")));
  expect(await screen.findByText("You are offline")).toBeInTheDocument();
  expect(saveButton()).toBeDisabled();
});

it("offers Retry when the setting cannot be read", async () => {
  serve({ read: () => Promise.reject(new Error("down")) });
  renderSetting();
  expect(await screen.findByText("Could not load the build node")).toBeInTheDocument();
  expect(screen.getByRole("button", { name: "Retry" })).toBeInTheDocument();
  expect(screen.queryByRole("combobox", { name: "Build node" })).not.toBeInTheDocument();
});

it("pauses saving after a failed refresh and keeps the draft", async () => {
  const user = userEvent.setup();
  let fail = false;
  serve({ read: () => (fail ? Promise.reject(new Error("refresh failed")) : Promise.resolve(settings())) });
  const client = renderSetting();
  await screen.findByRole("combobox", { name: "Build node" });
  await choose(user, "builder-2");
  fail = true;
  await client.refetchQueries({ queryKey: ["workloads", "w1", "build-node"] });
  expect(await screen.findByText("Could not refresh the build node")).toBeInTheDocument();
  expect(screen.getByRole("combobox", { name: "Build node" })).toHaveTextContent("builder-2");
  expect(saveButton()).toBeDisabled();
});

it("shows operators the effective node read-only", async () => {
  serve({ read: () => Promise.resolve(settings({ node_id: "n2", effective_node_id: "n2" })) });
  renderSetting(workload(["read", "operate"]));
  expect(await screen.findByText("builder-2")).toBeInTheDocument();
  expect(screen.getByText("(designated)")).toBeInTheDocument();
  expect(screen.queryByRole("combobox", { name: "Build node" })).not.toBeInTheDocument();
  expect(screen.queryByRole("button", { name: "Save build node" })).not.toBeInTheDocument();
});

it("shows viewers the runtime default read-only", async () => {
  serve();
  renderSetting(workload(["read"]));
  expect(await screen.findByText("runtime-1")).toBeInTheDocument();
  expect(screen.getByText("(runtime node)")).toBeInTheDocument();
  expect(screen.queryByRole("combobox")).not.toBeInTheDocument();
});

it("treats a response without an effective node as a failed read", async () => {
  serve({ read: () => Promise.resolve({ node_id: "", runtime_node_id: "n1", nodes: [] }) });
  renderSetting();
  expect(await screen.findByText("Could not load the build node")).toBeInTheDocument();
});

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import type { ReactNode } from "react";
import { afterEach, expect, it, vi } from "vitest";
import { request } from "../api/client";
import type { BuildNodeSettings } from "../api/build-nodes";
import type { Node, Operation, Workload } from "../api/types";
import { ResourceBuild } from "./ResourceBuild";

vi.mock("@tanstack/react-router", () => ({ Link: ({ children }: { children: ReactNode }) => <a href="/">{children}</a> }));
vi.mock("../api/client", () => ({ request: vi.fn(), errorMessage: () => "Request failed", isStatus: () => false }));
afterEach(() => vi.clearAllMocks());

const workload = { id: "w1", name: "nightly", node_id: "n1", kind: "job", permissions: ["read", "edit_configuration", "read_secrets"], manually_stopped: false, deployed: true } as Workload;

function node(buildPublish: boolean | undefined, id = "n1", name = "worker-1"): Node {
  return { id, name, address: "", port: 22, state: "ready", mode: "ssh", last_checked_at: null, capabilities: { protocol: 1, os: "linux", arch: "amd64", docker: "27", compose: "2", ready: true, build_publish: buildPublish } };
}

const reference = "ghcr.io/team/app@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef";

function build(id: string, state: Operation["state"], images = false, executionNode?: string): Operation {
  const created_at = "2026-11-22T10:00:00Z";
  return {
    id,
    workload_id: "w1",
    action: "build",
    state,
    created_at,
    ...(executionNode ? { execution_node_id: executionNode } : {}),
    result: {
      id,
      workload: "w1",
      action: "build",
      state,
      created_at,
      ...(images ? { build: { images: [{ service: "web", image_id: "sha256:1", reference }] } } : {}),
      ...(state === "failed" ? { error: "push denied" } : {}),
    },
  };
}

const runtimeChoice = { id: "n1", name: "worker-1", eligible: true, reason: "" };

function settings(overrides: Partial<BuildNodeSettings> = {}): BuildNodeSettings {
  return { node_id: "", runtime_node_id: "n1", effective_node_id: "n1", nodes: [runtimeChoice], ...overrides };
}

interface Serve {
  nodes?: Node[];
  registryId?: string;
  operations?: Operation[] | (() => Promise<unknown>);
  buildNode?: () => Promise<unknown>;
}

function serve({ nodes = [node(true)], registryId = "r1", operations = [], buildNode = () => Promise.resolve(settings()) }: Serve = {}) {
  vi.mocked(request).mockImplementation((method: string, path: string) => {
    if (method === "POST") return Promise.resolve({ id: "op9", state: "queued" });
    if (path === "/api/workloads/w1/build-node") return buildNode();
    if (path === "/api/nodes") return Promise.resolve(nodes);
    if (path === "/api/registries") return Promise.resolve([{ id: "r1", name: "GHCR", registry: { server: "ghcr.io", username: "bot" } }]);
    if (path.endsWith("/registry")) return Promise.resolve({ registry_id: registryId });
    if (path.startsWith("/api/operations")) return typeof operations === "function" ? operations() : Promise.resolve(operations);
    return Promise.reject(new Error(`Unexpected request ${path}`));
  });
}

function renderBuild(target: Workload = workload) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(
    <QueryClientProvider client={client}>
      <ResourceBuild workload={target} />
    </QueryClientProvider>,
  );
  return client;
}

it("validates the repository and queues the exact build payload", async () => {
  serve();
  renderBuild();
  const button = screen.getByRole("button", { name: "Build and publish" });
  await waitFor(() => expect(button).toBeEnabled());
  expect(await screen.findByText("ghcr.io")).toBeInTheDocument();

  fireEvent.change(screen.getByLabelText("Repository"), { target: { value: "ghcr.io/team/app:latest" } });
  fireEvent.click(button);
  expect(screen.getByText(/without a registry host, tag or digest/)).toBeInTheDocument();
  expect(screen.getByLabelText("Repository")).toHaveAttribute("aria-invalid", "true");
  expect(request).not.toHaveBeenCalledWith("POST", expect.anything(), expect.anything());

  fireEvent.change(screen.getByLabelText("Repository"), { target: { value: " team/app " } });
  fireEvent.click(screen.getByRole("checkbox", { name: "Bypass the build cache" }));
  fireEvent.click(button);
  await waitFor(() => expect(request).toHaveBeenCalledWith("POST", "/api/workloads/w1/operations", { action: "build", build: { repository: "team/app", no_cache: true } }));
  expect(await screen.findByText("Build queued")).toBeInTheDocument();
});

it("sends an entered reason with the build", async () => {
  serve();
  renderBuild();
  const button = screen.getByRole("button", { name: "Build and publish" });
  await waitFor(() => expect(button).toBeEnabled());
  fireEvent.change(screen.getByLabelText("Repository"), { target: { value: "team/app" } });
  fireEvent.change(screen.getByRole("textbox", { name: /^Reason/ }), { target: { value: " release 4.2 " } });
  fireEvent.click(button);
  await waitFor(() => expect(request).toHaveBeenCalledWith("POST", "/api/workloads/w1/operations", { action: "build", build: { repository: "team/app", no_cache: false }, reason: "release 4.2" }));
});

it("asks for a worker upgrade when build support is not reported", async () => {
  serve({ nodes: [node(undefined)], buildNode: () => Promise.resolve(settings({ nodes: [{ ...runtimeChoice, eligible: false, reason: "the build node must be ready and report build and publish support" }] })) });
  renderBuild();
  expect(await screen.findByText("Worker upgrade needed")).toBeInTheDocument();
  expect(screen.getByRole("button", { name: "Build and publish" })).toBeDisabled();
});

it("asks for a registry when none is selected", async () => {
  serve({ registryId: "" });
  renderBuild();
  expect(await screen.findByText("No registry selected")).toBeInTheDocument();
  expect(screen.getByRole("button", { name: "Build and publish" })).toBeDisabled();
});

it.each([
  ["read only", ["read"]],
  ["operate without configuration rights", ["read", "operate"]],
  ["edit configuration without read secrets", ["read", "operate", "edit", "edit_configuration"]],
] as const)("shows results read-only with %s", async (_label, permissions) => {
  serve({ operations: [build("ok", "succeeded", true)] });
  renderBuild({ ...workload, permissions: [...permissions] });
  expect(await screen.findByText(reference)).toBeInTheDocument();
  expect(screen.getByText(/need Edit configuration and Read secrets/)).toBeInTheDocument();
  expect(screen.queryByRole("button", { name: "Build and publish" })).not.toBeInTheDocument();
});

it("keeps the latest successful references visible after a failed attempt", async () => {
  serve({ operations: [build("bad", "failed"), build("ok", "succeeded", true)] });
  renderBuild();
  expect(await screen.findByText("Latest attempt")).toBeInTheDocument();
  expect(screen.getByText("push denied")).toBeInTheDocument();
  expect(screen.getByText(/latest successful build is still shown below/)).toBeInTheDocument();
  const list = screen.getByRole("list", { name: "Published images" });
  expect(within(list).getByText("web")).toBeInTheDocument();
  expect(within(list).getByText(reference)).toBeInTheDocument();
  expect(within(list).getByRole("button", { name: "Copy reference for web" })).toBeInTheDocument();
});

it("blocks building while a build runs and links to its progress", async () => {
  serve({ operations: [build("run", "running")] });
  renderBuild();
  expect(await screen.findByText("View build progress")).toBeInTheDocument();
  expect(screen.getByText("No successful build in the recent operation history.")).toBeInTheDocument();
  expect(screen.getByRole("button", { name: "Build and publish" })).toBeDisabled();
});

const designated = settings({ node_id: "n2", effective_node_id: "n2", nodes: [{ ...runtimeChoice, eligible: false, reason: "upgrade" }, { id: "n2", name: "builder-2", eligible: true, reason: "" }] });

it("builds on an eligible designated node with the unchanged build body", async () => {
  serve({ nodes: [node(undefined), node(true, "n2", "builder-2")], buildNode: () => Promise.resolve(designated) });
  renderBuild();
  const button = screen.getByRole("button", { name: "Build and publish" });
  await waitFor(() => expect(button).toBeEnabled());
  expect(screen.queryByText("Worker upgrade needed")).not.toBeInTheDocument();
  fireEvent.change(screen.getByLabelText("Repository"), { target: { value: "team/app" } });
  fireEvent.click(button);
  await waitFor(() => expect(request).toHaveBeenCalledWith("POST", "/api/workloads/w1/operations", { action: "build", build: { repository: "team/app", no_cache: false } }));
});

it("blocks building when the effective node is not eligible, even if the runtime node can build", async () => {
  const reason = "build and runtime nodes must report the same Linux architecture";
  serve({ nodes: [node(true), node(true, "n2", "builder-2")], buildNode: () => Promise.resolve(settings({ node_id: "n2", effective_node_id: "n2", nodes: [runtimeChoice, { id: "n2", name: "builder-2", eligible: false, reason }] })) });
  renderBuild();
  expect(await screen.findByText("builder-2: " + reason)).toBeInTheDocument();
  expect(screen.getByRole("button", { name: "Build and publish" })).toBeDisabled();
});

it("blocks building when the effective node is missing from the list", async () => {
  serve({ buildNode: () => Promise.resolve(settings({ node_id: "n9", effective_node_id: "n9" })) });
  renderBuild();
  expect(await screen.findByText("Build node unavailable")).toBeInTheDocument();
  expect(screen.getByText("n9")).toBeInTheDocument();
  expect(screen.getByRole("button", { name: "Build and publish" })).toBeDisabled();
});

it("blocks building and offers Retry when the build node cannot be read", async () => {
  serve({ buildNode: () => Promise.reject(new Error("down")) });
  renderBuild();
  expect(await screen.findByText("Could not load the build node")).toBeInTheDocument();
  expect(screen.getByRole("button", { name: "Retry" })).toBeInTheDocument();
  expect(screen.getByRole("button", { name: "Build and publish" })).toBeDisabled();
});

it("stops trusting the build node after a failed refresh", async () => {
  let fail = false;
  serve({ buildNode: () => (fail ? Promise.reject(new Error("down")) : Promise.resolve(settings())) });
  const client = renderBuild();
  const button = screen.getByRole("button", { name: "Build and publish" });
  await waitFor(() => expect(button).toBeEnabled());
  fail = true;
  await client.refetchQueries({ queryKey: ["workloads", "w1", "build-node"] });
  expect(await screen.findByText("Could not load the build node")).toBeInTheDocument();
  expect(button).toBeDisabled();
});

it("stops trusting operations after a failed refresh", async () => {
  let fail = false;
  serve({ operations: () => (fail ? Promise.reject(new Error("down")) : Promise.resolve([])) });
  const client = renderBuild();
  const button = screen.getByRole("button", { name: "Build and publish" });
  await waitFor(() => expect(button).toBeEnabled());
  fail = true;
  await client.refetchQueries({ queryKey: ["operations"] });
  expect(await screen.findByText("Could not load the latest builds")).toBeInTheDocument();
  expect(button).toBeDisabled();
});

it("shows where each build ran from the operation, not the current assignment", async () => {
  serve({ nodes: [node(true), node(true, "n2", "builder-2")], buildNode: () => Promise.resolve(designated), operations: [build("bad", "failed", false), build("ok", "succeeded", true, "n1")] });
  renderBuild();
  expect(await screen.findByText("Built on worker-1")).toBeInTheDocument();
  expect(screen.getByText("Build node not recorded")).toBeInTheDocument();
  expect(screen.queryByText("Built on builder-2")).not.toBeInTheDocument();
});

it("shows the node ID when the execution node is not listed", async () => {
  serve({ operations: [build("ok", "succeeded", true, "gone")] });
  renderBuild();
  expect(await screen.findByText("gone")).toBeInTheDocument();
});

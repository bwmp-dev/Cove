import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import type { ReactNode } from "react";
import { afterEach, expect, it, vi } from "vitest";
import { request } from "../api/client";
import type { Node, Operation, Workload } from "../api/types";
import { ResourceBuild } from "./ResourceBuild";

vi.mock("@tanstack/react-router", () => ({ Link: ({ children }: { children: ReactNode }) => <a href="/">{children}</a> }));
vi.mock("../api/client", () => ({ request: vi.fn(), errorMessage: () => "Request failed", isStatus: () => false }));
afterEach(() => vi.clearAllMocks());

const workload = { id: "w1", name: "nightly", node_id: "n1", kind: "job", permissions: ["read", "operate"], manually_stopped: false, deployed: true } as Workload;

function node(buildPublish: boolean | undefined): Node {
  return { id: "n1", name: "worker-1", address: "", port: 22, state: "ready", mode: "ssh", last_checked_at: null, capabilities: { protocol: 1, os: "linux", arch: "amd64", docker: "27", compose: "2", ready: true, build_publish: buildPublish } };
}

const reference = "ghcr.io/team/app@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef";

function build(id: string, state: Operation["state"], images = false): Operation {
  const created_at = "2026-11-22T10:00:00Z";
  return {
    id,
    workload_id: "w1",
    action: "build",
    state,
    created_at,
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

function serve({ nodes = [node(true)], registryId = "r1", operations = [] as Operation[] } = {}) {
  vi.mocked(request).mockImplementation((method: string, path: string) => {
    if (method === "POST") return Promise.resolve({ id: "op9", state: "queued" });
    if (path === "/api/nodes") return Promise.resolve(nodes);
    if (path === "/api/registries") return Promise.resolve([{ id: "r1", name: "GHCR", registry: { server: "ghcr.io", username: "bot" } }]);
    if (path.endsWith("/registry")) return Promise.resolve({ registry_id: registryId });
    if (path.startsWith("/api/operations")) return Promise.resolve(operations);
    return Promise.reject(new Error(`Unexpected request ${path}`));
  });
}

function renderBuild(target: Workload = workload) {
  render(
    <QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}>
      <ResourceBuild workload={target} />
    </QueryClientProvider>,
  );
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

it("asks for a worker upgrade when build support is not reported", async () => {
  serve({ nodes: [node(undefined)] });
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

it("shows results read-only without operator access", async () => {
  serve({ operations: [build("ok", "succeeded", true)] });
  renderBuild({ ...workload, permissions: ["read"] });
  expect(await screen.findByText(reference)).toBeInTheDocument();
  expect(screen.getByText(/need operator access/)).toBeInTheDocument();
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

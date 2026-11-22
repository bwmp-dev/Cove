import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import type { ReactNode } from "react";
import { afterEach, expect, it, vi } from "vitest";
import { request } from "../api/client";
import type { Node, Workload, WorkloadImages } from "../api/types";
import { ResourceImages } from "./ResourceImages";

vi.mock("@tanstack/react-router", () => ({ Link: ({ children }: { children: ReactNode }) => <a href="/">{children}</a> }));
vi.mock("../api/client", () => ({ request: vi.fn(), errorMessage: () => "Request failed", isStatus: () => false }));
afterEach(() => vi.clearAllMocks());
// jsdom does not implement modal dialogs.
HTMLDialogElement.prototype.showModal = function (this: HTMLDialogElement) {
  this.open = true;
};
HTMLDialogElement.prototype.close = function (this: HTMLDialogElement) {
  this.open = false;
};

const workload = { id: "w1", name: "web", node_id: "n1", kind: "service", permissions: ["read", "operate"], manually_stopped: false, deployed: true } as Workload;

function node(imageLifecycle: boolean): Node {
  return { id: "n1", name: "worker-1", address: "", port: 22, state: "ready", mode: "ssh", last_checked_at: null, capabilities: { protocol: 1, os: "linux", arch: "amd64", docker: "27", compose: "2", ready: true, image_lifecycle: imageLifecycle } };
}

function serve(nodes: Node[], images: WorkloadImages) {
  vi.mocked(request).mockImplementation((method: string, path: string) => {
    if (method === "POST") return Promise.resolve({ id: "op2", state: "queued" });
    if (path === "/api/nodes") return Promise.resolve(nodes);
    if (path.startsWith("/api/operations")) return Promise.resolve([]);
    return Promise.resolve(images);
  });
}

function renderImages(target: Workload = workload) {
  render(<QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}><ResourceImages workload={target} /></QueryClientProvider>);
}

it("marks results from before the latest deployment as stale and asks before pulling and redeploying", async () => {
  serve([node(true)], {
    checked_at: "2026-11-22T10:00:00Z",
    operation_id: "op1",
    stale: true,
    images: [{ service: "web", reference: "ghcr.io/acme/web:1", image_id: "sha256:0123456789abcdef", status: "update_available" }, { service: "db", reference: "postgres:17", image_id: "sha256:fedcba9876543210", status: "unknown", message: "registry unavailable" }],
  });
  renderImages();
  expect(await screen.findByText("Result is from before the latest deployment")).toBeInTheDocument();
  const list = screen.getByRole("list", { name: "Image status at the last check" });
  expect(within(list).getByText("Update available")).toBeInTheDocument();
  expect(screen.getByText("1 update available, 1 unknown")).toBeInTheDocument();

  await waitFor(() => expect(screen.getByRole("button", { name: "Pull images and redeploy" })).toBeEnabled());
  fireEvent.click(screen.getByRole("button", { name: "Pull images and redeploy" }));
  fireEvent.click(screen.getByRole("button", { name: "Pull and redeploy" }));
  await waitFor(() => expect(request).toHaveBeenCalledWith("POST", "/api/workloads/w1/operations", { action: "deploy", pull_latest: true }));

  fireEvent.click(await screen.findByRole("button", { name: "Check images" }));
  await waitFor(() => expect(request).toHaveBeenCalledWith("POST", "/api/workloads/w1/operations", { action: "image_check" }));
});

it("asks for a worker upgrade when image lifecycle is unsupported", async () => {
  serve([node(false)], { checked_at: null, stale: true, images: [] });
  renderImages();
  expect(await screen.findByText("Worker upgrade needed")).toBeInTheDocument();
  expect(screen.getByText(/No image check has completed yet/)).toBeInTheDocument();
  expect(screen.getByRole("button", { name: "Check images" })).toBeDisabled();
});

it("shows results read-only without operator access", async () => {
  serve([node(true)], { checked_at: null, stale: true, images: [] });
  renderImages({ ...workload, permissions: ["read"] });
  expect(await screen.findByText(/need operator access/)).toBeInTheDocument();
  expect(screen.queryByRole("button", { name: "Check images" })).not.toBeInTheDocument();
});

it("holds pull and redeploy while idle sleep keeps the app asleep, but still allows image checks", async () => {
  // Test fixture: a worker runtime report for a sleeping app.
  const runtime = { containers: [], idle: { enabled: true, after_seconds: 300, phase: "sleeping", active_requests: 0 } };
  vi.mocked(request).mockImplementation((_method: string, path: string) => {
    if (path === "/api/nodes") return Promise.resolve([node(true)]);
    if (path.startsWith("/api/operations")) return Promise.resolve([]);
    if (path === "/api/workloads/w1/runtime") return Promise.resolve(runtime);
    if (path === "/api/workloads/w1/idle-policy") return Promise.resolve({ enabled: true, after_seconds: 300, request_driven: true });
    return Promise.resolve({ checked_at: null, stale: true, images: [] });
  });
  renderImages();
  expect(await screen.findByText("The app is asleep. Pulling images and redeploying is available after it wakes: use Start or Wake now first.")).toBeInTheDocument();
  expect(screen.getByRole("button", { name: "Pull images and redeploy" })).toBeDisabled();
  await waitFor(() => expect(screen.getByRole("button", { name: "Check images" })).toBeEnabled());
});

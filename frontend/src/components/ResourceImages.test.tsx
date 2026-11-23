import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import type { ReactNode } from "react";
import { afterEach, expect, it, vi } from "vitest";
import { request } from "../api/client";
import type { Node, Workload, WorkloadImages } from "../api/types";
import { ResourceImages } from "./ResourceImages";

vi.mock("@tanstack/react-router", () => ({ Link: ({ children }: { children: ReactNode }) => <a href="/">{children}</a> }));
vi.mock("../api/client", () => ({ request: vi.fn(), errorMessage: () => "Request failed", isStatus: () => false }));
afterEach(() => {
  vi.clearAllMocks();
  vi.restoreAllMocks();
});
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

// Fixtures for job image checks; no worker or registry is contacted.
const job = { ...workload, name: "nightly", kind: "job" } as Workload;
const jobCapable = { protocol: 1, os: "linux", arch: "amd64", docker: "27", compose: "2", ready: true, image_lifecycle: true, job_image_lifecycle: true };
const checked: WorkloadImages = { checked_at: "2026-11-22T10:00:00Z", operation_id: "op1", stale: false, images: [{ service: "task", reference: "busybox:1.37", image_id: "sha256:0123456789abcdef", status: "update_available" }] };
const checkOperation = { id: "op1", workload_id: "w1", action: "image_check", state: "succeeded", result: null, created_at: "2026-11-22T10:00:00Z" };

function jobNode(capabilities: Node["capabilities"]): Node {
  return { ...node(true), capabilities };
}

function serveJob(nodes: Node[] | Error, images: WorkloadImages, operations: unknown[] | Error = []) {
  vi.mocked(request).mockImplementation((method: string, path: string) => {
    if (method === "POST") return Promise.resolve({ id: "op2", state: "queued" });
    if (path === "/api/nodes") return nodes instanceof Error ? Promise.reject(nodes) : Promise.resolve(nodes);
    if (path.startsWith("/api/operations")) return operations instanceof Error ? Promise.reject(operations) : Promise.resolve(operations);
    if (path === "/api/workloads/w1/images") return Promise.resolve(images);
    return Promise.reject(new Error(`Unexpected request ${path}`));
  });
}

it("checks retained job containers hourly or on request and never offers pulling or redeploying", async () => {
  serveJob([jobNode(jobCapable)], { checked_at: null, stale: false, images: [] });
  renderImages(job);
  expect(await screen.findByText(/Retained job containers are checked about once an hour, or check now\. Checks never run the job\. If the job has never run, run it first/)).toBeInTheDocument();
  expect(screen.getByText(/never run the job, pull images or start containers/)).toBeInTheDocument();
  expect(screen.queryByRole("button", { name: "Pull images and redeploy" })).not.toBeInTheDocument();
  expect(screen.queryByText(/Running services are checked/)).not.toBeInTheDocument();

  const check = screen.getByRole("button", { name: "Check images" });
  await waitFor(() => expect(check).toBeEnabled());
  fireEvent.click(check);
  await waitFor(() => expect(request).toHaveBeenCalledWith("POST", "/api/workloads/w1/operations", { action: "image_check" }));
  expect(await screen.findByText("Image check queued")).toBeInTheDocument();
  expect(vi.mocked(request).mock.calls.filter(([method]) => method === "POST")).toHaveLength(1);
});

it("describes job results by the last run, not by running containers or redeploys", async () => {
  serveJob([jobNode(jobCapable)], checked);
  renderImages(job);
  const status = await screen.findByText("Update available");
  expect(status.closest("[title]")).toHaveAttribute("title", "The registry has a different image for the same tag than the job's last run used. Checking does not pull it or run the job.");
  expect(screen.queryByText(/Pull images and redeploy to use it/)).not.toBeInTheDocument();
});

it("blocks job checks on workers without job image support but keeps cached results readable", async () => {
  serveJob([jobNode({ ...jobCapable, job_image_lifecycle: undefined })], checked);
  renderImages(job);
  expect(await screen.findByText("Worker upgrade needed")).toBeInTheDocument();
  expect(screen.getByText(/does not report job image checks/)).toBeInTheDocument();
  expect(screen.getByRole("button", { name: "Check images" })).toBeDisabled();
  expect(await screen.findByRole("list", { name: "Image status" })).toHaveTextContent("busybox:1.37");
});

it("blocks job checks when the worker has not reported its capabilities", async () => {
  serveJob([jobNode(null)], checked);
  renderImages(job);
  expect(await screen.findByText(/has not reported its capabilities, so job image checks cannot be confirmed/)).toBeInTheDocument();
  expect(screen.getByRole("button", { name: "Check images" })).toBeDisabled();
});

it("fails closed when the node cannot be refreshed, even with a cached ready node", async () => {
  serveJob(new Error("unreachable"), checked);
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  client.setQueryData(["nodes"], [jobNode(jobCapable)]);
  render(<QueryClientProvider client={client}><ResourceImages workload={job} /></QueryClientProvider>);
  expect(await screen.findByText("Cove cannot read the node. Checking images is available once the node loads.")).toBeInTheDocument();
  expect(screen.getByRole("button", { name: "Check images" })).toBeDisabled();
});

it("fails closed when operations cannot be read", async () => {
  serveJob([jobNode(jobCapable)], checked, new Error("unreachable"));
  renderImages(job);
  expect(await screen.findByText(/Cove cannot tell whether another operation is running/)).toBeInTheDocument();
  expect(screen.getByRole("button", { name: "Check images" })).toBeDisabled();
});

it("pauses job checks while a run is active and names it as a run", async () => {
  serveJob([jobNode(jobCapable)], checked, [{ id: "run1", workload_id: "w1", action: "deploy", state: "running", result: null, created_at: "2026-11-22T11:00:00Z" }]);
  renderImages(job);
  expect(await screen.findByText("Run running. Image actions are available when it finishes.")).toBeInTheDocument();
  expect(screen.getByRole("button", { name: "Check images" })).toBeDisabled();
});

it("shows job results read-only without operator access", async () => {
  serveJob([jobNode(jobCapable)], checked);
  renderImages({ ...job, permissions: ["read"] });
  expect(await screen.findByText("Checking images needs operator access. Results are read-only.")).toBeInTheDocument();
  expect(screen.queryByRole("button", { name: "Check images" })).not.toBeInTheDocument();
  expect(await screen.findByText("Update available")).toBeInTheDocument();
});

it("blocks job checks while offline", async () => {
  vi.spyOn(navigator, "onLine", "get").mockReturnValue(false);
  serveJob([jobNode(jobCapable)], checked);
  renderImages(job);
  expect(await screen.findByText("You are offline. Checking images is available when the connection returns.")).toBeInTheDocument();
  expect(screen.getByRole("button", { name: "Check images" })).toBeDisabled();
});

it("marks a job result stale after a newer run fails, reading it again when the run finishes", async () => {
  let operations: unknown[] = [{ id: "run2", workload_id: "w1", action: "deploy", state: "running", result: null, created_at: "2026-11-22T11:00:00Z" }, checkOperation];
  let images: WorkloadImages = checked;
  vi.mocked(request).mockImplementation((_method: string, path: string) => {
    if (path === "/api/nodes") return Promise.resolve([jobNode(jobCapable)]);
    if (path.startsWith("/api/operations")) return Promise.resolve(operations);
    return Promise.resolve(images);
  });
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(<QueryClientProvider client={client}><ResourceImages workload={job} /></QueryClientProvider>);
  expect(await screen.findByText("Run running. Image actions are available when it finishes.")).toBeInTheDocument();
  const imageReads = () => vi.mocked(request).mock.calls.filter(([, path]) => path === "/api/workloads/w1/images").length;
  await waitFor(() => expect(imageReads()).toBeGreaterThan(0));
  const before = imageReads();

  operations = [{ id: "run2", workload_id: "w1", action: "deploy", state: "failed", result: null, created_at: "2026-11-22T11:00:00Z" }, checkOperation];
  images = { ...checked, stale: true };
  await client.refetchQueries({ queryKey: ["operations"] });
  expect(await screen.findByText("Result is from before the latest run")).toBeInTheDocument();
  expect(screen.getByText(/including runs that failed, were cancelled or are still going/)).toBeInTheDocument();
  expect(imageReads()).toBeGreaterThan(before);
});

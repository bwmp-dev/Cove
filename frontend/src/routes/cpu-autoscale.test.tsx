import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import type { ReactNode } from "react";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { ApiError, request } from "../api/client";
import type { AutoscalePolicy, AutoscaleState, Node, Operation, Workload } from "../api/types";
import { ScaleOutcome } from "../components/CpuAutoscale";
import { ResourceDeploymentSafety } from "./resource-config";

let resource: Workload;

vi.mock("@tanstack/react-router", () => ({ Link: ({ children }: { children: ReactNode }) => <a href="/">{children}</a>, useBlocker: () => ({ status: "idle" }) }));
vi.mock("../api/client", async (importOriginal) => ({ ...(await importOriginal<typeof import("../api/client")>()), request: vi.fn() }));
vi.mock("./workload-detail", () => ({ useResource: () => resource }));
beforeEach(() => vi.stubGlobal("ResizeObserver", class { observe = vi.fn(); disconnect = vi.fn(); }));
afterEach(() => {
  vi.clearAllMocks();
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
});

// Test fixtures below are API-shaped responses for a single resource; they are not product data.
const base = {
  id: "w1",
  name: "web",
  node_id: "n1",
  kind: "service",
  compose: "services:\n  web:\n    image: nginx\n",
  source: null,
  environment_keys: [],
  has_source_token: false,
  permissions: ["read", "operate", "edit"],
  manually_stopped: false,
  deployed: true,
  project_id: "p1",
  guarded_deploy: false,
  rollout: { strategy: "rolling", parallel_volumes: false, drain_seconds: 0, timeout_seconds: 120, canary_percent: 10, replicas: { web: 2 } },
  limits: { web: { cpus: 0.5, memory_mb: 256 }, db: { cpus: 0, memory_mb: 0 } },
} as Workload;

const offState: AutoscaleState = { policy: { enabled: false, services: {} }, samples: [], last_queued_at: null, operation_id: "", status: "disabled", last_error: "" };

function node(capabilities: Partial<NonNullable<Node["capabilities"]>> | null = { cpu_autoscale: true }, state: Node["state"] = "ready"): Node {
  return { id: "n1", name: "worker-1", address: "", port: 22, state, mode: "ssh", last_checked_at: null, capabilities: capabilities && { protocol: 1, os: "linux", arch: "amd64", docker: "27", compose: "2", ready: true, rollouts: true, ...capabilities } };
}

interface Serve {
  state?: () => Promise<AutoscaleState>;
  put?: (body: AutoscalePolicy) => Promise<AutoscaleState>;
  nodes?: () => Promise<Node[]>;
  operations?: () => Promise<Operation[]>;
}

function serve({ state = () => Promise.resolve(offState), put = (body) => Promise.resolve({ ...offState, policy: body, status: body.enabled ? "waiting_for_samples" : "disabled" }), nodes = () => Promise.resolve([node()]), operations = () => Promise.resolve([]) }: Serve = {}) {
  vi.mocked(request).mockImplementation((method: string, path: string, body?: unknown) => {
    if (path === "/api/workloads/w1/autoscale-policy") return method === "PUT" ? put(body as AutoscalePolicy) : state();
    if (path === "/api/workloads/w1" && method === "PUT") return Promise.resolve(base);
    if (path === "/api/workloads/w1/restart-policy") return Promise.resolve({ enabled: false, last_queued_at: null, attempts_in_window: 0, window_started_at: null, operation_id: null, last_error: "" });
    if (path === "/api/workloads/w1/idle-policy") return Promise.resolve({ enabled: false, after_seconds: 300, request_driven: false });
    if (path === "/api/workloads/w1/runtime") return Promise.resolve({ containers: [], idle: null });
    if (path === "/api/nodes") return nodes();
    if (path.startsWith("/api/operations")) return operations();
    return Promise.reject(new Error(`unexpected ${method} ${path}`));
  });
}

function renderPage(workload: Workload = base) {
  resource = workload;
  render(
    <QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}>
      <ResourceDeploymentSafety />
    </QueryClientProvider>,
  );
}

const section = () => screen.getByRole("region", { name: "CPU scaling" });
const scaleSwitch = () => within(section()).getByRole("switch", { name: "Scale on CPU use" });
const webBox = () => within(section()).getByRole("checkbox", { name: "Scale web" });
const save = () => within(section()).getByRole("button", { name: "Save changes" });
const policyPuts = () => vi.mocked(request).mock.calls.filter(([method, path]) => method === "PUT" && path === "/api/workloads/w1/autoscale-policy");
const workloadPuts = () => vi.mocked(request).mock.calls.filter(([method, path]) => method === "PUT" && path === "/api/workloads/w1");

it("saves an explicit policy through its own endpoint, and ordinary saves never send autoscale", async () => {
  serve();
  renderPage();
  await waitFor(() => expect(scaleSwitch()).toBeEnabled());
  // Services without explicit CPU and memory limits cannot be selected.
  expect(within(section()).getByRole("checkbox", { name: "Scale db" })).toBeDisabled();
  expect(within(section()).getByText("Off")).toBeInTheDocument();

  fireEvent.click(scaleSwitch());
  fireEvent.click(webBox());
  fireEvent.change(within(section()).getByLabelText("Maximum replicas"), { target: { value: "6" } });
  fireEvent.click(save());
  await waitFor(() => expect(policyPuts()).toHaveLength(1));
  expect(policyPuts()[0]?.[2]).toEqual({ enabled: true, services: { web: { min_replicas: 1, max_replicas: 6, target_cpu_percent: 70 } } });
  expect(await within(section()).findByText("Saved. The panel uses it on its next check.")).toBeInTheDocument();
  expect(within(section()).getByText("Collecting samples")).toBeInTheDocument();
  expect(workloadPuts()).toHaveLength(0);

  const safety = screen.getByRole("region", { name: "Deployment safety" });
  fireEvent.click(within(safety).getByRole("switch", { name: "Enable guarded deployment" }));
  fireEvent.click(within(safety).getByRole("button", { name: "Save changes" }));
  await waitFor(() => expect(workloadPuts()).toHaveLength(1));
  expect(workloadPuts()[0]?.[2]).not.toHaveProperty("autoscale");
});

it("checks bounds and the replica budget before saving", async () => {
  serve();
  renderPage();
  await waitFor(() => expect(scaleSwitch()).toBeEnabled());
  fireEvent.click(scaleSwitch());
  fireEvent.click(save());
  expect(await within(section()).findByText("Select at least one service to scale.")).toBeInTheDocument();
  fireEvent.click(webBox());
  fireEvent.change(within(section()).getByLabelText("Minimum replicas"), { target: { value: "5" } });
  fireEvent.change(within(section()).getByLabelText("Maximum replicas"), { target: { value: "3" } });
  fireEvent.change(within(section()).getByLabelText("Target CPU (%)"), { target: { value: "95" } });
  fireEvent.click(save());
  expect(await within(section()).findByText("Use a maximum at least as large as the minimum.")).toBeInTheDocument();
  expect(within(section()).getByLabelText("Target CPU (%)")).toHaveAttribute("aria-invalid", "true");
  expect(policyPuts()).toHaveLength(0);
});

it("keeps typed input and shows the server's reason when saving fails", async () => {
  serve({ put: () => Promise.reject(new ApiError(409, "deploy a healthy eligible rolling release with CPU limits before enabling scaling")) });
  renderPage();
  await waitFor(() => expect(scaleSwitch()).toBeEnabled());
  fireEvent.click(scaleSwitch());
  fireEvent.click(webBox());
  fireEvent.change(within(section()).getByLabelText("Target CPU (%)"), { target: { value: "55" } });
  fireEvent.click(save());
  expect(await within(section()).findByText("deploy a healthy eligible rolling release with CPU limits before enabling scaling")).toBeInTheDocument();
  expect(within(section()).getByText(/Your changes are kept/)).toBeInTheDocument();
  expect(scaleSwitch()).toBeChecked();
  expect(within(section()).getByLabelText("Target CPU (%)")).toHaveValue("55");
});

it("blocks turning scaling on without confirmed worker support but allows turning it off", async () => {
  const onState: AutoscaleState = { ...offState, policy: { enabled: true, services: { web: { min_replicas: 1, max_replicas: 4, target_cpu_percent: 70 } } }, status: "waiting" };
  serve({ state: () => Promise.resolve(onState), nodes: () => Promise.resolve([node({ cpu_autoscale: false })]) });
  renderPage();
  expect(await within(section()).findByText("Worker upgrade needed for CPU scaling")).toBeInTheDocument();
  await waitFor(() => expect(scaleSwitch()).toBeChecked());
  fireEvent.change(within(section()).getByLabelText("Maximum replicas"), { target: { value: "5" } });
  expect(save()).toBeDisabled();
  expect(within(section()).getByText(/does not report CPU scaling support, so it cannot be turned on/)).toBeInTheDocument();

  fireEvent.click(scaleSwitch());
  expect(save()).toBeEnabled();
  fireEvent.click(save());
  await waitFor(() => expect(policyPuts()).toHaveLength(1));
  expect(policyPuts()[0]?.[2]).toEqual({ enabled: false, services: { web: { min_replicas: 1, max_replicas: 5, target_cpu_percent: 70 } } });
});

it("treats unreadable or unreported worker capabilities as unsupported", async () => {
  serve({ nodes: () => Promise.resolve([node(null)]) });
  renderPage();
  expect(await within(section()).findByText("Worker capabilities unknown")).toBeInTheDocument();
  fireEvent.click(scaleSwitch());
  fireEvent.click(webBox());
  expect(save()).toBeDisabled();
  expect(within(section()).getByText(/cannot confirm the worker supports CPU scaling/)).toBeInTheDocument();
});

it("blocks every policy edit while an operation is active", async () => {
  const active: Operation = { id: "op-deploy", workload_id: "w1", action: "deploy", state: "running", result: null, created_at: new Date().toISOString() };
  serve({ operations: () => Promise.resolve([active]) });
  renderPage();
  await waitFor(() => expect(scaleSwitch()).toBeEnabled());
  fireEvent.click(webBox());
  expect(await within(section()).findByText(/Changes can be saved after the active deploy finishes/)).toBeInTheDocument();
  expect(save()).toBeDisabled();
});

it("shows the deployed sample, stale and unknown states, the last error and the last scaling deploy", async () => {
  const queued = new Date(Date.now() - 60_000).toISOString();
  const state: AutoscaleState = {
    policy: { enabled: true, services: { web: { min_replicas: 1, max_replicas: 4, target_cpu_percent: 70 } } },
    samples: [{ sampled_at: new Date(Date.now() - 5 * 60_000).toISOString(), generation: "g1", replicas: { web: 3 }, cpu_percent: { web: 104.25 } }],
    last_queued_at: queued,
    operation_id: "op-scale",
    status: "brand_new_state",
    last_error: "Could not read a complete healthy CPU sample; no scaling was queued.",
  };
  serve({ state: () => Promise.resolve(state), operations: () => Promise.resolve([{ id: "op-scale", workload_id: "w1", action: "deploy", state: "succeeded", result: null, created_at: queued }]) });
  renderPage();
  expect(await within(section()).findByText("Unknown status: brand_new_state")).toBeInTheDocument();
  expect(within(section()).getByText("Stale")).toBeInTheDocument();
  expect(within(section()).getByText("Could not read a complete healthy CPU sample; no scaling was queued.")).toBeInTheDocument();
  const table = within(section()).getByRole("table");
  // The deployed count from the sample, not the configured 2.
  const row = within(table).getByRole("row", { name: /web/ });
  expect(within(row).getByText("104.3%")).toBeInTheDocument();
  expect(within(row).getByText("3")).toBeInTheDocument();
  expect(within(section()).getByText("1 of 3")).toBeInTheDocument();
  expect(await within(section()).findByText("Succeeded")).toBeInTheDocument();
  expect(within(section()).getByText("Cooldown ends")).toBeInTheDocument();
});

it("shows the policy read-only without edit access", async () => {
  serve();
  renderPage({ ...base, permissions: ["read", "operate"] });
  expect(await within(section()).findByText("Changing this needs editor access. Ask a team owner or admin.")).toBeInTheDocument();
  await waitFor(() => expect(scaleSwitch()).toBeDisabled());
  expect(within(section()).queryByRole("button", { name: "Save changes" })).not.toBeInTheDocument();
});

it("reports a failed policy read with Retry", async () => {
  serve({ state: () => Promise.reject(new ApiError(503, "CPU scaling unavailable")) });
  renderPage();
  expect(await within(section()).findByText("Could not load the CPU scaling policy")).toBeInTheDocument();
  expect(within(section()).getByText("CPU scaling unavailable")).toBeInTheDocument();
  expect(within(section()).getByRole("button", { name: "Retry" })).toBeInTheDocument();
});

it("is not offered for jobs or previews and does not read the policy there", async () => {
  serve();
  renderPage({ ...base, kind: "job", job_service: "web" });
  await screen.findByRole("region", { name: "Deployment safety" });
  expect(screen.queryByRole("region", { name: "CPU scaling" })).not.toBeInTheDocument();
  expect(vi.mocked(request).mock.calls.some(([, path]) => path === "/api/workloads/w1/autoscale-policy")).toBe(false);
});

it("explains a CPU scaling deploy with before and after counts", () => {
  render(<ScaleOutcome scale={{ generation: "g1", before: { web: 2 }, replicas: { web: 3 }, cpu_percent: { web: 85 } }} />);
  expect(screen.getByText("CPU scaling deployed the current release with new replica counts.")).toBeInTheDocument();
  expect(screen.getByText(/2 to 3 replicas at 85% CPU of limit/)).toBeInTheDocument();
});

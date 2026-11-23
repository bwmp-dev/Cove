import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import type { ReactNode } from "react";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { ApiError, request } from "../api/client";
import type { EnvironmentSuspension, IdlePolicy, IdleRuntime, Node, Operation, Workload, WorkloadRuntime } from "../api/types";
import { IdleHoldNotice, useIdleState } from "../components/IdleSleep";
import { RuntimeContainers } from "../components/Runtime";
import { ResourceDeploymentSafety } from "./resource-config";

let resource: Workload;

vi.mock("@tanstack/react-router", () => ({ Link: ({ children }: { children: ReactNode }) => <a href="/">{children}</a>, useBlocker: () => ({ status: "idle" }), useRouter: () => undefined }));
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
} as Workload;

const off: IdlePolicy = { enabled: false, after_seconds: 300, request_driven: false };
const on: IdlePolicy = { enabled: true, after_seconds: 120, request_driven: true };

function node(capabilities: Partial<NonNullable<Node["capabilities"]>> | null = { idle_sleep: true }, state: Node["state"] = "ready"): Node {
  return { id: "n1", name: "worker-1", address: "", port: 22, state, mode: "ssh", last_checked_at: null, capabilities: capabilities && { protocol: 1, os: "linux", arch: "amd64", docker: "27", compose: "2", ready: true, rollouts: true, unhealthy_restart: true, ...capabilities } };
}

const idle = (phase: string, extra: Partial<IdleRuntime> = {}): WorkloadRuntime => ({ containers: [], idle: { enabled: true, after_seconds: 120, phase, active_requests: 0, ...extra } });

interface Serve {
  policy?: () => Promise<IdlePolicy>;
  put?: (body: IdlePolicy) => Promise<unknown>;
  nodes?: () => Promise<Node[]>;
  operations?: () => Promise<Operation[]>;
  runtime?: () => Promise<WorkloadRuntime>;
  suspension?: () => Promise<EnvironmentSuspension>;
}

function serve({
  policy = () => Promise.resolve(off),
  put = () => Promise.resolve({ id: "op-policy", state: "queued" }),
  nodes = () => Promise.resolve([node()]),
  operations = () => Promise.resolve([]),
  runtime = () => Promise.resolve({ containers: [], idle: null }),
  suspension = () => Promise.resolve({ paused: false, phase: "active", resources: [] }),
}: Serve = {}) {
  vi.mocked(request).mockImplementation((method: string, path: string, body?: unknown) => {
    if (path === "/api/workloads/w1/idle-policy") return method === "PUT" ? put(body as IdlePolicy) : policy();
    if (path === "/api/workloads/w1/restart-policy") return Promise.resolve({ enabled: true, last_queued_at: null, attempts_in_window: 0, window_started_at: null, operation_id: null, last_error: "" });
    if (path === "/api/workloads/w1/operations" && method === "POST") return Promise.resolve({ id: "op-start", state: "queued" });
    if (path === "/api/nodes") return nodes();
    if (path.startsWith("/api/operations")) return operations();
    if (path === "/api/workloads/w1/runtime") return runtime();
    if (path === "/api/environments/e1/suspension") return suspension();
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

const section = () => screen.getByRole("region", { name: "Idle sleep" });
const sleepSwitch = () => within(section()).getByRole("switch", { name: "Sleep when idle" });
const timeout = () => within(section()).getByLabelText("Idle timeout");
const assertion = () => within(section()).getByRole("checkbox", { name: "Every service is a request-driven HTTP app" });
const save = () => within(section()).getByRole("button", { name: "Save changes" });
const puts = () => vi.mocked(request).mock.calls.filter(([method, path]) => method === "PUT" && path === "/api/workloads/w1/idle-policy");

it("requires the request-driven assertion, then queues exactly the policy fields and keeps the input until the worker applies it", { timeout: 10_000 }, async () => {
  let operations: Operation[] = [];
  let saved = off;
  serve({ operations: () => Promise.resolve(operations), policy: () => Promise.resolve(saved) });
  renderPage();
  await waitFor(() => expect(sleepSwitch()).toBeEnabled());
  expect(within(section()).getByText("Off")).toBeInTheDocument();
  expect(timeout()).toHaveValue("300");

  fireEvent.click(sleepSwitch());
  fireEvent.change(timeout(), { target: { value: "120" } });
  fireEvent.click(save());
  expect(await within(section()).findByText(/Confirm that every service is a request-driven HTTP app/)).toBeInTheDocument();
  expect(assertion()).toHaveAttribute("aria-invalid", "true");
  expect(puts()).toHaveLength(0);

  fireEvent.click(assertion());
  // The queued operation appears once the mutation refreshes operations, and it is polled until it finishes.
  operations = [{ id: "op-policy", workload_id: "w1", action: "idle_policy", state: "queued", result: null, created_at: new Date().toISOString() }];
  fireEvent.click(save());
  await waitFor(() => expect(puts()).toHaveLength(1));
  expect(await within(section()).findByText("Queued. The saved policy changes only after the worker applies it.")).toBeInTheDocument();
  expect(puts()[0]?.[2]).toEqual({ enabled: true, after_seconds: 120, request_driven: true });
  // Queued is not saved: the saved policy stays off while the input is kept.
  expect(within(section()).getByText("Off")).toBeInTheDocument();
  expect(sleepSwitch()).toBeChecked();
  expect(timeout()).toHaveValue("120");

  operations = [{ id: "op-policy", workload_id: "w1", action: "idle_policy", state: "succeeded", result: null, created_at: new Date().toISOString() }];
  saved = on;
  expect(await within(section()).findByText("On, after 2 minutes idle", undefined, { timeout: 5_000 })).toBeInTheDocument();
  expect(within(section()).getByText("Applied by the worker.")).toBeInTheDocument();
});

it("keeps the input and shows the worker's reason when the policy change fails", async () => {
  const failed: Operation = { id: "op-policy", workload_id: "w1", action: "idle_policy", state: "failed", result: { id: "op-policy", workload: "w1", action: "idle_policy", state: "failed", created_at: new Date().toISOString(), error: "idle sleep requires a Docker healthcheck for every service" }, created_at: new Date().toISOString() };
  let operations: Operation[] = [];
  serve({ operations: () => Promise.resolve(operations) });
  renderPage();
  await waitFor(() => expect(sleepSwitch()).toBeEnabled());
  fireEvent.click(sleepSwitch());
  fireEvent.click(assertion());
  operations = [failed];
  fireEvent.click(save());
  await waitFor(() => expect(puts()).toHaveLength(1));
  expect(await within(section()).findByText("idle sleep requires a Docker healthcheck for every service", undefined, { timeout: 3_000 })).toBeInTheDocument();
  expect(within(section()).getByText(/Your changes are kept/)).toBeInTheDocument();
  expect(sleepSwitch()).toBeChecked();
  expect(within(section()).getByText("Off")).toBeInTheDocument();
});

it("shows an API rejection without losing the input", async () => {
  serve({ put: () => Promise.reject(new ApiError(409, "private connections must be removed before idle sleep")) });
  renderPage();
  await waitFor(() => expect(sleepSwitch()).toBeEnabled());
  fireEvent.click(sleepSwitch());
  fireEvent.click(assertion());
  fireEvent.click(save());
  expect(await within(section()).findByText("Could not queue the policy change")).toBeInTheDocument();
  expect(within(section()).getByText("private connections must be removed before idle sleep")).toBeInTheDocument();
  expect(sleepSwitch()).toBeChecked();
});

it("blocks every change on a worker without idle sleep support, including turning a saved policy off", async () => {
  serve({ policy: () => Promise.resolve(on), nodes: () => Promise.resolve([node({ idle_sleep: false })]) });
  renderPage();
  expect(await within(section()).findByText("Worker upgrade needed for idle sleep")).toBeInTheDocument();
  expect(within(section()).getByText(/cannot be turned off until then/)).toBeInTheDocument();
  await waitFor(() => expect(sleepSwitch()).toBeChecked());
  expect(sleepSwitch()).toBeDisabled();
  expect(within(section()).getByText("On, after 2 minutes idle")).toBeInTheDocument();
});

it("does not call a worker old when its capabilities are unknown or cannot be read", async () => {
  serve({ nodes: () => Promise.resolve([node(null)]) });
  renderPage();
  expect(await within(section()).findByText("Worker capabilities unknown")).toBeInTheDocument();
  expect(within(section()).queryByText("Worker upgrade needed for idle sleep")).not.toBeInTheDocument();
});

it("shows a capability read failure with Retry instead of an upgrade notice", async () => {
  serve({ nodes: () => Promise.reject(new ApiError(503, "nodes unavailable")) });
  renderPage();
  expect(await within(section()).findByText("Could not load worker capabilities")).toBeInTheDocument();
  expect(within(section()).queryByText("Worker upgrade needed for idle sleep")).not.toBeInTheDocument();
  await waitFor(() => expect(sleepSwitch()).toBeDisabled());
});

it("lets viewers see the saved policy and the worker's state without editing", async () => {
  serve({ policy: () => Promise.resolve(on), runtime: () => Promise.resolve(idle("awake", { active_requests: 2, last_activity: new Date(Date.now() - 30_000).toISOString() })) });
  renderPage({ ...base, permissions: ["read"] });
  expect(await within(section()).findByText("On, after 2 minutes idle")).toBeInTheDocument();
  expect(await within(section()).findByText("Awake")).toBeInTheDocument();
  expect(within(section()).getByText("2 requests")).toBeInTheDocument();
  expect(within(section()).getByText("Idle timer reset")).toBeInTheDocument();
  expect(within(section()).getByText(/needs editor access/)).toBeInTheDocument();
  expect(within(section()).queryByRole("button", { name: "Save changes" })).not.toBeInTheDocument();
  expect(within(section()).queryByRole("button", { name: "Wake now" })).not.toBeInTheDocument();
  expect(sleepSwitch()).toBeDisabled();
});

it("blocks saving while another operation is active", async () => {
  const active: Operation = { id: "op-deploy", workload_id: "w1", action: "deploy", state: "running", result: null, created_at: new Date().toISOString() };
  serve({ operations: () => Promise.resolve([active]) });
  renderPage();
  await waitFor(() => expect(sleepSwitch()).toBeEnabled());
  fireEvent.click(sleepSwitch());
  fireEvent.click(assertion());
  expect(await within(section()).findByText(/until the active deploy finishes/)).toBeInTheDocument();
  expect(save()).toBeDisabled();
});

it("blocks saving while offline", async () => {
  vi.spyOn(navigator, "onLine", "get").mockReturnValue(false);
  serve();
  renderPage();
  await waitFor(() => expect(sleepSwitch()).toBeEnabled());
  fireEvent.click(sleepSwitch());
  fireEvent.click(assertion());
  expect(await within(section()).findByText(/You are offline. Changes can be saved/)).toBeInTheDocument();
  expect(save()).toBeDisabled();
});

it("blocks turning sleep on while the environment is paused but allows turning it off", async () => {
  serve({ policy: () => Promise.resolve(on), suspension: () => Promise.resolve({ paused: true, phase: "paused", resources: [] }) });
  renderPage({ ...base, environment_id: "e1", environment_paused: true });
  await waitFor(() => expect(sleepSwitch()).toBeChecked());
  fireEvent.change(timeout(), { target: { value: "600" } });
  expect(await within(section()).findByText(/The environment is paused or resuming/)).toBeInTheDocument();
  expect(save()).toBeDisabled();
  fireEvent.click(sleepSwitch());
  await waitFor(() => expect(save()).toBeEnabled());
  fireEvent.click(save());
  await waitFor(() => expect(puts()[0]?.[2]).toEqual({ enabled: false, after_seconds: 600, request_driven: true }));
});

it("shows an unknown state with Retry when the runtime read fails", async () => {
  serve({ policy: () => Promise.resolve(on), runtime: () => Promise.reject(new ApiError(502, "worker unreachable")) });
  renderPage();
  expect(await within(section()).findByText("Could not load the idle state")).toBeInTheDocument();
  expect(within(section()).getByText("Unknown")).toBeInTheDocument();
  expect(within(section()).getAllByRole("button", { name: "Retry" }).length).toBeGreaterThan(0);
});

it("keeps unrecognised phases unknown", async () => {
  serve({ policy: () => Promise.resolve(on), runtime: () => Promise.resolve(idle("hibernating")) });
  renderPage();
  expect(await within(section()).findByText("Unknown")).toBeInTheDocument();
});

it("wakes a sleeping app with the existing Start operation", async () => {
  serve({ policy: () => Promise.resolve(on), runtime: () => Promise.resolve(idle("sleeping", { last_sleep: new Date(Date.now() - 60_000).toISOString() })) });
  renderPage();
  expect(await within(section()).findByText("Sleeping")).toBeInTheDocument();
  expect(within(section()).getByText("Last sleep")).toBeInTheDocument();
  const wake = await within(section()).findByRole("button", { name: "Wake now" });
  await waitFor(() => expect(wake).toBeEnabled());
  fireEvent.click(wake);
  await waitFor(() => expect(request).toHaveBeenCalledWith("POST", "/api/workloads/w1/operations", { action: "start" }));
  expect(await within(section()).findByText(/Wake queued/)).toBeInTheDocument();
});

it("offers Wake now after a failed wake and shows the worker's error", async () => {
  serve({ policy: () => Promise.resolve(on), runtime: () => Promise.resolve(idle("wake_failed", { error: "health check did not pass within 2 minutes" })) });
  renderPage();
  expect(await within(section()).findByText("Wake failed")).toBeInTheDocument();
  expect(within(section()).getByText("health check did not pass within 2 minutes")).toBeInTheDocument();
  expect(within(section()).getByRole("button", { name: "Wake now" })).toBeInTheDocument();
});

it("does not offer Wake now while the app is waking or going to sleep", async () => {
  serve({ policy: () => Promise.resolve(on), runtime: () => Promise.resolve(idle("quiescing")) });
  renderPage();
  expect(await within(section()).findByText("Going to sleep")).toBeInTheDocument();
  expect(within(section()).queryByRole("button", { name: "Wake now" })).not.toBeInTheDocument();
});

it("shows health restarts as paused while the app sleeps", async () => {
  serve({ policy: () => Promise.resolve(on), runtime: () => Promise.resolve(idle("sleeping")) });
  renderPage();
  const restarts = screen.getByRole("region", { name: "Automatic health restarts" });
  expect(await within(restarts).findByText("Paused")).toBeInTheDocument();
  expect(within(restarts).getByText(/asleep after being idle/)).toBeInTheDocument();
  expect(within(restarts).queryByText("Checking")).not.toBeInTheDocument();
});

it("labels idle-stopped containers as sleeping and summarises the wake separately from an environment resume", async () => {
  const container = { id: "c1", name: "cove-w1-web-1", image: "busybox:1.37", state: "exited", status: "Exited (137) 1 minute ago", ports: "", exit_code: 137 };
  let runtime: WorkloadRuntime = { containers: [container], suspension: { operation: "episode", scope: "w1", reason: "idle", phase: "paused", captured: 1, previously_running: 1 }, idle: { enabled: true, after_seconds: 120, phase: "sleeping", active_requests: 0 } };
  serve({ runtime: () => Promise.resolve(runtime) });
  resource = base;
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const view = render(
    <QueryClientProvider client={client}>
      <RuntimeContainers workloadId="w1" kind="service" operationActive={false} manuallyStopped={false} />
    </QueryClientProvider>,
  );
  expect(await screen.findByText("Sleeping", { selector: "span.whitespace-nowrap" })).toBeInTheDocument();
  expect(screen.queryByText("Paused")).not.toBeInTheDocument();
  expect(screen.getByText(/This app is asleep after being idle/)).toBeInTheDocument();

  runtime = { ...runtime, suspension: { operation: "episode", scope: "w1", reason: "idle", phase: "resuming", captured: 1, previously_running: 1 } };
  await client.refetchQueries();
  expect(await screen.findByText(/The worker is waking this app/)).toBeInTheDocument();
  expect(screen.queryByText(/for its environment/)).not.toBeInTheDocument();
  view.unmount();
});

function HeaderNotice({ workload }: { workload: Workload }) {
  return <IdleHoldNotice workload={workload} idle={useIdleState(workload)} />;
}

function renderNotice(workload: Workload = base) {
  render(
    <QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}>
      <HeaderNotice workload={workload} />
    </QueryClientProvider>,
  );
}

it("explains on resource pages that a sleeping app must wake before deploys", async () => {
  serve({ policy: () => Promise.resolve(on), runtime: () => Promise.resolve(idle("sleeping")) });
  renderNotice();
  expect(await screen.findByText("App is asleep")).toBeInTheDocument();
  expect(screen.getByText(/Deploy, rollback and promotion are available after it wakes/)).toBeInTheDocument();
  expect(screen.getByRole("button", { name: "Wake now" })).toBeInTheDocument();
});

const asleep = () => Promise.resolve(idle("sleeping"));

it.each([
  ["the worker is missing from the node list", () => Promise.resolve([]), "The worker for this resource is not in the node list, so the app cannot be woken."],
  ["the worker has not reported capabilities", () => Promise.resolve([node(null)]), "worker-1 has not reported its capabilities, so idle sleep support cannot be confirmed."],
  ["the worker does not support idle sleep", () => Promise.resolve([node({ idle_sleep: false })]), "worker-1 does not report idle sleep support. Upgrade the worker to wake the app."],
  ["the worker reports capabilities without idle sleep", () => Promise.resolve([node({ idle_sleep: undefined })]), "worker-1 does not report idle sleep support. Upgrade the worker to wake the app."],
  ["the worker is not ready", () => Promise.resolve([node(undefined, "unreachable")]), "worker-1 is not ready. The worker must be online to wake the app."],
])("keeps Wake now disabled when %s", async (_, nodes, reason) => {
  serve({ policy: () => Promise.resolve(on), runtime: asleep, nodes });
  renderNotice();
  const wake = await screen.findByRole("button", { name: "Wake now" });
  await waitFor(() => expect(wake).toHaveAccessibleDescription(reason));
  expect(wake).toBeDisabled();
});

it("keeps Wake now disabled while the worker is loading", async () => {
  serve({ policy: () => Promise.resolve(on), runtime: asleep, nodes: () => new Promise<Node[]>(() => undefined) });
  renderNotice();
  const wake = await screen.findByRole("button", { name: "Wake now" });
  await waitFor(() => expect(wake).toHaveAccessibleDescription("Checking the worker."));
  expect(wake).toBeDisabled();
});

it("keeps Wake now disabled when the worker cannot be read, then enables it after Retry confirms a ready worker", async () => {
  let nodes = () => Promise.reject<Node[]>(new ApiError(503, "nodes unavailable"));
  serve({ policy: () => Promise.resolve(on), runtime: asleep, nodes: () => nodes() });
  renderNotice();
  const wake = await screen.findByRole("button", { name: "Wake now" });
  await waitFor(() => expect(wake).toHaveAccessibleDescription(/Could not load the worker.*nodes unavailable/));
  expect(wake).toBeDisabled();

  nodes = () => Promise.resolve([node()]);
  fireEvent.click(screen.getByRole("button", { name: "Retry" }));
  await waitFor(() => expect(wake).toBeEnabled());
  expect(screen.queryByRole("button", { name: "Retry" })).not.toBeInTheDocument();
  fireEvent.click(wake);
  await waitFor(() => expect(request).toHaveBeenCalledWith("POST", "/api/workloads/w1/operations", { action: "start" }));
});

it("keeps Wake now disabled with the error when active operations cannot be read, then enables it after Retry", async () => {
  let operations = () => Promise.reject<Operation[]>(new ApiError(503, "operations unavailable"));
  serve({ policy: () => Promise.resolve(on), runtime: asleep, operations: () => operations() });
  renderNotice();
  const wake = await screen.findByRole("button", { name: "Wake now" });
  await waitFor(() => expect(wake).toHaveAccessibleDescription(/Could not load active operations.*operations unavailable/));
  expect(wake).toBeDisabled();
  expect(screen.queryByText("Checking for active operations.")).not.toBeInTheDocument();

  operations = () => Promise.resolve([]);
  fireEvent.click(screen.getByRole("button", { name: "Retry" }));
  await waitFor(() => expect(wake).toBeEnabled());
  expect(screen.queryByRole("button", { name: "Retry" })).not.toBeInTheDocument();
});

it("does not trust cached operations after a failed refetch", async () => {
  let operations = () => Promise.resolve<Operation[]>([]);
  serve({ policy: () => Promise.resolve(on), runtime: asleep, operations: () => operations() });
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(
    <QueryClientProvider client={client}>
      <HeaderNotice workload={base} />
    </QueryClientProvider>,
  );
  const wake = await screen.findByRole("button", { name: "Wake now" });
  await waitFor(() => expect(wake).toBeEnabled());

  operations = () => Promise.reject<Operation[]>(new ApiError(503, "operations unavailable"));
  await client.refetchQueries({ queryKey: ["operations"] });
  await waitFor(() => expect(wake).toHaveAccessibleDescription(/Could not load active operations.*operations unavailable/));
  expect(wake).toBeDisabled();

  operations = () => Promise.resolve([]);
  fireEvent.click(screen.getByRole("button", { name: "Retry" }));
  await waitFor(() => expect(wake).toBeEnabled());
});

it("offers one Retry for unreadable operations in the idle sleep settings", async () => {
  serve({ policy: () => Promise.resolve(on), runtime: asleep, operations: () => Promise.reject(new ApiError(503, "operations unavailable")) });
  renderPage();
  expect(await within(section()).findByText("Could not load active operations")).toBeInTheDocument();
  const wake = await within(section()).findByRole("button", { name: "Wake now" });
  await waitFor(() => expect(wake).toHaveAccessibleDescription(/Could not load active operations.*operations unavailable/));
  expect(wake).toBeDisabled();
  expect(within(section()).getAllByRole("button", { name: "Retry" })).toHaveLength(1);
});

it("reports an unreadable idle state as unknown with Retry when the saved policy is on", async () => {
  serve({ policy: () => Promise.resolve(on), runtime: () => Promise.reject(new ApiError(502, "worker unreachable")) });
  renderNotice();
  expect(await screen.findByText("Idle sleep state unknown")).toBeInTheDocument();
  expect(screen.getByText(/worker unreachable/)).toBeInTheDocument();
  expect(screen.getByRole("button", { name: "Retry" })).toBeInTheDocument();
});

it("shows no idle notice for an unreadable state when the saved policy is off, or for jobs and previews", async () => {
  serve({ runtime: () => Promise.reject(new ApiError(502, "worker unreachable")) });
  renderNotice();
  await waitFor(() => expect(request).toHaveBeenCalledWith("GET", "/api/workloads/w1/idle-policy", undefined, expect.anything()));
  expect(screen.queryByText("Idle sleep state unknown")).not.toBeInTheDocument();
  renderNotice({ ...base, preview_source_id: "w0" });
  expect(screen.queryByText("App is asleep")).not.toBeInTheDocument();
});

it("labels containers the worker holds after a manual stop as stopped, not paused or sleeping, while live health and completed init win", async () => {
  const containers = [
    { id: "c1", name: "cove-w1-web-1", image: "busybox:1.37", state: "exited", status: "Exited (137) 1 minute ago", ports: "", exit_code: 137 },
    { id: "c2", name: "cove-w1-init-1", image: "busybox:1.37", state: "exited", status: "Exited (0) 5 minutes ago", ports: "", exit_code: 0, expected_completed: true },
    { id: "c3", name: "cove-w1-api-1", image: "busybox:1.37", state: "running", status: "Up 1 minute (healthy)", ports: "", health: "healthy" as const },
  ];
  serve({ runtime: () => Promise.resolve({ containers, suspension: { operation: "stop", scope: "w1", reason: "manual_stop", phase: "paused", captured: 1, previously_running: 1 }, idle: null }) });
  render(
    <QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}>
      <RuntimeContainers workloadId="w1" kind="service" operationActive={false} manuallyStopped={false} />
    </QueryClientProvider>,
  );
  expect(await screen.findByText("Stopped", { selector: "span.whitespace-nowrap" })).toBeInTheDocument();
  expect(screen.getByText("Completed", { selector: "span.whitespace-nowrap" })).toBeInTheDocument();
  expect(screen.getByText("Healthy", { selector: "span.whitespace-nowrap" })).toBeInTheDocument();
  expect(screen.queryByText("Paused", { selector: "span.whitespace-nowrap" })).not.toBeInTheDocument();
  expect(screen.queryByText("Sleeping", { selector: "span.whitespace-nowrap" })).not.toBeInTheDocument();
  expect(screen.getByText(/This resource was stopped intentionally/)).toBeInTheDocument();
  expect(screen.queryByText(/environment/i)).not.toBeInTheDocument();
});

it("does not offer Wake now or an idle hold for a worker-held manual stop", async () => {
  serve({ policy: () => Promise.resolve(on), runtime: () => Promise.resolve({ ...idle("paused"), suspension: { operation: "stop", scope: "w1", reason: "manual_stop", phase: "paused", captured: 1, previously_running: 1 } }) });
  renderNotice();
  await waitFor(() => expect(request).toHaveBeenCalledWith("GET", "/api/workloads/w1/runtime", undefined, expect.anything()));
  expect(screen.queryByText("App is asleep")).not.toBeInTheDocument();
  expect(screen.queryByRole("button", { name: "Wake now" })).not.toBeInTheDocument();
});

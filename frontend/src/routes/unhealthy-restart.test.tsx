import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import type { ReactNode } from "react";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { ApiError, request } from "../api/client";
import type { Node, Operation, RestartPolicy, Workload, WorkloadRuntime } from "../api/types";
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

const off: RestartPolicy = { enabled: false, last_queued_at: null, attempts_in_window: 0, window_started_at: null, operation_id: null, last_error: "" };

function node(unhealthyRestart: boolean): Node {
  return { id: "n1", name: "worker-1", address: "", port: 22, state: "ready", mode: "ssh", last_checked_at: null, capabilities: { protocol: 1, os: "linux", arch: "amd64", docker: "27", compose: "2", ready: true, rollouts: true, unhealthy_restart: unhealthyRestart } };
}

interface Serve {
  policy?: () => Promise<RestartPolicy>;
  put?: (body: { enabled: boolean }) => Promise<RestartPolicy>;
  nodes?: () => Promise<Node[]>;
  operations?: () => Promise<Operation[]>;
  runtime?: () => Promise<WorkloadRuntime>;
}

function serve({
  policy = () => Promise.resolve(off),
  put = (body) => Promise.resolve({ ...off, ...body }),
  nodes = () => Promise.resolve([node(true)]),
  operations = () => Promise.resolve([]),
  runtime = () => Promise.resolve({ containers: [] }),
}: Serve = {}) {
  vi.mocked(request).mockImplementation((method: string, path: string, body?: unknown) => {
    if (path === "/api/workloads/w1/restart-policy") return method === "PUT" ? put(body as { enabled: boolean }) : policy();
    if (path === "/api/nodes") return nodes();
    if (path.startsWith("/api/operations")) return operations();
    if (path === "/api/workloads/w1/runtime") return runtime();
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

const section = () => screen.getByRole("region", { name: "Automatic health restarts" });
const restartSwitch = () => within(section()).getByRole("switch", { name: "Restart unhealthy containers" });
const puts = () => vi.mocked(request).mock.calls.filter(([method]) => method === "PUT");

it("enables the policy through its own endpoint without saving the deployment form", async () => {
  serve();
  renderPage();
  await waitFor(() => expect(restartSwitch()).toBeEnabled());
  expect(restartSwitch()).not.toBeChecked();
  expect(within(section()).getByText("Off")).toBeInTheDocument();
  fireEvent.click(restartSwitch());
  fireEvent.click(within(section()).getByRole("button", { name: "Save changes" }));
  await waitFor(() => expect(puts()).toHaveLength(1));
  expect(puts()[0]).toEqual(["PUT", "/api/workloads/w1/restart-policy", { enabled: true }]);
  expect(await within(section()).findByText(/no deploy is needed/)).toBeInTheDocument();
  expect(restartSwitch()).toBeChecked();
  expect(within(section()).getByText("Checking")).toBeInTheDocument();
});

it("shows attempts, the hourly limit, the last operation and the last health error from the API", async () => {
  const minutesAgo = (minutes: number) => new Date(Date.now() - minutes * 60_000).toISOString();
  const operation: Operation = { id: "op9", workload_id: "w1", action: "restart_unhealthy", state: "succeeded", result: null, created_at: minutesAgo(10) };
  serve({
    policy: () => Promise.resolve({ enabled: true, last_queued_at: minutesAgo(10), attempts_in_window: 3, window_started_at: minutesAgo(50), operation_id: "op9", last_error: "Could not inspect Docker health; no automatic restart was queued." }),
    operations: () => Promise.resolve([operation]),
  });
  renderPage();
  expect(await within(section()).findByText("3 of 3")).toBeInTheDocument();
  expect(within(section()).getByText("Queued attempts in the last hour")).toBeInTheDocument();
  expect(within(section()).getByText(/Oldest counted attempt/)).toBeInTheDocument();
  expect(within(section()).getByText("Last queued restart")).toBeInTheDocument();
  expect(within(section()).queryByText(/Restarts in the last hour|Oldest counted restart|Last automatic restart/)).not.toBeInTheDocument();
  expect(within(section()).getByText("Limit reached")).toBeInTheDocument();
  expect(within(section()).getByText("Succeeded")).toBeInTheDocument();
  expect(within(section()).getByText("View operation")).toBeInTheDocument();
  expect(within(section()).getByText("Last health check failed")).toBeInTheDocument();
  expect(within(section()).getByText("Could not inspect Docker health; no automatic restart was queued.")).toBeInTheDocument();
  expect(within(section()).getByText(/does not reset the count/)).toBeInTheDocument();
});

it("blocks enabling on a worker without support but still allows turning the policy off", async () => {
  serve({ nodes: () => Promise.resolve([node(false)]) });
  renderPage();
  expect(await within(section()).findByText("Worker upgrade needed for health restarts")).toBeInTheDocument();
  expect(restartSwitch()).toBeDisabled();
});

it("turns a saved policy off on an old worker", async () => {
  const counted = { attempts_in_window: 1, window_started_at: new Date().toISOString() };
  serve({ nodes: () => Promise.resolve([node(false)]), policy: () => Promise.resolve({ ...off, ...counted, enabled: true }), put: (body) => Promise.resolve({ ...off, ...counted, ...body }) });
  renderPage();
  expect(await within(section()).findByText(/You can still turn the saved policy off/)).toBeInTheDocument();
  expect(within(section()).getByText("Not checking")).toBeInTheDocument();
  await waitFor(() => expect(restartSwitch()).toBeEnabled());
  fireEvent.click(restartSwitch());
  fireEvent.click(within(section()).getByRole("button", { name: "Save changes" }));
  await waitFor(() => expect(puts()[0]).toEqual(["PUT", "/api/workloads/w1/restart-policy", { enabled: false }]));
  await waitFor(() => expect(restartSwitch()).toBeDisabled());
  expect(within(section()).getByText("1 of 3")).toBeInTheDocument();
});

it("shows the server's reason when saving fails and keeps the unsaved choice", async () => {
  serve({ put: () => Promise.reject(new ApiError(409, "upgrade and verify the worker before enabling health restarts")) });
  renderPage();
  await waitFor(() => expect(restartSwitch()).toBeEnabled());
  fireEvent.click(restartSwitch());
  fireEvent.click(within(section()).getByRole("button", { name: "Save changes" }));
  expect(await within(section()).findByText("upgrade and verify the worker before enabling health restarts")).toBeInTheDocument();
  expect(within(section()).getByText("Could not save the restart policy")).toBeInTheDocument();
  expect(restartSwitch()).toBeChecked();
});

it("keeps the switch disabled while the policy is unknown and offers a retry after a failed read", async () => {
  let fail = true;
  serve({ policy: () => (fail ? Promise.reject(new ApiError(503, "restart policy unavailable")) : Promise.resolve(off)) });
  renderPage();
  expect(within(section()).getByText("Loading the restart policy")).toBeInTheDocument();
  expect(within(section()).queryByRole("switch")).not.toBeInTheDocument();
  expect(await within(section()).findByText("Could not load the restart policy")).toBeInTheDocument();
  expect(within(section()).queryByRole("button", { name: "Save changes" })).not.toBeInTheDocument();
  fail = false;
  fireEvent.click(within(section()).getByRole("button", { name: "Retry" }));
  await waitFor(() => expect(restartSwitch()).toBeEnabled());
});

it("blocks saving while offline", async () => {
  vi.spyOn(navigator, "onLine", "get").mockReturnValue(false);
  serve();
  renderPage();
  await waitFor(() => expect(restartSwitch()).toBeEnabled());
  fireEvent.click(restartSwitch());
  expect(within(section()).getByText("You are offline")).toBeInTheDocument();
  expect(within(section()).getByRole("button", { name: "Save changes" })).toBeDisabled();
});

it("shows the policy read-only without edit access", async () => {
  serve({ policy: () => Promise.resolve({ ...off, enabled: true }) });
  renderPage({ ...base, permissions: ["read", "operate"] });
  await waitFor(() => expect(restartSwitch()).toBeChecked());
  expect(restartSwitch()).toBeDisabled();
  expect(within(section()).getByText(/Changing this needs editor access/)).toBeInTheDocument();
  expect(within(section()).queryByRole("button", { name: "Save changes" })).not.toBeInTheDocument();
});

it("explains that a manually stopped resource is skipped", async () => {
  serve({ policy: () => Promise.resolve({ ...off, enabled: true }) });
  renderPage({ ...base, manually_stopped: true });
  expect(await within(section()).findByText("Paused")).toBeInTheDocument();
  expect(within(section()).getByText(/stopped manually/)).toBeInTheDocument();
});

it("offers no automatic restart controls for jobs or managed previews", async () => {
  serve();
  renderPage({ ...base, kind: "job" });
  await screen.findByText(/Jobs always use Compose replacement/);
  expect(screen.queryByRole("region", { name: "Automatic health restarts" })).not.toBeInTheDocument();
  expect(vi.mocked(request).mock.calls.some(([, path]) => path === "/api/workloads/w1/restart-policy")).toBe(false);
});

const enabled: RestartPolicy = { ...off, enabled: true };
const failure = () => Promise.reject(new ApiError(503, "temporarily unavailable"));

it("shows the status as loading, not checking, while the worker and operations are being read", async () => {
  serve({ policy: () => Promise.resolve(enabled), nodes: () => new Promise(() => {}), operations: () => new Promise(() => {}) });
  renderPage();
  expect(await within(section()).findByText("Loading")).toBeInTheDocument();
  expect(within(section()).getByText(/Reading the worker, staged rollouts and active operations/)).toBeInTheDocument();
  expect(within(section()).queryByText("Checking")).not.toBeInTheDocument();
});

it.each([
  ["a worker missing from the node list", [] as Node[], "Worker unknown"],
  ["a worker that has not reported capabilities", [{ ...node(true), capabilities: null }], "Worker capabilities unknown"],
])("reports an unknown status for %s", async (_, nodes, notice) => {
  serve({ policy: () => Promise.resolve(enabled), nodes: () => Promise.resolve(nodes) });
  renderPage();
  expect(await within(section()).findByText(notice)).toBeInTheDocument();
  expect(within(section()).getByText("Unknown")).toBeInTheDocument();
  expect(within(section()).getByText(/because the worker( and staged rollouts)? could not be read/)).toBeInTheDocument();
  expect(within(section()).queryByText("Checking")).not.toBeInTheDocument();
  expect(within(section()).queryByText("Worker upgrade needed for health restarts")).not.toBeInTheDocument();
  await waitFor(() => expect(restartSwitch()).toBeEnabled());
});

it.each([
  ["nodes", "Could not load worker capabilities", /because the worker( and staged rollouts)? could not be read/],
  ["operations", "Could not load active operations", /active operations could not be read/],
  ["runtime", "Could not load the staged rollout state", /staged rollouts could not be read/],
] as const)("reports an unknown status when the %s read fails and recovers after a retry", async (read, title, detail) => {
  let fail = true;
  const working: Required<Pick<Serve, "nodes" | "operations" | "runtime">> = {
    nodes: () => Promise.resolve([node(true)]),
    operations: () => Promise.resolve([]),
    runtime: () => Promise.resolve({ containers: [] }),
  };
  const ok = working[read] as () => Promise<never>;
  serve({ ...working, policy: () => Promise.resolve(enabled), [read]: () => (fail ? failure() : ok()) });
  renderPage();
  const error = (await within(section()).findByText(title)).closest<HTMLElement>('[role="alert"]');
  expect(error).not.toBeNull();
  expect(within(section()).getByText("Unknown")).toBeInTheDocument();
  expect(within(section()).getByText(detail)).toBeInTheDocument();
  expect(within(section()).queryByText("Checking")).not.toBeInTheDocument();
  fail = false;
  fireEvent.click(within(error!).getByRole("button", { name: "Retry" }));
  expect(await within(section()).findByText("Checking")).toBeInTheDocument();
});

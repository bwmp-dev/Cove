import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import type { ReactNode } from "react";
import { afterEach, expect, it, vi } from "vitest";
import { request } from "../api/client";
import type { Node, Operation, Workload, WorkloadRuntime } from "../api/types";
import { StagedRollout } from "./StagedRollout";

vi.mock("@tanstack/react-router", () => ({ Link: ({ children }: { children: ReactNode }) => <a href="/">{children}</a> }));
vi.mock("../api/client", async (importOriginal) => ({ ...(await importOriginal<typeof import("../api/client")>()), request: vi.fn() }));
afterEach(() => vi.clearAllMocks());
// jsdom does not implement modal dialogs.
HTMLDialogElement.prototype.showModal = function (this: HTMLDialogElement) {
  this.open = true;
};
HTMLDialogElement.prototype.close = function (this: HTMLDialogElement) {
  this.open = false;
};

const workload = { id: "w1", name: "web", node_id: "n1", kind: "service", permissions: ["read", "operate"], manually_stopped: false, deployed: true } as Workload;
const staged: WorkloadRuntime = {
  containers: [],
  rollout: { operation: "op-candidate", release: "op-candidate", strategy: "canary", phase: "waiting", candidate_percent: 25, replicas: { "a-very-long-compose-service-name-for-the-api": 3 } },
};

function node(rollouts = true): Node {
  return { id: "n1", name: "worker-1", address: "", port: 22, state: "ready", mode: "ssh", last_checked_at: null, capabilities: { protocol: 1, os: "linux", arch: "amd64", docker: "27", compose: "2", ready: true, rollouts } };
}

function serve({ runtime, nodes = [node()], operations = [] }: { runtime: WorkloadRuntime | Error; nodes?: Node[]; operations?: Operation[] | Error }) {
  vi.mocked(request).mockImplementation((method: string, path: string) => {
    if (method === "POST") return Promise.resolve({ id: "op-action", state: "queued" });
    if (path === "/api/nodes") return Promise.resolve(nodes);
    if (path.startsWith("/api/operations")) return operations instanceof Error ? Promise.reject(operations) : Promise.resolve(operations);
    if (path.endsWith("/runtime")) return runtime instanceof Error ? Promise.reject(runtime) : Promise.resolve(runtime);
    return Promise.reject(new Error(`unexpected ${method} ${path}`));
  });
}

function renderPanel(target: Workload = workload) {
  render(
    <QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}>
      <StagedRollout workload={target} operationActive={false} />
    </QueryClientProvider>,
  );
}

it("shows the staged candidate and promotes it after confirmation", async () => {
  serve({ runtime: staged });
  renderPanel();
  expect(await screen.findByText("Waiting for promotion")).toBeInTheDocument();
  expect(screen.getByText("The candidate receives 25% of traffic; the current release serves the rest.")).toBeInTheDocument();
  expect(screen.getByText("a-very-long-compose-service-name-for-the-api")).toBeInTheDocument();
  const promote = screen.getByRole("button", { name: "Promote" });
  await waitFor(() => expect(promote).toBeEnabled());
  fireEvent.click(promote);
  expect(screen.getByText(/receives all traffic and the current release drains/)).toBeInTheDocument();
  expect(request).not.toHaveBeenCalledWith("POST", expect.anything(), expect.anything());
  fireEvent.click(screen.getByRole("button", { name: "Promote release" }));
  await waitFor(() => expect(request).toHaveBeenCalledWith("POST", "/api/workloads/w1/operations", { action: "rollout_promote" }));
  expect(await screen.findByText("Promote rollout queued")).toBeInTheDocument();
});

it("aborts only after confirming that the candidate is discarded", async () => {
  serve({ runtime: staged });
  renderPanel();
  const abort = await screen.findByRole("button", { name: "Abort" });
  await waitFor(() => expect(abort).toBeEnabled());
  fireEvent.click(abort);
  expect(screen.getByText(/containers are removed/)).toBeInTheDocument();
  fireEvent.click(screen.getByRole("button", { name: "Abort rollout" }));
  await waitFor(() => expect(request).toHaveBeenCalledWith("POST", "/api/workloads/w1/operations", { action: "rollout_abort" }));
});

it("disables promote and abort while another operation is active or the worker is too old", async () => {
  serve({ runtime: staged, operations: [{ id: "op2", workload_id: "w1", action: "restart", state: "running", result: null, created_at: "2026-11-22T10:00:00Z" }] });
  renderPanel();
  expect(await screen.findByText(/Restart running\. Promote and abort are available when it finishes\./)).toBeInTheDocument();
  expect(screen.getByRole("button", { name: "Promote" })).toBeDisabled();
  expect(screen.getByRole("button", { name: "Abort" })).toBeDisabled();
});

it("treats a failed operations read as unknown and blocks promote and abort until a retry succeeds", async () => {
  serve({ runtime: staged, operations: new Error("operations unavailable") });
  renderPanel();
  expect(await screen.findByText("Could not load operations")).toBeInTheDocument();
  expect(screen.getByText(/Cove cannot tell whether another operation is running/)).toBeInTheDocument();
  expect(screen.getByRole("button", { name: "Promote" })).toBeDisabled();
  expect(screen.getByRole("button", { name: "Abort" })).toBeDisabled();

  serve({ runtime: staged });
  fireEvent.click(screen.getByRole("button", { name: "Retry" }));
  await waitFor(() => expect(screen.getByRole("button", { name: "Promote" })).toBeEnabled());
  expect(screen.queryByText("Could not load operations")).not.toBeInTheDocument();
  expect(screen.getByRole("button", { name: "Abort" })).toBeEnabled();
});

it("offers no actions without operator access", async () => {
  serve({ runtime: staged });
  renderPanel({ ...workload, permissions: ["read"] });
  expect(await screen.findByText("Promoting or aborting needs operator access.")).toBeInTheDocument();
  expect(screen.queryByRole("button", { name: "Promote" })).not.toBeInTheDocument();
});

it("renders nothing when no rollout is pending or the runtime read fails", async () => {
  serve({ runtime: { containers: [], rollout: null } });
  const { unmount } = render(
    <QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}>
      <StagedRollout workload={workload} operationActive={false} />
    </QueryClientProvider>,
  );
  await waitFor(() => expect(request).toHaveBeenCalledWith("GET", "/api/workloads/w1/runtime", undefined, expect.anything()));
  expect(screen.queryByText("Staged rollout")).not.toBeInTheDocument();
  unmount();

  serve({ runtime: new Error("worker unreachable") });
  renderPanel();
  await waitFor(() => expect(request).toHaveBeenCalledWith("GET", "/api/workloads/w1/runtime", undefined, expect.anything()));
  expect(screen.queryByRole("button", { name: "Promote" })).not.toBeInTheDocument();
});

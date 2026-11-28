import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { ApiError, request } from "../api/client";
import type { MovePreview, WorkloadMove as Move } from "../api/moves";
import type { Node, Operation, Workload } from "../api/types";
import { WorkloadMove } from "./WorkloadMove";

const session = vi.hoisted(() => ({ current: { is_admin: false, team_id: "t1" } }));
vi.mock("../lib/session", () => ({ useSession: () => session.current }));
vi.mock("../api/client", async (original) => ({ ...(await original<typeof import("../api/client")>()), request: vi.fn() }));
vi.mock("@tanstack/react-router", () => ({ Link: ({ children }: { children: React.ReactNode }) => <a href="#operation">{children}</a> }));
HTMLDialogElement.prototype.showModal = function (this: HTMLDialogElement) {
  this.open = true;
};
HTMLDialogElement.prototype.close = function (this: HTMLDialogElement) {
  this.open = false;
};

beforeEach(() => {
  vi.stubGlobal("ResizeObserver", class { observe = vi.fn(); disconnect = vi.fn(); });
  session.current = { is_admin: false, team_id: "t1" };
});
afterEach(() => {
  vi.resetAllMocks();
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

const capabilities = { protocol: 1, os: "linux", arch: "amd64", docker: "27", compose: "2", ready: true, workload_moves: true };
const node = (id: string, name: string, overrides: Partial<Node> = {}): Node => ({ id, name, address: `${id}.example`, port: 22, mode: "ssh", state: "ready", last_checked_at: null, capabilities, ...overrides });
const nodes = [node("n1", "alpha"), node("n2", "bravo"), node("n3", "charlie", { capabilities: { ...capabilities, workload_moves: false } }), node("n4", "delta", { state: "unreachable" })];

const workload: Workload = { id: "w1", name: "shop", node_id: "n1", kind: "service", permissions: ["read", "operate", "edit", "edit_configuration", "read_secrets"], manually_stopped: false, deployed: true, project_id: "p1", compose: "", environment_keys: [], source: null, has_source_token: false };

const plan = { root: "/var/lib/cove", daemon: "daemon-secret-id", architecture: "amd64", volumes: [{ name: "shop_data", labels: { "com.docker.compose.volume": "data" } }, { name: "cove-backups-w1" }], containers: 3, running: 2, max_bytes: 64 * 1024 ** 3 };
const preview: MovePreview = { source_node_id: "n1", destination_node_id: "n2", source: plan, target: { ...plan, daemon: "other", volumes: [], containers: 0, running: 0 }, revision: "rev-abc" };

interface Setup {
  move?: Move | null;
  operations?: Operation[];
  queue?: () => Promise<unknown>;
  preview?: () => Promise<unknown>;
  assigned?: string[];
  suspension?: unknown;
  runtime?: unknown;
  idlePolicy?: unknown;
}

function renderMove(target: Workload = workload, setup: Setup = {}) {
  vi.mocked(request).mockImplementation((method: string, path: string) => {
    if (path === "/api/nodes") return Promise.resolve(nodes);
    if (path === "/api/teams/t1/nodes") return Promise.resolve(setup.assigned ?? []);
    if (path.startsWith("/api/operations")) return Promise.resolve(setup.operations ?? []);
    if (path === "/api/environments/e1/suspension" && setup.suspension) return Promise.resolve(setup.suspension);
    if (path === "/api/workloads/w1/runtime" && setup.runtime) return Promise.resolve(setup.runtime);
    if (path === "/api/workloads/w1/idle-policy" && setup.idlePolicy) return Promise.resolve(setup.idlePolicy);
    if (method === "GET" && path === "/api/workloads/w1/node-move") return Promise.resolve({ move: setup.move ?? null });
    if (path === "/api/workloads/w1/node-move/preview") return setup.preview ? setup.preview() : Promise.resolve(preview);
    if (method === "POST" && path === "/api/workloads/w1/node-move") return setup.queue ? setup.queue() : Promise.resolve({ id: "op-move" });
    return Promise.reject(new Error(`Unexpected ${method} ${path}`));
  });
  render(
    <QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}>
      <WorkloadMove workload={target} />
    </QueryClientProvider>,
  );
}

const calls = (method: string, path: string) => vi.mocked(request).mock.calls.filter(([m, p]) => m === method && p === path);

async function chooseDestination(name: string) {
  const user = userEvent.setup();
  await user.click(await screen.findByRole("combobox", { name: "Destination node" }));
  await user.click(screen.getByRole("option", { name }));
  return user;
}

it("previews only on request and shows the plan without validation internals", async () => {
  renderMove();
  const user = await chooseDestination("bravo");
  expect(screen.queryByRole("option", { name: "charlie" })).not.toBeInTheDocument();
  expect(screen.getByText(/Not ready: delta\. Needs a worker upgrade and verify before it can receive moves: charlie\./)).toBeInTheDocument();
  expect(calls("POST", "/api/workloads/w1/node-move/preview")).toHaveLength(0);
  await user.click(screen.getByRole("button", { name: "Preview move" }));
  expect(await screen.findByText("Move preview")).toBeInTheDocument();
  expect(calls("POST", "/api/workloads/w1/node-move/preview")[0]?.[2]).toEqual({ destination_node_id: "n2" });
  expect(screen.getByText("3 containers, 2 running")).toBeInTheDocument();
  expect(screen.getByText("shop_data")).toBeInTheDocument();
  expect(screen.getByText("cove-backups-w1")).toBeInTheDocument();
  expect(screen.getByText("64.0 GiB")).toBeInTheDocument();
  expect(screen.getByText("No existing copy of this resource")).toBeInTheDocument();
  expect(screen.queryByText(/daemon-secret-id|\/var\/lib\/cove|com\.docker/)).not.toBeInTheDocument();
});

it("clears the preview when the destination changes", async () => {
  const third = node("n5", "echo");
  nodes.push(third);
  try {
    renderMove();
    const user = await chooseDestination("bravo");
    await user.click(screen.getByRole("button", { name: "Preview move" }));
    await screen.findByText("Move preview");
    await user.click(screen.getByRole("combobox", { name: "Destination node" }));
    await user.click(screen.getByRole("option", { name: "echo" }));
    expect(screen.queryByText("Move preview")).not.toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Preview move" })).toBeEnabled();
  } finally {
    nodes.pop();
  }
});

it("queues the exact reviewed revision after confirming downtime", async () => {
  renderMove();
  const user = await chooseDestination("bravo");
  await user.click(screen.getByRole("button", { name: "Preview move" }));
  await user.click(await screen.findByRole("button", { name: "Move with downtime" }));
  const dialog = screen.getByRole("dialog", { name: "Move shop to bravo?" });
  expect(within(dialog).getByText(/Running containers on alpha are stopped/)).toBeInTheDocument();
  expect(within(dialog).getByText(/local encrypted backup repository are copied to bravo/)).toBeInTheDocument();
  expect(within(dialog).getByText(/placement switches only after that/)).toBeInTheDocument();
  expect(calls("POST", "/api/workloads/w1/node-move")).toHaveLength(0);
  await user.click(within(dialog).getByRole("button", { name: "Stop and move" }));
  expect(await screen.findByText("Move queued")).toBeInTheDocument();
  expect(calls("POST", "/api/workloads/w1/node-move")[0]?.[2]).toEqual({ destination_node_id: "n2", revision: "rev-abc", confirm: true });
});

it("never repeats an unconfirmed submission and refreshes the move status", async () => {
  renderMove(workload, { queue: () => Promise.reject(new ApiError(0, "Cannot reach the control plane.")) });
  const user = await chooseDestination("bravo");
  await user.click(screen.getByRole("button", { name: "Preview move" }));
  await user.click(await screen.findByRole("button", { name: "Move with downtime" }));
  const reads = calls("GET", "/api/workloads/w1/node-move").length;
  await user.click(screen.getByRole("button", { name: "Stop and move" }));
  expect(await screen.findByText("The move may have been queued")).toBeInTheDocument();
  await waitFor(() => expect(calls("GET", "/api/workloads/w1/node-move").length).toBeGreaterThan(reads));
  expect(calls("POST", "/api/workloads/w1/node-move")).toHaveLength(1);
  expect(screen.queryByRole("button", { name: "Move with downtime" })).not.toBeInTheDocument();
});

it("reports a refused submission and requires a fresh preview", async () => {
  renderMove(workload, { queue: () => Promise.reject(new ApiError(409, "Resource changed; review a new preview")) });
  const user = await chooseDestination("bravo");
  await user.click(screen.getByRole("button", { name: "Preview move" }));
  await user.click(await screen.findByRole("button", { name: "Move with downtime" }));
  await user.click(screen.getByRole("button", { name: "Stop and move" }));
  expect(await screen.findByText("The move was not queued")).toBeInTheDocument();
  expect(screen.getByText("Resource changed; review a new preview")).toBeInTheDocument();
  expect(screen.getByRole("button", { name: "Preview move" })).toBeInTheDocument();
});

it("shows preview validation errors with a manual retry", async () => {
  let attempts = 0;
  renderMove(workload, { preview: () => (++attempts === 1 ? Promise.reject(new ApiError(409, "Volume shop_cache is external")) : Promise.resolve(preview)) });
  const user = await chooseDestination("bravo");
  await user.click(screen.getByRole("button", { name: "Preview move" }));
  expect(await screen.findByText("Volume shop_cache is external")).toBeInTheDocument();
  await user.click(screen.getByRole("button", { name: "Inspect again" }));
  expect(await screen.findByText("Move preview")).toBeInTheDocument();
  expect(attempts).toBe(2);
});

it("limits administrators to the team's assigned nodes", async () => {
  session.current = { is_admin: true, team_id: "t1" };
  renderMove(workload, { assigned: ["n1", "n3"] });
  expect(await screen.findByText("No destination available")).toBeInTheDocument();
  expect(screen.getByText(/Needs a worker upgrade and verify before it can receive moves: charlie\./)).toBeInTheDocument();
  expect(screen.queryByText(/bravo|delta/)).not.toBeInTheDocument();
});

it("asks for an upgraded source worker", async () => {
  renderMove({ ...workload, node_id: "n3" });
  expect(await screen.findByText("The current node needs a worker upgrade")).toBeInTheDocument();
  expect(screen.queryByRole("combobox", { name: "Destination node" })).not.toBeInTheDocument();
});

it("blocks previews while offline", async () => {
  vi.spyOn(navigator, "onLine", "get").mockReturnValue(false);
  renderMove();
  await chooseDestination("bravo");
  expect(screen.getByText("Reconnect to preview or start a move.")).toBeInTheDocument();
  expect(screen.getByRole("button", { name: "Preview move" })).toBeDisabled();
});

it("shows an unresolved move with honest cancellation guidance and no planner", async () => {
  renderMove(workload, { move: { id: "op-move", source_node_id: "n1", destination_node_id: "n2", phase: "committing", last_error: "", updated_at: new Date().toISOString() } });
  expect(await screen.findByText("Committing placement")).toBeInTheDocument();
  expect(screen.getByText(/Moving from/)).toHaveTextContent("Moving from alpha to bravo.");
  expect(screen.getByText(/does not undo a committed placement/)).toBeInTheDocument();
  expect(screen.getByText(/cancellation waits until the move reaches a safe point/)).toBeInTheDocument();
  expect(screen.getByRole("link", { name: "View the move operation" })).toBeInTheDocument();
  expect(screen.queryByRole("combobox", { name: "Destination node" })).not.toBeInTheDocument();
});

it("lets viewers read the last move without move controls", async () => {
  renderMove({ ...workload, permissions: ["read"] }, { move: { id: "op-move", source_node_id: "n1", destination_node_id: "n2", phase: "rolled_back", last_error: "destination health check failed", updated_at: new Date().toISOString() } });
  expect(await screen.findByText("Rolled back")).toBeInTheDocument();
  expect(screen.getByText("destination health check failed")).toBeInTheDocument();
  expect(screen.getByText("Moving this resource needs Edit configuration and Read secrets.")).toBeInTheDocument();
  expect(screen.queryByRole("combobox")).not.toBeInTheDocument();
  expect(calls("POST", "/api/workloads/w1/node-move/preview")).toHaveLength(0);
});

it.each([
  ["the legacy Edit level", ["read", "operate", "edit"]],
  ["Edit configuration without Read secrets", ["read", "operate", "edit", "edit_configuration"]],
  ["Read secrets without Edit configuration", ["read", "operate", "read_secrets"]],
] as const)("does not offer a move with %s", async (_, permissions) => {
  renderMove({ ...workload, permissions: [...permissions] });
  expect(await screen.findByText("Moving this resource needs Edit configuration and Read secrets.")).toBeInTheDocument();
  expect(screen.queryByRole("combobox", { name: "Destination node" })).not.toBeInTheDocument();
  expect(calls("POST", "/api/workloads/w1/node-move/preview")).toHaveLength(0);
});

it("asks for idle sleep to be turned off before moving", async () => {
  renderMove(workload, { idlePolicy: { enabled: true, after_seconds: 300, request_driven: true }, runtime: { idle: { phase: "awake" } } });
  expect(await screen.findByText("Idle sleep is on")).toBeInTheDocument();
  expect(screen.getByText(/Turn off idle sleep on the Deployment safety page first/)).toBeInTheDocument();
  expect(screen.queryByRole("combobox", { name: "Destination node" })).not.toBeInTheDocument();
  expect(calls("POST", "/api/workloads/w1/node-move/preview")).toHaveLength(0);
});

it("shows the worker's reason when the preview refuses a resource it cannot move", async () => {
  renderMove(workload, { preview: () => Promise.reject(new ApiError(409, "resources in adopted projects cannot be moved")) });
  const user = await chooseDestination("bravo");
  await user.click(screen.getByRole("button", { name: "Preview move" }));
  expect(await screen.findByText("resources in adopted projects cannot be moved")).toBeInTheDocument();
  expect(screen.queryByRole("button", { name: "Move with downtime" })).not.toBeInTheDocument();
});

it("waits for other operations before previewing", async () => {
  renderMove(workload, { operations: [{ id: "op-1", workload_id: "w1", action: "deploy", state: "running", result: null, created_at: new Date().toISOString() }] });
  expect(await screen.findByText(/once the active deploy finishes/)).toBeInTheDocument();
  expect(screen.queryByRole("button", { name: "Preview move" })).not.toBeInTheDocument();
});

it("holds the move while the environment is paused and calls no move API", async () => {
  renderMove({ ...workload, environment_id: "e1" }, { suspension: { paused: true, phase: "paused", resources: [] } });
  expect(await screen.findByText(/The environment is paused\. Moving to another node is available after it resumes\./)).toBeInTheDocument();
  expect(screen.getByText("Moving is on hold")).toBeInTheDocument();
  expect(screen.queryByRole("combobox", { name: "Destination node" })).not.toBeInTheDocument();
  expect(calls("POST", "/api/workloads/w1/node-move/preview")).toHaveLength(0);
});

it("waits for a staged rollout to be promoted or aborted before moving", async () => {
  renderMove(workload, { runtime: { rollout: { phase: "waiting", strategy: "canary", operation: "op-roll" } } });
  expect(await screen.findByText(/A staged rollout is waiting/)).toBeInTheDocument();
  expect(screen.queryByRole("combobox", { name: "Destination node" })).not.toBeInTheDocument();
});

it("never calls the reviewed project and team move endpoints", async () => {
  renderMove();
  await chooseDestination("bravo");
  expect(vi.mocked(request).mock.calls.some(([, path]) => path === "/api/workloads/w1/move" || path === "/api/workloads/w1/move/preview")).toBe(false);
});

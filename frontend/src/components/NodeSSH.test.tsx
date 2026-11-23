import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import type { Bastion, NodeIdentity } from "../api/bastions";
import { ApiError, request } from "../api/client";
import { useVerifyNode } from "../api/mutations";
import type { Node } from "../api/types";
import { NodeSSH } from "./NodeSSH";

vi.mock("../api/client", async (original) => ({ ...(await original<typeof import("../api/client")>()), request: vi.fn() }));

beforeEach(() => {
  vi.stubGlobal("ResizeObserver", class { observe = vi.fn(); disconnect = vi.fn(); });
  HTMLDialogElement.prototype.showModal = function (this: HTMLDialogElement) { this.setAttribute("open", ""); };
  HTMLDialogElement.prototype.close = function (this: HTMLDialogElement) { this.removeAttribute("open"); };
});
afterEach(() => { vi.clearAllMocks(); vi.unstubAllGlobals(); });

const capabilities = { protocol: 1, os: "linux", arch: "amd64", docker: "27", compose: "2", ready: true, ssh_identity: true };
const sshNode: Node = { id: "n1", name: "worker", state: "ready", mode: "ssh", address: "10.0.0.12", port: 22, last_checked_at: null, capabilities };
const jump: Bastion = { id: "b1", name: "Jump", address: "jump.example", port: 22, user: "cove-jump", host_key: "ssh-ed25519 AAAA", public_key: "ssh-ed25519 BBBB", fingerprint: "SHA256:abc", targets: [], limited: false };

function mockApi({ bastionId = "", hostError = "", identity = { id: "", state: "idle", error: "", updated_at: null }, action }: { bastionId?: string; hostError?: string; identity?: NodeIdentity; action?: () => Promise<unknown> } = {}) {
  let route = bastionId;
  vi.mocked(request).mockImplementation((method, path, body) => {
    if (path === "/api/bastions") return Promise.resolve([jump]);
    if (path === "/api/nodes/n1/bastion") {
      if (method === "PUT") route = (body as { bastion_id: string }).bastion_id;
      return Promise.resolve({ bastion_id: route, host_key_error: hostError });
    }
    if (path === "/api/nodes/n1/verify") return Promise.resolve({ state: "ready" });
    if (path === "/api/nodes/n1/identity") return method === "POST" && action ? action() : Promise.resolve(identity);
    return Promise.reject(new Error(`unexpected ${method} ${path}`));
  });
}

function Harness({ node }: { node: Node }) {
  return <NodeSSH node={node} verify={useVerifyNode()} />;
}

function renderSSH(node: Node = sshNode) {
  render(<QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}><Harness node={node} /></QueryClientProvider>);
}

it("shows a direct notice for local nodes without route or rotation controls", () => {
  renderSSH({ ...sshNode, mode: "local" });
  expect(screen.getByText(/local socket on the main node/)).toBeInTheDocument();
  expect(screen.queryByRole("combobox")).not.toBeInTheDocument();
  expect(screen.queryByRole("button", { name: "Rotate key" })).not.toBeInTheDocument();
  expect(request).not.toHaveBeenCalled();
});

it("saves a bastion route and asks for verification", async () => {
  mockApi();
  renderSSH();
  const route = await screen.findByRole("combobox", { name: "Connect through" });
  expect(screen.getByRole("button", { name: "Save route" })).toBeDisabled();
  fireEvent.click(route);
  fireEvent.click(screen.getByRole("option", { name: "Jump (cove-jump@jump.example:22)" }));
  expect(screen.getByText(/must permit forwarding to 10.0.0.12:22/)).toBeInTheDocument();
  fireEvent.click(screen.getByRole("button", { name: "Save route" }));
  await waitFor(() => expect(request).toHaveBeenCalledWith("PUT", "/api/nodes/n1/bastion", { bastion_id: "b1" }));
  expect(await screen.findByText("Route saved")).toBeInTheDocument();
});

it("warns about a host key mismatch without offering to accept the observed key", async () => {
  mockApi({ hostError: "bastion host key mismatch: got SHA256:new, pinned SHA256:abc" });
  renderSSH();
  const alert = await screen.findByRole("alert");
  expect(within(alert).getByText(/got SHA256:new/)).toBeInTheDocument();
  expect(within(alert).getByText(/never accepts the key it observed/)).toBeInTheDocument();
  expect(within(alert).queryByRole("button", { name: /accept|trust|replace/i })).not.toBeInTheDocument();
  fireEvent.click(within(alert).getByRole("button", { name: "Verify" }));
  await waitFor(() => expect(request).toHaveBeenCalledWith("POST", "/api/nodes/n1/verify"));
});

it("confirms a rotation and sends an explicit action", async () => {
  mockApi({ action: () => Promise.resolve({ id: "r1", state: "queued" }) });
  renderSSH();
  expect(await screen.findByText("Not rotated")).toBeInTheDocument();
  fireEvent.click(screen.getByRole("button", { name: "Rotate key" }));
  const dialog = screen.getByRole("dialog", { name: "Rotate this node's SSH key?" });
  expect(within(dialog).getByText(/never removes the old key before the new one is saved/)).toBeInTheDocument();
  expect(request).not.toHaveBeenCalledWith("POST", "/api/nodes/n1/identity", expect.anything());
  fireEvent.click(within(dialog).getByRole("button", { name: "Rotate key" }));
  await waitFor(() => expect(request).toHaveBeenCalledWith("POST", "/api/nodes/n1/identity", { action: "rotate", confirm: true }));
});

it("offers retry and cancel while staged, and only retry once switched", async () => {
  mockApi({ identity: { id: "r1", state: "staged", error: "worker unreachable", updated_at: "2026-11-22T10:00:00Z" } });
  const { unmount } = render(<QueryClientProvider client={new QueryClient()}><Harness node={sshNode} /></QueryClientProvider>);
  expect(await screen.findByText("Proving the new key")).toBeInTheDocument();
  expect(screen.getByText("worker unreachable")).toBeInTheDocument();
  expect(screen.getByRole("button", { name: "Retry now" })).toBeEnabled();
  fireEvent.click(screen.getByRole("button", { name: "Cancel rotation" }));
  expect(screen.getByRole("dialog", { name: "Cancel the key rotation?" })).toHaveTextContent(/removes only the new staged key/);
  unmount();

  mockApi({ identity: { id: "r1", state: "switched", error: "", updated_at: null } });
  renderSSH();
  expect(await screen.findByText("Removing the old key")).toBeInTheDocument();
  expect(screen.getByText("Cleanup cannot be cancelled")).toBeInTheDocument();
  expect(screen.getByRole("button", { name: "Retry now" })).toBeInTheDocument();
  expect(screen.queryByRole("button", { name: "Cancel rotation" })).not.toBeInTheDocument();
});

it("refreshes status after an ambiguous failure instead of declaring a result", async () => {
  mockApi({ identity: { id: "r1", state: "queued", error: "", updated_at: null }, action: () => Promise.reject(new TypeError("Failed to fetch")) });
  renderSSH();
  fireEvent.click(await screen.findByRole("button", { name: "Retry now" }));
  fireEvent.click(within(screen.getByRole("dialog", { name: "Retry the pending step?" })).getByRole("button", { name: "Retry now" }));
  expect(await screen.findByText(/may or may not have been saved/)).toBeInTheDocument();
  await waitFor(() => expect(vi.mocked(request).mock.calls.filter(([method, path]) => method === "GET" && path === "/api/nodes/n1/identity").length).toBeGreaterThan(1));
});

it("explains old-worker and refusal states", async () => {
  mockApi({ action: () => Promise.reject(new ApiError(409, "verify an upgraded SSH worker and wait for active operations")) });
  const { unmount } = render(<QueryClientProvider client={new QueryClient()}><Harness node={{ ...sshNode, capabilities: { ...capabilities, ssh_identity: false } }} /></QueryClientProvider>);
  expect(await screen.findByText(/Upgrade the worker and verify the node/)).toBeInTheDocument();
  expect(screen.getByRole("button", { name: "Rotate key" })).toBeDisabled();
  unmount();

  const unreachable = render(<QueryClientProvider client={new QueryClient()}><Harness node={{ ...sshNode, state: "unreachable", capabilities: null }} /></QueryClientProvider>);
  expect(await screen.findByText(/no current probe result/)).toBeInTheDocument();
  expect(screen.queryByText(/Upgrade the worker/)).not.toBeInTheDocument();
  unreachable.unmount();

  renderSSH();
  fireEvent.click(await screen.findByRole("button", { name: "Rotate key" }));
  fireEvent.click(within(screen.getByRole("dialog", { name: "Rotate this node's SSH key?" })).getByRole("button", { name: "Rotate key" }));
  expect(await screen.findByText("verify an upgraded SSH worker and wait for active operations")).toBeInTheDocument();
  expect(screen.queryByText(/may or may not have been saved/)).not.toBeInTheDocument();
});

it("keeps polling a pending rotation until it finishes and refreshes the node list once", async () => {
  vi.useFakeTimers({ shouldAdvanceTime: true });
  try {
    const states: NodeIdentity["state"][] = ["staged", "switched", "succeeded"];
    let polls = 0;
    mockApi();
    const base = vi.mocked(request).getMockImplementation();
    vi.mocked(request).mockImplementation((method, path, body) => {
      if (path === "/api/nodes/n1/identity") return Promise.resolve({ id: "r1", state: states[Math.min(polls++, states.length - 1)], error: "", updated_at: null });
      if (path === "/api/nodes") return Promise.resolve([sshNode]);
      return base ? base(method, path, body) : Promise.reject(new Error("no mock"));
    });
    const client = new QueryClient();
    const nodes = vi.spyOn(client, "invalidateQueries");
    render(<QueryClientProvider client={client}><Harness node={sshNode} /></QueryClientProvider>);
    expect(await screen.findByText("Proving the new key")).toBeInTheDocument();
    await vi.advanceTimersByTimeAsync(5_100);
    expect(await screen.findByText("Removing the old key")).toBeInTheDocument();
    await vi.advanceTimersByTimeAsync(5_100);
    expect(await screen.findByText("Rotated")).toBeInTheDocument();
    await vi.advanceTimersByTimeAsync(15_000);
    expect(polls).toBe(3);
    expect(nodes.mock.calls.filter(([filters]) => JSON.stringify(filters?.queryKey) === '["nodes"]')).toEqual([[{ queryKey: ["nodes"], exact: true }]]);
  } finally {
    vi.useRealTimers();
  }
});

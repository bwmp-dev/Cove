import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { ApiError, request } from "../api/client";
import type { Node } from "../api/types";
import { ProxyTraffic } from "./ProxyTraffic";

vi.mock("../api/client", async (importActual) => ({ ...(await importActual<typeof import("../api/client")>()), request: vi.fn() }));

beforeEach(() => {
  vi.stubGlobal("ResizeObserver", class { observe = vi.fn(); disconnect = vi.fn(); });
  HTMLDialogElement.prototype.showModal = vi.fn(function (this: HTMLDialogElement) { this.open = true; });
  HTMLDialogElement.prototype.close = vi.fn(function (this: HTMLDialogElement) { this.open = false; });
});
afterEach(() => { vi.resetAllMocks(); vi.unstubAllGlobals(); });

const node = { id: "n1", name: "edge-1", capabilities: { traffic_logs: true } } as unknown as Node;
const off = { settings: { enabled: false, trusted_proxies: [] }, configured: false, pending: false, notice: "" };

function renderTraffic(target: Node = node) {
  render(<QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}><ProxyTraffic node={target} /></QueryClientProvider>);
}

it("previews, then restarts only after the node name is typed, sending the previewed revision", async () => {
  vi.mocked(request).mockImplementation((method, _path, body) => {
    if (method === "GET") return Promise.resolve({ state: off, plan: null });
    const action = (body as { action: string }).action;
    if (action === "preview") return Promise.resolve({ state: off, plan: { settings: { enabled: true, trusted_proxies: ["203.0.113.0/24"] }, revision: "rev-1" } });
    return Promise.resolve({ state: { ...off, settings: { enabled: true, trusted_proxies: ["203.0.113.0/24"] }, configured: true }, plan: null });
  });
  renderTraffic();
  fireEvent.click(await screen.findByRole("switch", { name: "Record access logs" }));
  fireEvent.change(screen.getByLabelText(/Trusted proxies/), { target: { value: "0.0.0.0/0" } });
  fireEvent.click(screen.getByRole("button", { name: "Review change" }));
  expect(await screen.findByText(/trusts every address/)).toBeInTheDocument();
  expect(request).not.toHaveBeenCalledWith("POST", expect.anything(), expect.anything());

  fireEvent.change(screen.getByLabelText(/Trusted proxies/), { target: { value: "203.0.113.0/24" } });
  fireEvent.click(screen.getByRole("button", { name: "Review change" }));
  const dialog = await screen.findByRole("dialog", { name: "Restart the proxy on edge-1?" });
  expect(request).toHaveBeenCalledWith("POST", "/api/nodes/n1/traffic", { action: "preview", settings: { enabled: true, trusted_proxies: ["203.0.113.0/24"] } });
  const restart = within(dialog).getByRole("button", { name: "Restart proxy" });
  expect(restart).toBeDisabled();
  fireEvent.change(within(dialog).getByLabelText("Type edge-1 to confirm"), { target: { value: "edge-1" } });
  fireEvent.click(restart);
  await waitFor(() => expect(request).toHaveBeenCalledWith("POST", "/api/nodes/n1/traffic", { action: "apply", settings: { enabled: true, trusted_proxies: ["203.0.113.0/24"] }, revision: "rev-1" }));
  expect(await screen.findByText("Proxy restarted")).toBeInTheDocument();
  expect(screen.getByText("Access logging on")).toBeInTheDocument();
});

it("keeps the dialog open with guidance when the restart is refused", async () => {
  vi.mocked(request).mockImplementation((method, _path, body) => {
    if (method === "GET") return Promise.resolve({ state: off, plan: null });
    if ((body as { action: string }).action === "preview") return Promise.resolve({ state: off, plan: { settings: { enabled: true, trusted_proxies: [] }, revision: "rev-1" } });
    return Promise.reject(new ApiError(409, "Proxy change unavailable; inspect readiness, active operations and recovery state"));
  });
  renderTraffic();
  fireEvent.click(await screen.findByRole("switch", { name: "Record access logs" }));
  fireEvent.click(screen.getByRole("button", { name: "Review change" }));
  const dialog = await screen.findByRole("dialog");
  fireEvent.change(within(dialog).getByLabelText("Type edge-1 to confirm"), { target: { value: "edge-1" } });
  fireEvent.click(within(dialog).getByRole("button", { name: "Restart proxy" }));
  expect(await within(dialog).findByText(/inspect readiness/)).toBeInTheDocument();
  expect(vi.mocked(request).mock.calls.filter(([, , body]) => (body as { action?: string } | undefined)?.action === "apply")).toHaveLength(1);
});

it("blocks changes while an interrupted change needs explicit recovery", async () => {
  vi.mocked(request).mockImplementation((method) => (method === "GET" ? Promise.resolve({ state: { ...off, pending: true, notice: "A previous proxy change needs recovery before another change." }, plan: null }) : Promise.resolve({ state: off, plan: null })));
  renderTraffic();
  expect(await screen.findByText("A proxy change was interrupted")).toBeInTheDocument();
  expect(screen.getByRole("switch", { name: "Record access logs" })).toBeDisabled();
  fireEvent.click(screen.getByRole("button", { name: "Recover" }));
  fireEvent.click(within(await screen.findByRole("dialog", { name: "Recover the proxy on edge-1?" })).getByRole("button", { name: "Recover proxy" }));
  await waitFor(() => expect(request).toHaveBeenCalledWith("POST", "/api/nodes/n1/traffic", { action: "recover" }));
  expect(await screen.findByText("Recovery finished")).toBeInTheDocument();
});

it("explains when the worker cannot collect traffic", () => {
  renderTraffic({ ...node, capabilities: { traffic_logs: false } } as unknown as Node);
  expect(screen.getByText("Not supported by this worker")).toBeInTheDocument();
  expect(request).not.toHaveBeenCalled();
});

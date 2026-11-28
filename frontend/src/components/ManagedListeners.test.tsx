import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { ApiError, request } from "../api/client";
import type { ListenerPlan, ListenerStatus } from "../api/ingress-listeners";
import type { Node } from "../api/types";
import { ManagedListeners } from "./ManagedListeners";

vi.mock("../api/client", async (original) => ({ ...(await original<typeof import("../api/client")>()), request: vi.fn() }));
// The guard needs a router; leaving the page is covered by the shared component's own use.
vi.mock("./SaveBar", () => ({ UnsavedChangesGuard: () => null }));
beforeEach(() => vi.stubGlobal("ResizeObserver", class { observe = vi.fn(); disconnect = vi.fn(); }));
afterEach(() => {
  vi.clearAllMocks();
  vi.unstubAllGlobals();
});
// jsdom does not implement modal dialogs.
HTMLDialogElement.prototype.showModal = function (this: HTMLDialogElement) {
  this.open = true;
};
HTMLDialogElement.prototype.close = function (this: HTMLDialogElement) {
  this.open = false;
};

const revision = "a".repeat(64);
const digest = "b".repeat(64);
const publicBindings = { http: [{ address: "0.0.0.0", port: 80 }], https: [{ address: "0.0.0.0", port: 443 }] };

function node(supported: boolean | null = true): Node {
  return { id: "n1", name: "worker-1", address: "", port: 22, state: "ready", mode: "ssh", last_checked_at: null, capabilities: supported === null ? null : { protocol: 1, os: "linux", arch: "amd64", docker: "27", compose: "2", ready: true, ingress_exposures: supported } };
}

function status(extra: Partial<ListenerStatus> = {}): ListenerStatus {
  return { settings: { http3: false, listeners: [] }, ports: null, proxy_present: true, running: true, public_bindings: publicBindings, ...extra };
}

const extra = { id: "local-db", protocol: "tcp" as const, visibility: "public" as const, bindings: [{ address: "127.0.0.1", port: 18080 }] };

function plan(extraFields: Partial<ListenerPlan> = {}): ListenerPlan {
  return { affected_workloads: ["w1", "f".repeat(32)], change_id: "c1", revision, digest, settings: { http3: false, listeners: [extra] }, public_bindings: publicBindings, ports: { "local-db": 10001 }, requires_restart: true, warnings: ["Docker publishes IPv6 only when the daemon supports it."], ...extraFields };
}

type Handler = (method: string, path: string, body?: unknown) => unknown;

function serve(handler: Handler) {
  vi.mocked(request).mockImplementation((method: string, path: string, body?: unknown) => {
    try {
      return Promise.resolve(handler(method, path, body));
    } catch (error) {
      return Promise.reject(error instanceof Error ? error : new Error(String(error)));
    }
  });
}

function renderListeners(target = node()) {
  render(
    <QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}>
      <ManagedListeners node={target} />
    </QueryClientProvider>,
  );
}

function calls(method: string) {
  return vi.mocked(request).mock.calls.filter(([callMethod]) => callMethod === method);
}

async function addLocalListener() {
  fireEvent.click(await screen.findByRole("button", { name: "Add listener" }));
  fireEvent.change(screen.getByLabelText("Name"), { target: { value: "local-db" } });
  fireEvent.click(screen.getByRole("combobox", { name: "Visibility" }));
  fireEvent.click(screen.getByRole("option", { name: "Public" }));
  fireEvent.change(screen.getByLabelText("Bindings for local-db: address 1"), { target: { value: "127.0.0.1" } });
  fireEvent.change(screen.getByLabelText("Bindings for local-db: port 1"), { target: { value: "18080" } });
}

it("plans the exact settings, reviews the interruption and applies only after confirmation", async () => {
  let saved = status();
  serve((method, path) => {
    if (path === "/api/workloads") return [{ id: "w1", name: "shop-web" }];
    if (method === "GET") return saved;
    if (path.endsWith("/plan")) return plan();
    saved = status({ settings: { http3: false, listeners: [extra] }, ports: { "local-db": 10001 }, change: { id: "c1", phase: "completed", committed: true, started_at: "2026-11-23T10:00:00Z", finished_at: "2026-11-23T10:00:20Z" } });
    return { ...saved, proxy_present: false, running: false };
  });
  renderListeners();
  expect(await screen.findByText("Applying listener changes restarts the shared proxy")).toBeInTheDocument();
  await addLocalListener();
  expect(screen.getByText("cove-listener-local-db")).toBeInTheDocument();
  fireEvent.click(screen.getByRole("button", { name: "Review changes" }));

  const heading = await screen.findByRole("heading", { name: "Review listener change" });
  await waitFor(() => expect(heading).toHaveFocus());
  expect(request).toHaveBeenCalledWith("POST", "/api/nodes/n1/ingress-listeners/plan", { http3: false, listeners: [extra] });
  expect(screen.getByText("Applying interrupts every connection through the proxy on worker-1")).toBeInTheDocument();
  expect(await screen.findByText("shop-web")).toBeInTheDocument();
  // Resources outside the current team stay listed by ID.
  expect(screen.getByText("f".repeat(32))).toBeInTheDocument();
  expect(screen.getByText("10001")).toBeInTheDocument();
  expect(screen.getByText("Docker publishes IPv6 only when the daemon supports it.")).toBeInTheDocument();
  expect(calls("PUT")).toHaveLength(0);

  fireEvent.click(screen.getByRole("button", { name: "Apply and restart proxy" }));
  const dialog = screen.getByRole("dialog", { name: "Restart the proxy on worker-1?" });
  fireEvent.click(within(dialog).getByRole("button", { name: "Interrupt connections and apply" }));
  await waitFor(() => expect(request).toHaveBeenCalledWith("PUT", "/api/nodes/n1/ingress-listeners", { settings: { http3: false, listeners: [extra] }, revision, digest, accept_interruption: true }));
  // The reply's runtime fields are not trusted; status is read again.
  await waitFor(() => expect(calls("GET").length).toBeGreaterThanOrEqual(2));
  expect(await screen.findByText("Listener change: Applied")).toBeInTheDocument();
  expect(screen.queryByText("The proxy is stopped")).not.toBeInTheDocument();
  expect(screen.getByText("Internal port 10001, assigned by Cove.")).toBeInTheDocument();
});

it("expires the reviewed plan on any edit", async () => {
  serve((method, path) => (method === "GET" ? status() : path.endsWith("/plan") ? plan() : undefined));
  renderListeners();
  await addLocalListener();
  fireEvent.click(screen.getByRole("button", { name: "Review changes" }));
  expect(await screen.findByRole("heading", { name: "Review listener change" })).toBeInTheDocument();
  fireEvent.change(screen.getByLabelText("Bindings for local-db: port 1"), { target: { value: "18081" } });
  expect(screen.queryByRole("heading", { name: "Review listener change" })).not.toBeInTheDocument();
  expect(screen.getByRole("button", { name: "Review changes" })).toBeEnabled();
});

it("keeps edits after a refused apply and never resubmits it", async () => {
  serve((method, path) => {
    if (method === "GET") return status();
    if (path.endsWith("/plan")) return plan();
    throw new ApiError(409, "the listener plan changed; review a fresh plan before applying");
  });
  renderListeners();
  await addLocalListener();
  fireEvent.click(screen.getByRole("button", { name: "Review changes" }));
  fireEvent.click(await screen.findByRole("button", { name: "Apply and restart proxy" }));
  fireEvent.click(within(screen.getByRole("dialog")).getByRole("button", { name: "Interrupt connections and apply" }));
  expect(await screen.findByText("the listener plan changed; review a fresh plan before applying")).toBeInTheDocument();
  expect(screen.getByText("The listener change was not applied")).toBeInTheDocument();
  expect(screen.queryByRole("heading", { name: "Review listener change" })).not.toBeInTheDocument();
  expect(screen.getByLabelText("Name")).toHaveValue("local-db");
  await waitFor(() => expect(calls("GET").length).toBeGreaterThanOrEqual(2));
  expect(calls("PUT")).toHaveLength(1);
});

it("shows a lost reply as unknown rather than failed", async () => {
  serve((method, path) => {
    if (method === "GET") return status();
    if (path.endsWith("/plan")) return plan();
    throw new ApiError(502, "worker unavailable; verify the node before changing listeners");
  });
  renderListeners();
  await addLocalListener();
  fireEvent.click(screen.getByRole("button", { name: "Review changes" }));
  fireEvent.click(await screen.findByRole("button", { name: "Apply and restart proxy" }));
  fireEvent.click(within(screen.getByRole("dialog")).getByRole("button", { name: "Interrupt connections and apply" }));
  expect(await screen.findByText("No reply from the worker")).toBeInTheDocument();
});

it("validates before planning and focuses the first invalid field", async () => {
  serve(() => status());
  renderListeners();
  fireEvent.click(await screen.findByRole("button", { name: "Add listener" }));
  fireEvent.change(screen.getByLabelText("Name"), { target: { value: "Bad Name" } });
  fireEvent.change(screen.getByLabelText("Bindings for Bad Name: address 1"), { target: { value: "0.0.0.0" } });
  fireEvent.click(screen.getByRole("button", { name: "Review changes" }));
  expect(await screen.findByText(/Use up to 32 lowercase letters/)).toBeInTheDocument();
  expect(screen.getByText(/Use an address this node owns/)).toBeInTheDocument();
  await waitFor(() => expect(screen.getByLabelText("Name")).toHaveFocus());
  expect(calls("POST")).toHaveLength(0);
});

it("warns before removing every public binding", async () => {
  serve(() => status());
  renderListeners();
  fireEvent.click(await screen.findByRole("radio", { name: /Set explicitly/ }));
  expect(screen.getByLabelText("HTTP bindings: address 1")).toHaveValue("0.0.0.0");
  fireEvent.click(screen.getByRole("button", { name: "Remove http bindings 1" }));
  fireEvent.click(screen.getByRole("button", { name: "Remove https bindings 1" }));
  expect(screen.getByText("This removes every public binding")).toBeInTheDocument();
});

it("does not offer to apply a plan that needs no restart", async () => {
  serve((method, path) => (method === "GET" ? status() : path.endsWith("/plan") ? plan({ requires_restart: false, affected_workloads: [] }) : undefined));
  renderListeners();
  fireEvent.click(await screen.findByRole("switch", { name: "HTTP/3" }));
  fireEvent.click(screen.getByRole("button", { name: "Review changes" }));
  expect(await screen.findByText("No change to apply")).toBeInTheDocument();
  expect(screen.queryByRole("button", { name: "Apply and restart proxy" })).not.toBeInTheDocument();
});

it("creates a missing proxy only after confirmation and reloads status", async () => {
  let present = false;
  serve((method) => {
    if (method === "POST") present = true;
    return status({ proxy_present: present, running: false, public_bindings: present ? { http: [], https: [] } : undefined });
  });
  renderListeners();
  fireEvent.click(await screen.findByRole("button", { name: "Create proxy" }));
  expect(calls("POST")).toHaveLength(0);
  fireEvent.click(within(screen.getByRole("dialog")).getByRole("button", { name: "Create stopped proxy" }));
  await waitFor(() => expect(request).toHaveBeenCalledWith("POST", "/api/nodes/n1/ingress-listeners/initialize"));
  expect(await screen.findByText("The proxy is stopped")).toBeInTheDocument();
  expect(screen.getByRole("button", { name: "Add listener" })).toBeInTheDocument();
});

it("shows saved settings read-only on a worker without support", async () => {
  serve(() => status({ settings: { http3: false, listeners: [extra] }, ports: { "local-db": 10001 } }));
  renderListeners(node(false));
  expect(screen.getByText("This worker cannot manage listeners")).toBeInTheDocument();
  expect(await screen.findByLabelText("Name")).toHaveValue("local-db");
  expect(screen.getByLabelText("Name")).toBeDisabled();
  expect(screen.getByRole("button", { name: "Review changes" })).toBeDisabled();
});

it("pauses editing while a change is settling", async () => {
  serve(() => status({ change: { id: "c2", phase: "restoring", committed: false, started_at: "2026-11-23T10:00:00Z" } }));
  renderListeners();
  expect(await screen.findByText("Listener change: Restoring the previous proxy")).toBeInTheDocument();
  expect(screen.getByRole("button", { name: "Add listener" })).toBeDisabled();
});

it("offers a retry when status cannot be read", async () => {
  serve(() => {
    throw new ApiError(409, "this worker does not support managed listeners; upgrade and verify it first");
  });
  renderListeners();
  expect(await screen.findByText("Could not load the listener settings")).toBeInTheDocument();
  expect(screen.getByText("this worker does not support managed listeners; upgrade and verify it first")).toBeInTheDocument();
  expect(screen.getByRole("button", { name: "Retry" })).toBeInTheDocument();
});

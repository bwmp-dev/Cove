import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { ApiError, request } from "../api/client";
import type { TunnelStatus, TunnelWorkerStatus } from "../api/cloudflare-tunnel";
import type { Node } from "../api/types";
import { CloudflareTunnel } from "./CloudflareTunnel";

vi.mock("../api/client", async (original) => ({ ...(await original<typeof import("../api/client")>()), request: vi.fn() }));
vi.mock("./SaveBar", () => ({ UnsavedChangesGuard: () => null }));
vi.mock("@tanstack/react-router", () => ({ Link: ({ children }: { children: React.ReactNode }) => <span>{children}</span> }));
beforeEach(() => vi.stubGlobal("ResizeObserver", class { observe = vi.fn(); disconnect = vi.fn(); }));
afterEach(() => {
  vi.clearAllMocks();
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
});
HTMLDialogElement.prototype.showModal = function (this: HTMLDialogElement) {
  this.open = true;
};
HTMLDialogElement.prototype.close = function (this: HTMLDialogElement) {
  this.open = false;
};

// Fixtures only: no real control plane, worker or Cloudflare account is involved.
const pin = "docker.io/cloudflare/cloudflared:2026.11.1@sha256:e613aadbc2d9511785d2e67a86b0c8ac18dd63a3f9af0d06faf04cbfdcb35f2d";
const override = `cloudflare/cloudflared:2026.10.0@sha256:${"c".repeat(64)}`;

function node(supported: boolean | null = true): Node {
  return { id: "n1", name: "worker-1", address: "", port: 22, state: "ready", mode: "ssh", last_checked_at: null, capabilities: supported === null ? null : { protocol: 1, os: "linux", arch: "amd64", docker: "27", compose: "2", ready: true, cloudflare_publishing: supported } };
}

function status(extra: Partial<TunnelStatus> = {}): TunnelStatus {
  return {
    configuration: null,
    phase: "disabled",
    cleanup_requested: false,
    last_error: "",
    next_check_at: null,
    worker: null,
    worker_error: null,
    zones: [
      { id: "z1", domain: "example.com", connection_kind: "token" },
      { id: "z2", domain: "example.org", connection_kind: "oauth" },
    ],
    available_hostnames: ["shop.example.com", "app.example.com", "blog.example.org"],
    dns: [],
    default_image: pin,
    ...extra,
  };
}

const worker: TunnelWorkerStatus = { tunnel_id: "t-1", image: pin, hostnames: ["app.example.com"], phase: "active", running: true, connected: true, withheld: null };
const configured = { zone_id: "z1", hostnames: ["app.example.com"], image: pin };

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

function renderTunnel(target = node()) {
  render(
    <QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}>
      <CloudflareTunnel node={target} />
    </QueryClientProvider>,
  );
}

const calls = (method: string) => vi.mocked(request).mock.calls.filter(([callMethod]) => callMethod === method);

it("publishes the chosen hostnames with the pinned image and treats the accepted request as queued", async () => {
  let saved = status();
  serve((method, _path, body) => {
    if (method === "PUT") {
      saved = status({ configuration: body as TunnelStatus["configuration"], phase: "pending" });
      return saved;
    }
    return saved;
  });
  renderTunnel();

  expect(await screen.findByText("How publishing works")).toBeInTheDocument();
  // No credential is ever entered on this page.
  expect(document.querySelector("input[type=password]")).toBeNull();
  expect(screen.getByRole("combobox", { name: "Zone" })).toHaveTextContent("example.com");
  // Hostnames outside the chosen zone are not offered.
  expect(screen.queryByText("blog.example.org")).not.toBeInTheDocument();
  expect(screen.getByLabelText("Connector image")).toHaveValue(pin);
  expect(screen.getByText(/Cove's pinned official image \(2026\.11\.1\)/)).toBeInTheDocument();

  fireEvent.click(screen.getByRole("checkbox", { name: "shop.example.com" }));
  fireEvent.click(screen.getByRole("checkbox", { name: "app.example.com" }));
  fireEvent.click(screen.getByRole("button", { name: "Publish through Cloudflare" }));

  await waitFor(() => expect(request).toHaveBeenCalledWith("PUT", "/api/nodes/n1/cloudflare-tunnel", { zone_id: "z1", hostnames: ["app.example.com", "shop.example.com"], image: pin }));
  await waitFor(() => expect(calls("GET").length).toBeGreaterThanOrEqual(2));
  expect(await screen.findByText(/Requested\. Follow the status above/)).toBeInTheDocument();
  expect(screen.getByText("Queued")).toBeInTheDocument();
  expect(screen.queryByText("Published")).not.toBeInTheDocument();
  expect(screen.getByText("Unknown")).toBeInTheDocument();
});

it("keeps the Requested note until the worker confirms a running, connected connector", async () => {
  let saved = status();
  serve((method, _path, body) => {
    if (method === "PUT") saved = status({ configuration: body as TunnelStatus["configuration"], phase: "pending" });
    return saved;
  });
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(
    <QueryClientProvider client={client}>
      <CloudflareTunnel node={node()} />
    </QueryClientProvider>,
  );
  fireEvent.click(await screen.findByRole("checkbox", { name: "app.example.com" }));
  fireEvent.click(screen.getByRole("button", { name: "Publish through Cloudflare" }));
  const requested = /Requested\. Follow the status above/;
  expect(await screen.findByText(requested)).toBeInTheDocument();

  const reload = async (next: TunnelStatus) => {
    saved = next;
    await client.refetchQueries({ queryKey: ["cloudflare-tunnel", "n1"] });
  };
  // A control phase of active is not enough: the connector is running but not connected.
  await reload(status({ configuration: configured, phase: "active", worker: { ...worker, connected: false } }));
  expect(await screen.findByText("Running, not connected")).toBeInTheDocument();
  expect(screen.getByText(requested)).toBeInTheDocument();
  // A connected report from a worker read that failed is not confirmation either.
  await reload(status({ configuration: configured, phase: "active", worker, worker_error: "The worker could not be reached." }));
  expect(await screen.findByText("Could not read the connector on worker-1")).toBeInTheDocument();
  expect(screen.getByText(requested)).toBeInTheDocument();

  await reload(status({ configuration: configured, phase: "active", worker }));
  expect(await screen.findByText("Connected to the Cloudflare edge")).toBeInTheDocument();
  expect(screen.queryByText(requested)).not.toBeInTheDocument();
  expect(screen.getByText("No unsaved changes")).toBeInTheDocument();

  // After disable and cleanup the publish request is long finished, so the new empty form never shows it as queued.
  await reload(status());
  expect(await screen.findByRole("button", { name: "Publish through Cloudflare" })).toBeInTheDocument();
  expect(screen.queryByText(requested)).not.toBeInTheDocument();
  expect(screen.getByText("No unsaved changes")).toBeInTheDocument();
});

it("keeps the selections when publishing is refused", async () => {
  serve((method) => {
    if (method === "PUT") throw new ApiError(409, "A DNS record already exists for shop.example.com.");
    return status();
  });
  renderTunnel();
  fireEvent.click(await screen.findByRole("checkbox", { name: "shop.example.com" }));
  fireEvent.click(screen.getByRole("button", { name: "Publish through Cloudflare" }));

  expect(await screen.findByText("A DNS record already exists for shop.example.com.")).toBeInTheDocument();
  expect(screen.getByText("Your selections are kept.")).toBeInTheDocument();
  expect(screen.getByRole("checkbox", { name: "shop.example.com" })).toBeChecked();
});

it("refuses an unpinned or unofficial connector image before sending anything", async () => {
  serve(() => status());
  renderTunnel();
  fireEvent.click(await screen.findByRole("checkbox", { name: "app.example.com" }));
  const image = screen.getByLabelText("Connector image");
  fireEvent.change(image, { target: { value: "cloudflare/cloudflared:latest" } });
  expect(screen.getByText(/An administrator override/)).toBeInTheDocument();
  fireEvent.click(screen.getByRole("button", { name: "Publish through Cloudflare" }));

  expect(await screen.findByText("Pin a release version such as 2026.11.1, not latest.")).toBeInTheDocument();
  await waitFor(() => expect(image).toHaveFocus());
  expect(calls("PUT")).toHaveLength(0);

  fireEvent.click(screen.getByRole("button", { name: "Use Cove default" }));
  expect(image).toHaveValue(pin);
});

it("keeps an administrator image override and locks the zone of an existing tunnel", async () => {
  serve((method, _path, body) => (method === "PUT" ? status({ configuration: body as TunnelStatus["configuration"], phase: "pending" }) : status({ configuration: { ...configured, image: override }, phase: "active", worker })));
  renderTunnel();

  const image = await screen.findByLabelText("Connector image");
  expect(image).toHaveValue(override);
  expect(screen.getByText(/Cove releases never change it/)).toBeInTheDocument();
  expect(screen.getByRole("combobox", { name: "Zone" })).toBeDisabled();

  fireEvent.click(screen.getByRole("checkbox", { name: "shop.example.com" }));
  fireEvent.click(screen.getByRole("button", { name: "Save changes" }));
  await waitFor(() => expect(request).toHaveBeenCalledWith("PUT", "/api/nodes/n1/cloudflare-tunnel", { zone_id: "z1", hostnames: ["app.example.com", "shop.example.com"], image: override }));
});

it("distinguishes running from connected and lists withheld hostnames with their reasons", async () => {
  serve(() =>
    status({
      configuration: { ...configured, hostnames: ["app.example.com", "shop.example.com"] },
      phase: "active",
      worker: { ...worker, connected: false, withheld: { "shop.example.com": "Origin certificate is not trusted." } },
      dns: [{ hostname: "app.example.com", phase: "present", cleanup_requested: false, last_error: "" }],
    }),
  );
  renderTunnel();

  expect(await screen.findByText("Running, not connected")).toBeInTheDocument();
  expect(screen.getByText("Published")).toBeInTheDocument();
  expect(screen.getByText("Present")).toBeInTheDocument();
  expect(screen.getByText("1 hostname is withheld by the worker")).toBeInTheDocument();
  expect(screen.getByText(/Origin certificate is not trusted\./)).toBeInTheDocument();
  expect(within(screen.getByRole("table", { name: "DNS records owned by this tunnel" })).getByText("app.example.com")).toBeInTheDocument();
});

it("shows an unreadable worker as unknown and retries the saved intent without a body", async () => {
  serve((method) => (method === "POST" ? status({ configuration: configured, phase: "connecting" }) : status({ configuration: configured, phase: "active", worker, worker_error: "The worker could not be reached." })));
  renderTunnel();

  expect(await screen.findByText("Unknown")).toBeInTheDocument();
  expect(screen.queryByText("Connected to the Cloudflare edge")).not.toBeInTheDocument();
  expect(screen.getByText("Could not read the connector on worker-1")).toBeInTheDocument();

  fireEvent.click(screen.getByRole("button", { name: "Retry" }));
  await waitFor(() => expect(request).toHaveBeenCalledWith("POST", "/api/nodes/n1/cloudflare-tunnel/retry"));
  expect(await screen.findByText("Retry requested")).toBeInTheDocument();
});

it("disables only after confirmation and follows cleanup until it finishes", async () => {
  let saved = status({ configuration: configured, phase: "active", worker });
  serve((method) => {
    if (method === "DELETE") saved = status({ phase: "disabling", cleanup_requested: true, dns: [{ hostname: "app.example.com", phase: "present", cleanup_requested: true, last_error: "" }] });
    return saved;
  });
  renderTunnel();

  fireEvent.click(await screen.findByRole("button", { name: "Disable tunnel" }));
  expect(calls("DELETE")).toHaveLength(0);
  const dialog = screen.getByRole("dialog", { name: "Disable the Cloudflare Tunnel on worker-1?" });
  fireEvent.click(within(dialog).getByRole("button", { name: "Disable and clean up" }));

  await waitFor(() => expect(request).toHaveBeenCalledWith("DELETE", "/api/nodes/n1/cloudflare-tunnel"));
  expect(await screen.findByText("Cleanup requested")).toBeInTheDocument();
  expect(screen.getByText("Removal pending")).toBeInTheDocument();
  expect(screen.queryByRole("button", { name: "Disable tunnel" })).not.toBeInTheDocument();
  // A new tunnel cannot be published until cleanup finishes.
  expect(screen.getByRole("button", { name: "Publish through Cloudflare" })).toBeDisabled();
});

it("keeps blocked cleanup visible with its error and offers only a retry", async () => {
  serve(() => status({ phase: "blocked", cleanup_requested: true, last_error: "Cloudflare API is unreachable.", dns: [{ hostname: "app.example.com", phase: "present", cleanup_requested: true, last_error: "DNS delete failed." }] }));
  renderTunnel();

  expect(await screen.findByText("Cleanup is blocked")).toBeInTheDocument();
  expect(screen.getByText("Cloudflare API is unreachable.")).toBeInTheDocument();
  expect(screen.getByText("Removal failed")).toBeInTheDocument();
  expect(screen.getByText("DNS delete failed.")).toBeInTheDocument();
  expect(screen.getByRole("button", { name: "Retry" })).toBeEnabled();
  expect(screen.queryByRole("button", { name: "Disable tunnel" })).not.toBeInTheDocument();
});

it("keeps publishing read-only when the worker's support is unknown or missing", async () => {
  serve(() => status({ configuration: configured, phase: "blocked", last_error: "Connector stopped." }));
  renderTunnel(node(null));
  expect(screen.getByText("Worker capabilities unknown")).toBeInTheDocument();
  expect(await screen.findByRole("checkbox", { name: "app.example.com" })).toBeDisabled();
  expect(screen.getByRole("button", { name: "Retry" })).toBeEnabled();
  expect(screen.getByRole("button", { name: "Disable tunnel" })).toBeEnabled();
});

it("pauses every action while offline", async () => {
  vi.spyOn(navigator, "onLine", "get").mockReturnValue(false);
  serve(() => status({ configuration: configured, phase: "blocked", last_error: "Connector stopped." }));
  renderTunnel();
  expect(screen.getByText("You are offline")).toBeInTheDocument();
  expect(await screen.findByRole("button", { name: "Retry" })).toBeDisabled();
  expect(screen.getByRole("button", { name: "Disable tunnel" })).toBeDisabled();
  expect(screen.getByRole("checkbox", { name: "app.example.com" })).toBeDisabled();
});

it("explains that OAuth-only zones cannot manage tunnels", async () => {
  serve(() => status({ zones: [{ id: "z2", domain: "example.org", connection_kind: "oauth" }] }));
  renderTunnel();
  expect(await screen.findByText("No zone can manage tunnels")).toBeInTheDocument();
});

it("shows a failed status read with Retry", async () => {
  serve(() => {
    throw new ApiError(403, "Access denied. Your account does not have permission for this action.");
  });
  renderTunnel();
  expect(await screen.findByText("Could not load the Cloudflare Tunnel status")).toBeInTheDocument();
  expect(screen.getByText("Access denied. Your account does not have permission for this action.")).toBeInTheDocument();
});

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { request } from "../api/client";
import type { CleanupEnvelope, CleanupPolicy } from "../api/cleanup";
import type { Node } from "../api/types";
import { NodeCleanup } from "./NodeCleanup";

vi.mock("../api/client", () => ({ request: vi.fn(), errorMessage: (error: Error) => error.message, isStatus: (error: { status?: number }, status: number) => error.status === status }));
vi.mock("@tanstack/react-router", () => ({ useBlocker: () => ({ status: "idle" }) }));
beforeEach(() => {
  vi.stubGlobal("ResizeObserver", class { observe = vi.fn(); disconnect = vi.fn(); });
  HTMLDialogElement.prototype.showModal = vi.fn(function (this: HTMLDialogElement) { this.open = true; });
  HTMLDialogElement.prototype.close = vi.fn(function (this: HTMLDialogElement) { this.open = false; });
});
afterEach(() => { vi.clearAllMocks(); vi.unstubAllGlobals(); });

const node = { id: "n1", name: "edge-1", state: "ready", mode: "local", address: "", port: 0, last_checked_at: null, capabilities: { scheduled_cleanup: true, protocol: 1, os: "", arch: "", docker: "", compose: "", ready: true } } satisfies Node;
const policy: CleanupPolicy = { enabled: true, interval_hours: 6, minimum_age_hours: 168, images: true, build_cache: true, volumes: false, non_cove: false };
const state = { policy, last_run: null, last_error: "", removed: 0, estimated_bytes: 0 };
const revision = "a".repeat(64);
const plan = {
  items: [
    { kind: "image" as const, id: "sha256:abc", bytes: 1024 * 1024 },
    { kind: "cache" as const, id: "cache1", bytes: null },
    { kind: "volume" as const, id: "orphaned-data", bytes: 2048 },
  ],
  estimated_bytes: 1024 * 1024 + 2048,
  unknown_sizes: true,
  skipped_cache: 2,
  revision,
  sampled_at: "2026-11-22T12:00:00Z",
};

function mock(handlers: { run?: () => Promise<CleanupEnvelope> } = {}) {
  vi.mocked(request).mockImplementation((method, path) => {
    if (path === "/api/nodes/n1/cleanup-policy") return Promise.resolve(method === "PUT" ? { state: { ...state, policy: { ...policy, volumes: true } }, plan: null } : { state, plan: null });
    if (path === "/api/nodes/n1/cleanup-plan") return Promise.resolve({ state, plan });
    if (path === "/api/nodes/n1/cleanup-run") return handlers.run ? handlers.run() : Promise.resolve({ state: { ...state, last_run: "2026-11-22T12:01:00Z", removed: 3, estimated_bytes: 1050624 }, plan: null });
    return Promise.reject(new Error(`unexpected ${path}`));
  });
  render(<QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}><NodeCleanup node={node} /></QueryClientProvider>);
}

it("previews with unknown sizes and requires the node name before deleting volumes", async () => {
  mock();
  expect(await screen.findByText("Never")).toBeInTheDocument();
  fireEvent.click(screen.getByRole("button", { name: "Preview cleanup" }));
  await waitFor(() => expect(request).toHaveBeenCalledWith("POST", "/api/nodes/n1/cleanup-plan", {}));
  const table = await screen.findByRole("table", { name: "Items the cleanup would remove" });
  expect(within(table).getByText("Unknown")).toBeInTheDocument();
  expect(screen.getByText(/1 image, 1 build cache record, 1 volume/)).toBeInTheDocument();
  expect(screen.getAllByText(/1 item has an unknown size/).length).toBeGreaterThan(0);
  expect(screen.getByText(/2 build cache records kept/)).toBeInTheDocument();
  expect(request).not.toHaveBeenCalledWith("POST", "/api/nodes/n1/cleanup-run", expect.anything());

  fireEvent.click(screen.getByRole("button", { name: "Run cleanup" }));
  const dialog = await screen.findByRole("dialog", { name: "Run cleanup on edge-1?" });
  expect(within(dialog).getByText("1 volume will be deleted with their data")).toBeInTheDocument();
  const confirm = within(dialog).getByRole("button", { name: "Run cleanup" });
  expect(confirm).toBeDisabled();
  fireEvent.change(within(dialog).getByLabelText("Type edge-1 to confirm"), { target: { value: "edge-1" } });
  fireEvent.click(confirm);
  await waitFor(() => expect(request).toHaveBeenCalledWith("POST", "/api/nodes/n1/cleanup-run", { revision }));
  expect(await screen.findByText("Cleanup finished")).toBeInTheDocument();
  expect(screen.queryByRole("table", { name: "Items the cleanup would remove" })).not.toBeInTheDocument();
});

it("reports a partial run, reloads state and does not retry", async () => {
  mock({ run: () => Promise.reject(Object.assign(new Error("cleanup could not be confirmed"), { status: 409 })) });
  fireEvent.click(await screen.findByRole("button", { name: "Preview cleanup" }));
  fireEvent.click(await screen.findByRole("button", { name: "Run cleanup" }));
  const dialog = await screen.findByRole("dialog");
  fireEvent.change(within(dialog).getByLabelText("Type edge-1 to confirm"), { target: { value: "edge-1" } });
  fireEvent.click(within(dialog).getByRole("button", { name: "Run cleanup" }));
  expect(await screen.findByText("Cleanup did not finish")).toBeInTheDocument();
  expect(screen.getByText(/Some items may already have been removed/)).toBeInTheDocument();
  expect(vi.mocked(request).mock.calls.filter(([, path]) => path === "/api/nodes/n1/cleanup-run")).toHaveLength(1);
  await waitFor(() => expect(vi.mocked(request).mock.calls.filter(([method, path]) => method === "GET" && path === "/api/nodes/n1/cleanup-policy")).toHaveLength(2));
  expect(screen.getByRole("button", { name: "Preview cleanup" })).toBeInTheDocument();
});

it("confirms before allowing volume cleanup and blocks previews while the policy is unsaved", async () => {
  mock();
  const volumes = await screen.findByRole("switch", { name: "Unused volumes" });
  fireEvent.click(volumes);
  expect(screen.getByRole("button", { name: "Preview cleanup" })).toBeDisabled();
  expect(screen.getByText(/Save or discard the policy changes first/)).toBeInTheDocument();
  fireEvent.click(screen.getByRole("button", { name: "Save policy" }));
  const dialog = await screen.findByRole("dialog", { name: "Allow cleanup to remove more?" });
  expect(within(dialog).getByText(/Deleted volume data cannot be recovered/)).toBeInTheDocument();
  expect(request).not.toHaveBeenCalledWith("PUT", expect.anything(), expect.anything());
  fireEvent.click(within(dialog).getByRole("button", { name: "Save policy" }));
  await waitFor(() => expect(request).toHaveBeenCalledWith("PUT", "/api/nodes/n1/cleanup-policy", { ...policy, volumes: true }));
  await waitFor(() => expect(screen.getByRole("button", { name: "Preview cleanup" })).toBeEnabled());
});

it("validates schedule hours before saving", async () => {
  mock();
  fireEvent.change(await screen.findByLabelText("Minimum age"), { target: { value: "12" } });
  fireEvent.click(screen.getByRole("button", { name: "Save policy" }));
  expect(await screen.findByText("Enter a whole number of hours from 24 to 8,760.")).toBeInTheDocument();
  expect(request).not.toHaveBeenCalledWith("PUT", expect.anything(), expect.anything());
});

it("explains when the worker cannot provide cleanup controls", async () => {
  vi.mocked(request).mockRejectedValue(Object.assign(new Error("upgrade and verify a ready worker for cleanup policies"), { status: 409 }));
  render(<QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}><NodeCleanup node={node} /></QueryClientProvider>);
  expect(await screen.findByText("Cleanup controls unavailable")).toBeInTheDocument();
  expect(screen.getByText(/upgrade and verify a ready worker/)).toBeInTheDocument();
});

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import type { HostPatchStatus } from "../api/hostMaintenance";
import { ApiError, request } from "../api/client";
import type { Node } from "../api/types";
import { HostSecurityUpdates } from "./HostSecurityUpdates";

vi.mock("../api/client", async (original) => ({ ...(await original<typeof import("../api/client")>()), request: vi.fn() }));

beforeEach(() => {
  HTMLDialogElement.prototype.showModal = function (this: HTMLDialogElement) { this.setAttribute("open", ""); };
  HTMLDialogElement.prototype.close = function (this: HTMLDialogElement) { this.removeAttribute("open"); };
});
afterEach(() => { vi.clearAllMocks(); vi.unstubAllGlobals(); });

const capabilities = { protocol: 1, os: "linux", arch: "amd64", docker: "27", compose: "2", ready: true, host_maintenance: true };
const node: Node = { id: "n1", name: "worker", state: "ready", mode: "ssh", address: "10.0.0.12", port: 22, last_checked_at: null, capabilities };
const path = "/api/nodes/n1/host-maintenance";

const apt: HostPatchStatus = {
  checked_at: "2026-11-22T10:00:00Z",
  os: "ubuntu",
  version: "24.04",
  manager: "apt",
  security_updates: 2,
  packages: ["openssl", "libssl3t64"],
  limited: false,
  metadata_at: "2026-11-22T08:00:00Z",
  reboot_required: true,
  automatic_reboot: false,
  policy_supported: true,
  policy_enabled: false,
  last_result: "",
  notices: [],
};

function mockApi(status: HostPatchStatus, put?: (enabled: boolean) => Promise<unknown>) {
  vi.mocked(request).mockImplementation((method, url, body) => {
    if (url !== path) return Promise.reject(new Error(`unexpected ${method} ${url}`));
    if (method === "PUT" && put) return put((body as { enabled: boolean }).enabled);
    return Promise.resolve(status);
  });
}

function renderSection(target: Node = node) {
  return render(<QueryClientProvider client={new QueryClient()}><HostSecurityUpdates node={target} /></QueryClientProvider>);
}

const gets = () => vi.mocked(request).mock.calls.filter(([method]) => method === "GET").length;

it("inspects only when asked and shows cached metadata details", async () => {
  mockApi(apt);
  renderSection();
  expect(screen.getByText(/Not inspected yet/)).toBeInTheDocument();
  expect(request).not.toHaveBeenCalled();
  fireEvent.click(screen.getByRole("button", { name: "Inspect" }));
  expect(await screen.findByText("ubuntu 24.04")).toBeInTheDocument();
  expect(screen.getByText("2 pending in cached metadata")).toBeInTheDocument();
  expect(screen.getByText("Not allowed by the host's policy")).toBeInTheDocument();
  const packages = screen.getByRole("region", { name: "Packages with pending security updates" });
  expect(packages).toHaveAttribute("tabindex", "0");
  packages.focus();
  expect(packages).toHaveFocus();
  expect(within(packages).getAllByRole("listitem")).toHaveLength(2);
  expect(screen.queryByText("Last run")).not.toBeInTheDocument();
  expect(screen.getByRole("button", { name: "Inspect again" })).toBeInTheDocument();
  expect(request).toHaveBeenCalledTimes(1);
});

it("keeps unknown values unknown and shows backend notices", async () => {
  mockApi({ ...apt, manager: "dnf", os: "", version: "", security_updates: null, packages: [], metadata_at: null, reboot_required: null, automatic_reboot: null, last_result: "exit-code", notices: ["Package metadata time is unknown; an empty cached result does not prove the host is current."] });
  renderSection();
  fireEvent.click(screen.getByRole("button", { name: "Inspect" }));
  expect(await screen.findByText(/Cached metadata could not be read/)).toBeInTheDocument();
  expect(screen.queryByText(/None in cached metadata/)).not.toBeInTheDocument();
  expect(screen.getByText(/DNF does not report a reboot policy/)).toBeInTheDocument();
  expect(screen.getByText(/an empty cached result does not prove/)).toBeInTheDocument();
  expect(screen.getByText("Failed")).toBeInTheDocument();
  expect(screen.getByText("exit-code")).toBeInTheDocument();
});

it("summarises a limited package list", async () => {
  mockApi({ ...apt, security_updates: 350, packages: Array.from({ length: 200 }, (_, index) => `pkg${index}`), limited: true });
  renderSection();
  fireEvent.click(screen.getByRole("button", { name: "Inspect" }));
  expect(await screen.findByText("Packages (first 200 of 350)")).toBeInTheDocument();
});

it("confirms before enabling and shows the host's read-back status", async () => {
  mockApi(apt, (enabled) => Promise.resolve({ ...apt, policy_enabled: enabled, checked_at: "2026-11-22T10:05:00Z" }));
  renderSection();
  fireEvent.click(screen.getByRole("button", { name: "Inspect" }));
  fireEvent.click(await screen.findByRole("button", { name: "Enable schedule" }));
  const dialog = screen.getByRole("dialog", { name: "Enable Cove's security-update schedule?" });
  expect(dialog).toHaveTextContent(/04:00 UTC/);
  expect(dialog).toHaveTextContent(/unattended-upgrades allowed origins/);
  expect(request).not.toHaveBeenCalledWith("PUT", path, expect.anything());
  fireEvent.click(within(dialog).getByRole("button", { name: "Enable schedule" }));
  await waitFor(() => expect(request).toHaveBeenCalledWith("PUT", path, { enabled: true, confirm: true }));
  expect(await screen.findByText("Schedule enabled")).toBeInTheDocument();
  expect(screen.getByText("Enabled")).toBeInTheDocument();
  expect(screen.getByRole("button", { name: "Disable schedule" })).toBeInTheDocument();
});

it("re-inspects after an ambiguous failure without claiming the change failed", async () => {
  mockApi({ ...apt, policy_enabled: true }, () => Promise.reject(new ApiError(502, "Host maintenance request could not be confirmed; refresh before retrying")));
  renderSection();
  fireEvent.click(screen.getByRole("button", { name: "Inspect" }));
  fireEvent.click(await screen.findByRole("button", { name: "Disable schedule" }));
  const dialog = screen.getByRole("dialog", { name: "Disable Cove's security-update schedule?" });
  expect(dialog).toHaveTextContent(/does not interrupt an update that is already running/);
  fireEvent.click(within(dialog).getByRole("button", { name: "Disable schedule" }));
  expect(await screen.findByText(/may have been applied before the connection failed/)).toBeInTheDocument();
  await waitFor(() => expect(gets()).toBe(2));
});

it("reports definite refusals and still refreshes the status", async () => {
  mockApi(apt, () => Promise.reject(new ApiError(409, "Verify an upgraded Linux worker and wait for active operations before changing its schedule")));
  renderSection();
  fireEvent.click(screen.getByRole("button", { name: "Inspect" }));
  fireEvent.click(await screen.findByRole("button", { name: "Enable schedule" }));
  fireEvent.click(within(screen.getByRole("dialog")).getByRole("button", { name: "Enable schedule" }));
  expect(await screen.findByText(/wait for active operations/)).toBeInTheDocument();
  expect(screen.queryByText(/may have been applied/)).not.toBeInTheDocument();
  await waitFor(() => expect(gets()).toBe(2));
});

it("blocks enabling on unsupported hosts but allows disabling an enabled timer", async () => {
  mockApi({ ...apt, manager: "unsupported", security_updates: null, packages: [], policy_supported: false, notices: ["This host distribution has no supported security-update provider."] });
  const { unmount } = renderSection();
  fireEvent.click(screen.getByRole("button", { name: "Inspect" }));
  expect(await screen.findByText("Host prerequisites missing")).toBeInTheDocument();
  expect(screen.getByText("No supported provider")).toBeInTheDocument();
  expect(screen.getByRole("button", { name: "Enable schedule" })).toBeDisabled();
  unmount();

  mockApi({ ...apt, policy_supported: false, policy_enabled: true });
  renderSection();
  fireEvent.click(screen.getByRole("button", { name: "Inspect" }));
  expect(await screen.findByRole("button", { name: "Disable schedule" })).toBeEnabled();
});

it("explains why inspection is unavailable", () => {
  const { unmount } = renderSection({ ...node, capabilities: { ...capabilities, host_maintenance: false } });
  expect(screen.getByText(/has not reported support for host maintenance/)).toBeInTheDocument();
  expect(screen.getByRole("button", { name: "Inspect" })).toBeDisabled();
  unmount();

  const unreachable = renderSection({ ...node, state: "unreachable" });
  expect(screen.getByText(/needs a ready worker/)).toBeInTheDocument();
  expect(screen.getByRole("button", { name: "Inspect" })).toBeDisabled();
  unreachable.unmount();

  vi.spyOn(navigator, "onLine", "get").mockReturnValue(false);
  renderSection();
  expect(screen.getByText(/You are offline/)).toBeInTheDocument();
  expect(screen.getByRole("button", { name: "Inspect" })).toBeDisabled();
  vi.restoreAllMocks();
});

it("shows inspection errors with a retry", async () => {
  vi.mocked(request).mockRejectedValue(new ApiError(502, "Host maintenance request could not be confirmed; refresh before retrying"));
  renderSection();
  fireEvent.click(screen.getByRole("button", { name: "Inspect" }));
  expect(await screen.findByText("Could not load host security status")).toBeInTheDocument();
  expect(screen.getByRole("button", { name: "Inspect again" })).toBeEnabled();
});

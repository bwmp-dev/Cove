import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { ApiError, request } from "../api/client";
import type { PanelRecovery as Recovery } from "../api/panel-recovery";
import { PanelRecovery } from "./PanelRecovery";

vi.mock("../api/client", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../api/client")>();
  return { ...actual, request: vi.fn() };
});
vi.mock("@tanstack/react-router", () => ({ useBlocker: () => ({ status: "idle" }) }));
beforeEach(() => vi.stubGlobal("ResizeObserver", class { observe = vi.fn(); disconnect = vi.fn(); }));
afterEach(() => {
  vi.resetAllMocks();
  vi.unstubAllGlobals();
});
HTMLDialogElement.prototype.showModal = function (this: HTMLDialogElement) {
  this.open = true;
};
HTMLDialogElement.prototype.close = function (this: HTMLDialogElement) {
  this.open = false;
};

const snapshotId = "ab12cd34".padEnd(64, "0");
const base: Recovery = {
  config: { enabled: true, interval_hours: 24, keep_last: 7, keep_days: 30, max_bytes: 8589934592, storage_id: "" },
  password_set: false,
  agent_id: "",
  checked_at: null,
  last_error: "",
  updates_paused: false,
  pending_recovery: false,
  offsite_pending: false,
  public_origin: "https://panel.example.com",
  snapshots: [],
  jobs: [],
};
const registered: Recovery = {
  ...base,
  agent_id: "host-agent",
  checked_at: new Date().toISOString(),
  snapshots: [
    { id: snapshotId, created_at: "2026-11-21T10:00:00Z", version: "v1.4.0", revision: "abcdef0123", bytes: 52428800, verified: true, offsite: false, origin: "https://panel.example.com" },
    { id: "ff".repeat(32), created_at: "2026-11-20T10:00:00Z", version: "v1.3.0", revision: "", bytes: 1024, verified: true, offsite: true, origin: "https://old.example.com" },
  ],
};
const locations = [{ id: "s1", name: "Offsite", storage: { protocol: "s3", endpoint: "https://s3.example", path: "bucket" } }];

function serve(recovery: Recovery | (() => Promise<Recovery>)) {
  vi.mocked(request).mockImplementation((method: string, path: string, body?: unknown) => {
    if (path === "/api/storage-locations") return Promise.resolve(locations);
    if (path === "/api/settings/panel-backup/actions") return Promise.resolve({ id: "job", state: "queued" });
    if (method === "PUT") return Promise.resolve({ ...base, config: body, password_set: true });
    return typeof recovery === "function" ? recovery() : Promise.resolve(recovery);
  });
}

function wrap() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(<QueryClientProvider client={client}><PanelRecovery /></QueryClientProvider>);
  return client;
}

it("shows the host setup command and disables actions until the host timer connects", async () => {
  serve(base);
  wrap();
  expect(await screen.findByText("Host timer not connected")).toBeInTheDocument();
  expect(screen.getByText("sudo sh scripts/install-panel-recovery.sh /opt/cove")).toBeInTheDocument();
  expect(screen.getByRole("button", { name: "Back up now" })).toBeDisabled();
  expect(screen.getByText(/Snapshots appear here once the host timer is installed/)).toBeInTheDocument();
});

it("restores only after typing the snapshot code and never offers other origins", async () => {
  serve(registered);
  wrap();
  expect(await screen.findByText("Host timer connected")).toBeInTheDocument();
  expect(screen.getByText(/Taken for https:\/\/old\.example\.com/)).toBeInTheDocument();
  fireEvent.click(screen.getByRole("button", { name: /^Restore ?snapshot from/ }));
  const dialog = await screen.findByRole("dialog", { name: "Roll back the panel to this snapshot?" });
  expect(within(dialog).getByText(/Resource and worker data is not rolled back/)).toBeInTheDocument();
  const confirm = within(dialog).getByRole("button", { name: "Restore snapshot" });
  expect(confirm).toBeDisabled();
  fireEvent.change(within(dialog).getByLabelText("Type ab12cd34 to confirm"), { target: { value: "ab12cd34" } });
  fireEvent.click(confirm);
  await waitFor(() => expect(request).toHaveBeenCalledWith("POST", "/api/settings/panel-backup/actions", { action: "restore", snapshot_id: snapshotId, confirm: true }));
});

it("asks before resuming paused updates", async () => {
  serve({ ...registered, updates_paused: true });
  wrap();
  fireEvent.click(await screen.findByRole("button", { name: "Resume updates" }));
  const dialog = await screen.findByRole("dialog", { name: "Resume panel updates?" });
  expect(request).not.toHaveBeenCalledWith("POST", expect.anything(), expect.anything());
  fireEvent.click(within(dialog).getByRole("button", { name: "Resume updates" }));
  await waitFor(() => expect(request).toHaveBeenCalledWith("POST", "/api/settings/panel-backup/actions", { action: "resume_updates", confirm: true }));
});

it("requires an offsite password for a new destination and sends the full configuration", async () => {
  serve(registered);
  wrap();
  const storage = await screen.findByRole("combobox", { name: "Offsite copy" });
  await waitFor(() => expect(storage).toBeEnabled());
  fireEvent.click(storage);
  fireEvent.click(await screen.findByRole("option", { name: "Offsite (s3)" }));
  fireEvent.click(screen.getByRole("button", { name: "Save changes" }));
  expect(await screen.findByText("Enter an encryption password for the offsite copy.")).toBeInTheDocument();
  expect(request).not.toHaveBeenCalledWith("PUT", expect.anything(), expect.anything());
  fireEvent.change(screen.getByLabelText("Offsite encryption password"), { target: { value: "independent-panel-secret" } });
  fireEvent.click(screen.getByRole("button", { name: "Save changes" }));
  await waitFor(() =>
    expect(request).toHaveBeenCalledWith("PUT", "/api/settings/panel-backup", { enabled: true, interval_hours: 24, keep_last: 7, keep_days: 30, max_bytes: 8589934592, storage_id: "s1", password: "independent-panel-secret" }),
  );
  expect(await screen.findByText(/Saved. The host uses these settings/)).toBeInTheDocument();
  expect(screen.getByLabelText(/Offsite encryption password/)).toHaveValue("");
});

it("explains the outage when the panel stops answering during a restore", async () => {
  let calls = 0;
  const restoring = { ...registered, jobs: [{ id: "j1", action: "restore" as const, snapshot_id: snapshotId, state: "running" as const, requested_at: new Date().toISOString(), finished_at: null, error: "" }] };
  serve(() => (calls++ === 0 ? Promise.resolve(restoring) : Promise.reject(new ApiError(0, "Cannot reach the control plane. Check your connection."))));
  const client = wrap();
  expect(await screen.findByText("Running")).toBeInTheDocument();
  expect(screen.getByRole("button", { name: "Back up now" })).toBeDisabled();
  await client.refetchQueries({ queryKey: ["panel-recovery"] });
  expect(await screen.findByText("The panel is restoring a snapshot")).toBeInTheDocument();
  expect(screen.getByRole("button", { name: "Reload" })).toBeEnabled();
  expect(screen.getByRole("button", { name: "Back up now" })).toBeDisabled();
});

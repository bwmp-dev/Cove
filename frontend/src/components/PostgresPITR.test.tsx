import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, expect, it, vi } from "vitest";
import { ApiError, request } from "../api/client";
import type { BackupPolicy } from "../api/backups";
import type { PITRInfo } from "../api/pitr";
import type { Permission } from "../api/types";
import { MiB } from "../lib/backups";
import { pitrRights } from "../lib/permissions";
import { utcInputValue } from "../lib/pitr";
import { PostgresPITR } from "./PostgresPITR";

vi.mock("../api/client", async (original) => ({ ...(await original<typeof import("../api/client")>()), request: vi.fn() }));
vi.mock("@tanstack/react-router", () => ({ Link: ({ children }: { children: React.ReactNode }) => <span>{children}</span> }));
HTMLDialogElement.prototype.showModal = function (this: HTMLDialogElement) {
  this.open = true;
};
HTMLDialogElement.prototype.close = function (this: HTMLDialogElement) {
  this.open = false;
};
afterEach(() => vi.resetAllMocks());

const settings = { enabled: true, service: "db", interval_hours: 12, keep_last: 4, retention_days: 7, max_bytes: 3 * 1024 * MiB + 7 };
const iso = (offset: number) => new Date(Date.now() + offset).toISOString();

function configured(overrides: Partial<Extract<PITRInfo, { configured: true }>["status"]> = {}, settingsOverride: Partial<typeof settings> = {}): PITRInfo {
  return {
    configured: true,
    supported: true,
    settings: { ...settings, ...settingsOverride },
    storage_id: "",
    operation_id: "op-latest",
    status: { enabled: true, state: "capturing", checked_at: iso(-2_000), measured_at: iso(-60_000), last_archive_at: iso(-30_000), bytes: 512 * MiB, objects: 42, receiver: true, needs_base: false, gaps: false, bases: [], ...overrides },
  };
}

const postgresPolicy: BackupPolicy = { repository: "local", keep_last: 3, interval_hours: 24, database: { engine: "postgresql", service: "db", name: "app" } };

const allRights: Permission[] = ["view", "operate", "manage_backups", "edit_configuration", "read_secrets"];

function renderPITR(pitr: PITRInfo | Error, { policy = postgresPolicy, permissions = allRights, captureHeld, restoreHeld, managed }: { policy?: BackupPolicy | null; permissions?: Permission[]; captureHeld?: string; restoreHeld?: string; managed?: boolean } = {}) {
  vi.mocked(request).mockImplementation((method: string, path: string) => {
    if (method === "GET" && path.endsWith("/pitr")) return pitr instanceof Error ? Promise.reject(pitr) : Promise.resolve(pitr);
    if (path.endsWith("/backup-policy")) return Promise.resolve({ backup: policy });
    if (path.endsWith("/restore-copy")) return Promise.resolve({ id: "op-restore", resource_id: "w-copy", credentials: { username: "postgres", password: "one-time-secret-value" } });
    if (method !== "GET") return Promise.resolve({ id: "op-new" });
    return Promise.resolve([]);
  });
  render(<QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}><PostgresPITR workloadId="w1" managed={managed} busy={false} rights={pitrRights(permissions)} nodeName="edge" captureHeld={captureHeld} restoreHeld={restoreHeld} /></QueryClientProvider>);
}

function mutation(method: string, suffix: string) {
  return vi.mocked(request).mock.calls.find(([m, path]) => m === method && path === `/api/workloads/w1${suffix}`);
}

it("asks for a backup policy before setup", async () => {
  renderPITR({ configured: false, supported: true, settings: null, status: null }, { policy: null });
  expect(await screen.findByText("Save a backup policy first")).toBeInTheDocument();
  expect(screen.queryByRole("button", { name: "Set up point-in-time recovery" })).not.toBeInTheDocument();
});

it("explains old workers and legacy inline repositories", async () => {
  renderPITR({ configured: false, supported: false, settings: null, status: null });
  expect(await screen.findByText("Point-in-time recovery needs an upgraded worker")).toBeInTheDocument();
});

it("refuses inline S3 repositories", async () => {
  renderPITR({ configured: false, supported: true, settings: null, status: null }, { policy: { repository: "s3:https://s3.example.com/b", keep_last: 1, interval_hours: 0 } });
  expect(await screen.findByText("Use a configured storage location")).toBeInTheDocument();
});

it("enables capture only after confirming the PostgreSQL restart", async () => {
  renderPITR({ configured: false, supported: true, settings: null, status: null });
  fireEvent.click(await screen.findByRole("button", { name: "Set up point-in-time recovery" }));
  expect(screen.getByLabelText("PostgreSQL Compose service")).toHaveValue("db");
  expect(screen.getByText(/A separate local Docker volume on this node/)).toBeInTheDocument();
  fireEvent.change(screen.getByLabelText("Base backups to keep"), { target: { value: "33" } });
  fireEvent.click(screen.getByRole("button", { name: "Enable point-in-time recovery" }));
  expect(screen.getByLabelText("Base backups to keep")).toHaveFocus();
  fireEvent.change(screen.getByLabelText("Base backups to keep"), { target: { value: "5" } });
  fireEvent.click(screen.getByRole("button", { name: "Enable point-in-time recovery" }));
  const dialog = screen.getByRole("dialog", { name: "Enable point-in-time recovery?" });
  expect(within(dialog).getByText(/PostgreSQL restarts once/)).toBeInTheDocument();
  expect(mutation("PUT", "/pitr")).toBeUndefined();
  fireEvent.click(within(dialog).getByRole("button", { name: "Restart and enable" }));
  await waitFor(() => expect(mutation("PUT", "/pitr")?.[2]).toEqual({ settings: { enabled: true, service: "db", interval_hours: 24, keep_last: 5, retention_days: 7, max_bytes: 10 * 1024 * MiB }, confirm_restart: true }));
});

it("shows status, usage and bases with known and unknown windows", async () => {
  renderPITR(configured({ bases: [
    { id: "base-old", created_at: iso(-7_200_000), bytes: 2048, postgres_major: 17 },
    { id: "base-new", created_at: iso(-3_600_000), available_to: iso(-60_000), bytes: 4096, postgres_major: 17 },
  ] }));
  expect(await screen.findByText("Capturing")).toBeInTheDocument();
  expect(screen.getByRole("meter", { name: "Archive storage" })).toHaveAttribute("aria-valuenow", String(512 * MiB));
  expect(screen.getByText("Latest PITR operation")).toBeInTheDocument();
  expect(screen.getByText("Streaming")).toBeInTheDocument();
  expect(screen.getByText("Deploys and rollbacks are paused")).toBeInTheDocument();
  expect(screen.getByText(/No known continuous recovery window/)).toBeInTheDocument();
  expect(screen.getAllByText(/^Recoverable from/)).toHaveLength(1);
  const buttons = screen.getAllByRole("button", { name: "Restore to new resource" });
  expect(buttons[0]).toBeEnabled();
  expect(buttons[1]).toBeDisabled();
});

it("restores a chosen UTC time to a new resource", async () => {
  const createdAt = Date.now() - 3_600_000;
  renderPITR(configured({ bases: [{ id: "base-new", created_at: new Date(createdAt).toISOString(), available_to: iso(-60_000), bytes: 4096, postgres_major: 17 }] }));
  fireEvent.click(await screen.findByRole("button", { name: "Restore to new resource" }));
  const dialog = screen.getByRole("dialog", { name: "Restore to a new PostgreSQL resource" });
  fireEvent.change(within(dialog).getByLabelText("New resource name"), { target: { value: " recovered " } });
  fireEvent.change(within(dialog).getByLabelText("Recovery time (UTC)"), { target: { value: utcInputValue(createdAt - 60_000) } });
  fireEvent.click(within(dialog).getByRole("button", { name: "Create recovery copy" }));
  expect(within(dialog).getByText(/^Choose a time from/)).toBeInTheDocument();
  expect(mutation("POST", "/pitr/restore-copy")).toBeUndefined();
  const target = Math.floor((createdAt + 600_000) / 1000) * 1000;
  fireEvent.change(within(dialog).getByLabelText("Recovery time (UTC)"), { target: { value: utcInputValue(target) } });
  fireEvent.click(within(dialog).getByRole("button", { name: "Create recovery copy" }));
  await waitFor(() => expect(mutation("POST", "/pitr/restore-copy")?.[2]).toEqual({ name: "recovered", base: "base-new", timestamp: new Date(target).toISOString() }));
  expect(await within(dialog).findByText("Open new resource")).toBeInTheDocument();
  expect(within(dialog).getByText(/It will not be shown again/)).toBeInTheDocument();
  const password = within(dialog).getByLabelText("Administrator password");
  expect(password).toHaveAttribute("type", "password");
  expect(password).toHaveValue("one-time-secret-value");
  expect(within(dialog).getByLabelText("Administrator username")).toHaveValue("postgres");
  fireEvent.click(within(dialog).getByRole("button", { name: "Show" }));
  expect(password).toHaveAttribute("type", "text");
  expect(within(dialog).getByRole("button", { name: "Hide" })).toHaveAttribute("aria-pressed", "true");
  const writeText = vi.fn(() => Promise.resolve());
  Object.defineProperty(navigator, "clipboard", { configurable: true, value: { writeText } });
  fireEvent.click(within(dialog).getByRole("button", { name: "Copy password" }));
  expect(writeText).toHaveBeenCalledWith("one-time-secret-value");
  expect(JSON.stringify({ ...localStorage })).not.toContain("one-time-secret-value");
  expect(JSON.stringify({ ...sessionStorage })).not.toContain("one-time-secret-value");
  expect(window.location.href).not.toContain("one-time-secret-value");

  fireEvent.click(within(dialog).getByRole("button", { name: "Close" }));
  expect(screen.queryByDisplayValue("one-time-secret-value")).not.toBeInTheDocument();
  fireEvent.click(screen.getByRole("button", { name: "Restore to new resource" }));
  const reopened = screen.getByRole("dialog", { name: "Restore to a new PostgreSQL resource" });
  expect(within(reopened).queryByLabelText("Administrator password")).not.toBeInTheDocument();
  expect(within(reopened).getByRole("button", { name: "Create recovery copy" })).toBeInTheDocument();
});

it("marks a quiet heartbeat and an unmeasured archive as unknown", async () => {
  renderPITR(configured({ checked_at: iso(-120_000), measured_at: undefined, last_archive_at: undefined, gaps: true }));
  expect(await screen.findByText("Status unknown", { selector: "span" })).toBeInTheDocument();
  expect(screen.queryByRole("meter")).not.toBeInTheDocument();
  expect(screen.getAllByText("Unknown").length).toBeGreaterThanOrEqual(2);
  expect(screen.getByText("The WAL history has gaps")).toBeInTheDocument();
});

it("shows an unset Go zero heartbeat as not checked rather than an ancient time", async () => {
  renderPITR(configured({ checked_at: "0001-01-01T00:00:00Z", measured_at: "0001-01-01T00:00:00Z" }));
  expect(await screen.findByText("Not checked yet")).toBeInTheDocument();
  expect(screen.getByText(/has not reported a capture heartbeat yet/)).toBeInTheDocument();
  expect(screen.queryByText(/days ago/)).not.toBeInTheDocument();
  expect(screen.queryByRole("meter")).not.toBeInTheDocument();
});

it("keeps viewers read-only", async () => {
  renderPITR(configured({ bases: [{ id: "b", created_at: iso(-3_600_000), available_to: iso(-60_000), bytes: 1, postgres_major: 16 }] }), { permissions: ["view", "read", "operate"] });
  expect(await screen.findByText("Capturing")).toBeInTheDocument();
  expect(screen.queryByRole("button")).not.toBeInTheDocument();
});

it("preserves an odd saved cap and requires confirmation when editing", async () => {
  renderPITR(configured());
  fireEvent.click(await screen.findByRole("button", { name: "Edit settings" }));
  expect(screen.getByLabelText("PostgreSQL Compose service")).toHaveAttribute("readonly");
  fireEvent.change(screen.getByLabelText("Base backup interval (hours)"), { target: { value: "6" } });
  fireEvent.click(screen.getByRole("button", { name: "Save settings" }));
  fireEvent.click(within(screen.getByRole("dialog", { name: "Save capture settings?" })).getByRole("button", { name: "Save settings" }));
  await waitFor(() => expect(mutation("PUT", "/pitr")?.[2]).toEqual({ settings: { ...settings, interval_hours: 6 }, confirm_restart: true }));
});

it("deletes disabled history only after typing the service name", async () => {
  renderPITR(configured({ enabled: false, state: "disabled", receiver: false }, { enabled: false }));
  expect(screen.queryByRole("button", { name: "Take base backup now" })).not.toBeInTheDocument();
  expect(screen.queryByText("Deploys and rollbacks are paused")).not.toBeInTheDocument();
  fireEvent.click(await screen.findByRole("button", { name: "Delete PITR history" }));
  const dialog = screen.getByRole("dialog", { name: "Delete point-in-time recovery history?" });
  const confirm = within(dialog).getByRole("button", { name: "Delete history" });
  expect(confirm).toBeDisabled();
  fireEvent.change(within(dialog).getByLabelText("Type db to confirm"), { target: { value: "db" } });
  fireEvent.click(confirm);
  await waitFor(() => expect(mutation("DELETE", "/pitr")?.[2]).toEqual({ confirm_delete: true }));
});

it("disables capture after confirming the restart and queues manual bases", async () => {
  renderPITR(configured());
  fireEvent.click(await screen.findByRole("button", { name: "Take base backup now" }));
  await waitFor(() => expect(mutation("POST", "/pitr/backup")?.[2]).toEqual({}));
  fireEvent.click(screen.getByRole("button", { name: "Disable capture" }));
  fireEvent.click(within(screen.getByRole("dialog", { name: "Disable capture?" })).getByRole("button", { name: "Restart and disable" }));
  await waitFor(() => expect(mutation("PUT", "/pitr")?.[2]).toEqual({ settings: { ...settings, enabled: false }, confirm_restart: true }));
});

it("reports an unreachable worker separately from other failures", async () => {
  renderPITR(new ApiError(502, "PITR status unavailable; verify worker connectivity"));
  expect(await screen.findByText("Capture status unavailable")).toBeInTheDocument();
});

const restorableBase = { id: "b", created_at: iso(-3_600_000), available_to: iso(-60_000), bytes: 1, postgres_major: 16 };

it("lets Manage backups alone take a base backup without changing capture or restoring", async () => {
  renderPITR(configured({ bases: [restorableBase] }), { permissions: ["view", "manage_backups"] });
  fireEvent.click(await screen.findByRole("button", { name: "Take base backup now" }));
  await waitFor(() => expect(mutation("POST", "/pitr/backup")).toBeDefined());
  expect(screen.queryByRole("button", { name: "Edit settings" })).not.toBeInTheDocument();
  expect(screen.queryByRole("button", { name: "Disable capture" })).not.toBeInTheDocument();
  expect(screen.queryByRole("button", { name: "Restore to new resource" })).not.toBeInTheDocument();
});

it("needs Read secrets to change capture settings or restore a copy", async () => {
  renderPITR(configured({ bases: [restorableBase] }), { permissions: ["view", "manage_backups", "edit_configuration"] });
  expect(await screen.findByRole("button", { name: "Take base backup now" })).toBeInTheDocument();
  expect(screen.queryByRole("button", { name: "Edit settings" })).not.toBeInTheDocument();
  expect(screen.queryByRole("button", { name: "Restore to new resource" })).not.toBeInTheDocument();
});

it("lets Manage backups and Edit configuration delete disabled history without Read secrets", async () => {
  renderPITR(configured({ enabled: false, state: "disabled", receiver: false }, { enabled: false }), { permissions: ["view", "manage_backups", "edit_configuration"] });
  expect(await screen.findByRole("button", { name: "Delete PITR history" })).toBeEnabled();
  expect(screen.queryByRole("button", { name: "Enable capture" })).not.toBeInTheDocument();
});

it("does not treat the legacy Edit level as PITR rights", async () => {
  renderPITR(configured({ bases: [restorableBase] }), { permissions: ["read", "operate", "edit"] });
  expect(await screen.findByText("Capturing")).toBeInTheDocument();
  expect(screen.queryByRole("button")).not.toBeInTheDocument();
});

it("holds capture changes, base backups and recovery copies while the environment is paused", async () => {
  const paused = "The environment is paused.";
  renderPITR(configured({ bases: [restorableBase] }), { captureHeld: paused, restoreHeld: paused });
  expect(await screen.findByRole("button", { name: "Edit settings" })).toBeDisabled();
  expect(screen.getByRole("button", { name: "Disable capture" })).toBeDisabled();
  expect(screen.getByRole("button", { name: "Restore to new resource" })).toBeDisabled();
  expect(screen.getByRole("button", { name: "Take base backup now" })).toBeDisabled();
});

it("explains the hold to people who can only take base backups", async () => {
  const asleep = "The app is asleep. Changing capture or taking a base backup is available after it wakes: use Start or Wake now first.";
  renderPITR(configured(), { permissions: ["view", "manage_backups"], captureHeld: asleep });
  expect(await screen.findByRole("button", { name: "Take base backup now" })).toBeDisabled();
  expect(screen.getByText(asleep)).toBeInTheDocument();
  expect(mutation("POST", "/pitr/backup")).toBeUndefined();
});

it("keeps deleting disabled history available while capture changes are held", async () => {
  renderPITR(configured({ enabled: false, state: "disabled", receiver: false }, { enabled: false }), { captureHeld: "The environment is paused." });
  expect(await screen.findByRole("button", { name: "Enable capture" })).toBeDisabled();
  expect(screen.getByRole("button", { name: "Delete PITR history" })).toBeEnabled();
});

it("explains that a managed PostgreSQL recovery copy is private without the source's TLS or pooler", async () => {
  renderPITR(configured({ bases: [restorableBase] }), { managed: true });
  fireEvent.click(await screen.findByRole("button", { name: "Restore to new resource" }));
  const dialog = screen.getByRole("dialog", { name: "Restore to a new PostgreSQL resource" });
  expect(within(dialog).getByText(/new managed PostgreSQL resource with its own private hostname/)).toBeInTheDocument();
  expect(within(dialog).getByText(/TLS, public access and the connection pooler are off, and this resource’s TLS and pooler settings are not copied/)).toBeInTheDocument();
  expect(within(dialog).getByText(/POSTGRES_PASSWORD, which later password rotations use/)).toBeInTheDocument();
  expect(within(dialog).queryByText(/no public ports or routes/)).not.toBeInTheDocument();
});

it("keeps the plain service recovery copy description for self-hosted PostgreSQL", async () => {
  renderPITR(configured({ bases: [restorableBase] }));
  fireEvent.click(await screen.findByRole("button", { name: "Restore to new resource" }));
  const dialog = screen.getByRole("dialog", { name: "Restore to a new PostgreSQL resource" });
  expect(within(dialog).getByText(/no public ports or routes/)).toBeInTheDocument();
  expect(within(dialog).queryByText(/connection pooler/)).not.toBeInTheDocument();
});

it("holds first-time setup while the environment is paused", async () => {
  renderPITR({ configured: false, supported: true, settings: null, status: null }, { captureHeld: "The environment is paused." });
  expect(await screen.findByRole("button", { name: "Set up point-in-time recovery" })).toBeDisabled();
  expect(screen.getByText("The environment is paused.")).toBeInTheDocument();
});

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { request } from "../api/client";
import type { DatabaseResource } from "../api/types";
import { Backups, type WorkerSupport } from "./Backups";

vi.mock("../api/client", () => ({ request: vi.fn(), errorMessage: (error: Error) => error.message }));
vi.mock("@tanstack/react-router", () => ({ Link: ({ children }: { children: React.ReactNode }) => <span>{children}</span> }));
HTMLDialogElement.prototype.showModal = function (this: HTMLDialogElement) {
  this.open = true;
};
HTMLDialogElement.prototype.close = function (this: HTMLDialogElement) {
  this.open = false;
};
beforeEach(() => vi.stubGlobal("ResizeObserver", class { observe = vi.fn(); disconnect = vi.fn(); }));
afterEach(() => {
  vi.resetAllMocks();
  vi.unstubAllGlobals();
});

const policy = { repository: "local", keep_last: 5, keep_weekly: 8, interval_hours: 24, drill_interval_hours: 0 };
const upgraded: WorkerSupport = { retention: true, instanceEngines: true, nodeName: "edge" };

interface Database { engine: string; service: string; name: string }
type SavedPolicy = typeof policy & { database?: Database; volume_snapshot?: { max_bytes: number } | null };
interface Snapshot { id: string; created_at: string; repository: string; database?: Database; volume_method?: string; fallback_reason?: string }

function renderBackups(saved: SavedPolicy | null, support: WorkerSupport, backups: Snapshot[] = [], preview: () => Promise<unknown> = () => Promise.reject(new Error("unexpected preview")), database?: DatabaseResource) {
  vi.mocked(request).mockImplementation((method: string, path: string) => {
    if (path.includes("/backups/snapshot-preview")) return preview();
    if (method === "PUT") return Promise.resolve({ ok: true });
    if (method === "POST" && path.endsWith("/restore-copy")) return Promise.resolve({ id: "op1", resource_id: "w2" });
    if (path.endsWith("/backup-policy")) return Promise.resolve({ backup: saved });
    if (path.endsWith("/backups")) return Promise.resolve({ backups });
    return Promise.resolve([]);
  });
  render(<QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}><Backups workloadId="w1" busy={false} rights={{ backup: true, configure: true, restore: true, download: true, import: true }} support={support} restoreHeld={null} verifyHeld={null} database={database} /></QueryClientProvider>);
}

function chooseMethod(name: string) {
  fireEvent.click(screen.getByRole("combobox", { name: "Backup method" }));
  fireEvent.click(screen.getByRole("option", { name }));
}

async function savedPolicy() {
  return waitFor(() => {
    const call = vi.mocked(request).mock.calls.find(([method, path]) => method === "PUT" && path === "/api/workloads/w1/backup-policy");
    if (!call) throw new Error("Policy was not saved");
    return (call[2] as { backup: unknown }).backup;
  });
}

it("summarises and preserves calendar tiers, sending explicit zeros for disabled tiers", async () => {
  renderBackups(policy, upgraded);
  expect(await screen.findByText(/Keep 5 latest snapshots plus 8 weekly/)).toBeInTheDocument();
  fireEvent.click(screen.getByRole("button", { name: "Edit policy" }));
  expect(screen.getByLabelText("Weekly snapshots")).toHaveValue(8);
  expect(screen.getByLabelText("Daily snapshots")).toHaveValue(0);
  expect(screen.getByLabelText("Monthly snapshots")).toHaveAttribute("max", "120");
  fireEvent.change(screen.getByLabelText("Daily snapshots"), { target: { value: "14" } });
  fireEvent.click(screen.getByRole("button", { name: "Save policy" }));
  expect(await savedPolicy()).toMatchObject({ keep_last: 5, keep_daily: 14, keep_weekly: 8, keep_monthly: 0, password: "" });
});

it("explains the upgrade and refuses calendar rules on a worker without retention support", async () => {
  renderBackups(null, { retention: false, instanceEngines: true, nodeName: "old-worker" });
  fireEvent.click(await screen.findByRole("button", { name: "Configure backups" }));
  expect(screen.getByText("Daily, weekly and monthly retention needs an upgraded worker")).toBeInTheDocument();
  fireEvent.change(screen.getByLabelText("Encryption password"), { target: { value: "a-long-enough-password" } });
  fireEvent.change(screen.getByLabelText("Monthly snapshots"), { target: { value: "3" } });
  fireEvent.click(screen.getByRole("button", { name: "Save policy" }));
  expect(await screen.findByText("Set these to 0, or upgrade and verify old-worker first.")).toBeInTheDocument();
  expect(screen.getByLabelText("Monthly snapshots")).toHaveFocus();
  expect(screen.getByLabelText("Monthly snapshots")).toHaveAttribute("aria-invalid", "true");
  expect(request).not.toHaveBeenCalledWith("PUT", expect.anything(), expect.anything());

  fireEvent.change(screen.getByLabelText("Monthly snapshots"), { target: { value: "0" } });
  fireEvent.click(screen.getByRole("button", { name: "Save policy" }));
  expect(await savedPolicy()).toMatchObject({ keep_last: 7, keep_daily: 0, keep_weekly: 0, keep_monthly: 0 });
});

it("saves MongoDB, Redis and Valkey as complete-instance backups without a database name", async () => {
  renderBackups(null, upgraded);
  fireEvent.click(await screen.findByRole("button", { name: "Configure backups" }));
  chooseMethod("Redis RDB snapshot (online)");
  expect(screen.queryByLabelText("Database name")).not.toBeInTheDocument();
  expect(screen.getByText(/Complete instance: every logical database/)).toBeInTheDocument();
  expect(screen.getByText("Redis requirements")).toBeInTheDocument();
  chooseMethod("MongoDB replica set archive (online)");
  expect(screen.getByText(/Standalone servers cannot be backed up/)).toBeInTheDocument();
  fireEvent.change(screen.getByLabelText("Database Compose service"), { target: { value: "mongo" } });
  fireEvent.change(screen.getByLabelText("Encryption password"), { target: { value: "a-long-enough-password" } });
  fireEvent.click(screen.getByRole("button", { name: "Save policy" }));
  expect(await savedPolicy()).toMatchObject({ database: { engine: "mongodb", service: "mongo", name: "all" } });
});

it("keeps the database name for PostgreSQL and MariaDB policies", async () => {
  renderBackups({ ...policy, database: { engine: "postgresql", service: "db", name: "app" } }, upgraded);
  expect(await screen.findByText(/PostgreSQL database app \(db\)/)).toBeInTheDocument();
  fireEvent.click(screen.getByRole("button", { name: "Edit policy" }));
  expect(screen.getByLabelText("Database name")).toHaveValue("app");
  chooseMethod("MariaDB logical dump (online, InnoDB only)");
  fireEvent.click(screen.getByRole("button", { name: "Save policy" }));
  expect(await savedPolicy()).toMatchObject({ database: { engine: "mariadb", service: "db", name: "app" } });
});

it("blocks instance engines on a worker without support but still allows other methods", async () => {
  renderBackups(null, { retention: true, instanceEngines: false, nodeName: "old-worker" });
  fireEvent.click(await screen.findByRole("button", { name: "Configure backups" }));
  chooseMethod("Valkey RDB snapshot (online)");
  expect(screen.getByText("Valkey backups need an upgraded worker")).toBeInTheDocument();
  fireEvent.change(screen.getByLabelText("Database Compose service"), { target: { value: "cache" } });
  fireEvent.change(screen.getByLabelText("Encryption password"), { target: { value: "a-long-enough-password" } });
  fireEvent.click(screen.getByRole("button", { name: "Save policy" }));
  expect(await screen.findByText("Choose another method, or upgrade and verify old-worker first.")).toBeInTheDocument();
  const method = screen.getByRole("combobox", { name: "Backup method" });
  expect(method).toHaveFocus();
  expect(method).toHaveAttribute("aria-invalid", "true");
  expect(request).not.toHaveBeenCalledWith("PUT", expect.anything(), expect.anything());

  chooseMethod("Volume snapshot");
  expect(screen.queryByText("Choose another method, or upgrade and verify old-worker first.")).not.toBeInTheDocument();
  fireEvent.click(screen.getByRole("button", { name: "Save policy" }));
  expect(await savedPolicy()).toMatchObject({ database: undefined });
});

it("describes complete-instance restores and restore copies without database-specific claims", async () => {
  const snapshot = { id: "snap-redis", created_at: "2026-11-21T10:00:00Z", repository: "local", database: { engine: "redis", service: "cache", name: "all" } };
  renderBackups({ ...policy, database: snapshot.database }, upgraded, [snapshot]);
  expect(await screen.findByText("Redis complete instance")).toBeInTheDocument();

  fireEvent.click(screen.getByRole("button", { name: "Restore" }));
  expect(screen.getByText("Restore Redis complete instance?")).toBeInTheDocument();
  expect(screen.getByText(/pauses this resource's containers/)).toBeInTheDocument();
  fireEvent.click(screen.getByRole("button", { name: "Cancel" }));

  fireEvent.click(screen.getByRole("button", { name: "Restore as new resource" }));
  expect(screen.getByText(/The complete Redis instance snapshot is restored into the database service. Other service volumes start empty./)).toBeInTheDocument();
  expect(screen.queryByText(/Only this database/)).not.toBeInTheDocument();
  expect(screen.queryByText(/PostgreSQL ownership and grants/)).not.toBeInTheDocument();
  fireEvent.change(screen.getByLabelText("New resource name"), { target: { value: "cache-copy" } });
  fireEvent.click(screen.getByRole("button", { name: "Restore separate resource" }));
  await waitFor(() => expect(request).toHaveBeenCalledWith("POST", "/api/workloads/w1/restore-copy", { name: "cache-copy", backup_id: "snap-redis" }));
  expect(await screen.findByText("Open new resource")).toBeInTheDocument();
});

const MiB = 1024 * 1024;
const snapshotWorker: WorkerSupport = { ...upgraded, snapshots: true };

it("keeps an untouched odd saved snapshot size exact", async () => {
  const odd = 100 * MiB + 1;
  renderBackups({ ...policy, volume_snapshot: { max_bytes: odd } }, snapshotWorker);
  expect(await screen.findByText(/Filesystem snapshot, stopped fallback/)).toBeInTheDocument();
  fireEvent.click(screen.getByRole("button", { name: "Edit policy" }));
  expect(screen.getByRole("switch", { name: "Use filesystem snapshots" })).toBeChecked();
  expect(screen.getByLabelText("LVM snapshot space (MiB)")).toHaveValue("100.00");
  fireEvent.click(screen.getByRole("button", { name: "Save policy" }));
  expect(await savedPolicy()).toMatchObject({ volume_snapshot: { max_bytes: odd } });
});

it("validates snapshot space before saving a volume policy", async () => {
  renderBackups(null, snapshotWorker);
  fireEvent.click(await screen.findByRole("button", { name: "Configure backups" }));
  fireEvent.change(screen.getByLabelText("Encryption password"), { target: { value: "a-long-enough-password" } });
  expect(screen.queryByLabelText("LVM snapshot space (MiB)")).not.toBeInTheDocument();
  fireEvent.click(screen.getByRole("switch", { name: "Use filesystem snapshots" }));
  const space = screen.getByLabelText("LVM snapshot space (MiB)");
  expect(space).toHaveValue("1024");
  fireEvent.change(space, { target: { value: "32" } });
  fireEvent.click(screen.getByRole("button", { name: "Save policy" }));
  expect(await screen.findByText("Enter a whole number from 64 to 65,536 MiB.")).toBeInTheDocument();
  expect(space).toHaveFocus();
  expect(space).toHaveAttribute("aria-invalid", "true");
  expect(request).not.toHaveBeenCalledWith("PUT", expect.anything(), expect.anything());

  fireEvent.change(space, { target: { value: "2048" } });
  fireEvent.click(screen.getByRole("button", { name: "Save policy" }));
  expect(await savedPolicy()).toMatchObject({ volume_snapshot: { max_bytes: 2048 * MiB } });
});

it("sends no snapshot settings for database methods", async () => {
  renderBackups({ ...policy, volume_snapshot: { max_bytes: 0 } }, snapshotWorker);
  fireEvent.click(await screen.findByRole("button", { name: "Edit policy" }));
  chooseMethod("MariaDB logical dump (online, InnoDB only)");
  expect(screen.queryByRole("switch", { name: "Use filesystem snapshots" })).not.toBeInTheDocument();
  fireEvent.change(screen.getByLabelText("Database Compose service"), { target: { value: "db" } });
  fireEvent.change(screen.getByLabelText("Database name"), { target: { value: "app" } });
  fireEvent.click(screen.getByRole("button", { name: "Save policy" }));
  expect(await savedPolicy()).toMatchObject({ volume_snapshot: null, database: { engine: "mariadb" } });
});

it("explains the upgrade on a worker without filesystem snapshot support", async () => {
  renderBackups(null, { ...upgraded, snapshots: false, nodeName: "old-worker" });
  fireEvent.click(await screen.findByRole("button", { name: "Configure backups" }));
  expect(screen.getByRole("switch", { name: "Use filesystem snapshots" })).toBeDisabled();
  expect(screen.getByText("Filesystem snapshots need an upgraded worker")).toBeInTheDocument();
  fireEvent.change(screen.getByLabelText("Encryption password"), { target: { value: "a-long-enough-password" } });
  fireEvent.click(screen.getByRole("button", { name: "Save policy" }));
  expect(await savedPolicy()).toMatchObject({ volume_snapshot: null });
});

it("checks volumes only on request and lists each mount's method or fallback reason", async () => {
  const preview = vi.fn(() => Promise.resolve({ available: false, reason: "one or more mounts cannot be snapshotted", mounts: [
    { mount: "app-data", filesystem: "btrfs", method: "btrfs", available: true },
    { mount: "/srv/uploads", filesystem: "ext4", method: "stopped", available: false, reason: "ext4 is not on a logical volume" },
  ] }));
  renderBackups({ ...policy, volume_snapshot: { max_bytes: 0 } }, snapshotWorker, [], preview);
  fireEvent.click(await screen.findByRole("button", { name: "Edit policy" }));
  expect(preview).not.toHaveBeenCalled();
  fireEvent.click(screen.getByRole("button", { name: "Check volumes" }));
  expect(await screen.findByText("Backups will use the stopped method")).toBeInTheDocument();
  expect(request).toHaveBeenCalledWith("GET", "/api/workloads/w1/backups/snapshot-preview");
  const list = screen.getByRole("list", { name: "Volumes checked" });
  expect(list).toHaveTextContent("app-data");
  expect(list).toHaveTextContent("ext4 is not on a logical volume");
  expect(list).toHaveTextContent("Not supported");

  fireEvent.change(screen.getByLabelText("LVM snapshot space (MiB)"), { target: { value: "512" } });
  expect(screen.queryByRole("list", { name: "Volumes checked" })).not.toBeInTheDocument();
  fireEvent.click(screen.getByRole("button", { name: "Check volumes" }));
  await waitFor(() => expect(request).toHaveBeenCalledWith("GET", `/api/workloads/w1/backups/snapshot-preview?max_bytes=${512 * MiB}`));
});

it("shows a host-level reason when every mount is supported but the host cannot clean up snapshots", async () => {
  const preview = vi.fn(() => Promise.resolve({ available: false, reason: "A working host systemd manager is required for crash-independent snapshot cleanup", mounts: [{ mount: "app-data", filesystem: "zfs", method: "zfs", available: true }] }));
  renderBackups({ ...policy, volume_snapshot: { max_bytes: 0 } }, snapshotWorker, [], preview);
  fireEvent.click(await screen.findByRole("button", { name: "Edit policy" }));
  fireEvent.click(screen.getByRole("button", { name: "Check volumes" }));
  expect(await screen.findByText("Backups will use the stopped method")).toBeInTheDocument();
  expect(screen.getByText(/host systemd manager is required/)).toBeInTheDocument();
  expect(screen.getByRole("list", { name: "Volumes checked" })).toHaveTextContent("Supported");
});

it("refuses an invalid size before checking and shows why volumes could not be checked", async () => {
  const preview = vi.fn(() => Promise.reject(new Error("verify an upgraded worker and wait for active operations before inspecting snapshots")));
  renderBackups({ ...policy, volume_snapshot: { max_bytes: 0 } }, snapshotWorker, [], preview);
  fireEvent.click(await screen.findByRole("button", { name: "Edit policy" }));
  fireEvent.change(screen.getByLabelText("LVM snapshot space (MiB)"), { target: { value: "abc" } });
  fireEvent.click(screen.getByRole("button", { name: "Check volumes" }));
  expect(screen.getByText("Enter a whole number from 64 to 65,536 MiB.")).toBeInTheDocument();
  expect(screen.getByLabelText("LVM snapshot space (MiB)")).toHaveFocus();
  expect(preview).not.toHaveBeenCalled();
  fireEvent.change(screen.getByLabelText("LVM snapshot space (MiB)"), { target: { value: "1024" } });
  fireEvent.click(screen.getByRole("button", { name: "Check volumes" }));
  expect(await screen.findByText("Could not check volumes")).toBeInTheDocument();
  expect(screen.getByText(/wait for active operations/)).toBeInTheDocument();
});

it("shows the actual volume method and fallback reason in backup history", async () => {
  renderBackups({ ...policy, volume_snapshot: { max_bytes: 0 } }, snapshotWorker, [
    { id: "snap-fs", created_at: "2026-11-21T10:00:00Z", repository: "local", volume_method: "btrfs+zfs" },
    { id: "snap-stopped", created_at: "2026-11-21T09:00:00Z", repository: "local", volume_method: "stopped", fallback_reason: "/srv/uploads: tmpfs is not supported" },
    { id: "snap-old", created_at: "2026-11-20T09:00:00Z", repository: "local" },
  ]);
  expect(await screen.findByText("Btrfs + ZFS filesystem snapshot")).toBeInTheDocument();
  expect(screen.getByText("Stopped containers")).toBeInTheDocument();
  expect(screen.getByText("Snapshots unavailable: /srv/uploads: tmpfs is not supported")).toBeInTheDocument();
  expect(screen.getAllByText(/filesystem snapshot$|^Stopped containers$/)).toHaveLength(2);
});

const managed = (engine: DatabaseResource["engine"], service: string): DatabaseResource => ({ engine, version: "1", template_revision: "r1", service, image: `${engine}:1`, port: 1, host: `${service}.internal` });

it("offers a managed database only its own online backup and never an in-place restore", async () => {
  const snapshot = { id: "snap-pg", created_at: "2026-11-21T10:00:00Z", repository: "local", database: { engine: "postgresql", service: "postgres", name: "app" } };
  renderBackups(null, upgraded, [snapshot], undefined, managed("postgresql", "postgres"));
  expect(await screen.findByText("Restoring a managed database")).toBeInTheDocument();
  expect(screen.getByRole("button", { name: "Restore as new resource" })).toBeInTheDocument();
  expect(screen.queryByRole("button", { name: "Restore" })).not.toBeInTheDocument();

  fireEvent.click(screen.getByRole("button", { name: "Configure backups" }));
  expect(screen.getByLabelText("Database Compose service")).toHaveValue("postgres");
  expect(screen.getByLabelText("Database Compose service")).toHaveAttribute("readonly");
  expect(screen.getByLabelText("Database name")).toHaveValue("app");
  fireEvent.click(screen.getByRole("combobox", { name: "Backup method" }));
  expect(screen.getByRole("option", { name: "PostgreSQL logical dump (online)" })).toBeInTheDocument();
  expect(screen.queryByRole("option", { name: "MariaDB logical dump (online, InnoDB only)" })).not.toBeInTheDocument();
  expect(screen.queryByRole("option", { name: "Redis RDB snapshot (online)" })).not.toBeInTheDocument();
});

it("keeps a managed standalone MongoDB on volume snapshots", async () => {
  renderBackups(null, upgraded, [], undefined, managed("mongodb", "mongo"));
  fireEvent.click(await screen.findByRole("button", { name: "Configure backups" }));
  expect(screen.getByText(/MongoDB uses consistent volume snapshots/)).toBeInTheDocument();
  fireEvent.click(screen.getByRole("combobox", { name: "Backup method" }));
  expect(screen.queryByRole("option", { name: "MongoDB replica set archive (online)" })).not.toBeInTheDocument();
});

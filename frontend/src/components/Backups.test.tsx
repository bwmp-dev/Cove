import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { request } from "../api/client";
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
type SavedPolicy = typeof policy & { database?: Database };
interface Snapshot { id: string; created_at: string; repository: string; database?: Database }

function renderBackups(saved: SavedPolicy | null, support: WorkerSupport, backups: Snapshot[] = []) {
  vi.mocked(request).mockImplementation((method: string, path: string) => {
    if (method === "PUT") return Promise.resolve({ ok: true });
    if (method === "POST" && path.endsWith("/restore-copy")) return Promise.resolve({ id: "op1", resource_id: "w2" });
    if (path.endsWith("/backup-policy")) return Promise.resolve({ backup: saved });
    if (path.endsWith("/backups")) return Promise.resolve({ backups });
    return Promise.resolve([]);
  });
  render(<QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}><Backups workloadId="w1" busy={false} canOperate canEdit support={support} /></QueryClientProvider>);
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

  chooseMethod("Consistent volume snapshot (stops containers)");
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

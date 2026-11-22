import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { request, uploadFile } from "../api/client";
import { Backups, type WorkerSupport } from "./Backups";
import { BackupTransferLimit } from "./BackupTransferLimit";

vi.mock("../api/client", () => ({ request: vi.fn(), uploadFile: vi.fn(), errorMessage: (error: Error) => error.message }));
vi.mock("../lib/team", () => ({ selectedTeam: () => "team-a" }));
vi.mock("@tanstack/react-router", () => ({ Link: ({ children }: { children: React.ReactNode }) => <span>{children}</span>, useBlocker: () => ({ status: "idle" }) }));
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

const MiB = 1024 * 1024;
const redis = { engine: "redis", service: "cache", name: "all" };
const policy = { repository: "local", keep_last: 5, interval_hours: 24, database: redis };
const snapshot = { id: "snap-1", created_at: "2026-11-21T10:00:00Z", repository: "local", database: redis };
const upgraded: WorkerSupport = { retention: true, instanceEngines: true, transfers: true, nodeName: "edge" };
const limits = { backup_bytes: 8, min_backup_bytes: MiB, max_backup_bytes: 64 * 1024 * MiB };
const imported = { backup_id: "snap-new", size: 6, sha256: "ab".repeat(32), verified: true };

function mockApi(backups: () => object[], saved: object | null = policy) {
  vi.mocked(request).mockImplementation((method: string, path: string) => {
    if (path.endsWith("/backup-policy")) return Promise.resolve({ backup: saved });
    if (path.endsWith("/backups")) return Promise.resolve({ backups: backups() });
    if (path.endsWith("/transfer-limits")) return Promise.resolve(limits);
    if (path === "/api/storage-locations") return Promise.resolve([{ id: "s3a", name: "Offsite", storage: { protocol: "s3", endpoint: "https://s3.example", path: "bucket" } }, { id: "sftp", name: "Box", storage: { protocol: "sftp", endpoint: "box", path: "/x" } }]);
    if (method === "POST" && path.endsWith("/import-s3")) return Promise.resolve(imported);
    return Promise.resolve({});
  });
}

function renderBackups(support: WorkerSupport = upgraded) {
  render(<QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}><Backups workloadId="w1" busy={false} canOperate canEdit support={support} /></QueryClientProvider>);
}

it("warns about plaintext scope and downloads through a plain same-origin link with the team context", async () => {
  mockApi(() => [snapshot]);
  renderBackups();
  fireEvent.click(await screen.findByRole("button", { name: "Download" }));
  const dialog = screen.getByRole("dialog", { name: "Download unencrypted backup?" });
  expect(within(dialog).getByText(/Redis complete instance dump/)).toBeInTheDocument();
  expect(within(dialog).getByText(/anyone with the file can read the data/)).toBeInTheDocument();
  expect(within(dialog).getByText("cove-backup-snap-1.rdb")).toBeInTheDocument();
  const link = within(dialog).getByRole("link", { name: "Download" });
  expect(link).toHaveAttribute("href", "/api/workloads/w1/backups/snap-1/download?team_id=team-a");
  expect(link).toHaveAttribute("target", "_blank");
  expect(link).toHaveAttribute("rel", "noopener");
});

it("disables downloads for snapshots that no longer match the policy and on workers without transfer support", async () => {
  mockApi(() => [{ ...snapshot, database: { ...redis, service: "old" } }]);
  renderBackups({ ...upgraded, transfers: false });
  expect(await screen.findByRole("button", { name: "Download" })).toBeDisabled();
  expect(screen.getByText("Downloads and imports need an upgraded worker")).toBeInTheDocument();
  expect(screen.getByRole("button", { name: "Import dump" })).toBeDisabled();
});

it("offers import only for logical database policies", async () => {
  mockApi(() => [], { repository: "local", keep_last: 5, interval_hours: 24 });
  renderBackups();
  expect(await screen.findByRole("button", { name: "Edit policy" })).toBeInTheDocument();
  expect(screen.queryByRole("button", { name: "Import dump" })).not.toBeInTheDocument();
});

it("refuses oversized and empty files before uploading", async () => {
  mockApi(() => []);
  renderBackups();
  fireEvent.click(await screen.findByRole("button", { name: "Import dump" }));
  const input = await screen.findByLabelText("Dump file");
  await waitFor(() => expect(input).toBeEnabled());
  fireEvent.change(input, { target: { files: [new File(["123456789"], "big.rdb")] } });
  expect(screen.getByText("This file is 9 B. The limit is 8 B.")).toBeInTheDocument();
  expect(screen.getByRole("button", { name: "Import and verify" })).toBeDisabled();
  fireEvent.change(input, { target: { files: [new File([], "empty.rdb")] } });
  expect(screen.getByText("This file is empty.")).toBeInTheDocument();
  expect(uploadFile).not.toHaveBeenCalled();
});

it("uploads the file, reports the verified backup, and restores only through the existing confirmation", async () => {
  let rows: object[] = [snapshot];
  mockApi(() => rows);
  let finish: (value: typeof imported) => void = () => undefined;
  vi.mocked(uploadFile).mockImplementation(() => new Promise((resolve) => { finish = resolve; }));
  renderBackups();
  fireEvent.click(await screen.findByRole("button", { name: "Import dump" }));
  const dialog = screen.getByRole("dialog", { name: "Import database dump" });
  expect(within(dialog).getByText("Importing does not change this resource")).toBeInTheDocument();
  expect(within(dialog).getByText("Accepted file: Redis RDB snapshot")).toBeInTheDocument();
  const input = within(dialog).getByLabelText("Dump file");
  await waitFor(() => expect(input).toBeEnabled());
  const file = new File(["REDIS0"], "dump.rdb");
  fireEvent.change(input, { target: { files: [file] } });
  fireEvent.click(within(dialog).getByRole("button", { name: "Import and verify" }));

  expect(await within(dialog).findByText("Uploading and verifying")).toBeInTheDocument();
  expect(within(dialog).queryByText(/%/)).not.toBeInTheDocument();
  expect(uploadFile).toHaveBeenCalledWith("/api/workloads/w1/backups/import", file, expect.any(AbortSignal));

  rows = [{ id: "snap-new", created_at: "2026-11-22T10:00:00Z", repository: "local", database: redis }, snapshot];
  finish(imported);
  expect(await within(dialog).findByText("Verified backup stored")).toBeInTheDocument();
  expect(within(dialog).getByText("ab".repeat(32))).toBeInTheDocument();
  fireEvent.click(await within(dialog).findByRole("button", { name: "Restore this backup…" }));
  expect(screen.getByText("Restore Redis complete instance?")).toBeInTheDocument();
  expect(request).not.toHaveBeenCalledWith("POST", "/api/workloads/w1/operations", expect.anything());
  fireEvent.click(screen.getByRole("button", { name: "Restore data" }));
  await waitFor(() => expect(request).toHaveBeenCalledWith("POST", "/api/workloads/w1/operations", { action: "restore", backup_id: "snap-new" }));
});

it("explains that cancelling does not roll back an import the worker may have finished", async () => {
  mockApi(() => []);
  vi.mocked(uploadFile).mockImplementation((_path, _file, signal) => new Promise((_resolve, reject) => {
    signal?.addEventListener("abort", () => reject(new DOMException("aborted", "AbortError")));
  }));
  renderBackups();
  fireEvent.click(await screen.findByRole("button", { name: "Import dump" }));
  const input = await screen.findByLabelText("Dump file");
  await waitFor(() => expect(input).toBeEnabled());
  fireEvent.change(input, { target: { files: [new File(["REDIS0"], "dump.rdb")] } });
  fireEvent.click(screen.getByRole("button", { name: "Import and verify" }));
  fireEvent.click(await screen.findByRole("button", { name: "Cancel import" }));
  expect(await screen.findByText("Import cancelled in this browser")).toBeInTheDocument();
  expect(screen.getByText(/Check the backup list before trying again/)).toBeInTheDocument();
  expect(screen.queryByText("Import failed")).not.toBeInTheDocument();
});

it("imports from a configured S3 location with a validated relative key", async () => {
  mockApi(() => []);
  renderBackups();
  fireEvent.click(await screen.findByRole("button", { name: "Import dump" }));
  fireEvent.click(screen.getByRole("radio", { name: /S3 storage location/ }));
  fireEvent.click(await screen.findByRole("combobox", { name: "Storage location" }));
  expect(screen.queryByRole("option", { name: "Box" })).not.toBeInTheDocument();
  fireEvent.click(screen.getByRole("option", { name: "Offsite" }));
  const key = screen.getByLabelText("Object key");
  fireEvent.change(key, { target: { value: "../dump.rdb" } });
  fireEvent.click(screen.getByRole("button", { name: "Import and verify" }));
  expect(await screen.findByText("Object keys cannot contain empty, . or .. segments.")).toBeInTheDocument();
  expect(key).toHaveFocus();
  fireEvent.change(key, { target: { value: "dumps/dump.rdb" } });
  fireEvent.click(screen.getByRole("button", { name: "Import and verify" }));
  await waitFor(() => expect(request).toHaveBeenCalledWith("POST", "/api/workloads/w1/backups/import-s3", { storage_id: "s3a", object_key: "dumps/dump.rdb" }, expect.any(AbortSignal)));
  expect(await screen.findByText("Verified backup stored")).toBeInTheDocument();
});

it("saves the transfer limit as an exact byte count and keeps an untouched saved value", async () => {
  const saved = { backup_bytes: 1024 * MiB, min_backup_bytes: MiB, max_backup_bytes: 64 * 1024 * MiB };
  vi.mocked(request).mockImplementation((method: string, _path: string, body?: unknown) => Promise.resolve(method === "PUT" ? { ...saved, ...(body as object) } : saved));
  render(<QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}><BackupTransferLimit /></QueryClientProvider>);
  const field = await screen.findByLabelText("Largest backup (MiB)");
  expect(field).toHaveValue(1024);
  expect(screen.getByText(/At most four transfers run at once/)).toBeInTheDocument();
  expect(screen.getByText(/Raising the limit does not add storage/)).toBeInTheDocument();

  fireEvent.change(field, { target: { value: "70000" } });
  fireEvent.click(screen.getByRole("button", { name: "Save changes" }));
  expect(await screen.findByText("Enter a whole number from 1 to 65,536 MiB.")).toBeInTheDocument();
  expect(field).toHaveFocus();
  expect(request).not.toHaveBeenCalledWith("PUT", expect.anything(), expect.anything());

  fireEvent.change(field, { target: { value: "2048" } });
  fireEvent.click(screen.getByRole("button", { name: "Save changes" }));
  await waitFor(() => expect(request).toHaveBeenCalledWith("PUT", "/api/settings/transfer-limits", { backup_bytes: 2048 * MiB }));
  expect(await screen.findByText("Saved. New transfers use this limit.")).toBeInTheDocument();
});

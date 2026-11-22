import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, expect, it, vi } from "vitest";
import { request } from "../api/client";
import type { RetentionSettings, StorageUsage as Usage } from "../api/telemetry";
import { OperationRetentionPasses, StorageUsage, TelemetryRetention } from "./TelemetryRetention";

vi.mock("../api/client", () => ({ request: vi.fn(), errorMessage: (error: Error) => error.message, isStatus: () => false }));
vi.mock("@tanstack/react-router", () => ({ useBlocker: () => ({ status: "idle" }) }));
afterEach(() => vi.resetAllMocks());
HTMLDialogElement.prototype.showModal = function (this: HTMLDialogElement) {
  this.open = true;
};
HTMLDialogElement.prototype.close = function (this: HTMLDialogElement) {
  this.open = false;
};

const MiB = 1024 * 1024;
const saved: RetentionSettings = { metric_rows: 200000, metric_bytes: 256 * MiB, log_rows: 200000, log_bytes: 128 * MiB, log_days: 7, audit_rows: 100000, audit_days: 90, raw_days: 1, hour_days: 30, day_days: 365, operation_rows: 100000, operation_days: 90 };

function wrap(children: React.ReactNode) {
  return render(<QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}>{children}</QueryClientProvider>);
}

it("confirms irreversible pruning and saves only the changed limits", async () => {
  vi.mocked(request).mockImplementation((method: string) => Promise.resolve(method === "PUT" ? { ...saved, log_days: 3 } : saved));
  wrap(<TelemetryRetention />);
  const days = await screen.findByLabelText("Keep lines for");
  fireEvent.change(days, { target: { value: "3" } });
  fireEvent.click(screen.getByRole("button", { name: "Save changes" }));

  const dialog = await screen.findByRole("dialog", { name: "Save and delete data beyond the new limits?" });
  expect(within(dialog).getByText(/older than or beyond the new limits are permanently deleted/)).toBeInTheDocument();
  expect(within(dialog).getByText("Log history, keep lines for: 7 days to 3 days")).toBeInTheDocument();
  expect(within(dialog).getByText(/Eligible operation records and their worker journals/)).toBeInTheDocument();
  expect(request).not.toHaveBeenCalledWith("PUT", expect.anything(), expect.anything());

  fireEvent.click(within(dialog).getByRole("button", { name: "Save and delete" }));
  await waitFor(() => expect(request).toHaveBeenCalledWith("PUT", "/api/settings/retention", { log_days: 3 }));
  expect(await screen.findByText(/Saved. Telemetry beyond the new limits was removed/)).toBeInTheDocument();
});

it("saves operation history limits within the allowed range", async () => {
  vi.mocked(request).mockImplementation((method: string) => Promise.resolve(method === "PUT" ? { ...saved, operation_days: 30 } : saved));
  wrap(<TelemetryRetention />);
  fireEvent.change(await screen.findByLabelText("Keep records for"), { target: { value: "3651" } });
  fireEvent.click(screen.getByRole("button", { name: "Save changes" }));
  expect(await screen.findByText("Enter a whole number from 1 to 3,650.")).toBeInTheDocument();
  fireEvent.change(screen.getByLabelText("Keep records for"), { target: { value: "30" } });
  fireEvent.click(screen.getByRole("button", { name: "Save changes" }));
  const dialog = await screen.findByRole("dialog");
  expect(within(dialog).getByText("Operation history, keep records for: 90 days to 30 days")).toBeInTheDocument();
  fireEvent.click(within(dialog).getByRole("button", { name: "Save and delete" }));
  await waitFor(() => expect(request).toHaveBeenCalledWith("PUT", "/api/settings/retention", { operation_days: 30 }));
});

it("shows last-pass operation retention status and runs a confirmed pass", async () => {
  const before = [
    { node_id: "n1", name: "edge-1", checked_at: null, error: "", pruned: 0, protected: 0 },
    { node_id: "n2", name: "old-worker", checked_at: "2026-11-22T10:00:00Z", error: "Upgrade and verify the worker to prune operation journals.", pruned: 0, protected: 0 },
  ];
  const after = [{ ...before[0]!, checked_at: "2026-11-22T12:00:00Z", pruned: 40, protected: 2 }, before[1]!];
  vi.mocked(request).mockImplementation((method: string) => Promise.resolve(method === "POST" ? after : before));
  wrap(<OperationRetentionPasses />);
  const table = await screen.findByRole("table", { name: "Last operation retention pass by node" });
  expect(within(table).getByText("Not checked yet")).toBeInTheDocument();
  expect(within(table).getByText("Deferred")).toBeInTheDocument();
  expect(within(table).getByText(/Upgrade and verify the worker/)).toBeInTheDocument();
  fireEvent.click(screen.getByRole("button", { name: "Run retention pass" }));
  const dialog = await screen.findByRole("dialog", { name: "Run an operation retention pass now?" });
  expect(request).not.toHaveBeenCalledWith("POST", expect.anything(), expect.anything());
  fireEvent.click(within(dialog).getByRole("button", { name: "Run pass and delete" }));
  await waitFor(() => expect(request).toHaveBeenCalledWith("POST", "/api/settings/operation-retention/run", {}));
  expect(await screen.findByText("Retention pass finished")).toBeInTheDocument();
  expect(await within(table).findByText("40")).toBeInTheDocument();
});

it("shows field errors instead of asking to save invalid limits", async () => {
  vi.mocked(request).mockResolvedValue(saved);
  wrap(<TelemetryRetention />);
  fireEvent.change(await screen.findByLabelText("Hourly samples"), { target: { value: "0" } });
  fireEvent.click(screen.getByRole("button", { name: "Save changes" }));
  expect(await screen.findByText("Enter a whole number from 1 to 90.")).toBeInTheDocument();
  expect(screen.getByLabelText("Hourly samples")).toHaveAttribute("aria-invalid", "true");
  expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
});

it("labels accounted telemetry size separately from PostgreSQL size and shows eligible operation limits", async () => {
  const usage: Usage = {
    metrics: { rows: 1200, bytes: 3 * MiB, basis: "accounted payload and row allowance" },
    logs: { rows: 50, bytes: 2048, basis: "accounted payload and row allowance" },
    audit: { rows: 10, bytes: 16384, basis: "PostgreSQL table and indexes" },
    operations: { rows: 4, bytes: 8192, basis: "PostgreSQL table and indexes" },
  };
  vi.mocked(request).mockImplementation((_method: string, path: string) => Promise.resolve(path.endsWith("storage-usage") ? usage : saved));
  wrap(<StorageUsage />);
  const table = await screen.findByRole("table", { name: "Stored data by category" });
  const operations = within(table).getByText("Operations and worker journals").closest("tr");
  expect(operations && within(operations).getAllByText(/100,000 eligible rows, 90 days/).length).toBeGreaterThan(0);
  expect(screen.queryByText(/Not pruned/)).not.toBeInTheDocument();
  expect(within(table).getAllByText("Accounted payload and row allowance").length).toBeGreaterThan(0);
  expect(screen.getByText(/not database file size/)).toBeInTheDocument();
  expect(await screen.findByRole("meter", { name: "Metric history" })).toHaveAttribute("aria-valuetext", "3.00 MiB of 256 MiB, 1%");
});

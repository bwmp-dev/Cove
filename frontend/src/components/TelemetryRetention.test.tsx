import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, expect, it, vi } from "vitest";
import { request } from "../api/client";
import type { RetentionSettings, StorageUsage as Usage } from "../api/telemetry";
import { StorageUsage, TelemetryRetention } from "./TelemetryRetention";

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
const saved: RetentionSettings = { metric_rows: 200000, metric_bytes: 256 * MiB, log_rows: 200000, log_bytes: 128 * MiB, log_days: 7, audit_rows: 100000, audit_days: 90, raw_days: 1, hour_days: 30, day_days: 365 };

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
  expect(within(dialog).getByText(/permanently deleted/)).toBeInTheDocument();
  expect(within(dialog).getByText("Log history, keep lines for: 7 days to 3 days")).toBeInTheDocument();
  expect(request).not.toHaveBeenCalledWith("PUT", expect.anything(), expect.anything());

  fireEvent.click(within(dialog).getByRole("button", { name: "Save and delete" }));
  await waitFor(() => expect(request).toHaveBeenCalledWith("PUT", "/api/settings/retention", { log_days: 3 }));
  expect(await screen.findByText("Saved. Data beyond the new limits was removed.")).toBeInTheDocument();
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

it("labels accounted telemetry size separately from PostgreSQL size and keeps journals unpruned", async () => {
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
  expect(operations && within(operations).getAllByText(/Not pruned/).length).toBeGreaterThan(0);
  expect(within(table).getAllByText("Accounted payload and row allowance").length).toBeGreaterThan(0);
  expect(screen.getByText(/not database file size/)).toBeInTheDocument();
  expect(await screen.findByRole("meter", { name: "Metric history" })).toHaveAttribute("aria-valuetext", "3.00 MiB of 256 MiB, 1%");
});

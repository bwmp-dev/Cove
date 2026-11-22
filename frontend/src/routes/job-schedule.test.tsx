import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, expect, it, vi } from "vitest";
import { request } from "../api/client";
import type { Workload } from "../api/types";
import { JobSchedule } from "./job-schedule";

const resource: Partial<Workload> = { id: "job-1", kind: "job", manually_stopped: false, permissions: ["read", "edit"] };

vi.mock("../api/client", async importOriginal => ({ ...await importOriginal<typeof import("../api/client")>(), request: vi.fn() }));
vi.mock("./workload-detail", () => ({ useResource: () => resource }));
afterEach(() => vi.clearAllMocks());

function renderSchedule() {
  render(<QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}><JobSchedule /></QueryClientProvider>);
}

it("shows the saved time zone and saves it with the cron expression", async () => {
  vi.mocked(request).mockImplementation(method => Promise.resolve(method === "GET"
    ? { cron: "0 9 * * *", timezone: "America/Los_Angeles", next_run_at: "2026-11-23T17:00:00Z" }
    : { cron: "0 9 * * 1", timezone: "America/New_York", next_run_at: "2026-11-23T14:00:00Z" }));
  renderSchedule();
  const timezone = await screen.findByLabelText("Time zone");
  expect(timezone).toHaveValue("America/Los_Angeles");
  expect(screen.getByText(/Nov 23, 2026, 9:00:00 AM PST/)).toBeInTheDocument();

  fireEvent.change(screen.getByLabelText("Cron expression"), { target: { value: " 0 9 * * 1 " } });
  fireEvent.change(timezone, { target: { value: "America/New_York" } });
  fireEvent.click(screen.getByRole("button", { name: "Save schedule" }));
  await waitFor(() => expect(request).toHaveBeenCalledWith("PUT", "/api/workloads/job-1/schedule", { cron: "0 9 * * 1", timezone: "America/New_York" }));
  expect(await screen.findByText(/Nov 23, 2026, 9:00:00 AM EST/)).toBeInTheDocument();
});

it("keeps the saved time zone when the schedule is disabled", async () => {
  vi.mocked(request).mockImplementation(method => Promise.resolve(method === "GET"
    ? { cron: "*/15 * * * *", timezone: "Europe/Berlin", next_run_at: "2026-11-23T08:15:00Z" }
    : { cron: "", timezone: "Europe/Berlin", next_run_at: null }));
  renderSchedule();
  fireEvent.change(await screen.findByLabelText("Cron expression"), { target: { value: "" } });
  fireEvent.click(screen.getByRole("button", { name: "Save schedule" }));
  await waitFor(() => expect(request).toHaveBeenCalledWith("PUT", "/api/workloads/job-1/schedule", { cron: "", timezone: "Europe/Berlin" }));
  expect(await screen.findByText("Scheduling is disabled.")).toBeInTheDocument();
  expect(screen.getByLabelText("Time zone")).toHaveValue("Europe/Berlin");
});

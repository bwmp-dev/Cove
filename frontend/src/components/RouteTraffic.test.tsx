import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { request } from "../api/client";
import { RouteTraffic } from "./RouteTraffic";

vi.mock("../api/client", async (importActual) => ({ ...(await importActual<typeof import("../api/client")>()), request: vi.fn() }));

beforeEach(() => {
  vi.useFakeTimers({ toFake: ["Date"], now: Date.parse("2026-11-22T12:00:00Z") });
  vi.stubGlobal("ResizeObserver", class { observe = vi.fn(); disconnect = vi.fn(); });
});
afterEach(() => { vi.useRealTimers(); vi.resetAllMocks(); vi.unstubAllGlobals(); });

const base = { routes: [], entries: [], collected_at: null, available: false, limited: false, collection_error: "", hours: 24, retention_days: 7, notice: "Statistics cover retained observations only." };

function renderTraffic(data: unknown) {
  vi.mocked(request).mockResolvedValue(data);
  render(<QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}><RouteTraffic workloadId="w1" /></QueryClientProvider>);
}

it("does not present uncollected traffic as zero requests", async () => {
  renderTraffic(base);
  expect(await screen.findByText("Traffic has not been collected")).toBeInTheDocument();
  expect(screen.getByText("No traffic data.")).toBeInTheDocument();
  expect(screen.queryByText(/No requests recorded/)).not.toBeInTheDocument();
});

it("shows per-domain counts, recent requests, staleness and the selected range", async () => {
  renderTraffic({
    ...base,
    available: true,
    limited: true,
    collected_at: "2026-11-22T11:50:00Z",
    routes: [{ domain: "shop.example.test", requests: 120, status_1xx: 0, status_2xx: 100, status_3xx: 5, status_4xx: 10, status_5xx: 5, bytes: 2048, p95_ms: 230 }],
    entries: [{ timestamp: "2026-11-22T11:49:00Z", domain: "shop.example.test", status: 502, bytes: 120, duration_ms: 1500, client_ip: "198.51.100.7" }],
  });
  const domains = await screen.findByRole("table", { name: /Requests per domain/ });
  const row = within(domains).getByRole("row", { name: /shop\.example\.test/ });
  expect(within(row).getByText("120")).toBeInTheDocument();
  expect(within(row).getByText("230 ms")).toBeInTheDocument();
  const recent = screen.getByRole("table", { name: "Most recent requests" });
  expect(within(recent).getByText("502")).toHaveClass("text-bad");
  expect(within(recent).getAllByText("198.51.100.7").length).toBeGreaterThan(0);
  expect(screen.getByText("Traffic data is stale")).toBeInTheDocument();
  expect(screen.getByText("Some traffic is not shown")).toBeInTheDocument();
  expect(request).toHaveBeenCalledWith("GET", "/api/workloads/w1/traffic?range=day", undefined, expect.anything());
  fireEvent.click(screen.getByRole("combobox", { name: "Range" }));
  fireEvent.click(screen.getByRole("option", { name: "Last 7 days" }));
  await waitFor(() => expect(request).toHaveBeenCalledWith("GET", "/api/workloads/w1/traffic?range=week", undefined, expect.anything()));
});

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen } from "@testing-library/react";
import { afterEach, expect, it, vi } from "vitest";
import { request } from "../api/client";
import { RightSizing } from "./RightSizing";

vi.mock("../api/client", async (importActual) => ({ ...(await importActual<typeof import("../api/client")>()), request: vi.fn() }));

function renderSizing() {
  render(
    <QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}>
      <RightSizing workloadId="w1" limits={{ web: { cpus: 2, memory_mb: 1024 } }} />
    </QueryClientProvider>,
  );
}

const base = { first_sample: "2026-11-15T12:00:00Z", last_sample: new Date().toISOString(), cpu_p95_percent: 40, memory_p95_bytes: 200 * 1024 * 1024 };

afterEach(() => vi.resetAllMocks());

it("shows read-only suggestions next to configured limits and never shows warming services as zero", async () => {
  vi.mocked(request).mockResolvedValueOnce({
    window: "7 days",
    sampling: "Latest sample per hour and container, grouped by service; no automatic limit changes.",
    services: [
      { ...base, service: "web", samples: 40, hours: 24, suggested_cpus: 0.5, suggested_memory_mb: 256, notice: "25% headroom above observed p95 hourly samples; review burst usage before changing limits." },
      { ...base, service: "worker", samples: 30, hours: 12, suggested_cpus: null, suggested_memory_mb: null, notice: "At least 20 distinct hours of samples are needed." },
    ],
  });
  renderSizing();
  expect(await screen.findByText("0.5 CPU, 256 MiB")).toBeInTheDocument();
  expect(screen.getByText("Configured: 2 CPU, 1,024 MiB")).toBeInTheDocument();
  expect(screen.getByText("Configured: CPU from Compose, memory from Compose")).toBeInTheDocument();
  expect(screen.getByText("No suggestion")).toBeInTheDocument();
  expect(screen.getByText("At least 20 distinct hours of samples are needed.")).toBeInTheDocument();
  expect(screen.queryByRole("button")).not.toBeInTheDocument();
});

it("explains empty history from workers without service labels", async () => {
  vi.mocked(request).mockResolvedValueOnce({ window: "7 days", sampling: "", services: [] });
  renderSizing();
  expect(await screen.findByText(/No per-service usage history yet/)).toBeInTheDocument();
});

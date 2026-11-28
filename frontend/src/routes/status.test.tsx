import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, within } from "@testing-library/react";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { PublicStatusPage } from "./status";

vi.mock("@tanstack/react-router", () => ({ getRouteApi: () => ({ useParams: () => ({ slug: "a".repeat(64) }) }) }));

const fetchMock = vi.fn<typeof fetch>();
beforeEach(() => vi.stubGlobal("fetch", fetchMock));
afterEach(() => { vi.unstubAllGlobals(); fetchMock.mockReset(); });

function renderPage() {
  render(<QueryClientProvider client={new QueryClient()}><PublicStatusPage /></QueryClientProvider>);
}

it("shows unknown readings and observed ratios without signing in", async () => {
  fetchMock.mockResolvedValue(new Response(JSON.stringify({
    title: "Shop status",
    source: "Periodic HTTPS checks from the panel; missing or stale readings are unknown.",
    checks: [
      { label: "API", up: true, checked_at: "2026-11-22T11:59:00Z", latency_ms: 42, observed_samples_24h: 1440, observed_up_ratio_24h: 0.99930555 },
      { label: "Website", up: null, checked_at: "2026-11-21T08:00:00Z", latency_ms: 80, observed_samples_24h: 0, observed_up_ratio_24h: null },
    ],
  }), { status: 200, headers: { "content-type": "application/json" } }));
  renderPage();
  expect(await screen.findByRole("heading", { name: "Shop status" })).toBeInTheDocument();
  expect(fetchMock).toHaveBeenCalledWith(`/api/status/${"a".repeat(64)}`, expect.objectContaining({ credentials: "omit" }));
  expect(new Headers(fetchMock.mock.calls[0]?.[1]?.headers).get("x-cove-team")).toBeNull();
  expect(screen.getByText(/not continuous availability/)).toBeInTheDocument();
  expect(screen.getByText("Some readings are unknown")).toBeInTheDocument();
  const api = screen.getByRole("row", { name: /API/ });
  expect(within(api).getByText("Up")).toBeInTheDocument();
  expect(within(api).getByText("99.93%")).toBeInTheDocument();
  expect(within(api).getByText("of 1440 checks")).toBeInTheDocument();
  const website = screen.getByRole("row", { name: /Website/ });
  expect(within(website).getByText("Unknown", { selector: "span" })).toBeInTheDocument();
  expect(within(website).getByText("No samples")).toBeInTheDocument();
  expect(within(website).queryByText("80 ms")).not.toBeInTheDocument();
});

it("reports a missing or unpublished page", async () => {
  fetchMock.mockResolvedValue(new Response("not found", { status: 404, headers: { "content-type": "text/plain" } }));
  renderPage();
  expect(await screen.findByText("Status page not found")).toBeInTheDocument();
});

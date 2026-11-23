import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { request, requestText } from "../api/client";
import type { LogHistory as History } from "../api/telemetry";
import { LogHistory } from "./LogHistory";

vi.mock("../api/client", () => ({
  request: vi.fn(),
  requestText: vi.fn(),
  errorMessage: (error: Error) => error.message,
  isStatus: () => false,
}));

const page = (overrides: Partial<History>): History => ({
  entries: [],
  collected_at: "2026-11-22T12:00:00Z",
  limited: false,
  collection_error: "",
  limit: 500,
  next_cursor: null,
  ...overrides,
});

const newest = page({
  entries: [
    { id: "2", container_id: "c2", container: "worker-1", timestamp: "2026-11-22T11:59:00Z", message: "second line", clipped: true },
    { id: "1", container_id: "c1", container: "web-1", timestamp: "2026-11-22T11:58:00Z", message: "first line", clipped: false },
  ],
  next_cursor: { before: "2026-11-22T11:58:00Z", before_id: "1" },
  limited: true,
  collection_error: "worker unreachable",
});

const older = page({ entries: [{ id: "0", container_id: "c1", container: "web-1", timestamp: "2026-11-22T11:00:00Z", message: "older line", clipped: false }] });

function renderHistory() {
  render(
    <QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}>
      <LogHistory workloadId="res 1" />
    </QueryClientProvider>,
  );
}

const createObjectURL = vi.fn(() => "blob:logs");
beforeEach(() => {
  URL.createObjectURL = createObjectURL;
  URL.revokeObjectURL = vi.fn();
});
afterEach(() => vi.resetAllMocks());

it("shows stored lines oldest first with collection status, then pages to older lines", async () => {
  vi.mocked(request).mockResolvedValueOnce(newest).mockResolvedValueOnce(older);
  renderHistory();
  const lines = await screen.findAllByText(/line$/);
  expect(lines.map((line) => line.textContent)).toEqual(["first line", "second line"]);
  expect(screen.getByText("Line clipped at 8 KiB")).toBeInTheDocument();
  expect(screen.getByText("worker unreachable")).toBeInTheDocument();
  expect(screen.getByText("Some lines may be missing")).toBeInTheDocument();
  expect(request).toHaveBeenCalledWith("GET", "/api/workloads/res%201/log-history", undefined, expect.anything());

  fireEvent.click(screen.getByRole("button", { name: "Older lines" }));
  expect(await screen.findByText("older line")).toBeInTheDocument();
  expect(request).toHaveBeenLastCalledWith("GET", "/api/workloads/res%201/log-history?before=2026-11-22T11%3A58%3A00Z&before_id=1", undefined, expect.anything());
  expect(screen.getByRole("button", { name: "Newer lines" })).toBeEnabled();
});

it("searches from the first page and downloads the page on screen with the same filter", async () => {
  vi.mocked(request).mockResolvedValueOnce(newest).mockResolvedValueOnce(page({ entries: [newest.entries[1]!] }));
  vi.mocked(requestText).mockResolvedValueOnce("2026-11-22T11:58:00Z [web-1] first line\n");
  renderHistory();
  await screen.findByText("second line");
  fireEvent.change(screen.getByLabelText("Search log text"), { target: { value: " First " } });
  fireEvent.click(screen.getByRole("button", { name: "Search" }));
  await waitFor(() => expect(screen.queryByText("second line")).not.toBeInTheDocument());
  expect(request).toHaveBeenLastCalledWith("GET", "/api/workloads/res%201/log-history?q=First", undefined, expect.anything());

  fireEvent.click(screen.getByRole("button", { name: "Download this page" }));
  await waitFor(() => expect(createObjectURL).toHaveBeenCalled());
  expect(requestText).toHaveBeenCalledWith("/api/workloads/res%201/log-history?q=First&download=1");
});

it("explains an empty history before the first collection and blocks oversized searches", async () => {
  vi.mocked(request).mockResolvedValueOnce(page({ collected_at: null }));
  renderHistory();
  expect(await screen.findByText("Not collected yet")).toBeInTheDocument();
  expect(screen.getByRole("button", { name: "Download this page" })).toBeDisabled();
  fireEvent.change(screen.getByLabelText("Search log text"), { target: { value: "x".repeat(201) } });
  expect(screen.getByText("Search for at most 200 bytes of text.")).toBeInTheDocument();
  expect(screen.getByRole("button", { name: "Search" })).toBeDisabled();
});

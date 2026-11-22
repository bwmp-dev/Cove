import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { ApiError, request } from "../api/client";
import { LiveLogs } from "./LiveLogs";

vi.mock("../api/client", async (importActual) => ({ ...(await importActual<typeof import("../api/client")>()), request: vi.fn() }));

function renderTail() {
  render(
    <QueryClientProvider client={new QueryClient()}>
      <LiveLogs workloadId="res 1" />
    </QueryClientProvider>,
  );
}

const lines = () => within(screen.getByRole("region", { name: "Live log lines" })).queryAllByText(/^(boot|ready|query|again)/).map((line) => line.textContent);

beforeEach(() => {
  vi.useFakeTimers({ shouldAdvanceTime: true });
  vi.stubGlobal("ResizeObserver", class { observe = vi.fn(); disconnect = vi.fn(); });
});
afterEach(() => {
  vi.useRealTimers();
  vi.unstubAllGlobals();
  vi.resetAllMocks();
});

it("merges container output in timestamp order, drops overlapping lines and reads on from the cursor", async () => {
  vi.mocked(request)
    .mockResolvedValueOnce({
      entries: [
        { id: "1", container: "web-1", timestamp: "2026-11-22T12:00:01Z", message: "boot", clipped: false },
        { id: "2", container: "db-1", timestamp: "2026-11-22T12:00:02Z", message: "ready", clipped: true },
      ],
      sampled_at: "2026-11-22T12:00:03Z",
      limited: false,
    })
    .mockResolvedValueOnce({
      entries: [
        { id: "2", container: "db-1", timestamp: "2026-11-22T12:00:02Z", message: "ready", clipped: true },
        { id: "3", container: "web-1", timestamp: "2026-11-22T12:00:02.5Z", message: "query", clipped: false },
      ],
      sampled_at: "2026-11-22T12:00:05Z",
      limited: true,
    });
  renderTail();
  await screen.findByText("boot");
  expect(request).toHaveBeenCalledWith("GET", "/api/workloads/res%201/logs/live", undefined, expect.anything());
  expect(screen.getByText("Line clipped at 8 KiB")).toBeInTheDocument();

  await act(() => vi.advanceTimersByTimeAsync(2_100));
  await screen.findByText("query");
  expect(lines()).toEqual(["boot", "ready", "query"]);
  expect(request).toHaveBeenLastCalledWith("GET", `/api/workloads/res%201/logs/live?since=${encodeURIComponent("2026-11-22T12:00:02.000Z")}`, undefined, expect.anything());
  expect(screen.getByText("Some lines may be missing")).toBeInTheDocument();

  fireEvent.click(screen.getByRole("combobox", { name: "Container" }));
  fireEvent.click(screen.getByRole("option", { name: "web-1" }));
  await waitFor(() => expect(lines()).toEqual(["boot", "query"]));
});

it("stops reading while paused and clears lines on request", async () => {
  vi.mocked(request).mockResolvedValue({ entries: [{ id: "1", container: "web-1", timestamp: "2026-11-22T12:00:01Z", message: "boot", clipped: false }], sampled_at: "2026-11-22T12:00:03Z", limited: false });
  renderTail();
  await screen.findByText("boot");
  fireEvent.click(screen.getByRole("button", { name: "Pause" }));
  const calls = vi.mocked(request).mock.calls.length;
  await act(() => vi.advanceTimersByTimeAsync(6_000));
  expect(request).toHaveBeenCalledTimes(calls);
  fireEvent.click(screen.getByRole("button", { name: "Clear" }));
  expect(await screen.findByText(/No new output/)).toBeInTheDocument();
  expect(screen.queryByText("boot")).not.toBeInTheDocument();
});

it("falls back to the plain-text tail for workers without structured logs", async () => {
  vi.mocked(request).mockImplementation((_method, path) =>
    path.endsWith("/logs/live") ? Promise.reject(new ApiError(409, "upgrade and verify a ready worker for merged live logs")) : Promise.resolve({ logs: "legacy output" }),
  );
  renderTail();
  expect(await screen.findByText("Merged live tail needs a newer worker")).toBeInTheDocument();
  expect(await screen.findByText("legacy output")).toBeInTheDocument();
});

it("keeps shown lines when the worker becomes unavailable", async () => {
  vi.mocked(request)
    .mockResolvedValueOnce({ entries: [{ id: "1", container: "web-1", timestamp: "2026-11-22T12:00:01Z", message: "boot", clipped: false }], sampled_at: "2026-11-22T12:00:03Z", limited: false })
    .mockRejectedValue(new ApiError(502, "live logs unavailable; worker may be busy or unreachable"));
  renderTail();
  await screen.findByText("boot");
  await act(() => vi.advanceTimersByTimeAsync(2_100));
  expect(await screen.findByText("Worker unavailable")).toBeInTheDocument();
  expect(screen.getByText("boot")).toBeInTheDocument();
});

it("uses the plain-text tail without trying the merged endpoint when the node's worker lacks structured logs", async () => {
  vi.mocked(request).mockResolvedValue({ logs: "legacy output" });
  render(
    <QueryClientProvider client={new QueryClient()}>
      <LiveLogs workloadId="res 1" supported={false} />
    </QueryClientProvider>,
  );
  expect(await screen.findByText("legacy output")).toBeInTheDocument();
  expect(screen.getByText(/does not report structured logs/)).toBeInTheDocument();
  expect(request).not.toHaveBeenCalledWith("GET", expect.stringContaining("/logs/live"), undefined, expect.anything());
});

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, expect, it, vi } from "vitest";
import { request } from "../api/client";
import type { Node, Workload } from "../api/types";
import { AlertObservations } from "./AlertObservations";

vi.mock("../api/client", () => ({ request: vi.fn(), errorMessage: (error: Error) => error.message }));
afterEach(() => vi.clearAllMocks());

const workload = { id: "w1", name: "api", routes: [{ domain: "app.example.test", service: "web", port: 80 }, { domain: "missing.example.test", service: "web", port: 80 }] } as Workload;
const node = { id: "n1", name: "edge-1", capabilities: { alert_observations: true } } as Node;
const observations = {
  sampled_at: "2026-11-22T12:00:00Z",
  containers: [
    { id: "0123456789abcdef", restart_count: 4, oom_killed: true, exit_code: 137, exited: true, expected_completed: false },
    { id: "fedcba9876543210", restart_count: 0, oom_killed: false, exit_code: 0, exited: true, expected_completed: true },
  ],
  certificates: [{ domain: "app.example.test", expires_at: "2026-12-02T12:00:00Z" }],
  events_available: true,
  restart_events: 3,
  oom_events: 1,
  crash_events: 2,
  events: [],
};

function renderObservations(target: Node | undefined = node) {
  render(<QueryClientProvider client={new QueryClient()}><AlertObservations workload={workload} node={target} /></QueryClientProvider>);
}

it("reads observations only when asked and shows raw counters, state and unknown expiry", async () => {
  vi.mocked(request).mockResolvedValue(observations);
  renderObservations();
  expect(screen.getByText(/Not inspected yet/)).toBeInTheDocument();
  expect(request).not.toHaveBeenCalled();
  fireEvent.click(screen.getByRole("button", { name: "Inspect" }));
  expect(await screen.findByText("Automatic restarts")).toBeInTheDocument();
  expect(request).toHaveBeenCalledWith("GET", "/api/workloads/w1/observations", undefined, expect.anything());
  expect(screen.getByText("3")).toBeInTheDocument();
  const containers = screen.getByRole("list", { name: "Inspected containers" });
  expect(within(containers).getByText("0123456789ab")).toBeInTheDocument();
  expect(within(containers).getByText("Exited with code 137")).toBeInTheDocument();
  expect(within(containers).getByText(/excluded from alerts/)).toBeInTheDocument();
  const certificates = screen.getByRole("list", { name: "Inspected certificates" });
  expect(within(certificates).getByText("10 days remaining when inspected")).toBeInTheDocument();
  expect(within(within(certificates).getByText("missing.example.test").closest("li") as HTMLElement).getByText(/Unknown/)).toBeInTheDocument();
  fireEvent.click(screen.getByRole("button", { name: "Inspect again" }));
  await vi.waitFor(() => expect(request).toHaveBeenCalledTimes(2));
});

it("marks event counts unknown when Docker events were unavailable", async () => {
  vi.mocked(request).mockResolvedValue({ ...observations, events_available: false, containers: [], certificates: [] });
  renderObservations();
  fireEvent.click(screen.getByRole("button", { name: "Inspect" }));
  expect(await screen.findByText("Event counts unknown")).toBeInTheDocument();
  expect(screen.queryByText("Automatic restarts")).not.toBeInTheDocument();
  expect(screen.getByText(/No containers were found/)).toBeInTheDocument();
});

it("shows worker errors with a retry", async () => {
  vi.mocked(request).mockRejectedValue(new Error("upgrade and verify worker for resource observations"));
  renderObservations();
  fireEvent.click(screen.getByRole("button", { name: "Inspect" }));
  expect(await screen.findByText("upgrade and verify worker for resource observations")).toBeInTheDocument();
  expect(screen.getByRole("button", { name: "Retry" })).toBeInTheDocument();
});

it("asks for a worker upgrade instead of inspecting old nodes", () => {
  renderObservations({ ...node, capabilities: null });
  expect(screen.getByText("Worker upgrade needed")).toBeInTheDocument();
  expect(screen.getByRole("button", { name: "Inspect" })).toBeDisabled();
});

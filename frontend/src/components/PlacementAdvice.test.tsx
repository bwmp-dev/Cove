import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, expect, it, vi } from "vitest";
import { request } from "../api/client";
import { PlacementAdvice } from "./PlacementAdvice";

vi.mock("../api/client", () => ({ request: vi.fn(), errorMessage: (error: Error) => error.message }));
afterEach(() => vi.resetAllMocks());

const sampled = new Date().toISOString();
const report = {
  requirements: { cpus: 1, memory_mb: 0 },
  limited: false,
  nodes: [
    { id: "n1", name: "roomy", state: "ready", sampled_at: sampled, available_cpus: 3.5, available_memory_bytes: 2 * 1024 ** 3, fits: true, notice: "Measured free capacity; limits are caps, not reservations. Placement remains your choice." },
    { id: "n2", name: "busy", state: "ready", sampled_at: sampled, available_cpus: 0.25, available_memory_bytes: 512 * 1024 ** 2, fits: false, notice: "Measured free capacity; limits are caps, not reservations. Placement remains your choice." },
    { id: "n3", name: "offline", state: "unreachable", sampled_at: null, available_cpus: null, available_memory_bytes: null, fits: null, notice: "Fresh node capacity is unavailable." },
  ],
};

function renderAdvice(selectedNodeId = "n2") {
  return render(
    <QueryClientProvider client={new QueryClient()}>
      <PlacementAdvice projectId="p1" environmentId="e1" selectedNodeId={selectedNodeId} />
    </QueryClientProvider>,
  );
}

it("fetches only on request and shows fit, unknown capacity and the current selection", async () => {
  vi.mocked(request).mockResolvedValue(report);
  renderAdvice();
  expect(request).not.toHaveBeenCalled();

  fireEvent.change(screen.getByLabelText(/Estimated CPU cores/), { target: { value: "1" } });
  fireEvent.click(screen.getByRole("button", { name: "Inspect nodes" }));
  await waitFor(() => expect(request).toHaveBeenCalledWith("GET", "/api/placement-suggestions?project_id=p1&environment_id=e1&cpus=1", undefined, expect.any(AbortSignal)));

  expect(await screen.findByText("Fits measured free capacity")).toBeInTheDocument();
  expect(screen.getByText("Exceeds measured free capacity")).toBeInTheDocument();
  expect(screen.getByText("Capacity unknown")).toBeInTheDocument();
  expect(screen.getByText("CPU unknown · memory unknown")).toBeInTheDocument();
  expect(screen.getByText(/no explicit memory requirement/)).toBeInTheDocument();
  expect(screen.getByText("Selected").closest("li")).toHaveTextContent("busy");
  fireEvent.click(screen.getByRole("button", { name: "Refresh advice" }));
  await waitFor(() => expect(request).toHaveBeenCalledTimes(2));
});

it("hides advice from a previous scope until it is inspected again", async () => {
  vi.mocked(request).mockResolvedValue(report);
  const client = new QueryClient();
  const { rerender } = render(<QueryClientProvider client={client}><PlacementAdvice projectId="p1" environmentId="e1" selectedNodeId="" /></QueryClientProvider>);
  fireEvent.click(screen.getByRole("button", { name: "Inspect nodes" }));
  expect(await screen.findByText("roomy")).toBeInTheDocument();

  rerender(<QueryClientProvider client={client}><PlacementAdvice projectId="p1" environmentId="e2" selectedNodeId="" /></QueryClientProvider>);
  expect(screen.queryByText("roomy")).not.toBeInTheDocument();
  expect(screen.getByRole("button", { name: "Inspect nodes" })).toBeInTheDocument();
  expect(request).toHaveBeenCalledTimes(1);
});

it("inspects on Enter instead of submitting the surrounding form", async () => {
  vi.mocked(request).mockResolvedValue(report);
  const submit = vi.fn((event: Event) => event.preventDefault());
  render(
    <QueryClientProvider client={new QueryClient()}>
      <form onSubmit={(event) => submit(event.nativeEvent)}>
        <PlacementAdvice projectId="p1" environmentId="" selectedNodeId="" />
      </form>
    </QueryClientProvider>,
  );
  fireEvent.keyDown(screen.getByLabelText(/Estimated memory/), { key: "Enter" });
  await waitFor(() => expect(request).toHaveBeenCalledWith("GET", "/api/placement-suggestions?project_id=p1", undefined, expect.any(AbortSignal)));
  expect(submit).not.toHaveBeenCalled();
});

it("rejects invalid estimates and reports unavailable advice", async () => {
  renderAdvice();
  const cpu = screen.getByLabelText(/Estimated CPU cores/);
  fireEvent.change(cpu, { target: { value: "2000" } });
  fireEvent.click(screen.getByRole("button", { name: "Inspect nodes" }));
  expect(await screen.findByText("Enter CPU cores from 0.01 to 1024, or leave blank.")).toBeInTheDocument();
  expect(cpu).toHaveAttribute("aria-invalid", "true");
  expect(request).not.toHaveBeenCalled();

  vi.mocked(request).mockRejectedValue(new Error("placement measurements unavailable"));
  fireEvent.change(cpu, { target: { value: "" } });
  fireEvent.click(screen.getByRole("button", { name: "Inspect nodes" }));
  expect(await screen.findByText("placement measurements unavailable")).toBeInTheDocument();
});

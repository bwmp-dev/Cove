import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, expect, it, vi } from "vitest";
import { ApiError, request } from "../api/client";
import { useCapacityGate } from "./CapacityCheck";

vi.mock("../api/client", async (importActual) => ({ ...(await importActual<typeof import("../api/client")>()), request: vi.fn() }));
vi.mock("@tanstack/react-router", () => ({ Link: ({ children }: { children: React.ReactNode }) => <span>{children}</span> }));

HTMLDialogElement.prototype.showModal = function (this: HTMLDialogElement) {
  this.open = true;
};
HTMLDialogElement.prototype.close = function (this: HTMLDialogElement) {
  this.open = false;
};

const deploy = vi.fn();

function Harness() {
  const gate = useCapacityGate({ workloadId: "res 1", actionLabel: "Deploy", onDeploy: deploy });
  return (
    <>
      <button type="button" onClick={gate.start}>
        Start deploy
      </button>
      {gate.dialog}
    </>
  );
}

function renderGate() {
  render(
    <QueryClientProvider client={new QueryClient({ defaultOptions: { mutations: { retry: false } } })}>
      <Harness />
    </QueryClientProvider>,
  );
  fireEvent.click(screen.getByRole("button", { name: "Start deploy" }));
}

afterEach(() => vi.resetAllMocks());

it("queues the deploy straight away when every capacity check passes", async () => {
  vi.mocked(request).mockResolvedValueOnce({ checks: [{ name: "Memory", state: "pass", message: "Enough memory" }] });
  renderGate();
  await waitFor(() => expect(deploy).toHaveBeenCalledTimes(1));
  expect(request).toHaveBeenCalledWith("GET", "/api/workloads/res%201/capacity");
});

it("asks before deploying when a check needs review, and cancelling does not deploy", async () => {
  vi.mocked(request).mockResolvedValueOnce({ checks: [{ name: "CPU", state: "warning", message: "web has no CPU limit" }] });
  renderGate();
  expect(await screen.findByText("web has no CPU limit")).toBeInTheDocument();
  expect(deploy).not.toHaveBeenCalled();
  fireEvent.click(screen.getByRole("button", { name: "Cancel" }));
  expect(deploy).not.toHaveBeenCalled();
});

it("lets people continue explicitly after a failed check", async () => {
  vi.mocked(request).mockResolvedValueOnce({ checks: [{ name: "Memory", state: "error", message: "Needs 4 GiB, 1 GiB available" }] });
  renderGate();
  fireEvent.click(await screen.findByRole("button", { name: "Deploy anyway" }));
  expect(deploy).toHaveBeenCalledTimes(1);
});

it("shows an unavailable check and offers retry or deploying without it", async () => {
  vi.mocked(request)
    .mockRejectedValueOnce(new ApiError(409, "upgrade and verify a ready worker for capacity advice"))
    .mockResolvedValueOnce({ checks: [{ name: "Memory", state: "warning", message: "Unknown limit" }] });
  renderGate();
  expect(await screen.findByText("Capacity check unavailable")).toBeInTheDocument();
  expect(screen.getByText(/upgrade and verify a ready worker/)).toBeInTheDocument();
  fireEvent.click(screen.getByRole("button", { name: "Retry check" }));
  expect(await screen.findByText("Unknown limit")).toBeInTheDocument();
  expect(deploy).not.toHaveBeenCalled();
});

it("deploys without the check only when chosen", async () => {
  vi.mocked(request).mockRejectedValueOnce(new ApiError(502, "capacity inspection unavailable"));
  renderGate();
  fireEvent.click(await screen.findByRole("button", { name: "Deploy without check" }));
  expect(deploy).toHaveBeenCalledTimes(1);
});

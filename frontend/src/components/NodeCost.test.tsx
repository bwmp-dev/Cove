import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, expect, it, vi } from "vitest";
import { request } from "../api/client";
import { NodeCost } from "./NodeCost";

vi.mock("../api/client", async (importActual) => ({ ...(await importActual<typeof import("../api/client")>()), request: vi.fn() }));

function renderCost() {
  render(
    <QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}>
      <NodeCost nodeId="n 1" />
    </QueryClientProvider>,
  );
}

afterEach(() => vi.resetAllMocks());

it("treats a missing price as unknown and saves an exact amount in minor units", async () => {
  vi.mocked(request)
    .mockResolvedValueOnce({ monthly_minor: 0, currency: "", configured: false })
    .mockResolvedValueOnce({ monthly_minor: 1999, currency: "USD", configured: true });
  renderCost();
  expect(await screen.findByText(/No price set/)).toBeInTheDocument();
  fireEvent.change(screen.getByLabelText("Currency"), { target: { value: "usd" } });
  fireEvent.change(screen.getByLabelText("Monthly price"), { target: { value: "19.99" } });
  fireEvent.click(screen.getByRole("button", { name: "Save price" }));
  await waitFor(() => expect(request).toHaveBeenCalledWith("PUT", "/api/nodes/n%201/cost", { monthly_minor: 1999, currency: "USD" }));
  expect(await screen.findByText("Price saved")).toBeInTheDocument();
});

it("rejects amounts with more decimals than the currency uses and unknown currencies", async () => {
  vi.mocked(request).mockResolvedValueOnce({ monthly_minor: 500, currency: "JPY", configured: true });
  renderCost();
  expect(await screen.findByText(/JPY\s500/)).toBeInTheDocument();
  fireEvent.change(screen.getByLabelText("Monthly price"), { target: { value: "5.5" } });
  fireEvent.click(screen.getByRole("button", { name: "Save price" }));
  expect(await screen.findByText("JPY has no minor unit; enter a whole amount.")).toBeInTheDocument();
  fireEvent.change(screen.getByLabelText("Currency"), { target: { value: "ZZ" } });
  fireEvent.click(screen.getByRole("button", { name: "Save price" }));
  expect(await screen.findByText(/three-letter ISO 4217/)).toBeInTheDocument();
  expect(request).toHaveBeenCalledTimes(1);
});

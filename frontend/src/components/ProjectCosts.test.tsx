import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, render, screen, within } from "@testing-library/react";
import { afterEach, expect, it, vi } from "vitest";
import { ApiError, request } from "../api/client";
import { ProjectCosts } from "./ProjectCosts";

vi.mock("../api/client", async (importActual) => ({ ...(await importActual<typeof import("../api/client")>()), request: vi.fn() }));

const plain = (text: string | null | undefined) => (text ?? "").replace(/\s/g, " ");

function renderCosts(environmentId?: string) {
  render(
    <QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}>
      <ProjectCosts projectId="p1" environmentId={environmentId} />
    </QueryClientProvider>,
  );
}

const costs = {
  allocation: "Equal share per configured resource on a node; currencies are not converted.",
  items: [
    { environment_id: "e1", environment_name: "production", node_id: "n-hel", node_name: "hel-1", currency: "EUR", monthly_minor: 1250.5, resources: 2 },
    { environment_id: "e1", environment_name: "production", node_id: "n-home", node_name: "home", currency: "USD", monthly_minor: 0, resources: 1 },
    { environment_id: "", environment_name: "", node_id: "n-spare", node_name: "spare", currency: null, monthly_minor: null, resources: 1 },
  ],
};

afterEach(() => vi.resetAllMocks());

it("keeps currencies apart and distinguishes free nodes from unpriced ones", async () => {
  vi.mocked(request).mockResolvedValueOnce(costs);
  renderCosts();
  const total = await screen.findByText(/Project total/);
  expect(plain(total.parentElement?.textContent)).toBe("Project total: EUR 12.51 + USD 0.00 and unknown per month for 4 resources");
  expect(screen.getByText(/1 resource runs on nodes without a price/)).toBeInTheDocument();
  expect(screen.getByText("No node price")).toBeInTheDocument();
  expect(screen.getByText("(free)")).toBeInTheDocument();
  expect(screen.getByText("Project resources")).toBeInTheDocument();
  expect(screen.getByText(/including resources that are not deployed/)).toBeInTheDocument();
});

it("shows one environment's share on its page", async () => {
  vi.mocked(request).mockResolvedValueOnce(costs);
  renderCosts("e1");
  const total = await screen.findByText(/Environment total/);
  expect(plain(total.parentElement?.textContent)).toBe("Environment total: EUR 12.51 + USD 0.00 per month for 3 resources");
  expect(screen.queryByText("spare")).not.toBeInTheDocument();
});

it("keeps nodes with identical names and shares as separate rows across refreshes", async () => {
  const errors = vi.spyOn(console, "error").mockImplementation(() => undefined);
  const twins = {
    allocation: costs.allocation,
    items: [
      { environment_id: "e1", environment_name: "production", node_id: "node-a", node_name: "worker", currency: "EUR", monthly_minor: 500, resources: 1 },
      { environment_id: "e1", environment_name: "production", node_id: "node-b", node_name: "worker", currency: "EUR", monthly_minor: 500, resources: 1 },
    ],
  };
  vi.mocked(request).mockResolvedValue(twins);
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(
    <QueryClientProvider client={client}>
      <ProjectCosts projectId="p1" />
    </QueryClientProvider>,
  );
  const table = await screen.findByRole("table");
  expect(within(table).getAllByText("worker")).toHaveLength(2);
  await act(() => client.refetchQueries());
  expect(request).toHaveBeenCalledTimes(2);
  expect(within(screen.getByRole("table")).getAllByText("worker")).toHaveLength(2);
  expect(errors.mock.calls.flat().join(" ")).not.toMatch(/same key/);
  errors.mockRestore();
});

it("explains when cost estimates need project access", async () => {
  vi.mocked(request).mockRejectedValueOnce(new ApiError(403, "forbidden"));
  renderCosts();
  expect(await screen.findByText("Cost estimates need project access")).toBeInTheDocument();
});

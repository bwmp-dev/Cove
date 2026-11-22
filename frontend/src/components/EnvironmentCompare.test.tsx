import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, within } from "@testing-library/react";
import { afterEach, expect, it, vi } from "vitest";
import { ApiError, request } from "../api/client";
import type { Environment } from "../api/projects";
import { EnvironmentCompare } from "./EnvironmentCompare";

vi.mock("../api/client", async (original) => ({ ...(await original<typeof import("../api/client")>()), request: vi.fn() }));
afterEach(() => vi.clearAllMocks());

const environments: Environment[] = [
  { id: "e1", name: "Production", permissions: ["read"] },
  { id: "e2", name: "Staging", permissions: ["read"] },
];

function setup(response: () => Promise<unknown>, list = environments) {
  vi.mocked(request).mockImplementation(response);
  render(
    <QueryClientProvider client={new QueryClient()}>
      <EnvironmentCompare projectId="p" environments={list} />
    </QueryClientProvider>,
  );
}

it("compares two environments by resource name without secret values", async () => {
  setup(() =>
    Promise.resolve({
      from: "Production",
      to: "Staging",
      resources: [
        { name: "web", state: "different", fields: ["source.branch", "environment"], diffs: [{ field: "source.branch", before: '"main"', after: '"develop"' }], from_revision: "aaaaaaaaaaaaaaaa", to_revision: "bbbbbbbbbbbbbbbb" },
        { name: "db", state: "equal", fields: [], diffs: null, from_revision: "", to_revision: "" },
        { name: "worker", state: "only_from", fields: null, diffs: null, from_revision: "cccccccccccccccc", to_revision: "" },
      ],
      variable_changes: ["API_URL"],
      policy_changed: true,
    }),
  );
  expect(await screen.findByText("2 of 3 resources differ between Production and Staging.")).toBeInTheDocument();
  expect(request).toHaveBeenCalledWith("GET", "/api/projects/p/configuration/compare?from=e1&to=e2", undefined, expect.anything());
  const table = screen.getByRole("table", { name: "Resources in Production compared with Staging" });
  const web = within(table).getByRole("row", { name: /^web/ });
  expect(within(web).getByText("main")).toBeInTheDocument();
  expect(within(web).getByText("develop")).toBeInTheDocument();
  expect(within(web).getByText("environment")).toBeInTheDocument();
  expect(within(table).getAllByText("Only in Production").length).toBeGreaterThan(0);
  expect(screen.getByText("API_URL")).toBeInTheDocument();
  expect(screen.getByText("different")).toBeInTheDocument();
});

it("asks for duplicate resource names to be renamed", async () => {
  setup(() => Promise.reject(new ApiError(409, "Staging has two resources named web.")));
  expect(await screen.findByText("Resource names must be unique to compare")).toBeInTheDocument();
  expect(screen.getByText(/Staging has two resources named web\. Rename the duplicate resources/)).toBeInTheDocument();
});

it("needs two readable environments", () => {
  setup(() => Promise.reject(new Error("unexpected")), [environments[0]!, { id: "e3", name: "QA", permissions: [] }]);
  expect(screen.getByText(/needs two environments you can read/)).toBeInTheDocument();
  expect(request).not.toHaveBeenCalled();
});

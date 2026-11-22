import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { ReactNode } from "react";
import { afterEach, expect, it, vi } from "vitest";
import { ApiError, request } from "../api/client";
import type { WorkloadDrift } from "../api/project-configuration";
import { RuntimeDrift } from "./RuntimeDrift";

vi.mock("../api/client", async (original) => ({ ...(await original<typeof import("../api/client")>()), request: vi.fn() }));
vi.mock("@tanstack/react-router", () => ({ Link: ({ children }: { children: ReactNode }) => <a href="#link">{children}</a> }));
afterEach(() => vi.clearAllMocks());

const report = (change: Partial<WorkloadDrift>): WorkloadDrift => ({ baseline: true, busy: false, operation: "op1", captured_at: "2026-11-21T09:00:00Z", checked_at: "2026-11-22T09:00:00Z", changes: [], ...change });

function setup(...responses: (() => Promise<unknown>)[]) {
  const mock = vi.mocked(request);
  for (const response of responses) mock.mockImplementationOnce(response);
  render(
    <QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}>
      <RuntimeDrift workloadId="w1" />
    </QueryClientProvider>,
  );
}

it("does not call a missing baseline or a deferred inspection clean", async () => {
  const user = userEvent.setup();
  setup(
    () => Promise.resolve(report({ baseline: false, captured_at: null, operation: "" })),
    () => Promise.resolve({ baseline: false, busy: true, operation: "", captured_at: null, checked_at: "2026-11-22T09:00:00Z", changes: null }),
  );
  expect(await screen.findByText("No baseline recorded yet")).toBeInTheDocument();
  expect(screen.queryByText(/No drift/)).not.toBeInTheDocument();
  await user.click(screen.getByRole("button", { name: "Refresh" }));
  expect(await screen.findByText("Inspection deferred")).toBeInTheDocument();
  expect(screen.queryByText("No baseline recorded yet")).not.toBeInTheDocument();
  expect(screen.queryByText(/No drift/)).not.toBeInTheDocument();
  expect(request).toHaveBeenCalledWith("GET", "/api/workloads/w1/drift", undefined, expect.anything());
});

it("lists changed categories per container without values", async () => {
  setup(() =>
    Promise.resolve(
      report({
        changes: [
          { container: "a".repeat(64), service: "web", state: "changed", fields: ["image", "mounts"] },
          { container: "b".repeat(64), service: "worker", state: "missing", fields: null },
        ],
      }),
    ),
  );
  expect(await screen.findByText("2 containers differ from the baseline")).toBeInTheDocument();
  const table = screen.getByRole("table", { name: "Containers that differ from the baseline" });
  expect(within(within(table).getByRole("row", { name: /^web/ })).getByText("image, mounts")).toBeInTheDocument();
  expect(within(within(table).getByRole("row", { name: /^worker/ })).getAllByText("Missing").length).toBeGreaterThan(0);
  expect(screen.getByRole("link", { name: "Baseline operation" })).toBeInTheDocument();
});

it("reports a clean inspection and asks to upgrade unsupported workers", async () => {
  const user = userEvent.setup();
  setup(
    () => Promise.resolve(report({})),
    () => Promise.reject(new ApiError(409, "upgrade and verify the worker before checking configuration drift")),
  );
  expect(await screen.findByText("No drift since the last Cove operation")).toBeInTheDocument();
  await user.click(screen.getByRole("button", { name: "Refresh" }));
  expect(await screen.findByText("The worker cannot inspect drift")).toBeInTheDocument();
});

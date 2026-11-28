import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { ReactNode } from "react";
import { afterEach, expect, it, vi } from "vitest";
import { ApiError, request } from "../api/client";
import { DeployReview } from "./DeployReview";

vi.mock("../api/client", async (original) => ({ ...(await original<typeof import("../api/client")>()), request: vi.fn() }));
vi.mock("@tanstack/react-router", () => ({ Link: ({ children }: { children: ReactNode }) => <a href="#link">{children}</a> }));
afterEach(() => vi.clearAllMocks());

function setup(response: () => Promise<unknown>) {
  vi.mocked(request).mockImplementation(response);
  const onConfirm = vi.fn();
  render(
    <QueryClientProvider client={new QueryClient()}>
      <DeployReview workloadId="w1" confirmLabel="Deploy" onCancel={() => undefined} onConfirm={onConfirm} />
    </QueryClientProvider>,
  );
  return onConfirm;
}

it("waits for the review before allowing the deploy and then shows safe field changes", async () => {
  const user = userEvent.setup();
  let resolve: (value: unknown) => void = () => undefined;
  const onConfirm = setup(() => new Promise((done) => (resolve = done)));
  expect(screen.getByText("Loading pending configuration changes")).toBeInTheDocument();
  expect(screen.getByRole("button", { name: "Deploy" })).toBeDisabled();
  expect(onConfirm).not.toHaveBeenCalled();

  resolve({ has_baseline: true, operation_id: "op1", revision: "abcdef0123456789", fields: ["source.branch", "environment"], diffs: [{ field: "source.branch", before: '"main"', after: '"release"' }] });
  expect(await screen.findByText("release")).toBeInTheDocument();
  expect(screen.getByText("abcdef0")).toBeInTheDocument();
  expect(screen.getByText("environment")).toBeInTheDocument();
  expect(request).toHaveBeenCalledWith("GET", "/api/workloads/w1/configuration-diff", undefined, expect.anything());
  await user.click(screen.getByRole("button", { name: "Deploy" }));
  expect(onConfirm).toHaveBeenCalledTimes(1);
});

it("says changes are unknown rather than absent without a baseline", async () => {
  setup(() => Promise.resolve({ has_baseline: false, operation_id: "", revision: "", fields: [], diffs: null }));
  expect(await screen.findByText("Changes since the last deployment are unknown")).toBeInTheDocument();
  expect(screen.queryByText(/No configuration changes/)).not.toBeInTheDocument();
  expect(screen.getByRole("button", { name: "Deploy" })).toBeEnabled();
});

it("keeps the deploy available but visible when the review fails", async () => {
  setup(() => Promise.reject(new ApiError(503, "Configuration storage is unavailable.")));
  expect(await screen.findByText(/Configuration storage is unavailable\./)).toBeInTheDocument();
  expect(screen.getByRole("button", { name: "Deploy" })).toBeEnabled();
});

it("shows request inputs such as a reason between the review and the actions and honours their validity", async () => {
  vi.mocked(request).mockResolvedValue({ has_baseline: true, operation_id: "op1", revision: "", fields: [], diffs: null });
  const onConfirm = vi.fn();
  const { rerender } = render(
    <QueryClientProvider client={new QueryClient()}>
      <DeployReview workloadId="w1" confirmLabel="Deploy" onCancel={() => undefined} onConfirm={onConfirm} confirmDisabled>
        <label htmlFor="reason">Reason</label>
        <textarea id="reason" />
      </DeployReview>
    </QueryClientProvider>,
  );
  expect(await screen.findByText(/No configuration changes/)).toBeInTheDocument();
  expect(screen.getByLabelText("Reason")).toBeInTheDocument();
  expect(screen.getByRole("button", { name: "Deploy" })).toBeDisabled();
  rerender(
    <QueryClientProvider client={new QueryClient()}>
      <DeployReview workloadId="w1" confirmLabel="Deploy" onCancel={() => undefined} onConfirm={onConfirm} pending>
        <label htmlFor="reason">Reason</label>
        <textarea id="reason" />
      </DeployReview>
    </QueryClientProvider>,
  );
  expect(screen.getByRole("button", { name: "Cancel" })).toBeDisabled();
});

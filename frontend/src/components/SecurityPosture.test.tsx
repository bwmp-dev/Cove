import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen } from "@testing-library/react";
import { expect, it, vi } from "vitest";
import { request } from "../api/client";
import { SecurityPosture } from "./SecurityPosture";

vi.mock("../api/client", async (original) => ({ ...(await original<typeof import("../api/client")>()), request: vi.fn() }));

it("lists checks that need review first with their remediation", async () => {
  vi.mocked(request).mockResolvedValue({
    checks: [
      { id: "https", status: "pass", message: "The public URL uses HTTPS.", remediation: "Use HTTPS." },
      { id: "backups", status: "warn", message: "Host backup freshness is unknown.", remediation: "Check host backups outside Cove." },
    ],
  });
  render(
    <QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}>
      <SecurityPosture />
    </QueryClientProvider>,
  );
  const items = await screen.findAllByRole("listitem");
  expect(items[0]).toHaveTextContent("Host backup freshness is unknown.");
  expect(items[0]).toHaveTextContent("Check host backups outside Cove.");
  expect(items[0]).toHaveTextContent("Review");
  expect(items[1]).toHaveTextContent("Passed");
  expect(items[1]).not.toHaveTextContent("Use HTTPS.");
  expect(screen.getByText(/1 of 2 checks passed/)).toBeInTheDocument();
});

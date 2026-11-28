import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { createMemoryHistory, createRootRoute, createRouter, RouterProvider } from "@tanstack/react-router";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, expect, it, vi } from "vitest";
import { request } from "../api/client";
import { McpSettings } from "./McpSettings";

vi.mock("../api/client", () => ({ request: vi.fn(), errorMessage: (error: unknown) => (error instanceof Error ? error.message : String(error)) }));
afterEach(() => vi.clearAllMocks());

function renderSettings() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const router = createRouter({ routeTree: createRootRoute({ component: McpSettings }), history: createMemoryHistory() });
  return render(
    <QueryClientProvider client={client}>
      <RouterProvider router={router} />
    </QueryClientProvider>,
  );
}

it("shows the real endpoint and turns MCP on", async () => {
  const user = userEvent.setup();
  vi.mocked(request).mockImplementation((method: string) => Promise.resolve({ enabled: method === "PUT", url: "https://cove.example.test/mcp" }));
  renderSettings();
  expect(await screen.findByText("https://cove.example.test/mcp")).toBeInTheDocument();
  expect(screen.getByText("Off")).toBeInTheDocument();
  await user.click(screen.getByRole("button", { name: "Turn on" }));
  await waitFor(() => expect(request).toHaveBeenCalledWith("PUT", "/api/mcp", { enabled: true }));
  expect(await screen.findByRole("button", { name: "Turn off" })).toBeInTheDocument();
});

it("reports permission errors from the control plane", async () => {
  vi.mocked(request).mockRejectedValue(new Error("Permission denied"));
  renderSettings();
  expect(await screen.findByText("Permission denied")).toBeInTheDocument();
});

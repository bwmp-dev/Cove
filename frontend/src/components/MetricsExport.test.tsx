import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { request } from "../api/client";
import { MetricsExport } from "./MetricsExport";

vi.mock("../api/client", async (importActual) => ({ ...(await importActual<typeof import("../api/client")>()), request: vi.fn() }));

beforeEach(() => {
  HTMLDialogElement.prototype.showModal = vi.fn(function (this: HTMLDialogElement) { this.open = true; });
  HTMLDialogElement.prototype.close = vi.fn(function (this: HTMLDialogElement) { this.open = false; });
});
afterEach(() => { vi.resetAllMocks(); localStorage.clear(); sessionStorage.clear(); });

function renderExport(configured: boolean) {
  vi.mocked(request).mockImplementation((method) => {
    if (method === "GET") return Promise.resolve({ configured });
    if (method === "POST") return Promise.resolve({ token: "cove-metrics-one-time" });
    return Promise.resolve({ configured: false });
  });
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(<QueryClientProvider client={client}><MetricsExport /></QueryClientProvider>);
  return client;
}

it("rotates only after confirmation and shows the token once, outside storage and the URL", async () => {
  const client = renderExport(true);
  expect(await screen.findByText("Configured")).toBeInTheDocument();
  expect(screen.getByText(/credentials_file: \/etc\/prometheus\/cove-token/)).toBeInTheDocument();
  fireEvent.click(screen.getByRole("button", { name: "Rotate token" }));
  const dialog = screen.getByRole("dialog", { name: "Rotate the scrape token?" });
  expect(within(dialog).getByText(/stops working immediately/)).toBeInTheDocument();
  expect(request).not.toHaveBeenCalledWith("POST", expect.anything());
  fireEvent.click(within(dialog).getByRole("button", { name: "Rotate token" }));
  const field = await screen.findByLabelText("Scrape token");
  expect(field).toHaveValue("cove-metrics-one-time");
  expect(field).toHaveAttribute("type", "password");
  fireEvent.click(screen.getByRole("button", { name: "Show" }));
  expect(field).toHaveAttribute("type", "text");
  expect(window.location.href).not.toContain("cove-metrics-one-time");
  expect(JSON.stringify({ ...localStorage, ...sessionStorage })).not.toContain("cove-metrics-one-time");
  expect(JSON.stringify(client.getQueryCache().getAll().map((query) => query.state.data))).not.toContain("cove-metrics-one-time");
  fireEvent.click(screen.getByRole("button", { name: "Done" }));
  await waitFor(() => expect(screen.queryByDisplayValue("cove-metrics-one-time")).not.toBeInTheDocument());
});

it("revokes the token after confirmation", async () => {
  renderExport(true);
  fireEvent.click(await screen.findByRole("button", { name: "Revoke" }));
  fireEvent.click(within(screen.getByRole("dialog", { name: "Revoke the scrape token?" })).getByRole("button", { name: "Revoke token" }));
  await waitFor(() => expect(request).toHaveBeenCalledWith("DELETE", "/api/settings/metrics-token"));
  expect(await screen.findByText("Not configured")).toBeInTheDocument();
  expect(screen.getByRole("button", { name: "Create token" })).toBeInTheDocument();
});

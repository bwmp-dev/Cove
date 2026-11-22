import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { request } from "../api/client";
import { StatusPageSettings } from "./StatusPageSettings";

vi.mock("../api/client", async (importActual) => ({ ...(await importActual<typeof import("../api/client")>()), request: vi.fn() }));
vi.mock("./SaveBar", async (importActual) => ({ ...(await importActual<typeof import("./SaveBar")>()), UnsavedChangesGuard: () => null }));

const slug = "a".repeat(64);
beforeEach(() => {
  HTMLDialogElement.prototype.showModal = vi.fn(function (this: HTMLDialogElement) { this.open = true; });
  HTMLDialogElement.prototype.close = vi.fn(function (this: HTMLDialogElement) { this.open = false; });
});
afterEach(() => vi.resetAllMocks());

function renderSettings(saved: unknown) {
  vi.mocked(request).mockImplementation((method, _path, body) => (method === "GET" ? Promise.resolve(saved) : Promise.resolve({ ...(body as object), slug })));
  render(<QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}><StatusPageSettings projectId="p1" /></QueryClientProvider>);
}

it("asks for explicit consent to the public disclosure before publishing", async () => {
  renderSettings({ enabled: false, title: "", slug: "" });
  fireEvent.click(await screen.findByRole("switch", { name: "Publish status page" }));
  fireEvent.click(screen.getByRole("button", { name: "Save changes" }));
  expect(await screen.findByText("Enter a title of at most 120 bytes.")).toBeInTheDocument();
  fireEvent.change(screen.getByLabelText(/Title/), { target: { value: "Shop status" } });
  fireEvent.click(screen.getByRole("button", { name: "Save changes" }));
  const dialog = screen.getByRole("dialog", { name: "Publish this status page?" });
  expect(within(dialog).getByText(/without signing in/)).toBeInTheDocument();
  expect(request).not.toHaveBeenCalledWith("PUT", expect.anything(), expect.anything());
  fireEvent.click(within(dialog).getByRole("button", { name: "Publish page" }));
  await waitFor(() => expect(request).toHaveBeenCalledWith("PUT", "/api/projects/p1/status-page", { enabled: true, title: "Shop status" }));
  expect(await screen.findByDisplayValue(`${window.location.origin}/status/${slug}`)).toBeInTheDocument();
});

it("turns the page off without a confirmation and hides the link", async () => {
  renderSettings({ enabled: true, title: "Shop status", slug });
  expect(await screen.findByRole("link", { name: "Open page" })).toHaveAttribute("href", `${window.location.origin}/status/${slug}`);
  fireEvent.click(screen.getByRole("switch", { name: "Publish status page" }));
  fireEvent.click(screen.getByRole("button", { name: "Save changes" }));
  await waitFor(() => expect(request).toHaveBeenCalledWith("PUT", "/api/projects/p1/status-page", { enabled: false, title: "Shop status" }));
  await waitFor(() => expect(screen.queryByRole("link", { name: "Open page" })).not.toBeInTheDocument());
});

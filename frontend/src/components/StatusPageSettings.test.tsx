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

function renderSettings(saved: unknown, savedSlug = slug) {
  vi.mocked(request).mockImplementation((method, _path, body) => (method === "GET" ? Promise.resolve(saved) : Promise.resolve({ ...(body as object), slug: savedSlug, ownership_required: false })));
  render(<QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}><StatusPageSettings projectId="p1" /></QueryClientProvider>);
}

it("asks for explicit consent to the public disclosure before publishing", async () => {
  renderSettings({ enabled: false, title: "", slug: "", ownership_required: false });
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
  renderSettings({ enabled: true, title: "Shop status", slug, ownership_required: false });
  expect(await screen.findByRole("link", { name: "Open page" })).toHaveAttribute("href", `${window.location.origin}/status/${slug}`);
  fireEvent.click(screen.getByRole("switch", { name: "Publish status page" }));
  fireEvent.click(screen.getByRole("button", { name: "Save changes" }));
  await waitFor(() => expect(request).toHaveBeenCalledWith("PUT", "/api/projects/p1/status-page", { enabled: false, title: "Shop status" }));
  await waitFor(() => expect(screen.queryByRole("link", { name: "Open page" })).not.toBeInTheDocument());
});

it("keeps a page without an authorised publisher off and claims it with a new link only after confirmation", async () => {
  const oldSlug = "b".repeat(64);
  const newSlug = "c".repeat(64);
  renderSettings({ enabled: false, title: "Shop status", slug: oldSlug, ownership_required: true }, newSlug);
  expect(await screen.findByText("Status page is suspended")).toBeInTheDocument();
  expect(screen.getByRole("switch", { name: "Publish status page" })).not.toBeChecked();
  expect(screen.queryByDisplayValue(new RegExp(oldSlug))).not.toBeInTheDocument();
  expect(screen.queryByRole("link", { name: "Open page" })).not.toBeInTheDocument();
  fireEvent.click(screen.getByRole("switch", { name: "Publish status page" }));
  fireEvent.click(screen.getByRole("button", { name: "Save changes" }));
  const dialog = screen.getByRole("dialog", { name: "Publish this status page?" });
  expect(within(dialog).getByText(/gets a new link/)).toBeInTheDocument();
  expect(request).not.toHaveBeenCalledWith("PUT", expect.anything(), expect.anything());
  fireEvent.click(within(dialog).getByRole("button", { name: "Publish page" }));
  await waitFor(() => expect(request).toHaveBeenCalledWith("PUT", "/api/projects/p1/status-page", { enabled: true, title: "Shop status" }));
  expect(await screen.findByDisplayValue(`${window.location.origin}/status/${newSlug}`)).toBeInTheDocument();
  expect(screen.queryByText("Status page is suspended")).not.toBeInTheDocument();
  expect(screen.queryByDisplayValue(new RegExp(oldSlug))).not.toBeInTheDocument();
});

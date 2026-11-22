import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { request } from "../api/client";
import { ResourceOwner } from "./ResourceOwner";

vi.mock("../api/client", () => ({ request: vi.fn(), errorMessage: (error: Error) => error.message }));
vi.mock("@tanstack/react-router", () => ({ useBlocker: () => ({ status: "idle" }) }));
beforeEach(() => vi.stubGlobal("ResizeObserver", class { observe = vi.fn(); disconnect = vi.fn(); }));
afterEach(() => { vi.clearAllMocks(); vi.unstubAllGlobals(); });

function renderOwner(owner: unknown, editable: boolean) {
  vi.mocked(request).mockImplementation((method) => Promise.resolve(method === "GET" ? owner : { ok: true }));
  render(<QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}><ResourceOwner id="w/1" editable={editable} /></QueryClientProvider>);
}

it("shows the owner read-only to viewers", async () => {
  renderOwner({ user_id: "u1", name: "Sam", candidates: [] }, false);
  expect(await screen.findByText("Sam")).toBeInTheDocument();
  expect(screen.queryByRole("combobox")).not.toBeInTheDocument();
  expect(request).toHaveBeenCalledWith("GET", "/api/workloads/w%2F1/owner", undefined, expect.anything());
});

it("assigns an owner from the active team members", async () => {
  renderOwner({ user_id: "", name: "", candidates: [{ user_id: "u1", name: "Sam" }, { user_id: "u2", name: "Alex" }] }, true);
  const field = await screen.findByRole("combobox", { name: /Resource owner/ });
  expect(field).toHaveTextContent("No owner");
  fireEvent.click(field);
  fireEvent.click(screen.getByRole("option", { name: "Alex" }));
  expect(screen.getByText("Unsaved changes")).toBeInTheDocument();
  fireEvent.click(screen.getByRole("button", { name: "Save changes" }));
  await waitFor(() => expect(request).toHaveBeenCalledWith("PUT", "/api/workloads/w%2F1/owner", { user_id: "u2" }));
});

it("clears the owner and shows API errors", async () => {
  renderOwner({ user_id: "u1", name: "Sam", candidates: [{ user_id: "u1", name: "Sam" }, { user_id: "u2", name: "Alex" }] }, true);
  fireEvent.click(await screen.findByRole("combobox", { name: /Resource owner/ }));
  fireEvent.click(screen.getByRole("option", { name: "No owner" }));
  vi.mocked(request).mockImplementationOnce(() => Promise.reject(new Error("preview configuration changes are not allowed")));
  fireEvent.click(screen.getByRole("button", { name: "Save changes" }));
  expect(await screen.findByText("preview configuration changes are not allowed")).toBeInTheDocument();
  expect(request).toHaveBeenCalledWith("PUT", "/api/workloads/w%2F1/owner", { user_id: "" });
});

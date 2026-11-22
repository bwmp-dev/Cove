import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { createMemoryHistory, createRootRoute, createRoute, createRouter, Outlet, RouterProvider } from "@tanstack/react-router";
import { fireEvent, render, screen } from "@testing-library/react";
import { afterEach, expect, it, vi } from "vitest";
import { request } from "../api/client";
import { RegisterPage } from "./auth";

vi.mock("../api/client", async importOriginal => ({ ...await importOriginal<typeof import("../api/client")>(), request: vi.fn() }));
afterEach(() => vi.clearAllMocks());

it("creates the first administrator without email and enters the dashboard", async () => {
  vi.mocked(request).mockImplementation((method, path) => {
    if (path === "/auth/config") return Promise.resolve({ setup_required: true, registration_enabled: true, email_enabled: false, github_enabled: false });
    if (method === "POST" && path === "/auth/register") return Promise.resolve({ ok: true, setup_complete: true });
    throw new Error(`Unexpected request ${method} ${path}`);
  });
  const root = createRootRoute({ component: Outlet });
  const register = createRoute({ getParentRoute: () => root, path: "/register", component: RegisterPage });
  const dashboard = createRoute({ getParentRoute: () => root, path: "/", component: () => <p>Setup dashboard</p> });
  const router = createRouter({ routeTree: root.addChildren([register, dashboard]), history: createMemoryHistory({ initialEntries: ["/register"] }) });
  render(<QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}><RouterProvider router={router} /></QueryClientProvider>);
  expect(await screen.findByRole("heading", { name: "Set up Cove" })).toBeInTheDocument();
  fireEvent.change(screen.getByLabelText("Name"), { target: { value: "Owner" } });
  fireEvent.change(screen.getByLabelText("Email"), { target: { value: "owner@example.com" } });
  fireEvent.change(screen.getByLabelText("Password"), { target: { value: "a-strong-test-password" } });
  fireEvent.change(screen.getByLabelText("Confirm password"), { target: { value: "a-strong-test-password" } });
  fireEvent.click(screen.getByRole("button", { name: "Create administrator account" }));
  expect(await screen.findByText("Setup dashboard")).toBeInTheDocument();
  expect(request).toHaveBeenCalledWith("POST", "/auth/register", { name: "Owner", email: "owner@example.com", password: "a-strong-test-password" });
});

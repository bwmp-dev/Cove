import { createBrowserHistory, createRootRoute, createRoute, createRouter, Outlet, RouterProvider } from "@tanstack/react-router";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, expect, it, vi } from "vitest";
import { ApiError } from "../api/client";
import { QueryError } from "./ui";

afterEach(() => window.history.replaceState(null, "", "/"));

it("keeps the original message and adds a fix for known errors outside a router", () => {
  render(<QueryError error={new ApiError(409, "Upgrade and verify the worker to run preflight")} onRetry={vi.fn()} what="preflight" />);
  const alert = screen.getByRole("alert");
  expect(alert).toHaveTextContent("Upgrade and verify the worker to run preflight");
  expect(alert).toHaveTextContent("Upgrade the worker from its node page, then verify the node.");
  expect(screen.getByRole("link", { name: "Open nodes" })).toHaveAttribute("href", "/nodes");
});

it("renders unknown errors unchanged", () => {
  render(<QueryError error={new ApiError(400, "Name must contain 1-120 characters")} onRetry={vi.fn()} what="the project" />);
  expect(screen.getByRole("alert")).toHaveTextContent(/^Could not load the projectName must contain 1-120 charactersRetry$/);
  expect(screen.queryByRole("link")).not.toBeInTheDocument();
});

it("links to the open resource's troubleshooting page through the router", async () => {
  const user = userEvent.setup();
  const root = createRootRoute({ component: Outlet });
  const logs = createRoute({
    getParentRoute: () => root,
    path: "/resources/$resourceId/logs",
    component: () => <QueryError error={new ApiError(502, "The worker could not be reached.")} onRetry={vi.fn()} what="logs" />,
  });
  const troubleshooting = createRoute({ getParentRoute: () => root, path: "/resources/$resourceId/troubleshooting", component: () => <h1>Troubleshooting page</h1> });
  window.history.replaceState(null, "", "/resources/r1/logs");
  const router = createRouter({ routeTree: root.addChildren([logs, troubleshooting]), history: createBrowserHistory() });
  render(<RouterProvider router={router} />);
  await user.click(await screen.findByRole("link", { name: "Open troubleshooting" }));
  expect(await screen.findByRole("heading", { name: "Troubleshooting page" })).toBeInTheDocument();
  expect(router.state.location.pathname).toBe("/resources/r1/troubleshooting");
});

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { createMemoryHistory, createRootRoute, createRoute, createRouter, Outlet, RouterProvider } from "@tanstack/react-router";
import { act, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { ApiError, request } from "../api/client";
import { closeHelp, closePalette } from "../lib/command-palette";
import { setShortcutsEnabled } from "../lib/shortcuts";
import { CommandCenter, SearchButton } from "./CommandCenter";

const session = vi.hoisted(() => {
  const listeners = new Set<() => void>();
  type TestSession = { team_id: string; is_admin: boolean; teams: never[]; permissions: string[] };
  let value: TestSession = { team_id: "team-a", is_admin: true, teams: [], permissions: [] };
  return {
    get: () => value,
    set: (next: TestSession) => {
      value = next;
      listeners.forEach((listener) => listener());
    },
    subscribe: (listener: () => void) => {
      listeners.add(listener);
      return () => listeners.delete(listener);
    },
  };
});
vi.mock("../lib/session", async () => {
  const { useSyncExternalStore } = await import("react");
  return { useSession: () => useSyncExternalStore(session.subscribe, session.get) };
});
vi.mock("../api/client", async (importOriginal) => ({ ...(await importOriginal<typeof import("../api/client")>()), request: vi.fn() }));

HTMLDialogElement.prototype.showModal = function (this: HTMLDialogElement) {
  this.open = true;
};
HTMLDialogElement.prototype.close = function (this: HTMLDialogElement) {
  if (!this.open) return;
  this.open = false;
  this.dispatchEvent(new Event("close"));
};
Element.prototype.scrollIntoView = function () {};

beforeEach(() => {
  session.set({ team_id: "team-a", is_admin: true, teams: [], permissions: [] });
  setShortcutsEnabled(true);
  vi.mocked(request).mockImplementation((_method, path) => {
    const query = new URL(path, "http://cove.test").searchParams.get("q") ?? "";
    if (query === "fail") return Promise.reject(new ApiError(503, "Search unavailable; try again"));
    return Promise.resolve({
      limit: 40,
      items: [
        { kind: "workload", name: `${query || "suggested"}-api`, detail: "Shop", path: "/resources/r1" },
        { kind: "project", name: "Shop", detail: "", path: "/projects/p1" },
        { kind: "page", name: "Unsafe", detail: "", path: "https://example.com" },
        { kind: "page", name: "Nodes", detail: "Infrastructure", path: "/nodes" },
      ],
    });
  });
});

afterEach(() => {
  closePalette(false);
  closeHelp();
  setShortcutsEnabled(false);
  vi.clearAllMocks();
});

function renderApp() {
  const root = createRootRoute({
    component: () => (
      <>
        <SearchButton />
        <input aria-label="Notes" />
        <Outlet />
        <CommandCenter />
      </>
    ),
  });
  const page = (path: string, title: string) => createRoute({ getParentRoute: () => root, path, component: () => <h1>{title}</h1> });
  const router = createRouter({
    routeTree: root.addChildren([page("/dashboard", "Dashboard page"), page("/projects", "Projects page"), page("/resources/new", "New resource page"), page("/resources/$resourceId", "Resource page"), page("/nodes", "Nodes page")]),
    history: createMemoryHistory({ initialEntries: ["/dashboard"] }),
  });
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(
    <QueryClientProvider client={client}>
      <RouterProvider router={router} />
    </QueryClientProvider>,
  );
  return { router, client };
}

it("opens search with Ctrl+K, debounces the query and navigates to a validated result", async () => {
  const user = userEvent.setup();
  const { router } = renderApp();
  await screen.findByRole("heading", { name: "Dashboard page" });
  await user.keyboard("{Control>}k{/Control}");
  const input = await screen.findByRole("combobox", { name: "Search projects, resources, nodes and pages" });
  expect(input).toHaveFocus();
  expect(await screen.findByRole("option", { name: /suggested-api/ })).toBeInTheDocument();

  await user.type(input, "shop");
  expect(await screen.findByRole("option", { name: /shop-api/ })).toBeInTheDocument();
  expect(request).toHaveBeenLastCalledWith("GET", "/api/search?q=shop", undefined, expect.any(AbortSignal));
  expect(request).not.toHaveBeenCalledWith("GET", "/api/search?q=s", undefined, expect.anything());
  const list = screen.getByRole("listbox", { name: "Search results" });
  expect(within(list).queryByText("Unsafe")).not.toBeInTheDocument();
  expect(within(list).getAllByRole("option", { selected: true })).toHaveLength(1);
  expect(input).toHaveAttribute("aria-activedescendant", within(list).getByRole("option", { selected: true }).id);
  expect(within(list).getByRole("option", { selected: true })).toHaveTextContent("shop-api");

  await user.keyboard("{ArrowDown}");
  expect(within(list).getByRole("option", { name: /^Shop/ })).toHaveAttribute("aria-selected", "true");
  await user.keyboard("{ArrowUp}{Enter}");
  expect(await screen.findByRole("heading", { name: "Resource page" })).toBeInTheDocument();
  expect(router.state.location.pathname).toBe("/resources/r1");
  expect(screen.queryByRole("combobox")).not.toBeInTheDocument();
});

it("closes on Escape, clears its text and returns focus to the trigger", async () => {
  const user = userEvent.setup();
  renderApp();
  const trigger = await screen.findByRole("button", { name: /Search/ });
  await user.click(trigger);
  await user.type(screen.getByRole("combobox"), "shop");
  act(() => {
    screen.getByRole("combobox").closest("dialog")?.close();
  });
  expect(screen.queryByRole("combobox")).not.toBeInTheDocument();
  expect(trigger).toHaveFocus();
  await user.click(trigger);
  expect(screen.getByRole("combobox")).toHaveValue("");
});

it("shows loading, empty, error and length-limit states", async () => {
  const user = userEvent.setup();
  renderApp();
  await user.click(await screen.findByRole("button", { name: /Search/ }));
  const input = screen.getByRole("combobox");
  await user.type(input, "fail");
  expect(await screen.findByRole("alert")).toHaveTextContent("Search unavailable; try again");
  expect(screen.getByRole("button", { name: "Retry" })).toBeInTheDocument();

  vi.mocked(request).mockResolvedValue({ limit: 40, items: [] });
  await user.clear(input);
  await user.type(input, "zzz");
  expect(within(screen.getByRole("dialog", { name: "Search Cove" })).getByRole("status")).toHaveTextContent("Searching…");
  expect(await screen.findByText("No matches for “zzz”.")).toBeInTheDocument();

  vi.mocked(request).mockClear();
  await user.clear(input);
  await user.click(input);
  await user.paste("é".repeat(101));
  expect(within(screen.getByRole("dialog", { name: "Search Cove" })).getByRole("status")).toHaveTextContent("Search text can be at most 200 bytes.");
  expect(input).toHaveAttribute("aria-invalid", "true");
  await new Promise((resolve) => setTimeout(resolve, 250));
  expect(request).not.toHaveBeenCalledWith("GET", expect.stringContaining("%C3%A9"), undefined, expect.anything());
});

it("navigates with g chords and opens help with ?, but never while typing", async () => {
  const user = userEvent.setup();
  const { router } = renderApp();
  await screen.findByRole("heading", { name: "Dashboard page" });

  await user.click(screen.getByRole("textbox", { name: "Notes" }));
  await user.keyboard("gp?");
  await user.keyboard("{Control>}k{/Control}");
  expect(router.state.location.pathname).toBe("/dashboard");
  expect(screen.queryByRole("dialog")).not.toBeInTheDocument();

  (document.activeElement as HTMLElement).blur();
  await user.keyboard("gp");
  expect(await screen.findByRole("heading", { name: "Projects page" })).toBeInTheDocument();
  await user.keyboard("gn");
  expect(await screen.findByRole("heading", { name: "Nodes page" })).toBeInTheDocument();

  await user.keyboard("?");
  const help = await screen.findByRole("dialog", { name: "Keyboard shortcuts" });
  expect(within(help).getByText("Go to All resources").closest("tr")).toHaveTextContent(/^Go to All resourcesg then r$/);
});

it("keeps shortcuts off until people opt in, while Search and help buttons always work", async () => {
  setShortcutsEnabled(false);
  const user = userEvent.setup();
  const { router } = renderApp();
  await screen.findByRole("heading", { name: "Dashboard page" });
  await user.keyboard("gp?{Control>}k{/Control}");
  expect(router.state.location.pathname).toBe("/dashboard");
  expect(screen.queryByRole("dialog")).not.toBeInTheDocument();

  await user.click(screen.getByRole("button", { name: /^Search/ }));
  await user.type(screen.getByRole("combobox"), "shortcuts");
  await user.click(await screen.findByRole("option", { name: /Keyboard shortcuts/ }));
  const help = await screen.findByRole("dialog", { name: "Keyboard shortcuts" });
  const toggle = within(help).getByRole("switch", { name: "Use keyboard shortcuts" });
  expect(toggle).not.toBeChecked();
  await user.click(toggle);
  await user.click(within(help).getByRole("button", { name: "Close" }));
  expect(window.localStorage.getItem("cove.shortcuts")).toBe("on");

  await user.keyboard("gp");
  expect(await screen.findByRole("heading", { name: "Projects page" })).toBeInTheDocument();
  await user.click(screen.getByRole("button", { name: "Keyboard shortcuts" }));
  await user.click(await screen.findByRole("switch", { name: "Use keyboard shortcuts" }));
  expect(window.localStorage.getItem("cove.shortcuts")).toBeNull();
});

it("refreshes the view and opens New resource only with r r and n r, without queuing anything", async () => {
  const user = userEvent.setup();
  const { router, client } = renderApp();
  await screen.findByRole("heading", { name: "Dashboard page" });
  const invalidate = vi.spyOn(client, "invalidateQueries");
  await user.keyboard("rr");
  expect(invalidate).toHaveBeenCalledWith({ type: "active" });
  expect(await screen.findByText("Current view refreshed.")).toBeInTheDocument();

  act(() => session.set({ team_id: "team-a", is_admin: false, teams: [], permissions: [] }));
  await user.keyboard("nr");
  expect(router.state.location.pathname).toBe("/dashboard");
  act(() => session.set({ team_id: "team-a", is_admin: true, teams: [], permissions: [] }));
  await user.keyboard("nr");
  expect(await screen.findByRole("heading", { name: "New resource page" })).toBeInTheDocument();
  expect(request).not.toHaveBeenCalledWith("POST", expect.anything(), expect.anything());
});

it("discards an open palette when the team changes and keys results by team", async () => {
  const user = userEvent.setup();
  const { client } = renderApp();
  await user.click(await screen.findByRole("button", { name: /Search/ }));
  await user.type(screen.getByRole("combobox"), "shop");
  await screen.findByRole("option", { name: /shop-api/ });
  expect(client.getQueryCache().find({ queryKey: ["search", "team-a", "shop"] })).toBeDefined();

  act(() => session.set({ team_id: "team-b", is_admin: true, teams: [], permissions: [] }));
  await waitFor(() => expect(screen.queryByRole("combobox")).not.toBeInTheDocument());
  await user.click(screen.getByRole("button", { name: /Search/ }));
  expect(screen.getByRole("combobox")).toHaveValue("");
  await screen.findByRole("option", { name: /suggested-api/ });
  expect(client.getQueryCache().find({ queryKey: ["search", "team-b", ""] })).toBeDefined();
});

it("does not open a result from the previous query while the new query is pending", async () => {
  const user = userEvent.setup();
  const { router } = renderApp();
  await user.click(await screen.findByRole("button", { name: /Search/ }));
  const input = screen.getByRole("combobox");
  expect(await screen.findByRole("option", { name: /suggested-api/ })).toBeInTheDocument();

  await user.type(input, "nodes{Enter}");
  expect(screen.queryByRole("option", { name: /suggested-api/ })).not.toBeInTheDocument();
  expect(input).toHaveAccessibleDescription("Searching…");
  expect(router.state.location.pathname).toBe("/dashboard");
  expect(input).toBeInTheDocument();

  expect(await screen.findByRole("option", { name: /nodes-api/ })).toHaveAttribute("aria-selected", "true");
  await user.keyboard("{Enter}");
  expect(await screen.findByRole("heading", { name: "Resource page" })).toBeInTheDocument();
});

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import type { ReactNode } from "react";
import { afterEach, expect, it, vi } from "vitest";
import { request } from "../api/client";
import { ResourceBrowserPage } from "../routes/resource-browser";
import { ActivityFeed } from "./ActivityFeed";
import { ClientPagination } from "./ListPagination";
import { SetupChecklist } from "./SetupChecklist";

vi.mock("@tanstack/react-router", () => ({
  Link: ({ children, className, "aria-label": label }: { children: ReactNode; className?: string; "aria-label"?: string }) => (
    <a href="#link" className={className} aria-label={label}>
      {children}
    </a>
  ),
}));
vi.mock("../api/client", () => ({ request: vi.fn(), errorMessage: (error: unknown) => (error instanceof Error ? error.message : String(error)) }));
afterEach(() => vi.clearAllMocks());

HTMLDialogElement.prototype.showModal = function (this: HTMLDialogElement) {
  this.open = true;
};
HTMLDialogElement.prototype.close = function (this: HTMLDialogElement) {
  this.open = false;
};

function renderWithClient(children: ReactNode) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(<QueryClientProvider client={client}>{children}</QueryClientProvider>);
}

const steps = [
  { key: "account", name: "Sign in to Cove", complete: true, path: "/account" },
  { key: "node", name: "Enroll and verify a worker", complete: false, path: "/nodes" },
  { key: "project", name: "Create a project", complete: false, path: "/projects" },
];

it("skips and reopens the setup checklist and explains who can enroll a worker", async () => {
  let dismissed = false;
  vi.mocked(request).mockImplementation((method: string, path: string, body?: unknown) => {
    if (path === "/api/onboarding") {
      if (method === "PUT") dismissed = (body as { dismissed: boolean }).dismissed;
      return Promise.resolve({ dismissed, complete: false, can_enroll_node: false, steps });
    }
    return Promise.reject(new Error(`unexpected ${path}`));
  });
  renderWithClient(<SetupChecklist />);
  expect(await screen.findByText("1 of 3 steps done. Progress follows what exists in this team.")).toBeInTheDocument();
  expect(screen.getByText(/Only an instance administrator can enroll workers/)).toBeInTheDocument();
  expect(screen.queryByRole("link", { name: /Enroll and verify a worker/ })).not.toBeInTheDocument();
  expect(screen.getByRole("link", { name: "Open: Create a project" })).toBeInTheDocument();

  fireEvent.click(screen.getByRole("button", { name: "Skip for now" }));
  await screen.findByRole("button", { name: "Show setup checklist" });
  expect(request).toHaveBeenCalledWith("PUT", "/api/onboarding", { dismissed: true });

  fireEvent.click(screen.getByRole("button", { name: "Show setup checklist" }));
  await screen.findByRole("list", { name: "Setup steps" });
  expect(request).toHaveBeenCalledWith("PUT", "/api/onboarding", { dismissed: false });
});

it("shows activity from the API and its empty state", async () => {
  vi.mocked(request).mockResolvedValueOnce({
    items: [
      { kind: "operation", id: "o1", action: "deploy", state: "succeeded", name: "web", path: "/operations/o1", created_at: "2026-11-22T10:00:00Z" },
      { kind: "configuration", id: "9", action: "PUT /api/workloads/{id}", state: "failed", name: "api", path: "/resources/w2", created_at: "2026-11-22T09:00:00Z" },
    ],
  });
  const { unmount } = renderWithClient(<ActivityFeed />);
  const list = await screen.findByRole("list", { name: "Activity" });
  expect(within(list).getAllByRole("listitem")).toHaveLength(2);
  expect(within(list).getByText("Deploy")).toBeInTheDocument();
  expect(within(list).getByText("Configuration change")).toBeInTheDocument();
  unmount();

  vi.mocked(request).mockResolvedValueOnce({ items: [] });
  renderWithClient(<ActivityFeed />);
  expect(await screen.findByText("No activity yet")).toBeInTheDocument();
});

it("pages through loaded rows and returns to the first page when filters change", () => {
  const rows = Array.from({ length: 30 }, (_, index) => `row-${index + 1}`);
  const view = (filter: string) => (
    <ClientPagination rows={rows} pageSize={25} resetKey={filter} label="Row pages" noun="rows">
      {(visible) => <p>{visible.join(",")}</p>}
    </ClientPagination>
  );
  const { rerender } = render(view("a"));
  expect(screen.getByText("Showing 1-25 of 30 rows")).toBeInTheDocument();
  fireEvent.click(screen.getByRole("button", { name: "Next" }));
  expect(screen.getByText("row-26,row-27,row-28,row-29,row-30")).toBeInTheDocument();
  rerender(view("b"));
  expect(screen.getByText("Showing 1-25 of 30 rows")).toBeInTheDocument();
});

const resource = (id: string, permissions: string[], kind = "service") => ({
  id,
  name: id,
  node_id: "n1",
  node: "worker",
  kind,
  project_id: "p1",
  project: "Shop",
  environment_id: "e1",
  environment: "production",
  permissions,
  tags: id === "web" ? ["frontend"] : [],
  pinned: false,
});

it("confirms a bulk deploy, reports each result honestly and clears the selection", async () => {
  vi.mocked(request).mockImplementation((method: string, path: string) => {
    if (path.startsWith("/api/resource-browser")) {
      return Promise.resolve({ items: [resource("web", ["read", "operate", "edit"]), resource("api", ["read", "operate"]), resource("docs", ["read"])], page: 1, page_size: 25, total: 3, has_more: false });
    }
    if (path === "/api/projects" || path === "/api/nodes") return Promise.resolve([]);
    if (path === "/api/pins") return Promise.resolve({ items: [] });
    if (method === "POST" && path === "/api/bulk-actions") {
      return Promise.resolve({ items: [{ id: "web", status: 202, operation_id: "op1" }, { id: "api", status: 409, error: "An operation is already running" }] });
    }
    return Promise.reject(new Error(`unexpected ${method} ${path}`));
  });
  renderWithClient(<ResourceBrowserPage />);
  await screen.findByRole("link", { name: "web" });
  expect(screen.getByRole("checkbox", { name: "docs: you cannot run operations on this resource" })).toBeDisabled();

  fireEvent.click(screen.getByRole("checkbox", { name: "Select every resource on this page that you can operate" }));
  expect(screen.getByText("2 resources selected")).toBeInTheDocument();
  fireEvent.click(screen.getByRole("button", { name: "Review deploy" }));
  const dialog = screen.getByRole("dialog", { name: "Deploy 2 resources?" });
  expect(within(dialog).getByText("Deploy without a per-resource review")).toBeInTheDocument();
  fireEvent.click(within(dialog).getByRole("button", { name: "Deploy 2 resources" }));

  const results = await screen.findByRole("list", { name: "Bulk action results" });
  expect(request).toHaveBeenCalledWith("POST", "/api/bulk-actions", { ids: ["web", "api"], action: "deploy", acknowledge: true });
  expect(within(results).getByRole("link", { name: "View operation" })).toBeInTheDocument();
  expect(within(results).getByText("Not queued (409): An operation is already running")).toBeInTheDocument();
  expect(screen.getByText(/1 operation queued, 1 not queued/)).toBeInTheDocument();
  expect(screen.queryByText(/selected$/)).not.toBeInTheDocument();
});

it("sends an optional reason with a bulk deploy and refuses an invalid one", async () => {
  vi.mocked(request).mockImplementation((method: string, path: string) => {
    if (path.startsWith("/api/resource-browser")) return Promise.resolve({ items: [resource("web", ["read", "operate", "edit"])], page: 1, page_size: 25, total: 1, has_more: false });
    if (path === "/api/projects" || path === "/api/nodes") return Promise.resolve([]);
    if (path === "/api/pins") return Promise.resolve({ items: [] });
    if (method === "POST" && path === "/api/bulk-actions") return Promise.resolve({ items: [{ id: "web", status: 202, operation_id: "op1" }] });
    return Promise.reject(new Error(`unexpected ${method} ${path}`));
  });
  renderWithClient(<ResourceBrowserPage />);
  fireEvent.click(await screen.findByRole("checkbox", { name: "Select web" }));
  fireEvent.click(screen.getByRole("button", { name: "Review deploy" }));
  const dialog = screen.getByRole("dialog", { name: "Deploy 1 resource?" });
  const reason = within(dialog).getByLabelText(/Reason/);
  fireEvent.change(reason, { target: { value: "bad reason" } });
  expect(reason).toHaveAttribute("aria-invalid", "true");
  fireEvent.click(within(dialog).getByRole("button", { name: "Deploy 1 resource" }));
  expect(request).not.toHaveBeenCalledWith("POST", "/api/bulk-actions", expect.anything());

  fireEvent.change(reason, { target: { value: "  Weekly patch rollout  " } });
  fireEvent.click(within(dialog).getByRole("button", { name: "Deploy 1 resource" }));
  await screen.findByRole("list", { name: "Bulk action results" });
  expect(request).toHaveBeenCalledWith("POST", "/api/bulk-actions", { ids: ["web"], action: "deploy", acknowledge: true, reason: "Weekly patch rollout" });
});

it("drops the selection when a filter changes", async () => {
  vi.mocked(request).mockImplementation((_method: string, path: string) => {
    if (path.startsWith("/api/resource-browser")) return Promise.resolve({ items: [resource("web", ["read", "operate", "edit"])], page: 1, page_size: 25, total: 1, has_more: false });
    if (path === "/api/projects" || path === "/api/nodes") return Promise.resolve([]);
    if (path === "/api/pins") return Promise.resolve({ items: [] });
    return Promise.reject(new Error(`unexpected ${path}`));
  });
  renderWithClient(<ResourceBrowserPage />);
  fireEvent.click(await screen.findByRole("checkbox", { name: "Select web" }));
  expect(screen.getByText("1 resource selected")).toBeInTheDocument();
  fireEvent.click(screen.getByRole("checkbox", { name: "Pinned only" }));
  await waitFor(() => expect(screen.queryByText("1 resource selected")).not.toBeInTheDocument());
  expect(request).toHaveBeenCalledWith("GET", expect.stringContaining("pinned=true"), undefined, expect.anything());
});

const browserPage = (items: ReturnType<typeof resource>[]) => ({ items, page: 1, page_size: 25, total: items.length, has_more: false });

function mockBrowser(browse: (path: string) => Promise<unknown>, bulk?: () => Promise<unknown>) {
  vi.mocked(request).mockImplementation((method: string, path: string) => {
    if (path.startsWith("/api/resource-browser")) return browse(path);
    if (path === "/api/projects" || path === "/api/nodes") return Promise.resolve([]);
    if (path === "/api/pins") return Promise.resolve({ items: [] });
    if (method === "POST" && path === "/api/bulk-actions" && bulk) return bulk();
    return Promise.reject(new Error(`unexpected ${method} ${path}`));
  });
}

it("keeps previous rows unselectable until the new filter's rows arrive", async () => {
  let release: (value: unknown) => void = () => {};
  mockBrowser((path) => (path.includes("pinned=true") ? new Promise((resolve) => (release = resolve)) : Promise.resolve(browserPage([resource("web", ["read", "operate"])]))));
  renderWithClient(<ResourceBrowserPage />);
  fireEvent.click(await screen.findByRole("checkbox", { name: "Select web" }));
  fireEvent.click(screen.getByRole("checkbox", { name: "Pinned only" }));

  await waitFor(() => expect(screen.getByRole("checkbox", { name: "Select web" })).toBeDisabled());
  expect(screen.getByRole("checkbox", { name: "Select every resource on this page that you can operate" })).toBeDisabled();
  expect(screen.queryByRole("region", { name: "Bulk actions" })).not.toBeInTheDocument();

  release(browserPage([resource("api", ["read", "operate"])]));
  expect(await screen.findByRole("checkbox", { name: "Select api" })).toBeEnabled();
});

it("disables review while typed search text has not been applied yet", async () => {
  mockBrowser(() => Promise.resolve(browserPage([resource("web", ["read", "operate"])])));
  renderWithClient(<ResourceBrowserPage />);
  fireEvent.click(await screen.findByRole("checkbox", { name: "Select web" }));
  expect(screen.getByRole("button", { name: "Review deploy" })).toBeEnabled();

  fireEvent.change(screen.getByRole("searchbox", { name: "Search by name" }), { target: { value: "api" } });
  expect(screen.getByRole("button", { name: "Review deploy" })).toBeDisabled();
  expect(screen.getByRole("checkbox", { name: "Select web" })).toBeDisabled();
  await waitFor(() => expect(request).toHaveBeenCalledWith("GET", expect.stringContaining("q=api"), undefined, expect.anything()));
  await waitFor(() => expect(screen.queryByRole("region", { name: "Bulk actions" })).not.toBeInTheDocument());
});

it("counts and confirms only selected resources that are still listed after a refresh", async () => {
  let rows = [resource("web", ["read", "operate"]), resource("api", ["read", "operate"])];
  mockBrowser(
    () => Promise.resolve(browserPage(rows)),
    () => Promise.resolve({ items: [{ id: "api", status: 202, operation_id: "op1" }] }),
  );
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(
    <QueryClientProvider client={client}>
      <ResourceBrowserPage />
    </QueryClientProvider>,
  );
  fireEvent.click(await screen.findByRole("checkbox", { name: "Select every resource on this page that you can operate" }));
  expect(screen.getByText("2 resources selected")).toBeInTheDocument();

  rows = [resource("api", ["read", "operate"])];
  await act(() => client.invalidateQueries({ queryKey: ["resource-browser"] }));
  await waitFor(() => expect(screen.queryByRole("link", { name: "web" })).not.toBeInTheDocument());
  expect(screen.getByText("1 resource selected")).toBeInTheDocument();

  fireEvent.click(screen.getByRole("button", { name: "Review deploy" }));
  const dialog = screen.getByRole("dialog", { name: "Deploy 1 resource?" });
  fireEvent.click(within(dialog).getByRole("button", { name: "Deploy 1 resource" }));
  await screen.findByRole("list", { name: "Bulk action results" });
  expect(request).toHaveBeenCalledWith("POST", "/api/bulk-actions", { ids: ["api"], action: "deploy", acknowledge: true });
});

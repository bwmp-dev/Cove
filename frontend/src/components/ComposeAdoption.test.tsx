import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { ReactNode } from "react";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import type { Adoption, AdoptionDiscovery, AdoptionSummary } from "../api/adoptions";
import { ApiError, request } from "../api/client";
import type { Project } from "../api/projects";
import type { Node } from "../api/types";
import { ComposeAdoption, reviewFromHash } from "./ComposeAdoption";

vi.mock("../api/client", async (original) => ({ ...(await original<typeof import("../api/client")>()), request: vi.fn() }));
vi.mock("@tanstack/react-router", () => ({
  Link: ({ children, params, to, className }: { children: ReactNode; params?: Record<string, string>; to: string; className?: string }) => (
    <a href={to.replace("$resourceId", params?.resourceId ?? "")} className={className}>
      {children}
    </a>
  ),
}));
beforeEach(() => {
  vi.stubGlobal("ResizeObserver", class { observe = vi.fn(); disconnect = vi.fn(); });
  Element.prototype.scrollIntoView = vi.fn();
});
afterEach(() => {
  vi.clearAllMocks();
  vi.unstubAllGlobals();
});
HTMLDialogElement.prototype.showModal = function (this: HTMLDialogElement) {
  this.open = true;
};
HTMLDialogElement.prototype.close = function (this: HTMLDialogElement) {
  this.open = false;
};

const capabilities = { protocol: 1, os: "linux", arch: "amd64", docker: "27", compose: "2", ready: true };
const node: Node = { id: "n1", name: "edge-1", address: "", port: 22, state: "ready", mode: "ssh", last_checked_at: null, capabilities: { ...capabilities, adoption: true } };
const projects: Project[] = [
  { id: "p", name: "Shop", permissions: ["read", "edit"], environments: [{ id: "e1", name: "Production", permissions: ["read", "edit"] }, { id: "pv", name: "pr-4", preview_id: "x", permissions: ["read"] }] },
  { id: "ro", name: "Read only", permissions: ["read"], environments: [] },
];
const discovery: AdoptionDiscovery = {
  projects: [
    { project: "shop", services: ["web", "worker"], containers: 2, blockers: [] },
    { project: "blog", services: ["app"], containers: 1, blockers: null },
    { project: "cove-system", services: ["proxy"], containers: 1, blockers: ["Cove system projects cannot be adopted."] },
  ],
  warnings: ["Adoption records Cove ownership without restarting containers."],
};
const resources = [
  { project: "shop", key: "shop-2", name: "shop", resource_id: "w1", complete: false },
  { project: "blog", key: "blog", name: "Blog", resource_id: "w2", complete: false },
];
const planned: Adoption = {
  id: "a1",
  status: "planned",
  project_id: "p",
  environment_id: "e1",
  resources,
  warnings: ["You must pause the source manager's automation yourself."],
  changes: [{ resource: "shop-2", action: "create", fields: ["compose", "environment"], diffs: null }],
};

function setup(handlers: Record<string, (body?: unknown) => Promise<unknown>> = {}, props: Partial<Parameters<typeof ComposeAdoption>[0]> = {}) {
  vi.mocked(request).mockImplementation((method: string, path: string, body?: unknown) => {
    const key = `${method} ${path}`;
    if (handlers[key]) return handlers[key](body);
    if (key === "GET /api/projects") return Promise.resolve(projects);
    if (key === "GET /api/projects/p/configuration") return Promise.resolve({ version: "v1", manifest: { api_version: "cove/v1", project: "Shop", resources: [{ key: "shop", configuration: { name: "shop" } }] }, resource_ids: {} });
    if (key === "GET /api/nodes/n1/adoptions") return Promise.resolve([]);
    if (key === "POST /api/nodes/n1/adoptions/discover") return Promise.resolve(discovery);
    if (key === "POST /api/nodes/n1/adoptions") return Promise.resolve(planned);
    if (key === "POST /api/nodes/n1/adoptions/a1/adopt") return Promise.resolve({ ...planned, status: "complete", resources: resources.map((item) => ({ ...item, complete: true })) });
    return Promise.reject(new Error(key));
  });
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(
    <QueryClientProvider client={client}>
      <ComposeAdoption node={node} initialReview={null} {...props} />
    </QueryClientProvider>,
  );
}

async function choose(user: ReturnType<typeof userEvent.setup>, label: string, option: string) {
  await user.click(screen.getByRole("combobox", { name: label }));
  await user.click(await screen.findByRole("option", { name: option }));
}

it("scans, bulk maps, reviews and adopts only after both confirmations", async () => {
  const user = userEvent.setup();
  setup();
  expect(await screen.findByText(/No adoption reviews/)).toBeInTheDocument();
  expect(request).not.toHaveBeenCalledWith("POST", "/api/nodes/n1/adoptions/discover", expect.anything());

  await user.click(screen.getByRole("button", { name: "Scan node" }));
  const list = await screen.findByRole("list", { name: "Discovered Compose projects" });
  expect(screen.getByText("Adoption records Cove ownership without restarting containers.")).toBeInTheDocument();
  expect(within(list).getByText("Cove system projects cannot be adopted.")).toBeInTheDocument();
  expect(within(list).queryByRole("checkbox", { name: "cove-system" })).not.toBeInTheDocument();

  await user.click(screen.getByRole("button", { name: "Review adoption" }));
  expect(screen.getByText("Choose at least one Compose project without blockers.")).toBeInTheDocument();
  expect(screen.getByText("Choose a destination project.")).toBeInTheDocument();

  await choose(user, "Project", "Shop");
  await user.click(screen.getByRole("combobox", { name: "Environment" }));
  expect(screen.queryByRole("option", { name: "pr-4" })).not.toBeInTheDocument();
  await user.click(screen.getByRole("option", { name: "Production" }));
  await waitFor(() => expect(request).toHaveBeenCalledWith("GET", "/api/projects/p/configuration", undefined, expect.anything()));
  await user.click(within(list).getByRole("checkbox", { name: "shop" }));
  await user.click(within(list).getByRole("checkbox", { name: "blog" }));
  const keys = within(list).getAllByLabelText("Resource key");
  expect(keys[0]).toHaveValue("shop-2");
  expect(keys[1]).toHaveValue("blog");
  await user.type(within(list).getAllByLabelText(/^Name/)[1]!, "Blog");
  await user.click(screen.getByRole("button", { name: "Review adoption" }));

  expect(await screen.findByRole("list", { name: "Adoption configuration changes" })).toBeInTheDocument();
  expect(request).toHaveBeenCalledWith("POST", "/api/nodes/n1/adoptions", {
    project_id: "p",
    environment_id: "e1",
    projects: [
      { project: "shop", key: "shop-2", name: "" },
      { project: "blog", key: "blog", name: "Blog" },
    ],
  });
  expect(screen.queryByRole("list", { name: "Discovered Compose projects" })).not.toBeInTheDocument();
  expect(screen.getByText("Destination: Shop / Production")).toBeInTheDocument();
  expect(screen.getByText("You must pause the source manager's automation yourself.")).toBeInTheDocument();

  const adoptButton = screen.getByRole("button", { name: "Adopt without restarting" });
  expect(adoptButton).toBeDisabled();
  await user.click(screen.getByRole("checkbox", { name: /reviewed the resources and warnings/ }));
  expect(adoptButton).toBeDisabled();
  await user.click(screen.getByRole("checkbox", { name: /paused the source manager/ }));
  await user.click(adoptButton);

  expect(await screen.findByText("Adopted without restarting")).toBeInTheDocument();
  expect(request).toHaveBeenCalledWith("POST", "/api/nodes/n1/adoptions/a1/adopt", { acknowledge: true, manager_paused: true });
  expect(screen.getByRole("link", { name: "Blog" })).toHaveAttribute("href", "/resources/w2");
  expect(screen.queryByRole("button", { name: /Adopt|Retry/ })).not.toBeInTheDocument();
});

it("resumes a pending adoption with the same id and refreshes only on request", async () => {
  const user = userEvent.setup();
  const applying: Adoption = { ...planned, status: "applying", resources: [{ ...resources[0]!, complete: true }, resources[1]!] };
  let attempts = 0;
  let current = applying;
  const refresh = vi.fn(() => Promise.resolve(applying));
  setup(
    {
      "GET /api/nodes/n1/adoptions": () => Promise.resolve([{ id: "a1", project_id: "p", environment_id: "e1", status: "applying", created_at: "2026-11-22T08:00:00Z" }] satisfies AdoptionSummary[]),
      "GET /api/nodes/n1/adoptions/a1": () => Promise.resolve(current),
      "POST /api/nodes/n1/adoptions/a1/refresh": refresh,
      "POST /api/nodes/n1/adoptions/a1/adopt": () => (attempts++ === 0 ? Promise.reject(new ApiError(502, "adoption is pending; containers were not restarted")) : Promise.resolve((current = { ...applying, status: "complete" }))),
    },
    { initialReview: "a1" },
  );
  expect(await screen.findByText("Adoption is pending")).toBeInTheDocument();
  expect(screen.getByText("1 adoption pending")).toBeInTheDocument();
  expect(screen.getByText(/1 of 2 resources are adopted/)).toBeInTheDocument();
  expect(within(screen.getByRole("list", { name: "Adoption configuration changes" })).getByText("shop-2")).toBeInTheDocument();
  expect(refresh).not.toHaveBeenCalled();
  expect(screen.getByRole("link", { name: "shop" })).toHaveAttribute("href", "/resources/w1");

  await user.click(screen.getByRole("checkbox", { name: /reviewed the resources/ }));
  await user.click(screen.getByRole("checkbox", { name: /paused the source manager/ }));
  await user.click(screen.getByRole("button", { name: "Retry adoption" }));
  expect(await screen.findByText("Adoption did not finish")).toBeInTheDocument();
  expect(screen.getByText(/retry this same adoption/)).toBeInTheDocument();
  expect(screen.getByRole("checkbox", { name: /reviewed the resources/ })).not.toBeChecked();

  await user.click(screen.getByRole("checkbox", { name: /reviewed the resources/ }));
  await user.click(screen.getByRole("button", { name: "Refresh source review" }));
  expect(await screen.findByText("Source review refreshed")).toBeInTheDocument();
  expect(refresh).toHaveBeenCalledTimes(1);
  expect(screen.getByRole("checkbox", { name: /reviewed the resources/ })).not.toBeChecked();

  await user.click(screen.getByRole("checkbox", { name: /reviewed the resources/ }));
  await user.click(screen.getByRole("checkbox", { name: /paused the source manager/ }));
  await user.click(screen.getByRole("button", { name: "Retry adoption" }));
  expect(await screen.findByText("Adopted without restarting")).toBeInTheDocument();
  expect(vi.mocked(request).mock.calls.filter(([method, path]) => method === "POST" && path === "/api/nodes/n1/adoptions")).toHaveLength(0);
  expect(vi.mocked(request).mock.calls.filter(([, path]) => path === "/api/nodes/n1/adoptions/a1/adopt")).toHaveLength(2);
});

it("deletes only unstarted reviews after confirmation", async () => {
  const user = userEvent.setup();
  const remove = vi.fn(() => Promise.resolve({ ok: true }));
  setup({
    "GET /api/nodes/n1/adoptions": () =>
      Promise.resolve([
        { id: "a1", project_id: "p", environment_id: "", status: "planned", created_at: new Date().toISOString() },
        { id: "a2", project_id: "p", environment_id: "", status: "complete", created_at: "2026-11-20T08:00:00Z" },
      ] satisfies AdoptionSummary[]),
    "DELETE /api/nodes/n1/adoptions/a1": remove,
  });
  const list = await screen.findByRole("list", { name: "Adoption reviews" });
  expect(within(list).getAllByRole("button", { name: "Delete" })).toHaveLength(1);
  expect(within(list).getByText("Reviewed, not started")).toBeInTheDocument();
  expect(within(list).getByText(/^Expires/)).toBeInTheDocument();
  await user.click(within(list).getByRole("button", { name: "Delete" }));
  await user.click(screen.getByRole("button", { name: "Delete review" }));
  await waitFor(() => expect(remove).toHaveBeenCalled());
});

it("asks for a worker upgrade without calling adoption endpoints", async () => {
  setup({}, { node: { ...node, capabilities: capabilities } });
  expect(await screen.findByText("Upgrade the worker to adopt Compose projects")).toBeInTheDocument();
  expect(screen.queryByRole("button", { name: "Scan node" })).not.toBeInTheDocument();
  expect(request).not.toHaveBeenCalledWith("GET", "/api/nodes/n1/adoptions", undefined, expect.anything());
});

it("disables scanning while the node is offline", async () => {
  setup({}, { node: { ...node, state: "unreachable" } });
  expect(await screen.findByText("The node is not ready")).toBeInTheDocument();
  expect(screen.getByRole("button", { name: "Scan node" })).toBeDisabled();
});

it("reports an empty scan and refused scans", async () => {
  const user = userEvent.setup();
  let attempts = 0;
  setup({ "POST /api/nodes/n1/adoptions/discover": () => (attempts++ === 0 ? Promise.reject(new ApiError(502, "Compose discovery unavailable")) : Promise.resolve({ projects: null, warnings: [] })) });
  await user.click(await screen.findByRole("button", { name: "Scan node" }));
  expect(await screen.findByText("Compose discovery unavailable")).toBeInTheDocument();
  await user.click(screen.getByRole("button", { name: "Scan node" }));
  expect(await screen.findByText("No unmanaged Compose projects were found on this node.")).toBeInTheDocument();
});

it("reads review ids from node page links", () => {
  expect(reviewFromHash("adoption-a1")).toBe("a1");
  expect(reviewFromHash("adoption-")).toBeNull();
  expect(reviewFromHash("other")).toBeNull();
});

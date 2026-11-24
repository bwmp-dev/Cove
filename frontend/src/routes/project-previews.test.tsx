import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import type { ReactNode } from "react";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { request } from "../api/client";
import { previewKeys, type EnvironmentPreview, type PreviewList, type PreviewResource, type RecipeView } from "../api/previews";
import type { Project } from "../api/projects";
import { PreviewStatus, ProjectPreviews } from "./project-previews";

vi.mock("../api/client", async (original) => ({ ...(await original<typeof import("../api/client")>()), request: vi.fn() }));
vi.mock("@tanstack/react-router", () => ({
  getRouteApi: () => ({ useParams: () => ({ projectId: "p" }) }),
  Link: ({ children }: { children: ReactNode }) => <a href="/">{children}</a>,
  useBlocker: () => ({ status: "idle" }),
}));
beforeEach(() => vi.stubGlobal("ResizeObserver", class { observe = vi.fn(); disconnect = vi.fn(); }));
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

// Fixtures only; the revision and digests are made up.
const revision = "0123456789abcdef0123456789abcdef01234567";
const digest = `registry.example.test/team/web@sha256:${"a".repeat(64)}`;
const project: Project = { id: "p", name: "Shop", permissions: ["read", "operate", "edit"], environments: [{ id: "prod", name: "Production", permissions: ["edit"] }] };

const view: RecipeView = {
  saved: true,
  recipe: { enabled: false, base_environment_id: "prod", branch_pattern: "staging/*", repositories: [], domain_template: "", access: "private", ttl_hours: 72, max_previews: 3, require_approval: true, resources: {} },
  plan: {
    blocked: false,
    dns: "",
    example_hostnames: [],
    repositories: [],
    resources: [{ id: "web", name: "web", kind: "service", included: true, follows_branch: false, source_kind: "compose", services: [], volumes: [], variables: [], dependencies: [], routes: [], blockers: [], notes: [] }],
  },
};

const baseResource: PreviewResource = {
  base_id: "web",
  resource_id: "pw",
  name: "web",
  kind: "service",
  source: "ci",
  revision,
  baseline_revision: "",
  baseline_release: "r1",
  baseline_deployed_at: null,
  deployed: true,
  data_state: "none",
  snapshot_at: null,
  data_report: null,
  routes: [],
  published: false,
  operation: "",
  ci_images: { web: digest, worker: `registry.example.test/team/worker@sha256:${"b".repeat(64)}` },
};

const preview: EnvironmentPreview = {
  id: "pv1",
  project_id: "p",
  environment_id: "env1",
  base_environment_id: "prod",
  suffix: "pr-42",
  slug: "pr-42",
  state: "ready",
  stage: "ready",
  detail: "",
  pinned: false,
  access: "public",
  delete_reason: "",
  ci: true,
  last_activity_at: "2026-11-24T00:00:00Z",
  expires_at: null,
  created_at: "2026-11-24T00:00:00Z",
  branches: [],
  permissions: ["read"],
  resources: [baseResource, { ...baseResource, base_id: "api", resource_id: "pa", name: "api", source: "baseline", revision: "fedcba9876543210fedcba9876543210fedcba98", ci_images: undefined }],
};

const images = { configuration: null, resources: [], endpoint: "/hooks/preview-images/p", can_manage: true, setup_reason: "Enable the preview recipe first.", max_resources: 16, max_preview_keys: 1024, max_deliveries: 4096, used_preview_keys: 0, retained_deliveries: 0 };

function serve() {
  vi.mocked(request).mockImplementation((method: string, path: string) => {
    if (method === "PUT") return Promise.resolve({ ok: true });
    if (path === "/api/projects") return Promise.resolve([project]);
    if (path === "/api/projects/p/environment-previews") return Promise.resolve({ previews: [preview], waiting: [] });
    if (path === "/api/projects/p/preview-images") return Promise.resolve(images);
    if (path.startsWith("/api/projects/p/preview-recipe")) return Promise.resolve(view);
    if (path === "/api/environment-previews/pv1") return Promise.resolve(preview);
    return Promise.reject(new Error(`unexpected ${method} ${path}`));
  });
}

function renderWithClient(children: ReactNode) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(<QueryClientProvider client={client}>{children}</QueryClientProvider>);
  return client;
}

const reads = (target: string) => vi.mocked(request).mock.calls.filter(([method, path]) => method === "GET" && path === target).length;
const imageReads = () => vi.mocked(request).mock.calls.filter(([method, path]) => method === "GET" && path === "/api/projects/p/preview-images").length;

it("labels CI previews and keeps the branch guard on manual creation", async () => {
  serve();
  renderWithClient(<ProjectPreviews />);
  const list = await screen.findByRole("list", { name: "Previews in Shop" });
  expect(within(list).getByText("CI-built images")).toBeInTheDocument();
  expect(screen.getByText(/repository branches and CI image submissions each create or update an isolated preview/)).toBeInTheDocument();
  expect(screen.queryByRole("button", { name: /Submit|Deploy/ })).not.toBeInTheDocument();

  fireEvent.click(screen.getByRole("button", { name: "New preview" }));
  const dialog = screen.getByRole("dialog", { name: "New preview" });
  expect(await within(dialog).findByText("Branch previews are not set up")).toBeInTheDocument();
  expect(dialog).toHaveTextContent("needs an enabled, saved recipe with at least one selected repository");
  fireEvent.change(within(dialog).getByLabelText("Branch"), { target: { value: "checkout" } });
  expect(within(dialog).getByRole("button", { name: "Create preview" })).toBeDisabled();
});

it("saves an enabled recipe without repositories and rechecks the CI image scope", async () => {
  serve();
  renderWithClient(<ProjectPreviews />);
  expect(await screen.findByText(/Previews can still come from CI-built images/)).toBeInTheDocument();
  await waitFor(() => expect(imageReads()).toBe(1));
  fireEvent.click(screen.getByRole("switch", { name: "Create previews" }));
  fireEvent.click(screen.getByRole("button", { name: "Save recipe" }));
  await waitFor(() => expect(request).toHaveBeenCalledWith("PUT", "/api/projects/p/preview-recipe", expect.objectContaining({ enabled: true, repositories: [] })));
  await waitFor(() => expect(imageReads()).toBe(2));
});

it("shows the CI revision and each service's immutable image on a CI preview", async () => {
  serve();
  renderWithClient(<PreviewStatus previewId="pv1" />);
  expect(await screen.findByText("Source")).toBeInTheDocument();
  expect(screen.queryByText("Branches")).not.toBeInTheDocument();
  const table = screen.getByRole("table");
  expect(within(table).getAllByText(revision).length).toBeGreaterThan(0);
  expect(within(table).getAllByText(digest, { exact: false }).length).toBeGreaterThan(0);
  expect(table).toHaveTextContent(`b`.repeat(64));
  expect(within(table).getAllByText(/^Production/).length).toBeGreaterThan(0);
});

it("refreshes the project hierarchy only when polling shows a preview environment appear or go away", async () => {
  serve();
  let list: PreviewList | Error = { previews: [{ ...preview, state: "creating", stage: "deploy", environment_id: "" }], waiting: [] };
  const base = vi.mocked(request).getMockImplementation()!;
  vi.mocked(request).mockImplementation((method: string, path: string, ...rest: unknown[]) => {
    if (path === "/api/projects/p/environment-previews") return list instanceof Error ? Promise.reject(list) : Promise.resolve(list);
    return base(method, path, ...rest);
  });
  const client = renderWithClient(<ProjectPreviews />);
  await screen.findByRole("list", { name: "Previews in Shop" });
  const poll = async (next: PreviewList | Error) => {
    list = next;
    await client.refetchQueries({ queryKey: previewKeys.list("p") });
  };
  const projectReads = reads("/api/projects");

  await poll({ previews: [{ ...preview, state: "creating", stage: "deploy", environment_id: "env1" }], waiting: [] });
  await waitFor(() => expect(reads("/api/projects")).toBe(projectReads + 1));

  await poll({ previews: [{ ...preview, environment_id: "env1", last_activity_at: "2026-11-24T01:00:00Z" }], waiting: [] });
  await poll({ previews: [{ ...preview, state: "deleting", stage: "remove", environment_id: "env1" }], waiting: [] });
  await poll(new Error("temporarily unavailable"));
  await new Promise((resolve) => setTimeout(resolve, 0));
  expect(reads("/api/projects")).toBe(projectReads + 1);

  await poll({ previews: [], waiting: [] });
  expect(await screen.findByText(/No previews yet/)).toBeInTheDocument();
  await waitFor(() => expect(reads("/api/projects")).toBe(projectReads + 2));
  await poll({ previews: [], waiting: [] });
  await new Promise((resolve) => setTimeout(resolve, 0));
  expect(reads("/api/projects")).toBe(projectReads + 2);
});

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import type { ReactNode } from "react";
import { afterEach, expect, it, vi } from "vitest";
import { request } from "../api/client";
import type { BuildNodeSettings } from "../api/build-nodes";
import type { BuildRecipeSettings, BuildpacksSettings, NixpacksSettings } from "../api/build-recipes";
import type { Node, Operation, OperationBuildRecipe, Workload } from "../api/types";
import { ResourceBuild } from "./ResourceBuild";

vi.mock("@tanstack/react-router", () => ({ Link: ({ children }: { children: ReactNode }) => <a href="/">{children}</a> }));
vi.mock("../api/client", () => ({ request: vi.fn(), errorMessage: () => "Request failed", isStatus: () => false }));
afterEach(() => vi.clearAllMocks());

const workload = { id: "w1", name: "nightly", node_id: "n1", kind: "job", permissions: ["read", "operate"], manually_stopped: false, deployed: true } as Workload;

function node(buildPublish: boolean | undefined, id = "n1", name = "worker-1"): Node {
  return { id, name, address: "", port: 22, state: "ready", mode: "ssh", last_checked_at: null, capabilities: { protocol: 1, os: "linux", arch: "amd64", docker: "27", compose: "2", ready: true, build_publish: buildPublish } };
}

const reference = "ghcr.io/team/app@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef";

function build(id: string, state: Operation["state"], images = false, executionNode?: string, recipe?: OperationBuildRecipe): Operation {
  const created_at = "2026-11-22T10:00:00Z";
  return {
    id,
    workload_id: "w1",
    action: "build",
    state,
    created_at,
    ...(executionNode ? { execution_node_id: executionNode } : {}),
    result: {
      id,
      workload: "w1",
      action: "build",
      state,
      created_at,
      ...(images ? { build: { images: [{ service: "web", image_id: "sha256:1", reference }] } } : {}),
      ...(state === "failed" ? { error: "push denied" } : {}),
      ...(recipe ? { build_recipe: recipe } : {}),
    },
  };
}

const runtimeChoice = { id: "n1", name: "worker-1", eligible: true, reason: "" };

function settings(overrides: Partial<BuildNodeSettings> = {}): BuildNodeSettings {
  return { node_id: "", runtime_node_id: "n1", effective_node_id: "n1", nodes: [runtimeChoice], ...overrides };
}

function recipeSettings(overrides: Partial<BuildRecipeSettings> = {}): BuildRecipeSettings {
  return {
    recipe: { engine: "compose", service: "", directory: "", output: "", build_command: "", start_command: "" },
    can_edit: false,
    source_ready: true,
    supported: true,
    reason: "",
    build_node_id: "n1",
    active_operation_id: "",
    build_command_set: false,
    start_command_set: false,
    railpack_version: "0.43.1",
    pinned_images: [],
    ...overrides,
  };
}

interface Serve {
  recipe?: () => Promise<unknown>;
  nodes?: Node[];
  registryId?: string;
  operations?: Operation[] | (() => Promise<unknown>);
  buildNode?: () => Promise<unknown>;
}

function serve({ nodes = [node(true)], registryId = "r1", operations = [], buildNode = () => Promise.resolve(settings()), recipe = () => Promise.resolve(recipeSettings()) }: Serve = {}) {
  vi.mocked(request).mockImplementation((method: string, path: string) => {
    if (method === "POST") return Promise.resolve({ id: "op9", state: "queued" });
    if (path === "/api/workloads/w1/build-recipe") return recipe();
    if (path === "/api/workloads/w1/build-node") return buildNode();
    if (path === "/api/nodes") return Promise.resolve(nodes);
    if (path === "/api/registries") return Promise.resolve([{ id: "r1", name: "GHCR", registry: { server: "ghcr.io", username: "bot" } }]);
    if (path.endsWith("/registry")) return Promise.resolve({ registry_id: registryId });
    if (path.startsWith("/api/operations")) return typeof operations === "function" ? operations() : Promise.resolve(operations);
    return Promise.reject(new Error(`Unexpected request ${path}`));
  });
}

function renderBuild(target: Workload = workload) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(
    <QueryClientProvider client={client}>
      <ResourceBuild workload={target} />
    </QueryClientProvider>,
  );
  return client;
}

it("validates the repository and queues the exact build payload", async () => {
  serve();
  renderBuild();
  const button = screen.getByRole("button", { name: "Build and publish" });
  await waitFor(() => expect(button).toBeEnabled());
  expect(await screen.findByText("ghcr.io")).toBeInTheDocument();

  fireEvent.change(screen.getByLabelText("Repository"), { target: { value: "ghcr.io/team/app:latest" } });
  fireEvent.click(button);
  expect(screen.getByText(/without a registry host, tag or digest/)).toBeInTheDocument();
  expect(screen.getByLabelText("Repository")).toHaveAttribute("aria-invalid", "true");
  expect(request).not.toHaveBeenCalledWith("POST", expect.anything(), expect.anything());

  fireEvent.change(screen.getByLabelText("Repository"), { target: { value: " team/app " } });
  fireEvent.click(screen.getByRole("checkbox", { name: "Bypass the build cache" }));
  fireEvent.click(button);
  await waitFor(() => expect(request).toHaveBeenCalledWith("POST", "/api/workloads/w1/operations", { action: "build", build: { repository: "team/app", no_cache: true } }));
  expect(await screen.findByText("Build queued")).toBeInTheDocument();
});

it("asks for a worker upgrade when build support is not reported", async () => {
  serve({ nodes: [node(undefined)], buildNode: () => Promise.resolve(settings({ nodes: [{ ...runtimeChoice, eligible: false, reason: "the build node must be ready and report build and publish support" }] })) });
  renderBuild();
  expect(await screen.findByText("Worker upgrade needed")).toBeInTheDocument();
  expect(screen.getByRole("button", { name: "Build and publish" })).toBeDisabled();
});

it("asks for a registry when none is selected", async () => {
  serve({ registryId: "" });
  renderBuild();
  expect(await screen.findByText("No registry selected")).toBeInTheDocument();
  expect(screen.getByRole("button", { name: "Build and publish" })).toBeDisabled();
});

it("shows results read-only without operator access", async () => {
  serve({ operations: [build("ok", "succeeded", true)] });
  renderBuild({ ...workload, permissions: ["read"] });
  expect(await screen.findByText(reference)).toBeInTheDocument();
  expect(screen.getByText(/need operator access/)).toBeInTheDocument();
  expect(screen.queryByRole("button", { name: "Build and publish" })).not.toBeInTheDocument();
});

it("keeps the latest successful references visible after a failed attempt", async () => {
  serve({ operations: [build("bad", "failed"), build("ok", "succeeded", true)] });
  renderBuild();
  expect(await screen.findByText("Latest attempt")).toBeInTheDocument();
  expect(screen.getByText("push denied")).toBeInTheDocument();
  expect(screen.getByText(/latest successful build is still shown below/)).toBeInTheDocument();
  const list = screen.getByRole("list", { name: "Published images" });
  expect(within(list).getByText("web")).toBeInTheDocument();
  expect(within(list).getByText(reference)).toBeInTheDocument();
  expect(within(list).getByRole("button", { name: "Copy reference for web" })).toBeInTheDocument();
});

it("blocks building while a build runs and links to its progress", async () => {
  serve({ operations: [build("run", "running")] });
  renderBuild();
  expect(await screen.findByText("View build progress")).toBeInTheDocument();
  expect(screen.getByText("No successful build in the recent operation history.")).toBeInTheDocument();
  expect(screen.getByRole("button", { name: "Build and publish" })).toBeDisabled();
});

const designated = settings({ node_id: "n2", effective_node_id: "n2", nodes: [{ ...runtimeChoice, eligible: false, reason: "upgrade" }, { id: "n2", name: "builder-2", eligible: true, reason: "" }] });

it("builds on an eligible designated node with the unchanged build body", async () => {
  serve({ nodes: [node(undefined), node(true, "n2", "builder-2")], buildNode: () => Promise.resolve(designated) });
  renderBuild();
  const button = screen.getByRole("button", { name: "Build and publish" });
  await waitFor(() => expect(button).toBeEnabled());
  expect(screen.queryByText("Worker upgrade needed")).not.toBeInTheDocument();
  fireEvent.change(screen.getByLabelText("Repository"), { target: { value: "team/app" } });
  fireEvent.click(button);
  await waitFor(() => expect(request).toHaveBeenCalledWith("POST", "/api/workloads/w1/operations", { action: "build", build: { repository: "team/app", no_cache: false } }));
});

it("blocks building when the effective node is not eligible, even if the runtime node can build", async () => {
  const reason = "build and runtime nodes must report the same Linux architecture";
  serve({ nodes: [node(true), node(true, "n2", "builder-2")], buildNode: () => Promise.resolve(settings({ node_id: "n2", effective_node_id: "n2", nodes: [runtimeChoice, { id: "n2", name: "builder-2", eligible: false, reason }] })) });
  renderBuild();
  expect(await screen.findByText("builder-2: " + reason)).toBeInTheDocument();
  expect(screen.getByRole("button", { name: "Build and publish" })).toBeDisabled();
});

it("blocks building when the effective node is missing from the list", async () => {
  serve({ buildNode: () => Promise.resolve(settings({ node_id: "n9", effective_node_id: "n9" })) });
  renderBuild();
  expect(await screen.findByText("Build node unavailable")).toBeInTheDocument();
  expect(screen.getByText("n9")).toBeInTheDocument();
  expect(screen.getByRole("button", { name: "Build and publish" })).toBeDisabled();
});

it("blocks building and offers Retry when the build node cannot be read", async () => {
  serve({ buildNode: () => Promise.reject(new Error("down")) });
  renderBuild();
  expect(await screen.findByText("Could not load the build node")).toBeInTheDocument();
  expect(screen.getByRole("button", { name: "Retry" })).toBeInTheDocument();
  expect(screen.getByRole("button", { name: "Build and publish" })).toBeDisabled();
});

it("stops trusting the build node after a failed refresh", async () => {
  let fail = false;
  serve({ buildNode: () => (fail ? Promise.reject(new Error("down")) : Promise.resolve(settings())) });
  const client = renderBuild();
  const button = screen.getByRole("button", { name: "Build and publish" });
  await waitFor(() => expect(button).toBeEnabled());
  fail = true;
  await client.refetchQueries({ queryKey: ["workloads", "w1", "build-node"] });
  expect(await screen.findByText("Could not load the build node")).toBeInTheDocument();
  expect(button).toBeDisabled();
});

it("stops trusting operations after a failed refresh", async () => {
  let fail = false;
  serve({ operations: () => (fail ? Promise.reject(new Error("down")) : Promise.resolve([])) });
  const client = renderBuild();
  const button = screen.getByRole("button", { name: "Build and publish" });
  await waitFor(() => expect(button).toBeEnabled());
  fail = true;
  await client.refetchQueries({ queryKey: ["operations"] });
  expect(await screen.findByText("Could not load the latest builds")).toBeInTheDocument();
  expect(button).toBeDisabled();
});

it("shows where each build ran from the operation, not the current assignment", async () => {
  serve({ nodes: [node(true), node(true, "n2", "builder-2")], buildNode: () => Promise.resolve(designated), operations: [build("bad", "failed", false), build("ok", "succeeded", true, "n1")] });
  renderBuild();
  expect(await screen.findByText("Built on worker-1")).toBeInTheDocument();
  expect(screen.getByText("Build node not recorded")).toBeInTheDocument();
  expect(screen.queryByText("Built on builder-2")).not.toBeInTheDocument();
});

it("shows the node ID when the execution node is not listed", async () => {
  serve({ operations: [build("ok", "succeeded", true, "gone")] });
  renderBuild();
  expect(await screen.findByText("gone")).toBeInTheDocument();
});

it("keeps ordinary Compose builds available when explicit recipes are unavailable", async () => {
  serve({ recipe: () => Promise.resolve(recipeSettings({ source_ready: false, supported: false, reason: "select a Git source using the saved Compose configuration before choosing a recipe" })) });
  renderBuild();
  await waitFor(() => expect(screen.getByRole("button", { name: "Build and publish" })).toBeEnabled());
  expect(screen.queryByText("The build recipe cannot build")).not.toBeInTheDocument();
});

it("refuses to build a saved explicit recipe the server reports as unusable", async () => {
  serve({ recipe: () => Promise.resolve(recipeSettings({ recipe: { engine: "static", service: "web", directory: "site", output: "dist", build_command: "", start_command: "" }, reason: "the selected static service is absent from saved Compose" })) });
  renderBuild();
  expect(await screen.findByText("The build recipe cannot build")).toBeInTheDocument();
  expect(screen.getByText("the selected static service is absent from saved Compose", { exact: true }).tagName).toBe("P");
  expect(screen.getByText("Change the build recipe above or resolve the problem to build.", { exact: true }).tagName).toBe("P");
  expect(screen.getByRole("button", { name: "Build and publish" })).toBeDisabled();
});

it("refuses to build a saved recipe on a worker without recipe support", async () => {
  serve({ recipe: () => Promise.resolve(recipeSettings({ recipe: { engine: "railpack", service: "web", directory: ".", output: "", build_command: "", start_command: "" }, supported: false })) });
  renderBuild();
  expect(await screen.findByText("The build recipe cannot build")).toBeInTheDocument();
  expect(screen.getByRole("button", { name: "Build and publish" })).toBeDisabled();
});

it("refuses to build an engine this panel does not recognise", async () => {
  serve({ recipe: () => Promise.resolve(recipeSettings({ recipe: { engine: "kaniko", service: "web", directory: ".", output: "", build_command: "", start_command: "" } })) });
  renderBuild();
  expect(await screen.findByText(/Unknown engine \(kaniko\)/)).toBeInTheDocument();
  expect(screen.getByRole("button", { name: "Build and publish" })).toBeDisabled();
});

it("blocks building when the build recipe cannot be read", async () => {
  serve({ recipe: () => Promise.reject(new Error("down")) });
  renderBuild();
  expect(await screen.findByText("Could not load the build recipe")).toBeInTheDocument();
  expect(screen.getByRole("button", { name: "Build and publish" })).toBeDisabled();
});

it("shows the engine each build recorded and never assumes Compose for older builds", async () => {
  serve({ operations: [build("bad", "failed", false, "n1", { engine: "railpack", service: "web", version: "0.43.1" }), build("ok", "succeeded", true, "n1")] });
  renderBuild();
  expect(await screen.findByText("Railpack 0.43.1 for web")).toBeInTheDocument();
  expect(screen.getByText("Build engine not recorded")).toBeInTheDocument();
  expect(screen.queryByText(/Existing Compose/)).not.toBeInTheDocument();
});

const cnbRecipe = { engine: "buildpacks", service: "web", directory: ".", output: "", build_command: "", start_command: "" };

function buildpacks(overrides: Partial<BuildpacksSettings> = {}): BuildpacksSettings {
  return {
    supported: true,
    reason: "",
    lifecycle_version: "0.21.29",
    builder_version: "0.0.230",
    platform_api: "0.13",
    pinned_images: ["lifecycle@sha256:a", "builder@sha256:b", "run@sha256:c"],
    work_storage: "disk",
    work_hard_cap: false,
    work_target_bytes: 3221225472,
    minimum_phase_memory_bytes: 2147483648,
    maximum_phase_memory_bytes: 4294967296,
    memory_safety_bytes: 2147483648,
    required_available_memory_bytes: 4294967296,
    required_docker_disk_bytes: 25769803776,
    required_worker_disk_bytes: 10737418240,
    quota_memory_mb: 4352,
    quota_cpu: 2.5,
    builder_download_bytes: 1446326381,
    builder_unpacked_bytes: 5108638067,
    experimental: true,
    ...overrides,
  };
}

it("builds a saved Cloud Native Buildpacks recipe with the unchanged build payload", async () => {
  serve({ recipe: () => Promise.resolve(recipeSettings({ recipe: cnbRecipe, supported: false, buildpacks: buildpacks() })) });
  renderBuild();
  const button = screen.getByRole("button", { name: "Build and publish" });
  await waitFor(() => expect(button).toBeEnabled());
  fireEvent.change(screen.getByLabelText("Repository"), { target: { value: "team/app" } });
  fireEvent.click(button);
  await waitFor(() => expect(request).toHaveBeenCalledWith("POST", "/api/workloads/w1/operations", { action: "build", build: { repository: "team/app", no_cache: false } }));
});

it.each([
  ["the server omits its eligibility", recipeSettings({ recipe: cnbRecipe }), /did not report Cloud Native Buildpacks eligibility/],
  ["the build node cannot run it", recipeSettings({ recipe: cnbRecipe, buildpacks: buildpacks({ supported: false, reason: "the build node has 6 GiB available; Cloud Native Buildpacks needs 9 GiB" }) }), /needs 9 GiB/],
  ["the server refuses the saved recipe", recipeSettings({ recipe: cnbRecipe, reason: "the selected service is absent from saved Compose", buildpacks: buildpacks() }), /absent from saved Compose/],
])("refuses to build a saved Cloud Native Buildpacks recipe when %s", async (_case, read, reason) => {
  serve({ recipe: () => Promise.resolve(read) });
  renderBuild();
  expect(await screen.findByText("The build recipe cannot build")).toBeInTheDocument();
  expect(screen.getByText(reason)).toBeInTheDocument();
  expect(screen.getByRole("button", { name: "Build and publish" })).toBeDisabled();
});

it("keeps Compose builds available when Cloud Native Buildpacks eligibility is unknown", async () => {
  serve();
  renderBuild();
  await waitFor(() => expect(screen.getByRole("button", { name: "Build and publish" })).toBeEnabled());
  expect(screen.queryByText("The build recipe cannot build")).not.toBeInTheDocument();
});

it("blocks building after a failed refresh of a usable Cloud Native Buildpacks recipe and offers Retry", async () => {
  let fail = false;
  serve({ recipe: () => (fail ? Promise.reject(new Error("down")) : Promise.resolve(recipeSettings({ recipe: cnbRecipe, buildpacks: buildpacks() }))) });
  const client = renderBuild();
  const button = screen.getByRole("button", { name: "Build and publish" });
  await waitFor(() => expect(button).toBeEnabled());
  fail = true;
  await client.refetchQueries({ queryKey: ["workloads", "w1", "build-recipe"] });
  expect(await screen.findByText("Could not load the build recipe")).toBeInTheDocument();
  expect(button).toBeDisabled();
  expect(screen.getByRole("button", { name: "Retry" })).toBeInTheDocument();
});

it("shows the Cloud Native Buildpacks versions a build recorded, whatever the current recipe is", async () => {
  serve({
    recipe: () => Promise.resolve(recipeSettings()),
    operations: [build("cnb", "succeeded", true, "n1", { engine: "buildpacks", service: "web", version: "0.21.29", builder_version: "0.0.230", platform_api: "0.13", pinned_images: ["a", "b", "c"] })],
  });
  renderBuild();
  expect(await screen.findByText("Cloud Native Buildpacks (lifecycle 0.21.29, builder 0.0.230, platform API 0.13) for web")).toBeInTheDocument();
});

it("shows the phase memory limit a Cloud Native Buildpacks build recorded, independent of the current settings", async () => {
  serve({
    recipe: () => Promise.resolve(recipeSettings({ recipe: cnbRecipe, buildpacks: buildpacks({ maximum_phase_memory_bytes: 8 * 1024 ** 3 }) })),
    operations: [build("cnb", "succeeded", true, "n1", { engine: "buildpacks", service: "web", version: "0.21.29", phase_memory_bytes: 2430 * 1024 ** 2, admission_available_memory_bytes: 4430 * 1024 ** 2 })],
  });
  renderBuild();
  expect(await screen.findByText("Phase memory limit 2,430 MiB (from 4,430 MiB available at admission)")).toBeInTheDocument();
  expect(screen.queryByText(/8 GiB|4 GiB/)).not.toBeInTheDocument();
});

it.each([
  ["a queued build before admission", "queued" as const, undefined, "Phase memory not assigned"],
  ["older metadata without a limit", "failed" as const, undefined, "Phase memory not assigned"],
  ["an unrecognised recorded limit", "failed" as const, "4GiB", "Phase memory limit unavailable"],
  ["a zero recorded limit", "failed" as const, 0, "Phase memory limit unavailable"],
])("never assumes a phase memory limit for %s", async (_case, state, limit, text) => {
  const recipe = { engine: "buildpacks", service: "web", ...(limit === undefined ? {} : { phase_memory_bytes: limit }) } as OperationBuildRecipe;
  serve({ recipe: () => Promise.resolve(recipeSettings({ recipe: cnbRecipe, buildpacks: buildpacks() })), operations: [build("cnb", state, false, "n1", recipe)] });
  renderBuild();
  expect(await screen.findByText(text)).toBeInTheDocument();
  expect(screen.queryByText(/Phase memory limit \d/)).not.toBeInTheDocument();
});

it("shows no phase memory for builds by other engines", async () => {
  serve({ recipe: () => Promise.resolve(recipeSettings()), operations: [build("rp", "succeeded", true, "n1", { engine: "railpack", service: "web", version: "0.43.1" })] });
  renderBuild();
  expect(await screen.findByText("Railpack 0.43.1 for web")).toBeInTheDocument();
  expect(screen.queryByText(/Phase memory/)).not.toBeInTheDocument();
});

const nixRecipe = { engine: "nixpacks", service: "web", directory: ".", output: "", build_command: "", start_command: "" };
const nixBase = `ghcr.io/railwayapp/nixpacks@sha256:${"d4".repeat(32)}`;
const nixDigest = `sha256:${"0f".repeat(32)}`;

function nixpacks(overrides: Partial<NixpacksSettings> = {}): NixpacksSettings {
  return {
    supported: true,
    reason: "",
    version: "1.41.0",
    planner_sha256: nixDigest,
    planner_download_bytes: 3717141,
    base_image: nixBase,
    base_download_bytes: 138966568,
    maintenance: true,
    proxy_supported: false,
    planner_memory_bytes: 536870912,
    planner_cpu: 1,
    planner_timeout_seconds: 60,
    planner_output_bytes: 134217728,
    ...overrides,
  };
}

it("builds a saved Nixpacks recipe with the unchanged build payload", async () => {
  serve({ recipe: () => Promise.resolve(recipeSettings({ recipe: nixRecipe, supported: false, nixpacks: nixpacks() })) });
  renderBuild();
  const button = screen.getByRole("button", { name: "Build and publish" });
  await waitFor(() => expect(button).toBeEnabled());
  fireEvent.change(screen.getByLabelText("Repository"), { target: { value: "team/app" } });
  fireEvent.click(button);
  await waitFor(() => expect(request).toHaveBeenCalledWith("POST", "/api/workloads/w1/operations", { action: "build", build: { repository: "team/app", no_cache: false } }));
});

it.each([
  ["the server omits its eligibility", recipeSettings({ recipe: nixRecipe }), /did not report usable Nixpacks eligibility/],
  ["the build node is an old worker", recipeSettings({ recipe: nixRecipe, nixpacks: nixpacks({ supported: false, reason: "upgrade and verify the effective build worker before using Nixpacks" }) }), /before using Nixpacks/],
  ["the server refuses a configured proxy", recipeSettings({ recipe: nixRecipe, reason: "Nixpacks is unavailable while an outbound build proxy is configured", nixpacks: nixpacks() }), /outbound build proxy is configured/],
])("refuses to build a saved Nixpacks recipe when %s", async (_case, read, reason) => {
  serve({ recipe: () => Promise.resolve(read) });
  renderBuild();
  expect(await screen.findByText("The build recipe cannot build")).toBeInTheDocument();
  expect(screen.getByText(reason)).toBeInTheDocument();
  expect(screen.getByRole("button", { name: "Build and publish" })).toBeDisabled();
});

it("shows an unpunctuated Nixpacks refusal apart from the recovery step", async () => {
  const reason = "Nixpacks is unavailable while an outbound build proxy is configured";
  serve({ recipe: () => Promise.resolve(recipeSettings({ recipe: nixRecipe, reason, nixpacks: nixpacks() })) });
  renderBuild();
  expect(await screen.findByText("The build recipe cannot build")).toBeInTheDocument();
  expect(screen.getByText(reason, { exact: true }).tagName).toBe("P");
  expect(screen.getByText("Change the build recipe above or resolve the problem to build.", { exact: true }).tagName).toBe("P");
  expect(screen.queryByText(/^Nixpacks: /)).not.toBeInTheDocument();
});

it("keeps Compose builds available when Nixpacks eligibility is unknown", async () => {
  serve({ recipe: () => Promise.resolve(recipeSettings()) });
  renderBuild();
  await waitFor(() => expect(screen.getByRole("button", { name: "Build and publish" })).toBeEnabled());
  expect(screen.queryByText("The build recipe cannot build")).not.toBeInTheDocument();
});

it("blocks building after a failed refresh of a usable Nixpacks recipe", async () => {
  let fail = false;
  serve({ recipe: () => (fail ? Promise.reject(new Error("down")) : Promise.resolve(recipeSettings({ recipe: nixRecipe, nixpacks: nixpacks() }))) });
  const client = renderBuild();
  const button = screen.getByRole("button", { name: "Build and publish" });
  await waitFor(() => expect(button).toBeEnabled());
  fail = true;
  await client.refetchQueries({ queryKey: ["workloads", "w1", "build-recipe"] });
  expect(await screen.findByText("Could not load the build recipe")).toBeInTheDocument();
  expect(button).toBeDisabled();
});

it("shows the Nixpacks version and planner archive checksum a build recorded, independent of the current settings", async () => {
  const recorded = `sha256:${"12".repeat(32)}`;
  serve({
    recipe: () => Promise.resolve(recipeSettings({ recipe: nixRecipe, nixpacks: nixpacks({ version: "1.42.0" }) })),
    operations: [build("nix", "succeeded", true, "n1", { engine: "nixpacks", service: "web", version: "1.41.0", pinned_images: [nixBase], planner_sha256: recorded })],
  });
  renderBuild();
  expect(await screen.findByText("Nixpacks 1.41.0 for web")).toBeInTheDocument();
  expect(screen.getByText(`Planner archive checksum ${recorded}`)).toBeInTheDocument();
  expect(screen.queryByText(new RegExp(nixDigest))).not.toBeInTheDocument();
  expect(screen.queryByText(/1\.42\.0/)).not.toBeInTheDocument();
});

it("never takes the planner archive checksum from current settings for a build that did not record one", async () => {
  serve({
    recipe: () => Promise.resolve(recipeSettings({ recipe: nixRecipe, nixpacks: nixpacks() })),
    operations: [build("nix", "queued", false, "n1", { engine: "nixpacks", service: "web", version: "1.41.0" })],
  });
  renderBuild();
  expect(await screen.findByText("Planner archive checksum not recorded")).toBeInTheDocument();
  expect(screen.queryByText(new RegExp(nixDigest))).not.toBeInTheDocument();
});

it("shows no planner archive checksum for builds by other engines", async () => {
  serve({ recipe: () => Promise.resolve(recipeSettings()), operations: [build("rp", "succeeded", true, "n1", { engine: "railpack", service: "web", version: "0.43.1", planner_sha256: nixDigest })] });
  renderBuild();
  expect(await screen.findByText("Railpack 0.43.1 for web")).toBeInTheDocument();
  expect(screen.queryByText(/Planner archive checksum/)).not.toBeInTheDocument();
});

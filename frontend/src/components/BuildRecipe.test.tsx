import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { ReactNode } from "react";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import type { BuildRecipe, BuildRecipeSettings, BuildpacksSettings, NixpacksSettings } from "../api/build-recipes";
import { ApiError, request } from "../api/client";
import type { Operation, Permission, Workload } from "../api/types";
import { railpackDisclosure } from "../lib/build-recipes";
import { BuildRecipeSetting } from "./BuildRecipe";

vi.mock("../api/client", async (original) => ({ ...(await original<typeof import("../api/client")>()), request: vi.fn() }));
vi.mock("./SaveBar", async (original) => ({ ...(await original<typeof import("./SaveBar")>()), UnsavedChangesGuard: () => null }));
vi.mock("@tanstack/react-router", () => ({ Link: ({ children }: { children: ReactNode }) => <a href="/">{children}</a> }));

// jsdom does not implement modal dialogs.
HTMLDialogElement.prototype.showModal = function (this: HTMLDialogElement) {
  this.open = true;
};
HTMLDialogElement.prototype.close = function (this: HTMLDialogElement) {
  this.open = false;
};

beforeEach(() => vi.stubGlobal("ResizeObserver", class { observe = vi.fn(); disconnect = vi.fn(); }));
afterEach(() => {
  vi.clearAllMocks();
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
  window.dispatchEvent(new Event("online"));
});

const editor: Permission[] = ["read", "operate", "edit"];
const compose = "services:\n  web:\n    build: .\n  worker:\n    image: busybox\n";

function workload(overrides: Partial<Workload> = {}): Workload {
  return { id: "w1", name: "app", node_id: "n1", kind: "service", permissions: editor, manually_stopped: false, deployed: true, compose, ...overrides } as Workload;
}

const composeRecipe: BuildRecipe = { engine: "compose", service: "", directory: "", output: "", build_command: "", start_command: "" };
const pins = ["ghcr.io/railwayapp/railpack-frontend:v0.43.1@sha256:aaa", "ghcr.io/railwayapp/railpack-builder:v0.43.1@sha256:bbb", "ghcr.io/railwayapp/railpack-runtime:v0.43.1@sha256:ccc"];

const cnbPins = [
  "docker.io/buildpacksio/lifecycle@sha256:6119d8ac92f81873a43b1db6fbedcc1538809161b93dcf8b9e58086e46002661",
  "docker.io/paketobuildpacks/ubuntu-noble-builder@sha256:45f9d50b1f96c1cddd9c288823a9291ec2e1295e6fa3dc24d2f7f8ee2b46cc45",
  "docker.io/paketobuildpacks/ubuntu-noble-run@sha256:a0386bc457590ffed6ddd6039c569233ac3f9afefe38766641e214e49b3cbfba",
];

function buildpacks(overrides: Partial<BuildpacksSettings> = {}): BuildpacksSettings {
  return {
    supported: true,
    reason: "",
    lifecycle_version: "0.21.29",
    builder_version: "0.0.230",
    platform_api: "0.13",
    pinned_images: cnbPins,
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

const cnbRecipe: BuildRecipe = { ...composeRecipe, engine: "buildpacks", service: "web", directory: "app" };

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

const nixRecipe: BuildRecipe = { ...composeRecipe, engine: "nixpacks", service: "web", directory: "app" };

function settings(overrides: Partial<BuildRecipeSettings> = {}): BuildRecipeSettings {
  return {
    recipe: composeRecipe,
    can_edit: true,
    source_ready: true,
    supported: true,
    reason: "",
    build_node_id: "n1",
    active_operation_id: "",
    build_command_set: false,
    start_command_set: false,
    railpack_version: "0.43.1",
    pinned_images: pins,
    buildpacks: buildpacks(),
    nixpacks: nixpacks(),
    ...overrides,
  };
}

// An older server's read, without the buildpacks object.
function legacySettings(overrides: Partial<BuildRecipeSettings> = {}): BuildRecipeSettings {
  const read = settings(overrides);
  delete read.buildpacks;
  return read;
}

interface Serve {
  read?: () => Promise<unknown>;
  operations?: () => Promise<unknown>;
  save?: () => Promise<unknown>;
}

function serve({ read = () => Promise.resolve(settings()), operations = () => Promise.resolve([]), save = () => Promise.resolve({ ok: true }) }: Serve = {}) {
  vi.mocked(request).mockImplementation((method: string, path: string) => {
    if (path === "/api/workloads/w1/build-recipe") return method === "PUT" ? save() : read();
    if (path.startsWith("/api/operations")) return operations();
    return Promise.reject(new Error(`Unexpected request ${method} ${path}`));
  });
}

function renderSetting(target = workload()) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(
    <QueryClientProvider client={client}>
      <BuildRecipeSetting workload={target} />
    </QueryClientProvider>,
  );
  return client;
}

type User = ReturnType<typeof userEvent.setup>;

async function choose(user: User, label: string, option: string) {
  await user.click(screen.getByRole("combobox", { name: label }));
  await user.click(screen.getByRole("option", { name: option }));
}

async function type(user: User, label: string, value: string) {
  const field = screen.getByLabelText(new RegExp(`^${label}`));
  await user.clear(field);
  if (value) await user.type(field, value);
}

const saveButton = () => screen.getByRole("button", { name: "Save build recipe" });
const puts = () => vi.mocked(request).mock.calls.filter(([method]) => method === "PUT");

it("defaults to the existing Compose build and offers only the supported engines", async () => {
  const user = userEvent.setup();
  serve();
  renderSetting();
  const engine = await screen.findByRole("combobox", { name: "Build engine" });
  expect(engine).toHaveTextContent("Existing Compose / Dockerfile");
  await user.click(engine);
  expect(screen.getAllByRole("option").map((option) => option.textContent)).toEqual(["Existing Compose / Dockerfile", "Static output", "Railpack", "Cloud Native Buildpacks", "Nixpacks"]);
  expect(screen.queryByText(/detect/i)).not.toBeInTheDocument();
});

it("confirms the engine change and saves the exact static payload", async () => {
  const user = userEvent.setup();
  serve();
  const client = renderSetting();
  const invalidate = vi.spyOn(client, "invalidateQueries");
  await screen.findByRole("combobox", { name: "Build engine" });
  await choose(user, "Build engine", "Static output");
  expect(screen.getByText(/No build command runs/)).toBeInTheDocument();
  expect(screen.queryByLabelText(/^Build command/)).not.toBeInTheDocument();
  await choose(user, "Service", "web");
  await type(user, "Source directory", "site");
  await type(user, "Output directory", "dist");
  await user.click(saveButton());

  const dialog = await screen.findByRole("dialog", { name: "Change the build engine?" });
  expect(dialog).toHaveTextContent("replace that service's existing Compose build section or Dockerfile with Static output");
  expect(dialog).toHaveTextContent("non-root user on a read-only filesystem and cannot use mounts, devices or added capabilities");
  expect(puts()).toHaveLength(0);
  await user.click(within(dialog).getByRole("button", { name: "Change build engine" }));

  await waitFor(() => expect(request).toHaveBeenCalledWith("PUT", "/api/workloads/w1/build-recipe", { engine: "static", service: "web", directory: "site", output: "dist", build_command: "", start_command: "" }));
  expect(await screen.findByText(/Build recipe saved/)).toBeInTheDocument();
  expect(invalidate).toHaveBeenCalledWith({ queryKey: ["workloads", "w1", "build-recipe"], exact: true });
  expect(invalidate).toHaveBeenCalledWith({ queryKey: ["operations"] });
});

it("does not save when the engine change is cancelled", async () => {
  const user = userEvent.setup();
  serve();
  renderSetting();
  await screen.findByRole("combobox", { name: "Build engine" });
  await choose(user, "Build engine", "Railpack");
  await choose(user, "Service", "web");
  await type(user, "Source directory", ".");
  await user.click(saveButton());
  await user.click(within(await screen.findByRole("dialog")).getByRole("button", { name: "Cancel" }));
  expect(puts()).toHaveLength(0);
  expect(screen.getByRole("status")).toHaveTextContent("Unsaved changes");
});

it("clears the static output when switching to Railpack and sends its command overrides", async () => {
  const user = userEvent.setup();
  serve({ read: () => Promise.resolve(settings({ recipe: { ...composeRecipe, engine: "static", service: "web", directory: "site", output: "dist" } })) });
  renderSetting();
  await screen.findByRole("combobox", { name: "Build engine" });
  await choose(user, "Build engine", "Railpack");
  await type(user, "Build command", "npm run build");
  await type(user, "Start command", "node server.js");
  await user.click(saveButton());
  await user.click(within(await screen.findByRole("dialog")).getByRole("button", { name: "Change build engine" }));
  await waitFor(() => expect(request).toHaveBeenCalledWith("PUT", "/api/workloads/w1/build-recipe", { engine: "railpack", service: "web", directory: "site", output: "", build_command: "npm run build", start_command: "node server.js" }));
});

it("clears every recipe field when going back to Compose", async () => {
  const user = userEvent.setup();
  serve({ read: () => Promise.resolve(settings({ recipe: { engine: "railpack", service: "web", directory: "app", output: "", build_command: "make", start_command: "./run" }, build_command_set: true, start_command_set: true })) });
  renderSetting();
  await screen.findByRole("combobox", { name: "Build engine" });
  await choose(user, "Build engine", "Existing Compose / Dockerfile");
  await user.click(saveButton());
  const dialog = await screen.findByRole("dialog");
  expect(dialog).toHaveTextContent("go back to its existing Compose build section or Dockerfile");
  await user.click(within(dialog).getByRole("button", { name: "Change build engine" }));
  await waitFor(() => expect(request).toHaveBeenCalledWith("PUT", "/api/workloads/w1/build-recipe", composeRecipe));
});

it("saves a change within the same engine without asking", async () => {
  const user = userEvent.setup();
  serve({ read: () => Promise.resolve(settings({ recipe: { ...composeRecipe, engine: "static", service: "web", directory: "site", output: "dist" } })) });
  renderSetting();
  await screen.findByRole("combobox", { name: "Build engine" });
  await type(user, "Output directory", "public");
  await user.click(saveButton());
  await waitFor(() => expect(request).toHaveBeenCalledWith("PUT", "/api/workloads/w1/build-recipe", { engine: "static", service: "web", directory: "site", output: "public", build_command: "", start_command: "" }));
  expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
});

it("refuses unsafe paths and oversized commands before saving", async () => {
  const user = userEvent.setup();
  serve({ read: () => Promise.resolve(settings({ recipe: { ...composeRecipe, engine: "railpack", service: "web", directory: "." } })) });
  renderSetting();
  await screen.findByRole("combobox", { name: "Build engine" });
  await type(user, "Source directory", "../outside");
  const command = screen.getByLabelText(/^Build command/);
  await user.click(command);
  await user.paste("é".repeat(4097));
  await user.click(saveButton());
  expect(screen.getByText("Paths cannot contain .. segments.")).toBeInTheDocument();
  expect(screen.getByText("Use at most 8192 bytes (currently 8194).")).toBeInTheDocument();
  expect(screen.getByLabelText(/^Source directory/)).toHaveAttribute("aria-invalid", "true");
  expect(puts()).toHaveLength(0);
});

it("offers static output only to services", async () => {
  const user = userEvent.setup();
  serve();
  renderSetting(workload({ kind: "job" }));
  await user.click(await screen.findByRole("combobox", { name: "Build engine" }));
  expect(screen.getByRole("option", { name: "Static output (jobs cannot use static output)" })).toHaveAttribute("aria-disabled", "true");
  expect(screen.getByRole("option", { name: "Railpack" })).not.toHaveAttribute("aria-disabled");
});

it("explains that explicit engines need a Git source with the saved Compose file, without changing it", async () => {
  const user = userEvent.setup();
  serve({ read: () => Promise.resolve(settings({ source_ready: false, reason: "select a Git source using the saved Compose configuration before choosing a recipe" })) });
  renderSetting();
  await user.click(await screen.findByRole("combobox", { name: "Build engine" }));
  expect(screen.getByRole("option", { name: /^Static output \(needs a Git source/ })).toHaveAttribute("aria-disabled", "true");
  expect(screen.getByRole("option", { name: /^Railpack \(needs a Git source/ })).toHaveAttribute("aria-disabled", "true");
  expect(screen.getByText(/Cove does not change it for you/)).toBeInTheDocument();
});

it("keeps explicit engines unavailable on a worker without recipe support", async () => {
  const user = userEvent.setup();
  serve({ read: () => Promise.resolve(settings({ supported: false, reason: "upgrade and verify the effective build worker before using static output or Railpack" })) });
  renderSetting();
  await user.click(await screen.findByRole("combobox", { name: "Build engine" }));
  expect(screen.getByRole("option", { name: "Railpack (the build worker does not support it)" })).toHaveAttribute("aria-disabled", "true");
  expect(screen.getByText(/Upgrade and verify the build node/)).toBeInTheDocument();
});

it("shows a stale saved recipe as it is and lets Compose clear it", async () => {
  const user = userEvent.setup();
  serve({ read: () => Promise.resolve(settings({ supported: false, recipe: { ...composeRecipe, engine: "static", service: "gone", directory: "site", output: "dist" }, reason: "the selected static service is absent from saved Compose" })) });
  renderSetting();
  expect(await screen.findByText("The saved Static output recipe cannot build")).toBeInTheDocument();
  expect(screen.getByText(/not changed automatically/)).toBeInTheDocument();
  expect(screen.getByRole("combobox", { name: "Build engine" })).toHaveTextContent("Static output");
  expect(screen.getByRole("combobox", { name: "Service" })).toHaveTextContent("gone (not in the saved Compose file)");
  await choose(user, "Build engine", "Existing Compose / Dockerfile");
  await user.click(saveButton());
  await user.click(within(await screen.findByRole("dialog")).getByRole("button", { name: "Change build engine" }));
  await waitFor(() => expect(request).toHaveBeenCalledWith("PUT", "/api/workloads/w1/build-recipe", composeRecipe));
});

it("keeps an unknown saved engine visible instead of treating it as Compose", async () => {
  serve({ read: () => Promise.resolve(settings({ recipe: { ...composeRecipe, engine: "kaniko", service: "web", directory: "." } })) });
  renderSetting();
  expect(await screen.findByText("The saved build engine is not recognised")).toBeInTheDocument();
  expect(screen.getByRole("combobox", { name: "Build engine" })).toHaveTextContent("Unknown engine (kaniko)");
  expect(saveButton()).toBeDisabled();
});

it.each([
  [400, "service must be an existing saved Compose service", "error"],
  [409, "wait for active operations before changing the build recipe", "warn"],
])("keeps the draft when the server answers %i", async (status, message) => {
  const user = userEvent.setup();
  serve({ read: () => Promise.resolve(settings({ recipe: { ...composeRecipe, engine: "static", service: "web", directory: "site", output: "dist" } })), save: () => Promise.reject(new ApiError(status, message)) });
  renderSetting();
  await screen.findByRole("combobox", { name: "Build engine" });
  await type(user, "Output directory", "public");
  await user.click(saveButton());
  expect(await screen.findByText("The build recipe was not saved")).toBeInTheDocument();
  expect(screen.getByText(message)).toBeInTheDocument();
  expect(screen.getByLabelText(/^Output directory/)).toHaveValue("public");
  expect(screen.getByRole("status")).toHaveTextContent("Unsaved changes");
  expect(screen.queryByText(/Build recipe saved/)).not.toBeInTheDocument();
});

it("shows the server's policy refusal for a Railpack save and keeps the draft", async () => {
  const user = userEvent.setup();
  const reason = "Railpack is unavailable while the selected service's build network is none or an outbound build proxy is configured";
  serve({ save: () => Promise.reject(new ApiError(400, reason)) });
  renderSetting();
  await screen.findByRole("combobox", { name: "Build engine" });
  await choose(user, "Build engine", "Railpack");
  await choose(user, "Service", "web");
  await type(user, "Source directory", "app");
  await user.click(saveButton());
  await user.click(within(await screen.findByRole("dialog")).getByRole("button", { name: "Change build engine" }));
  expect(await screen.findByText("The build recipe was not saved")).toBeInTheDocument();
  expect(screen.getByText(reason)).toBeInTheDocument();
  expect(puts()).toEqual([["PUT", "/api/workloads/w1/build-recipe", { engine: "railpack", service: "web", directory: "app", output: "", build_command: "", start_command: "" }]]);
  expect(screen.getByRole("combobox", { name: "Build engine" })).toHaveTextContent("Railpack");
  expect(screen.getByLabelText(/^Source directory/)).toHaveValue("app");
  expect(screen.getByRole("status")).toHaveTextContent("Unsaved changes");
  expect(screen.queryByText(/Build recipe saved/)).not.toBeInTheDocument();
});

it("blocks every change while the server reports an active operation", async () => {
  const user = userEvent.setup();
  serve({ read: () => Promise.resolve(settings({ active_operation_id: "op1" })) });
  renderSetting();
  expect(await screen.findByText("An operation is in progress")).toBeInTheDocument();
  await choose(user, "Build engine", "Railpack");
  expect(saveButton()).toBeDisabled();
});

it("blocks saving while the operation list shows an active operation", async () => {
  const user = userEvent.setup();
  const running: Operation = { id: "op2", workload_id: "w1", action: "deploy", state: "running", created_at: "2026-11-22T10:00:00Z", result: null };
  serve({ operations: () => Promise.resolve([running]) });
  renderSetting();
  expect(await screen.findByText("An operation is in progress")).toBeInTheDocument();
  await choose(user, "Build engine", "Railpack");
  expect(saveButton()).toBeDisabled();
});

it("blocks saving when operations cannot be read", async () => {
  serve({ operations: () => Promise.reject(new Error("down")) });
  renderSetting();
  expect(await screen.findByText("Operations could not be read")).toBeInTheDocument();
});

it("blocks saving while offline", async () => {
  const user = userEvent.setup();
  serve();
  renderSetting();
  await screen.findByRole("combobox", { name: "Build engine" });
  await choose(user, "Build engine", "Railpack");
  vi.spyOn(navigator, "onLine", "get").mockReturnValue(false);
  act(() => void window.dispatchEvent(new Event("offline")));
  expect(await screen.findByText("You are offline")).toBeInTheDocument();
  expect(saveButton()).toBeDisabled();
});

it("shows loading, then offers Retry when the recipe cannot be read", async () => {
  let fail = true;
  serve({ read: () => (fail ? Promise.reject(new Error("down")) : Promise.resolve(settings())) });
  renderSetting();
  expect(screen.getByRole("status", { name: "Loading build recipe" })).toBeInTheDocument();
  expect(await screen.findByText("Could not load the build recipe")).toBeInTheDocument();
  expect(screen.queryByRole("combobox", { name: "Build engine" })).not.toBeInTheDocument();
  fail = false;
  await userEvent.setup().click(screen.getByRole("button", { name: "Retry" }));
  expect(await screen.findByRole("combobox", { name: "Build engine" })).toBeInTheDocument();
});

it("treats an incomplete response as a failed read, not as Compose", async () => {
  serve({ read: () => Promise.resolve({ recipe: { engine: "compose", service: "" }, can_edit: true }) });
  renderSetting();
  expect(await screen.findByText("Could not load the build recipe")).toBeInTheDocument();
  expect(screen.queryByText("Existing Compose / Dockerfile")).not.toBeInTheDocument();
});

it("hides stale editable data after a failed refresh and keeps the draft for later", async () => {
  const user = userEvent.setup();
  let fail = false;
  serve({ read: () => (fail ? Promise.reject(new Error("refresh failed")) : Promise.resolve(settings())) });
  const client = renderSetting();
  await screen.findByRole("combobox", { name: "Build engine" });
  await choose(user, "Build engine", "Railpack");
  fail = true;
  await client.refetchQueries({ queryKey: ["workloads", "w1", "build-recipe"] });
  expect(await screen.findByText("Could not load the current build recipe")).toBeInTheDocument();
  expect(screen.queryByRole("combobox", { name: "Build engine" })).not.toBeInTheDocument();
  expect(saveButton()).toBeDisabled();
  fail = false;
  await user.click(screen.getByRole("button", { name: "Retry" }));
  expect(await screen.findByRole("combobox", { name: "Build engine" })).toHaveTextContent("Railpack");
});

it("keeps the success notice while the refreshed recipe is still loading", async () => {
  const user = userEvent.setup();
  let hold = false;
  serve({
    read: () => (hold ? new Promise(() => undefined) : Promise.resolve(settings({ recipe: { ...composeRecipe, engine: "static", service: "web", directory: "site", output: "dist" } }))),
    save: () => {
      hold = true;
      return Promise.resolve({ ok: true });
    },
  });
  renderSetting();
  await screen.findByRole("combobox", { name: "Build engine" });
  await type(user, "Output directory", "public");
  await user.click(saveButton());
  expect(await screen.findByText(/Build recipe saved/)).toBeInTheDocument();
  expect(screen.getByLabelText(/^Output directory/)).toHaveValue("public");
  expect(screen.getByRole("status")).toHaveTextContent("Build recipe saved");
});

it("shows readers the saved recipe without command text", async () => {
  serve({ read: () => Promise.resolve(settings({ can_edit: false, recipe: { engine: "railpack", service: "web", directory: "app", output: "", build_command: "secret-build", start_command: "" }, build_command_set: true })) });
  renderSetting(workload({ permissions: ["read", "operate"] }));
  expect(await screen.findByText("Railpack")).toBeInTheDocument();
  expect(screen.getByText("Custom command saved")).toBeInTheDocument();
  expect(screen.getByText("Railpack plan")).toBeInTheDocument();
  expect(screen.queryByText(/secret-build/)).not.toBeInTheDocument();
  expect(screen.queryByRole("combobox")).not.toBeInTheDocument();
  expect(screen.queryByRole("button", { name: "Save build recipe" })).not.toBeInTheDocument();
});

it("shows the Railpack disclosure and its pinned images read-only", async () => {
  const user = userEvent.setup();
  serve();
  renderSetting();
  await screen.findByRole("combobox", { name: "Build engine" });
  expect(screen.queryByText(railpackDisclosure)).not.toBeInTheDocument();
  await choose(user, "Build engine", "Railpack");
  expect(screen.getByText(railpackDisclosure)).toBeInTheDocument();
  expect(railpackDisclosure).toMatch(/default build networking/);
  expect(railpackDisclosure).toMatch(/neither reproducible nor fully pinned/);
  expect(screen.getByText(/A build network of none and outbound build proxies are unavailable with Railpack/)).toBeInTheDocument();
  expect(screen.getByText(/receives the names of the selected build secrets, never their values/)).toBeInTheDocument();
  expect(screen.queryByText(/egress follows|planning fails/i)).not.toBeInTheDocument();
  const list = screen.getByRole("list", { name: "Pinned Railpack images" });
  expect(within(list).getAllByRole("listitem").map((item) => item.textContent)).toEqual(pins);
  expect(screen.getByText("0.43.1")).toBeInTheDocument();
});

it("confirms the change to Cloud Native Buildpacks and saves the exact six-field payload", async () => {
  const user = userEvent.setup();
  serve();
  renderSetting();
  await screen.findByRole("combobox", { name: "Build engine" });
  await choose(user, "Build engine", "Cloud Native Buildpacks");
  expect(screen.queryByLabelText(/^Output directory/)).not.toBeInTheDocument();
  expect(screen.queryByLabelText(/^Build command/)).not.toBeInTheDocument();
  expect(screen.queryByLabelText(/^Start command/)).not.toBeInTheDocument();
  // No builder override, buildpack or extension inputs: only the engine, the service and the source directory.
  expect([...screen.getAllByRole("combobox"), ...screen.getAllByRole("textbox")].map((field) => field.getAttribute("id"))).toEqual(["build-recipe-engine", "build-recipe-service", "build-recipe-directory"]);
  await choose(user, "Service", "web");
  await type(user, "Source directory", "app");
  await user.click(saveButton());

  const dialog = await screen.findByRole("dialog", { name: "Change the build engine?" });
  expect(dialog).toHaveTextContent("From Existing Compose / Dockerfile to Cloud Native Buildpacks.");
  expect(dialog).toHaveTextContent("replace that service's existing Compose build section or Dockerfile with Cloud Native Buildpacks");
  expect(dialog).toHaveTextContent("experimental upstream");
  expect(puts()).toHaveLength(0);
  await user.click(within(dialog).getByRole("button", { name: "Change build engine" }));

  await waitFor(() => expect(puts()).toEqual([["PUT", "/api/workloads/w1/build-recipe", { engine: "buildpacks", service: "web", directory: "app", output: "", build_command: "", start_command: "" }]]));
  expect(await screen.findByText(/Build recipe saved/)).toBeInTheDocument();
});

it("lets jobs choose Cloud Native Buildpacks", async () => {
  const user = userEvent.setup();
  serve();
  renderSetting(workload({ kind: "job" }));
  await user.click(await screen.findByRole("combobox", { name: "Build engine" }));
  expect(screen.getByRole("option", { name: "Cloud Native Buildpacks" })).not.toHaveAttribute("aria-disabled");
});

it.each([
  ["an older server that omits it", legacySettings(), "Cloud Native Buildpacks (eligibility not reported)", /did not report its eligibility/],
  ["an unreadable buildpacks object", { ...legacySettings(), buildpacks: { ...buildpacks(), work_target_bytes: "lots" } }, "Cloud Native Buildpacks (eligibility not reported)", /did not report its eligibility/],
  ["an unrecognised work storage mode", { ...legacySettings(), buildpacks: { ...buildpacks(), work_storage: "tmpfs" } }, "Cloud Native Buildpacks (eligibility not reported)", /did not report its eligibility/],
  ["a hard storage cap it does not describe", { ...legacySettings(), buildpacks: { ...buildpacks(), work_hard_cap: true } }, "Cloud Native Buildpacks (eligibility not reported)", /did not report its eligibility/],
  ["an invalid CPU reservation", { ...legacySettings(), buildpacks: { ...buildpacks(), quota_cpu: "2.5" } }, "Cloud Native Buildpacks (eligibility not reported)", /did not report its eligibility/],
  ["an old worker", settings({ buildpacks: buildpacks({ supported: false, reason: "upgrade the build worker to one that reports the buildpacks capability" }) }), "Cloud Native Buildpacks (unavailable on the build node)", /reports the buildpacks capability/],
  ["too little memory", settings({ buildpacks: buildpacks({ supported: false, reason: "the build node has 6 GiB available; Cloud Native Buildpacks needs 9 GiB" }) }), "Cloud Native Buildpacks (unavailable on the build node)", /needs 9 GiB/],
])("keeps Cloud Native Buildpacks unavailable for %s", async (_case, read, option, hint) => {
  const user = userEvent.setup();
  serve({ read: () => Promise.resolve(read) });
  renderSetting();
  await user.click(await screen.findByRole("combobox", { name: "Build engine" }));
  expect(screen.getByRole("option", { name: option })).toHaveAttribute("aria-disabled", "true");
  expect(screen.getByRole("option", { name: "Railpack" })).not.toHaveAttribute("aria-disabled");
  await user.keyboard("{Escape}");
  expect(screen.getByText(hint)).toBeInTheDocument();
});

it("keeps Cloud Native Buildpacks unavailable without a ready Git source", async () => {
  const user = userEvent.setup();
  serve({ read: () => Promise.resolve(settings({ source_ready: false })) });
  renderSetting();
  await user.click(await screen.findByRole("combobox", { name: "Build engine" }));
  expect(screen.getByRole("option", { name: /^Cloud Native Buildpacks \(needs a Git source/ })).toHaveAttribute("aria-disabled", "true");
});

it("keeps an unavailable saved Cloud Native Buildpacks recipe selected and lets Compose clear it", async () => {
  const user = userEvent.setup();
  const reason = "the build node runs on linux/s390x; the pinned builder supports linux/amd64 and linux/arm64";
  serve({ read: () => Promise.resolve(settings({ recipe: cnbRecipe, reason, buildpacks: buildpacks({ supported: false, reason }) })) });
  renderSetting();
  expect(await screen.findByText("The saved Cloud Native Buildpacks recipe cannot build")).toBeInTheDocument();
  expect(screen.getByText(reason)).toBeInTheDocument();
  expect(screen.getByText(/not changed automatically/)).toBeInTheDocument();
  expect(screen.getByRole("combobox", { name: "Build engine" })).toHaveTextContent("Cloud Native Buildpacks (unavailable on the build node)");
  expect(screen.getByLabelText(/^Source directory/)).toHaveValue("app");
  await choose(user, "Build engine", "Existing Compose / Dockerfile");
  await user.click(saveButton());
  await user.click(within(await screen.findByRole("dialog")).getByRole("button", { name: "Change build engine" }));
  await waitFor(() => expect(puts()).toEqual([["PUT", "/api/workloads/w1/build-recipe", composeRecipe]]));
});

it("treats a saved Cloud Native Buildpacks recipe as unable to build when the server omits its eligibility", async () => {
  serve({ read: () => Promise.resolve(legacySettings({ recipe: cnbRecipe })) });
  renderSetting();
  expect(await screen.findByText("The saved Cloud Native Buildpacks recipe cannot build")).toBeInTheDocument();
  expect(screen.getByText(/did not report Cloud Native Buildpacks eligibility/)).toBeInTheDocument();
  expect(screen.getByRole("combobox", { name: "Build engine" })).toHaveTextContent("Cloud Native Buildpacks");
  expect(screen.getByText(/did not report the resource needs, versions or pinned images/)).toBeInTheDocument();
});

it("shows the Cloud Native Buildpacks disclosures with the server's numbers, versions and pins", async () => {
  const user = userEvent.setup();
  serve();
  renderSetting();
  await screen.findByRole("combobox", { name: "Build engine" });
  expect(screen.queryByText(/pinned official Paketo builder/)).not.toBeInTheDocument();
  await choose(user, "Build engine", "Cloud Native Buildpacks");
  expect(screen.getByText(/No repository-selected replacement builder, external or inline buildpacks or image extensions are supported/)).toBeInTheDocument();
  expect(screen.getByText("The selected service must be the only Compose service with a build definition. Other image-only services are unchanged.")).toBeInTheDocument();
  expect(screen.getByText(/without a Docker socket or registry, Git or host credentials/)).toBeInTheDocument();
  expect(screen.getByText(/Detection gets no Cove secret values/)).toBeInTheDocument();
  expect(screen.getByText(/OCI-layout export is experimental upstream/)).toBeInTheDocument();
  expect(screen.getByText(/A build network of none is enforced/)).toBeInTheDocument();
  expect(screen.getByText(/neither reproducible nor fully pinned/)).toBeInTheDocument();
  expect(screen.getByText(/no persistent Cloud Native Buildpacks cache/)).toBeInTheDocument();
  expect(screen.getByText(/not a promise that it runs under resource pressure/)).toBeInTheDocument();
  const needs = screen.getByRole("list", { name: "Cloud Native Buildpacks resource needs" });
  expect(needs).toHaveTextContent("Work is operation-owned disk state with a monitored 3 GiB target, not a hard storage cap.");
  expect(needs).toHaveTextContent("Cove cancels the build when it observes the target overrun or node resource pressure; brief growth between checks is possible.");
  expect(needs).toHaveTextContent("separate from the BuildKit hard cache limits");
  expect(needs).toHaveTextContent("at least 4 GiB of actually available memory");
  expect(needs).toHaveTextContent("available memory minus 2 GiB of safety headroom, clamped between 2 GiB and 4 GiB and rounded down to a whole MiB");
  expect(needs).toHaveTextContent("recorded in the operation and build history, and a build that exceeds it can fail. There is no operator memory setting.");
  expect(needs).toHaveTextContent("24 GiB of free Docker storage and 10 GiB of free worker-state storage");
  expect(needs).toHaveTextContent("about 1.45 GB to download and about 5.11 GB once unpacked");
  expect(needs).toHaveTextContent("phase memory limit plus helper overhead, up to 4,352 MiB, and 2.5 CPUs including helpers. The memory figure is a maximum");
  expect(needs).not.toHaveTextContent(/of RAM|\b2 CPUs/);
  const list = screen.getByRole("list", { name: "Pinned Cloud Native Buildpacks images" });
  const items = within(list).getAllByRole("listitem");
  expect(items.map((item) => item.textContent)).toEqual(cnbPins);
  expect(items[0]).toHaveClass("break-all");
  expect(screen.getByText("0.21.29")).toBeInTheDocument();
  expect(screen.getByText("0.0.230")).toBeInTheDocument();
  expect(screen.getByText("0.13")).toBeInTheDocument();
});

it("renders whatever sizes the server reports instead of built-in numbers", async () => {
  const other = buildpacks({
    work_target_bytes: 5 * 1024 ** 3,
    minimum_phase_memory_bytes: 1536 * 1024 ** 2,
    maximum_phase_memory_bytes: 6 * 1024 ** 3,
    memory_safety_bytes: 3 * 1024 ** 3,
    required_available_memory_bytes: 7 * 1024 ** 3,
    required_docker_disk_bytes: 30 * 1024 ** 3,
    required_worker_disk_bytes: 12 * 1024 ** 3,
    quota_memory_mb: 6656,
    quota_cpu: 1.75,
    builder_download_bytes: 2_000_000_000,
    builder_unpacked_bytes: 6_500_000_000,
  });
  serve({ read: () => Promise.resolve(settings({ recipe: cnbRecipe, buildpacks: other })) });
  renderSetting();
  const needs = await screen.findByRole("list", { name: "Cloud Native Buildpacks resource needs" });
  expect(needs).toHaveTextContent("monitored 5 GiB target");
  expect(needs).toHaveTextContent("at least 7 GiB of actually available memory");
  expect(needs).toHaveTextContent("minus 3 GiB of safety headroom, clamped between 1,536 MiB and 6 GiB");
  expect(needs).toHaveTextContent("30 GiB of free Docker storage and 12 GiB of free worker-state storage");
  expect(needs).toHaveTextContent("about 2.00 GB to download and about 6.50 GB once unpacked");
  expect(needs).toHaveTextContent("up to 6,656 MiB, and 1.75 CPUs including helpers");
  expect(needs).not.toHaveTextContent(/\b3 GiB target|\b24 GiB|4,352 MiB|2\.5 CPUs|1\.45 GB|5\.11 GB/);
});

it.each([
  [400, "Cloud Native Buildpacks cannot use the custom build network of the selected service", "error"],
  [409, "wait for active operations before changing the build recipe", "warn"],
])("keeps the Cloud Native Buildpacks draft when the server answers %i", async (status, message) => {
  const user = userEvent.setup();
  serve({ read: () => Promise.resolve(settings({ recipe: cnbRecipe })), save: () => Promise.reject(new ApiError(status, message)) });
  renderSetting();
  await screen.findByRole("combobox", { name: "Build engine" });
  await type(user, "Source directory", "services/web");
  await user.click(saveButton());
  expect(await screen.findByText("The build recipe was not saved")).toBeInTheDocument();
  expect(screen.getByText(message)).toBeInTheDocument();
  expect(puts()).toEqual([["PUT", "/api/workloads/w1/build-recipe", { engine: "buildpacks", service: "web", directory: "services/web", output: "", build_command: "", start_command: "" }]]);
  expect(screen.getByLabelText(/^Source directory/)).toHaveValue("services/web");
  expect(screen.getByRole("status")).toHaveTextContent("Unsaved changes");
  expect(screen.queryByText(/Build recipe saved/)).not.toBeInTheDocument();
});

it("keeps the Cloud Native Buildpacks success notice while the refreshed recipe is still loading", async () => {
  const user = userEvent.setup();
  let hold = false;
  serve({
    read: () => (hold ? new Promise(() => undefined) : Promise.resolve(settings({ recipe: cnbRecipe }))),
    save: () => {
      hold = true;
      return Promise.resolve({ ok: true });
    },
  });
  renderSetting();
  await screen.findByRole("combobox", { name: "Build engine" });
  await type(user, "Source directory", ".");
  await user.click(saveButton());
  expect(await screen.findByText(/Build recipe saved/)).toBeInTheDocument();
  expect(screen.getByLabelText(/^Source directory/)).toHaveValue(".");
});

it("shows readers the saved Cloud Native Buildpacks recipe and its disclosures read-only", async () => {
  serve({ read: () => Promise.resolve(settings({ can_edit: false, recipe: cnbRecipe })) });
  renderSetting(workload({ permissions: ["read", "operate"] }));
  expect(await screen.findByText("Cloud Native Buildpacks")).toBeInTheDocument();
  expect(screen.getByText("web")).toBeInTheDocument();
  expect(screen.getByText("app")).toBeInTheDocument();
  expect(screen.getByRole("list", { name: "Pinned Cloud Native Buildpacks images" })).toBeInTheDocument();
  expect(screen.queryByText(/Build command|Start command|Output directory/)).not.toBeInTheDocument();
  expect(screen.queryByRole("combobox")).not.toBeInTheDocument();
  expect(screen.queryByRole("textbox")).not.toBeInTheDocument();
  expect(screen.queryByRole("button", { name: "Save build recipe" })).not.toBeInTheDocument();
});

// A read without the nixpacks object, as an older server sends it.
function withoutNixpacks(overrides: Partial<BuildRecipeSettings> = {}): BuildRecipeSettings {
  const read = settings(overrides);
  delete read.nixpacks;
  return read;
}

it("confirms the change to Nixpacks, clears the static output and saves the exact six-field payload", async () => {
  const user = userEvent.setup();
  serve({ read: () => Promise.resolve(settings({ recipe: { ...composeRecipe, engine: "static", service: "web", directory: "site", output: "dist" } })) });
  renderSetting();
  await screen.findByRole("combobox", { name: "Build engine" });
  await choose(user, "Build engine", "Nixpacks");
  expect(screen.queryByLabelText(/^Output directory/)).not.toBeInTheDocument();
  expect(screen.getByLabelText(/^Build command/)).toHaveValue("");
  expect(screen.getByText("Overrides the build command Nixpacks plans. Leave empty to use its plan.")).toBeInTheDocument();
  await type(user, "Start command", "./start.sh");
  await user.click(saveButton());
  const dialog = await screen.findByRole("dialog", { name: "Change the build engine?" });
  expect(dialog).toHaveTextContent("From Static output to Nixpacks.");
  expect(dialog).toHaveTextContent("Nixpacks is in maintenance mode upstream, which recommends Railpack.");
  expect(dialog).toHaveTextContent("outbound build proxies are unavailable");
  expect(puts()).toHaveLength(0);
  await user.click(within(dialog).getByRole("button", { name: "Change build engine" }));
  await waitFor(() => expect(puts()).toEqual([["PUT", "/api/workloads/w1/build-recipe", { engine: "nixpacks", service: "web", directory: "site", output: "", build_command: "", start_command: "./start.sh" }]]));
  expect(await screen.findByText(/Build recipe saved/)).toBeInTheDocument();
});

it("saves a Nixpacks recipe without commands and asks before going back to Compose", async () => {
  const user = userEvent.setup();
  serve({ read: () => Promise.resolve(settings({ recipe: { ...nixRecipe, build_command: "make", start_command: "./run" }, build_command_set: true, start_command_set: true })) });
  renderSetting();
  await screen.findByRole("combobox", { name: "Build engine" });
  await type(user, "Build command", "");
  await type(user, "Start command", "");
  await user.click(saveButton());
  await waitFor(() => expect(puts()).toEqual([["PUT", "/api/workloads/w1/build-recipe", nixRecipe]]));
  expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
  await choose(user, "Build engine", "Existing Compose / Dockerfile");
  await user.click(saveButton());
  await user.click(within(await screen.findByRole("dialog")).getByRole("button", { name: "Change build engine" }));
  await waitFor(() => expect(puts()[1]).toEqual(["PUT", "/api/workloads/w1/build-recipe", composeRecipe]));
});

it("lets jobs choose Nixpacks", async () => {
  const user = userEvent.setup();
  serve();
  renderSetting(workload({ kind: "job" }));
  await user.click(await screen.findByRole("combobox", { name: "Build engine" }));
  expect(screen.getByRole("option", { name: "Nixpacks" })).not.toHaveAttribute("aria-disabled");
});

it("refuses oversized Nixpacks commands before saving", async () => {
  const user = userEvent.setup();
  serve({ read: () => Promise.resolve(settings({ recipe: nixRecipe })) });
  renderSetting();
  await screen.findByRole("combobox", { name: "Build engine" });
  await user.click(screen.getByLabelText(/^Start command/));
  await user.paste("x".repeat(8193));
  await user.click(saveButton());
  expect(screen.getByText("Use at most 8192 bytes (currently 8193).")).toBeInTheDocument();
  expect(screen.getByLabelText(/^Start command/)).toHaveAttribute("aria-invalid", "true");
  expect(puts()).toHaveLength(0);
});

const unknownArch = "the pinned Nixpacks planner requires a Linux amd64 or arm64 build node";

it.each([
  ["an older server that omits it", withoutNixpacks(), "Nixpacks (eligibility unknown)", /did not report usable eligibility/],
  ["an unreadable nixpacks object", { ...withoutNixpacks(), nixpacks: { ...nixpacks(), planner_timeout_seconds: "60" } }, "Nixpacks (eligibility unknown)", /did not report usable eligibility/],
  ["an unrecognised maintenance flag", { ...withoutNixpacks(), nixpacks: { ...nixpacks(), maintenance: false } }, "Nixpacks (eligibility unknown)", /did not report usable eligibility/],
  ["a proxy flag it does not describe", { ...withoutNixpacks(), nixpacks: { ...nixpacks(), proxy_supported: true } }, "Nixpacks (eligibility unknown)", /did not report usable eligibility/],
  ["a supported status without a pinned planner", { ...withoutNixpacks(), nixpacks: { ...nixpacks(), planner_sha256: "", planner_download_bytes: 0 } }, "Nixpacks (eligibility unknown)", /did not report usable eligibility/],
  ["an old worker", settings({ nixpacks: nixpacks({ supported: false, reason: "upgrade and verify the effective build worker before using Nixpacks" }) }), "Nixpacks (unavailable on the build node)", /before using Nixpacks/],
  ["an unknown architecture", settings({ nixpacks: nixpacks({ supported: false, reason: unknownArch, planner_sha256: "", planner_download_bytes: 0, base_download_bytes: 0 }) }), "Nixpacks (unavailable on the build node)", /requires a Linux amd64 or arm64/],
])("keeps Nixpacks unavailable for %s while the other engines stay usable", async (_case, read, option, hint) => {
  const user = userEvent.setup();
  serve({ read: () => Promise.resolve(read) });
  renderSetting();
  await user.click(await screen.findByRole("combobox", { name: "Build engine" }));
  expect(screen.getByRole("option", { name: option })).toHaveAttribute("aria-disabled", "true");
  expect(screen.getByRole("option", { name: "Railpack" })).not.toHaveAttribute("aria-disabled");
  expect(screen.getByRole("option", { name: "Cloud Native Buildpacks" })).not.toHaveAttribute("aria-disabled");
  await user.keyboard("{Escape}");
  expect(screen.getByText(hint)).toBeInTheDocument();
});

it("keeps Nixpacks unavailable without a ready Git source", async () => {
  const user = userEvent.setup();
  serve({ read: () => Promise.resolve(settings({ source_ready: false })) });
  renderSetting();
  await user.click(await screen.findByRole("combobox", { name: "Build engine" }));
  expect(screen.getByRole("option", { name: /^Nixpacks \(needs a Git source/ })).toHaveAttribute("aria-disabled", "true");
  await user.keyboard("{Escape}");
  expect(screen.getByText(/Cloud Native Buildpacks and Nixpacks need a Git source/)).toBeInTheDocument();
});

it("keeps an unsupported saved Nixpacks recipe selected, shows the top-level reason and lets Compose clear it", async () => {
  const user = userEvent.setup();
  const reason = "Nixpacks is unavailable while an outbound build proxy is configured";
  serve({ read: () => Promise.resolve(settings({ recipe: nixRecipe, reason, nixpacks: nixpacks({ supported: false, reason: unknownArch }) })) });
  renderSetting();
  expect(await screen.findByText("The saved Nixpacks recipe cannot build")).toBeInTheDocument();
  expect(screen.getByText(reason)).toBeInTheDocument();
  expect(screen.queryByText(unknownArch)).not.toBeInTheDocument();
  expect(screen.getByText(/not changed automatically/)).toBeInTheDocument();
  expect(screen.getByRole("combobox", { name: "Build engine" })).toHaveTextContent("Nixpacks (unavailable on the build node)");
  expect(screen.getByLabelText(/^Source directory/)).toHaveValue("app");
  await choose(user, "Build engine", "Existing Compose / Dockerfile");
  await user.click(saveButton());
  await user.click(within(await screen.findByRole("dialog")).getByRole("button", { name: "Change build engine" }));
  await waitFor(() => expect(puts()).toEqual([["PUT", "/api/workloads/w1/build-recipe", composeRecipe]]));
});

it("treats a saved Nixpacks recipe as unable to build when the server omits its eligibility", async () => {
  serve({ read: () => Promise.resolve(withoutNixpacks({ recipe: nixRecipe })) });
  renderSetting();
  expect(await screen.findByText("The saved Nixpacks recipe cannot build")).toBeInTheDocument();
  expect(screen.getByText(/did not report usable Nixpacks eligibility, so it is treated as unavailable/)).toBeInTheDocument();
  expect(screen.getByText(/did not report usable Nixpacks eligibility, versions, pins or planner limits/)).toBeInTheDocument();
  expect(screen.queryByRole("list", { name: "Nixpacks resource limits" })).not.toBeInTheDocument();
});

it.each([
  [400, "Nixpacks is unavailable while an outbound build proxy is configured"],
  [400, "Nixpacks cannot use Compose build args on the selected service"],
  [409, "wait for active operations before changing the build recipe"],
])("keeps the Nixpacks draft when the server answers %i: %s", async (status, message) => {
  const user = userEvent.setup();
  serve({ save: () => Promise.reject(new ApiError(status, message)) });
  renderSetting();
  await screen.findByRole("combobox", { name: "Build engine" });
  await choose(user, "Build engine", "Nixpacks");
  await choose(user, "Service", "web");
  await type(user, "Source directory", "app");
  await type(user, "Build command", "npm run build");
  await user.click(saveButton());
  await user.click(within(await screen.findByRole("dialog")).getByRole("button", { name: "Change build engine" }));
  expect(await screen.findByText("The build recipe was not saved")).toBeInTheDocument();
  expect(screen.getByText(message)).toBeInTheDocument();
  expect(puts()).toEqual([["PUT", "/api/workloads/w1/build-recipe", { engine: "nixpacks", service: "web", directory: "app", output: "", build_command: "npm run build", start_command: "" }]]);
  expect(screen.getByRole("combobox", { name: "Build engine" })).toHaveTextContent("Nixpacks");
  expect(screen.getByLabelText(/^Build command/)).toHaveValue("npm run build");
  expect(screen.getByRole("status")).toHaveTextContent("Unsaved changes");
  expect(screen.queryByText(/Build recipe saved/)).not.toBeInTheDocument();
});

it("shows the Nixpacks disclosures with the server's version, pins and planner limits", async () => {
  const user = userEvent.setup();
  serve();
  renderSetting();
  await screen.findByRole("combobox", { name: "Build engine" });
  expect(screen.queryByText("Nixpacks is in maintenance mode")).not.toBeInTheDocument();
  await choose(user, "Build engine", "Nixpacks");
  expect(screen.getByText("Nixpacks is in maintenance mode")).toBeInTheDocument();
  expect(screen.getByText(/its maintainers recommend Railpack instead. Cove still supports it as an explicit choice/)).toBeInTheDocument();
  expect(screen.getByText(/normal builds download Nix, system and language packages from upstream/)).toBeInTheDocument();
  expect(screen.getByText(/neither reproducible nor fully pinned/)).toBeInTheDocument();
  expect(screen.getByText(/Planning runs offline in an unprivileged, bounded container with no Docker socket, no Git, registry or host credentials and no Cove secret values/)).toBeInTheDocument();
  expect(screen.getByText(/generated build.sh is never executed/)).toBeInTheDocument();
  expect(screen.getByText(/A build network of none is enforced for the generated RUN steps, so dependency downloads can fail/)).toBeInTheDocument();
  expect(screen.getByText(/Outbound build proxies are unavailable because the shared builder cannot enforce proxy-only egress/)).toBeInTheDocument();
  expect(screen.getByText(/never planner inputs or ordinary ARG or ENV values/)).toBeInTheDocument();
  expect(screen.getByText(/does not forward environment variables as Nixpacks variables/)).toBeInTheDocument();
  expect(screen.getByText(/is reserved for generated output, so source that contains it is refused/)).toBeInTheDocument();
  expect(screen.getByText(/Cove does not accept arbitrary Dockerfiles here/)).toBeInTheDocument();
  expect(screen.queryByText(/proxy support|proxies are applied|reproducible builds/i)).not.toBeInTheDocument();
  const limits = screen.getByRole("list", { name: "Nixpacks resource limits" });
  expect(limits).toHaveTextContent("Planning is limited to 512 MiB of memory, 1 CPU and 60 seconds.");
  expect(limits).toHaveTextContent("Up to 128 MiB of planner output is held in temporary memory within that limit; it is not the build cache limit.");
  expect(limits).toHaveTextContent("The planner archive is about 3.7 MB to download and the base image about 0.14 GB.");
  expect(screen.getByText("Planner archive checksum").nextElementSibling).toHaveTextContent(nixDigest);
  expect(screen.getByText("Base image").nextElementSibling).toHaveTextContent(nixBase);
  expect(screen.getByText("1.41.0")).toBeInTheDocument();
});

it("renders whatever Nixpacks values the server reports instead of built-in numbers", async () => {
  const armDigest = `sha256:${"91".repeat(32)}`;
  const armBase = `ghcr.io/railwayapp/nixpacks@sha256:${"ab".repeat(32)}`;
  const other = nixpacks({ version: "1.42.0", planner_sha256: armDigest, planner_download_bytes: 4111227, base_image: armBase, base_download_bytes: 1_235_771_180, planner_memory_bytes: 768 * 1024 ** 2, planner_cpu: 1.5, planner_timeout_seconds: 90, planner_output_bytes: 64 * 1024 ** 2 });
  serve({ read: () => Promise.resolve(settings({ recipe: nixRecipe, nixpacks: other })) });
  renderSetting();
  const limits = await screen.findByRole("list", { name: "Nixpacks resource limits" });
  expect(limits).toHaveTextContent("Planning is limited to 768 MiB of memory, 1.5 CPUs and 90 seconds.");
  expect(limits).toHaveTextContent("Up to 64 MiB of planner output");
  expect(limits).toHaveTextContent("about 4.1 MB to download and the base image about 1.24 GB");
  expect(limits).not.toHaveTextContent(/512 MiB|60 seconds|128 MiB|3\.7 MB|0\.14 GB/);
  expect(screen.getByText("1.42.0")).toBeInTheDocument();
  expect(screen.getByText(armDigest)).toBeInTheDocument();
  expect(screen.getByText(armBase)).toBeInTheDocument();
  expect(screen.queryByText(nixDigest)).not.toBeInTheDocument();
  expect(screen.queryByText(nixBase)).not.toBeInTheDocument();
});

it("shows an unpinned architecture's Nixpacks metadata without claiming download sizes or a checksum", async () => {
  serve({ read: () => Promise.resolve(settings({ recipe: nixRecipe, reason: unknownArch, nixpacks: nixpacks({ supported: false, reason: unknownArch, planner_sha256: "", planner_download_bytes: 0, base_download_bytes: 0 }) })) });
  renderSetting();
  const limits = await screen.findByRole("list", { name: "Nixpacks resource limits" });
  expect(limits).toHaveTextContent("No planner archive is pinned for this build node's architecture, so download sizes are not known.");
  expect(screen.queryByText("Planner archive checksum")).not.toBeInTheDocument();
  expect(screen.getByText(nixBase)).toBeInTheDocument();
});

it("blocks Nixpacks saves during an active operation", async () => {
  const user = userEvent.setup();
  serve({ read: () => Promise.resolve(settings({ recipe: nixRecipe, active_operation_id: "op1" })) });
  renderSetting();
  expect(await screen.findByText("An operation is in progress")).toBeInTheDocument();
  await type(user, "Source directory", "web");
  expect(saveButton()).toBeDisabled();
  expect(puts()).toHaveLength(0);
});

it("blocks Nixpacks saves when operations cannot be read", async () => {
  serve({ read: () => Promise.resolve(settings({ recipe: nixRecipe })), operations: () => Promise.reject(new Error("down")) });
  renderSetting();
  expect(await screen.findByText("Operations could not be read")).toBeInTheDocument();
  expect(puts()).toHaveLength(0);
});

it("blocks Nixpacks saves while offline and keeps the draft", async () => {
  const user = userEvent.setup();
  serve({ read: () => Promise.resolve(settings({ recipe: nixRecipe })) });
  renderSetting();
  await screen.findByRole("combobox", { name: "Build engine" });
  await type(user, "Build command", "make");
  vi.spyOn(navigator, "onLine", "get").mockReturnValue(false);
  act(() => void window.dispatchEvent(new Event("offline")));
  expect(await screen.findByText("You are offline")).toBeInTheDocument();
  expect(saveButton()).toBeDisabled();
  expect(screen.getByLabelText(/^Build command/)).toHaveValue("make");
  expect(puts()).toHaveLength(0);
});

it("hides a stale Nixpacks recipe after a failed refresh and keeps the draft", async () => {
  const user = userEvent.setup();
  let fail = false;
  serve({ read: () => (fail ? Promise.reject(new Error("refresh failed")) : Promise.resolve(settings({ recipe: nixRecipe }))) });
  const client = renderSetting();
  await screen.findByRole("combobox", { name: "Build engine" });
  await type(user, "Start command", "./run");
  fail = true;
  await client.refetchQueries({ queryKey: ["workloads", "w1", "build-recipe"] });
  expect(await screen.findByText("Could not load the current build recipe")).toBeInTheDocument();
  expect(screen.queryByText("Nixpacks is in maintenance mode")).not.toBeInTheDocument();
  expect(saveButton()).toBeDisabled();
  fail = false;
  await user.click(screen.getByRole("button", { name: "Retry" }));
  expect(await screen.findByLabelText(/^Start command/)).toHaveValue("./run");
});

it("keeps the Nixpacks success notice while the refreshed recipe is still loading", async () => {
  const user = userEvent.setup();
  let hold = false;
  serve({
    read: () => (hold ? new Promise(() => undefined) : Promise.resolve(settings({ recipe: nixRecipe }))),
    save: () => {
      hold = true;
      return Promise.resolve({ ok: true });
    },
  });
  renderSetting();
  await screen.findByRole("combobox", { name: "Build engine" });
  await type(user, "Build command", "make");
  await user.click(saveButton());
  expect(await screen.findByText(/Build recipe saved/)).toBeInTheDocument();
  expect(screen.getByLabelText(/^Build command/)).toHaveValue("make");
});

it("shows readers the saved Nixpacks recipe and its disclosures without command text", async () => {
  serve({ read: () => Promise.resolve(settings({ can_edit: false, recipe: nixRecipe, build_command_set: true })) });
  renderSetting(workload({ permissions: ["read", "operate"] }));
  expect(await screen.findByText("Nixpacks is in maintenance mode")).toBeInTheDocument();
  expect(screen.getByText("Custom command saved")).toBeInTheDocument();
  expect(screen.getByText("Nixpacks plan")).toBeInTheDocument();
  expect(screen.getByRole("list", { name: "Nixpacks resource limits" })).toBeInTheDocument();
  expect(screen.queryByRole("combobox")).not.toBeInTheDocument();
  expect(screen.queryByRole("textbox")).not.toBeInTheDocument();
  expect(screen.queryByRole("button", { name: "Save build recipe" })).not.toBeInTheDocument();
});

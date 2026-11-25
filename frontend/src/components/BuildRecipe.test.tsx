import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { ReactNode } from "react";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import type { BuildRecipe, BuildRecipeSettings, BuildpacksSettings } from "../api/build-recipes";
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

const editor: Permission[] = ["read", "operate", "edit", "edit_configuration"];
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
  expect(screen.getAllByRole("option").map((option) => option.textContent)).toEqual(["Existing Compose / Dockerfile", "Static output", "Railpack", "Cloud Native Buildpacks"]);
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
  serve({ read: () => Promise.resolve(settings({ recipe: { ...composeRecipe, engine: "nixpacks", service: "web", directory: "." } })) });
  renderSetting();
  expect(await screen.findByText("The saved build engine is not recognised")).toBeInTheDocument();
  expect(screen.getByRole("combobox", { name: "Build engine" })).toHaveTextContent("Unknown engine (nixpacks)");
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

it("keeps the recipe read-only without Edit configuration", async () => {
  serve();
  renderSetting(workload({ permissions: ["read", "operate", "edit", "read_secrets"] }));
  expect(await screen.findByText("Existing Compose / Dockerfile")).toBeInTheDocument();
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

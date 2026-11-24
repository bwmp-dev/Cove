import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { ReactNode } from "react";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import type { BuildRecipe, BuildRecipeSettings } from "../api/build-recipes";
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
    ...overrides,
  };
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
  expect(screen.getAllByRole("option").map((option) => option.textContent)).toEqual(["Existing Compose / Dockerfile", "Static output", "Railpack"]);
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

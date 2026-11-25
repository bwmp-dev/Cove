import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import type { ReactNode } from "react";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { ApiError, request } from "../api/client";
import type { ImageUpdatePolicyStatus } from "../api/image-updates";
import type { RegistryWebhookStatus } from "../api/registry-webhook";
import type { Workload } from "../api/types";
import { RegistryWebhook } from "./RegistryWebhook";

vi.mock("../api/client", async (original) => ({ ...(await original<typeof import("../api/client")>()), request: vi.fn() }));
vi.mock("./SaveBar", () => ({ UnsavedChangesGuard: () => null }));
vi.mock("@tanstack/react-router", () => ({ Link: ({ children }: { children: ReactNode }) => <a href="/">{children}</a> }));
beforeEach(() => vi.stubGlobal("ResizeObserver", class { observe = vi.fn(); disconnect = vi.fn(); }));
afterEach(() => {
  vi.clearAllMocks();
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
  localStorage.clear();
  sessionStorage.clear();
});
HTMLDialogElement.prototype.showModal = function (this: HTMLDialogElement) {
  this.open = true;
};
HTMLDialogElement.prototype.close = function (this: HTMLDialogElement) {
  this.open = false;
};

// Fixtures only: no registry, provider or worker is contacted, and the tokens are made up.
const token = "fixture-token-0123456789abcdef";
const endpoint = "https://cove.example.test/hooks/registry/h1";
const service = { id: "w1", name: "web", node_id: "n1", kind: "service", permissions: ["read", "operate", "edit", "view", "edit_configuration"], source: null, compose: "" } as unknown as Workload;
const registry = { id: "r1", name: "Team registry", server: "registry.example.test" };
const filters = [{ repository: "team/web", tag: "stable" }];

function status(extra: Partial<RegistryWebhookStatus> = {}): RegistryWebhookStatus {
  return { configuration: null, endpoint: null, registry, matches_saved_registry: false, worker_supported: true, last_received_at: null, last_matched_at: null, pending_at: null, last_operation: null, last_error: "", last_ignored_reason: "", ownership_required: false, ...extra };
}

function savedStatus(extra: Partial<RegistryWebhookStatus> = {}): RegistryWebhookStatus {
  return status({ configuration: { provider: "distribution", enabled: true, filters, registry_id: "r1", registry_server: registry.server }, endpoint, matches_saved_registry: true, ...extra });
}

type Handler = (method: string, path: string, body?: unknown) => unknown;

const policyPath = "/api/workloads/w1/image-update-policy";

function policyStatus(enabled: boolean | null): ImageUpdatePolicyStatus {
  return {
    configuration: enabled === null ? null : { enabled, mode: "auto", rules: [{ service: "web", range: "^1.2.0" }], window_start: "", window_end: "" },
    services: [{ service: "web", image: "registry.example.test/team/web:1.2.0", mutable: true }],
    eligible: true,
    unavailable_reason: "",
    worker_supported: true,
    node_ready: true,
    active_operation_id: null,
    last_check_id: null,
    last_operation_id: null,
    last_error: "",
    checked_at: null,
    stale: true,
    updates: null,
    ownership_required: false,
  };
}

// The webhook section also reads the image update policy; by default no policy exists.
function serve(handler: Handler, policy: () => unknown = () => policyStatus(null)) {
  vi.mocked(request).mockImplementation((method: string, path: string, body?: unknown) => {
    try {
      if (path === policyPath) return Promise.resolve(policy());
      return Promise.resolve(handler(method, path, body));
    } catch (error) {
      return Promise.reject(error instanceof Error ? error : new Error(String(error)));
    }
  });
}

function renderWebhook(workload: Workload = service) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(
    <QueryClientProvider client={client}>
      <RegistryWebhook workload={workload} />
    </QueryClientProvider>,
  );
  return client;
}

const calls = (method: string) => vi.mocked(request).mock.calls.filter(([callMethod, path]) => callMethod === method && path !== policyPath);

// Nothing durable or cached may hold the one-time token.
function expectTokenOnlyOnScreen(client: QueryClient) {
  expect(JSON.stringify(client.getQueryCache().getAll().map((query) => query.state.data))).not.toContain(token);
  expect(JSON.stringify(client.getMutationCache().getAll().map((mutation) => mutation.state.data))).not.toContain(token);
  expect(JSON.stringify({ ...localStorage }) + JSON.stringify({ ...sessionStorage }) + location.href).not.toContain(token);
}

async function fillFilter(repository: string, tag: string) {
  fireEvent.change(await screen.findByLabelText("Repository 1"), { target: { value: repository } });
  fireEvent.change(screen.getByLabelText("Tag 1"), { target: { value: tag } });
}

it("creates a disabled webhook by default and shows the one-time token only in local state", async () => {
  let saved = status();
  serve((method, _path, body) => {
    if (method === "PUT") {
      saved = savedStatus({ configuration: { ...(body as { provider: "distribution"; enabled: boolean; filters: typeof filters }), registry_id: "r1", registry_server: registry.server } });
      return { ...saved, token };
    }
    return saved;
  });
  const client = renderWebhook();
  expect(await screen.findByText(/queues an ordinary deploy that pulls the resource’s configured image tags and redeploys\. Compose and tag settings stay unchanged/)).toBeInTheDocument();
  expect(await screen.findByRole("switch", { name: "Pull and redeploy on push" })).not.toBeChecked();
  await fillFilter("team/web", "stable");
  fireEvent.click(screen.getByRole("button", { name: "Create webhook" }));

  await waitFor(() => expect(request).toHaveBeenCalledWith("PUT", "/api/workloads/w1/registry-webhook", { provider: "distribution", enabled: false, filters }));
  expect(await screen.findByLabelText("Webhook token")).toHaveValue(token);
  expect(screen.getByText("Webhook created")).toBeInTheDocument();
  expect(screen.getAllByText(/Authorization: Bearer <token>/).length).toBeGreaterThan(0);
  expectTokenOnlyOnScreen(client);
  await waitFor(() => expect(calls("GET").length).toBeGreaterThanOrEqual(2));
  expectTokenOnlyOnScreen(client);

  fireEvent.click(screen.getByRole("button", { name: "Clear" }));
  expect(screen.queryByLabelText("Webhook token")).not.toBeInTheDocument();
  expect(document.body.textContent).not.toContain(token);
});

it("asks before turning on automatic pull and redeploy", async () => {
  serve((method) => (method === "PUT" ? savedStatus() : savedStatus({ configuration: { provider: "distribution", enabled: false, filters, registry_id: "r1", registry_server: registry.server } })));
  renderWebhook();
  fireEvent.click(await screen.findByRole("switch", { name: "Pull and redeploy on push" }));
  fireEvent.click(screen.getByRole("button", { name: "Save webhook" }));
  const dialog = screen.getByRole("dialog", { name: "Turn on automatic pull and redeploy?" });
  expect(dialog).toHaveTextContent("Approval, freeze windows, deploy windows and quotas still apply");
  expect(dialog).toHaveTextContent("pulls the resource’s configured image tags and redeploys");
  expect(calls("PUT")).toHaveLength(0);
  fireEvent.click(within(dialog).getByRole("button", { name: "Turn on automatic deploys" }));
  await waitFor(() => expect(request).toHaveBeenCalledWith("PUT", "/api/workloads/w1/registry-webhook", { provider: "distribution", enabled: true, filters }));
  expect(await screen.findByText("Webhook saved")).toBeInTheDocument();
});

it("keeps the draft when a save is refused", async () => {
  serve((method) => {
    if (method === "PUT") throw new ApiError(409, "Registry webhooks redeploy image-only Compose services.");
    return status();
  });
  renderWebhook();
  await fillFilter("team/web", "stable");
  fireEvent.click(screen.getByRole("button", { name: "Create webhook" }));
  expect(await screen.findByText("Registry webhooks redeploy image-only Compose services.")).toBeInTheDocument();
  expect(screen.getByText("Your changes are kept.")).toBeInTheDocument();
  expect(screen.getByLabelText("Repository 1")).toHaveValue("team/web");
  expect(screen.getByLabelText("Tag 1")).toHaveValue("stable");
});

it("refuses invalid filters and a provider that does not match the registry before sending anything", async () => {
  serve(() => status());
  renderWebhook();
  await fillFilter("Team/Web", "");
  fireEvent.click(screen.getByRole("combobox", { name: "Provider" }));
  fireEvent.click(screen.getByRole("option", { name: "GitHub Container Registry" }));
  fireEvent.click(screen.getByRole("button", { name: "Create webhook" }));
  expect(await screen.findByText(/GitHub Container Registry needs the resource's registry to be ghcr.io, not registry.example.test/)).toBeInTheDocument();
  expect(screen.getByText(/Use a lowercase repository path/)).toBeInTheDocument();
  expect(screen.getByText(/Enter a tag, or \* for every tag/)).toBeInTheDocument();
  expect(calls("PUT")).toHaveLength(0);
});

it("builds the Docker Hub secret URL only in the one-time reveal and warns that it is a credential", async () => {
  const hub = { id: "r2", name: "Docker Hub", server: "docker.io" };
  let saved = status({ registry: hub });
  serve((method) => {
    if (method === "PUT") {
      saved = status({ registry: hub, configuration: { provider: "docker_hub", enabled: false, filters, registry_id: "r2", registry_server: "docker.io" }, endpoint, matches_saved_registry: true });
      return { ...saved, token };
    }
    return saved;
  });
  const client = renderWebhook();
  expect(await screen.findByRole("combobox", { name: "Provider" })).toHaveTextContent("Docker Hub");
  await fillFilter("team/web", "stable");
  fireEvent.click(screen.getByRole("button", { name: "Create webhook" }));
  await waitFor(() => expect(request).toHaveBeenCalledWith("PUT", "/api/workloads/w1/registry-webhook", { provider: "docker_hub", enabled: false, filters }));
  expect(await screen.findByLabelText("Secret webhook URL")).toHaveValue(`${endpoint}?secret=${token}`);
  expect(screen.getByRole("button", { name: "Copy secret URL" })).toBeInTheDocument();
  expect(screen.getAllByText(/Treat that URL as a credential/).length).toBeGreaterThan(0);
  expect(screen.getAllByText(/Docker Hub does not sign requests/).length).toBeGreaterThan(0);
  expectTokenOnlyOnScreen(client);
  expect(JSON.stringify(client.getQueryCache().getAll().map((query) => query.state.data))).not.toContain("?secret=");
});

it("explains GitHub signature requirements for GHCR without claiming to configure GitHub", async () => {
  const ghcr = { id: "r3", name: "GHCR", server: "ghcr.io" };
  serve(() => status({ registry: ghcr, configuration: { provider: "ghcr", enabled: true, filters, registry_id: "r3", registry_server: "ghcr.io" }, endpoint, matches_saved_registry: true }));
  renderWebhook();
  expect(await screen.findByText(/X-Hub-Signature-256 signature; SHA-1 signatures are refused/)).toBeInTheDocument();
  expect(screen.getByText(/Cove does not configure them/)).toBeInTheDocument();
});

it("shows delivery, pending deploy, last operation, error and ignored reason from the saved webhook", async () => {
  serve(() =>
    savedStatus({
      last_received_at: "2026-11-23T09:00:00Z",
      last_matched_at: "2026-11-23T09:00:00Z",
      pending_at: "2026-11-23T09:00:01Z",
      last_operation: { id: "op9", state: "failed", created_at: "2026-11-23T08:00:00Z" },
      last_error: "Deploy refused: the environment is frozen.",
      last_ignored_reason: "Tag nightly is not watched.",
    }),
  );
  renderWebhook();
  expect(await screen.findByText(/Waiting since/)).toBeInTheDocument();
  expect(screen.getByText("Failed")).toBeInTheDocument();
  expect(screen.getByText("View operation")).toBeInTheDocument();
  expect(screen.getByText("Deploy refused: the environment is frozen.")).toBeInTheDocument();
  expect(screen.getByText("Tag nightly is not watched.")).toBeInTheDocument();
  expect(screen.queryByRole("button", { name: /run|deploy now/i })).not.toBeInTheDocument();
});

it("rotates and removes only after confirmation, with no request body", async () => {
  serve((method) => (method === "POST" ? { ...savedStatus(), token } : method === "DELETE" ? { ok: true } : savedStatus()));
  const client = renderWebhook();
  fireEvent.click(await screen.findByRole("button", { name: "Rotate secret" }));
  expect(calls("POST")).toHaveLength(0);
  fireEvent.click(within(screen.getByRole("dialog", { name: "Rotate the webhook secret?" })).getByRole("button", { name: "Rotate secret" }));
  await waitFor(() => expect(request).toHaveBeenCalledWith("POST", "/api/workloads/w1/registry-webhook/rotate"));
  expect(await screen.findByLabelText("Webhook token")).toHaveValue(token);
  expectTokenOnlyOnScreen(client);

  fireEvent.click(screen.getByRole("button", { name: "Remove webhook" }));
  expect(calls("DELETE")).toHaveLength(0);
  fireEvent.click(within(screen.getByRole("dialog", { name: "Remove the registry webhook?" })).getByRole("button", { name: "Remove webhook" }));
  await waitFor(() => expect(request).toHaveBeenCalledWith("DELETE", "/api/workloads/w1/registry-webhook"));
  expect(await screen.findByText("Webhook removed")).toBeInTheDocument();
  expect(screen.queryByLabelText("Webhook token")).not.toBeInTheDocument();
});

it("ignores pushes after a registry change until a deliberate save issues a new secret", async () => {
  serve(() => savedStatus({ registry: { id: "r9", name: "Other registry", server: "other.example.test" }, matches_saved_registry: false }));
  renderWebhook();
  expect(await screen.findByText("Registry changed")).toBeInTheDocument();
  expect(screen.getByRole("button", { name: "Rotate secret" })).toBeDisabled();
  expect(screen.getByText("Saving issues a new secret; set up the registry again afterwards.")).toBeInTheDocument();
  expect(screen.getByRole("button", { name: "Save webhook" })).toBeEnabled();
});

it("warns that a provider change issues a new secret", async () => {
  serve(() => savedStatus({ configuration: { provider: "distribution", enabled: false, filters, registry_id: "r1", registry_server: "ghcr.io" }, registry: { id: "r1", name: "GHCR", server: "ghcr.io" } }));
  renderWebhook();
  fireEvent.click(await screen.findByRole("combobox", { name: "Provider" }));
  fireEvent.click(screen.getByRole("option", { name: "GitHub Container Registry" }));
  expect(screen.getByText("Saving issues a new secret; set up the registry again afterwards.")).toBeInTheDocument();
});

it("blocks turning on automatic deploys on an unsupported worker but allows keeping them off", async () => {
  serve(() => status({ worker_supported: false }));
  renderWebhook();
  expect(await screen.findByRole("switch", { name: "Pull and redeploy on push" })).toBeDisabled();
});

it("lets an unsupported worker's saved webhook be turned off", async () => {
  serve((method) => (method === "PUT" ? savedStatus({ worker_supported: false }) : savedStatus({ worker_supported: false })));
  renderWebhook();
  expect(await screen.findByText("Worker upgrade needed")).toBeInTheDocument();
  const toggle = screen.getByRole("switch", { name: "Pull and redeploy on push" });
  expect(toggle).toBeEnabled();
  fireEvent.click(toggle);
  fireEvent.click(screen.getByRole("button", { name: "Save webhook" }));
  await waitFor(() => expect(request).toHaveBeenCalledWith("PUT", "/api/workloads/w1/registry-webhook", { provider: "distribution", enabled: false, filters }));
});

it("is read-only without Edit configuration access", async () => {
  serve(() => savedStatus());
  renderWebhook({ ...service, permissions: ["read", "operate", "view"] });
  expect(await screen.findByText("Changing the registry webhook needs Edit configuration access. Settings are read-only.")).toBeInTheDocument();
  expect(screen.getByRole("list", { name: "Watched tags" })).toHaveTextContent("team/web:stable");
  expect(screen.queryByRole("button", { name: "Save webhook" })).not.toBeInTheDocument();
  expect(screen.queryByRole("button", { name: "Rotate secret" })).not.toBeInTheDocument();
});

const editOnly = { ...service, permissions: ["read", "view", "edit_configuration"] } as Workload;

it("lets Edit configuration without Operate save the webhook only with automatic deploys off, and not rotate it", async () => {
  serve((method) => (method === "PUT" ? savedStatus({ configuration: { provider: "distribution", enabled: false, filters, registry_id: "r1", registry_server: registry.server } }) : savedStatus()));
  renderWebhook(editOnly);
  const rotate = await screen.findByRole("button", { name: "Rotate secret" });
  expect(rotate).toBeDisabled();
  expect(rotate).toHaveAttribute("title", "Rotating the secret needs Edit configuration and Operate access.");
  expect(screen.getByRole("button", { name: "Remove webhook" })).toBeEnabled();
  fireEvent.change(screen.getByLabelText("Tag 1"), { target: { value: "latest" } });
  fireEvent.click(screen.getByRole("button", { name: "Save webhook" }));
  expect(screen.getByText(/Saving with automatic deploys on needs Operate access/)).toBeInTheDocument();
  expect(calls("PUT")).toHaveLength(0);
  fireEvent.click(screen.getByRole("switch", { name: "Pull and redeploy on push" }));
  fireEvent.click(screen.getByRole("button", { name: "Save webhook" }));
  await waitFor(() => expect(request).toHaveBeenCalledWith("PUT", "/api/workloads/w1/registry-webhook", { provider: "distribution", enabled: false, filters: [{ repository: "team/web", tag: "latest" }] }));
  expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
});

it("does not let Edit configuration without Operate turn automatic deploys on", async () => {
  serve(() => status());
  renderWebhook(editOnly);
  expect(await screen.findByRole("switch", { name: "Pull and redeploy on push" })).toBeDisabled();
  expect(screen.getByText(/Turning on automatic deploys needs Operate access as well as Edit configuration/)).toBeInTheDocument();
});

it("explains a suspended webhook with no authorised owner and lets an editor with Operate claim it only by a deliberate save", async () => {
  let current = savedStatus({ ownership_required: true });
  serve((method) => {
    if (method === "PUT") current = savedStatus();
    return current;
  });
  renderWebhook();
  expect(await screen.findByText("Automatic deploys suspended")).toBeInTheDocument();
  expect(screen.getByText(/Save the webhook or rotate its secret to take ownership/)).toBeInTheDocument();
  expect(screen.getByText("Saving makes you the webhook’s owner; later pushes deploy with your access.")).toBeInTheDocument();
  expect(calls("PUT")).toHaveLength(0);
  expect(calls("POST")).toHaveLength(0);
  fireEvent.click(screen.getByRole("button", { name: "Save webhook" }));
  const dialog = screen.getByRole("dialog", { name: "Take ownership of automatic deploys?" });
  expect(dialog).toHaveTextContent("You become the webhook’s owner");
  expect(calls("PUT")).toHaveLength(0);
  fireEvent.click(within(dialog).getByRole("button", { name: "Take ownership" }));
  await waitFor(() => expect(request).toHaveBeenCalledWith("PUT", "/api/workloads/w1/registry-webhook", { provider: "distribution", enabled: true, filters }));
  expect(await screen.findByText("Webhook saved")).toBeInTheDocument();
  await waitFor(() => expect(screen.queryByText("Automatic deploys suspended")).not.toBeInTheDocument());
});

it("lets Edit configuration without Operate disable or remove a suspended webhook but not claim it", async () => {
  serve(() => savedStatus({ ownership_required: true }));
  renderWebhook(editOnly);
  expect(await screen.findByText(/Taking ownership needs Edit configuration and Operate access/)).toBeInTheDocument();
  expect(screen.getByRole("button", { name: "Rotate secret" })).toBeDisabled();
  expect(screen.getByRole("button", { name: "Save webhook" })).toBeDisabled();
  expect(screen.getByRole("button", { name: "Remove webhook" })).toBeEnabled();
  expect(screen.getByRole("switch", { name: "Pull and redeploy on push" })).toBeEnabled();
});

it("never offers webhooks for jobs or managed previews and does not load them", () => {
  serve(() => status());
  renderWebhook({ ...service, kind: "job" });
  expect(screen.getByText(/Jobs are never pulled and redeployed automatically/)).toBeInTheDocument();
  renderWebhook({ ...service, preview_source_id: "w0" });
  expect(screen.getByText("Managed previews do not receive registry webhooks.")).toBeInTheDocument();
  expect(request).not.toHaveBeenCalled();
});

it("asks for a registry first and refuses Git-source resources", async () => {
  serve(() => status({ registry: null }));
  renderWebhook();
  expect(await screen.findByText(/Select a team registry under Docker registry first/)).toBeInTheDocument();
  expect(screen.queryByRole("button", { name: "Create webhook" })).not.toBeInTheDocument();
});

it("explains that Git-source resources cannot use registry webhooks", async () => {
  serve(() => status());
  renderWebhook({ ...service, source: { repository: "acme/web", branch: "main", compose_path: "compose.yaml" } });
  expect(await screen.findByText(/This resource deploys from Git/)).toBeInTheDocument();
  expect(screen.queryByRole("button", { name: "Create webhook" })).not.toBeInTheDocument();
});

it("pauses every write while offline", async () => {
  vi.spyOn(navigator, "onLine", "get").mockReturnValue(false);
  serve(() => savedStatus());
  renderWebhook();
  expect(await screen.findByRole("button", { name: "Rotate secret" })).toBeDisabled();
  expect(screen.getByRole("button", { name: "Remove webhook" })).toBeDisabled();
  expect(screen.getByRole("button", { name: "Save webhook" })).toBeDisabled();
  expect(screen.getByText("You are offline")).toBeInTheDocument();
});

it("shows a failed load with Retry", async () => {
  serve(() => {
    throw new ApiError(503, "Registry webhook unavailable.");
  });
  renderWebhook();
  expect(await screen.findByText("Could not load the registry webhook")).toBeInTheDocument();
  expect(screen.getByText("Registry webhook unavailable.")).toBeInTheDocument();
  fireEvent.click(screen.getByRole("button", { name: "Retry" }));
  await waitFor(() => expect(calls("GET").length).toBeGreaterThanOrEqual(2));
});

it("follows a queued webhook deploy to its outcome and discovers new pushes while the page stays open", async () => {
  vi.useFakeTimers({ shouldAdvanceTime: true });
  try {
    let current = savedStatus({ last_operation: { id: "op9", state: "queued", created_at: "2026-11-23T08:00:00Z" } });
    serve(() => current);
    renderWebhook();
    expect(await screen.findByText("Queued")).toBeInTheDocument();

    current = savedStatus({ last_operation: { id: "op9", state: "succeeded", created_at: "2026-11-23T08:00:00Z" } });
    await vi.advanceTimersByTimeAsync(3_100);
    expect(await screen.findByText("Succeeded")).toBeInTheDocument();

    // Idle now: a push received from outside the panel shows up on the slower recheck.
    const reads = calls("GET").length;
    current = savedStatus({ last_operation: { id: "op9", state: "succeeded", created_at: "2026-11-23T08:00:00Z" }, pending_at: "2026-11-23T09:00:00Z" });
    await vi.advanceTimersByTimeAsync(3_100);
    expect(calls("GET")).toHaveLength(reads);
    await vi.advanceTimersByTimeAsync(27_000);
    expect(await screen.findByText(/Waiting since/)).toBeInTheDocument();
  } finally {
    vi.useRealTimers();
  }
});

it("says pushes request an image check instead of a deploy while an image update policy is on", async () => {
  serve(
    (method) => (method === "PUT" ? savedStatus() : savedStatus({ configuration: { provider: "distribution", enabled: false, filters, registry_id: "r1", registry_server: registry.server } })),
    () => policyStatus(true),
  );
  renderWebhook();
  expect(await screen.findByText(/a push of a watched tag requests a fresh image check instead of a deploy/)).toBeInTheDocument();
  expect(screen.queryByText(/queues an ordinary deploy that pulls/)).not.toBeInTheDocument();
  fireEvent.click(screen.getByRole("switch", { name: "Pull and redeploy on push" }));
  fireEvent.click(screen.getByRole("button", { name: "Save webhook" }));
  const dialog = screen.getByRole("dialog", { name: "Turn on automatic pull and redeploy?" });
  expect(dialog).toHaveTextContent("requests a fresh image check, and the policy decides whether to notify or deploy");
});

it("keeps the direct pull-and-redeploy copy when the saved image update policy is off", async () => {
  serve(() => savedStatus(), () => policyStatus(false));
  renderWebhook();
  expect(await screen.findByText(/queues an ordinary deploy that pulls the resource’s configured image tags/)).toBeInTheDocument();
  expect(await screen.findByText("On")).toBeInTheDocument();
});

it("does not claim direct deploys are certain when the image update policy cannot be read", async () => {
  serve(
    () => savedStatus(),
    () => {
      throw new ApiError(503, "image update policy unavailable");
    },
  );
  renderWebhook();
  expect(await screen.findByText(/Could not read the image update policy\. If one is on, it decides what a matching push deploys\./)).toBeInTheDocument();
});

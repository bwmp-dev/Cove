import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import type { ReactNode } from "react";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { ApiError, request } from "../api/client";
import { managedRegistriesKey, type ManagedRegistry, type ManagedRegistryList } from "../api/managed-registries";
import type { Project } from "../api/projects";
import type { Node, RouteCertificate } from "../api/types";
import { ManagedRegistries } from "./ManagedRegistries";

vi.mock("../api/client", async (original) => ({ ...(await original<typeof import("../api/client")>()), request: vi.fn() }));
vi.mock("./SaveBar", async (original) => ({ ...(await original<typeof import("./SaveBar")>()), UnsavedChangesGuard: () => null }));
vi.mock("@tanstack/react-router", () => ({
  Link: ({ children, to, params }: { children: ReactNode; to: string; params?: { operationId?: string; resourceId?: string } }) => (
    <a href={params?.operationId ? `/operations/${params.operationId}` : params?.resourceId ? to.replace("$resourceId", params.resourceId) : to}>{children}</a>
  ),
}));
beforeEach(() => vi.stubGlobal("ResizeObserver", class { observe = vi.fn(); disconnect = vi.fn(); }));
afterEach(() => {
  vi.clearAllMocks();
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
});
HTMLDialogElement.prototype.showModal = function (this: HTMLDialogElement) {
  this.open = true;
};
HTMLDialogElement.prototype.close = function (this: HTMLDialogElement) {
  this.open = false;
};

// Fixtures only: no worker, registry, DNS or certificate authority is contacted.
const listPath = "/api/managed-registries";
const capable = { managed_registry: true, file_secrets: true, protocol: 1, os: "linux", arch: "amd64", docker: "", compose: "", ready: true };
const nodes = [
  { id: "n1", name: "builder", state: "ready", mode: "local", address: "", port: 0, last_checked_at: null, capabilities: capable },
  { id: "n2", name: "old-worker", state: "ready", mode: "local", address: "", port: 0, last_checked_at: null, capabilities: { ...capable, managed_registry: false } },
  { id: "n3", name: "no-secrets", state: "ready", mode: "local", address: "", port: 0, last_checked_at: null, capabilities: { ...capable, file_secrets: false } },
  { id: "n4", name: "offline", state: "unreachable", mode: "local", address: "", port: 0, last_checked_at: null, capabilities: capable },
] as Node[];
const projects: Project[] = [
  { id: "p1", name: "platform", permissions: ["read", "operate", "edit"], environments: [] },
  { id: "p2", name: "read-only", permissions: ["read"], environments: [] },
];
const storage = { hard_cap_verified: false, reason: "No hard cap on this storage: the managed Docker volume has no verified filesystem quota." };

function registry(extra: Partial<ManagedRegistry> = {}): ManagedRegistry {
  return {
    id: "m1",
    name: "Team registry",
    node_id: "n1",
    project_id: "p1",
    hostname: "registry.example.test",
    state: "ready",
    connection_id: "c1",
    operation_id: "op1",
    operation_state: "succeeded",
    last_error: "",
    retention: { keep_per_repository: 10, minimum_age_days: 30 },
    storage,
    image: "docker.io/library/registry:3.1.2@sha256:0123",
    data_retained: false,
    ...extra,
  };
}

type Reply = unknown;
type Handler = (method: string, path: string, body: unknown) => Reply;

let listReply: () => Reply;

function serve(handler: Handler = () => undefined) {
  vi.mocked(request).mockImplementation((method: string, path: string, body?: unknown) => {
    try {
      const handled = handler(method, path, body);
      if (handled !== undefined) return Promise.resolve(handled);
      if (method === "GET" && path === listPath) return Promise.resolve(listReply());
      if (method === "GET" && path === "/api/nodes") return Promise.resolve(nodes);
      if (method === "GET" && path === "/api/projects") return Promise.resolve(projects);
      return Promise.reject(new Error(`unexpected request ${method} ${path}`));
    } catch (error) {
      return Promise.reject(error instanceof Error ? error : new Error(String(error)));
    }
  });
}

function list(registries: ManagedRegistry[], canManage = true): ManagedRegistryList {
  return { registries, can_manage: canManage };
}

function renderPage() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(
    <QueryClientProvider client={client}>
      <ManagedRegistries />
    </QueryClientProvider>,
  );
  return client;
}

const calls = (method: string, path?: string) => vi.mocked(request).mock.calls.filter(([m, p]) => m === method && (path === undefined || p === path));

async function choose(label: string, option: string) {
  fireEvent.click(await screen.findByRole("combobox", { name: label }));
  fireEvent.click(screen.getByRole("option", { name: option }));
}

// Creating stays paused until node capabilities have loaded.
async function createReady() {
  const button = await screen.findByRole("button", { name: "Create managed registry" });
  await waitFor(() => expect(button).toBeEnabled());
}

async function fillCreate() {
  await createReady();
  fireEvent.change(screen.getByLabelText("Name"), { target: { value: "  Team registry  " } });
  await choose("Project", "platform");
  await choose("Node", "builder");
  fireEvent.change(screen.getByLabelText("Hostname"), { target: { value: "registry.example.test" } });
}

it("shows metadata to viewers without any create or maintenance controls and caches only documented fields", async () => {
  listReply = () => list([{ ...registry(), password: "never-cached", token: "never-cached" } as ManagedRegistry, registry({ id: "m0", hostname: "other.example.test", state: "hibernating", connection_id: "" })], false);
  serve();
  const client = renderPage();
  expect(await screen.findByText("registry.example.test", { selector: "span" })).toBeInTheDocument();
  expect(request).toHaveBeenCalledWith("GET", listPath, undefined, expect.any(AbortSignal));
  expect(screen.getByText("Unknown state (hibernating)")).toBeInTheDocument();
  expect(screen.getAllByText("No verified hard cap")).not.toHaveLength(0);
  expect(screen.getAllByText(storage.reason)).not.toHaveLength(0);
  expect(screen.getAllByText("Fixed by this Cove release")).not.toHaveLength(0);
  expect(screen.queryByRole("form", { name: "Create a managed registry" })).not.toBeInTheDocument();
  expect(screen.queryByRole("button", { name: "Run garbage collection" })).not.toBeInTheDocument();
  expect(screen.queryByRole("button", { name: "Remove registry" })).not.toBeInTheDocument();
  expect(screen.queryByRole("button", { name: "Save retention" })).not.toBeInTheDocument();
  const cached = client.getQueryData<ManagedRegistryList>(managedRegistriesKey);
  expect(JSON.stringify(cached)).not.toContain("never-cached");
});

it("creates with the exact body, keeps the notice and follows provisioning to ready", async () => {
  let current = list([]);
  listReply = () => current;
  serve((method, path) => {
    if (method === "POST" && path === listPath) {
      current = list([registry({ state: "provisioning", connection_id: "", operation_id: "op-create", operation_state: "queued" })]);
      return { id: "m1", operation_id: "op-create" };
    }
    return undefined;
  });
  const client = renderPage();
  const invalidate = vi.spyOn(client, "invalidateQueries");
  await fillCreate();
  fireEvent.click(screen.getByRole("button", { name: "Create managed registry" }));
  await waitFor(() => expect(calls("POST", listPath)).toHaveLength(1));
  expect(calls("POST", listPath)[0]).toEqual(["POST", listPath, { name: "Team registry", node_id: "n1", project_id: "p1", hostname: "registry.example.test", retention: { keep_per_repository: 10, minimum_age_days: 30 } }]);
  expect(await screen.findByText("Managed registry requested")).toBeInTheDocument();
  expect(await screen.findByText("Provisioning")).toBeInTheDocument();
  expect(screen.queryByRole("form", { name: "Create a managed registry" })).not.toBeInTheDocument();
  expect(screen.getByText("Created only after Cove verifies authenticated HTTPS")).toBeInTheDocument();
  expect(screen.getByRole("link", { name: "View operation" })).toHaveAttribute("href", "/operations/op-create");

  invalidate.mockClear();
  current = list([registry({ operation_id: "op-create" })]);
  await act(() => client.refetchQueries({ queryKey: managedRegistriesKey }));
  expect(await screen.findByText("Ready")).toBeInTheDocument();
  expect(screen.getByText("Managed registry requested")).toBeInTheDocument();
  expect(invalidate).toHaveBeenCalledWith({ queryKey: ["connections", "registries"] });
});

it("offers only ready nodes with managed registry and secret file support", async () => {
  listReply = () => list([]);
  serve();
  renderPage();
  await createReady();
  fireEvent.click(screen.getByRole("combobox", { name: "Node" }));
  expect(screen.getByRole("option", { name: "builder" })).not.toHaveAttribute("aria-disabled");
  expect(screen.getByRole("option", { name: "old-worker (no managed registry support)" })).toHaveAttribute("aria-disabled", "true");
  expect(screen.getByRole("option", { name: "no-secrets (no secret file support)" })).toHaveAttribute("aria-disabled", "true");
  expect(screen.getByRole("option", { name: "offline (not ready)" })).toHaveAttribute("aria-disabled", "true");
  fireEvent.click(await screen.findByRole("combobox", { name: "Project" }));
  expect(screen.queryByRole("option", { name: "read-only" })).not.toBeInTheDocument();
});

it("fails closed when cached node capabilities could not be refreshed", async () => {
  let failNodes = false;
  listReply = () => list([]);
  serve((method, path) => {
    if (method === "GET" && path === "/api/nodes" && failNodes) throw new ApiError(503, "Nodes unavailable");
    return undefined;
  });
  const client = renderPage();
  await createReady();
  failNodes = true;
  await act(() => client.refetchQueries({ queryKey: ["nodes"] }));
  expect(await screen.findByText("Could not load node capabilities")).toBeInTheDocument();
  expect(screen.getByRole("button", { name: "Create managed registry" })).toBeDisabled();
  expect(screen.getByText("Node capabilities have not loaded. Creating is paused.")).toBeInTheDocument();
  fireEvent.click(screen.getByRole("combobox", { name: "Node" }));
  expect(screen.getByRole("option", { name: "builder (capabilities unknown)" })).toHaveAttribute("aria-disabled", "true");
});

it("keeps the draft when the API refuses a second live registry", async () => {
  listReply = () => list([registry({ id: "old", state: "removed", connection_id: "", data_retained: false })]);
  serve((method, path) => {
    if (method === "POST" && path === listPath) throw new ApiError(409, "This team already has a managed registry");
    return undefined;
  });
  renderPage();
  await fillCreate();
  fireEvent.click(screen.getByRole("button", { name: "Create managed registry" }));
  expect(await screen.findByText("This team already has a managed registry")).toBeInTheDocument();
  expect(screen.getByText("Your entries are kept.")).toBeInTheDocument();
  expect(screen.getByLabelText("Name")).toHaveValue("  Team registry  ");
  expect(screen.getByLabelText("Hostname")).toHaveValue("registry.example.test");
  expect(screen.queryByText("Managed registry requested")).not.toBeInTheDocument();
});

it("hides the create form while a live registry exists", async () => {
  listReply = () => list([registry({ state: "failed", connection_id: "" })]);
  serve((method, path) => (method === "GET" && path.startsWith("/api/workloads/m1") ? (path.endsWith("/certificates") ? [] : { id: "m1", routes: [{ domain: "registry.example.test", service: "registry", port: 5000 }] }) : undefined));
  renderPage();
  expect(await screen.findByText("Provisioning failed")).toBeInTheDocument();
  expect(screen.queryByRole("form", { name: "Create a managed registry" })).not.toBeInTheDocument();
});

it("validates the create form before sending anything", async () => {
  listReply = () => list([]);
  serve();
  renderPage();
  await createReady();
  fireEvent.change(screen.getByLabelText("Hostname"), { target: { value: "Registry.Example.test" } });
  fireEvent.change(screen.getByLabelText("Keep per repository"), { target: { value: "101" } });
  fireEvent.change(screen.getByLabelText("Minimum age in days"), { target: { value: "0" } });
  fireEvent.click(screen.getByRole("button", { name: "Create managed registry" }));
  expect(await screen.findByText("Enter a name.")).toBeInTheDocument();
  expect(screen.getByText("Use lowercase letters.")).toBeInTheDocument();
  expect(screen.getByText("Choose a project.")).toBeInTheDocument();
  expect(screen.getByText("Choose a node.")).toBeInTheDocument();
  expect(screen.getByText("Enter a whole number from 1 to 100.")).toBeInTheDocument();
  expect(screen.getByText("Enter a whole number from 1 to 3650.")).toBeInTheDocument();
  for (const [value, message] of [
    ["*.example.test", "Wildcard hostnames cannot be used. Enter one exact hostname."],
    ["registry.example.test:5000", "Enter the hostname only, without a scheme, path or port."],
    ["https://registry.example.test/v2", "Enter the hostname only, without a scheme, path or port."],
    ["registry", "Enter a complete DNS hostname, such as registry.example.com."],
  ] as const) {
    fireEvent.change(screen.getByLabelText("Hostname"), { target: { value } });
    expect(await screen.findByText(message)).toBeInTheDocument();
  }
  expect(calls("POST")).toHaveLength(0);
});

it("saves retention with the exact body and keeps the notice while the refetch is held open", async () => {
  let current = list([registry()]);
  let release: () => void = () => undefined;
  let hold = false;
  listReply = () => {
    if (!hold) return current;
    const reply = current;
    return new Promise((resolve) => {
      release = () => resolve(reply);
    });
  };
  serve((method, path, body) => {
    if (method === "PUT" && path === `${listPath}/m1`) {
      current = list([registry({ retention: (body as { retention: ManagedRegistry["retention"] }).retention })]);
      hold = true;
      return { ok: true };
    }
    return undefined;
  });
  renderPage();
  fireEvent.change(await screen.findByLabelText("Keep per repository"), { target: { value: "5" } });
  fireEvent.change(screen.getByLabelText("Minimum age in days"), { target: { value: "14" } });
  fireEvent.click(screen.getByRole("button", { name: "Save retention" }));
  await waitFor(() => expect(calls("PUT")).toHaveLength(1));
  expect(calls("PUT")[0]).toEqual(["PUT", `${listPath}/m1`, { retention: { keep_per_repository: 5, minimum_age_days: 14 } }]);
  expect(await screen.findByText("Retention saved")).toBeInTheDocument();
  expect(screen.getByLabelText("Keep per repository")).toHaveValue(5);
  release();
  hold = false;
  await waitFor(() => expect(screen.getByText("Newest 5 manifests of each repository and every manifest younger than 14 days")).toBeInTheDocument());
  expect(screen.getByText("Retention saved")).toBeInTheDocument();
  expect(screen.getByRole("button", { name: "Save retention" })).toBeDisabled();
});

it("keeps refused retention edits and validates their range", async () => {
  listReply = () => list([registry()]);
  serve((method) => {
    if (method === "PUT") throw new ApiError(409, "Wait for registry maintenance or provisioning to finish");
    return undefined;
  });
  renderPage();
  fireEvent.change(await screen.findByLabelText("Keep per repository"), { target: { value: "abc" } });
  fireEvent.click(screen.getByRole("button", { name: "Save retention" }));
  expect(await screen.findByText("Enter a whole number from 1 to 100.")).toBeInTheDocument();
  expect(calls("PUT")).toHaveLength(0);
  fireEvent.change(screen.getByLabelText("Keep per repository"), { target: { value: "20" } });
  fireEvent.click(screen.getByRole("button", { name: "Save retention" }));
  expect(await screen.findByText("Wait for registry maintenance or provisioning to finish")).toBeInTheDocument();
  expect(screen.getByText("Your changes are kept.")).toBeInTheDocument();
  expect(screen.getByLabelText("Keep per repository")).toHaveValue(20);
});

it("confirms garbage collection with what is kept and deleted, then posts without a body", async () => {
  let current = list([registry()]);
  listReply = () => current;
  serve((method, path) => {
    if (method === "POST" && path === `${listPath}/m1/gc`) {
      current = list([registry({ state: "maintenance", operation_id: "op-gc", operation_state: "running" })]);
      return { operation_id: "op-gc" };
    }
    return undefined;
  });
  renderPage();
  fireEvent.click(await screen.findByRole("button", { name: "Run garbage collection" }));
  const dialog = screen.getByRole("dialog", { name: "Run garbage collection on Team registry?" });
  expect(dialog).toHaveTextContent("The registry is stopped while this runs");
  expect(dialog).toHaveTextContent("Running application containers are not stopped or deleted.");
  expect(dialog).toHaveTextContent("images referenced by retained releases, previews and latest builds; tags that Cove builds did not create; and the newest 10 manifests of each repository and every manifest younger than 30 days.");
  expect(dialog).toHaveTextContent("Deleted: every other Cove build manifest.");
  expect(dialog).toHaveTextContent("If it is interrupted or cancelled, manifests already deleted are not restored and cleanup can stay incomplete until the worker recovers the registry.");
  expect(dialog).toHaveTextContent("Cove never runs it on a schedule.");
  expect(dialog).toHaveTextContent("The project's approval, freeze and deployment window rules apply, so the operation can wait in the queue before it runs.");
  expect(dialog).not.toHaveTextContent(/\d+\s?(MB|GB|bytes)/);
  fireEvent.click(within(dialog).getByRole("button", { name: "Run garbage collection" }));
  await waitFor(() => expect(calls("POST", `${listPath}/m1/gc`)).toHaveLength(1));
  expect(calls("POST", `${listPath}/m1/gc`)[0]).toEqual(["POST", `${listPath}/m1/gc`]);
  expect(await screen.findByText("Garbage collection requested")).toBeInTheDocument();
  expect(await screen.findByText("Maintenance")).toBeInTheDocument();
  expect(screen.getByText(/Garbage collection is queued or running\. A queued run may be waiting for approval, a deployment window or the worker\. While it runs, the registry is stopped/)).toBeInTheDocument();
  expect(screen.queryByRole("button", { name: "Save retention" })).not.toBeInTheDocument();
});

it("explains an interrupted maintenance operation from the API error", async () => {
  listReply = () => list([registry({ operation_state: "interrupted", last_error: "The registry operation failed or was interrupted; inspect its operation before retrying." })]);
  serve();
  renderPage();
  expect(await screen.findByText("The last registry operation did not finish")).toBeInTheDocument();
  expect(screen.getByText("The registry operation failed or was interrupted; inspect its operation before retrying.")).toBeInTheDocument();
  expect(screen.getByText("Manifests that garbage collection deleted before it stopped are not restored.")).toBeInTheDocument();
  expect(screen.getByText("Interrupted")).toBeInTheDocument();
});

it("removes with delete_data false by default and requires the hostname to delete data", async () => {
  listReply = () => list([registry()]);
  serve((method) => (method === "DELETE" ? { operation_id: "op-rm" } : undefined));
  renderPage();
  fireEvent.click(await screen.findByRole("button", { name: "Remove registry" }));
  const dialog = screen.getByRole("dialog", { name: "Remove Team registry?" });
  expect(dialog).toHaveTextContent("Disconnect resources and registry webhooks that use it first");
  expect(dialog).toHaveTextContent("Certificates are not revoked");
  expect(dialog).toHaveTextContent("By default the registry's data volume is kept on the node");
  expect(within(dialog).getByRole("checkbox", { name: "Also delete the registry data permanently" })).not.toBeChecked();
  fireEvent.click(within(dialog).getByRole("button", { name: "Remove registry" }));
  await waitFor(() => expect(calls("DELETE")).toHaveLength(1));
  expect(calls("DELETE")[0]).toEqual(["DELETE", `${listPath}/m1`, { delete_data: false }]);
  expect(await screen.findByText("Removal requested")).toBeInTheDocument();
});

it("asks for the hostname before removing with data deletion", async () => {
  listReply = () => list([registry()]);
  serve((method) => (method === "DELETE" ? { operation_id: "op-rm" } : undefined));
  renderPage();
  fireEvent.click(await screen.findByRole("button", { name: "Remove registry" }));
  const dialog = screen.getByRole("dialog", { name: "Remove Team registry?" });
  fireEvent.click(within(dialog).getByRole("checkbox", { name: "Also delete the registry data permanently" }));
  expect(dialog).toHaveTextContent("Every image in this registry will be deleted");
  const confirm = within(dialog).getByRole("button", { name: "Remove and delete data" });
  expect(confirm).toBeDisabled();
  fireEvent.change(within(dialog).getByLabelText("Type registry.example.test to confirm"), { target: { value: "registry.example.test" } });
  fireEvent.click(confirm);
  await waitFor(() => expect(calls("DELETE")).toHaveLength(1));
  expect(calls("DELETE")[0]).toEqual(["DELETE", `${listPath}/m1`, { delete_data: true }]);
});

it("lists removed registries with retained data honestly and deletes that data on request", async () => {
  listReply = () => list([registry({ state: "removed", connection_id: "", data_retained: true, operation_state: "succeeded" })]);
  serve((method) => (method === "DELETE" ? { operation_id: "op-purge" } : undefined));
  renderPage();
  expect(await screen.findByText("Removed")).toBeInTheDocument();
  expect(screen.getByText("Data volume retained on the node")).toBeInTheDocument();
  expect(screen.getByText(/The registry is not running and has no connection/)).toBeInTheDocument();
  expect(screen.getByRole("form", { name: "Create a managed registry" })).toBeInTheDocument();
  fireEvent.click(screen.getByRole("button", { name: "Delete retained data" }));
  const dialog = screen.getByRole("dialog", { name: "Delete the retained data of Team registry?" });
  fireEvent.change(within(dialog).getByLabelText("Type registry.example.test to confirm"), { target: { value: "registry.example.test" } });
  fireEvent.click(within(dialog).getByRole("button", { name: "Delete data permanently" }));
  await waitFor(() => expect(calls("DELETE")).toHaveLength(1));
  expect(calls("DELETE")[0]).toEqual(["DELETE", `${listPath}/m1`, { delete_data: true }]);
});

it("offers deleting the record only to managers of a purged removed registry without an active operation", async () => {
  const purged = registry({ state: "removed", connection_id: "", data_retained: false });
  let current = list([purged], false);
  listReply = () => current;
  serve();
  const client = renderPage();
  expect(await screen.findByText("No retained data reported")).toBeInTheDocument();
  expect(screen.queryByRole("button", { name: "Delete registry record" })).not.toBeInTheDocument();
  const blocked: [ManagedRegistry, string][] = [
    [registry({ state: "removed", connection_id: "", data_retained: true }), "Registry data retained"],
    [registry({ state: "removed", connection_id: "", operation_state: "running" }), "An operation is in progress"],
    [registry(), "Run garbage collection"],
  ];
  for (const [next, shown] of blocked) {
    current = list([next]);
    await act(() => client.refetchQueries({ queryKey: managedRegistriesKey }));
    expect(await screen.findByText(shown)).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Delete registry record" })).not.toBeInTheDocument();
  }
  current = list([purged]);
  await act(() => client.refetchQueries({ queryKey: managedRegistriesKey }));
  expect(await screen.findByRole("button", { name: "Delete registry record" })).toBeEnabled();
});

it("confirms deleting the record of a purged registry, sends no body and refreshes the related lists", async () => {
  let current = list([registry({ state: "removed", connection_id: "", data_retained: false })]);
  listReply = () => current;
  serve((method, path) => {
    if (method === "DELETE" && path === `${listPath}/m1/record`) {
      current = list([]);
      return { ok: true };
    }
    return undefined;
  });
  const client = renderPage();
  const invalidate = vi.spyOn(client, "invalidateQueries");
  fireEvent.click(await screen.findByRole("button", { name: "Delete registry record" }));
  const dialog = screen.getByRole("dialog", { name: "Delete the record of Team registry?" });
  expect(dialog).toHaveTextContent("This deletes the empty registry resource and its operation history from Cove.");
  expect(dialog).toHaveTextContent("no images, volumes or running services are affected");
  expect(calls("DELETE")).toHaveLength(0);
  fireEvent.click(within(dialog).getByRole("button", { name: "Delete registry record" }));
  await waitFor(() => expect(calls("DELETE")).toHaveLength(1));
  expect(calls("DELETE")[0]).toEqual(["DELETE", `${listPath}/m1/record`]);
  await waitFor(() => expect(screen.queryByText("Team registry")).not.toBeInTheDocument());
  expect(invalidate).toHaveBeenCalledWith({ queryKey: managedRegistriesKey, exact: true });
  expect(invalidate).toHaveBeenCalledWith({ queryKey: ["connections", "registries"] });
  expect(invalidate).toHaveBeenCalledWith({ queryKey: ["operations"] });
  expect(invalidate).toHaveBeenCalledWith({ queryKey: ["workloads"] });
  expect(screen.getByRole("form", { name: "Create a managed registry" })).toBeInTheDocument();
});

it("keeps the purged registry and shows the refusal when deleting its record fails", async () => {
  listReply = () => list([registry({ state: "removed", connection_id: "", data_retained: false })]);
  serve((method) => {
    if (method === "DELETE") throw new ApiError(409, "Only an empty removed registry without an active operation can be deleted");
    return undefined;
  });
  const client = renderPage();
  const invalidate = vi.spyOn(client, "invalidateQueries");
  fireEvent.click(await screen.findByRole("button", { name: "Delete registry record" }));
  fireEvent.click(within(screen.getByRole("dialog", { name: "Delete the record of Team registry?" })).getByRole("button", { name: "Delete registry record" }));
  expect(await screen.findByText("The registry record was not deleted")).toBeInTheDocument();
  expect(screen.getByText("Only an empty removed registry without an active operation can be deleted")).toBeInTheDocument();
  expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
  expect(screen.getByText("Team registry")).toBeInTheDocument();
  expect(screen.getByRole("button", { name: "Delete registry record" })).toBeEnabled();
  expect(invalidate).not.toHaveBeenCalledWith({ queryKey: ["workloads"] });
});

it("repairs failed provisioning by uploading and selecting a certificate, then retries without a body", async () => {
  const certificate: RouteCertificate = { id: "cert1", name: "registry cert", source: "uploaded", fingerprint: "ab", dns_names: ["registry.example.test"], not_before: "2026-01-01T00:00:00Z", expires_at: "2099-01-01T00:00:00Z", created_at: "2026-01-01T00:00:00Z" };
  listReply = () => list([registry({ state: "failed", connection_id: "", operation_state: "failed", last_error: "Registry deployment finished, but authenticated TLS verification failed." })]);
  let stored: RouteCertificate[] = [];
  const chain = "-----BEGIN CERTIFICATE-----\nfixture\n-----END CERTIFICATE-----\n";
  const key = "-----BEGIN PRIVATE KEY-----\nfixture\n-----END PRIVATE KEY-----\n";
  serve((method, path) => {
    if (method === "GET" && path === "/api/workloads/m1") return { id: "m1", permissions: ["read"], routes: [{ domain: "registry.example.test", service: "registry", port: 5000 }] };
    if (method === "GET" && path === "/api/workloads/m1/certificates") return stored;
    if (method === "POST" && path === "/api/workloads/m1/certificates") {
      stored = [certificate];
      return certificate;
    }
    if (method === "PUT") return { ok: true };
    if (method === "POST" && path === `${listPath}/m1/retry`) return { operation_id: "op-retry" };
    return undefined;
  });
  renderPage();
  expect(await screen.findByText("Registry deployment finished, but authenticated TLS verification failed.")).toBeInTheDocument();
  expect(await screen.findByRole("combobox", { name: "Certificate" })).toHaveTextContent("Let's Encrypt");
  // The generated resource is read-only, yet team managers still get the upload form for its certificate API.
  const upload = screen.getByRole("form", { name: "Upload a certificate" });
  fireEvent.change(within(upload).getByLabelText("Name"), { target: { value: "registry cert" } });
  fireEvent.change(within(upload).getByLabelText("Certificate chain"), { target: { value: chain } });
  fireEvent.change(within(upload).getByLabelText("Private key"), { target: { value: key } });
  fireEvent.click(within(upload).getByRole("button", { name: "Upload certificate" }));
  await waitFor(() => expect(calls("POST", "/api/workloads/m1/certificates")).toHaveLength(1));
  expect(calls("POST", "/api/workloads/m1/certificates")[0]).toEqual(["POST", "/api/workloads/m1/certificates", { name: "registry cert", certificate_pem: chain, private_key_pem: key }]);
  expect(await within(upload).findByText("registry cert is stored for this resource. Choose it as the certificate above, save, and retry provisioning.")).toBeInTheDocument();
  expect(within(upload).getByLabelText("Private key")).toHaveValue("");
  await choose("Certificate", "registry cert · Uploaded");
  fireEvent.click(screen.getByRole("button", { name: "Save certificate selection" }));
  await waitFor(() => expect(calls("PUT")).toHaveLength(1));
  expect(calls("PUT")[0]).toEqual(["PUT", `${listPath}/m1`, { retention: { keep_per_repository: 10, minimum_age_days: 30 }, certificate_id: "cert1" }]);
  expect(await screen.findByText("Certificate selection saved. Retry provisioning to deploy it.")).toBeInTheDocument();
  fireEvent.click(screen.getByRole("button", { name: "Retry provisioning" }));
  await waitFor(() => expect(calls("POST", `${listPath}/m1/retry`)).toHaveLength(1));
  expect(calls("POST", `${listPath}/m1/retry`)[0]).toEqual(["POST", `${listPath}/m1/retry`]);
  expect(await screen.findByText("Provisioning retry requested")).toBeInTheDocument();
});

it("hides certificate repair from viewers of a failed registry", async () => {
  listReply = () => list([registry({ state: "failed", connection_id: "", operation_state: "failed", last_error: "Registry deployment finished, but authenticated TLS verification failed." })], false);
  serve();
  renderPage();
  expect(await screen.findByText("Registry deployment finished, but authenticated TLS verification failed.")).toBeInTheDocument();
  expect(screen.getByText(/A team administrator can repair it\./)).toBeInTheDocument();
  expect(screen.queryByRole("form", { name: "Upload a certificate" })).not.toBeInTheDocument();
  expect(screen.queryByRole("button", { name: "Retry provisioning" })).not.toBeInTheDocument();
  expect(calls("GET", "/api/workloads/m1/certificates")).toHaveLength(0);
});

it("keeps a verification failure that retries automatically visible with its repair actions until it becomes ready", async () => {
  const retrying = "Registry deployment finished, but authenticated HTTPS verification is not ready. Cove will retry automatically (up to 15 minutes between attempts).";
  let current = list([registry({ state: "failed", connection_id: "", operation_state: "succeeded", last_error: retrying })]);
  listReply = () => current;
  serve((method, path) => {
    if (method === "GET" && path === "/api/workloads/m1") return { id: "m1", permissions: ["read"], routes: [{ domain: "registry.example.test", service: "registry", port: 5000 }] };
    if (method === "GET" && path === "/api/workloads/m1/certificates") return [];
    return undefined;
  });
  const client = renderPage();
  expect(await screen.findByText(retrying)).toBeInTheDocument();
  expect(screen.getByText("Provisioning failed")).toBeInTheDocument();
  expect(screen.getByRole("button", { name: "Retry provisioning" })).toBeEnabled();
  expect(screen.getByRole("button", { name: "Remove registry" })).toBeEnabled();
  expect(screen.queryByText("An operation is in progress")).not.toBeInTheDocument();
  current = list([registry()]);
  await act(() => client.refetchQueries({ queryKey: managedRegistriesKey }));
  await waitFor(() => expect(screen.queryByText(retrying)).not.toBeInTheDocument());
  expect(screen.queryByText("Provisioning failed")).not.toBeInTheDocument();
  expect(screen.getByRole("button", { name: "Run garbage collection" })).toBeInTheDocument();
});

it("shows a failed first read with Retry", async () => {
  let fail = true;
  listReply = () => {
    if (fail) throw new ApiError(503, "Managed registries unavailable");
    return list([registry()]);
  };
  serve();
  renderPage();
  expect(await screen.findByText("Could not load managed registries")).toBeInTheDocument();
  fail = false;
  fireEvent.click(screen.getByRole("button", { name: "Retry" }));
  expect(await screen.findByText("Team registry")).toBeInTheDocument();
});

it("keeps the last list and unsaved edits when a refresh fails, and pauses changes", async () => {
  let fail = false;
  listReply = () => {
    if (fail) throw new ApiError(503, "Managed registries unavailable");
    return list([registry()]);
  };
  serve();
  const client = renderPage();
  fireEvent.change(await screen.findByLabelText("Keep per repository"), { target: { value: "7" } });
  fail = true;
  await act(() => client.refetchQueries({ queryKey: managedRegistriesKey }));
  expect(await screen.findByText("Could not refresh managed registries")).toBeInTheDocument();
  expect(screen.getByLabelText("Keep per repository")).toHaveValue(7);
  expect(screen.getByRole("button", { name: "Save retention" })).toBeDisabled();
  expect(screen.getByRole("button", { name: "Run garbage collection" })).toBeDisabled();
  fail = false;
  fireEvent.click(screen.getByRole("button", { name: "Retry" }));
  await waitFor(() => expect(screen.queryByText("Could not refresh managed registries")).not.toBeInTheDocument());
  expect(screen.getByLabelText("Keep per repository")).toHaveValue(7);
  expect(screen.getByRole("button", { name: "Save retention" })).toBeEnabled();
});

it("pauses deleting a purged registry record while offline", async () => {
  vi.spyOn(navigator, "onLine", "get").mockReturnValue(false);
  listReply = () => list([registry({ state: "removed", connection_id: "", data_retained: false })]);
  serve();
  renderPage();
  expect(await screen.findByRole("button", { name: "Delete registry record" })).toBeDisabled();
  expect(screen.getByText("You are offline. Registry actions are paused.")).toBeInTheDocument();
});

it("pauses changes while offline", async () => {
  vi.spyOn(navigator, "onLine", "get").mockReturnValue(false);
  listReply = () => list([registry()]);
  serve();
  renderPage();
  expect(await screen.findByText("You are offline")).toBeInTheDocument();
  expect(screen.getByRole("button", { name: "Run garbage collection" })).toBeDisabled();
  expect(screen.getByText("You are offline. Registry actions are paused.")).toBeInTheDocument();
});

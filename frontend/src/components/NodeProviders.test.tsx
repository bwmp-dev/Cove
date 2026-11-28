import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { createMemoryHistory, createRootRoute, createRouter, RouterProvider } from "@tanstack/react-router";
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import type { ReactNode } from "react";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { ApiError, request } from "../api/client";
import { hasActive, type NodeProvider, type ProviderCatalog, type Provision } from "../api/nodeProviders";
import { CloudServers, NodeCloudOrigin, ProviderTokens } from "./NodeProviders";

vi.mock("../api/client", async (original) => ({ ...(await original<typeof import("../api/client")>()), request: vi.fn() }));

beforeEach(() => {
  vi.stubGlobal("ResizeObserver", class { observe = vi.fn(); disconnect = vi.fn(); });
  HTMLDialogElement.prototype.showModal = function (this: HTMLDialogElement) { this.setAttribute("open", ""); };
  HTMLDialogElement.prototype.close = function (this: HTMLDialogElement) { this.removeAttribute("open"); };
});
afterEach(() => { vi.clearAllMocks(); vi.unstubAllGlobals(); });

const token = "hcloud-secret-token-0123456789";
const hetzner: NodeProvider = { id: "p1", kind: "hetzner", name: "Hetzner main", has_token: true };
const catalog: ProviderCatalog = {
  regions: [{ id: "fsn1", name: "Falkenstein" }, { id: "ash", name: "Ashburn" }],
  plans: [
    { id: "cx22", name: "CX22", cpus: 2, memory_mb: 4096, architecture: "x86", regions: ["fsn1"] },
    { id: "cpx11", name: "CPX11", cpus: 2, memory_mb: 2048, architecture: "x86", monthly: "4.99", currency: "USD", regions: ["ash"] },
  ],
  images: [{ id: "debian-12", name: "Debian 12", architecture: "x86" }],
  limited: false,
  checked_at: new Date().toISOString(),
};
const job = (overrides: Partial<Provision>): Provision => ({
  id: "j1", provider_id: "p1", provider_kind: "hetzner", provider_name: "Hetzner main", node_id: null, server_id: "", server_name: "worker-1",
  phase: "queued", last_error: "", created_at: new Date().toISOString(), updated_at: new Date().toISOString(), deadline: new Date(Date.now() + 600_000).toISOString(), ...overrides,
});

function renderWithRouter(children: ReactNode, client = new QueryClient({ defaultOptions: { queries: { retry: false } } })) {
  const router = createRouter({ routeTree: createRootRoute({ component: () => children }), history: createMemoryHistory() });
  render(<QueryClientProvider client={client}><RouterProvider router={router} /></QueryClientProvider>);
}

async function choose(label: string, option: string | RegExp) {
  fireEvent.click(await screen.findByRole("combobox", { name: label }));
  fireEvent.click(screen.getByRole("option", { name: option }));
}

it("saves a provider token without ever displaying it and keeps it on edit when left blank", async () => {
  let list: NodeProvider[] = [];
  vi.mocked(request).mockImplementation((method) => {
    if (method === "POST") { list = [hetzner]; return Promise.resolve({ id: "p1" }); }
    if (method === "PUT") return Promise.resolve({ id: "p1" });
    return Promise.resolve(list);
  });
  renderWithRouter(<ProviderTokens />);
  expect(await screen.findByText(/No provider tokens/)).toBeInTheDocument();
  fireEvent.click(screen.getByRole("button", { name: "Add token" }));
  const dialog = screen.getByRole("dialog", { name: "Add provider token" });
  fireEvent.change(within(dialog).getByLabelText("Name"), { target: { value: "Hetzner main" } });
  const field = within(dialog).getByLabelText("API token");
  expect(field).toHaveAttribute("type", "password");
  fireEvent.change(field, { target: { value: token } });
  fireEvent.click(within(dialog).getByRole("button", { name: "Save token" }));
  await waitFor(() => expect(request).toHaveBeenCalledWith("POST", "/api/node-providers", { kind: "hetzner", name: "Hetzner main", token }));
  expect(await screen.findByText("Hetzner main")).toBeInTheDocument();
  expect(screen.queryByDisplayValue(token)).not.toBeInTheDocument();

  fireEvent.click(screen.getByRole("button", { name: "Edit Hetzner main" }));
  const edit = screen.getByRole("dialog", { name: "Edit Hetzner main" });
  expect(within(edit).getByLabelText(/Replacement API token/)).toHaveValue("");
  expect(within(edit).getByRole("combobox", { name: "Provider" })).toBeDisabled();
  fireEvent.click(within(edit).getByRole("button", { name: "Save changes" }));
  await waitFor(() => expect(request).toHaveBeenCalledWith("PUT", "/api/node-providers/p1", { kind: "hetzner", name: "Hetzner main", token: "" }));
});

it("keeps a rejected token out of the mutation cache", async () => {
  vi.mocked(request).mockImplementation((method) => (method === "POST" ? Promise.reject(new ApiError(400, "The provider rejected the token.")) : Promise.resolve([])));
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  renderWithRouter(<ProviderTokens />, client);
  expect(await screen.findByText(/No provider tokens/)).toBeInTheDocument();
  fireEvent.click(screen.getByRole("button", { name: "Add token" }));
  const dialog = screen.getByRole("dialog", { name: "Add provider token" });
  fireEvent.change(within(dialog).getByLabelText("Name"), { target: { value: "Hetzner main" } });
  fireEvent.change(within(dialog).getByLabelText("API token"), { target: { value: token } });
  fireEvent.click(within(dialog).getByRole("button", { name: "Save token" }));
  expect(await within(dialog).findByText("The provider rejected the token.")).toBeInTheDocument();
  expect(request).toHaveBeenCalledWith("POST", "/api/node-providers", { kind: "hetzner", name: "Hetzner main", token });
  expect(JSON.stringify(client.getMutationCache().getAll().map((mutation) => mutation.state))).not.toContain(token);
});

it("explains that deleting a token never deletes servers and shows conflicts", async () => {
  vi.mocked(request).mockImplementation((method) => (method === "DELETE" ? Promise.reject(new ApiError(409, "Provider has unfinished server creations.")) : Promise.resolve([hetzner])));
  renderWithRouter(<ProviderTokens />);
  fireEvent.click(await screen.findByRole("button", { name: "Delete Hetzner main" }));
  const dialog = screen.getByRole("dialog", { name: "Delete Hetzner main?" });
  expect(within(dialog).getByText(/keep running and stay billed/)).toBeInTheDocument();
  fireEvent.click(within(dialog).getByRole("button", { name: "Delete token" }));
  expect(await within(dialog).findByText("Provider has unfinished server creations.")).toBeInTheDocument();
});

function mockCreation(create: () => Promise<unknown>) {
  let jobs: Provision[] = [];
  vi.mocked(request).mockImplementation((method, path) => {
    if (path === "/api/node-providers") return Promise.resolve([hetzner]);
    if (path === "/api/node-providers/p1/catalog") return Promise.resolve(catalog);
    if (path === "/api/node-provisions" && method === "POST") return create().then((result) => { jobs = [job({})]; return result; });
    if (path === "/api/node-provisions") return Promise.resolve(jobs);
    return Promise.reject(new Error(`unexpected ${method} ${path}`));
  });
}

async function fillServer() {
  renderWithRouter(<CloudServers />);
  const create = await screen.findByRole("button", { name: "Create server" });
  await waitFor(() => expect(create).toBeEnabled());
  fireEvent.click(create);
  expect(request).not.toHaveBeenCalledWith("GET", "/api/node-providers/p1/catalog", undefined, expect.anything());
  fireEvent.change(screen.getByLabelText("Server name"), { target: { value: "worker-1" } });
  fireEvent.click(screen.getByRole("button", { name: "Load catalogue" }));
  await choose("Region", "Ashburn");
  expect(screen.queryByRole("option", { name: /CX22/ })).not.toBeInTheDocument();
  await choose("Plan", /^CPX11 · 2 vCPU · 2 GB · x86 · USD\s4\.99 per month$/);
  await choose("Image", "Debian 12 (x86)");
  expect(screen.getByRole("combobox", { name: "Image" })).toHaveAccessibleDescription(/prepared with a trusted Cosign v3 or later.*Without it, enrollment stops as uncertain/);
  fireEvent.click(screen.getByRole("button", { name: "Review paid server" }));
  return screen.getByRole("dialog", { name: "Create a paid server?" });
}

it("loads the catalogue on request and needs explicit paid-server consent", async () => {
  mockCreation(() => Promise.resolve({ id: "j1" }));
  const dialog = await fillServer();
  expect(within(dialog).getByText(/Removing the node from Cove or abandoning tracking does not delete the server/)).toBeInTheDocument();
  expect(within(dialog).getByText(/Cove never downloads a verifier onto the server/)).toBeInTheDocument();
  const confirm = within(dialog).getByRole("button", { name: "Create paid server" });
  expect(confirm).toBeDisabled();
  fireEvent.click(within(dialog).getByLabelText(/I understand/));
  expect(confirm).toBeDisabled();
  fireEvent.click(confirm);
  expect(request).not.toHaveBeenCalledWith("POST", "/api/node-provisions", expect.anything());
  fireEvent.click(within(dialog).getByLabelText(/I understand/));
  fireEvent.click(within(dialog).getByLabelText(/already contains a trusted Cosign v3 or later/));
  expect(confirm).toBeDisabled();
  fireEvent.click(within(dialog).getByLabelText(/I understand/));
  fireEvent.click(confirm);
  await waitFor(() => expect(request).toHaveBeenCalledWith("POST", "/api/node-provisions", { provider_id: "p1", name: "worker-1", region: "ash", plan: "cpx11", image: "debian-12", confirm: true, confirm_verifier: true }));
  expect(await screen.findByText("Server creation queued")).toBeInTheDocument();
  expect(await screen.findByText("Queued")).toBeInTheDocument();
});

it("explains how to prepare an image when the catalogue lists none", async () => {
  vi.mocked(request).mockImplementation((_method, path) => {
    if (path === "/api/node-providers") return Promise.resolve([hetzner]);
    if (path === "/api/node-providers/p1/catalog") return Promise.resolve({ ...catalog, images: [] });
    return Promise.resolve([]);
  });
  renderWithRouter(<CloudServers />);
  const create = await screen.findByRole("button", { name: "Create server" });
  await waitFor(() => expect(create).toBeEnabled());
  fireEvent.click(create);
  fireEvent.click(screen.getByRole("button", { name: "Load catalogue" }));
  expect(await screen.findByText("No prepared images or snapshots")).toBeInTheDocument();
  expect(screen.getByText(/create a cloud-init-ready Linux image or snapshot that already contains a trusted Cosign v3 or later/)).toBeInTheDocument();
  expect(screen.getByRole("combobox", { name: "Image" })).toBeDisabled();
});

it("treats a lost creation response as possibly queued and never retries", async () => {
  mockCreation(() => Promise.reject(new ApiError(0, "Cannot reach the control plane. Check your connection.")));
  const dialog = await fillServer();
  fireEvent.click(within(dialog).getByLabelText(/I understand/));
  fireEvent.click(within(dialog).getByLabelText(/already contains a trusted Cosign v3 or later/));
  fireEvent.click(within(dialog).getByRole("button", { name: "Create paid server" }));
  expect(await within(dialog).findByText("The creation may have been queued")).toBeInTheDocument();
  expect(within(dialog).getByLabelText(/already contains a trusted Cosign v3 or later/)).not.toBeChecked();
  expect(within(dialog).getByRole("button", { name: "Create paid server" })).toBeDisabled();
  expect(vi.mocked(request).mock.calls.filter(([method, path]) => method === "POST" && path === "/api/node-provisions")).toHaveLength(1);
});

it("offers read-only reconcile for uncertain jobs and abandons tracking with a billing warning", async () => {
  const jobs = [job({ id: "u1", phase: "uncertain", server_name: "worker-u", last_error: "trusted_verifier_missing" }), job({ id: "s1", phase: "succeeded", server_name: "worker-s", node_id: "n1", server_id: "42" })];
  vi.mocked(request).mockImplementation((_method, path) => {
    if (path === "/api/node-providers") return Promise.resolve([hetzner]);
    if (path === "/api/node-provisions") return Promise.resolve(jobs);
    return Promise.resolve({ ok: true });
  });
  expect(hasActive(jobs)).toBe(false);
  renderWithRouter(<CloudServers />);
  expect(await screen.findByText("Outcome unknown")).toBeInTheDocument();
  expect(screen.getByText("trusted_verifier_missing")).toBeInTheDocument();
  expect(screen.queryByRole("button", { name: "Reconcile worker-s" })).not.toBeInTheDocument();
  expect(screen.queryByRole("button", { name: "Abandon tracking worker-s" })).not.toBeInTheDocument();

  fireEvent.click(screen.getByRole("button", { name: "Reconcile worker-u" }));
  const reconcile = screen.getByRole("dialog", { name: "Reconcile worker-u?" });
  expect(within(reconcile).getByText(/never creates another server/)).toBeInTheDocument();
  fireEvent.change(within(reconcile).getByLabelText(/Provider server ID/), { target: { value: " 9001 " } });
  fireEvent.click(within(reconcile).getByRole("button", { name: "Look up server" }));
  await waitFor(() => expect(request).toHaveBeenCalledWith("POST", "/api/node-provisions/u1/reconcile", { confirm: true, server_id: "9001" }));

  fireEvent.click(screen.getByRole("button", { name: "Abandon tracking worker-u" }));
  const abandon = screen.getByRole("dialog", { name: "Abandon tracking worker-u?" });
  expect(within(abandon).getByText("The server may still be billed")).toBeInTheDocument();
  fireEvent.click(within(abandon).getByRole("button", { name: "Abandon tracking" }));
  await waitFor(() => expect(request).toHaveBeenCalledWith("DELETE", "/api/node-provisions/u1", { confirm: true }));
});

it("polls only while a job is still being worked on", () => {
  expect(hasActive([job({ phase: "enrolling" })])).toBe(true);
  expect(hasActive([job({ phase: "uncertain" }), job({ phase: "failed" })])).toBe(false);
});

it("shows a persistent billing warning only for cloud-created nodes", async () => {
  vi.mocked(request).mockResolvedValueOnce({ provider_id: null, kind: "", server_id: "", server_name: "", checked_at: new Date().toISOString() });
  const { unmount } = render(<QueryClientProvider client={new QueryClient()}><NodeCloudOrigin nodeId="manual" /></QueryClientProvider>);
  await waitFor(() => expect(request).toHaveBeenCalled());
  expect(screen.queryByText("Cloud server")).not.toBeInTheDocument();
  unmount();

  vi.mocked(request).mockResolvedValueOnce({ provider_id: "p1", kind: "vultr", server_id: "abc-123", server_name: "worker-1", checked_at: new Date().toISOString() });
  render(<QueryClientProvider client={new QueryClient()}><NodeCloudOrigin nodeId="n1" /></QueryClientProvider>);
  expect(await screen.findByText("Deleting this node does not delete the server")).toBeInTheDocument();
  expect(screen.getByText("abc-123")).toBeInTheDocument();
  expect(screen.getByText("Vultr")).toBeInTheDocument();
});

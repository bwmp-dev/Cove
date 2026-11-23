import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { ApiError, request } from "../api/client";
import type { ScimGroup, ScimSettings } from "../api/directory";
import { ScimProvisioning } from "./ScimProvisioning";

vi.mock("../api/client", async (original) => ({ ...(await original<typeof import("../api/client")>()), request: vi.fn() }));
vi.mock("@tanstack/react-router", () => ({ useBlocker: () => ({ status: "idle" }) }));
HTMLDialogElement.prototype.showModal = function (this: HTMLDialogElement) {
  this.open = true;
};
HTMLDialogElement.prototype.close = function (this: HTMLDialogElement) {
  this.open = false;
};

const token = "scim_0123456789abcdef";
const settings = (overrides: Partial<ScimSettings> = {}): ScimSettings => ({
  enabled: false,
  token_configured: false,
  oidc_provider_id: "",
  rotated_at: null,
  version: 0,
  base_url: "https://cove.example.com/scim/v2",
  ...overrides,
});
const group: ScimGroup = { id: "g1", display_name: "Engineers", external_id: "00g1", version: 5, members: 4, mappings: [{ group: "g1", team_id: "team", role: "editor" }] };

let activity: unknown[] = [];

function serve(current: ScimSettings, routes: Record<string, (body: unknown) => Promise<unknown>> = {}) {
  vi.mocked(request).mockImplementation((method, path, body) => {
    const handler = routes[`${method} ${path}`];
    if (handler) return handler(body);
    if (path === "/api/settings/scim") return Promise.resolve(current);
    if (path === "/api/settings/scim/groups") return Promise.resolve({ groups: [group] });
    if (path === "/api/settings/scim/links") return Promise.resolve({ links: [{ user_id: "u1", user_name: "ada@example.com", external_id: "sub-1", oidc_provider_id: "", active: false, deleted_at: null, version: 2 }] });
    if (path === "/api/settings/oidc/providers") return Promise.resolve([]);
    if (path === "/api/settings/directory/teams") return Promise.resolve({ teams: [{ id: "team", name: "Platform" }] });
    if (path === "/api/settings/scim/activity") return Promise.resolve({ activity });
    if (path === "/api/teams/team/roles") return Promise.resolve({ roles: [], permission_catalogue: [], built_in: { viewer: [], operator: [], editor: [] } });
    if (path === "/api/users") return Promise.resolve([]);
    return Promise.reject(new ApiError(404, "Not found"));
  });
}

let client: QueryClient;
function renderScim() {
  client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={client}>
      <ScimProvisioning />
    </QueryClientProvider>,
  );
}

beforeEach(() => vi.stubGlobal("ResizeObserver", class { observe = vi.fn(); disconnect = vi.fn(); }));
afterEach(() => {
  activity = [];
  vi.clearAllMocks();
  vi.unstubAllGlobals();
});

it("shows a generated token once and keeps it out of the query cache", async () => {
  serve(settings(), { "POST /api/settings/scim/token": () => Promise.resolve({ ok: true, version: 1, token }) });
  renderScim();
  expect(await screen.findByText("https://cove.example.com/scim/v2")).toBeInTheDocument();
  fireEvent.click(screen.getByRole("button", { name: "Generate token" }));
  await waitFor(() => expect(request).toHaveBeenCalledWith("POST", "/api/settings/scim/token", { version: 0, confirm: true }));
  expect(await screen.findByLabelText("New SCIM bearer token")).toHaveValue(token);
  expect(screen.getByRole("button", { name: "Copy SCIM token" })).toBeInTheDocument();
  expect(JSON.stringify(client.getQueryCache().getAll().map((query) => query.state.data))).not.toContain(token);
  fireEvent.click(screen.getByRole("button", { name: "I have saved the token" }));
  await waitFor(() => expect(screen.queryByLabelText("New SCIM bearer token")).not.toBeInTheDocument());
  expect(screen.queryByDisplayValue(token)).not.toBeInTheDocument();
});

it("confirms rotation against the reviewed settings revision", async () => {
  serve(settings({ token_configured: true, enabled: true, version: 4, rotated_at: "2026-11-20T10:00:00Z" }), { "POST /api/settings/scim/token": () => Promise.resolve({ ok: true, version: 5, token }) });
  renderScim();
  fireEvent.click(await screen.findByRole("button", { name: "Rotate token" }));
  const dialog = await screen.findByRole("dialog", { name: "Rotate the SCIM token?" });
  expect(dialog).toHaveTextContent("The current token stops working immediately");
  expect(request).not.toHaveBeenCalledWith("POST", "/api/settings/scim/token", expect.anything());
  fireEvent.click(within(dialog).getByRole("button", { name: "Rotate token" }));
  await waitFor(() => expect(request).toHaveBeenCalledWith("POST", "/api/settings/scim/token", { version: 4, confirm: true }));
  expect(await screen.findByLabelText("New SCIM bearer token")).toHaveValue(token);
});

it("tells the administrator to reload when the settings changed", async () => {
  serve(settings({ token_configured: true, version: 2 }), { "PUT /api/settings/scim": () => Promise.reject(new ApiError(409, "Provisioning settings changed; reload before saving")) });
  renderScim();
  fireEvent.click(await screen.findByRole("switch", { name: "Accept provisioning requests" }));
  fireEvent.click(screen.getByRole("button", { name: "Save changes" }));
  await waitFor(() => expect(request).toHaveBeenCalledWith("PUT", "/api/settings/scim", { version: 2, enabled: true, oidc_provider_id: "" }));
  expect(await screen.findByText(/Reload, review the current values and save again/)).toBeInTheDocument();
  expect(screen.getByRole("button", { name: "Reload settings" })).toBeInTheDocument();
});

it("saves a group's mappings with the reviewed group revision and reports a stale group", async () => {
  serve(settings({ token_configured: true }), { "PUT /api/settings/scim/groups/g1/mappings": () => Promise.reject(new ApiError(409, "Review the current group and confirm its Cove role mappings")) });
  renderScim();
  fireEvent.click(await screen.findByRole("button", { name: /Edit mappings\s*for Engineers/ }));
  fireEvent.click(screen.getByRole("button", { name: "Review and save" }));
  const dialog = await screen.findByRole("dialog", { name: "Save mappings for Engineers?" });
  expect(dialog).toHaveTextContent("This replaces the mappings of this group only");
  fireEvent.click(within(dialog).getByRole("button", { name: "Save mappings" }));
  await waitFor(() => expect(request).toHaveBeenCalledWith("PUT", "/api/settings/scim/groups/g1/mappings", { group_version: 5, confirm: true, mappings: [{ team_id: "team", role: "editor" }] }));
  expect(await screen.findByText("This group changed")).toBeInTheDocument();
  expect(screen.getByRole("button", { name: "Review and save" })).toBeDisabled();
});

it("shows deprovisioned accounts as disabled with revoked sessions", async () => {
  serve(settings({ token_configured: true }));
  renderScim();
  expect(await screen.findByText("Disabled, sessions and tokens revoked")).toBeInTheDocument();
});

it("lists recent provisioning activity with names where known", async () => {
  activity = [
    { id: 9, time: "2026-11-22T10:00:00Z", operation: "PATCH SCIM groups", target_id: "g1", status: 409, outcome: "failed" },
    { id: 8, time: "2026-11-22T09:59:00Z", operation: "POST SCIM users", target_id: "u1", status: 200, outcome: "succeeded" },
    { id: 7, time: "2026-11-22T09:58:00Z", operation: "POST SCIM users", target_id: "", status: 400, outcome: "failed" },
  ];
  serve(settings({ token_configured: true }));
  renderScim();
  const list = await screen.findByRole("list", { name: "Provisioning activity" });
  expect(within(list).getByText("Failed (HTTP 409)")).toBeInTheDocument();
  expect(within(list).getByText("Engineers")).toBeInTheDocument();
  expect(within(list).getByText("ada@example.com")).toBeInTheDocument();
  expect(within(list).getAllByText("Created user")).toHaveLength(2);
});

it("explains an empty activity list", async () => {
  serve(settings({ token_configured: true }));
  renderScim();
  expect(await screen.findByText("No provisioning changes yet.")).toBeInTheDocument();
});

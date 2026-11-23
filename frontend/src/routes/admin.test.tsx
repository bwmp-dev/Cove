import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { request } from "../api/client";
import { AdminPage } from "./admin";

const session = vi.hoisted(() => ({ is_admin: true }));
vi.mock("../api/client", () => ({ request: vi.fn(), errorMessage: () => "Update unavailable", isStatus: () => false }));
vi.mock("../lib/session", () => ({ useSession: () => session }));
vi.mock("@tanstack/react-router", () => ({ useRouterState: () => "", useNavigate: () => vi.fn(), useBlocker: () => ({ status: "idle" }), Link: ({ children }: { children: React.ReactNode }) => <span>{children}</span> }));
// jsdom does not implement modal dialogs.
HTMLDialogElement.prototype.showModal = function (this: HTMLDialogElement) {
  this.open = true;
};
HTMLDialogElement.prototype.close = function (this: HTMLDialogElement) {
  this.open = false;
};

function securityResponse(path: string) {
  if (path === "/api/settings/oidc/providers") return Promise.resolve([]);
  if (path === "/api/settings/sign-in") return Promise.resolve({ password_login_disabled: false, oidc_registration_enabled: false, oidc_auto_redirect: "" });
  if (path === "/api/settings/template-repositories") return Promise.resolve({ repositories: [] });
  if (path === "/api/settings/second-factor") return Promise.resolve({ require_second_factor: false });
  if (path === "/api/settings/github-team-sync") return Promise.resolve({ sources: [] });
  if (path === "/api/settings/scim") return Promise.resolve({ enabled: false, token_configured: false, oidc_provider_id: "", rotated_at: null, version: 0, base_url: "https://cove.example.com/scim/v2" });
  if (path === "/api/settings/scim/groups") return Promise.resolve({ groups: [] });
  if (path === "/api/settings/scim/links") return Promise.resolve({ links: [] });
  if (path === "/api/settings/ldap/providers") return Promise.resolve({ network_policy_configured: true, providers: [] });
  if (path === "/api/users") return Promise.resolve([]);
  if (path === "/api/settings/scim/activity") return Promise.resolve({ activity: [] });
  if (path === "/api/settings/directory/teams") return Promise.resolve({ teams: [] });
  if (path === "/api/settings/audit-export") return Promise.resolve({ configuration: { webhook_url: "", syslog_address: "", syslog_tls: true, retention_days: 365 }, webhook_secret_configured: false, pending_deliveries: 0 });
  return undefined;
}

beforeEach(() => vi.stubGlobal("ResizeObserver", class { observe = vi.fn(); disconnect = vi.fn(); }));
afterEach(() => { vi.clearAllMocks(); vi.unstubAllGlobals(); session.is_admin = true; });

const fresh = new Date().toISOString();
function target(overrides: Record<string, unknown>) {
  return { version: "v0.2.0", revision: "beta-rev-0000001", schema_hash: "s1", revision_time: fresh, checked_at: fresh, source_root: "root", source_generation: 2, available: true, downgrade: false, allowed: true, message: "The observed channel does not downgrade the installed panel.", ...overrides };
}

function mockUpdates(updates: Record<string, unknown>, put?: (body: unknown) => Promise<unknown>) {
  vi.mocked(request).mockImplementation((method, path, body) => {
    if (path === "/api/settings/integrations") return Promise.resolve({ smtp: { host: "", port: "", username: "", from: "", password_set: false }, github: { client_id: "", client_secret_set: false, callback_url: "https://cove.example.com/auth/github/callback" } });
    if (path === "/api/settings") return Promise.resolve({ registration_enabled: false, email_enabled: false, github_enabled: false });
    const security = securityResponse(path);
    if (security) return security;
    if (path === "/api/updates" && method === "PUT") return put ? put(body) : Promise.resolve({ ...updates, channel: (body as { channel: string }).channel });
    return Promise.resolve(updates);
  });
}

function renderAdmin() {
  render(<QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}><AdminPage /></QueryClientProvider>);
}

it("saves an allowed channel bound to the reviewed target revision", async () => {
  mockUpdates({ channel: "release", version: "v0.1.0", build_channel: "release", revision: "abc", targets: { release: target({ version: "v0.1.0", revision: "abc" }), beta: target({}) } });
  renderAdmin();
  expect(await screen.findByText("v0.2.0")).toBeInTheDocument();
  fireEvent.click(screen.getByRole("combobox", { name: "Update channel" }));
  fireEvent.click(await screen.findByRole("option", { name: "Beta — every successful main build" }));
  await waitFor(() => expect(request).toHaveBeenCalledWith("PUT", "/api/updates", { channel: "beta", target_revision: "beta-rev-0000001" }));
  expect(await screen.findByText("Update channel saved")).toBeInTheDocument();
});

it("asks for explicit confirmation naming both versions before a downgrade", async () => {
  mockUpdates({ channel: "beta", version: "v0.3.0", build_channel: "beta", revision: "def", targets: { beta: target({ version: "v0.3.0", revision: "def" }), release: target({ version: "v0.1.0", revision: "rel-rev-111111", downgrade: true, message: "This channel would downgrade the panel." }) } });
  renderAdmin();
  fireEvent.click(await screen.findByRole("combobox", { name: "Update channel" }));
  fireEvent.click(await screen.findByRole("option", { name: "Release — published stable versions" }));
  const dialog = await screen.findByRole("dialog", { name: "Downgrade the panel?" });
  expect(dialog).toHaveTextContent("v0.3.0");
  expect(dialog).toHaveTextContent("v0.1.0");
  expect(request).not.toHaveBeenCalledWith("PUT", "/api/updates", expect.anything());
  fireEvent.click(within(dialog).getByRole("button", { name: "Switch to release" }));
  await waitFor(() => expect(request).toHaveBeenCalledWith("PUT", "/api/updates", { channel: "release", confirm_downgrade: true, target_revision: "rel-rev-111111" }));
});

it("never offers a blocked or unobserved channel", async () => {
  mockUpdates({ channel: "beta", version: "v0.3.0", build_channel: "beta", revision: "def", targets: { beta: target({ version: "v0.3.0", revision: "def" }), release: target({ version: "v0.1.0", downgrade: true, allowed: false, message: "Blocked: this channel would downgrade to a different schema." }) } });
  renderAdmin();
  expect(await screen.findByText("Blocked: this channel would downgrade to a different schema.")).toBeInTheDocument();
  expect(screen.getByText("Blocked")).toBeInTheDocument();
  fireEvent.click(screen.getByRole("combobox", { name: "Update channel" }));
  const option = await screen.findByRole("option", { name: "Release — published stable versions" });
  expect(option).toHaveAttribute("aria-disabled", "true");
  fireEvent.click(option);
  expect(screen.queryByRole("dialog", { name: "Downgrade the panel?" })).not.toBeInTheDocument();
  expect(request).not.toHaveBeenCalledWith("PUT", "/api/updates", expect.anything());
});

it("reports a rejected channel change from the API", async () => {
  mockUpdates({ channel: "release", version: "v0.1.0", build_channel: "release", revision: "abc", targets: { release: target({ version: "v0.1.0", revision: "abc" }), beta: target({}) } }, () => Promise.reject(new Error("channel observation changed")));
  renderAdmin();
  fireEvent.click(await screen.findByRole("combobox", { name: "Update channel" }));
  fireEvent.click(await screen.findByRole("option", { name: "Beta — every successful main build" }));
  expect(await screen.findByText("Could not change update channel")).toBeInTheDocument();
});

it("keeps channel controls private to instance administrators", () => {
  session.is_admin = false;
  render(<QueryClientProvider client={new QueryClient()}><AdminPage /></QueryClientProvider>);
  expect(screen.queryByRole("combobox", { name: "Update channel" })).not.toBeInTheDocument();
  expect(request).not.toHaveBeenCalled();
});

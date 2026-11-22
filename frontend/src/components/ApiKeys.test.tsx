import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { ReactNode } from "react";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import type { ApiScope } from "../api/access-tokens";
import { request } from "../api/client";
import { ApiKeyList, CreateApiKey } from "./ApiKeys";

interface TestSession { user_id: string; email?: string; is_admin: boolean; team_id: string; teams: { id: string; name: string; role: string; grants: never[] }[] }
const memberOf = (role: string): TestSession => ({ user_id: "u1", is_admin: false, team_id: "team", teams: [{ id: "team", name: "Platform", role, grants: [] }] });
const session = vi.hoisted((): { value?: TestSession } => ({}));
vi.mock("../api/client", () => ({ request: vi.fn(), errorMessage: (error: unknown) => (error instanceof Error ? error.message : String(error)) }));
vi.mock("../lib/session", () => ({ useSession: () => session.value }));
afterEach(() => {
  vi.clearAllMocks();
});
beforeEach(() => {
  session.value = memberOf("admin");
});
HTMLDialogElement.prototype.showModal = function (this: HTMLDialogElement) {
  this.open = true;
};
HTMLDialogElement.prototype.close = function (this: HTMLDialogElement) {
  this.open = false;
};

const scopes: ApiScope[] = [
  { name: "workloads:read", level: 1, description: "Read accessible resources, runtime and release metadata.", instance_admin: false, personal_only: false },
  { name: "workloads:operate", level: 2, description: "Deploy, restart, stop, start, roll back and back up resources.", instance_admin: false, personal_only: false },
  { name: "exec:run", level: 3, description: "Run a bounded one-off command in a resource container.", instance_admin: false, personal_only: false },
  { name: "migration:adopt", level: 3, description: "Instance administrators only: inspect and adopt unmanaged Compose projects; personal tokens only.", instance_admin: true, personal_only: true },
];

function wrap(children: ReactNode) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return { client, ...render(<QueryClientProvider client={client}>{children}</QueryClientProvider>) };
}

function mockApi(post: () => Promise<unknown>) {
  vi.mocked(request).mockImplementation((method: string, path: string) => {
    if (path === "/api/access-tokens/scopes") return Promise.resolve(scopes);
    if (method === "GET") return Promise.resolve([]);
    return post();
  });
}

it("offers restricted scopes only on an instance administrator's personal key", async () => {
  const user = userEvent.setup();
  session.value = { ...memberOf("owner"), is_admin: true };
  mockApi(() => Promise.resolve({ id: "k1", token: "cove_secret_value", expires_at: "2027-01-01T00:00:00Z" }));
  wrap(<CreateApiKey />);
  const adopt = await screen.findByRole("checkbox", { name: /migration:adopt/ });
  expect(adopt).toBeEnabled();
  await user.click(adopt);
  await user.click(screen.getByRole("checkbox", { name: /workloads:read/ }));

  await user.click(screen.getByRole("radio", { name: /Team/ }));
  expect(adopt).toBeDisabled();
  expect(adopt).not.toBeChecked();
  expect(adopt).toHaveAccessibleDescription(expect.stringContaining("Available on personal keys only."));
  await user.click(screen.getByRole("radio", { name: /Personal/ }));
  expect(adopt).toBeEnabled();
  expect(adopt).not.toBeChecked();

  await user.click(screen.getByRole("radio", { name: /Team/ }));
  await user.type(screen.getByLabelText("Name"), "Fleet bot");
  await user.click(screen.getByRole("button", { name: "Create API key" }));
  await waitFor(() => expect(request).toHaveBeenCalledWith("POST", "/api/access-tokens", { name: "Fleet bot", scope: "team", scopes: ["workloads:read"], days: 30 }));
});

it("disables instance administrator scopes for other accounts", async () => {
  mockApi(() => Promise.reject(new Error("unexpected")));
  wrap(<CreateApiKey />);
  const adopt = await screen.findByRole("checkbox", { name: /migration:adopt/ });
  expect(adopt).toBeDisabled();
  expect(adopt).toHaveAccessibleDescription(expect.stringContaining("Only instance administrators can grant this permission."));
});

it("creates a team key with explicit permissions and shows the key only until dismissed", async () => {
  const user = userEvent.setup();
  mockApi(() => Promise.resolve({ id: "k1", token: "cove_secret_value", expires_at: "2027-01-01T00:00:00Z" }));
  const { client } = wrap(<CreateApiKey />);
  await screen.findByText("workloads:read");
  await user.type(screen.getByLabelText("Name"), "Deploy bot");
  await user.click(screen.getByRole("button", { name: "Create API key" }));
  expect(screen.getByText("Choose at least one permission.")).toBeInTheDocument();
  expect(request).not.toHaveBeenCalledWith("POST", expect.anything(), expect.anything());

  await user.click(screen.getByRole("radio", { name: /Team/ }));
  await user.click(screen.getByRole("checkbox", { name: /workloads:operate/ }));
  await user.click(screen.getByRole("checkbox", { name: /workloads:read/ }));
  await user.click(screen.getByRole("button", { name: "Create API key" }));
  await waitFor(() => expect(request).toHaveBeenCalledWith("POST", "/api/access-tokens", { name: "Deploy bot", scope: "team", scopes: ["workloads:read", "workloads:operate"], days: 30 }));
  const field = await screen.findByLabelText("New API key");
  expect(field).toHaveValue("cove_secret_value");
  expect(field).toHaveFocus();

  await user.click(screen.getByRole("button", { name: "Done" }));
  expect(screen.queryByLabelText("New API key")).not.toBeInTheDocument();
  expect(screen.getByLabelText("Name")).toHaveValue("");
  await waitFor(() => expect(JSON.stringify(client.getMutationCache().getAll().map((mutation) => mutation.state.data))).not.toContain("cove_secret_value"));
});

it("offers only personal keys to members and shows backend refusals", async () => {
  const user = userEvent.setup();
  session.value = memberOf("member");
  mockApi(() => Promise.reject(new Error("Permission denied")));
  wrap(<CreateApiKey />);
  await screen.findByText("exec:run");
  expect(screen.queryByRole("radio")).not.toBeInTheDocument();
  await user.type(screen.getByLabelText("Name"), "Laptop");
  await user.click(screen.getByRole("checkbox", { name: /exec:run/ }));
  await user.click(screen.getByRole("button", { name: "Create API key" }));
  await waitFor(() => expect(request).toHaveBeenCalledWith("POST", "/api/access-tokens", { name: "Laptop", scope: "user", scopes: ["exec:run"], days: 30 }));
  expect(await screen.findByText("Permission denied")).toBeInTheDocument();
});

it("explains that the shared administrator sign-in cannot create keys", async () => {
  session.value = { ...memberOf("admin"), user_id: "bootstrap", is_admin: true };
  mockApi(() => Promise.reject(new Error("unexpected")));
  wrap(<CreateApiKey />);
  await screen.findByText("workloads:read");
  expect(screen.getByText("Sign in with your own account")).toBeInTheDocument();
  expect(screen.getByRole("button", { name: "Create API key" })).toBeDisabled();
});

it("lets a verified legacy account with the bootstrap ID create keys", async () => {
  const user = userEvent.setup();
  session.value = { ...memberOf("admin"), user_id: "bootstrap", email: "owner@example.com", is_admin: true };
  mockApi(() => Promise.resolve({ id: "k1", token: "cove_secret_value", expires_at: "2027-01-01T00:00:00Z" }));
  wrap(<CreateApiKey />);
  await screen.findByText("workloads:read");
  expect(screen.queryByText("Sign in with your own account")).not.toBeInTheDocument();
  await user.type(screen.getByLabelText("Name"), "Owner laptop");
  await user.click(screen.getByRole("checkbox", { name: /workloads:read/ }));
  await user.click(screen.getByRole("button", { name: "Create API key" }));
  await waitFor(() => expect(request).toHaveBeenCalledWith("POST", "/api/access-tokens", { name: "Owner laptop", scope: "user", scopes: ["workloads:read"], days: 30 }));
});

it("lists keys without values, marks expired keys and revokes after confirmation", async () => {
  const user = userEvent.setup();
  const keys = [
    { id: "a1", name: "CI", user_id: "u1", team_id: "", scope: "user", scopes: ["workloads:read", "logs:read"], expires_at: "2999-01-01T00:00:00Z" },
    { id: "b2", name: "Old team key", user_id: "u2", team_id: "team", scope: "team", scopes: ["workloads:operate"], expires_at: "2000-01-01T00:00:00Z" },
  ];
  vi.mocked(request).mockImplementation((method: string) => Promise.resolve(method === "GET" ? keys : { ok: true }));
  wrap(<ApiKeyList />);
  const table = await screen.findByRole("table", { name: "API keys" });
  expect(within(table).getByText("workloads:read logs:read")).toBeInTheDocument();
  expect(within(table).getByText(/Team · Platform · created by another member/)).toBeInTheDocument();
  expect(within(table).getAllByText("Expired").length).toBeGreaterThan(0);

  await user.click(screen.getByRole("button", { name: /^Revoke\s*Old team key$/ }));
  const dialog = screen.getByRole("dialog");
  expect(within(dialog).getByText("Revoke Old team key?")).toBeInTheDocument();
  await user.click(within(dialog).getByRole("button", { name: "Revoke key" }));
  await waitFor(() => expect(request).toHaveBeenCalledWith("DELETE", "/api/access-tokens/b2"));
});

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { ReactNode } from "react";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { request } from "../api/client";
import { SecretProviders } from "./SecretProviders";

const session = vi.hoisted(() => ({ value: { is_admin: false, team_id: "team", teams: [{ id: "team", name: "Team", role: "member", grants: [{ scope: "team", role: "editor" }] }], permissions: ["view", "operate", "read_secrets", "edit_configuration", "manage_backups"] } }));
vi.mock("../api/client", () => ({ request: vi.fn(), errorMessage: (error: unknown) => (error instanceof Error ? error.message : String(error)) }));
vi.mock("../lib/session", () => ({ useSession: () => session.value }));
beforeEach(() => vi.stubGlobal("ResizeObserver", class { observe = vi.fn(); disconnect = vi.fn(); }));
afterEach(() => {
  vi.clearAllMocks();
  vi.unstubAllGlobals();
});
HTMLDialogElement.prototype.showModal = function (this: HTMLDialogElement) {
  this.open = true;
};
HTMLDialogElement.prototype.close = function (this: HTMLDialogElement) {
  this.open = false;
};

const vault = { id: "a".repeat(32), name: "Team Vault", kind: "vault", url: "https://vault.example.com", project: "", environment: "", namespace: "admin", available: true };
const bitwarden = { id: "b".repeat(32), name: "Bitwarden", kind: "bitwarden", url: "https://vault.bitwarden.com", project: "", environment: "", namespace: "", available: false };

function wrap(children: ReactNode) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return { client, ...render(<QueryClientProvider client={client}>{children}</QueryClientProvider>) };
}

function mutationState(client: QueryClient) {
  return JSON.stringify(client.getMutationCache().getAll().map((mutation) => ({ variables: mutation.state.variables, data: mutation.state.data })));
}

it("lists public metadata without claiming provider health", async () => {
  vi.mocked(request).mockResolvedValue([vault, bitwarden]);
  wrap(<SecretProviders />);
  const list = await screen.findByRole("list", { name: "Secret providers" });
  expect(within(list).getByText("Team Vault")).toBeInTheDocument();
  expect(within(list).getByText("bws CLI not found on the control host")).toBeInTheDocument();
  expect(within(list).getByText(vault.id)).toBeInTheDocument();
  expect(screen.queryByText(/connected|healthy/i)).not.toBeInTheDocument();
});

it("saves an Infisical connection with its project and environment and clears the token", async () => {
  const user = userEvent.setup();
  vi.mocked(request).mockImplementation((method: string) => Promise.resolve(method === "GET" ? [] : { ...vault, kind: "infisical", name: "Infisical" }));
  const { client } = wrap(<SecretProviders />);
  expect(await screen.findByText("No secret providers saved.")).toBeInTheDocument();
  await user.click(screen.getByRole("combobox", { name: "Provider" }));
  await user.click(screen.getByRole("option", { name: "Infisical" }));
  await user.type(screen.getByLabelText("Name"), "Infisical");
  await user.click(screen.getByRole("button", { name: "Save provider" }));
  expect(screen.getByText("Enter the project ID.")).toBeInTheDocument();
  expect(request).not.toHaveBeenCalledWith("POST", expect.anything(), expect.anything());

  await user.type(screen.getByLabelText("Project ID"), "proj-1");
  await user.type(screen.getByLabelText("Environment slug"), "prod");
  await user.type(screen.getByLabelText("Access token"), "st.token-value");
  await user.click(screen.getByRole("button", { name: "Save provider" }));
  await waitFor(() => expect(request).toHaveBeenCalledWith("POST", "/api/secret-providers", { name: "Infisical", kind: "infisical", url: "https://app.infisical.com", token: "st.token-value", project: "proj-1", environment: "prod" }));
  expect(await screen.findByText("Infisical saved")).toBeInTheDocument();
  expect(screen.getByLabelText("Access token")).toHaveValue("");
  expect(client.getMutationCache().getAll()).not.toHaveLength(0);
  expect(mutationState(client)).not.toContain("st.token-value");
});

it("keeps the backend error and drops the token when saving fails", async () => {
  const user = userEvent.setup();
  vi.mocked(request).mockImplementation((method: string) => (method === "GET" ? Promise.resolve([]) : Promise.reject(new Error("vault URL must be a public HTTPS destination"))));
  const { client } = wrap(<SecretProviders />);
  expect(await screen.findByText("No secret providers saved.")).toBeInTheDocument();
  await user.type(screen.getByLabelText("Name"), "Vault");
  await user.type(screen.getByLabelText("Server URL"), "https://vault.example.com");
  await user.type(screen.getByLabelText("Token"), "hvs.secret-token");
  await user.click(screen.getByRole("button", { name: "Save provider" }));
  expect(await screen.findByText("vault URL must be a public HTTPS destination")).toBeInTheDocument();
  expect(screen.getByLabelText("Token")).toHaveValue("");
  expect(client.getMutationCache().getAll()).not.toHaveLength(0);
  expect(mutationState(client)).not.toContain("hvs.secret-token");
});

it("renames a connection with the saved settings and keeps a blank token", async () => {
  const user = userEvent.setup();
  vi.mocked(request).mockImplementation((method: string) => Promise.resolve(method === "GET" ? [vault] : { ...vault, name: "Renamed" }));
  const { client } = wrap(<SecretProviders />);
  await user.click(await screen.findByRole("button", { name: "Edit Team Vault" }));
  const dialog = screen.getByRole("dialog");
  const name = within(dialog).getByLabelText("Name");
  await user.clear(name);
  await user.type(name, "Renamed");
  await user.click(within(dialog).getByRole("button", { name: "Save changes" }));
  await waitFor(() => expect(request).toHaveBeenCalledWith("PUT", `/api/secret-providers/${vault.id}`, { name: "Renamed", kind: "vault", url: vault.url, token: "", project: "", environment: "", namespace: "admin" }));
  await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
  expect(client.getMutationCache().getAll()).toHaveLength(0);
});

it("does not keep a replacement token in the mutation cache when an edit fails", async () => {
  const user = userEvent.setup();
  vi.mocked(request).mockImplementation((method: string) => (method === "GET" ? Promise.resolve([vault]) : Promise.reject(new Error("create a new connection to change provider settings"))));
  const { client } = wrap(<SecretProviders />);
  await user.click(await screen.findByRole("button", { name: "Edit Team Vault" }));
  const dialog = screen.getByRole("dialog");
  await user.type(within(dialog).getByLabelText(/^New token/), "hvs.replacement");
  await user.click(within(dialog).getByRole("button", { name: "Save changes" }));
  expect(await within(dialog).findByText("create a new connection to change provider settings")).toBeInTheDocument();
  expect(within(dialog).getByLabelText(/^New token/)).toHaveValue("");
  expect(client.getMutationCache().getAll()).not.toHaveLength(0);
  expect(mutationState(client)).not.toContain("hvs.replacement");
});

it("warns that deleting a provider breaks deployments that reference it", async () => {
  const user = userEvent.setup();
  vi.mocked(request).mockImplementation((method: string) => Promise.resolve(method === "GET" ? [vault] : { ok: true }));
  wrap(<SecretProviders />);
  await user.click(await screen.findByRole("button", { name: "Delete Team Vault" }));
  expect(screen.getByText(/fails to resolve, and the deployment stops/)).toBeInTheDocument();
  await user.click(screen.getByRole("button", { name: "Delete provider" }));
  await waitFor(() => expect(request).toHaveBeenCalledWith("DELETE", `/api/secret-providers/${vault.id}`));
});

it("hides management controls from team members without team-wide edit access", async () => {
  session.value = { ...session.value, teams: [{ id: "team", name: "Team", role: "member", grants: [] }], permissions: [] };
  vi.mocked(request).mockResolvedValue([vault]);
  wrap(<SecretProviders />);
  expect(await screen.findByText("Team Vault")).toBeInTheDocument();
  expect(screen.queryByRole("button", { name: "Edit Team Vault" })).not.toBeInTheDocument();
  expect(screen.queryByRole("button", { name: "Save provider" })).not.toBeInTheDocument();
});

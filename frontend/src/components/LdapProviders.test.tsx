import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { ApiError, request } from "../api/client";
import type { LdapProvider } from "../api/directory";
import { emptyLdapConfiguration } from "../lib/directory";
import { LdapProviders } from "./LdapProviders";

vi.mock("../api/client", async (original) => ({ ...(await original<typeof import("../api/client")>()), request: vi.fn() }));
HTMLDialogElement.prototype.showModal = function (this: HTMLDialogElement) {
  this.open = true;
};
HTMLDialogElement.prototype.close = function (this: HTMLDialogElement) {
  this.open = false;
};

const provider: LdapProvider = {
  id: "corp",
  enabled: true,
  version: 6,
  password_configured: true,
  configuration: {
    ...emptyLdapConfiguration(),
    display_name: "Corp directory",
    host: "ldap.example.com",
    bind_dn: "cn=cove,dc=example,dc=com",
    user_base_dn: "ou=people,dc=example,dc=com",
    group_base_dn: "ou=groups,dc=example,dc=com",
  },
  mappings: [],
};

function serve(policy: boolean, providers: LdapProvider[], routes: Record<string, (body: unknown) => Promise<unknown>> = {}) {
  vi.mocked(request).mockImplementation((method, path, body) => {
    const handler = routes[`${method} ${path}`];
    if (handler) return handler(body);
    if (path === "/api/settings/ldap/providers") return Promise.resolve({ network_policy_configured: policy, providers });
    if (path === "/api/settings/directory/teams") return Promise.resolve({ teams: [] });
    return Promise.reject(new ApiError(404, "Not found"));
  });
}

function renderLdap() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(
    <QueryClientProvider client={client}>
      <LdapProviders />
    </QueryClientProvider>,
  );
  return client;
}

beforeEach(() => vi.stubGlobal("ResizeObserver", class { observe = vi.fn(); disconnect = vi.fn(); }));
afterEach(() => {
  vi.clearAllMocks();
  vi.unstubAllGlobals();
});

it("explains a missing host network policy instead of offering a bypass", async () => {
  serve(false, []);
  renderLdap();
  expect(await screen.findByText("The host does not allow directory connections")).toBeInTheDocument();
  expect(screen.getByText("COVE_LDAP_ALLOWED_CIDRS")).toBeInTheDocument();
});

it("requires a bind password for a new directory", async () => {
  serve(true, []);
  renderLdap();
  await screen.findByText("No LDAP directories.");
  fireEvent.click(screen.getByRole("button", { name: "Add directory" }));
  const form = screen.getByRole("form", { name: "New LDAP directory" });
  fireEvent.change(within(form).getByLabelText("Display name"), { target: { value: "Corp" } });
  fireEvent.click(within(form).getByRole("button", { name: "Add directory" }));
  expect(await within(form).findByText("Enter the service account's password.")).toBeInTheDocument();
  expect(within(form).getByLabelText("Allow sign-in with this directory")).not.toBeChecked();
  expect(within(form).getByLabelText("Create accounts for new directory users")).not.toBeChecked();
  expect(request).not.toHaveBeenCalledWith("POST", "/api/settings/ldap/providers", expect.anything());
});

it("keeps the saved bind password when the field is left blank and asks before turning sign-in off", async () => {
  serve(true, [provider], { "PUT /api/settings/ldap/providers/corp": () => Promise.resolve({ id: "corp", version: 7, ok: true }) });
  renderLdap();
  fireEvent.click(await screen.findByRole("button", { name: /^Edit\s*Corp directory/ }));
  const form = screen.getByRole("form", { name: "Edit Corp directory" });
  expect(within(form).getByLabelText(/Bind password/)).toHaveValue("");
  fireEvent.click(within(form).getByLabelText("Allow sign-in with this directory"));
  fireEvent.click(within(form).getByRole("button", { name: "Save directory" }));
  const dialog = await screen.findByRole("dialog", { name: "Turn off sign-in with Corp directory?" });
  fireEvent.click(within(dialog).getByRole("button", { name: "Turn off and save" }));
  await waitFor(() => expect(request).toHaveBeenCalledWith("PUT", "/api/settings/ldap/providers/corp", expect.objectContaining({ enabled: false, version: 6 })));
  const body = vi.mocked(request).mock.calls.find(([method]) => method === "PUT")?.[2] as { configuration: Record<string, unknown> };
  expect(body.configuration).not.toHaveProperty("bind_password");
});

it("keeps a replaced bind password out of the mutation cache when saving fails", async () => {
  serve(true, [provider], { "PUT /api/settings/ldap/providers/corp": () => Promise.reject(new ApiError(500, "Directory store unavailable")) });
  const client = renderLdap();
  fireEvent.click(await screen.findByRole("button", { name: /^Edit\s*Corp directory/ }));
  const form = screen.getByRole("form", { name: "Edit Corp directory" });
  fireEvent.change(within(form).getByLabelText(/Bind password/), { target: { value: "bind secret" } });
  fireEvent.click(within(form).getByRole("button", { name: "Save directory" }));
  expect(await within(form).findByText("Directory store unavailable")).toBeInTheDocument();
  const body = vi.mocked(request).mock.calls.find(([method]) => method === "PUT")?.[2] as { configuration: Record<string, unknown> };
  expect(body.configuration.bind_password).toBe("bind secret");
  expect(within(form).getByLabelText(/Bind password/)).toHaveValue("");
  const mutations = client.getMutationCache().getAll();
  expect(mutations.length).toBeGreaterThan(0);
  for (const mutation of mutations) expect(mutation.state.variables).toBeUndefined();
});

it("describes exactly what a connection test proves", async () => {
  serve(true, [provider], { "POST /api/settings/ldap/providers/corp/test": () => Promise.resolve({ ok: true }) });
  renderLdap();
  fireEvent.click(await screen.findByRole("button", { name: /Test connection\s*to Corp directory/ }));
  expect(await screen.findByText(/This does not check the user or group filters or anyone's password/)).toBeInTheDocument();
  expect(request).toHaveBeenCalledWith("POST", "/api/settings/ldap/providers/corp/test");
});

it("keeps a directory with linked identities and explains how to remove it", async () => {
  serve(true, [provider], { "DELETE /api/settings/ldap/providers/corp": () => Promise.reject(new ApiError(409, "Unlink directory identities before deleting; disable the provider temporarily instead")) });
  renderLdap();
  fireEvent.click(await screen.findByRole("button", { name: /^Delete\s*Corp directory/ }));
  fireEvent.click(within(await screen.findByRole("dialog", { name: "Delete Corp directory?" })).getByRole("button", { name: "Delete directory" }));
  await waitFor(() => expect(request).toHaveBeenCalledWith("DELETE", "/api/settings/ldap/providers/corp", { version: 6, confirm: true }));
  expect(await screen.findByText(/Turn sign-in off to pause it/)).toBeInTheDocument();
  expect(screen.getByRole("button", { name: /^Edit\s*Corp directory/ })).toBeInTheDocument();
});

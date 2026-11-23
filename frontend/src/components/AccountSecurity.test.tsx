import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, expect, it, vi } from "vitest";
import { ApiError, request } from "../api/client";
import { SignInIdentities, TwoStepVerification } from "./AccountSecurity";

vi.mock("../api/client", async (original) => ({ ...(await original<typeof import("../api/client")>()), request: vi.fn() }));

const session = { user: "Ada", user_id: "u1", email: "ada@example.com", name: "Ada", is_admin: false, has_password: true, github_linked: true, team_id: "t1", teams: [], permissions: ["view"] };
const factors = { totp_enrolled: true, passkey_enrolled: false, second_factor_required: false, enrolment_required: false, recovery_codes_remaining: 4 };
const publicMethods = { password_enabled: true, passkeys_enabled: true, oidc_registration_enabled: false, oidc_auto_redirect: "", oidc_providers: [{ id: "corp", display_name: "Corp SSO" }], second_factor_pending: false, second_factor_methods: [] };

function serve(overrides: { factors?: Partial<typeof factors> } = {}, routes: Record<string, (body: unknown) => Promise<unknown>> = {}) {
  vi.mocked(request).mockImplementation((method, path, body) => {
    if (path === "/api/session") return Promise.resolve(session);
    if (path === "/api/account/sign-in-methods") return Promise.resolve({ ...factors, ...overrides.factors });
    if (path === "/auth/sign-in-methods") return Promise.resolve(publicMethods);
    const handler = routes[`${method} ${path}`];
    return handler ? handler(body) : Promise.reject(new ApiError(404, "Not found"));
  });
}

function renderWith(element: React.ReactNode) {
  render(<QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}>{element}</QueryClientProvider>);
}

afterEach(() => vi.clearAllMocks());

it("shows the unused recovery codes and regenerates them once the person proves it's them", async () => {
  serve({}, { "POST /api/account/recovery-codes": () => Promise.resolve({ recovery_codes: ["aaaa-1111", "bbbb-2222"] }) });
  renderWith(<TwoStepVerification />);
  expect(await screen.findByText("4 unused codes")).toBeInTheDocument();
  fireEvent.click(screen.getByRole("button", { name: "Generate new codes" }));
  fireEvent.click(screen.getByRole("button", { name: "Generate new codes" }));
  expect(await screen.findByText("Enter your current password.")).toBeInTheDocument();
  fireEvent.change(screen.getByLabelText("Current password"), { target: { value: "correct horse battery" } });
  fireEvent.click(screen.getByRole("button", { name: "Generate new codes" }));
  expect(await screen.findByText("aaaa-1111")).toBeInTheDocument();
  expect(request).toHaveBeenCalledWith("POST", "/api/account/recovery-codes", { current_password: "correct horse battery" });
  expect(screen.queryByText(/ask an instance administrator/i)).not.toBeInTheDocument();
});

it("explains why the authenticator app cannot be removed under a required policy without a passkey", async () => {
  serve({ factors: { second_factor_required: true } });
  renderWith(<TwoStepVerification />);
  expect(await screen.findByRole("button", { name: "Remove" })).toBeDisabled();
  expect(screen.getByRole("button", { name: "Replace" })).toBeDisabled();
  expect(screen.getByText(/Add a passkey first/)).toBeInTheDocument();
});

it("removes the authenticator app with a code and reports a policy refusal from another team", async () => {
  serve({}, { "DELETE /api/account/totp": () => Promise.reject(new ApiError(409, "Second factor required by policy")) });
  renderWith(<TwoStepVerification />);
  fireEvent.click(await screen.findByRole("button", { name: "Remove" }));
  fireEvent.click(screen.getByRole("radio", { name: "Authenticator or recovery code" }));
  fireEvent.change(screen.getByLabelText("Code"), { target: { value: "123 456" } });
  fireEvent.click(screen.getByRole("button", { name: "Remove authenticator app" }));
  expect(await screen.findByText("A second factor is required")).toBeInTheDocument();
  expect(request).toHaveBeenCalledWith("DELETE", "/api/account/totp", { code: "123456" });
});

it("lists linked identities and explains refusing to unlink the last sign-in method", async () => {
  serve({}, {
    "GET /api/account/identities": () => Promise.resolve([{ provider_id: "corp", subject: "s-1" }, { provider_id: "github", subject: "42" }]),
    "DELETE /api/account/identities/corp": () => Promise.reject(new ApiError(409, "Last usable sign-in method")),
  });
  renderWith(<SignInIdentities />);
  expect(await screen.findByRole("button", { name: /^Unlink\s*GitHub$/ })).toBeInTheDocument();
  expect(screen.queryByText(/does not list identities/)).not.toBeInTheDocument();
  fireEvent.click(screen.getByRole("button", { name: /^Unlink\s*Corp SSO$/ }));
  fireEvent.click(screen.getByRole("radio", { name: "Recent sign-in" }));
  fireEvent.click(screen.getByRole("button", { name: /^Unlink\s*Corp SSO$/ }));
  expect(await screen.findByText("This sign-in method is still needed")).toBeInTheDocument();
  await waitFor(() => expect(request).toHaveBeenCalledWith("DELETE", "/api/account/identities/corp", {}));
});

it("links a directory account with local proof and labels it without exposing its subject", async () => {
  let linked = false;
  serve(
    {},
    {
      "GET /auth/ldap/providers": () => Promise.resolve([{ id: "dir", display_name: "Corp directory" }]),
      "GET /api/account/identities": () => Promise.resolve(linked ? [{ provider_id: "ldap:dir", subject: "a1b2c3d4" }] : []),
      "POST /auth/ldap/dir/sign-in": () => {
        linked = true;
        return Promise.resolve({ user: "u1", linked: true, second_factor_required: false });
      },
    },
  );
  renderWith(<SignInIdentities />);
  fireEvent.change(await screen.findByLabelText("Directory username"), { target: { value: "ada" } });
  fireEvent.change(screen.getByLabelText("Directory password"), { target: { value: "directory secret" } });
  // The OIDC linking form above has its own current password field; the directory form is last.
  fireEvent.change(screen.getAllByLabelText("Current password").at(-1)!, { target: { value: "local secret" } });
  fireEvent.click(screen.getByRole("button", { name: "Link directory account" }));
  await waitFor(() => expect(request).toHaveBeenCalledWith("POST", "/auth/ldap/dir/sign-in", { email: "ada", password: "directory secret", link: true, current_password: "local secret" }));
  expect(await screen.findByText("Directory account linked")).toBeInTheDocument();
  expect(screen.getByLabelText("Directory password")).toHaveValue("");
  expect(await screen.findByText("Directory account")).toBeInTheDocument();
  expect(screen.queryByText("a1b2c3d4")).not.toBeInTheDocument();
});

it("explains that SCIM provisioning must be detached before unlinking its identity", async () => {
  serve(
    {},
    {
      "GET /api/account/identities": () => Promise.resolve([{ provider_id: "corp", subject: "sub-1" }]),
      "DELETE /api/account/identities/corp": () => Promise.reject(new ApiError(409, "Ask an administrator to detach SCIM provisioning before unlinking its sign-in identity")),
    },
  );
  renderWith(<SignInIdentities />);
  fireEvent.click(await screen.findByRole("button", { name: /^Unlink\s*Corp SSO/ }));
  fireEvent.change(screen.getAllByLabelText("Current password")[0]!, { target: { value: "local secret" } });
  fireEvent.click(screen.getByRole("button", { name: "Unlink Corp SSO" }));
  expect(await screen.findByText(/SCIM provisioning manages this sign-in identity/)).toBeInTheDocument();
});

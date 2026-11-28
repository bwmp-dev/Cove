import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { ApiError, request } from "../api/client";
import { LoginPage } from "./login";

const router = vi.hoisted(() => {
  const search: Record<string, unknown> = {};
  return { search, navigate: vi.fn() };
});
vi.mock("@tanstack/react-router", () => ({
  getRouteApi: () => ({ useSearch: () => router.search }),
  useNavigate: () => router.navigate,
  Navigate: () => null,
  Link: ({ children }: { children: React.ReactNode }) => <a href="/login">{children}</a>,
}));
vi.mock("../api/client", async (original) => ({ ...(await original<typeof import("../api/client")>()), request: vi.fn() }));

const methods = { password_enabled: true, passkeys_enabled: true, oidc_registration_enabled: false, oidc_auto_redirect: "", oidc_providers: [{ id: "corp", display_name: "Corp SSO" }], second_factor_pending: false, second_factor_methods: [] as string[], ldap_enabled: false, ldap_providers: [] as { id: string; display_name: string }[] };
const pendingChallenge = { second_factor_pending: true, second_factor_methods: ["totp", "recovery_code", "passkey"] };
const config = { setup_required: false, registration_enabled: false, github_enabled: false, email_enabled: false };

// Overrides can be a function, so the public methods can change once the password creates a pending session.
function serve(overrides: Partial<typeof methods> | (() => Partial<typeof methods>) = {}, routes: Record<string, (body: unknown) => Promise<unknown>> = {}) {
  vi.mocked(request).mockImplementation((method, path, body) => {
    if (path === "/auth/config") return Promise.resolve(config);
    if (path === "/auth/sign-in-methods") return Promise.resolve({ ...methods, ...(typeof overrides === "function" ? overrides() : overrides) });
    const handler = routes[`${method} ${path}`];
    if (handler) return handler(body);
    return Promise.reject(new ApiError(404, "Not found"));
  });
}

function renderLogin() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(
    <QueryClientProvider client={client}>
      <LoginPage />
    </QueryClientProvider>,
  );
  return client;
}

beforeEach(() => {
  router.search = {};
});
afterEach(() => vi.clearAllMocks());

it("asks for a second factor after the password and only then enters the dashboard", async () => {
  let signedIn = false;
  serve(() => (signedIn ? pendingChallenge : {}), {
    "POST /auth/login": () => {
      signedIn = true;
      return Promise.resolve({ user: "u1", second_factor_required: true });
    },
    "POST /auth/second-factor": () => Promise.resolve({ ok: true }),
  });
  renderLogin();
  fireEvent.change(await screen.findByLabelText("Password"), { target: { value: "correct horse battery" } });
  fireEvent.click(screen.getByRole("button", { name: "Sign in" }));
  const code = await screen.findByLabelText("Authenticator or recovery code");
  expect(router.navigate).not.toHaveBeenCalled();
  fireEvent.change(code, { target: { value: "123456" } });
  fireEvent.click(screen.getByRole("button", { name: "Verify and sign in" }));
  await waitFor(() => expect(router.navigate).toHaveBeenCalledWith({ href: "/dashboard", replace: true }));
  expect(request).toHaveBeenCalledWith("POST", "/auth/second-factor", { code: "123456" });
});

it("keeps a wrong code on the challenge and restarts an expired one", async () => {
  let attempts = 0;
  serve(pendingChallenge, {
    "POST /auth/second-factor": () => Promise.reject(attempts++ === 0 ? new ApiError(401, "Invalid code") : new ApiError(401, "Invalid or expired challenge")),
  });
  renderLogin();
  const code = await screen.findByLabelText("Authenticator or recovery code");
  fireEvent.change(code, { target: { value: "000000" } });
  fireEvent.click(screen.getByRole("button", { name: "Verify and sign in" }));
  expect(await screen.findByText(/That code is not valid/)).toBeInTheDocument();
  fireEvent.change(screen.getByLabelText("Authenticator or recovery code"), { target: { value: "111111" } });
  fireEvent.click(screen.getByRole("button", { name: "Verify and sign in" }));
  expect(await screen.findByText("The verification step expired. Sign in again.")).toBeInTheDocument();
  expect(screen.getByLabelText("Password")).toBeInTheDocument();
});

it("hides the password form when password sign-in is off and offers the break-glass route", async () => {
  serve({ password_enabled: false });
  renderLogin();
  expect(await screen.findByText("Password sign-in is turned off")).toBeInTheDocument();
  expect(screen.queryByLabelText("Password")).not.toBeInTheDocument();
  expect(screen.getByRole("button", { name: "Continue with Corp SSO" })).toBeInTheDocument();
  expect(screen.getByText("Use break-glass password sign-in")).toBeInTheDocument();
});

it("sends break_glass only from the break-glass route and skips auto-redirect there", async () => {
  router.search = { break_glass: true };
  serve({ password_enabled: false, oidc_auto_redirect: "corp" }, { "POST /auth/login": () => Promise.reject(new ApiError(429, "Too many attempts; try again later", 900)) });
  renderLogin();
  expect(await screen.findByText("Break-glass password sign-in")).toBeInTheDocument();
  fireEvent.change(screen.getByLabelText("Password"), { target: { value: "admin password value" } });
  fireEvent.click(screen.getByRole("button", { name: "Sign in" }));
  expect(await screen.findByText(/paused after repeated failed attempts/)).toBeInTheDocument();
  expect(request).toHaveBeenCalledWith("POST", "/auth/login", { password: "admin password value", break_glass: true });
  expect(request).not.toHaveBeenCalledWith("POST", "/auth/oidc/corp/start", expect.anything());
});

it("starts the configured provider automatically on a plain visit", async () => {
  const assign = vi.fn();
  vi.stubGlobal("location", { ...window.location, assign });
  serve({ oidc_auto_redirect: "corp" }, { "POST /auth/oidc/corp/start": () => Promise.resolve({ url: "https://login.example.com/authorize?state=x" }) });
  renderLogin();
  expect(await screen.findByText("Redirecting to Corp SSO…")).toBeInTheDocument();
  await waitFor(() => expect(assign).toHaveBeenCalledWith("https://login.example.com/authorize?state=x"));
  expect(request).toHaveBeenCalledWith("POST", "/auth/oidc/corp/start", { link: false });
  vi.unstubAllGlobals();
});

it("explains a provider sign-in that matched an existing account without linking it", async () => {
  router.search = { error: "account_exists" };
  serve({ oidc_auto_redirect: "corp" });
  renderLogin();
  expect(await screen.findByText(/never linked automatically/)).toBeInTheDocument();
  expect(screen.getByLabelText("Password")).toBeInTheDocument();
});

it("offers only the passkey when that is the account's only available second factor", async () => {
  vi.stubGlobal("PublicKeyCredential", function PublicKeyCredential() {});
  vi.stubGlobal("navigator", { ...navigator, credentials: {} });
  serve({ second_factor_pending: true, second_factor_methods: ["passkey"] });
  renderLogin();
  expect(await screen.findByRole("button", { name: "Verify with a passkey" })).toBeInTheDocument();
  expect(screen.queryByLabelText(/code/i)).not.toBeInTheDocument();
  vi.unstubAllGlobals();
});

it("explains a provider return whose pending sign-in is gone and does not auto-redirect", async () => {
  router.search = { authenticated: true };
  serve({ oidc_auto_redirect: "corp" });
  renderLogin();
  expect(await screen.findByText(/did not finish/)).toBeInTheDocument();
  expect(screen.getByLabelText("Password")).toBeInTheDocument();
  expect(request).not.toHaveBeenCalledWith("POST", "/auth/oidc/corp/start", expect.anything());
});

const directories = { ldap_enabled: true, ldap_providers: [{ id: "corp", display_name: "Corp directory" }] };

it("signs in with a directory username and then asks for Cove's second factor", async () => {
  let signedIn = false;
  serve(() => (signedIn ? { ...directories, ...pendingChallenge } : directories), {
    "POST /auth/ldap/corp/sign-in": () => {
      signedIn = true;
      return Promise.resolve({ user: "u1", linked: false, second_factor_required: true });
    },
    "POST /auth/second-factor": () => Promise.resolve({ ok: true }),
  });
  renderLogin();
  fireEvent.click(await screen.findByRole("button", { name: "Continue with Corp directory" }));
  const username = screen.getByLabelText("Directory username");
  expect(username).not.toHaveAttribute("type", "email");
  fireEvent.change(username, { target: { value: " jsmith " } });
  fireEvent.change(screen.getByLabelText("Password"), { target: { value: "directory secret" } });
  fireEvent.click(screen.getByRole("button", { name: "Sign in" }));
  await waitFor(() => expect(request).toHaveBeenCalledWith("POST", "/auth/ldap/corp/sign-in", { email: "jsmith", password: "directory secret" }));
  const code = await screen.findByLabelText("Authenticator or recovery code");
  expect(router.navigate).not.toHaveBeenCalled();
  fireEvent.change(code, { target: { value: "123456" } });
  fireEvent.click(screen.getByRole("button", { name: "Verify and sign in" }));
  await waitFor(() => expect(router.navigate).toHaveBeenCalledWith({ href: "/dashboard", replace: true }));
});

it("enters directly after a directory sign-in without an enrolled factor", async () => {
  serve(directories, { "POST /auth/ldap/corp/sign-in": () => Promise.resolve({ user: "u1", linked: false, second_factor_required: false }) });
  renderLogin();
  fireEvent.click(await screen.findByRole("button", { name: "Continue with Corp directory" }));
  fireEvent.change(screen.getByLabelText("Directory username"), { target: { value: "jsmith" } });
  fireEvent.change(screen.getByLabelText("Password"), { target: { value: "directory secret" } });
  fireEvent.click(screen.getByRole("button", { name: "Sign in" }));
  await waitFor(() => expect(router.navigate).toHaveBeenCalledWith({ href: "/dashboard", replace: true }));
});

it("explains directory refusals and clears the typed password", async () => {
  const errors = [
    new ApiError(401, "Invalid directory credentials"),
    new ApiError(409, "Account exists; sign in and explicitly link this directory identity"),
    new ApiError(429, "Too many attempts", 30),
  ];
  serve(directories, { "POST /auth/ldap/corp/sign-in": () => Promise.reject(errors.shift() ?? new Error("unexpected")) });
  renderLogin();
  fireEvent.click(await screen.findByRole("button", { name: "Continue with Corp directory" }));
  fireEvent.change(screen.getByLabelText("Directory username"), { target: { value: "jsmith" } });
  for (const message of ["Incorrect directory username or password.", "Account exists; sign in and explicitly link this directory identity", "Too many attempts. Try again in 30 seconds."]) {
    fireEvent.change(screen.getByLabelText("Password"), { target: { value: "wrong" } });
    fireEvent.click(screen.getByRole("button", { name: "Sign in" }));
    expect(await screen.findByText(message)).toBeInTheDocument();
    expect(screen.getByLabelText("Password")).toHaveValue("");
  }
});

it("keeps a refused directory password out of the mutation cache", async () => {
  serve(directories, { "POST /auth/ldap/corp/sign-in": () => Promise.reject(new ApiError(401, "Invalid directory credentials")) });
  const client = renderLogin();
  fireEvent.click(await screen.findByRole("button", { name: "Continue with Corp directory" }));
  fireEvent.change(screen.getByLabelText("Directory username"), { target: { value: "jsmith" } });
  fireEvent.change(screen.getByLabelText("Password"), { target: { value: "directory secret" } });
  fireEvent.click(screen.getByRole("button", { name: "Sign in" }));
  expect(await screen.findByText("Incorrect directory username or password.")).toBeInTheDocument();
  expect(request).toHaveBeenCalledWith("POST", "/auth/ldap/corp/sign-in", { email: "jsmith", password: "directory secret" });
  const mutations = client.getMutationCache().getAll();
  expect(mutations.length).toBeGreaterThan(0);
  for (const mutation of mutations) expect(mutation.state.variables).toBeUndefined();
});

it("does not offer directory sign-in on the break-glass page", async () => {
  router.search = { break_glass: true };
  serve({ ...directories, password_enabled: false });
  renderLogin();
  expect(await screen.findByText("Break-glass password sign-in")).toBeInTheDocument();
  expect(screen.queryByRole("button", { name: "Continue with Corp directory" })).not.toBeInTheDocument();
});

it("keeps directory sign-in when local passwords are turned off", async () => {
  serve({ ...directories, password_enabled: false });
  renderLogin();
  expect(await screen.findByText(/Local Cove passwords are turned off. Sign in with single sign-on, your organization's directory or a passkey./)).toBeInTheDocument();
  expect(screen.queryByLabelText("Email")).not.toBeInTheDocument();
  expect(screen.getByRole("button", { name: "Continue with Corp directory" })).toBeInTheDocument();
});

it("shows a malformed directory entry at the fields, apart from wrong credentials", async () => {
  serve(directories, { "POST /auth/ldap/corp/sign-in": () => Promise.reject(new ApiError(400, "Invalid directory sign-in request")) });
  renderLogin();
  fireEvent.click(await screen.findByRole("button", { name: "Continue with Corp directory" }));
  fireEvent.change(screen.getByLabelText("Directory username"), { target: { value: "jsmith" } });
  fireEvent.change(screen.getByLabelText("Password"), { target: { value: "x" } });
  fireEvent.click(screen.getByRole("button", { name: "Sign in" }));
  expect(await screen.findByText(/without line breaks or control characters/)).toBeInTheDocument();
  expect(screen.getByLabelText("Directory username")).toHaveAttribute("aria-invalid", "true");
  expect(screen.queryByText("Incorrect directory username or password.")).not.toBeInTheDocument();
});

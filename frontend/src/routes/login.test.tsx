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

const methods = { password_enabled: true, passkeys_enabled: true, oidc_registration_enabled: false, oidc_auto_redirect: "", oidc_providers: [{ id: "corp", display_name: "Corp SSO" }], second_factor_pending: false, second_factor_methods: [] as string[] };
const pendingChallenge = { second_factor_pending: true, second_factor_methods: ["totp", "recovery_code", "passkey"] };
const config = { setup_required: false, registration_enabled: false, github_enabled: false, email_enabled: false };

// Overrides can be a function, so the public methods can change once the password creates a pending session.
function serve(overrides: Partial<typeof methods> | (() => Partial<typeof methods>) = {}, routes: Record<string, (body: unknown) => Promise<unknown>> = {}) {
  vi.mocked(request).mockImplementation((method, path, body) => {
    if (path === "/auth/config") return Promise.resolve(config);
    if (path === "/auth/sign-in-methods") return Promise.resolve({ ...methods, ...(typeof overrides === "function" ? overrides() : overrides) });
    const handler = routes[`${method} ${path}`];
    return handler ? handler(body) : Promise.reject(new ApiError(404, "Not found"));
  });
}

function renderLogin() {
  render(
    <QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}>
      <LoginPage />
    </QueryClientProvider>,
  );
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

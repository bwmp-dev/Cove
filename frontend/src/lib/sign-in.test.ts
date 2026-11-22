import { describe, expect, it } from "vitest";
import { ApiError } from "../api/client";
import { autoRedirectProvider, challengeError, challengeOptions, initialLoginState, loginErrorMessage, loginReducer, loginView, passwordMode, type SignInMethods } from "./sign-in";

function methods(overrides: Partial<SignInMethods> = {}): SignInMethods {
  return {
    password_enabled: true,
    passkeys_enabled: true,
    oidc_registration_enabled: false,
    oidc_auto_redirect: "",
    oidc_providers: [{ id: "corp", display_name: "Corp SSO" }],
    second_factor_pending: false,
    second_factor_methods: [],
    ...overrides,
  };
}

const context = { breakGlass: false, authenticated: false };

describe("login state", () => {
  it("opens the challenge when a provider sign-in returns with a pending second factor", () => {
    expect(loginView(initialLoginState, methods({ second_factor_pending: true }), true)).toEqual({ step: "second-factor" });
    expect(loginView(initialLoginState, methods(), false)).toEqual({ step: "credentials" });
    expect(loginView(initialLoginState, undefined, false)).toEqual({ step: "credentials" });
  });

  it("explains a provider return whose pending sign-in is gone instead of silently showing the form", () => {
    expect(loginView(initialLoginState, methods(), true).notice).toMatch(/did not finish/);
    expect(loginView(initialLoginState, undefined, true)).toEqual({ step: "credentials" });
  });

  it("treats a challenge the server no longer reports as expired, but keeps it when the methods are unknown", () => {
    const view = loginView({ step: "second-factor" }, methods(), false);
    expect(view.step).toBe("credentials");
    expect(view.notice).toMatch(/expired/);
    expect(loginView({ step: "second-factor" }, undefined, false)).toEqual({ step: "second-factor" });
  });

  it("moves to the challenge only when the password needs a second factor", () => {
    expect(loginReducer(initialLoginState, { type: "password-accepted", secondFactorRequired: true })).toEqual({ step: "second-factor" });
    expect(loginReducer(initialLoginState, { type: "password-accepted", secondFactorRequired: false })).toBe(initialLoginState);
  });

  it("returns to credentials after an expired challenge or a restart, even while the cookie still looks pending", () => {
    const expired = loginReducer({ step: "second-factor" }, { type: "challenge-expired" });
    expect(expired.step).toBe("credentials");
    expect(expired.notice).toMatch(/expired/);
    expect(loginView(loginReducer({ step: "second-factor" }, { type: "restart" }), methods({ second_factor_pending: true }), false).step).toBe("credentials");
  });
});

describe("challenge options", () => {
  it("offers only the factors the pending challenge reports", () => {
    expect(challengeOptions(["totp", "recovery_code", "passkey"]).code?.label).toBe("Authenticator or recovery code");
    expect(challengeOptions(["totp", "recovery_code", "passkey"]).passkey).toBe(true);
    expect(challengeOptions(["totp"]).code?.label).toBe("Authenticator code");
    expect(challengeOptions(["totp"]).passkey).toBe(false);
    expect(challengeOptions(["recovery_code", "passkey"]).code?.label).toBe("Recovery code");
    expect(challengeOptions(["recovery_code", "passkey"]).passkey).toBe(true);
    expect(challengeOptions(["passkey"])).toEqual({ code: null, passkey: true });
  });

  it("offers every factor when the list could not be loaded", () => {
    expect(challengeOptions(undefined).code?.label).toBe("Authenticator or recovery code");
    expect(challengeOptions(undefined).passkey).toBe(true);
  });
});

describe("password availability", () => {
  it("hides the password form when disabled unless the break-glass route is used", () => {
    expect(passwordMode(methods(), false)).toBe("password");
    expect(passwordMode(methods({ password_enabled: false }), false)).toBe("disabled");
    expect(passwordMode(methods({ password_enabled: false }), true)).toBe("break-glass");
    expect(passwordMode(undefined, false)).toBe("password");
  });
});

describe("auto-redirect", () => {
  const configured = methods({ oidc_auto_redirect: "corp" });

  it("redirects to the configured provider on a plain visit", () => {
    expect(autoRedirectProvider(configured, "credentials", context)?.id).toBe("corp");
  });

  it("never redirects after a failure, on break-glass, after returning, or with a pending challenge", () => {
    expect(autoRedirectProvider(configured, "credentials", { ...context, error: "oidc_failed" })).toBeUndefined();
    expect(autoRedirectProvider(configured, "credentials", { ...context, breakGlass: true })).toBeUndefined();
    expect(autoRedirectProvider(configured, "credentials", { ...context, authenticated: true })).toBeUndefined();
    expect(autoRedirectProvider({ ...configured, second_factor_pending: true }, "credentials", context)).toBeUndefined();
    expect(autoRedirectProvider(configured, "second-factor", context)).toBeUndefined();
  });

  it("ignores a redirect to a provider that no longer exists", () => {
    expect(autoRedirectProvider(methods({ oidc_auto_redirect: "gone" }), "credentials", context)).toBeUndefined();
  });
});

describe("messages", () => {
  it("separates the temporary lockout from the per-minute limit without revealing accounts", () => {
    expect(loginErrorMessage(new ApiError(429, "Too many attempts; try again later", 900))).toMatch(/15 minutes/);
    expect(loginErrorMessage(new ApiError(429, "Too many attempts", 60))).toBe("Too many attempts. Try again in 60 seconds.");
    expect(loginErrorMessage(new ApiError(401, "Invalid credentials"))).toBe("Incorrect email or password.");
  });

  it("explains disabled password sign-in apart from origin rejection", () => {
    expect(loginErrorMessage(new ApiError(403, "Password login disabled"))).toMatch(/turned off/);
    expect(loginErrorMessage(new ApiError(403, "Origin rejected"))).toMatch(/COVE_PUBLIC_URL/);
  });

  it("restarts sign-in only for an expired challenge, not a wrong code", () => {
    expect(challengeError(new ApiError(401, "Invalid code")).restart).toBe(false);
    expect(challengeError(new ApiError(401, "Invalid or expired challenge")).restart).toBe(true);
    expect(challengeError(new ApiError(401, "Unauthorized")).restart).toBe(true);
    expect(challengeError(new ApiError(503, "Authentication unavailable"))).toEqual({ message: "Authentication unavailable", restart: false });
  });
});

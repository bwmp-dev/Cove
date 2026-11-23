import { ApiError, errorMessage } from "../api/client";

export interface SignInProvider {
  id: string;
  display_name: string;
}

export type SecondFactorMethod = "totp" | "recovery_code" | "passkey";

export interface SignInMethods {
  password_enabled: boolean;
  passkeys_enabled: boolean;
  oidc_registration_enabled: boolean;
  oidc_auto_redirect: string;
  oidc_providers: SignInProvider[];
  second_factor_pending: boolean;
  // Only filled while this browser holds a pending challenge younger than ten minutes.
  second_factor_methods: SecondFactorMethod[];
}

export type LoginStep = "initial" | "credentials" | "second-factor";

export interface LoginState {
  step: LoginStep;
  notice?: string;
}

export type LoginEvent =
  | { type: "password-accepted"; secondFactorRequired: boolean }
  | { type: "challenge-expired" }
  | { type: "restart" };

export const initialLoginState: LoginState = { step: "initial" };

export function loginReducer(state: LoginState, event: LoginEvent): LoginState {
  switch (event.type) {
    case "password-accepted":
      return event.secondFactorRequired ? { step: "second-factor" } : state;
    case "challenge-expired":
      return { step: "credentials", notice: expiredChallengeNotice };
    case "restart":
      return { step: "credentials" };
  }
}

export const expiredChallengeNotice = "The verification step expired. Sign in again.";

export const unfinishedProviderNotice = "Your sign-in did not finish, for example because the verification step expired. Sign in again.";

export interface LoginView {
  step: Exclude<LoginStep, "initial">;
  notice?: string;
}

// GitHub and OIDC sign-ins return to /login?authenticated=1. A completed sign-in never renders this page (the
// route sends it on), so a return either still needs its second factor, which only the server knows, or lost its
// pending session. Once the page has asked for a second factor, a server that no longer reports the challenge
// means it expired. Unknown methods (the request failed) keep the current step and let the server decide.
export function loginView(state: LoginState, methods: SignInMethods | undefined, authenticated: boolean): LoginView {
  const pending = methods?.second_factor_pending;
  if (state.step === "initial") {
    if (pending) return { step: "second-factor" };
    if (authenticated && methods) return { step: "credentials", notice: unfinishedProviderNotice };
    return { step: "credentials" };
  }
  if (state.step === "second-factor" && pending === false) return { step: "credentials", notice: expiredChallengeNotice };
  return { step: state.step, notice: state.notice };
}

export interface ChallengeOptions {
  code: { label: string; hint: string } | null;
  passkey: boolean;
}

const allSecondFactors: SecondFactorMethod[] = ["totp", "recovery_code", "passkey"];

// Without the server's list (the request failed) every factor is offered and the server still checks the answer.
export function challengeOptions(methods: readonly SecondFactorMethod[] | undefined): ChallengeOptions {
  const available = methods ?? allSecondFactors;
  const totp = available.includes("totp");
  const recovery = available.includes("recovery_code");
  let code: ChallengeOptions["code"] = null;
  if (totp && recovery) code = { label: "Authenticator or recovery code", hint: "The 6-digit code from your authenticator app, or one unused recovery code." };
  else if (totp) code = { label: "Authenticator code", hint: "The 6-digit code from your authenticator app." };
  else if (recovery) code = { label: "Recovery code", hint: "One unused recovery code." };
  return { code, passkey: available.includes("passkey") };
}

export type PasswordMode = "password" | "break-glass" | "disabled";

export function passwordMode(methods: SignInMethods | undefined, breakGlass: boolean): PasswordMode {
  if (!methods || methods.password_enabled) return "password";
  return breakGlass ? "break-glass" : "disabled";
}

export interface LoginContext {
  error?: string;
  breakGlass: boolean;
  authenticated: boolean;
}

// Auto-redirect is a page preference only. It never runs after a failed provider sign-in, on the break-glass
// page, or while a returning sign-in still has to finish, so people are never stuck in a redirect loop.
export function autoRedirectProvider(methods: SignInMethods | undefined, step: LoginStep, context: LoginContext): SignInProvider | undefined {
  if (!methods || step !== "credentials" || context.error || context.breakGlass || context.authenticated || methods.second_factor_pending) return undefined;
  if (methods.oidc_auto_redirect === "") return undefined;
  return methods.oidc_providers.find((provider) => provider.id === methods.oidc_auto_redirect);
}

export const signInErrors: Record<string, string> = {
  oauth_failed: "GitHub sign-in did not complete. Try again.",
  oidc_failed: "Single sign-on did not complete. Try again, or ask an instance administrator to check the provider configuration.",
  verified_email_required: "The identity provider did not confirm a verified email address, so Cove cannot sign you in with it.",
  account_exists:
    "An account already uses this email address. Sign in to that account another way, then link this sign-in method from your account settings. Accounts are never linked automatically by email address.",
  registration_closed: "This instance does not create new accounts from this sign-in method. Ask a team administrator for an invitation.",
};

function throttleMessage(error: ApiError) {
  if ((error.retryAfter ?? 0) >= 900 || error.message.toLowerCase().includes("later")) {
    return "Sign-in is paused after repeated failed attempts. Try again in 15 minutes.";
  }
  return `Too many attempts. Try again in ${error.retryAfter ?? 60} seconds.`;
}

export function loginErrorMessage(error: unknown) {
  if (error instanceof ApiError) {
    if (error.status === 401) return "Incorrect email or password.";
    if (error.status === 429) return throttleMessage(error);
    if (error.status === 403 && error.message.toLowerCase().includes("password login disabled")) {
      return "Password sign-in is turned off for this instance. Use single sign-on or a passkey.";
    }
    if (error.status === 403) return "Sign-in was rejected because this page is not served from the configured public URL (COVE_PUBLIC_URL).";
  }
  return errorMessage(error);
}

export function challengeError(error: unknown): { message: string; restart: boolean } {
  if (error instanceof ApiError) {
    if (error.status === 429) return { message: throttleMessage(error), restart: false };
    if (error.status === 401 && error.message.toLowerCase().includes("invalid code")) {
      return { message: "That code is not valid or was already used. Try again.", restart: false };
    }
    if (error.status === 401) return { message: expiredChallengeNotice, restart: true };
  }
  return { message: errorMessage(error), restart: false };
}

export function passkeyErrorMessage(error: unknown) {
  if (error instanceof ApiError) {
    if (error.status === 429) return throttleMessage(error);
    if (error.status === 401) return "The passkey could not be verified. Try again.";
  }
  if (error instanceof Error && error.name === "PasskeyFailed") return error.message;
  return errorMessage(error);
}

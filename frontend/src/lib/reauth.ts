import { ApiError, errorMessage } from "../api/client";

export type ReauthMethod = "password" | "code" | "recent";

export type ReauthProof = { current_password: string } | { code: string } | Record<string, never>;

export interface ReauthFactors {
  totp_enrolled: boolean;
  recovery_codes_remaining: number;
}

// A recent passkey or second-factor sign-in is always offered: it is the only proof a passwordless,
// passkey-only account can give, and the server decides whether the session is still fresh enough.
export function reauthMethods(hasPassword: boolean, factors: ReauthFactors): ReauthMethod[] {
  const methods: ReauthMethod[] = [];
  if (hasPassword) methods.push("password");
  if (factors.totp_enrolled || factors.recovery_codes_remaining > 0) methods.push("code");
  methods.push("recent");
  return methods;
}

export const reauthLabels: Record<ReauthMethod, string> = {
  password: "Password",
  code: "Authenticator or recovery code",
  recent: "Recent sign-in",
};

export function reauthProof(method: ReauthMethod, value: string): { proof: ReauthProof } | { error: string } {
  if (method === "recent") return { proof: {} };
  if (method === "password") return value === "" ? { error: "Enter your current password." } : { proof: { current_password: value } };
  const code = value.trim();
  if (code === "") return { error: "Enter a code from your authenticator app or an unused recovery code." };
  // Authenticator codes are often typed with a space in the middle; recovery codes are used exactly as issued.
  return { proof: { code: /^\d{3}\s?\d{3}$/.test(code) ? code.replace(/\s/g, "") : code } };
}

export function reauthErrorMessage(error: unknown, method: ReauthMethod) {
  if (error instanceof ApiError) {
    if (error.status === 429) return `Too many attempts. Try again in ${error.retryAfter ?? 60} seconds.`;
    if (error.status === 403 && error.message.toLowerCase().includes("reauthentication required")) {
      if (method === "password") return "The password is incorrect.";
      if (method === "code") return "That code is not valid or was already used. Enter the current authenticator code or an unused recovery code.";
      return "Your last passkey or two-step sign-in in this browser was more than ten minutes ago. Use another method, or sign out, sign in again and retry.";
    }
  }
  return errorMessage(error);
}

import { useQuery } from "@tanstack/react-query";
import { getRouteApi, Link, Navigate, useNavigate } from "@tanstack/react-router";
import { useState, type FormEvent } from "react";
import { useGitHubStart } from "../api/auth";
import { ApiError, errorMessage } from "../api/client";
import { useLogin } from "../api/mutations";
import { authConfigQuery } from "../api/queries";
import type { AuthConfig } from "../api/types";
import { AuthFrame } from "../components/AuthFrame";
import { Button, Field, Notice } from "../components/ui";
import { safeRedirect } from "../lib/redirect";
import { inputClass } from "../lib/styles";

const route = getRouteApi("/login");

const oauthErrors: Record<string, string> = {
  oauth_failed: "GitHub sign-in did not complete. Try again.",
  account_exists: "An account already uses this email address. Sign in with its password, then link GitHub from your account settings.",
  registration_closed: "Registration is closed. Ask a team administrator for an invitation.",
};

function loginError(error: unknown) {
  if (error instanceof ApiError) {
    if (error.status === 401) return "Incorrect email or password.";
    if (error.status === 429) return `Too many attempts. Try again in ${error.retryAfter ?? 60} seconds.`;
    if (error.status === 403) return "Sign-in was rejected because this page is not served from the configured public URL (COVE_PUBLIC_URL).";
  }
  return errorMessage(error);
}

export function GitHubButton({ config, invitationToken, label = "Continue with GitHub" }: { config: AuthConfig | undefined; invitationToken?: string; label?: string }) {
  const start = useGitHubStart();
  if (!config) return null;
  if (!config.github_enabled) {
    return <p className="text-xs text-fg-subtle">GitHub sign-in is not configured on this instance.</p>;
  }
  return (
    <div className="space-y-2">
      <Button className="w-full" pending={start.isPending || start.isSuccess} onClick={() => start.mutate(invitationToken ? { invitation_token: invitationToken } : {})}>
        {label}
      </Button>
      {start.isError && (
        <p role="alert" className="text-[13px] text-bad">
          {errorMessage(start.error)}
        </p>
      )}
    </div>
  );
}

export function LoginPage() {
  const search = route.useSearch();
  const navigate = useNavigate();
  const login = useLogin();
  const config = useQuery(authConfigQuery);
  const [email, setEmail] = useState("");
  const [password, setPassword] = useState("");

  function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    if (password === "") return;
    const trimmed = email.trim();
    login.mutate(trimmed ? { email: trimmed, password } : { password }, {
      onSuccess: () => {
        setPassword("");
        void navigate({ href: safeRedirect(search.redirect), replace: true });
      },
      onError: () => setPassword(""),
    });
  }

  if (config.data?.setup_required) return <Navigate to="/register" replace />;

  const error = login.isError ? loginError(login.error) : null;
  const oauthError = search.error ? (oauthErrors[search.error] ?? "Sign-in did not complete. Try again.") : null;

  return (
    <AuthFrame title="Sign in to Cove">
      <div className="space-y-5">
        {oauthError && <Notice tone="error">{oauthError}</Notice>}
        <form onSubmit={submit} className="space-y-4" noValidate>
          <Field id="email" label="Email" hint="Use your registered email. Older installations may use a password-only administrator.">
            <input
              id="email"
              type="email"
              autoComplete="username"
              value={email}
              onChange={(event) => setEmail(event.target.value)}
              aria-describedby="email-hint"
              className={inputClass}
            />
          </Field>
          <div className="space-y-1.5">
            <div className="flex items-baseline justify-between gap-2">
              <label htmlFor="password" className="block text-[13px] font-medium">
                Password
              </label>
              {config.data?.email_enabled && (
                <Link to="/forgot-password" className="rounded text-xs text-fg-muted hover:text-fg">
                  Forgot password?
                </Link>
              )}
            </div>
            <input
              id="password"
              type="password"
              autoComplete="current-password"
              required
              maxLength={72}
              value={password}
              onChange={(event) => setPassword(event.target.value)}
              aria-invalid={error ? true : undefined}
              aria-describedby={error ? "login-error" : undefined}
              className={inputClass}
            />
          </div>
          {error && (
            <p id="login-error" role="alert" className="text-[13px] text-bad">
              {error}
            </p>
          )}
          <Button type="submit" variant="primary" className="w-full" pending={login.isPending}>
            Sign in
          </Button>
        </form>
        <div className="space-y-3 border-t border-line pt-4">
          <GitHubButton config={config.data} />
          {config.isError && <p className="text-xs text-fg-subtle">Sign-in options could not be loaded. Email and password sign-in is still available.</p>}
          {config.data &&
            (config.data.registration_enabled ? (
              <p className="text-[13px] text-fg-muted">
                New to this instance?{" "}
                <Link to="/register" className="rounded text-fg underline underline-offset-4">
                  Create an account
                </Link>
              </p>
            ) : (
              <p className="text-xs text-fg-subtle">Registration is closed. Ask a team administrator for an invitation.</p>
            ))}
        </div>
      </div>
    </AuthFrame>
  );
}

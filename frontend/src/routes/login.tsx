import { useQuery } from "@tanstack/react-query";
import { getRouteApi, Link, Navigate, useNavigate } from "@tanstack/react-router";
import { KeyRound } from "lucide-react";
import { useEffect, useReducer, useRef, useState, type FormEvent } from "react";
import { useGitHubStart } from "../api/auth";
import { errorMessage } from "../api/client";
import { useLogin } from "../api/mutations";
import { authConfigQuery } from "../api/queries";
import { signInMethodsQuery, useOidcStart, usePasskeySignIn, useSecondFactorChallenge } from "../api/security";
import type { AuthConfig } from "../api/types";
import { AuthFrame } from "../components/AuthFrame";
import { Button, Field, Notice } from "../components/ui";
import { withBasePath } from "../lib/base-path";
import { safeRedirect } from "../lib/redirect";
import {
  autoRedirectProvider,
  challengeError,
  challengeOptions,
  initialLoginState,
  loginErrorMessage,
  loginReducer,
  loginView,
  passkeyErrorMessage,
  passwordMode,
  signInErrors,
  type ChallengeOptions,
  type SignInProvider,
} from "../lib/sign-in";
import { describedBy, inputClass, linkClass } from "../lib/styles";
import { PasskeyCancelled, passkeysSupported } from "../lib/webauthn";

const route = getRouteApi("/login");

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

function PasskeySignIn({ onSignedIn, label = "Sign in with a passkey" }: { onSignedIn: () => void; label?: string }) {
  const passkey = usePasskeySignIn();
  if (!passkeysSupported()) return <p className="text-xs text-fg-subtle">This browser does not support passkeys.</p>;
  const cancelled = passkey.error instanceof PasskeyCancelled;
  return (
    <div className="space-y-2">
      <Button className="w-full" pending={passkey.isPending} onClick={() => passkey.mutate(undefined, { onSuccess: onSignedIn })}>
        <KeyRound aria-hidden className="size-3.5" />
        {label}
      </Button>
      {passkey.isError &&
        (cancelled ? (
          <p role="status" className="text-xs text-fg-muted">
            The passkey prompt was closed. Try again when you are ready.
          </p>
        ) : (
          <p role="alert" className="text-[13px] text-bad">
            {passkeyErrorMessage(passkey.error)}
          </p>
        ))}
    </div>
  );
}

function ProviderButtons({ providers }: { providers: SignInProvider[] }) {
  const start = useOidcStart();
  if (providers.length === 0) return null;
  return (
    <div className="space-y-2">
      {providers.map((provider) => (
        <Button
          key={provider.id}
          className="w-full"
          disabled={start.isPending || start.isSuccess}
          pending={(start.isPending || start.isSuccess) && start.variables.provider === provider.id}
          onClick={() => start.mutate({ provider: provider.id, link: false })}
        >
          Continue with {provider.display_name}
        </Button>
      ))}
      {start.isError && (
        <p role="alert" className="text-[13px] text-bad">
          {errorMessage(start.error)}
        </p>
      )}
    </div>
  );
}

function AutoRedirect({ provider, onSkip }: { provider: SignInProvider; onSkip: () => void }) {
  const start = useOidcStart();
  const { mutate } = start;
  const started = useRef(false);
  // Leaving for the provider is a side effect of arriving on this page, so it starts once, after the first render.
  useEffect(() => {
    if (started.current) return;
    started.current = true;
    mutate({ provider: provider.id, link: false });
  }, [provider.id, mutate]);
  return (
    <AuthFrame title="Sign in to Cove">
      <div className="space-y-4">
        {start.isError ? (
          <Notice tone="error" title={`Could not reach ${provider.display_name}`}>
            {errorMessage(start.error)}
          </Notice>
        ) : (
          <p role="status" className="text-[13px] text-fg-muted">
            Redirecting to {provider.display_name}…
          </p>
        )}
        <div className="flex flex-wrap gap-2">
          {start.isError && (
            <Button variant="primary" onClick={() => start.mutate({ provider: provider.id, link: false })}>
              Try again
            </Button>
          )}
          <Button variant="ghost" onClick={onSkip}>
            Use another sign-in method
          </Button>
        </div>
      </div>
    </AuthFrame>
  );
}

function SecondFactorChallenge({ options, onSignedIn, onRestart, onExpired }: { options: ChallengeOptions; onSignedIn: () => void; onRestart: () => void; onExpired: () => void }) {
  const challenge = useSecondFactorChallenge();
  const [code, setCode] = useState("");
  const [empty, setEmpty] = useState(false);
  const problem = challenge.isError ? challengeError(challenge.error) : null;

  function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    const value = code.trim();
    setEmpty(value === "");
    if (value === "") return;
    challenge.mutate(value, {
      onSuccess: onSignedIn,
      onError: (error) => {
        setCode("");
        if (challengeError(error).restart) onExpired();
      },
    });
  }

  const error = empty ? "Enter a code." : problem?.message;
  return (
    <AuthFrame title="Verify it's you" description="Your account uses two-step verification. Finish signing in with a second factor.">
      <div className="space-y-5">
        {options.code && (
        <form onSubmit={submit} className="space-y-4" noValidate>
          <Field id="second-factor-code" label={options.code.label} hint={options.code.hint} error={error}>
            <input
              id="second-factor-code"
              className={`${inputClass} font-mono`}
              autoComplete="one-time-code"
              autoCapitalize="none"
              spellCheck={false}
              maxLength={64}
              value={code}
              onChange={(event) => setCode(event.target.value)}
              aria-invalid={error ? true : undefined}
              aria-describedby={describedBy("second-factor-code", error, true)}
            />
          </Field>
          <Button type="submit" variant="primary" className="w-full" pending={challenge.isPending}>
            Verify and sign in
          </Button>
        </form>
        )}
        <div className={options.code ? "space-y-3 border-t border-line pt-4" : "space-y-3"}>
          {options.passkey && <PasskeySignIn onSignedIn={onSignedIn} label={options.code ? "Use a passkey instead" : "Verify with a passkey"} />}
          <Button variant="ghost" className="w-full" onClick={onRestart} disabled={challenge.isPending}>
            Start over
          </Button>
        </div>
      </div>
    </AuthFrame>
  );
}

function PasswordForm({ breakGlass, emailEnabled, onAccepted }: { breakGlass: boolean; emailEnabled: boolean; onAccepted: (secondFactorRequired: boolean) => Promise<void> | void }) {
  const login = useLogin();
  const [email, setEmail] = useState("");
  const [password, setPassword] = useState("");
  // Stays busy while the page loads the second-factor options for the new pending session.
  const [continuing, setContinuing] = useState(false);

  function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    if (password === "") return;
    const trimmed = email.trim();
    login.mutate(
      { ...(trimmed ? { email: trimmed } : {}), password, ...(breakGlass ? { break_glass: true } : {}) },
      {
        onSuccess: (result) => {
          setPassword("");
          setContinuing(true);
          void Promise.resolve(onAccepted(result.second_factor_required)).finally(() => setContinuing(false));
        },
        onError: () => setPassword(""),
      },
    );
  }

  const error = login.isError ? loginErrorMessage(login.error) : null;
  return (
    <form onSubmit={submit} className="space-y-4" noValidate>
      <Field id="email" label="Email" hint={breakGlass ? undefined : "Use your registered email. Older installations may use a password-only administrator."}>
        <input
          id="email"
          type="email"
          autoComplete="username"
          value={email}
          onChange={(event) => setEmail(event.target.value)}
          aria-describedby={breakGlass ? undefined : "email-hint"}
          className={inputClass}
        />
      </Field>
      <div className="space-y-1.5">
        <div className="flex items-baseline justify-between gap-2">
          <label htmlFor="password" className="block text-[13px] font-medium">
            Password
          </label>
          {emailEnabled && !breakGlass && (
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
      <Button type="submit" variant="primary" className="w-full" pending={login.isPending || continuing}>
        Sign in
      </Button>
    </form>
  );
}

export function LoginPage() {
  const search = route.useSearch();
  const navigate = useNavigate();
  const config = useQuery(authConfigQuery);
  const methods = useQuery(signInMethodsQuery);
  const [state, dispatch] = useReducer(loginReducer, initialLoginState);
  const [skipRedirect, setSkipRedirect] = useState(false);
  const breakGlass = search.break_glass === true;

  const finish = () => void navigate({ href: withBasePath(safeRedirect(search.redirect)), replace: true });

  if (config.data?.setup_required) return <Navigate to="/register" replace />;

  if (methods.isPending) {
    return (
      <AuthFrame title="Sign in to Cove">
        <p role="status" className="text-[13px] text-fg-muted">
          Loading sign-in options
        </p>
      </AuthFrame>
    );
  }

  // A failed refetch keeps the previous answer, which describes the session before the password was accepted.
  const known = methods.isError ? undefined : methods.data;
  const view = loginView(state, known, search.authenticated === true);
  if (view.step === "second-factor") {
    return (
      <SecondFactorChallenge
        options={challengeOptions(known?.second_factor_methods)}
        onSignedIn={finish}
        onRestart={() => dispatch({ type: "restart" })}
        onExpired={() => dispatch({ type: "challenge-expired" })}
      />
    );
  }

  // The pending session the password created decides which second factors the challenge offers.
  async function passwordAccepted(secondFactorRequired: boolean) {
    if (!secondFactorRequired) return finish();
    await methods.refetch();
    dispatch({ type: "password-accepted", secondFactorRequired });
  }

  const redirectTo = autoRedirectProvider(methods.data, view.step, { error: search.error, breakGlass: breakGlass || skipRedirect, authenticated: search.authenticated === true });
  if (redirectTo) return <AutoRedirect provider={redirectTo} onSkip={() => setSkipRedirect(true)} />;

  const mode = passwordMode(methods.data, breakGlass);

  const providerError = search.error ? (signInErrors[search.error] ?? "Sign-in did not complete. Try again.") : null;
  const providers = methods.data?.oidc_providers ?? [];

  return (
    <AuthFrame title={mode === "break-glass" ? "Administrator sign-in" : "Sign in to Cove"}>
      <div className="space-y-5">
        {view.notice && <Notice tone="warn">{view.notice}</Notice>}
        {providerError && <Notice tone="error">{providerError}</Notice>}
        {mode === "break-glass" && (
          <Notice tone="warn" title="Break-glass password sign-in">
            Password sign-in is turned off for this instance. Only instance administrators can sign in here, for example when single sign-on is unavailable.
            Enrolled second factors still apply and the attempt is recorded.
          </Notice>
        )}
        {mode === "disabled" ? (
          <Notice title="Password sign-in is turned off">Sign in with single sign-on or a passkey.</Notice>
        ) : (
          <PasswordForm breakGlass={mode === "break-glass"} emailEnabled={config.data?.email_enabled === true} onAccepted={passwordAccepted} />
        )}
        {mode !== "break-glass" && (
          <div className="space-y-3 border-t border-line pt-4">
            <ProviderButtons providers={providers} />
            {methods.data?.passkeys_enabled !== false && <PasskeySignIn onSignedIn={finish} />}
            <GitHubButton config={config.data} />
            {(config.isError || methods.isError) && <p className="text-xs text-fg-subtle">Some sign-in options could not be loaded. Email and password sign-in is still available.</p>}
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
        )}
        {mode === "disabled" && (
          <p className="text-xs text-fg-muted">
            Instance administrator and single sign-on is unavailable?{" "}
            <Link to="/login" search={{ break_glass: true }} className={linkClass}>
              Use break-glass password sign-in
            </Link>
          </p>
        )}
        {mode === "break-glass" && (
          <Link to="/login" className={linkClass + " text-[13px]"}>
            Back to standard sign-in
          </Link>
        )}
      </div>
    </AuthFrame>
  );
}

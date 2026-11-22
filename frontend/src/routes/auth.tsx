import { useQuery } from "@tanstack/react-query";
import { Link, useNavigate } from "@tanstack/react-router";
import { useId, useState, type FormEvent } from "react";
import { useAcceptInvitation, useForgotPassword, useInvitationDetails, useRegister, useResetPassword, useVerifyEmail, type InvitationDetails } from "../api/auth";
import type { AuthConfig } from "../api/types";
import { errorMessage } from "../api/client";
import { authConfigQuery, sessionQuery } from "../api/queries";
import { AuthFrame } from "../components/AuthFrame";
import { Button, Field, Notice } from "../components/ui";
import { forgetFragmentToken, fragmentToken } from "../lib/fragment";
import { passwordError } from "../lib/password";
import { grantRoleLabels, teamRoleLabels } from "../lib/permissions";
import { buttonClass, describedBy, inputClass } from "../lib/styles";
import { GitHubButton } from "./login";

function SignInLink({ children = "Back to sign in" }: { children?: string }) {
  return (
    <Link to="/login" className="rounded text-[13px] text-fg underline underline-offset-4">
      {children}
    </Link>
  );
}

function MissingToken({ title, what }: { title: string; what: string }) {
  return (
    <AuthFrame title={title}>
      <div className="space-y-4">
        <Notice tone="warn" title="This link is incomplete">
          Open the full {what} link again. For your security the code is removed from the address bar once read, so reloading this page cannot reuse it.
        </Notice>
        <SignInLink />
      </div>
    </AuthFrame>
  );
}

function PasswordFields({ id, password, confirmation, onPassword, onConfirmation, error }: { id: string; password: string; confirmation: string; onPassword: (value: string) => void; onConfirmation: (value: string) => void; error?: string }) {
  return (
    <>
      <Field id={`${id}-password`} label="Password" hint="16 to 72 bytes.">
        <input id={`${id}-password`} className={inputClass} type="password" autoComplete="new-password" required value={password} onChange={(event) => onPassword(event.target.value)} aria-describedby={`${id}-password-hint`} />
      </Field>
      <Field id={`${id}-confirm`} label="Confirm password" error={error}>
        <input
          id={`${id}-confirm`}
          className={inputClass}
          type="password"
          autoComplete="new-password"
          required
          value={confirmation}
          onChange={(event) => onConfirmation(event.target.value)}
          aria-invalid={error ? true : undefined}
          aria-describedby={describedBy(`${id}-confirm`, error)}
        />
      </Field>
    </>
  );
}

export function RegisterPage() {
  const config = useQuery(authConfigQuery);
  if (config.isPending) return <AuthFrame title="Create an account">Loading</AuthFrame>;
  if (config.isError || !config.data.registration_enabled) {
    return (
      <AuthFrame title="Create an account">
        <div className="space-y-4">
          <Notice tone={config.isError ? "error" : "info"} title={config.isError ? "Registration status could not be loaded" : "Registration is closed"}>
            {config.isError ? errorMessage(config.error) : "Ask a team administrator to send you an invitation."}
          </Notice>
          <SignInLink />
        </div>
      </AuthFrame>
    );
  }

  return <RegistrationForm config={config.data} />;
}

function RegistrationForm({ config }: { config: AuthConfig }) {
  const navigate = useNavigate();
  const id = useId();
  const register = useRegister();
  const [name, setName] = useState("");
  const [email, setEmail] = useState("");
  const [password, setPassword] = useState("");
  const [confirmation, setConfirmation] = useState("");
  const [error, setError] = useState<string>();

  if (register.isSuccess) {
    return (
      <AuthFrame title="Check your email">
        <div className="space-y-4">
          <p className="text-[13px] text-fg-muted">
            {config.email_enabled
              ? "If this address can be registered, a verification link is on its way. Follow it before signing in."
              : "If this address can be registered, the account is ready. Sign in with your email and password."}
          </p>
          <SignInLink>Go to sign in</SignInLink>
        </div>
      </AuthFrame>
    );
  }

  function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    const problem = passwordError(password, confirmation);
    setError(problem ?? undefined);
    if (problem) return;
    register.mutate({ name: name.trim(), email: email.trim(), password }, { onSuccess: data => { if (data.setup_complete) void navigate({ to: "/", replace: true }); }, onSettled: () => { setPassword(""); setConfirmation(""); } });
  }

  return (
    <AuthFrame title={config.setup_required ? "Set up Cove" : "Create an account"} description={config.setup_required ? "Create the first account to administer this instance. Email delivery can be configured after setup." : "Accounts start without access. A team owner or admin invites you to their projects, or you create your own team."}>
      <form className="space-y-4" onSubmit={submit}>
        <Field id={`${id}-name`} label="Name">
          <input id={`${id}-name`} className={inputClass} autoComplete="name" required maxLength={120} value={name} onChange={(event) => setName(event.target.value)} />
        </Field>
        <Field id={`${id}-email`} label="Email">
          <input id={`${id}-email`} className={inputClass} type="email" autoComplete="email" required value={email} onChange={(event) => setEmail(event.target.value)} />
        </Field>
        <PasswordFields id={id} password={password} confirmation={confirmation} onPassword={setPassword} onConfirmation={setConfirmation} error={error} />
        {register.isError && <Notice tone="error" title="Could not register">{errorMessage(register.error)}</Notice>}
        <Button type="submit" variant="primary" className="w-full" pending={register.isPending}>
          {config.setup_required ? "Create administrator account" : "Create account"}
        </Button>
        {!config.setup_required && <div className="space-y-3 border-t border-line pt-4">
          <GitHubButton config={config} label="Sign up with GitHub" />
          <SignInLink>I already have an account</SignInLink>
        </div>}
      </form>
    </AuthFrame>
  );
}

export function VerifyEmailPage() {
  const [token] = useState(() => fragmentToken("/verify"));
  const verify = useVerifyEmail();
  if (!token) return <MissingToken title="Verify your email" what="verification" />;
  return (
    <AuthFrame title="Verify your email">
      {verify.isSuccess ? (
        <div className="space-y-4">
          <Notice tone="success" title="Email address verified" />
          <SignInLink>Continue to sign in</SignInLink>
        </div>
      ) : (
        <div className="space-y-4">
          <p className="text-[13px] text-fg-muted">Confirm that this email address belongs to you.</p>
          {verify.isError && <Notice tone="error" title="Could not verify the address">{errorMessage(verify.error)}</Notice>}
          <Button variant="primary" className="w-full" pending={verify.isPending} onClick={() => verify.mutate(token, { onSuccess: forgetFragmentToken })}>
            Verify email address
          </Button>
        </div>
      )}
    </AuthFrame>
  );
}

export function ForgotPasswordPage() {
  const forgot = useForgotPassword();
  const config = useQuery(authConfigQuery);
  const [email, setEmail] = useState("");
  return (
    <AuthFrame title="Reset your password">
      {forgot.isSuccess ? (
        <div className="space-y-4">
          <p className="text-[13px] text-fg-muted">If an account uses this address, a reset link has been sent. It expires after a short time.</p>
          <SignInLink />
        </div>
      ) : (
        <form
          className="space-y-4"
          onSubmit={(event) => {
            event.preventDefault();
            forgot.mutate(email.trim());
          }}
        >
          {config.data && !config.data.email_enabled && (
            <Notice tone="warn" title="Email delivery is not configured">
              Reset links cannot be emailed. Ask an instance administrator to create a reset link for you.
            </Notice>
          )}
          <Field id="forgot-email" label="Email">
            <input id="forgot-email" className={inputClass} type="email" autoComplete="email" required value={email} onChange={(event) => setEmail(event.target.value)} />
          </Field>
          {forgot.isError && <Notice tone="error">{errorMessage(forgot.error)}</Notice>}
          <Button type="submit" variant="primary" className="w-full" pending={forgot.isPending} disabled={email.trim() === ""}>
            Send reset link
          </Button>
          <SignInLink />
        </form>
      )}
    </AuthFrame>
  );
}

export function ResetPasswordPage() {
  const id = useId();
  const [token] = useState(() => fragmentToken("/reset-password"));
  const reset = useResetPassword();
  const [password, setPassword] = useState("");
  const [confirmation, setConfirmation] = useState("");
  const [error, setError] = useState<string>();
  if (!token) return <MissingToken title="Choose a new password" what="password reset" />;
  if (reset.isSuccess) {
    return (
      <AuthFrame title="Password changed">
        <div className="space-y-4">
          <p className="text-[13px] text-fg-muted">Sign in with your new password. Other sessions for this account have been signed out.</p>
          <SignInLink>Go to sign in</SignInLink>
        </div>
      </AuthFrame>
    );
  }
  return (
    <AuthFrame title="Choose a new password">
      <form
        className="space-y-4"
        onSubmit={(event) => {
          event.preventDefault();
          const problem = passwordError(password, confirmation);
          setError(problem ?? undefined);
          if (problem) return;
          reset.mutate({ token, password }, { onSuccess: forgetFragmentToken, onSettled: () => { setPassword(""); setConfirmation(""); } });
        }}
      >
        <PasswordFields id={id} password={password} confirmation={confirmation} onPassword={setPassword} onConfirmation={setConfirmation} error={error} />
        {reset.isError && <Notice tone="error" title="Could not change the password">{errorMessage(reset.error)}</Notice>}
        <Button type="submit" variant="primary" className="w-full" pending={reset.isPending}>
          Change password
        </Button>
      </form>
    </AuthFrame>
  );
}

function InvitationSummary({ invitation }: { invitation: InvitationDetails }) {
  const teamWide = invitation.grants.find((grant) => grant.scope === "team");
  const scoped = invitation.grants.filter((grant) => grant.scope !== "team");
  return (
    <dl className="grid grid-cols-[6rem_1fr] gap-x-4 gap-y-2 rounded-md border border-line bg-surface p-3 text-[13px]">
      <dt className="text-fg-muted">Team</dt>
      <dd className="min-w-0 break-words">{invitation.team_name}</dd>
      <dt className="text-fg-muted">Email</dt>
      <dd className="min-w-0 break-all">{invitation.email}</dd>
      <dt className="text-fg-muted">Role</dt>
      <dd>{teamRoleLabels[invitation.role]}</dd>
      {invitation.role === "member" && (
        <>
          <dt className="text-fg-muted">Access</dt>
          <dd>
            {teamWide && <p>{grantRoleLabels[teamWide.role]} across the team</p>}
            {scoped.length > 0 && <p>{scoped.length === 1 ? "1 project or environment grant" : `${scoped.length} project or environment grants`}</p>}
            {invitation.grants.length === 0 && <p className="text-fg-muted">No access until an admin adds grants</p>}
          </dd>
        </>
      )}
    </dl>
  );
}

function AcceptInvitation({ token, invitation }: { token: string; invitation: InvitationDetails }) {
  const id = useId();
  const navigate = useNavigate();
  const session = useQuery(sessionQuery);
  const config = useQuery(authConfigQuery);
  const accept = useAcceptInvitation();
  const [name, setName] = useState("");
  const [password, setPassword] = useState("");
  const [confirmation, setConfirmation] = useState("");
  const [error, setError] = useState<string>();
  const done = () => {
    forgetFragmentToken();
    void navigate({ to: "/projects" });
  };

  if (invitation.account_exists) {
    const signedInAs = session.data?.email;
    if (signedInAs && signedInAs.toLowerCase() === invitation.email.toLowerCase()) {
      return (
        <div className="space-y-4">
          {accept.isError && <Notice tone="error" title="Could not accept the invitation">{errorMessage(accept.error)}</Notice>}
          <Button variant="primary" className="w-full" pending={accept.isPending} onClick={() => accept.mutate({ token }, { onSuccess: done })}>
            Join {invitation.team_name}
          </Button>
        </div>
      );
    }
    return (
      <div className="space-y-3">
        <Notice title={signedInAs ? `You are signed in as ${signedInAs}` : "Sign in to accept"}>
          This invitation is for an existing account. Sign in as {invitation.email} and open this page again from the same tab.
        </Notice>
        <Link to="/login" search={{ redirect: "/invite" }} className={buttonClass("primary", "w-full")}>
          Sign in
        </Link>
      </div>
    );
  }

  return (
    <form
      className="space-y-4"
      onSubmit={(event) => {
        event.preventDefault();
        const problem = passwordError(password, confirmation);
        setError(problem ?? undefined);
        if (problem) return;
        accept.mutate({ token, name: name.trim(), password }, { onSuccess: done, onSettled: () => { setPassword(""); setConfirmation(""); } });
      }}
    >
      <Field id={`${id}-name`} label="Name">
        <input id={`${id}-name`} className={inputClass} autoComplete="name" required maxLength={120} value={name} onChange={(event) => setName(event.target.value)} />
      </Field>
      <PasswordFields id={id} password={password} confirmation={confirmation} onPassword={setPassword} onConfirmation={setConfirmation} error={error} />
      {accept.isError && <Notice tone="error" title="Could not accept the invitation">{errorMessage(accept.error)}</Notice>}
      <Button type="submit" variant="primary" className="w-full" pending={accept.isPending}>
        Create account and join
      </Button>
      <div className="border-t border-line pt-4">
        <GitHubButton config={config.data} invitationToken={token} label="Join with GitHub" />
      </div>
    </form>
  );
}

export function InvitationPage() {
  const [token] = useState(() => fragmentToken("/invite"));
  const details = useInvitationDetails();
  if (!token) return <MissingToken title="Team invitation" what="invitation" />;
  return (
    <AuthFrame title="Team invitation">
      {details.data ? (
        <div className="space-y-4">
          <InvitationSummary invitation={details.data} />
          <AcceptInvitation token={token} invitation={details.data} />
        </div>
      ) : (
        <div className="space-y-4">
          <p className="text-[13px] text-fg-muted">Review the invitation before accepting it.</p>
          {details.isError && <Notice tone="error" title="This invitation cannot be used">{errorMessage(details.error)}</Notice>}
          <Button variant="primary" className="w-full" pending={details.isPending} onClick={() => details.mutate(token)}>
            Review invitation
          </Button>
        </div>
      )}
    </AuthFrame>
  );
}

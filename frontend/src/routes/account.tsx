import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Link, useNavigate } from "@tanstack/react-router";
import { CheckCircle2, UserRound } from "lucide-react";
import { useState } from "react";
import { useGitHubStart } from "../api/auth";
import { errorMessage, request } from "../api/client";
import { authConfigQuery } from "../api/queries";
import { useUpdateAccount } from "../api/teams";
import { AccountTimezone } from "../components/TimezoneSettings";
import { Button, Field, LoadingRows, Notice, PageHeader, Section, SettingRows } from "../components/ui";
import { formatAbsolute } from "../lib/format";
import { useOnline } from "../lib/hooks";
import { passwordError } from "../lib/password";
import { useSession } from "../lib/session";
import { buttonClass, inputClass } from "../lib/styles";

function SetPasswordGuidance({ action }: { action: string }) {
  const config = useQuery(authConfigQuery);
  return (
    <Notice title="This account signs in with GitHub only" action={config.data?.email_enabled && <Link to="/forgot-password" className={buttonClass("secondary")}>Email me a reset link</Link>}>
      Set a password first to {action}.{" "}
      {config.data?.email_enabled
        ? "A password reset link sent to your verified email lets you choose one."
        : "Email delivery is not configured, so ask an instance administrator for a password reset link."}
    </Notice>
  );
}

function Profile() {
  const session = useSession();
  const online = useOnline();
  const update = useUpdateAccount();
  const config = useQuery(authConfigQuery);
  const [name, setName] = useState(session?.name ?? "");
  const [email, setEmail] = useState(session?.email ?? "");
  const [current, setCurrent] = useState("");
  const emailChanged = email.trim() !== (session?.email ?? "");
  if (!session?.has_password) {
    return (
      <Section title="Profile" width="form">
        <SetPasswordGuidance action="change your name or email" />
      </Section>
    );
  }

  return (
    <form
      className="min-w-0"
      onSubmit={(event) => {
        event.preventDefault();
        update.mutate({ name: name.trim(), email: email.trim(), current_password: current }, { onSettled: () => setCurrent("") });
      }}
    >
      <Section
        title="Profile"
        width="form"
        footer={
          <div className="flex justify-end">
            <Button type="submit" variant="primary" disabled={!online || current === ""} pending={update.isPending}>
              Save profile
            </Button>
          </div>
        }
      >
      <div className="space-y-4">
      {!session?.email && (
        <Notice title="This account has no email address yet">
          The initial administrator password signs in without an email. Adding an email requires email delivery so the address can be verified.
        </Notice>
      )}
      <SettingRows>
      <Field layout="row" id="account-name" label="Name">
        <input id="account-name" className={inputClass} autoComplete="name" required maxLength={120} value={name} onChange={(event) => setName(event.target.value)} />
      </Field>
      <Field
        layout="row"
        id="account-email"
        label="Email"
        hint={emailChanged ? (config.data?.email_enabled === false ? "Email delivery is not configured, so the address cannot be verified and the change will be refused." : "A verification link is sent to the new address. The change applies once it is verified.") : undefined}
      >
        <input
          id="account-email"
          className={inputClass}
          type="email"
          autoComplete="email"
          value={email}
          onChange={(event) => setEmail(event.target.value)}
          aria-describedby={emailChanged ? "account-email-hint" : undefined}
        />
      </Field>
      <Field layout="row" id="account-current" label="Current password" hint="Required to save profile changes.">
        <input id="account-current" className={inputClass} type="password" autoComplete="current-password" required value={current} onChange={(event) => setCurrent(event.target.value)} aria-describedby="account-current-hint" />
      </Field>
      </SettingRows>
      {update.isError && <Notice tone="error" title="Could not save your profile">{errorMessage(update.error)}</Notice>}
      {update.isSuccess && (
        <Notice tone="success" title={update.data.verification_required ? "Check your email" : "Profile saved"}>
          {update.data.verification_required ? "Follow the link sent to the new address to finish changing it." : undefined}
        </Notice>
      )}
      </div>
      </Section>
    </form>
  );
}

function ChangePassword() {
  const session = useSession();
  const client = useQueryClient();
  const navigate = useNavigate();
  const online = useOnline();
  const [current, setCurrent] = useState("");
  const [password, setPassword] = useState("");
  const [confirmation, setConfirmation] = useState("");
  const [error, setError] = useState("");
  const change = useMutation({
    gcTime: 0,
    mutationFn: () => request("POST", "/api/password", { current_password: current, new_password: password }),
    onSuccess: () => {
      setCurrent("");
      setPassword("");
      setConfirmation("");
      client.clear();
      void navigate({ to: "/login" });
    },
  });
  if (!session?.has_password) {
    return (
      <Section title="Password" width="form">
        <SetPasswordGuidance action="sign in with a password" />
      </Section>
    );
  }
  return (
    <form
      className="min-w-0"
      onSubmit={(event) => {
        event.preventDefault();
        const problem = passwordError(password, confirmation);
        setError(problem ?? "");
        if (!problem) change.mutate();
      }}
    >
      <Section
        title="Password"
        width="form"
        footer={
          <div className="flex justify-end">
            <Button type="submit" variant="primary" disabled={!online || change.isPending}>
              Change password and sign out
            </Button>
          </div>
        }
      >
      <div className="space-y-4">
      <Notice title="All of your browser sessions will be signed out">Sign in again with the new password. API tokens are managed separately.</Notice>
      <SettingRows>
      <Field layout="row" id="current-password" label="Current password">
        <input id="current-password" className={inputClass} type="password" autoComplete="current-password" required value={current} onChange={(event) => setCurrent(event.target.value)} />
      </Field>
      <Field layout="row" id="new-password" label="New password" hint="16 to 72 bytes.">
        <input id="new-password" className={inputClass} type="password" autoComplete="new-password" required value={password} onChange={(event) => setPassword(event.target.value)} aria-describedby="new-password-hint" />
      </Field>
      <Field layout="row" id="confirm-password" label="Confirm new password">
        <input id="confirm-password" className={inputClass} type="password" autoComplete="new-password" required value={confirmation} onChange={(event) => setConfirmation(event.target.value)} />
      </Field>
      </SettingRows>
      {(error || change.isError) && (
        <Notice tone="error" title="Could not change password">
          {error || errorMessage(change.error)}
        </Notice>
      )}
      </div>
      </Section>
    </form>
  );
}

function LinkGitHub() {
  const session = useSession();
  const config = useQuery(authConfigQuery);
  const start = useGitHubStart();
  const online = useOnline();
  const [current, setCurrent] = useState("");
  if (!config.data) return <p className="text-[13px] text-fg-muted">{config.isError ? errorMessage(config.error) : "Loading sign-in options"}</p>;
  if (session?.github_linked) {
    return (
      <p className="flex items-center gap-2 text-[13px] text-fg">
        <CheckCircle2 aria-hidden className="size-4 text-ok" />
        A GitHub account is linked. You can sign in with GitHub{session.has_password ? " or with your password" : ""}.
      </p>
    );
  }
  if (!config.data.github_enabled) return <p className="text-[13px] text-fg-muted">GitHub sign-in is not configured on this instance.</p>;
  return (
    <form
      className="min-w-0 space-y-4"
      onSubmit={(event) => {
        event.preventDefault();
        start.mutate({ link: true, current_password: current }, { onSettled: () => setCurrent("") });
      }}
    >
      <p className="text-[13px] text-fg-muted">
        Link a GitHub account to sign in with it. The GitHub account must have a verified email address that matches this account. Accounts are never linked
        automatically.
      </p>
      <Field layout="row" id="github-current" label="Current password">
        <input id="github-current" className={inputClass} type="password" autoComplete="current-password" required value={current} onChange={(event) => setCurrent(event.target.value)} />
      </Field>
      {start.isError && <Notice tone="error" title="Could not start GitHub linking">{errorMessage(start.error)}</Notice>}
      <div className="flex justify-end border-t border-line pt-4">
        <Button type="submit" disabled={!online || current === ""} pending={start.isPending || start.isSuccess}>
          Link GitHub account
        </Button>
      </div>
    </form>
  );
}

interface AccountSession { id: string; expires_at: string; user_agent: string; current: boolean }

function Sessions() {
  const client = useQueryClient();
  const online = useOnline();
  const sessions = useQuery({ queryKey: ["account-sessions"], queryFn: () => request<AccountSession[]>("GET", "/api/account/sessions"), refetchInterval: 30_000 });
  const revoke = useMutation({
    mutationFn: (id: string) => request("DELETE", `/api/account/sessions/${encodeURIComponent(id)}`),
    onSuccess: () => client.invalidateQueries({ queryKey: ["account-sessions"] }),
    networkMode: "always",
  });
  if (sessions.isPending) return <LoadingRows label="Loading sessions" rows={2} />;
  if (sessions.error) return <div className="p-3"><Notice tone="error" title="Could not load sessions">{errorMessage(sessions.error)}</Notice></div>;
  return <>
    {revoke.error && <div className="p-3"><Notice tone="error" title="Could not revoke session">{errorMessage(revoke.error)}</Notice></div>}
    <ul className="divide-y divide-line">
      {sessions.data.map((session) => <li key={session.id} className="flex flex-wrap items-center justify-between gap-3 px-4 py-2.5">
        <div className="min-w-0 flex-1">
          <p className="text-[13px] font-medium text-fg">{session.current ? "This browser" : "Other session"}</p>
          <p className="break-all text-xs text-fg-muted">{session.user_agent || "Browser details unavailable for this sign-in"}</p>
          <p className="text-xs text-fg-muted">Expires {formatAbsolute(session.expires_at)}</p>
        </div>
        {!session.current && <Button size="sm" disabled={!online || revoke.isPending} onClick={() => revoke.mutate(session.id)}>Revoke session</Button>}
      </li>)}
    </ul>
  </>;
}

export function AccountPage() {
  const session = useSession();
  return (
    <>
      <PageHeader icon={<UserRound aria-hidden />} title="Account" description={session?.email || "Signed in with the initial administrator password"} />
      <Profile />
      <ChangePassword />
      <AccountTimezone />
      <Section title="Active sessions" description="Revoking another session signs that browser out on its next request. Use Sign out for this browser." width="form" flush>
        <Sessions />
      </Section>
      <Section title="GitHub" width="form">
        <LinkGitHub />
      </Section>
    </>
  );
}

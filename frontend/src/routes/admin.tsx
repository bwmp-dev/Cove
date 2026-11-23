import { InstanceIntegrations } from "../components/InstanceIntegrations";
import { McpSettings } from "../components/McpSettings";
import { AuditExport, InstanceSecondFactor, SignInProviders } from "../components/InstanceSecurity";
import { PanelAccessSettings } from "../components/PanelAccess";
import { OutboundProxy } from "../components/OutboundProxy";
import { TemplateRepositories } from "../components/TemplateRepositories";
import { SecurityPosture } from "../components/SecurityPosture";
import { LoginHistory, loginHistoryDescription } from "../components/LoginHistory";
import { useResetSecondFactor } from "../api/security";
import { Settings, ShieldCheck } from "lucide-react";
import { useQuery } from "@tanstack/react-query";
import { useState } from "react";
import { errorMessage } from "../api/client";
import { settingsQuery, usersQuery, useResetLink, useUpdateSettings, useUpdateUser, type User } from "../api/teams";
import { updatesQuery, useUpdateChannel, type UpdateChannelName, type UpdateTarget } from "../api/updates";
import type { Tone } from "../lib/nodes";
import { Select } from "../components/Select";
import { ConfirmDialog } from "../components/ConfirmDialog";
import { CopyButton } from "../components/CopyButton";
import { RelativeTime } from "../components/RelativeTime";
import { StatusLabel } from "../components/status";
import { Button, EmptyState, Field, LoadingRows, Notice, PageHeader, QueryError, Section, SettingRow, SettingRows } from "../components/ui";
import { useOnline } from "../lib/hooks";
import { useSession } from "../lib/session";
import { inputClass } from "../lib/styles";

function Configured({ enabled }: { enabled: boolean }) {
  return <StatusLabel tone={enabled ? "ok" : "neutral"} label={enabled ? "Configured" : "Not configured"} />;
}

const channelLabels: Record<UpdateChannelName, string> = {
  release: "Release — published stable versions",
  beta: "Beta — every successful main build",
};

function shortRevision(revision: string) {
  return revision ? revision.slice(0, 12) : "unknown";
}

function targetStatus(target: UpdateTarget | undefined, current: boolean): { tone: Tone; label: string } {
  if (current) return { tone: "ok", label: "Selected" };
  if (!target?.available) return { tone: "neutral", label: "Not observed" };
  if (!target.allowed) return { tone: "bad", label: "Blocked" };
  if (target.downgrade) return { tone: "warn", label: "Downgrade" };
  return { tone: "ok", label: "Available" };
}

// A channel can be chosen when it is already selected (an idempotent save) or when the host updater's latest
// observation of it is fresh and comparable with this build. Blocked targets are never sent to the API.
function selectable(target: UpdateTarget | undefined, current: boolean) {
  return current || Boolean(target?.available && target.allowed);
}

function ChannelTarget({ name, target, current }: { name: UpdateChannelName; target: UpdateTarget | undefined; current: boolean }) {
  const status = targetStatus(target, current);
  return (
    <li className="space-y-1 py-2.5 first:pt-0 last:pb-0">
      <div className="flex flex-wrap items-center justify-between gap-x-4 gap-y-1">
        <p className="text-[13px] font-medium text-fg">{name === "release" ? "Release" : "Beta"}</p>
        <StatusLabel tone={status.tone} label={status.label} />
      </div>
      {target?.version ? (
        <p className="text-xs text-fg-muted">
          <span className="font-mono break-all text-fg">{target.version}</span>
          <span> · revision </span>
          <span className="font-mono break-all" title={target.revision}>{shortRevision(target.revision)}</span>
          {target.checked_at && <> · checked <RelativeTime value={target.checked_at} /></>}
        </p>
      ) : (
        <p className="text-xs text-fg-muted">The host updater has not reported this channel.</p>
      )}
      {target?.message && <p className="text-xs break-words text-fg-muted">{target.message}</p>}
    </li>
  );
}

function UpdateChannel() {
  const settings = useQuery(updatesQuery);
  const update = useUpdateChannel();
  const online = useOnline();
  const [downgrade, setDowngrade] = useState<UpdateChannelName | null>(null);
  if (settings.isPending) return <LoadingRows label="Loading update settings" rows={2} />;
  if (settings.isError) return <QueryError what="update settings" error={settings.error} onRetry={() => void settings.refetch()} retrying={settings.isFetching} />;
  const data = settings.data;
  const targets = data.targets ?? {};
  const pendingTarget = downgrade ? targets[downgrade] : undefined;

  function choose(channel: UpdateChannelName) {
    if (channel === data.channel) {
      update.mutate({ channel });
      return;
    }
    const target = targets[channel];
    if (!target || !selectable(target, false)) return;
    if (target.downgrade) {
      update.reset();
      setDowngrade(channel);
      return;
    }
    update.mutate({ channel, target_revision: target.revision });
  }

  return <div className="space-y-4">
    <SettingRows>
      <SettingRow label="Installed version" description={<>Build channel: {data.build_channel} · revision <span className="font-mono" title={data.revision}>{shortRevision(data.revision)}</span></>}>
        <span className="font-mono text-xs break-all">{data.version}</span>
      </SettingRow>
      <Field id="update-channel" label="Update channel" layout="row" hint="The host updater checks both channels hourly and reports what each would install; the browser never contacts the release registry. Workers follow the panel. A channel whose database schema is incompatible with this build is blocked and cannot be confirmed. The host updater must be installed first.">
        <Select id="update-channel" value={data.channel} aria-describedby="update-channel-hint" disabled={!online || update.isPending} onChange={(event) => {
          const channel = event.target.value;
          if (channel === "beta" || channel === "release") choose(channel);
        }}>
          {(["release", "beta"] as const).map((channel) => (
            <option key={channel} value={channel} disabled={!selectable(targets[channel], channel === data.channel)}>{channelLabels[channel]}</option>
          ))}
        </Select>
      </Field>
      <SettingRow label="Channel targets" description="What each channel would install, from the host updater's latest observation. Observations older than two hours cannot be used.">
        <ul aria-label="Channel targets" className="w-full divide-y divide-line">
          {(["release", "beta"] as const).map((channel) => <ChannelTarget key={channel} name={channel} target={targets[channel]} current={channel === data.channel} />)}
        </ul>
      </SettingRow>
    </SettingRows>
    {data.confirmed_downgrade_version && (
      <Notice tone="warn" title="Downgrade confirmed">
        The host updater may install <span className="font-mono">{data.confirmed_downgrade_version}</span> (revision <span className="font-mono break-all">{shortRevision(data.confirmed_downgrade_revision ?? "")}</span>).
      </Notice>
    )}
    {update.isError && <Notice tone="error" title="Could not change update channel">{errorMessage(update.error)}</Notice>}
    {update.isSuccess && <Notice tone="success" title="Update channel saved">The next host update check will use this channel.</Notice>}
    <ConfirmDialog
      open={downgrade !== null && pendingTarget !== undefined}
      title="Downgrade the panel?"
      confirmLabel={`Switch to ${downgrade === "beta" ? "beta" : "release"}`}
      danger
      pending={update.isPending}
      onCancel={() => setDowngrade(null)}
      onConfirm={() => {
        if (!downgrade || !pendingTarget) return;
        update.mutate({ channel: downgrade, confirm_downgrade: true, target_revision: pendingTarget.revision }, { onSettled: () => setDowngrade(null) });
      }}
    >
      <p>
        The installed version is <span className="font-mono text-fg">{data.version}</span>. The {downgrade} channel would install the older <span className="font-mono text-fg">{pendingTarget?.version}</span> (revision <span className="font-mono break-all text-fg">{shortRevision(pendingTarget?.revision ?? "")}</span>).
      </p>
      <p className="mt-2">This confirmation applies only to that revision. If the channel reports a different revision, you will be asked again. Its database schema matches this build, but application data changed since then is not reversed. Take a backup first.</p>
    </ConfirmDialog>
  </div>;
}

function SignInSettings() {
  const settings = useQuery(settingsQuery);
  const update = useUpdateSettings();
  const online = useOnline();
  if (settings.isPending) return <p className="text-[13px] text-fg-muted">Loading settings</p>;
  if (settings.isError) return <QueryError what="settings" error={settings.error} onRetry={() => void settings.refetch()} retrying={settings.isFetching} />;
  const { registration_enabled: open, email_enabled: email, github_enabled: github } = settings.data;
  return (
    <div className="space-y-4">
      <SettingRows>
        <SettingRow label="Email delivery" description="Verification, invitations and password resets. Configure below.">
          <Configured enabled={email} />
        </SettingRow>
        <SettingRow label="GitHub sign-in" description="Configure below.">
          <Configured enabled={github} />
        </SettingRow>
        <SettingRow
          label={`Open registration ${open ? "is on" : "is off"}`}
          description={
            <>
              {open
                ? "Anyone who can reach this dashboard can create an account. New accounts have no team access until invited or they create a team."
                : "Only invited people can create accounts. Invitation links still work."}
              {!open && !email && " Registration needs email delivery so new addresses can be verified."}
            </>
          }
        >
          <Button disabled={!online || (!open && !email)} pending={update.isPending} onClick={() => update.mutate({ registration_enabled: !open })}>
            {open ? "Close registration" : "Open registration"}
          </Button>
        </SettingRow>
      </SettingRows>
      {update.isError && <Notice tone="error" title="Could not change registration">{errorMessage(update.error)}</Notice>}
    </div>
  );
}

function ResetLink({ user, onClose }: { user: User; onClose: () => void }) {
  const reset = useResetLink();
  return (
    <div className="space-y-3 border-t border-line bg-panel p-4">
      {reset.data ? (
        <>
          <Field id={`reset-${user.id}`} label={`Password reset link for ${user.email}`} hint={<>Expires <RelativeTime value={reset.data.expires_at} />. Share it privately; it is not shown again.</>}>
            <input id={`reset-${user.id}`} className={`${inputClass} font-mono`} readOnly value={reset.data.url} aria-describedby={`reset-${user.id}-hint`} />
          </Field>
          <div className="flex flex-wrap gap-2">
            <CopyButton text={reset.data.url} label="Copy link" />
            <Button onClick={onClose}>Done</Button>
          </div>
        </>
      ) : (
        <>
          <p className="text-[13px] text-fg-muted">Creates a one-time link that lets {user.email} choose a new password. Use it when email delivery is unavailable.</p>
          {reset.isError && <Notice tone="error" title="Could not create a reset link">{errorMessage(reset.error)}</Notice>}
          <div className="flex flex-wrap gap-2">
            <Button variant="primary" pending={reset.isPending} onClick={() => reset.mutate(user.id)}>
              Create reset link
            </Button>
            <Button variant="ghost" onClick={onClose}>
              Cancel
            </Button>
          </div>
        </>
      )}
    </div>
  );
}

function Users() {
  const session = useSession();
  const users = useQuery(usersQuery);
  const update = useUpdateUser();
  const online = useOnline();
  const resetFactor = useResetSecondFactor();
  const [resetting, setResetting] = useState<string | null>(null);
  const [history, setHistory] = useState<string | null>(null);
  const [disabling, setDisabling] = useState<User | null>(null);
  const [clearing, setClearing] = useState<User | null>(null);
  if (users.isPending) return <LoadingRows label="Loading users" rows={2} />;
  if (users.isError) {
    return (
      <div className="p-3">
        <QueryError what="users" error={users.error} onRetry={() => void users.refetch()} retrying={users.isFetching} />
      </div>
    );
  }
  return (
    <>
      {update.isError && (
        <div className="p-3">
          <Notice tone="error" title="Could not update the user">
            {errorMessage(update.error)}
          </Notice>
        </div>
      )}
      {resetFactor.isError && (
        <div className="p-3">
          <Notice tone="error" title="Could not reset the second factor">
            {errorMessage(resetFactor.error)}
          </Notice>
        </div>
      )}
      {resetFactor.isSuccess && (
        <div className="p-3">
          <Notice tone="success" title="Second factor reset">
            Their authenticator app, passkeys and recovery codes were removed and their sessions signed out. If a second factor is required, they enrol again after signing in.
          </Notice>
        </div>
      )}
      <ul className="divide-y divide-line">
        {users.data.map((user) => {
          const self = user.id === session?.user_id;
          const busy = !online || (update.isPending && update.variables.id === user.id);
          return (
            <li key={user.id}>
              <div className="flex flex-wrap items-center justify-between gap-x-6 gap-y-2 px-4 py-3">
                <div className="min-w-0">
                  <p className="text-[13px] font-medium break-words text-fg">
                    {user.name || user.email}
                    {self && <span className="ml-2 text-xs font-normal text-fg-subtle">You</span>}
                  </p>
                  <p className="text-xs break-all text-fg-muted">
                    {user.email} · {user.is_admin ? "Instance administrator" : "User"}
                    {user.disabled && <span className="text-warn"> · Disabled</span>}
                  </p>
                </div>
                <div className="flex flex-wrap gap-2">
                  <Button variant="ghost" size="sm" disabled={busy} onClick={() => update.mutate({ id: user.id, disabled: user.disabled, is_admin: !user.is_admin })}>
                    {user.is_admin ? "Remove administrator" : "Make administrator"}
                    <span className="sr-only"> {user.email}</span>
                  </Button>
                  <Button variant="ghost" size="sm" disabled={busy || self} onClick={() => (user.disabled ? update.mutate({ id: user.id, disabled: false, is_admin: user.is_admin }) : setDisabling(user))}>
                    {user.disabled ? "Enable" : "Disable"}
                    <span className="sr-only"> {user.email}</span>
                  </Button>
                  <Button variant="ghost" size="sm" disabled={!online || user.disabled} onClick={() => setResetting(user.id)}>
                    Reset link<span className="sr-only"> for {user.email}</span>
                  </Button>
                  <Button variant="ghost" size="sm" disabled={!online} pending={resetFactor.isPending && resetFactor.variables === user.id} onClick={() => setClearing(user)}>
                    Reset second factor<span className="sr-only"> for {user.email}</span>
                  </Button>
                  <Button variant="ghost" size="sm" aria-expanded={history === user.id} onClick={() => setHistory(history === user.id ? null : user.id)}>
                    {history === user.id ? "Hide sign-in history" : "Sign-in history"}
                    <span className="sr-only"> for {user.email}</span>
                  </Button>
                </div>
              </div>
              {resetting === user.id && <ResetLink user={user} onClose={() => setResetting(null)} />}
              {history === user.id && (
                <div className="border-t border-line bg-panel">
                  <p className="px-4 pt-3 text-xs text-fg-muted">{loginHistoryDescription}</p>
                  <LoginHistory userId={user.id} />
                </div>
              )}
            </li>
          );
        })}
      </ul>
      <ConfirmDialog
        open={disabling !== null}
        title={`Disable ${disabling?.email ?? "user"}?`}
        confirmLabel="Disable user"
        danger
        onCancel={() => setDisabling(null)}
        onConfirm={() => {
          if (disabling) update.mutate({ id: disabling.id, disabled: true, is_admin: disabling.is_admin });
          setDisabling(null);
        }}
      >
        They are signed out and cannot sign in until enabled again. Their team memberships are kept. Cove refuses to disable the last administrator.
      </ConfirmDialog>
      <ConfirmDialog
        open={clearing !== null}
        title={`Reset the second factor for ${clearing?.email ?? "this user"}?`}
        confirmLabel="Reset second factor"
        danger
        onCancel={() => setClearing(null)}
        onConfirm={() => {
          if (clearing) resetFactor.mutate(clearing.id);
          setClearing(null);
        }}
      >
        Removes their authenticator app, every passkey and their recovery codes, and signs out all of their sessions. Confirm their identity another way first. If the
        instance or one of their teams requires a second factor, they must enrol again before using projects and resources.
        {clearing?.id === session?.user_id && " This is your own account: you will be signed out."}
      </ConfirmDialog>
    </>
  );
}

export function AdminPage() {
  const session = useSession();
  if (!session?.is_admin) {
    return (
      <>
        <PageHeader icon={<Settings aria-hidden />} title="Settings" />
        <EmptyState title="Instance administrators only">Ask an instance administrator to change sign-in settings or user accounts.</EmptyState>
      </>
    );
  }
  return (
    <>
      <PageHeader icon={<Settings aria-hidden />} title="Settings" description="Sign-in, security, panel access, MCP, outbound proxy, template sources, audit export and update settings for this Cove instance." />
      <SecurityPosture />
      <Section title="Sign-in" width="form">
        <SignInSettings />
      </Section>
      <SignInProviders />
      <InstanceSecondFactor />
      <PanelAccessSettings />
      <InstanceIntegrations />
      <Section title="MCP server" width="form" description="Let MCP clients use the public API with scoped API keys. Off by default.">
        <McpSettings />
      </Section>
      <OutboundProxy />
      <TemplateRepositories scope={{ kind: "instance" }} />
      <AuditExport />
      <Section title="Updates" width="form"><UpdateChannel /></Section>
    </>
  );
}

export function UsersPage() {
  const session = useSession();
  return <>
    <PageHeader icon={<ShieldCheck aria-hidden />} title="Users" description="Manage accounts and instance administrators across all teams." />
    {session?.is_admin ? <Section title="User accounts" flush><Users /></Section> : <EmptyState title="Instance administrators only">Ask an instance administrator to manage user accounts.</EmptyState>}
  </>;
}

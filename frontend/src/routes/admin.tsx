import { BackupTransferLimit } from "../components/BackupTransferLimit";
import { FileTransferLimit } from "../components/FileTransferLimit";
import { InstanceIntegrations } from "../components/InstanceIntegrations";
import { MetricsExport } from "../components/MetricsExport";
import { PanelRecovery } from "../components/PanelRecovery";
import { OperationRetentionPasses, StorageUsage, TelemetryRetention } from "../components/TelemetryRetention";
import { Settings, ShieldCheck } from "lucide-react";
import { useQuery } from "@tanstack/react-query";
import { useState } from "react";
import { errorMessage } from "../api/client";
import { settingsQuery, usersQuery, useResetLink, useUpdateSettings, useUpdateUser, type User } from "../api/teams";
import { updatesQuery, useUpdateChannel } from "../api/updates";
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

function UpdateChannel() {
  const settings = useQuery(updatesQuery);
  const update = useUpdateChannel();
  const online = useOnline();
  if (settings.isPending) return <LoadingRows label="Loading update settings" rows={2} />;
  if (settings.isError) return <QueryError what="update settings" error={settings.error} onRetry={() => void settings.refetch()} retrying={settings.isFetching} />;
  return <div className="space-y-4">
    <SettingRows>
      <SettingRow label="Installed version" description={`Build channel: ${settings.data.build_channel}`}>
        <span className="font-mono text-xs break-all">{settings.data.version}</span>
      </SettingRow>
      <Field id="update-channel" label="Update channel" layout="row" hint="The host updater checks hourly. Workers follow the panel. Switching to release may install an older version. The host updater must be installed first.">
        <Select id="update-channel" value={settings.data.channel} aria-describedby="update-channel-hint" disabled={!online || update.isPending} onChange={(event) => {
          const channel = event.target.value;
          if (channel === "beta" || channel === "release") update.mutate(channel);
        }}>
          <option value="release">Release — published stable versions</option>
          <option value="beta">Beta — every successful main build</option>
        </Select>
      </Field>
    </SettingRows>
    {update.isError && <Notice tone="error" title="Could not change update channel">{errorMessage(update.error)}</Notice>}
    {update.isSuccess && <Notice tone="success" title="Update channel saved">The next host update check will use this channel.</Notice>}
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
  const [resetting, setResetting] = useState<string | null>(null);
  const [disabling, setDisabling] = useState<User | null>(null);
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
                </div>
              </div>
              {resetting === user.id && <ResetLink user={user} onClose={() => setResetting(null)} />}
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
      <PageHeader icon={<Settings aria-hidden />} title="Settings" description="Sign-in, update, panel recovery, backup and file transfer, and data retention settings for this Cove instance." />
      <Section title="Sign-in" width="form">
        <SignInSettings />
      </Section>
      <InstanceIntegrations />
      <Section title="Updates" width="form"><UpdateChannel /></Section>
      <PanelRecovery />
      <BackupTransferLimit />
      <FileTransferLimit />
      <TelemetryRetention />
      <MetricsExport />
      <StorageUsage />
      <OperationRetentionPasses />
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

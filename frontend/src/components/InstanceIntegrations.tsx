import { useQuery } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { useState, type FormEvent } from "react";
import { errorMessage } from "../api/client";
import { integrationsQuery, useUpdateIntegrations, useSendTestEmail, type InstanceIntegrations } from "../api/instance-settings";
import { useOnline } from "../lib/hooks";
import { inputClass } from "../lib/styles";
import { ConfirmDialog } from "./ConfirmDialog";
import { SaveBar, UnsavedChangesGuard } from "./SaveBar";
import { Button, Field, LoadingRows, Notice, QueryError, Section, SettingRow, SettingRows } from "./ui";

function SMTPSettings({ saved }: { saved: InstanceIntegrations["smtp"] }) {
  const [draft, setDraft] = useState<typeof saved>();
  const [password, setPassword] = useState("");
  const [recipient, setRecipient] = useState("");
  const testEmail = useSendTestEmail();
  const update = useUpdateIntegrations();
  const online = useOnline();
  const [confirmDisable, setConfirmDisable] = useState(false);
  const value = draft ?? saved;
  const dirty = draft !== undefined || password !== "";
  const blocked = !online || update.isPending;
  function discard() { setDraft(undefined); setPassword(""); }
  function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    update.mutate({ smtp: { host: value.host, port: value.port, username: value.username, from: value.from, ...(password !== "" ? { password } : {}) } }, { onSuccess: discard });
  }
  return <form onSubmit={submit}>
    <ConfirmDialog open={confirmDisable} title="Disable email delivery?" confirmLabel="Disable email" danger onCancel={() => setConfirmDisable(false)} onConfirm={() => { setConfirmDisable(false); update.mutate({ smtp: { host: "", port: "", username: "", from: "", password: "" } }, { onSuccess: discard }); }}>The saved SMTP credentials will be removed. Account verification and password reset emails will stop until email delivery is configured again.</ConfirmDialog>
    <UnsavedChangesGuard dirty={dirty} />
    <Section title="Email delivery" description="SMTP for account verification and password resets. TLS is required; port 465 uses implicit TLS, other ports use STARTTLS." width="form" footer={<SaveBar dirty={dirty} pending={update.isPending} blocked={!online} onDiscard={discard} status={update.isSuccess ? "Email settings saved" : undefined} />}>
      <fieldset disabled={blocked} className="space-y-4">
        <SettingRows>
          <Field id="smtp-host" label="SMTP host" layout="row"><input id="smtp-host" className={inputClass} value={value.host} onChange={event => setDraft({ ...value, host: event.target.value })} /></Field>
          <Field id="smtp-port" label="SMTP port" layout="row"><input id="smtp-port" className={inputClass} type="number" min={1} max={65535} value={value.port} onChange={event => setDraft({ ...value, port: event.target.value })} /></Field>
          <Field id="smtp-from" label="Sender email" layout="row"><input id="smtp-from" className={inputClass} type="email" value={value.from} onChange={event => setDraft({ ...value, from: event.target.value })} /></Field>
          <Field id="smtp-user" label="SMTP username" optional layout="row"><input id="smtp-user" className={inputClass} autoComplete="off" value={value.username} onChange={event => setDraft({ ...value, username: event.target.value })} /></Field>
          <Field id="smtp-password" label="SMTP password" optional layout="row" hint={saved.password_set ? "A password is saved. Leave blank to keep it." : "Only needed when the SMTP server requires authentication."}><input id="smtp-password" className={inputClass} type="password" autoComplete="new-password" aria-describedby="smtp-password-hint" value={password} onChange={event => setPassword(event.target.value)} /></Field>
          <Field id="smtp-test-recipient" label="Test recipient" layout="row" hint="Save changes before testing delivery."><div className="flex flex-wrap gap-2"><input id="smtp-test-recipient" className={inputClass} type="email" aria-describedby="smtp-test-recipient-hint" value={recipient} onChange={event => setRecipient(event.target.value)} /><Button disabled={dirty || !saved.host || !recipient} pending={testEmail.isPending} onClick={() => testEmail.mutate(recipient)}>Send test email</Button></div></Field>
          <SettingRow label="Disable email delivery" description="Close open registration first. This removes the stored SMTP credentials."><Button disabled={!saved.host} onClick={() => setConfirmDisable(true)}>Disable email</Button></SettingRow>
        </SettingRows>
        {testEmail.isError && <Notice tone="error" title="Test email failed">{errorMessage(testEmail.error)}</Notice>}
        {testEmail.isSuccess && <Notice tone="success">Test email accepted by the SMTP server. Check the recipient inbox.</Notice>}
        {update.isError && <Notice tone="error" title="Could not save email settings">{errorMessage(update.error)}</Notice>}
      </fieldset>
    </Section>
  </form>;
}

function GitHubSignInSettings({ saved }: { saved: InstanceIntegrations["github"] }) {
  const [clientID, setClientID] = useState<string>();
  const [secret, setSecret] = useState("");
  const update = useUpdateIntegrations();
  const online = useOnline();
  const [confirmDisable, setConfirmDisable] = useState(false);
  const dirty = clientID !== undefined || secret !== "";
  function discard() { setClientID(undefined); setSecret(""); }
  function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    update.mutate({ github: { client_id: clientID ?? saved.client_id, ...(secret !== "" ? { client_secret: secret } : {}) } }, { onSuccess: discard });
  }
  return <form onSubmit={submit}>
    <ConfirmDialog open={confirmDisable} title="Disable GitHub sign-in?" confirmLabel="Disable GitHub sign-in" danger onCancel={() => setConfirmDisable(false)} onConfirm={() => { setConfirmDisable(false); update.mutate({ github: { client_id: "", client_secret: "" } }, { onSuccess: discard }); }}>The saved OAuth credentials will be removed. People who use GitHub will need a password or a password reset link to sign in.</ConfirmDialog>
    <UnsavedChangesGuard dirty={dirty} />
    <Section title="GitHub sign-in" description="Use the client ID and secret from a GitHub OAuth App. Configure repository access separately in Integrations." width="form" footer={<SaveBar dirty={dirty} pending={update.isPending} blocked={!online} onDiscard={discard} status={update.isSuccess ? "GitHub settings saved" : undefined} />}>
      <fieldset disabled={!online || update.isPending} className="space-y-4">
        <SettingRows>
          <SettingRow label="Authorization callback URL"><span className="break-all font-mono text-xs">{saved.callback_url}</span></SettingRow>
          <Field id="github-client-id" label="Client ID" layout="row"><input id="github-client-id" className={inputClass} autoComplete="off" value={clientID ?? saved.client_id} onChange={event => setClientID(event.target.value)} /></Field>
          <Field id="github-client-secret" label="Client secret" layout="row" hint={saved.client_secret_set ? "A secret is saved. Leave blank to keep it." : "Secrets are encrypted and never returned to the browser."}><input id="github-client-secret" className={inputClass} type="password" autoComplete="new-password" aria-describedby="github-client-secret-hint" value={secret} onChange={event => setSecret(event.target.value)} /></Field>
          <SettingRow label="Repository access"><Link to="/integrations" className="rounded text-fg underline underline-offset-4">Configure GitHub App</Link></SettingRow>
          <SettingRow label="Disable GitHub sign-in" description="Password sign-in remains available."><Button disabled={!saved.client_id} onClick={() => setConfirmDisable(true)}>Disable GitHub sign-in</Button></SettingRow>
        </SettingRows>
        {update.isError && <Notice tone="error" title="Could not save GitHub settings">{errorMessage(update.error)}</Notice>}
      </fieldset>
    </Section>
  </form>;
}

export function InstanceIntegrations() {
  const settings = useQuery(integrationsQuery);
  if (settings.isPending) return <Section title="Instance integrations" width="form"><LoadingRows label="Loading integration settings" rows={3} /></Section>;
  if (settings.isError) return <Section title="Instance integrations" width="form"><QueryError what="integration settings" error={settings.error} onRetry={() => void settings.refetch()} retrying={settings.isFetching} /></Section>;
  return <>
    <SMTPSettings saved={settings.data.smtp} />
    <GitHubSignInSettings saved={settings.data.github} />
  </>;
}

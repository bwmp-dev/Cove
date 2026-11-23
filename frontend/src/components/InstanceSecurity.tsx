import { useQuery } from "@tanstack/react-query";
import { Plus } from "lucide-react";
import { useId, useState, type FormEvent } from "react";
import { errorMessage } from "../api/client";
import {
  auditExportQuery,
  oidcProvidersQuery,
  signInPolicyQuery,
  useDeleteOidcProvider,
  useSaveOidcProvider,
  useUpdateAuditExport,
  useUpdateSignInPolicy,
  type AuditExportSettings,
  type OidcProvider,
  type SignInPolicy,
} from "../api/instance-settings";
import { secondFactorPolicyQuery, useUpdateSecondFactorPolicy } from "../api/security";
import { withBasePath } from "../lib/base-path";
import { useOnline } from "../lib/hooks";
import { extraScopes, generateSecret, parseScopes, validateAuditExport, validateProvider, type AuditExportDraft, type AuditExportErrors, type ProviderDraft, type ProviderErrors } from "../lib/security-settings";
import { describedBy, inputClass } from "../lib/styles";
import { ConfirmDialog } from "./ConfirmDialog";
import { CopyButton } from "./CopyButton";
import { SaveBar, UnsavedChangesGuard } from "./SaveBar";
import { Select } from "./Select";
import { StatusLabel } from "./status";
import { Button, Field, LoadingRows, Notice, QueryError, Section, SettingRow, SettingRows, SwitchField } from "./ui";

const breakGlassURL = () => `${window.location.origin}${withBasePath("/login?break_glass=true")}`;

export function InstanceSecondFactor() {
  const policy = useQuery(secondFactorPolicyQuery(null));
  const update = useUpdateSecondFactorPolicy(null);
  const online = useOnline();
  const [confirming, setConfirming] = useState(false);
  return (
    <Section title="Two-step verification" description="Teams can also require a second factor for their own members." width="form">
      {policy.isPending ? (
        <p role="status" className="text-[13px] text-fg-muted">
          Loading policy
        </p>
      ) : policy.isError ? (
        <QueryError what="the two-step verification policy" error={policy.error} onRetry={() => void policy.refetch()} retrying={policy.isFetching} />
      ) : (
        <div className="space-y-4">
          <SwitchField
            id="instance-second-factor"
            layout="row"
            label="Require a second factor for every account"
            description="Accounts without an authenticator app or passkey keep signing in, but can only reach their account page until they enrol one."
            checked={policy.data.require_second_factor}
            disabled={!online || update.isPending}
            onChange={(event) => {
              if (event.target.checked) setConfirming(true);
              else update.mutate(false);
            }}
          />
          {update.isError && <Notice tone="error" title="Could not change the policy">{errorMessage(update.error)}</Notice>}
          {update.isSuccess && <Notice tone="success" title={update.data.require_second_factor ? "Second factor required for every account" : "Second factor optional unless a team requires it"} />}
        </div>
      )}
      <ConfirmDialog
        open={confirming}
        title="Require a second factor for every account?"
        confirmLabel="Require second factor"
        pending={update.isPending}
        onCancel={() => setConfirming(false)}
        onConfirm={() => update.mutate(true, { onSettled: () => setConfirming(false) })}
      >
        Everyone without an authenticator app or passkey, including you, is blocked from projects and resources until they set one up from their account page. The
        startup administrator token is not affected.
      </ConfirmDialog>
    </Section>
  );
}

function SignInPolicyForm({ saved, providers }: { saved: SignInPolicy; providers: OidcProvider[] }) {
  const update = useUpdateSignInPolicy();
  const online = useOnline();
  const [draft, setDraft] = useState<SignInPolicy>();
  const value = draft ?? saved;
  const dirty = draft !== undefined;
  const redirectMissing = value.oidc_auto_redirect !== "" && !providers.some((provider) => provider.id === value.oidc_auto_redirect);
  function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    update.mutate(value, { onSuccess: () => setDraft(undefined) });
  }
  return (
    <form onSubmit={submit}>
      <UnsavedChangesGuard dirty={dirty} />
      <Section
        title="Sign-in policy"
        description="How people sign in with passwords and identity providers. GitHub sign-in and open registration are configured separately."
        width="form"
        footer={<SaveBar dirty={dirty} pending={update.isPending} blocked={!online} onDiscard={() => setDraft(undefined)} status={update.isSuccess ? "Sign-in policy saved" : undefined} />}
      >
        <fieldset disabled={!online || update.isPending} className="space-y-4">
          <SettingRows>
            <SwitchField
              id="password-login-disabled"
              layout="row"
              label="Turn off password sign-in"
              description="People sign in with an identity provider, GitHub or a passkey. Instance administrators keep a break-glass password route that ignores auto-redirect; their enrolled second factors still apply."
              checked={value.password_login_disabled}
              onChange={(event) => setDraft({ ...value, password_login_disabled: event.target.checked })}
            />
            <SettingRow label="Break-glass sign-in" description="Keep this address somewhere you can find it if your identity provider is down. If no administrator can sign in, the startup administrator token can reset this policy.">
              <span className="font-mono text-xs break-all">{breakGlassURL()}</span>
            </SettingRow>
            <SwitchField
              id="oidc-registration"
              layout="row"
              label="Create accounts from single sign-on"
              description="People who sign in with a configured identity provider and have no account get one, even when open registration is off. Existing accounts are never linked by email address."
              checked={value.oidc_registration_enabled}
              onChange={(event) => setDraft({ ...value, oidc_registration_enabled: event.target.checked })}
            />
            <Field id="oidc-auto-redirect" layout="row" label="Send the sign-in page to" hint="Skips the sign-in page and opens this provider directly. It never applies after a failed provider sign-in or on the break-glass address.">
              <Select id="oidc-auto-redirect" className={inputClass} value={value.oidc_auto_redirect} aria-describedby="oidc-auto-redirect-hint" onChange={(event) => setDraft({ ...value, oidc_auto_redirect: event.target.value })}>
                <option value="">Nobody: show the sign-in page</option>
                {providers.map((provider) => (
                  <option key={provider.id} value={provider.id}>
                    {provider.configuration.display_name}
                  </option>
                ))}
              </Select>
            </Field>
          </SettingRows>
          {value.password_login_disabled && providers.length === 0 && (
            <Notice tone="warn" title="No identity provider is configured">
              With password sign-in off, people can only use GitHub or passkeys they already registered. Add a provider first, or keep password sign-in on.
            </Notice>
          )}
          {redirectMissing && <Notice tone="warn">The auto-redirect provider no longer exists. Choose another provider or turn auto-redirect off.</Notice>}
          {update.isError && <Notice tone="error" title="Could not save the sign-in policy">{errorMessage(update.error)}</Notice>}
        </fieldset>
      </Section>
    </form>
  );
}

function ProviderEditor({ provider, onClose }: { provider?: OidcProvider; onClose: () => void }) {
  const id = useId();
  const online = useOnline();
  const save = useSaveOidcProvider();
  const [draft, setDraft] = useState<ProviderDraft>(() => ({
    display_name: provider?.configuration.display_name ?? "",
    issuer: provider?.configuration.issuer ?? "",
    client_id: provider?.configuration.client_id ?? "",
    scopes: extraScopes(provider?.configuration.scopes ?? []).join(" "),
  }));
  const [secret, setSecret] = useState("");
  const [errors, setErrors] = useState<ProviderErrors>({});
  const input = (key: keyof ProviderDraft, label: string, hint?: string, extra: { type?: string; placeholder?: string; mono?: boolean } = {}) => (
    <Field id={`${id}-${key}`} label={label} hint={hint} error={errors[key]}>
      <input
        id={`${id}-${key}`}
        className={`${inputClass} ${extra.mono ? "font-mono" : ""}`}
        type={extra.type ?? "text"}
        autoComplete="off"
        spellCheck={false}
        placeholder={extra.placeholder}
        value={draft[key]}
        onChange={(event) => setDraft({ ...draft, [key]: event.target.value })}
        aria-invalid={errors[key] ? true : undefined}
        aria-describedby={describedBy(`${id}-${key}`, errors[key], hint)}
      />
    </Field>
  );
  return (
    <form
      className="space-y-4 border-t border-line bg-panel p-4"
      noValidate
      onSubmit={(event) => {
        event.preventDefault();
        const problems = validateProvider(draft);
        setErrors(problems);
        if (Object.keys(problems).length > 0) return;
        save.mutate(
          {
            id: provider?.id,
            input: {
              display_name: draft.display_name.trim(),
              issuer: draft.issuer.trim(),
              client_id: draft.client_id.trim(),
              scopes: parseScopes(draft.scopes),
              ...(secret !== "" ? { client_secret: secret } : {}),
            },
          },
          { onSuccess: onClose, onSettled: () => setSecret("") },
        );
      }}
    >
      <div className="grid gap-4 sm:grid-cols-2">
        {input("display_name", "Display name", "Shown on the sign-in button.")}
        {input("issuer", "Issuer URL", "HTTPS. Discovery runs when someone first signs in.", { type: "url", placeholder: "https://login.example.com", mono: true })}
        {input("client_id", "Client ID", undefined, { mono: true })}
        <Field id={`${id}-secret`} label="Client secret" optional hint={provider ? "A secret is saved. Leave blank to keep it." : "Encrypted and never shown again."}>
          <input
            id={`${id}-secret`}
            className={inputClass}
            type="password"
            autoComplete="new-password"
            value={secret}
            onChange={(event) => setSecret(event.target.value)}
            aria-describedby={`${id}-secret-hint`}
          />
        </Field>
      </div>
      {input("scopes", "Additional scopes", "openid, email and profile are always requested. Separate extra scopes with spaces. The provider must return email_verified.", { mono: true })}
      {save.isError && <Notice tone="error" title="Could not save the provider">{errorMessage(save.error)}</Notice>}
      <div className="flex flex-wrap justify-end gap-2">
        <Button variant="ghost" onClick={onClose} disabled={save.isPending}>
          Cancel
        </Button>
        <Button type="submit" variant="primary" disabled={!online} pending={save.isPending}>
          {provider ? "Save provider" : "Add provider"}
        </Button>
      </div>
    </form>
  );
}

function OidcProviders({ providers, autoRedirect }: { providers: OidcProvider[]; autoRedirect: string | undefined }) {
  const online = useOnline();
  const remove = useDeleteOidcProvider();
  const [editing, setEditing] = useState<string | null>(null);
  const [deleting, setDeleting] = useState<OidcProvider | null>(null);
  return (
    <Section
      title="Identity providers"
      description="OpenID Connect providers people can sign in with. Register each provider's redirect URI with that provider."
      flush
      actions={
        editing !== "new" && (
          <Button disabled={!online} onClick={() => setEditing("new")}>
            <Plus aria-hidden className="size-3.5" />
            Add provider
          </Button>
        )
      }
    >
      {remove.isError && (
        <div className="p-3">
          <Notice tone="error" title="Could not remove the provider">
            {errorMessage(remove.error)}
          </Notice>
        </div>
      )}
      {editing === "new" && <ProviderEditor onClose={() => setEditing(null)} />}
      {providers.length === 0 && editing !== "new" && <p className="px-4 py-3 text-[13px] text-fg-muted">No identity providers yet.</p>}
      <ul className="divide-y divide-line">
        {providers.map((provider) => {
          const redirecting = provider.id === autoRedirect;
          return (
            <li key={provider.id}>
              <div className="flex flex-wrap items-start justify-between gap-x-6 gap-y-2 px-4 py-3">
                <div className="min-w-0 flex-1 space-y-0.5">
                  <p className="text-[13px] font-medium break-words text-fg">
                    {provider.configuration.display_name}
                    {redirecting && <span className="ml-2 text-xs font-normal text-fg-subtle">Auto-redirect</span>}
                  </p>
                  <p className="font-mono text-xs break-all text-fg-muted">{provider.configuration.issuer}</p>
                  <p className="text-xs break-all text-fg-muted">
                    Client ID <span className="font-mono">{provider.configuration.client_id}</span> · Scopes {provider.configuration.scopes.join(" ")}
                  </p>
                  <div className="flex flex-wrap items-center gap-x-2 pt-1">
                    <span className="text-xs text-fg-muted">Redirect URI</span>
                    <span className="font-mono text-xs break-all text-fg">{provider.redirect_uri}</span>
                  </div>
                </div>
                <div className="flex flex-wrap items-center gap-2">
                  <CopyButton text={provider.redirect_uri} label="Copy redirect URI" variant="ghost" />
                  <Button variant="ghost" size="sm" disabled={!online || editing === provider.id} onClick={() => setEditing(provider.id)}>
                    Edit<span className="sr-only"> {provider.configuration.display_name}</span>
                  </Button>
                  <Button
                    variant="ghost"
                    size="sm"
                    disabled={!online || redirecting}
                    title={redirecting ? "Turn off auto-redirect to this provider first" : undefined}
                    pending={remove.isPending && remove.variables === provider.id}
                    onClick={() => setDeleting(provider)}
                  >
                    Remove<span className="sr-only"> {provider.configuration.display_name}</span>
                  </Button>
                </div>
              </div>
              {redirecting && <p className="px-4 pb-3 text-xs text-fg-muted">The sign-in page redirects here. Turn auto-redirect off in the sign-in policy before removing it.</p>}
              {editing === provider.id && <ProviderEditor provider={provider} onClose={() => setEditing(null)} />}
            </li>
          );
        })}
      </ul>
      <ConfirmDialog
        open={deleting !== null}
        title={`Remove ${deleting?.configuration.display_name ?? "provider"}?`}
        confirmLabel="Remove provider"
        danger
        onCancel={() => setDeleting(null)}
        onConfirm={() => {
          if (deleting) remove.mutate(deleting.id);
          setDeleting(null);
        }}
      >
        Every account's link to this provider is deleted. Accounts without a password, passkey or other linked sign-in method will be unable to sign in until an
        administrator sends them a password reset link.
      </ConfirmDialog>
    </Section>
  );
}

export function SignInProviders() {
  const providers = useQuery(oidcProvidersQuery);
  const policy = useQuery(signInPolicyQuery);
  if (providers.isPending || policy.isPending) {
    return (
      <Section title="Sign-in policy" width="form">
        <LoadingRows label="Loading sign-in policy" rows={3} />
      </Section>
    );
  }
  if (providers.isError || policy.isError) {
    const failed = providers.isError ? providers : policy;
    return (
      <Section title="Sign-in policy" width="form">
        <QueryError what="sign-in policy" error={failed.error} onRetry={() => void failed.refetch()} retrying={failed.isFetching} />
      </Section>
    );
  }
  return (
    <>
      <SignInPolicyForm saved={policy.data} providers={providers.data} />
      <OidcProviders providers={providers.data} autoRedirect={policy.data.oidc_auto_redirect} />
    </>
  );
}

function AuditExportForm({ saved }: { saved: AuditExportSettings }) {
  const id = useId();
  const online = useOnline();
  const update = useUpdateAuditExport();
  const initial: AuditExportDraft = {
    webhook_url: saved.configuration.webhook_url,
    webhook_secret: "",
    syslog_address: saved.configuration.syslog_address,
    retention_days: String(saved.configuration.retention_days),
  };
  const [draft, setDraft] = useState<AuditExportDraft>();
  const [tls, setTls] = useState<boolean>();
  const [revealSecret, setRevealSecret] = useState(false);
  const [errors, setErrors] = useState<AuditExportErrors>({});
  const [retentionDays, setRetentionDays] = useState(saved.configuration.retention_days);
  if (saved.configuration.retention_days !== retentionDays) {
    // Telemetry retention saves the same period. Follow it so this form never sends the old value back.
    setRetentionDays(saved.configuration.retention_days);
    if (draft) setDraft({ ...draft, retention_days: String(saved.configuration.retention_days) });
  }
  const value = draft ?? initial;
  const syslogTls = tls ?? saved.configuration.syslog_tls;
  const dirty = draft !== undefined || tls !== undefined;
  function discard() {
    setDraft(undefined);
    setTls(undefined);
    setErrors({});
    setRevealSecret(false);
  }
  function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    const problems = validateAuditExport(value, saved.webhook_secret_configured);
    setErrors(problems);
    if (Object.keys(problems).length > 0) return;
    update.mutate(
      {
        webhook_url: value.webhook_url.trim(),
        syslog_address: value.syslog_address.trim(),
        syslog_tls: syslogTls,
        retention_days: Number(value.retention_days),
        ...(value.webhook_secret !== "" ? { webhook_secret: value.webhook_secret } : {}),
      },
      { onSuccess: discard },
    );
  }
  const text = (key: keyof AuditExportDraft, label: string, hint: string, extra: { placeholder?: string; type?: string; mono?: boolean } = {}) => (
    <Field id={`${id}-${key}`} layout="row" label={label} hint={hint} error={errors[key]}>
      <input
        id={`${id}-${key}`}
        className={`${inputClass} ${extra.mono ? "font-mono" : ""}`}
        type={extra.type ?? "text"}
        autoComplete="off"
        spellCheck={false}
        placeholder={extra.placeholder}
        value={value[key]}
        onChange={(event) => setDraft({ ...value, [key]: event.target.value })}
        aria-invalid={errors[key] ? true : undefined}
        aria-describedby={describedBy(`${id}-${key}`, errors[key], hint)}
      />
    </Field>
  );
  const pending = saved.pending_deliveries;
  return (
    <form onSubmit={submit} noValidate>
      <UnsavedChangesGuard dirty={dirty} />
      <Section
        title="Audit export"
        description="Stream every audit event to a webhook, a syslog collector, or both. Changes apply to new events only."
        width="form"
        footer={<SaveBar dirty={dirty} pending={update.isPending} blocked={!online} onDiscard={discard} status={update.isSuccess ? "Audit export saved" : undefined} />}
      >
        <fieldset disabled={!online || update.isPending} className="space-y-4">
          <SettingRows>
            <SettingRow label="Delivery backlog" description="Queued events retry with backoff until a destination accepts them. Queued events keep the destination and secret they were queued with; there is no replay or purge.">
              <StatusLabel tone={pending > 0 ? "warn" : "ok"} label={pending === 0 ? "Nothing waiting" : `${pending} ${pending === 1 ? "delivery" : "deliveries"} waiting`} />
            </SettingRow>
            {text("webhook_url", "Webhook URL", "HTTPS. Leave empty to turn the webhook off. Only 2xx responses count as delivered.", { type: "url", placeholder: "https://siem.example.com/cove", mono: true })}
            <Field
              id={`${id}-webhook_secret`}
              layout="row"
              label="Signing secret"
              optional={saved.webhook_secret_configured}
              hint={`${saved.webhook_secret_configured ? "A secret is saved. Leave blank to keep it. " : ""}At least 32 bytes. Requests carry X-Cove-Timestamp (Unix seconds), X-Cove-Signature: sha256=<HMAC of the timestamp, a dot and the body> and a stable X-Cove-Delivery ID to deduplicate.`}
              error={errors.webhook_secret}
            >
              <div className="space-y-2">
                <input
                  id={`${id}-webhook_secret`}
                  className={`${inputClass} font-mono`}
                  type={revealSecret ? "text" : "password"}
                  autoComplete="new-password"
                  spellCheck={false}
                  value={value.webhook_secret}
                  onChange={(event) => setDraft({ ...value, webhook_secret: event.target.value })}
                  aria-invalid={errors.webhook_secret ? true : undefined}
                  aria-describedby={describedBy(`${id}-webhook_secret`, errors.webhook_secret, true)}
                />
                <div className="flex flex-wrap items-center gap-2">
                  <Button
                    size="sm"
                    onClick={() => {
                      setDraft({ ...value, webhook_secret: generateSecret() });
                      setRevealSecret(true);
                    }}
                  >
                    Generate secret
                  </Button>
                  {revealSecret && value.webhook_secret !== "" && <CopyButton text={value.webhook_secret} label="Copy secret" variant="ghost" />}
                </div>
              </div>
            </Field>
            {text("syslog_address", "Syslog collector", "host:port over TCP, RFC 5424 with octet-counting framing. Leave empty to turn syslog off.", { placeholder: "logs.example.com:6514", mono: true })}
            <SwitchField
              id={`${id}-syslog-tls`}
              layout="row"
              label="Use TLS for syslog"
              description="Verifies the collector's certificate and requires TLS 1.2 or later. A TCP write being accepted does not prove the collector stored the event."
              checked={syslogTls}
              onChange={(event) => setTls(event.target.checked)}
            />
            {text("retention_days", "Keep audit events for", "Days, from 1 to 3650. The same setting as Audit log in Telemetry retention; saving either changes both. Events still waiting for delivery are kept until delivered, so a backlog can grow stored audit data.", { type: "number" })}
          </SettingRows>
          {update.isError && <Notice tone="error" title="Could not save audit export">{errorMessage(update.error)}</Notice>}
        </fieldset>
      </Section>
    </form>
  );
}

export function AuditExport() {
  const settings = useQuery(auditExportQuery);
  if (settings.isPending) {
    return (
      <Section title="Audit export" width="form">
        <LoadingRows label="Loading audit export settings" rows={3} />
      </Section>
    );
  }
  if (settings.isError) {
    return (
      <Section title="Audit export" width="form">
        <QueryError what="audit export settings" error={settings.error} onRetry={() => void settings.refetch()} retrying={settings.isFetching} />
      </Section>
    );
  }
  return <AuditExportForm saved={settings.data} />;
}

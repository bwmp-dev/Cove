import { useQuery } from "@tanstack/react-query";
import { Plus } from "lucide-react";
import { useId, useState, type FormEvent, type ReactNode } from "react";
import { errorMessage, isStatus } from "../api/client";
import { ldapProvidersQuery, useDeleteLdapProvider, useSaveLdapProvider, useTestLdapProvider, type LdapProvider } from "../api/directory";
import {
  emptyLdapConfiguration,
  fromMappingRows,
  hasMappingErrors,
  ldapConfigurationFromDraft,
  ldapDraft,
  ldapPorts,
  plural,
  toMappingRows,
  validateLdap,
  validateMappings,
  type LdapDraft,
  type LdapErrors,
  type MappingRow,
} from "../lib/directory";
import { useOnline, useReviewed } from "../lib/hooks";
import { choiceClass, describedBy, inputClass } from "../lib/styles";
import { ConfirmDialog } from "./ConfirmDialog";
import { MappingEditor, MappingList } from "./DirectoryMappings";
import { Select } from "./Select";
import { StatusLabel } from "./status";
import { Button, Field, LoadingRows, MetaList, Notice, QueryError, Section, SwitchField } from "./ui";

const maxProviders = 8;

type TextKey = "display_name" | "host" | "port" | "bind_dn" | "user_base_dn" | "user_filter" | "email_attribute" | "name_attribute" | "group_base_dn" | "group_filter";

function Group({ title, description, children }: { title: string; description?: string; children: ReactNode }) {
  return (
    <fieldset className="min-w-0 space-y-3 border-t border-line pt-4 first:border-t-0 first:pt-0">
      <legend className="float-left w-full">
        <span className="block text-[13px] font-semibold text-fg">{title}</span>
        {description && <span className="mt-0.5 block text-xs font-normal text-fg-muted">{description}</span>}
      </legend>
      <div className="clear-both space-y-4 pt-1">{children}</div>
    </fieldset>
  );
}

function DisableDialog({ name, pending, onConfirm, onCancel }: { name: string; pending: boolean; onConfirm: () => void; onCancel: () => void }) {
  const reviewed = useReviewed(name);
  return (
    <ConfirmDialog open title={`Turn off sign-in with ${reviewed}?`} confirmLabel="Turn off and save" danger pending={pending} onCancel={onCancel} onConfirm={onConfirm}>
      <p>Nobody can sign in with this directory until it is turned on again. People who have no other sign-in method are locked out meanwhile; their accounts and team access stay as they are.</p>
    </ConfirmDialog>
  );
}

function ProviderEditor({ provider, onSaved, onClose }: { provider?: LdapProvider; onSaved: () => void; onClose: () => void }) {
  const id = useId();
  const online = useOnline();
  const baseVersion = useReviewed(provider?.version);
  const [draft, setDraft] = useState<LdapDraft>(() => ldapDraft(provider?.configuration ?? emptyLdapConfiguration()));
  const [enabled, setEnabled] = useState(provider?.enabled ?? false);
  const [rows, setRows] = useState<MappingRow[]>(() => toMappingRows(provider?.mappings));
  const save = useSaveLdapProvider(() => ({
    id: provider?.id,
    input: { configuration: ldapConfigurationFromDraft(draft), enabled, ...(baseVersion !== undefined ? { version: baseVersion } : {}), mappings: fromMappingRows(rows) },
  }));
  const [errors, setErrors] = useState<LdapErrors>({});
  const [mappingErrors, setMappingErrors] = useState<ReturnType<typeof validateMappings>>({ rows: {} });
  const [confirmingDisable, setConfirmingDisable] = useState(false);
  const passwordSaved = provider?.password_configured ?? false;
  const stale = isStatus(save.error, 409);

  function send() {
    save.mutate(undefined, {
      onSuccess: onSaved,
      onSettled: () => {
        setDraft((current) => ({ ...current, bind_password: "" }));
        setConfirmingDisable(false);
      },
    });
  }

  function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    const problems = validateLdap(draft, passwordSaved);
    const mappingProblems = validateMappings(rows, "ldap");
    setErrors(problems);
    setMappingErrors(mappingProblems);
    if (Object.keys(problems).length > 0 || hasMappingErrors(mappingProblems)) return;
    if (provider?.enabled && !enabled) setConfirmingDisable(true);
    else send();
  }

  const text = (key: TextKey, label: string, hint?: string, extra: { placeholder?: string; mono?: boolean; type?: string; inputMode?: "numeric" } = {}) => (
    <Field id={`${id}-${key}`} label={label} hint={hint} error={errors[key]}>
      <input
        id={`${id}-${key}`}
        className={`${inputClass} ${extra.mono ? "font-mono" : ""}`}
        type={extra.type ?? "text"}
        inputMode={extra.inputMode}
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
    <form className="space-y-4 border-t border-line bg-panel p-4" noValidate onSubmit={submit} aria-label={provider ? `Edit ${provider.configuration.display_name}` : "New LDAP directory"}>
      <Group title="Directory">
        <div className="grid gap-4 sm:grid-cols-2">{text("display_name", "Display name", "Shown on the sign-in page.")}</div>
        <SwitchField id={`${id}-enabled`} label="Allow sign-in with this directory" description="Save and test the connection before turning this on." checked={enabled} onChange={(event) => setEnabled(event.target.checked)} />
      </Group>
      <Group title="Connection" description="Cove always verifies the server's TLS certificate against the host name below. There is no unencrypted mode.">
        <div className="grid gap-4 sm:grid-cols-[minmax(0,3fr)_minmax(0,1fr)]">
          {text("host", "Host name", "Without ldaps:// or a port.", { placeholder: "ldap.example.com", mono: true })}
          {text("port", "Port", undefined, { inputMode: "numeric", mono: true })}
        </div>
        <fieldset>
          <legend className="mb-1.5 text-[13px] font-medium text-fg">TLS</legend>
          <div className="grid gap-2 sm:grid-cols-2">
            {(["ldaps", "starttls"] as const).map((transport) => (
              <label key={transport} className={choiceClass}>
                <input
                  type="radio"
                  name={`${id}-transport`}
                  value={transport}
                  checked={draft.transport === transport}
                  className="accent-info"
                  onChange={() => {
                    const other = transport === "ldaps" ? ldapPorts.starttls : ldapPorts.ldaps;
                    setDraft({ ...draft, transport, port: draft.port === String(other) ? String(ldapPorts[transport]) : draft.port });
                  }}
                />
                {transport === "ldaps" ? "LDAPS (TLS from the start, usually port 636)" : "StartTLS (upgrade, usually port 389)"}
              </label>
            ))}
          </div>
        </fieldset>
        <Field id={`${id}-ca`} label="CA certificates" optional hint="PEM certificates to trust in addition to the system authorities, for a private CA. Certificates only, never a private key." error={errors.ca_pem}>
          <textarea
            id={`${id}-ca`}
            className={`${inputClass} h-28 py-2 font-mono text-xs`}
            spellCheck={false}
            placeholder="-----BEGIN CERTIFICATE-----"
            value={draft.ca_pem}
            onChange={(event) => setDraft({ ...draft, ca_pem: event.target.value })}
            aria-invalid={errors.ca_pem ? true : undefined}
            aria-describedby={describedBy(`${id}-ca`, errors.ca_pem, true)}
          />
        </Field>
      </Group>
      <Group title="Service account" description="Cove binds as this account to search for users and groups.">
        <div className="grid gap-4 sm:grid-cols-2">
          {text("bind_dn", "Bind DN", undefined, { placeholder: "cn=cove,ou=services,dc=example,dc=com", mono: true })}
          <Field id={`${id}-bind-password`} label="Bind password" optional={passwordSaved} hint={passwordSaved ? "A password is saved. Leave blank to keep it." : "Encrypted and never shown again."} error={errors.bind_password}>
            <input
              id={`${id}-bind-password`}
              className={inputClass}
              type="password"
              autoComplete="new-password"
              value={draft.bind_password}
              onChange={(event) => setDraft({ ...draft, bind_password: event.target.value })}
              aria-invalid={errors.bind_password ? true : undefined}
              aria-describedby={describedBy(`${id}-bind-password`, errors.bind_password, true)}
            />
          </Field>
        </div>
      </Group>
      <Group title="Users" description="The suggested values suit OpenLDAP. Active Directory usually uses objectGUID and a filter on sAMAccountName.">
        {text("user_base_dn", "User search base", undefined, { placeholder: "ou=people,dc=example,dc=com", mono: true })}
        {text("user_filter", "User filter", "{username} is replaced by the escaped directory username typed on the sign-in page.", { mono: true })}
        <div className="grid gap-4 sm:grid-cols-3">
          <Field id={`${id}-subject`} label="Immutable ID attribute" hint="Identifies the person even if their name or email changes.">
            <Select id={`${id}-subject`} className={inputClass} value={draft.subject_attribute} aria-describedby={`${id}-subject-hint`} onChange={(event) => setDraft({ ...draft, subject_attribute: event.target.value === "objectGUID" ? "objectGUID" : "entryUUID" })}>
              <option value="entryUUID">entryUUID (OpenLDAP)</option>
              <option value="objectGUID">objectGUID (Active Directory)</option>
            </Select>
          </Field>
          {text("email_attribute", "Email attribute", undefined, { mono: true })}
          {text("name_attribute", "Name attribute", undefined, { mono: true })}
        </div>
      </Group>
      <Group title="Groups">
        {text("group_base_dn", "Group search base", undefined, { placeholder: "ou=groups,dc=example,dc=com", mono: true })}
        {text("group_filter", "Group filter", "{dn} is replaced by the user's escaped DN and {username} by the escaped directory username.", { mono: true })}
      </Group>
      <Group title="Accounts and team access">
        <SwitchField
          id={`${id}-registration`}
          label="Create accounts for new directory users"
          description="Off: only people who already linked this directory to their account can sign in. An existing Cove account with the same email is never linked automatically."
          checked={draft.allow_registration}
          onChange={(event) => setDraft({ ...draft, allow_registration: event.target.checked })}
        />
        <div className="space-y-2">
          <h4 className="text-[13px] font-semibold text-fg">Group mappings</h4>
          <p className="text-xs text-fg-muted">
            Changes take effect the next time each person signs in with this directory; Cove does not poll it. To revoke access immediately, disable the account or use SCIM.
          </p>
          <MappingEditor id={`${id}-mappings`} kind="ldap" rows={rows} onChange={setRows} errors={mappingErrors.rows} listError={mappingErrors.list} />
        </div>
      </Group>
      {save.isError && (
        <Notice tone={stale ? "warn" : "error"} title={stale ? "Settings changed or limit reached" : "Could not save the directory"}>
          {errorMessage(save.error)}
          {stale && provider && " Cancel, review the current settings and edit again."}
        </Notice>
      )}
      <div className="flex flex-wrap justify-end gap-2">
        <Button variant="ghost" onClick={onClose} disabled={save.isPending}>
          Cancel
        </Button>
        <Button type="submit" variant="primary" disabled={!online} pending={save.isPending}>
          {provider ? "Save directory" : "Add directory"}
        </Button>
      </div>
      {confirmingDisable && <DisableDialog name={provider?.configuration.display_name ?? draft.display_name} pending={save.isPending} onCancel={() => setConfirmingDisable(false)} onConfirm={send} />}
    </form>
  );
}

function DeleteDialog({ provider, pending, onConfirm, onCancel }: { provider: LdapProvider; pending: boolean; onConfirm: (version: number) => void; onCancel: () => void }) {
  const reviewed = useReviewed({ version: provider.version, name: provider.configuration.display_name, mappings: (provider.mappings ?? []).length });
  return (
    <ConfirmDialog open title={`Delete ${reviewed.name}?`} confirmLabel="Delete directory" danger pending={pending} onCancel={onCancel} onConfirm={() => onConfirm(reviewed.version)}>
      <p>Its settings, bind password and {plural(reviewed.mappings, "group mapping")} are removed, along with the team access they granted.</p>
      <p className="mt-2">Cove refuses while any account still has this directory linked. Turn sign-in off instead if you only need to pause it.</p>
    </ConfirmDialog>
  );
}

function ProviderSummary({ provider }: { provider: LdapProvider }) {
  const { configuration } = provider;
  return (
    <div className="min-w-0 flex-1 basis-72 space-y-1">
      <div className="flex flex-wrap items-center gap-x-3 gap-y-1">
        <p className="text-[13px] font-medium break-words text-fg">{configuration.display_name}</p>
        <StatusLabel tone={provider.enabled ? "ok" : "neutral"} label={provider.enabled ? "Sign-in on" : "Sign-in off"} />
      </div>
      <MetaList className="text-xs text-fg-muted">
        <span className="font-mono break-all">
          {configuration.host}:{configuration.port}
        </span>
        <span>{configuration.transport === "ldaps" ? "LDAPS" : "StartTLS"}</span>
        {configuration.ca_pem !== "" && <span>Custom CA</span>}
        <span>{configuration.allow_registration ? "Creates accounts" : "Linked accounts only"}</span>
        <span>{provider.password_configured ? "Bind password saved" : "No bind password"}</span>
      </MetaList>
      <div className="pt-1">
        <MappingList mappings={provider.mappings ?? []} />
      </div>
    </div>
  );
}

function ProviderNotices({ tested, testError, removeError }: { tested: boolean; testError: unknown; removeError: unknown }) {
  return (
    <div className="space-y-2 px-4 pb-3">
      {tested && (
        <Notice tone="success" title="Connected">
          The TLS certificate was verified and the service account bind succeeded, using the saved settings. This does not check the user or group filters or anyone's password.
        </Notice>
      )}
      {testError !== null && <Notice tone="error" title="Connection test failed">{errorMessage(testError)}</Notice>}
      {removeError !== null && (
        <Notice tone="error" title="Could not delete the directory">
          {errorMessage(removeError)}
          {isStatus(removeError, 409) && " Turn sign-in off to pause it. To delete it, people must link another sign-in method and unlink this directory from their account first."}
        </Notice>
      )}
    </div>
  );
}

function ProviderRow({ provider, editing, onEdit, onClose, onSaved }: { provider: LdapProvider; editing: boolean; onEdit: () => void; onClose: () => void; onSaved: () => void }) {
  const online = useOnline();
  const test = useTestLdapProvider();
  const remove = useDeleteLdapProvider();
  const [deleting, setDeleting] = useState(false);
  const name = provider.configuration.display_name;
  return (
    <li>
      <div className="flex flex-wrap items-start justify-between gap-x-6 gap-y-2 px-4 py-3">
        <ProviderSummary provider={provider} />
        <div className="flex flex-wrap items-center gap-2">
          <Button variant="ghost" size="sm" disabled={!online} pending={test.isPending} onClick={() => test.mutate(provider.id)}>
            Test connection<span className="sr-only"> to {name}</span>
          </Button>
          <Button variant="ghost" size="sm" disabled={!online || editing} onClick={onEdit}>
            Edit<span className="sr-only"> {name}</span>
          </Button>
          <Button variant="ghost" size="sm" disabled={!online} pending={remove.isPending} onClick={() => { remove.reset(); setDeleting(true); }}>
            Delete<span className="sr-only"> {name}</span>
          </Button>
        </div>
      </div>
      {(test.isSuccess || test.isError || remove.isError) && <ProviderNotices tested={test.isSuccess} testError={test.isError ? test.error : null} removeError={remove.isError ? remove.error : null} />}
      {editing && <ProviderEditor provider={provider} onSaved={onSaved} onClose={onClose} />}
      {deleting && <DeleteDialog provider={provider} pending={remove.isPending} onCancel={() => setDeleting(false)} onConfirm={(version) => remove.mutate({ id: provider.id, version }, { onSettled: () => setDeleting(false) })} />}
    </li>
  );
}

export function LdapProviders() {
  const online = useOnline();
  const settings = useQuery(ldapProvidersQuery);
  const [editing, setEditing] = useState<string | null>(null);
  const [saved, setSaved] = useState(false);
  const providers = settings.data?.providers ?? [];
  return (
    <Section
      title="LDAP directories"
      description="Let people sign in with their directory username and password. Two-step verification, lockout and explicit account linking work as for other sign-in methods."
      flush
      actions={
        editing !== "new" && (
          <Button disabled={!online || !settings.isSuccess || providers.length >= maxProviders} title={providers.length >= maxProviders ? `At most ${maxProviders} directories` : undefined} onClick={() => { setSaved(false); setEditing("new"); }}>
            <Plus aria-hidden className="size-3.5" />
            Add directory
          </Button>
        )
      }
    >
      {settings.isPending ? (
        <LoadingRows label="Loading LDAP directories" rows={2} />
      ) : settings.isError ? (
        <div className="p-3">
          <QueryError what="LDAP directories" error={settings.error} onRetry={() => void settings.refetch()} retrying={settings.isFetching} />
        </div>
      ) : (
        <>
          {(!settings.data.network_policy_configured || saved) && (
            <div className="space-y-2 p-3">
              {!settings.data.network_policy_configured && (
                <Notice tone="warn" title="The host does not allow directory connections">
                  Set <span className="font-mono">COVE_LDAP_ALLOWED_CIDRS</span> in the host's <span className="font-mono">.env</span> to your directory servers' address ranges and restart Cove. Until then, connection tests and directory sign-in fail. This cannot be changed from the dashboard.
                </Notice>
              )}
              {saved && <Notice tone="success" title="Directory saved">Test the connection to check the saved settings.</Notice>}
            </div>
          )}
          {editing === "new" && <ProviderEditor onSaved={() => { setSaved(true); setEditing(null); }} onClose={() => setEditing(null)} />}
          {providers.length === 0 && editing !== "new" && <p className="px-4 py-3 text-[13px] text-fg-muted">No LDAP directories.</p>}
          <ul className="divide-y divide-line">
            {providers.map((provider) => (
              <ProviderRow
                key={provider.id}
                provider={provider}
                editing={editing === provider.id}
                onEdit={() => { setSaved(false); setEditing(provider.id); }}
                onClose={() => setEditing(null)}
                onSaved={() => { setSaved(true); setEditing(null); }}
              />
            ))}
          </ul>
        </>
      )}
    </Section>
  );
}

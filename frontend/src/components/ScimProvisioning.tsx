import { useQuery } from "@tanstack/react-query";
import { useId, useState, type FormEvent } from "react";
import { errorMessage, isStatus } from "../api/client";
import {
  scimActivityQuery,
  scimGroupsQuery,
  scimLinksQuery,
  scimSettingsQuery,
  useAdoptScimAccount,
  useDetachScimAccount,
  useGenerateScimToken,
  useSaveScimGroupMappings,
  useUpdateScimSettings,
  type ScimActivity,
  type ScimGroup,
  type ScimLink,
  type ScimSettings,
} from "../api/directory";
import { oidcProvidersQuery, type OidcProvider } from "../api/instance-settings";
import { usersQuery } from "../api/teams";
import { fromMappingRows, hasMappingErrors, plural, toMappingRows, validateMappings, type MappingRow } from "../lib/directory";
import { useOnline, useReviewed } from "../lib/hooks";
import { describedBy, inputClass } from "../lib/styles";
import { ConfirmDialog } from "./ConfirmDialog";
import { CopyButton } from "./CopyButton";
import { MappingEditor, MappingList } from "./DirectoryMappings";
import { RelativeTime } from "./RelativeTime";
import { SaveBar } from "./SaveBar";
import { Select } from "./Select";
import { StatusLabel } from "./status";
import { Button, Field, LoadingRows, MetaList, Notice, QueryError, Section, SettingRow, SettingRows, Subsection, SwitchField } from "./ui";

function providerName(providers: readonly OidcProvider[] | undefined, id: string) {
  if (id === "") return "None";
  return providers?.find((provider) => provider.id === id)?.configuration.display_name ?? id;
}

function TokenReveal({ token, onDone }: { token: string; onDone: () => void }) {
  return (
    <div className="space-y-3 rounded-md border border-warn-line bg-warn-tint p-3">
      <Field id="scim-token" label="New SCIM bearer token" hint="Copy it into your identity provider now. Cove stores it sealed and cannot show it again.">
        <input id="scim-token" className={`${inputClass} font-mono`} readOnly value={token} autoComplete="off" spellCheck={false} aria-describedby="scim-token-hint" onFocus={(event) => event.currentTarget.select()} />
      </Field>
      <div className="flex flex-wrap items-center gap-2">
        <CopyButton text={token} label="Copy SCIM token" />
        <Button variant="ghost" onClick={onDone}>
          I have saved the token
        </Button>
      </div>
    </div>
  );
}

function RotateDialog({ settings, pending, onConfirm, onCancel }: { settings: ScimSettings; pending: boolean; onConfirm: (version: number) => void; onCancel: () => void }) {
  const reviewed = useReviewed({ version: settings.version, enabled: settings.enabled });
  return (
    <ConfirmDialog open title="Rotate the SCIM token?" confirmLabel="Rotate token" danger pending={pending} onCancel={onCancel} onConfirm={() => onConfirm(reviewed.version)}>
      <p>The current token stops working immediately, including requests your identity provider has already started but Cove has not finished.</p>
      {reviewed.enabled && <p className="mt-2">Provisioning fails until you paste the new token into your identity provider.</p>}
    </ConfirmDialog>
  );
}

function Connection({ settings }: { settings: ScimSettings }) {
  const online = useOnline();
  const generate = useGenerateScimToken();
  const [rotating, setRotating] = useState(false);
  const token = generate.data?.token;
  return (
    <Subsection title="Connection" description="Give your identity provider this base URL and a bearer token. SCIM provisions accounts; people still sign in with a password, passkey or single sign-on.">
      <SettingRows>
        <SettingRow label="Status">
          <StatusLabel tone={settings.enabled ? "ok" : "neutral"} label={settings.enabled ? "Accepting provisioning requests" : settings.token_configured ? "Turned off" : "Not set up"} />
        </SettingRow>
        <SettingRow label="SCIM base URL" description="The panel access allowlist also applies to SCIM, so allow your identity provider's addresses.">
          <span className="font-mono text-xs break-all">{settings.base_url}</span>
          <CopyButton text={settings.base_url} label="Copy URL" variant="ghost" />
        </SettingRow>
        <SettingRow label="Bearer token" description={settings.token_configured ? <>Generated {settings.rotated_at ? <RelativeTime value={settings.rotated_at} /> : "earlier"}. It cannot be shown again; rotate it to issue a new one.</> : "Generating a token does not turn provisioning on."}>
          <StatusLabel tone={settings.token_configured ? "ok" : "neutral"} label={settings.token_configured ? "Configured" : "Not generated"} />
          <Button
            disabled={!online || token !== undefined}
            pending={generate.isPending}
            onClick={() => {
              if (settings.token_configured) setRotating(true);
              else generate.mutate(settings.version);
            }}
          >
            {settings.token_configured ? "Rotate token" : "Generate token"}
          </Button>
        </SettingRow>
      </SettingRows>
      {token && <TokenReveal token={token} onDone={() => generate.reset()} />}
      {generate.isError && (
        <Notice tone="error" title="Could not generate a token">
          {errorMessage(generate.error)}
          {isStatus(generate.error, 409) && " The settings changed; review them and try again."}
        </Notice>
      )}
      {rotating && <RotateDialog settings={settings} pending={generate.isPending} onCancel={() => setRotating(false)} onConfirm={(version) => generate.mutate(version, { onSettled: () => setRotating(false) })} />}
    </Subsection>
  );
}

function ProviderSelect({ providers, value, onChange }: { providers: OidcProvider[]; value: string; onChange: (id: string) => void }) {
  const missing = value !== "" && !providers.some((provider) => provider.id === value);
  return (
    <Select id="scim-provider" className={inputClass} value={value} aria-describedby="scim-provider-hint" onChange={(event) => onChange(event.target.value)}>
      <option value="">None: no sign-in identity</option>
      {providers.map((provider) => (
        <option key={provider.id} value={provider.id}>
          {provider.configuration.display_name}
        </option>
      ))}
      {missing && <option value={value}>Removed provider</option>}
    </Select>
  );
}

function PolicySaveError({ error, onReload }: { error: unknown; onReload: () => void }) {
  const stale = isStatus(error, 409);
  return (
    <Notice
      tone={stale ? "warn" : "error"}
      title={stale ? "Settings changed" : "Could not save provisioning settings"}
      action={
        stale && (
          <Button size="sm" variant="ghost" onClick={onReload}>
            Reload settings
          </Button>
        )
      }
    >
      {stale ? "Someone changed these settings or rotated the token since you loaded them. Reload, review the current values and save again." : errorMessage(error)}
    </Notice>
  );
}

function Policy({ settings, providers }: { settings: ScimSettings; providers: OidcProvider[] }) {
  const online = useOnline();
  const update = useUpdateScimSettings();
  const settingsQuery = useQuery(scimSettingsQuery);
  // The draft keeps the revision it started from, so a save after someone else's change is refused instead of overwriting it.
  const [draft, setDraft] = useState<{ version: number; enabled: boolean; oidc_provider_id: string }>();
  const value = draft ?? { version: settings.version, enabled: settings.enabled, oidc_provider_id: settings.oidc_provider_id };
  const dirty = draft !== undefined && (draft.enabled !== settings.enabled || draft.oidc_provider_id !== settings.oidc_provider_id);
  function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    update.mutate(value, { onSuccess: () => setDraft(undefined) });
  }
  return (
    <form onSubmit={submit} noValidate>
      <Subsection title="Provisioning">
        <fieldset disabled={!online || update.isPending} className="space-y-4">
          <SettingRows>
            <SwitchField
              id="scim-enabled"
              layout="row"
              label="Accept provisioning requests"
              description={settings.token_configured ? "Turning this off pauses provisioning. Users, groups and team access stay as they are." : "Generate a token first."}
              checked={value.enabled}
              disabled={!settings.token_configured && !value.enabled}
              onChange={(event) => setDraft({ ...value, enabled: event.target.checked })}
            />
            <Field
              id="scim-provider"
              layout="row"
              label="Bind accounts to identity provider"
              hint="New and adopted accounts get this provider's sign-in identity, using the SCIM externalId as the exact OIDC subject. Without a provider, provisioned people sign in through an invitation or by linking a sign-in method themselves."
            >
              <ProviderSelect providers={providers} value={value.oidc_provider_id} onChange={(oidc_provider_id) => setDraft({ ...value, oidc_provider_id })} />
            </Field>
          </SettingRows>
          {value.oidc_provider_id !== "" && (
            <Notice tone="warn" title="externalId must be the OIDC subject">
              Configure your identity provider to send the user's OIDC subject claim (sub) as externalId. An object ID or email address is not necessarily the subject. Accounts provisioned earlier stay bound to their original provider until detached.
            </Notice>
          )}
          {update.isError && <PolicySaveError error={update.error} onReload={() => { setDraft(undefined); update.reset(); void settingsQuery.refetch(); }} />}
          <div className="border-t border-line pt-4">
            <SaveBar dirty={dirty} pending={update.isPending} blocked={!online} onDiscard={() => { setDraft(undefined); update.reset(); }} status={update.isSuccess ? "Provisioning settings saved" : undefined} />
          </div>
        </fieldset>
      </Subsection>
    </form>
  );
}

function GroupMappingDialog({ group, rows, pending, onConfirm, onCancel }: { group: ScimGroup; rows: MappingRow[]; pending: boolean; onConfirm: (version: number, rows: MappingRow[]) => void; onCancel: () => void }) {
  const reviewed = useReviewed({ version: group.version, rows, name: group.display_name, members: group.members });
  return (
    <ConfirmDialog open title={`Save mappings for ${reviewed.name}?`} confirmLabel="Save mappings" pending={pending} onCancel={onCancel} onConfirm={() => onConfirm(reviewed.version, reviewed.rows)}>
      <p>
        This replaces the mappings of this group only. Its {plural(reviewed.members, "member")} gain or lose team access right away to match; access granted by hand or by other sources is kept.
      </p>
      <div className="mt-2">
        <MappingList mappings={fromMappingRows(reviewed.rows)} showGroup={false} />
      </div>
    </ConfirmDialog>
  );
}

function GroupEditor({ group, onClose }: { group: ScimGroup; onClose: () => void }) {
  const id = useId();
  const online = useOnline();
  const save = useSaveScimGroupMappings();
  const [rows, setRows] = useState<MappingRow[]>(() => toMappingRows(group.mappings));
  const [errors, setErrors] = useState<ReturnType<typeof validateMappings>>({ rows: {} });
  const [confirming, setConfirming] = useState(false);
  const stale = isStatus(save.error, 409);
  return (
    <form
      className="space-y-4 border-t border-line bg-panel p-4"
      noValidate
      aria-label={`Mappings for ${group.display_name}`}
      onSubmit={(event) => {
        event.preventDefault();
        const problems = validateMappings(rows, "scim");
        setErrors(problems);
        if (!hasMappingErrors(problems)) setConfirming(true);
      }}
    >
      <MappingEditor id={id} kind="scim" rows={rows} onChange={setRows} errors={errors.rows} listError={errors.list} fixedGroup={group.id} />
      {save.isError && (
        <Notice tone={stale ? "warn" : "error"} title={stale ? "This group changed" : "Could not save the mappings"}>
          {stale ? "Another save or an update from your identity provider changed this group since you opened it. Close the editor, review the current mappings and edit again." : errorMessage(save.error)}
        </Notice>
      )}
      <div className="flex flex-wrap justify-end gap-2">
        <Button variant="ghost" onClick={onClose} disabled={save.isPending}>
          Cancel
        </Button>
        <Button type="submit" variant="primary" disabled={!online || stale} pending={save.isPending}>
          Review and save
        </Button>
      </div>
      {confirming && (
        <GroupMappingDialog
          group={group}
          rows={rows}
          pending={save.isPending}
          onCancel={() => setConfirming(false)}
          onConfirm={(version, reviewedRows) =>
            save.mutate(
              { group: group.id, group_version: version, mappings: fromMappingRows(reviewedRows).map(({ team_id, role }) => ({ team_id, role })) },
              { onSuccess: onClose, onSettled: () => setConfirming(false) },
            )
          }
        />
      )}
    </form>
  );
}

function Groups() {
  const online = useOnline();
  const groups = useQuery(scimGroupsQuery);
  const [editing, setEditing] = useState<string | null>(null);
  return (
    <Subsection title="Groups" description="Groups your identity provider has provisioned. Map a group to Cove teams to give its members access.">
      {groups.isPending ? (
        <LoadingRows label="Loading provisioned groups" rows={2} />
      ) : groups.isError ? (
        <QueryError what="provisioned groups" error={groups.error} onRetry={() => void groups.refetch()} retrying={groups.isFetching} />
      ) : groups.data.length === 0 ? (
        <p className="text-[13px] text-fg-muted">No groups yet. Groups appear here after your identity provider pushes them to Cove.</p>
      ) : (
        <ul aria-label="Provisioned groups" className="divide-y divide-line rounded-md border border-line">
          {groups.data.map((group) => (
            <li key={group.id}>
              <div className="flex flex-wrap items-start justify-between gap-x-6 gap-y-2 px-3 py-2.5">
                <div className="min-w-0 flex-1 basis-64 space-y-1">
                  <p className="text-[13px] font-medium break-words text-fg">{group.display_name}</p>
                  <MetaList className="text-xs text-fg-muted">
                    <span>{plural(group.members, "member")}</span>
                    {group.external_id && <span className="font-mono break-all">{group.external_id}</span>}
                  </MetaList>
                  <MappingList mappings={group.mappings ?? []} showGroup={false} />
                </div>
                <Button variant="ghost" size="sm" disabled={!online || editing === group.id} onClick={() => setEditing(group.id)}>
                  Edit mappings<span className="sr-only"> for {group.display_name}</span>
                </Button>
              </div>
              {editing === group.id && <GroupEditor group={group} onClose={() => setEditing(null)} />}
            </li>
          ))}
        </ul>
      )}
    </Subsection>
  );
}

function linkState(link: ScimLink): { tone: "ok" | "warn" | "neutral"; label: string } {
  if (link.deleted_at) return { tone: "neutral", label: "Deleted by identity provider" };
  if (!link.active) return { tone: "warn", label: "Disabled, sessions and tokens revoked" };
  return { tone: "ok", label: "Active" };
}

function DetachDialog({ link, pending, onConfirm, onCancel }: { link: ScimLink; pending: boolean; onConfirm: () => void; onCancel: () => void }) {
  const reviewed = useReviewed(link);
  return (
    <ConfirmDialog open title={`Stop provisioning ${reviewed.user_name}?`} confirmLabel="Detach account" danger pending={pending} onCancel={onCancel} onConfirm={onConfirm}>
      <p>Cove removes the team access and group memberships SCIM gave this account. The account and its sign-in identity stay{reviewed.active ? "" : ", and it stays disabled: re-enable it from Users if needed"}.</p>
      <p className="mt-2">If your identity provider provisions the same person again, an administrator must adopt the account explicitly.</p>
    </ConfirmDialog>
  );
}

function AdoptDialog({ email, externalId, pending, onConfirm, onCancel }: { email: string; externalId: string; pending: boolean; onConfirm: () => void; onCancel: () => void }) {
  const reviewed = useReviewed({ email, externalId });
  return (
    <ConfirmDialog open title={`Let SCIM manage ${reviewed.email}?`} confirmLabel="Adopt account" danger pending={pending} onCancel={onCancel} onConfirm={onConfirm}>
      <p>
        Your identity provider can then change this account's profile and group access, and disable it, which signs it out everywhere and revokes its API tokens. It is bound to externalId <span className="font-mono break-all text-fg">{reviewed.externalId}</span>.
      </p>
    </ConfirmDialog>
  );
}

function AdoptForm({ links }: { links: ScimLink[] }) {
  const id = useId();
  const online = useOnline();
  const users = useQuery(usersQuery);
  const adopt = useAdoptScimAccount();
  const [user, setUser] = useState("");
  const [externalId, setExternalId] = useState("");
  const [userName, setUserName] = useState("");
  const [errors, setErrors] = useState<{ user?: string; external?: string }>({});
  const [confirming, setConfirming] = useState(false);
  const managed = new Set(links.map((link) => link.user_id));
  const candidates = (users.data ?? []).filter((item) => !item.is_admin && !item.disabled && !managed.has(item.id));
  const selected = candidates.find((item) => item.id === user);
  return (
    <form
      className="space-y-4 border-t border-line pt-4"
      noValidate
      onSubmit={(event) => {
        event.preventDefault();
        const problems: typeof errors = {};
        if (!selected) problems.user = "Choose an account.";
        if (externalId.trim() === "") problems.external = "Enter the externalId your identity provider uses for this person.";
        setErrors(problems);
        if (Object.keys(problems).length === 0) setConfirming(true);
      }}
    >
      <div>
        <h4 className="text-[13px] font-semibold text-fg">Adopt an existing account</h4>
        <p className="mt-0.5 text-xs text-fg-muted">Cove never adopts accounts by email. Instance administrators and disabled accounts cannot be provisioned.</p>
      </div>
      <div className="grid gap-4 sm:grid-cols-2">
        <Field id={`${id}-user`} label="Cove account" error={errors.user ?? (users.isError ? `Users could not be loaded: ${errorMessage(users.error)}` : undefined)}>
          <Select id={`${id}-user`} className={inputClass} value={user} disabled={users.isPending} onChange={(event) => setUser(event.target.value)} aria-invalid={errors.user ? true : undefined} aria-describedby={describedBy(`${id}-user`, errors.user)}>
            <option value="" disabled>
              {users.isPending ? "Loading accounts" : candidates.length === 0 ? "No eligible accounts" : "Choose an account"}
            </option>
            {candidates.map((item) => (
              <option key={item.id} value={item.id}>
                {item.name ? `${item.name} (${item.email})` : item.email}
              </option>
            ))}
          </Select>
        </Field>
        <Field id={`${id}-external`} label="externalId" hint="With an identity provider binding, this must be the person's exact OIDC subject." error={errors.external}>
          <input
            id={`${id}-external`}
            className={`${inputClass} font-mono`}
            autoComplete="off"
            spellCheck={false}
            value={externalId}
            onChange={(event) => setExternalId(event.target.value)}
            aria-invalid={errors.external ? true : undefined}
            aria-describedby={describedBy(`${id}-external`, errors.external, true)}
          />
        </Field>
        <Field id={`${id}-username`} label="SCIM userName" optional hint="Defaults to the account's email address.">
          <input id={`${id}-username`} className={inputClass} autoComplete="off" spellCheck={false} value={userName} onChange={(event) => setUserName(event.target.value)} aria-describedby={`${id}-username-hint`} />
        </Field>
      </div>
      {adopt.isError && <Notice tone="error" title="Could not adopt the account">{errorMessage(adopt.error)}</Notice>}
      {adopt.isSuccess && <Notice tone="success" title="Account adopted">Your identity provider now manages it.</Notice>}
      <div className="flex justify-end">
        <Button type="submit" disabled={!online} pending={adopt.isPending}>
          Review adoption
        </Button>
      </div>
      {confirming && selected && (
        <AdoptDialog
          email={selected.email}
          externalId={externalId.trim()}
          pending={adopt.isPending}
          onCancel={() => setConfirming(false)}
          onConfirm={() =>
            adopt.mutate(
              { user_id: selected.id, external_id: externalId.trim(), ...(userName.trim() ? { user_name: userName.trim() } : {}) },
              {
                onSuccess: () => {
                  setUser("");
                  setExternalId("");
                  setUserName("");
                },
                onSettled: () => setConfirming(false),
              },
            )
          }
        />
      )}
    </form>
  );
}

function Accounts({ providers }: { providers: OidcProvider[] }) {
  const online = useOnline();
  const links = useQuery(scimLinksQuery);
  const detach = useDetachScimAccount();
  const [detaching, setDetaching] = useState<ScimLink | null>(null);
  return (
    <Subsection title="Provisioned accounts" description="Accounts your identity provider manages. Deactivating a user there disables the Cove account and revokes its sessions and API tokens.">
      {links.isPending ? (
        <LoadingRows label="Loading provisioned accounts" rows={2} />
      ) : links.isError ? (
        <QueryError what="provisioned accounts" error={links.error} onRetry={() => void links.refetch()} retrying={links.isFetching} />
      ) : (
        <>
          {links.data.length === 0 ? (
            <p className="text-[13px] text-fg-muted">No provisioned accounts yet.</p>
          ) : (
            <ul aria-label="Provisioned accounts" className="divide-y divide-line rounded-md border border-line">
              {links.data.map((link) => {
                const state = linkState(link);
                return (
                  <li key={link.user_id} className="flex flex-wrap items-start justify-between gap-x-6 gap-y-2 px-3 py-2.5">
                    <div className="min-w-0 flex-1 basis-64 space-y-1">
                      <div className="flex flex-wrap items-center gap-x-3 gap-y-1">
                        <p className="text-[13px] font-medium break-all text-fg">{link.user_name}</p>
                        <StatusLabel tone={state.tone} label={state.label} />
                      </div>
                      <MetaList className="text-xs text-fg-muted">
                        <span>
                          externalId <span className="font-mono break-all">{link.external_id}</span>
                        </span>
                        <span>Sign-in identity: {providerName(providers, link.oidc_provider_id)}</span>
                        {link.deleted_at && <span>Deleted <RelativeTime value={link.deleted_at} /></span>}
                      </MetaList>
                    </div>
                    <Button variant="ghost" size="sm" disabled={!online} pending={detach.isPending && detach.variables === link.user_id} onClick={() => { detach.reset(); setDetaching(link); }}>
                      Detach<span className="sr-only"> {link.user_name}</span>
                    </Button>
                  </li>
                );
              })}
            </ul>
          )}
          {detach.isError && <Notice tone="error" title="Could not detach the account">{errorMessage(detach.error)}</Notice>}
          {detach.isSuccess && <Notice tone="success" title="Account detached">It keeps its sign-in identity and current enabled or disabled state.</Notice>}
          <AdoptForm links={links.data} />
        </>
      )}
      {detaching && <DetachDialog link={detaching} pending={detach.isPending} onCancel={() => setDetaching(null)} onConfirm={() => detach.mutate(detaching.user_id, { onSettled: () => setDetaching(null) })} />}
    </Subsection>
  );
}

const operationLabels: Record<string, string> = {
  "POST SCIM users": "Created user",
  "PUT SCIM users": "Replaced user",
  "PATCH SCIM users": "Updated user",
  "DELETE SCIM users": "Deleted user",
  "POST SCIM groups": "Created group",
  "PUT SCIM groups": "Replaced group",
  "PATCH SCIM groups": "Updated group",
  "DELETE SCIM groups": "Deleted group",
};

function activityTarget(entry: ScimActivity, links: readonly ScimLink[] | undefined, groups: readonly ScimGroup[] | undefined) {
  if (entry.target_id === "") return null;
  const name = entry.operation.endsWith("groups") ? groups?.find((group) => group.id === entry.target_id)?.display_name : links?.find((link) => link.user_id === entry.target_id)?.user_name;
  return name ? <span className="break-all">{name}</span> : <span className="font-mono break-all">{entry.target_id}</span>;
}

function Activity() {
  const activity = useQuery(scimActivityQuery);
  const links = useQuery(scimLinksQuery);
  const groups = useQuery(scimGroupsQuery);
  return (
    <Subsection title="Recent activity" description="The 100 most recent changes your identity provider made or attempted. Reads and rejected authentication are not listed.">
      {activity.isPending ? (
        <LoadingRows label="Loading provisioning activity" rows={2} />
      ) : activity.isError ? (
        <QueryError what="provisioning activity" error={activity.error} onRetry={() => void activity.refetch()} retrying={activity.isFetching} />
      ) : activity.data.length === 0 ? (
        <p className="text-[13px] text-fg-muted">No provisioning changes yet.</p>
      ) : (
        <ul aria-label="Provisioning activity" className="max-h-80 divide-y divide-line overflow-y-auto rounded-md border border-line">
          {activity.data.map((entry) => {
            const target = activityTarget(entry, links.data, groups.data);
            return (
              <li key={entry.id} className="flex flex-wrap items-start justify-between gap-x-4 gap-y-1 px-3 py-2">
                <div className="min-w-0 flex-1 basis-56">
                  <p className="text-[13px] text-fg">
                    {operationLabels[entry.operation] ?? entry.operation}
                    {target && <span className="text-fg-muted"> · {target}</span>}
                  </p>
                  <p className="text-xs text-fg-muted">
                    <RelativeTime value={entry.time} />
                  </p>
                </div>
                <StatusLabel tone={entry.outcome === "succeeded" ? "ok" : "bad"} label={entry.outcome === "succeeded" ? "Succeeded" : `Failed (HTTP ${entry.status})`} />
              </li>
            );
          })}
        </ul>
      )}
    </Subsection>
  );
}

export function ScimProvisioning() {
  const settings = useQuery(scimSettingsQuery);
  const providers = useQuery(oidcProvidersQuery);
  return (
    <Section title="SCIM provisioning" description="Let an identity provider such as Okta, Entra ID or Authentik create, update and deactivate Cove accounts and map its groups to teams." flush>
      {settings.isPending || providers.isPending ? (
        <LoadingRows label="Loading SCIM settings" rows={3} />
      ) : settings.isError || providers.isError ? (
        <div className="p-3">
          <QueryError
            what="SCIM settings"
            error={settings.isError ? settings.error : providers.error}
            onRetry={() => void (settings.isError ? settings.refetch() : providers.refetch())}
            retrying={settings.isFetching || providers.isFetching}
          />
        </div>
      ) : (
        <div className="divide-y divide-line">
          <Connection settings={settings.data} />
          <Policy settings={settings.data} providers={providers.data} />
          <Groups />
          <Accounts providers={providers.data} />
          <Activity />
        </div>
      )}
    </Section>
  );
}

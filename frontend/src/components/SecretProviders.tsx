import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useId, useState, type FormEvent } from "react";
import { errorMessage, request } from "../api/client";
import {
  providerCommands,
  referenceFormats,
  secretProviderKeys,
  secretProviderKinds,
  secretProviderLabels,
  secretProvidersQuery,
  type SecretProvider,
  type SecretProviderInput,
  type SecretProviderKind,
} from "../api/secret-providers";
import { useOnline } from "../lib/hooks";
import { canManageIntegrations } from "../lib/permissions";
import { useSession } from "../lib/session";
import { describedBy, inputClass } from "../lib/styles";
import { ConfirmDialog, Dialog } from "./ConfirmDialog";
import { CopyButton } from "./CopyButton";
import { Select } from "./Select";
import { StatusLabel } from "./status";
import { Button, Field, LoadingRows, Notice, QueryError, Subsection } from "./ui";

const dopplerURL = "https://api.doppler.com";
const bitwardenRegions = [
  { url: "https://vault.bitwarden.com", label: "United States (vault.bitwarden.com)" },
  { url: "https://vault.bitwarden.eu", label: "European Union (vault.bitwarden.eu)" },
];

const defaultURLs: Partial<Record<SecretProviderKind, string>> = {
  infisical: "https://app.infisical.com",
  doppler: dopplerURL,
  bitwarden: "https://vault.bitwarden.com",
};

const tokenLabels: Record<SecretProviderKind, string> = {
  vault: "Token",
  openbao: "Token",
  infisical: "Access token",
  doppler: "Service token",
  "onepassword-connect": "Connect token",
  "onepassword-service": "Service account token",
  bitwarden: "Machine account access token",
};

const tokenHints: Record<SecretProviderKind, string> = {
  vault: "Needs read access to the KV version 2 paths you reference.",
  openbao: "Needs read access to the KV version 2 paths you reference.",
  infisical: "A machine identity access token with read access to the project environment.",
  doppler: "A read-only service token. Its project and config apply when the fields below are empty.",
  "onepassword-connect": "A Connect server token with read access to the vaults you reference.",
  "onepassword-service": "A service account token with read access to the vaults you reference. Its account must sign in on 1password.com, 1password.eu or 1password.ca.",
  bitwarden: "A machine account token with read access to the secrets you reference.",
};

type FormErrors = Partial<Record<"name" | "url" | "project" | "environment" | "namespace" | "token", string>>;

function validate(input: SecretProviderInput, requireToken: boolean): FormErrors {
  const errors: FormErrors = {};
  if (!input.name) errors.name = "Enter a name.";
  else if (input.name.length > 120) errors.name = "Use at most 120 characters.";
  if (input.kind === "vault" || input.kind === "openbao" || input.kind === "infisical" || input.kind === "onepassword-connect") {
    try {
      const url = new URL(input.url);
      if (url.protocol !== "https:" || url.username || url.password || url.search || url.hash) errors.url = "Use an HTTPS URL without credentials, query or fragment.";
    } catch {
      errors.url = "Enter the server URL.";
    }
  }
  if (input.kind === "infisical" && !input.project) errors.project = "Enter the project ID.";
  if (input.kind === "infisical" && !input.environment) errors.environment = "Enter the environment slug.";
  for (const field of ["project", "environment", "namespace"] as const) {
    if ((input[field]?.length ?? 0) > 256) errors[field] = "Use at most 256 characters.";
  }
  if (requireToken || input.token !== "") {
    if (input.token.length < 8 || input.token.length > 8192 || /[\r\n\0]/.test(input.token)) errors.token = "Enter the token on one line, 8 to 8192 characters.";
  }
  return errors;
}

function focusFirstInvalid(form: HTMLFormElement) {
  requestAnimationFrame(() => form.querySelector<HTMLElement>("[aria-invalid=true]")?.focus());
}

function CommandNotice({ kind }: { kind: SecretProviderKind }) {
  const command = providerCommands[kind];
  if (!command) return null;
  return (
    <Notice tone="warn" title={`Requires the ${command} CLI on the control host`}>
      An operator must install the official <span className="font-mono">{command}</span> CLI from the vendor on the host running the Cove control plane. The Cove image does not include it, and saving fails until it is found.
    </Notice>
  );
}

function ProviderForm() {
  const id = useId();
  const online = useOnline();
  const client = useQueryClient();
  const [kind, setKind] = useState<SecretProviderKind>("vault");
  const [name, setName] = useState("");
  const [url, setUrl] = useState("");
  const [project, setProject] = useState("");
  const [environment, setEnvironment] = useState("");
  const [namespace, setNamespace] = useState("");
  const [token, setToken] = useState("");
  const [errors, setErrors] = useState<FormErrors>({});
  const [created, setCreated] = useState<SecretProvider | null>(null);
  // No mutation variables: the input carries the token, and variables stay in the mutation cache after it settles.
  const create = useMutation({
    gcTime: 0,
    mutationFn: () => request<SecretProvider>("POST", "/api/secret-providers", formInput()),
    onSuccess: (provider) => {
      setName("");
      setToken("");
      setProject("");
      setEnvironment("");
      setNamespace("");
      setUrl(defaultURLs[kind] ?? "");
      setCreated(provider);
      return client.invalidateQueries({ queryKey: secretProviderKeys.list });
    },
    onError: () => setToken(""),
    networkMode: "always",
  });

  function choose(next: SecretProviderKind) {
    setKind(next);
    setUrl(defaultURLs[next] ?? "");
    setProject("");
    setEnvironment("");
    setNamespace("");
    setErrors({});
    create.reset();
  }

  function formInput(): SecretProviderInput {
    const vaultLike = kind === "vault" || kind === "openbao";
    return {
      name: name.trim(),
      kind,
      url: kind === "onepassword-service" ? "" : url.trim().replace(/\/+$/, ""),
      token,
      ...(kind === "infisical" || kind === "doppler" ? { project: project.trim(), environment: environment.trim() } : {}),
      ...(vaultLike && namespace.trim() ? { namespace: namespace.trim() } : {}),
    };
  }

  function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    setCreated(null);
    const found = validate(formInput(), true);
    setErrors(found);
    if (Object.keys(found).length > 0) {
      focusFirstInvalid(event.currentTarget);
      return;
    }
    create.mutate();
  }

  const hasURL = kind !== "onepassword-service" && kind !== "bitwarden";
  return (
    <form onSubmit={submit} noValidate className="space-y-4">
      <fieldset disabled={create.isPending} className="min-w-0 space-y-4">
        <div className="grid gap-4 sm:grid-cols-2">
          <Field id={`${id}-kind`} label="Provider">
            <Select id={`${id}-kind`} value={kind} onChange={(event) => choose(event.target.value as SecretProviderKind)}>
              {secretProviderKinds.map((option) => <option key={option} value={option}>{secretProviderLabels[option]}</option>)}
            </Select>
          </Field>
          <Field id={`${id}-name`} label="Name" error={errors.name}>
            <input id={`${id}-name`} className={inputClass} value={name} maxLength={120} autoComplete="off" onChange={(event) => setName(event.target.value)} aria-invalid={errors.name ? true : undefined} aria-describedby={describedBy(`${id}-name`, errors.name)} />
          </Field>
        </div>
        <CommandNotice kind={kind} />
        {hasURL && (
          <Field id={`${id}-url`} label="Server URL" error={errors.url} hint={kind === "doppler" ? "Doppler always uses its public API." : "HTTPS on a public address. Private and self-managed internal endpoints are refused."}>
            <input id={`${id}-url`} className={`${inputClass} font-mono`} value={url} readOnly={kind === "doppler"} inputMode="url" autoComplete="off" autoCapitalize="none" spellCheck={false} placeholder={kind === "doppler" ? undefined : "https://vault.example.com"} onChange={(event) => setUrl(event.target.value)} aria-invalid={errors.url ? true : undefined} aria-describedby={describedBy(`${id}-url`, errors.url, true)} />
          </Field>
        )}
        {kind === "bitwarden" && (
          <Field id={`${id}-region`} label="Region">
            <Select id={`${id}-region`} value={url} onChange={(event) => setUrl(event.target.value)}>
              {bitwardenRegions.map((region) => <option key={region.url} value={region.url}>{region.label}</option>)}
            </Select>
          </Field>
        )}
        {(kind === "vault" || kind === "openbao") && (
          <Field id={`${id}-namespace`} label="Namespace" optional error={errors.namespace} hint="Only for Vault Enterprise or OpenBao namespaces.">
            <input id={`${id}-namespace`} className={`${inputClass} font-mono`} value={namespace} maxLength={256} autoComplete="off" autoCapitalize="none" spellCheck={false} onChange={(event) => setNamespace(event.target.value)} aria-invalid={errors.namespace ? true : undefined} aria-describedby={describedBy(`${id}-namespace`, errors.namespace, true)} />
          </Field>
        )}
        {(kind === "infisical" || kind === "doppler") && (
          <div className="grid gap-4 sm:grid-cols-2">
            <Field id={`${id}-project`} label={kind === "infisical" ? "Project ID" : "Project"} optional={kind === "doppler"} error={errors.project}>
              <input id={`${id}-project`} className={`${inputClass} font-mono`} value={project} maxLength={256} autoComplete="off" autoCapitalize="none" spellCheck={false} onChange={(event) => setProject(event.target.value)} aria-invalid={errors.project ? true : undefined} aria-describedby={describedBy(`${id}-project`, errors.project)} />
            </Field>
            <Field id={`${id}-environment`} label={kind === "infisical" ? "Environment slug" : "Config"} optional={kind === "doppler"} error={errors.environment}>
              <input id={`${id}-environment`} className={`${inputClass} font-mono`} value={environment} maxLength={256} autoComplete="off" autoCapitalize="none" spellCheck={false} onChange={(event) => setEnvironment(event.target.value)} aria-invalid={errors.environment ? true : undefined} aria-describedby={describedBy(`${id}-environment`, errors.environment)} />
            </Field>
          </div>
        )}
        <Field id={`${id}-token`} label={tokenLabels[kind]} error={errors.token} hint={`${tokenHints[kind]} Stored encrypted and never shown again.`}>
          <input id={`${id}-token`} type="password" className={`${inputClass} font-mono`} value={token} autoComplete="off" spellCheck={false} data-1p-ignore data-lpignore="true" onChange={(event) => setToken(event.target.value)} aria-invalid={errors.token ? true : undefined} aria-describedby={describedBy(`${id}-token`, errors.token, true)} />
        </Field>
      </fieldset>
      {create.isError && <Notice tone="error" title="Could not save the provider">{errorMessage(create.error)}</Notice>}
      {created && (
        <Notice tone="success" title={`${created.name} saved`}>
          Cove has not contacted the provider. The token is first used when a deployment resolves a reference to this connection.
        </Notice>
      )}
      <div className="flex justify-end">
        <Button type="submit" pending={create.isPending} disabled={!online}>Save provider</Button>
      </div>
    </form>
  );
}

function EditProvider({ provider, onClose }: { provider: SecretProvider; onClose: () => void }) {
  const id = useId();
  const online = useOnline();
  const client = useQueryClient();
  const [name, setName] = useState(provider.name);
  const [token, setToken] = useState("");
  const [errors, setErrors] = useState<FormErrors>({});
  // No mutation variables, for the same reason as in ProviderForm.
  const update = useMutation({
    gcTime: 0,
    mutationFn: () => request<SecretProvider>("PUT", `/api/secret-providers/${encodeURIComponent(provider.id)}`, formInput()),
    onSuccess: () => {
      onClose();
      return client.invalidateQueries({ queryKey: secretProviderKeys.list });
    },
    onError: () => setToken(""),
    networkMode: "always",
  });

  // The server refuses changes to the provider settings, so they are sent exactly as saved.
  function formInput(): SecretProviderInput {
    return { name: name.trim(), kind: provider.kind, url: provider.url, token, project: provider.project, environment: provider.environment, namespace: provider.namespace };
  }

  function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    const found = validate(formInput(), false);
    delete found.url;
    delete found.project;
    delete found.environment;
    setErrors(found);
    if (Object.keys(found).length > 0) {
      focusFirstInvalid(event.currentTarget);
      return;
    }
    update.mutate();
  }

  return (
    <form onSubmit={submit} noValidate className="space-y-4">
      <fieldset disabled={update.isPending} className="min-w-0 space-y-4">
        <Field id={`${id}-name`} label="Name" error={errors.name}>
          <input id={`${id}-name`} className={inputClass} value={name} maxLength={120} autoComplete="off" onChange={(event) => setName(event.target.value)} aria-invalid={errors.name ? true : undefined} aria-describedby={describedBy(`${id}-name`, errors.name)} />
        </Field>
        <Field id={`${id}-token`} label={`New ${tokenLabels[provider.kind].toLowerCase()}`} optional error={errors.token} hint="Leave empty to keep the saved token.">
          <input id={`${id}-token`} type="password" className={`${inputClass} font-mono`} value={token} autoComplete="off" spellCheck={false} data-1p-ignore data-lpignore="true" onChange={(event) => setToken(event.target.value)} aria-invalid={errors.token ? true : undefined} aria-describedby={describedBy(`${id}-token`, errors.token, true)} />
        </Field>
        <p className="text-xs text-fg-muted">To change the provider, URL, project, environment or namespace, create a new connection and update the references that use this one.</p>
      </fieldset>
      {update.isError && <Notice tone="error" title="Could not save the provider">{errorMessage(update.error)}</Notice>}
      <div className="flex justify-end gap-2">
        <Button variant="ghost" onClick={onClose} disabled={update.isPending}>Cancel</Button>
        <Button type="submit" variant="primary" pending={update.isPending} disabled={!online}>Save changes</Button>
      </div>
    </form>
  );
}

function ProviderRow({ provider, manage }: { provider: SecretProvider; manage: boolean }) {
  const online = useOnline();
  const client = useQueryClient();
  const [editing, setEditing] = useState(false);
  const [confirming, setConfirming] = useState(false);
  const remove = useMutation({
    mutationFn: () => request<{ ok: boolean }>("DELETE", `/api/secret-providers/${encodeURIComponent(provider.id)}`),
    onSuccess: () => {
      setConfirming(false);
      return client.invalidateQueries({ queryKey: secretProviderKeys.list });
    },
    networkMode: "always",
  });
  const command = providerCommands[provider.kind];
  const details = [
    provider.url && { label: "URL", value: provider.url },
    provider.namespace && { label: "Namespace", value: provider.namespace },
    provider.project && { label: provider.kind === "doppler" ? "Project" : "Project ID", value: provider.project },
    provider.environment && { label: provider.kind === "doppler" ? "Config" : "Environment", value: provider.environment },
  ].filter((item): item is { label: string; value: string } => Boolean(item));
  return (
    <li className="space-y-2 px-3 py-2.5">
      <div className="flex flex-wrap items-start justify-between gap-x-4 gap-y-2">
        <div className="min-w-0 space-y-0.5">
          <p className="text-[13px] font-medium break-words text-fg">{provider.name}</p>
          <p className="text-xs text-fg-muted">{secretProviderLabels[provider.kind]}</p>
        </div>
        {manage && (
          <div className="flex gap-1">
            <Button size="sm" variant="ghost" disabled={!online} onClick={() => setEditing(true)} aria-label={`Edit ${provider.name}`}>Edit</Button>
            <Button size="sm" variant="ghost" disabled={!online} onClick={() => { remove.reset(); setConfirming(true); }} aria-label={`Delete ${provider.name}`}>Delete</Button>
          </div>
        )}
      </div>
      {details.length > 0 && (
        <dl className="grid gap-x-3 gap-y-0.5 text-xs sm:grid-cols-[auto_minmax(0,1fr)]">
          {details.map((item) => (
            <div key={item.label} className="contents">
              <dt className="text-fg-subtle">{item.label}</dt>
              <dd className="font-mono break-all text-fg-muted">{item.value}</dd>
            </div>
          ))}
        </dl>
      )}
      {command && (
        provider.available
          ? <StatusLabel tone="neutral" label={`${command} CLI found on the control host`} />
          : <StatusLabel tone="warn" label={`${command} CLI not found on the control host`} />
      )}
      <div className="flex flex-wrap items-center gap-2">
        <span className="text-xs text-fg-subtle">Connection ID</span>
        <span className="min-w-0 font-mono text-xs break-all text-fg-muted">{provider.id}</span>
        <CopyButton text={provider.id} label="Copy ID" variant="ghost" />
      </div>
      <Dialog open={editing} title={`Edit ${provider.name}`} onClose={() => setEditing(false)}>
        {editing && <EditProvider provider={provider} onClose={() => setEditing(false)} />}
      </Dialog>
      <ConfirmDialog open={confirming} title={`Delete ${provider.name}?`} confirmLabel="Delete provider" danger pending={remove.isPending} onCancel={() => setConfirming(false)} onConfirm={() => remove.mutate()}>
        <div className="space-y-3">
          <p>Its saved token is deleted. Cove does not track which variables reference this connection.</p>
          <p>Any saved variable that still references it fails to resolve, and the deployment stops. There is no fallback value.</p>
          {remove.isError && <Notice tone="error" title="Could not delete the provider">{errorMessage(remove.error)}</Notice>}
        </div>
      </ConfirmDialog>
    </li>
  );
}

export function SecretProviders() {
  const session = useSession();
  const manage = canManageIntegrations(session);
  const providers = useQuery(secretProvidersQuery);
  return (
    <div className="divide-y divide-line">
      <Subsection title="Connections" description="Saved variables reference secrets with {{secret.CONNECTION_ID:REFERENCE}}. Cove reads them when a deployment starts and never shows their values.">
        {providers.isPending ? (
          <LoadingRows label="Loading secret providers" rows={2} />
        ) : providers.isError ? (
          <QueryError what="secret providers" error={providers.error} onRetry={() => void providers.refetch()} retrying={providers.isFetching} />
        ) : providers.data.length === 0 ? (
          <p className="text-[13px] text-fg-muted">No secret providers saved.</p>
        ) : (
          <ul aria-label="Secret providers" className="divide-y divide-line rounded-md border border-line">
            {providers.data.map((provider) => <ProviderRow key={provider.id} provider={provider} manage={manage} />)}
          </ul>
        )}
      </Subsection>
      <Subsection title="Reference formats">
        <dl className="grid gap-x-4 gap-y-1 text-xs sm:grid-cols-[auto_minmax(0,1fr)]">
          {secretProviderKinds.filter((kind) => kind !== "openbao").map((kind) => (
            <div key={kind} className="contents">
              <dt className="text-fg-muted">{kind === "vault" ? "Vault and OpenBao" : secretProviderLabels[kind]}</dt>
              <dd className="mb-1 font-mono break-all text-fg sm:mb-0">{referenceFormats[kind].format}</dd>
            </div>
          ))}
        </dl>
      </Subsection>
      <Subsection title="Add a provider" description={manage ? "Cove saves the token without contacting the provider. Errors surface when a deployment resolves a reference." : undefined}>
        {manage ? <ProviderForm /> : <p className="text-[13px] text-fg-muted">Team owners, admins and team-wide editors add secret providers.</p>}
      </Subsection>
    </div>
  );
}

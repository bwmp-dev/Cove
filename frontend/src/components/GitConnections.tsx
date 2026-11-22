import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useId, useState, type FormEvent } from "react";
import { errorMessage, request } from "../api/client";
import { gitConnectionsQuery, gitProviderKeys, providerLabels, type CreatedGitConnection, type GitConnection, type GitConnectionInput, type GitProviderKind } from "../api/git-providers";
import { useOnline } from "../lib/hooks";
import { canManageIntegrations } from "../lib/permissions";
import { useSession } from "../lib/session";
import { describedBy, inputClass } from "../lib/styles";
import { ConfirmDialog } from "./ConfirmDialog";
import { CopyButton } from "./CopyButton";
import { Select } from "./Select";
import { Button, Field, LoadingRows, Notice, QueryError, Subsection } from "./ui";

const bitbucketAPI = "https://api.bitbucket.org";

const providerChoices: { kind: GitProviderKind; url: string }[] = [
  { kind: "gitlab", url: "https://gitlab.com" },
  { kind: "gitea", url: "" },
  { kind: "forgejo", url: "" },
  { kind: "codeberg", url: "https://codeberg.org" },
  { kind: "bitbucket", url: bitbucketAPI },
];

const createLabels: Record<GitProviderKind, string> = { ...providerLabels, gitea: "Gitea" };

function secretLabel(kind: GitProviderKind) {
  return kind === "gitlab" ? "Signing token" : "Secret";
}

function webhookEvents(kind: GitProviderKind) {
  if (kind === "gitlab") return "Push events, Tag push events and Merge request events";
  if (kind === "bitbucket") return "Repository push and the Pull request created, updated, merged and declined events";
  return "Push and Pull request events";
}

function urlHint(kind: GitProviderKind) {
  if (kind === "bitbucket") return "Bitbucket Cloud always uses its public API.";
  return "The server address, with a path prefix for self-managed installs. Do not add /api/v4 or /api/v1. Must be HTTPS on a public address.";
}

function validate(input: GitConnectionInput) {
  const errors: Partial<Record<"name" | "url" | "workspace" | "username" | "token", string>> = {};
  if (!input.name) errors.name = "Enter a name.";
  else if (input.name.length > 120) errors.name = "Use at most 120 characters.";
  try {
    const url = new URL(input.url);
    if (url.protocol !== "https:" || url.username || url.password || url.search || url.hash) errors.url = "Use an HTTPS URL without credentials, query or fragment.";
    else if (/\/api\/v[14]\/?$/.test(url.pathname)) errors.url = "Remove the API path; Cove adds it.";
  } catch {
    errors.url = "Enter the server URL.";
  }
  if (input.kind === "bitbucket" && !input.workspace) errors.workspace = "Enter the workspace slug.";
  else if (input.workspace && (input.workspace.length > 100 || /[/\\]/.test(input.workspace))) errors.workspace = "Enter the workspace slug, without slashes.";
  if (input.username && (input.username.length > 254 || !/^[^\s@]+@[^\s@]+$/.test(input.username))) errors.username = "Enter the Atlassian account email, or leave it empty for an OAuth token.";
  if (input.token.length < 8 || input.token.length > 8192 || /[\r\n\0]/.test(input.token)) errors.token = "Enter the provider access token on one line.";
  return errors;
}

function CreatedConnection({ connection, onDone }: { connection: CreatedGitConnection; onDone: () => void }) {
  const label = secretLabel(connection.kind);
  return (
    <Notice tone="success" title={`${connection.name} connected. Save the webhook ${label.toLowerCase()} now.`}>
      <div className="min-w-0 space-y-3">
        <p>
          Add a webhook at the provider with this URL and {label.toLowerCase()}, selecting {webhookEvents(connection.kind)}.
          {connection.kind === "gitlab" ? " Requires GitLab 19 or later signing tokens; a plain secret token is not accepted." : " Payloads are verified with HMAC SHA-256."} Cove does not create the webhook for you.
        </p>
        <dl className="space-y-2 text-xs">
          <div className="space-y-1">
            <dt className="font-medium text-fg">Webhook URL</dt>
            <dd className="font-mono break-all">{connection.webhook_url}</dd>
            <dd><CopyButton text={connection.webhook_url} label="Copy URL" /></dd>
          </div>
          <div className="space-y-1">
            <dt className="font-medium text-fg">{label}</dt>
            <dd className="font-mono break-all">{connection.webhook_secret}</dd>
            <dd><CopyButton text={connection.webhook_secret} label={`Copy ${label.toLowerCase()}`} /></dd>
          </div>
        </dl>
        <p className="text-xs">This {label.toLowerCase()} is not shown again. To replace it, delete the connection and create a new one.</p>
        <Button onClick={onDone}>Done</Button>
      </div>
    </Notice>
  );
}

function ConnectionForm({ onCreated }: { onCreated: (connection: CreatedGitConnection) => void }) {
  const id = useId();
  const online = useOnline();
  const client = useQueryClient();
  const [kind, setKind] = useState<GitProviderKind>("gitlab");
  const [name, setName] = useState("");
  const [url, setUrl] = useState("https://gitlab.com");
  const [workspace, setWorkspace] = useState("");
  const [username, setUsername] = useState("");
  const [token, setToken] = useState("");
  const [errors, setErrors] = useState<ReturnType<typeof validate>>({});
  const create = useMutation({
    gcTime: 0,
    mutationFn: (input: GitConnectionInput) => request<CreatedGitConnection>("POST", "/api/git/connections", input),
    onSuccess: (connection) => {
      setName("");
      setToken("");
      setWorkspace("");
      setUsername("");
      onCreated(connection);
      return client.invalidateQueries({ queryKey: gitProviderKeys.connections });
    },
    onError: () => setToken(""),
    networkMode: "always",
  });
  const bitbucket = kind === "bitbucket";

  function choose(next: GitProviderKind) {
    setKind(next);
    setUrl(providerChoices.find((choice) => choice.kind === next)?.url ?? "");
    setErrors({});
    create.reset();
  }

  function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    const input: GitConnectionInput = { name: name.trim(), kind, url: url.trim().replace(/\/+$/, ""), token, ...(bitbucket ? { workspace: workspace.trim(), ...(username.trim() ? { username: username.trim() } : {}) } : {}) };
    const found = validate(input);
    setErrors(found);
    if (Object.keys(found).length > 0) {
      const form = event.currentTarget;
      requestAnimationFrame(() => form.querySelector<HTMLElement>("[aria-invalid=true]")?.focus());
      return;
    }
    create.mutate(input);
  }

  return (
    <form onSubmit={submit} noValidate className="space-y-4">
      <fieldset disabled={create.isPending} className="min-w-0 space-y-4">
        <div className="grid gap-4 sm:grid-cols-2">
          <Field id={`${id}-kind`} label="Provider">
            <Select id={`${id}-kind`} value={kind} onChange={(event) => choose(event.target.value as GitProviderKind)}>
              {providerChoices.map((choice) => <option key={choice.kind} value={choice.kind}>{createLabels[choice.kind]}</option>)}
            </Select>
          </Field>
          <Field id={`${id}-name`} label="Name" error={errors.name}>
            <input id={`${id}-name`} className={inputClass} value={name} maxLength={120} autoComplete="off" onChange={(event) => setName(event.target.value)} aria-invalid={errors.name ? true : undefined} aria-describedby={describedBy(`${id}-name`, errors.name)} />
          </Field>
        </div>
        <div className="grid gap-4 sm:grid-cols-2">
          <Field id={`${id}-url`} label="Server URL" error={errors.url} hint={urlHint(kind)}>
            <input id={`${id}-url`} className={`${inputClass} font-mono`} value={url} readOnly={bitbucket} inputMode="url" autoComplete="off" autoCapitalize="none" spellCheck={false} placeholder={kind === "gitea" || kind === "forgejo" ? "https://git.example.com" : undefined} onChange={(event) => setUrl(event.target.value)} aria-invalid={errors.url ? true : undefined} aria-describedby={describedBy(`${id}-url`, errors.url, true)} />
          </Field>
          {bitbucket && (
            <Field id={`${id}-workspace`} label="Workspace" error={errors.workspace} hint="The workspace slug from bitbucket.org/workspace.">
              <input id={`${id}-workspace`} className={`${inputClass} font-mono`} value={workspace} maxLength={100} autoComplete="off" autoCapitalize="none" spellCheck={false} onChange={(event) => setWorkspace(event.target.value)} aria-invalid={errors.workspace ? true : undefined} aria-describedby={describedBy(`${id}-workspace`, errors.workspace, true)} />
            </Field>
          )}
        </div>
        {bitbucket && (
          <Field id={`${id}-username`} label="API token email" optional error={errors.username} hint="The Atlassian account email of a Bitbucket API token. Leave empty to use a short-lived OAuth access token instead.">
            <input id={`${id}-username`} type="email" className={inputClass} value={username} maxLength={254} autoComplete="off" autoCapitalize="none" spellCheck={false} onChange={(event) => setUsername(event.target.value)} aria-invalid={errors.username ? true : undefined} aria-describedby={describedBy(`${id}-username`, errors.username, true)} />
          </Field>
        )}
        <Field
          id={`${id}-token`}
          label={bitbucket ? (username.trim() ? "API token" : "OAuth access token") : "Access token"}
          error={errors.token}
          hint="Needs repository read and clone access. To report deployments, it also needs write access to commit statuses and pull request comments. Stored encrypted and never shown again."
        >
          <input id={`${id}-token`} type="password" className={`${inputClass} font-mono`} value={token} autoComplete="off" spellCheck={false} onChange={(event) => setToken(event.target.value)} aria-invalid={errors.token ? true : undefined} aria-describedby={describedBy(`${id}-token`, errors.token, true)} />
        </Field>
      </fieldset>
      {create.isError && <Notice tone="error" title="Could not connect the provider">{errorMessage(create.error)}</Notice>}
      <div className="flex justify-end">
        <Button type="submit" pending={create.isPending} disabled={!online}>Connect provider</Button>
      </div>
    </form>
  );
}

function ConnectionRow({ connection, manage }: { connection: GitConnection; manage: boolean }) {
  const online = useOnline();
  const client = useQueryClient();
  const [confirming, setConfirming] = useState(false);
  const remove = useMutation({
    mutationFn: () => request<{ ok: boolean }>("DELETE", `/api/git/connections/${connection.id}`),
    onSuccess: () => {
      setConfirming(false);
      return client.invalidateQueries({ queryKey: gitProviderKeys.connections });
    },
    networkMode: "always",
  });
  return (
    <li className="space-y-2 px-3 py-2.5">
      <div className="flex flex-wrap items-start justify-between gap-x-4 gap-y-2">
        <div className="min-w-0 space-y-0.5">
          <p className="text-[13px] font-medium break-words text-fg">{connection.name}</p>
          <p className="text-xs text-fg-muted">
            {providerLabels[connection.kind]} · <span className="font-mono break-all">{connection.url}</span>
            {connection.workspace && <> · workspace <span className="font-mono">{connection.workspace}</span></>}
          </p>
        </div>
        {manage && <Button size="sm" variant="ghost" disabled={!online} onClick={() => { remove.reset(); setConfirming(true); }}>Delete</Button>}
      </div>
      <div className="flex flex-wrap items-center gap-2">
        <span className="min-w-0 font-mono text-xs break-all text-fg-muted">{connection.webhook_url}</span>
        <CopyButton text={connection.webhook_url} label="Copy webhook URL" variant="ghost" />
      </div>
      <ConfirmDialog open={confirming} title={`Delete ${connection.name}?`} confirmLabel="Delete connection" danger pending={remove.isPending} onCancel={() => setConfirming(false)} onConfirm={() => remove.mutate()}>
        <div className="space-y-3">
          <p>Its access token and webhook {secretLabel(connection.kind).toLowerCase()} are deleted. Remove the webhook at the provider separately.</p>
          <p>A connection used by a resource or preview cannot be deleted; disconnect those first.</p>
          {remove.isError && <Notice tone="error" title="Could not delete the connection">{errorMessage(remove.error)}</Notice>}
        </div>
      </ConfirmDialog>
    </li>
  );
}

export function GitConnections() {
  const session = useSession();
  const manage = canManageIntegrations(session);
  const connections = useQuery(gitConnectionsQuery);
  const [created, setCreated] = useState<CreatedGitConnection | null>(null);
  return (
    <div className="divide-y divide-line">
      <Subsection title="Connections" description="This team's GitLab, Gitea, Forgejo, Codeberg and Bitbucket Cloud accounts. Resources choose a repository from one of them on their Source page.">
        {connections.isPending ? (
          <LoadingRows label="Loading Git providers" rows={2} />
        ) : connections.isError ? (
          <QueryError what="Git providers" error={connections.error} onRetry={() => void connections.refetch()} retrying={connections.isFetching} />
        ) : connections.data.length === 0 ? (
          <p className="text-[13px] text-fg-muted">No providers connected.</p>
        ) : (
          <ul aria-label="Connected Git providers" className="divide-y divide-line rounded-md border border-line">
            {connections.data.map((connection) => <ConnectionRow key={connection.id} connection={connection} manage={manage} />)}
          </ul>
        )}
      </Subsection>
      <Subsection title="Connect a provider" description={manage ? "Cove verifies the token with the provider before saving it." : undefined}>
        {created && <CreatedConnection connection={created} onDone={() => setCreated(null)} />}
        {manage ? <ConnectionForm onCreated={setCreated} /> : <p className="text-[13px] text-fg-muted">Team owners, admins and team-wide editors connect providers.</p>}
      </Subsection>
    </div>
  );
}

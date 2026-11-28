import { Checkbox } from "../components/Checkbox";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { useState, type FormEvent } from "react";
import { errorMessage, request } from "../api/client";
import { Button, Field, Notice, Section, SwitchField } from "../components/ui";
import { ConfirmDialog } from "../components/ConfirmDialog";
import { UnsavedChangesGuard } from "../components/SaveBar";
import { formText } from "../lib/form-data";
import { useOnline } from "../lib/hooks";
import { can } from "../lib/permissions";
import { inputClass } from "../lib/styles";
import { useResource } from "./workload-detail";
import { formatAbsolute } from "../lib/format";

interface Policy { enabled: boolean; hours: number; domain: string; service: string; port: number; environment?: Record<string,string> }
interface Preview { number: number; resource_id: string; revision: string; expires_at: string; closing: boolean; manually_stopped: boolean }
interface Data { policy: Policy; environment_keys: string[]; previews: Preview[] }

export function Previews() {
 const resource = useResource();
 const client = useQueryClient();
 const online = useOnline();
 const [closing, setClosing] = useState<number | null>(null);
 const [saved, setSaved] = useState(false);
 const path = `/api/workloads/${resource.id}/previews`;
 const key = ["previews", resource.id];
 const data = useQuery({ queryKey: key, queryFn: ({ signal }) => request<Data>("GET", path, undefined, signal), refetchInterval: 10000 });
 const save = useMutation({ gcTime: 0, mutationFn: (policy: Policy) => request("PUT", path, policy), onSuccess: () => client.invalidateQueries({ queryKey: key }) });
 const close = useMutation({ mutationFn: (number: number) => request("DELETE", path, { number }), onSuccess: () => { setClosing(null); return client.invalidateQueries({ queryKey: key }); } });
 const deploy = useMutation({ mutationFn: (number: number) => request("POST", path, { number }), onSuccess: () => client.invalidateQueries({ queryKey: key }) });
 const editable = can(resource.permissions, "edit");
 return <div className="space-y-5">
  <Section width="form" title="Pull request previews" description="Deploy pull and merge requests from the repository connected through the GitHub App or a Git provider connection. Forks are never deployed. New commits refresh expiry; closing a PR, disabling previews or reaching expiry removes its containers and preview volumes.">
   <div className="space-y-4">
    <Notice tone="warn">Only enable this for trusted contributors. Containers share the worker's kernel; public previews also share the ingress network. Never supply production credentials. Preview data is temporary and deleted on close.</Notice>
    <p className="text-sm text-fg-muted">Each preview gets a separate environment, network and fresh named volumes. Production variables, hooks, secrets and shared networks are not copied. Maximum five previews per resource and four services per preview, capped at 1 CPU and 512 MiB per service. Each preview reserves 4 CPUs and 2 GiB against team quotas.</p>
    <p className="text-sm text-fg-muted">Compose must use the isolated subset: no host ports, bind mounts, external storage/networks, includes, env_file, privileged options or repository symlinks. Use explicit {"${NAME}"} variables from this page and $$ for shell variables. Runtime networks have no outbound access unless a service is attached to public ingress.</p>
    <p className="text-sm text-fg-muted">Existing GitHub Apps need Pull requests: read permission and the pull_request webhook subscription enabled on GitHub. GitLab, Gitea, Forgejo, Codeberg and Bitbucket Cloud connections need pull or merge request events selected in the connection's webhook. Existing open PRs start on their next push or reopen. Settings apply to future preview deployments; existing domains stay until close. Project-level edit permission is required to save.</p>
    {data.data && <PreviewForm key={resource.id} policy={data.data.policy} names={data.data.environment_keys} disabled={!editable || !online || save.isPending} pending={save.isPending} onSave={async policy => { setSaved(false); try { await save.mutateAsync(policy); setSaved(true); } finally { save.reset(); } }} />}
    {(data.isError || save.isError || close.isError || deploy.isError) && <Notice tone="error">{errorMessage(data.error ?? save.error ?? close.error ?? deploy.error)}</Notice>}
    {saved && <Notice>Preview settings saved.</Notice>}
   </div>
  </Section>
  <Section title="Active previews"><ul className="divide-y divide-line">{data.data?.previews.map(preview => <li key={preview.number} className="flex flex-wrap items-center justify-between gap-3 py-3"><div><Link to="/resources/$resourceId" params={{ resourceId: preview.resource_id }} className="text-sm underline underline-offset-4">PR #{preview.number}</Link><p className="text-xs text-fg-muted">{preview.closing ? "Closing — cleanup must succeed before removal" : preview.manually_stopped ? "Manually stopped — pushes will not restart it" : `Expires ${formatAbsolute(preview.expires_at)}`} · {preview.revision.slice(0, 8)}</p></div>{editable && <div className="flex gap-2"><Button size="sm" disabled={!online || preview.closing || deploy.isPending} onClick={() => deploy.mutate(preview.number)}>{preview.manually_stopped ? "Resume preview" : "Deploy preview"}</Button><Button size="sm" disabled={!online || preview.closing || close.isPending} onClick={() => setClosing(preview.number)}>Close preview</Button></div>}</li>)}</ul>{data.data?.previews.length === 0 && <p className="text-sm text-fg-muted">No previews yet.</p>}</Section>
  <ConfirmDialog open={closing !== null} title="Close preview and delete its data?" confirmLabel="Close preview" danger pending={close.isPending} onCancel={() => setClosing(null)} onConfirm={() => { if (closing !== null) close.mutate(closing); }}>The preview's containers and volumes will be deleted. Production data is not affected. {close.isError && <Notice tone="error">{errorMessage(close.error)}</Notice>}</ConfirmDialog>
 </div>;
}

function PreviewForm({ policy, names, disabled, pending, onSave }: { policy: Policy; names: string[]; disabled: boolean; pending: boolean; onSave: (policy: Policy) => Promise<unknown> }) {
 const [replace, setReplace] = useState(false);
 const [variables, setVariables] = useState("");
 const [error, setError] = useState("");
 const [dirty, setDirty] = useState(false);
 async function submit(event: FormEvent<HTMLFormElement>) {
  event.preventDefault(); setError(""); const form = new FormData(event.currentTarget);
  const environment: Record<string,string> = {};
  if (replace) for (const line of variables.split(/\r?\n/).filter(line => line.trim() && !line.startsWith("#"))) { const at = line.indexOf("="); if (at < 1 || !/^[A-Za-z_][A-Za-z0-9_]*$/.test(line.slice(0, at))) { setError("Use one NAME=value per line."); return; } environment[line.slice(0, at)] = line.slice(at + 1); }
  try { await onSave({ enabled: form.has("enabled"), hours: Number(form.get("hours")), domain: formText(form, "domain").trim(), service: formText(form, "service").trim(), port: Number(form.get("port")), ...(replace ? { environment } : {}) }); setVariables(""); setReplace(false); setDirty(false); } catch (cause) { setError(errorMessage(cause)); }
 }
 return <form onChange={() => setDirty(true)} onSubmit={event => { void submit(event); }} className="space-y-3">
  <UnsavedChangesGuard dirty={dirty} />
  <SwitchField id="preview-enabled" name="enabled" defaultChecked={policy.enabled} disabled={disabled} label="Enable trusted repository previews" />
  <div className="grid gap-3 sm:grid-cols-2">
   <Field id="preview-hours" label="Expiry after last push (hours)"><input id="preview-hours" name="hours" type="number" min={1} max={168} defaultValue={policy.hours} required disabled={disabled} className={inputClass} /></Field>
   <Field id="preview-domain" label="Preview domain suffix" optional hint="For example preview.example.com. Leave empty for no public route. Point wildcard DNS to this worker when using a suffix."><input id="preview-domain" name="domain" defaultValue={policy.domain} disabled={disabled} className={inputClass} /></Field>
   <Field id="preview-service" label="Public service" optional><input id="preview-service" name="service" defaultValue={policy.service} disabled={disabled} className={inputClass} /></Field>
   <Field id="preview-port" label="Public container port" optional><input id="preview-port" name="port" type="number" min={1} max={65535} defaultValue={policy.port || 3000} disabled={disabled} className={inputClass} /></Field>
  </div>
  <p className="text-xs text-fg-muted">Saved preview variables: {names.join(", ") || "none"}</p>
  <label className="flex items-center gap-2.5 text-[13px]"><Checkbox checked={replace} onChange={event => setReplace(event.target.checked)} disabled={disabled} />Replace preview variables</label>
  {replace && <Field id="preview-variables" label="Preview variables" hint="NAME=value, one per line. Empty clears all saved values."><textarea id="preview-variables" value={variables} onChange={event => setVariables(event.target.value)} rows={4} disabled={disabled} className={inputClass} autoComplete="off" spellCheck={false} /></Field>}
  {error && <Notice tone="error">{error}</Notice>}
  <Button type="submit" disabled={disabled} pending={pending}>Save preview settings</Button>
 </form>;
}

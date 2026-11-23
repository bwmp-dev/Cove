import { Select } from "./Select";
import { formText } from "../lib/form-data";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useState, type FormEvent } from "react";
import { errorMessage, request } from "../api/client";
import { workloadQuery, workloadsQuery } from "../api/queries";
import { can } from "../lib/permissions";
import { useOnline } from "../lib/hooks";
import { inputClass } from "../lib/styles";
import { Button, Field, Notice, Section } from "./ui";
import { exportLabels, isDatabaseExport, isSecretDatabaseExport } from "../lib/databases";
import type { WorkloadSummary } from "../api/types";

interface Binding { name: string; source_id: string; source_key: string }

// Database metadata exports need only View on the source; saved variables and database credentials need Read secrets.
function bindableSource(item: WorkloadSummary) {
 return can(item.permissions, "read_secrets") || (item.kind === "database" && can(item.permissions, "view"));
}

export function ResourceVariables({ workloadId }: { workloadId: string }) {
 const client = useQueryClient();
 const online = useOnline();
 const [source, setSource] = useState("");
 const resources = useQuery(workloadsQuery);
 const detail = useQuery({ ...workloadQuery(source), enabled: Boolean(source) });
 const sourceItem = resources.data?.find(item => item.id === source);
 const readsSecrets = can(sourceItem?.permissions, "read_secrets");
 const databaseSource = sourceItem?.kind === "database";
 const sourceKeys = (detail.data?.environment_keys ?? []).filter(key => databaseSource ? isDatabaseExport(key) && (readsSecrets || !isSecretDatabaseExport(key)) : readsSecrets);
 const hidesSecrets = databaseSource && !readsSecrets;
 const bindings = useQuery({ queryKey: ["variable-bindings", workloadId], queryFn: ({ signal }) => request<Binding[]>("GET", `/api/workloads/${workloadId}/variable-bindings`, undefined, signal) });
 const save = useMutation({ mutationFn: ({ method, binding }: { method: "PUT" | "DELETE"; binding: Partial<Binding> }) => request(method, `/api/workloads/${workloadId}/variable-bindings`, binding), onSuccess: () => client.invalidateQueries({ queryKey: ["variable-bindings", workloadId] }) });
 function submit(event: FormEvent<HTMLFormElement>) {
  event.preventDefault();
  const values = new FormData(event.currentTarget);
  save.mutate({ method: "PUT", binding: { name: formText(values, "name"), source_id: source, source_key: formText(values, "key") } });
 }
 return <Section width="form" title="Resource variable bindings" description="Share a saved variable with this resource. Binding a saved variable, or a database password or connection URL, needs Read secrets on the source resource. Database hostnames, ports, users, names and the public CA certificate need only View on the database. Every binding also needs edit access to this resource. People who can deploy or edit this resource can use the bound value. Changes apply on the next deploy.">
  <div className="space-y-4">
   {bindings.isError && <Notice tone="error">{errorMessage(bindings.error)}</Notice>}
   {bindings.data?.length === 0 && <p className="text-sm text-fg-muted">No resource bindings.</p>}
   <ul className="divide-y divide-line">{bindings.data?.map(binding => <li key={binding.name} className="flex flex-wrap items-center justify-between gap-2 py-2"><div className="min-w-0 text-sm"><code className="break-all">{`{{resource.${binding.name}}}`}</code><p className="text-xs text-fg-muted">{resources.data?.find(item => item.id === binding.source_id)?.name ?? "Restricted resource"} · {binding.source_key}</p></div><Button size="sm" disabled={!online || save.isPending} onClick={() => save.mutate({ method: "DELETE", binding: { name: binding.name } })}>Remove binding</Button></li>)}</ul>
   <form className="space-y-3" onSubmit={submit}>
    <div className="grid gap-3 md:grid-cols-3">
     <Field id="binding-name" label="Binding name"><input className={inputClass} id="binding-name" name="name" required maxLength={128} pattern="[A-Za-z_][A-Za-z0-9_]*" /></Field>
     <Field id="binding-source" label="Source resource"><Select className={inputClass} id="binding-source" value={source} onChange={event => setSource(event.target.value)} required><option value="">Select resource</option>{resources.data?.filter(item => item.id !== workloadId && bindableSource(item)).map(item => <option key={item.id} value={item.id}>{item.project} / {item.name}</option>)}</Select></Field>
     <Field id="binding-key" label="Source variable" hint={hidesSecrets ? "Passwords and connection URLs need Read secrets on this database." : undefined}><Select key={source} className={inputClass} id="binding-key" name="key" required disabled={!detail.data} aria-describedby={hidesSecrets ? "binding-key-hint" : undefined}><option value="">Select variable</option>{sourceKeys.map(key => <option key={key} value={key}>{databaseSource ? `${key} (${exportLabels[key]}${isSecretDatabaseExport(key) ? ", secret" : ""})` : key}</option>)}</Select></Field>
    </div>
    {detail.data?.database && <Notice title="A binding grants no network access">Also create a private connection from this resource to service <span className="font-mono">{detail.data.database.service}</span>, port <span className="font-mono">{detail.data.database.port}</span> and hostname <span className="font-mono break-all">{detail.data.database.host}</span>, then deploy this resource.</Notice>}
    {(resources.isError || detail.isError) && <Notice tone="error">{errorMessage(resources.error ?? detail.error)}</Notice>}
    {save.isError && <Notice tone="error">{errorMessage(save.error)}</Notice>}
    <Button type="submit" disabled={!online || !source} pending={save.isPending}>Save binding</Button>
   </form>
   <p className="text-xs text-fg-muted">Use the reference above in an environment variable value, for example DATABASE_PASSWORD = {"{{resource.DB_PASSWORD}}"}. Values are resolved at deployment time and never returned to this page. Missing variables or circular bindings block deployment.</p>
  </div>
 </Section>;
}

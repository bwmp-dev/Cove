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
import { exportLabels, isDatabaseExport } from "../lib/databases";

const secretExports = new Set(["DB_PASSWORD", "DB_URL"]);

interface Binding { name: string; source_id: string; source_key: string }

export function ResourceVariables({ workloadId }: { workloadId: string }) {
 const client = useQueryClient();
 const online = useOnline();
 const [source, setSource] = useState("");
 const resources = useQuery(workloadsQuery);
 const detail = useQuery({ ...workloadQuery(source), enabled: Boolean(source) });
 const bindings = useQuery({ queryKey: ["variable-bindings", workloadId], queryFn: ({ signal }) => request<Binding[]>("GET", `/api/workloads/${workloadId}/variable-bindings`, undefined, signal) });
 const save = useMutation({ mutationFn: ({ method, binding }: { method: "PUT" | "DELETE"; binding: Partial<Binding> }) => request(method, `/api/workloads/${workloadId}/variable-bindings`, binding), onSuccess: () => client.invalidateQueries({ queryKey: ["variable-bindings", workloadId] }) });
 function submit(event: FormEvent<HTMLFormElement>) {
  event.preventDefault();
  const values = new FormData(event.currentTarget);
  save.mutate({ method: "PUT", binding: { name: formText(values, "name"), source_id: source, source_key: formText(values, "key") } });
 }
 return <Section width="form" title="Resource variable bindings" description="Share a saved variable with this resource. Creating a binding needs edit access to both resources. People who can deploy or edit this resource can use the bound value. Changes apply on the next deploy.">
  <div className="space-y-4">
   {bindings.isError && <Notice tone="error">{errorMessage(bindings.error)}</Notice>}
   {bindings.data?.length === 0 && <p className="text-sm text-fg-muted">No resource bindings.</p>}
   <ul className="divide-y divide-line">{bindings.data?.map(binding => <li key={binding.name} className="flex flex-wrap items-center justify-between gap-2 py-2"><div className="min-w-0 text-sm"><code className="break-all">{`{{resource.${binding.name}}}`}</code><p className="text-xs text-fg-muted">{resources.data?.find(item => item.id === binding.source_id)?.name ?? "Restricted resource"} · {binding.source_key}</p></div><Button size="sm" disabled={!online || save.isPending} onClick={() => save.mutate({ method: "DELETE", binding: { name: binding.name } })}>Remove binding</Button></li>)}</ul>
   <form className="space-y-3" onSubmit={submit}>
    <div className="grid gap-3 md:grid-cols-3">
     <Field id="binding-name" label="Binding name"><input className={inputClass} id="binding-name" name="name" required maxLength={128} pattern="[A-Za-z_][A-Za-z0-9_]*" /></Field>
     <Field id="binding-source" label="Source resource"><Select className={inputClass} id="binding-source" value={source} onChange={event => setSource(event.target.value)} required><option value="">Select resource</option>{resources.data?.filter(item => item.id !== workloadId && can(item.permissions, "edit")).map(item => <option key={item.id} value={item.id}>{item.project} / {item.name}</option>)}</Select></Field>
     <Field id="binding-key" label="Source variable"><Select key={source} className={inputClass} id="binding-key" name="key" required disabled={!detail.data}><option value="">Select variable</option>{detail.data?.environment_keys.map(key => <option key={key} value={key}>{detail.data.database && isDatabaseExport(key) ? `${key} (${exportLabels[key]}${secretExports.has(key) ? ", secret" : ""})` : key}</option>)}</Select></Field>
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

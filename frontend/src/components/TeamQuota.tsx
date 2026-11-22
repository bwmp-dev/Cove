import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { type FormEvent } from "react";
import { request, errorMessage } from "../api/client";
import { useOnline } from "../lib/hooks";
import { Button, Field, LoadingRows, Meter, MeterGroup, Notice, Section, SettingRows } from "./ui";
import { inputClass } from "../lib/styles";

interface Quota { resources: number; cpus: number; memory_mb: number; deployments: number }
const fields = [
  { key: "resources", label: "Resources", meter: "Resources", max: 100000, format: (value: number) => String(value) },
  { key: "cpus", label: "Configured CPU total", meter: "Configured CPU", max: 1000000, format: (value: number) => `${Number(value.toFixed(2))} cores` },
  { key: "memory_mb", label: "Configured memory total (MiB)", meter: "Configured memory", max: 1099511627776, format: (value: number) => (value >= 1024 ? `${Number((value / 1024).toFixed(1))} GiB` : `${value} MiB`) },
  { key: "deployments", label: "Concurrent deployments", meter: "Running deployments", max: 64, format: (value: number) => String(value) },
] as const;

export function TeamQuota({ teamId, admin }: { teamId: string; admin: boolean }) {
 const client = useQueryClient();
 const online = useOnline();
 const key = ["team-quota", teamId];
 const quota = useQuery({ queryKey: key, queryFn: ({ signal }) => request<{ limits: Quota; usage: Quota }>("GET", `/api/teams/${teamId}/quota`, undefined, signal), refetchInterval: 15000 });
 const save = useMutation({ mutationFn: (limits: Quota) => request("PUT", `/api/teams/${teamId}/quota`, limits), onSuccess: () => client.invalidateQueries({ queryKey: key }) });
 function submit(event: FormEvent<HTMLFormElement>) { event.preventDefault(); const data = new FormData(event.currentTarget); save.mutate({ resources: Number(data.get("resources")), cpus: Number(data.get("cpus")), memory_mb: Number(data.get("memory_mb")), deployments: Number(data.get("deployments")) }); }
 return <Section title="Team quotas" description="Instance administrators set resource counts, configured Cove limit budgets, and concurrent deployment slots. Zero means unlimited. Running deployments keep their slots until they finish.">
  <div className="space-y-5">
   {quota.isPending && <LoadingRows label="Loading quotas" rows={1} />}
   {quota.isError && <Notice tone="error">{errorMessage(quota.error)}</Notice>}
   {quota.data && <MeterGroup label="Quota usage" columns={4}>
    {fields.map(field => <Meter key={field.key} label={field.meter} value={quota.data.usage[field.key]} limit={quota.data.limits[field.key]} format={field.format} />)}
   </MeterGroup>}
   <p className="text-xs text-fg-muted">CPU and memory totals count limits saved through Cove, not measured usage or arbitrary Compose settings. Shared workers remain a trust boundary; quotas do not provide tenant isolation.</p>
   {quota.data && admin && <form onSubmit={submit} className="space-y-4 border-t border-line pt-4">
    <SettingRows>
     {fields.map(field => <Field key={field.key} layout="row" id={`quota-${field.key}`} label={field.label} hint={`Currently ${quota.data.usage[field.key]}. Zero means unlimited.`}><input key={teamId + quota.data.limits[field.key]} id={`quota-${field.key}`} name={field.key} className={inputClass} type="number" min={0} max={field.max} step={field.key === "cpus" ? "any" : 1} defaultValue={quota.data.limits[field.key]} required aria-describedby={`quota-${field.key}-hint`} /></Field>)}
    </SettingRows>
    {save.isError && <Notice tone="error">{errorMessage(save.error)}</Notice>}
    <div className="flex justify-end"><Button type="submit" variant="primary" pending={save.isPending} disabled={!online}>Save quotas</Button></div>
   </form>}
  </div>
 </Section>;
}

import { Select } from "./Select";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { useState } from "react";
import { errorMessage, request } from "../api/client";
import type { WorkloadSummary } from "../api/types";
import { useOnline } from "../lib/hooks";
import { can } from "../lib/permissions";
import { inputClass } from "../lib/styles";
import { Dialog } from "./ConfirmDialog";
import { Button, Notice, SwitchField } from "./ui";

export function DeploymentPlan({ projectId, resources }: { projectId: string; resources: WorkloadSummary[] }) {
  const client = useQueryClient();
  const online = useOnline();
  const [open, setOpen] = useState(false);
  const saved = useQuery({ queryKey: ["deployment-sequence", projectId], queryFn: ({ signal }) => request<{ workload_ids: string[]; automatic: boolean }>("GET", `/api/projects/${projectId}/deployment-sequence`, undefined, signal) });
  const [automatic, setAutomatic] = useState(false);
  const save = useMutation({ mutationFn: (value?: { workload_ids: string[]; automatic: boolean }) => request("PUT", `/api/projects/${projectId}/deployment-sequence`, value ?? { workload_ids: steps, automatic }), onSuccess: async () => { await client.invalidateQueries({ queryKey: ["deployment-sequence", projectId] }); } });
  const [steps, setSteps] = useState<string[]>(["", ""]);
  const deploy = useMutation({
    mutationFn: () => request<{ operation_ids: string[] }>("POST", `/api/projects/${projectId}/deployments`, { workload_ids: steps }),
    onSuccess: async () => { await client.invalidateQueries({ queryKey: ["operations"] }); },
  });
  const available = resources.filter(resource => can(resource.permissions, "operate"));
  if (available.length < 2 && !saved.data?.workload_ids.length) return null;
  return <>
    {saved.isError && <Notice tone="error">{errorMessage(saved.error)}</Notice>}
    <Button disabled={!saved.isSuccess} onClick={() => { deploy.reset(); save.reset(); setSteps(saved.data?.workload_ids.length ? saved.data.workload_ids : ["", ""]); setAutomatic(saved.data?.automatic ?? false); setOpen(true); }}>Deploy in order</Button>
    <Dialog open={open} title="Deploy resources in order" onClose={() => { if (!deploy.isPending) setOpen(false); }}>
      <div className="mt-4 space-y-4">
        <p>Each step waits for the previous deployment to succeed. Put migration jobs before the services that need them. Failure or cancellation skips later steps.</p>
        {steps.map((step, index) => <div key={index} className="flex items-center gap-2"><label htmlFor={`deploy-step-${index}`} className="shrink-0">Step {index + 1}</label><Select id={`deploy-step-${index}`} className={inputClass} value={step} disabled={deploy.isPending || deploy.isSuccess} onChange={event => setSteps(steps.map((value, i) => i === index ? event.target.value : value))}><option value="">Select resource</option>{available.filter(resource => resource.id === step || !steps.includes(resource.id)).map(resource => <option key={resource.id} value={resource.id}>{resource.name}{resource.kind === "job" ? " (job)" : ""}</option>)}</Select></div>)}
        {!deploy.isSuccess && <div className="flex gap-2"><Button disabled={deploy.isPending || steps.length >= Math.min(32, available.length)} onClick={() => setSteps([...steps, ""])}>Add step</Button><Button disabled={deploy.isPending || steps.length <= 2} onClick={() => setSteps(steps.slice(0, -1))}>Remove last step</Button></div>}
        <SwitchField id="deploy-sequence-automatic" checked={automatic} onChange={event => setAutomatic(event.target.checked)} label="Run this sequence on matching GitHub pushes" />
        <p className="text-xs text-fg-muted">Automatic runs require every step to have GitHub automatic deployment enabled for the same repository and branch. If a step is stopped or does not match, the entire sequence is skipped. Matching resources use the pushed commit.</p>
        <Button disabled={!online || save.isPending || deploy.isPending || steps.some(step => !step)} onClick={() => save.mutate()}>Save sequence</Button>
        <Button disabled={!online || save.isPending || deploy.isPending} onClick={() => save.mutate({ workload_ids: [], automatic: false }, { onSuccess: () => { setSteps(["", ""]); setAutomatic(false); } })}>Clear saved sequence</Button>
        {save.isError && <Notice tone="error">{errorMessage(save.error)}</Notice>}
        {save.isSuccess && <Notice>Sequence saved.</Notice>}
        <Notice>This explicitly deploys the selected resources, including stopped ones. Database migrations are not automatically reversed if a later step fails.</Notice>
        {deploy.isError && <Notice tone="error">{errorMessage(deploy.error)}</Notice>}
        {deploy.isSuccess ? <div><p>Deployment plan queued.</p><Link to="/operations" onClick={() => setOpen(false)} className="underline">View operations</Link></div> : <Button variant="primary" disabled={!online || deploy.isPending || steps.some(step => !step)} onClick={() => deploy.mutate()}>{deploy.isPending ? "Queueing…" : "Deploy selected resources"}</Button>}
      </div>
    </Dialog>
  </>;
}

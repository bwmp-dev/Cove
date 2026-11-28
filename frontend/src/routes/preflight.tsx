import { useMutation } from "@tanstack/react-query";
import { request, errorMessage } from "../api/client";
import { CapacityPanel } from "../components/CapacityCheck";
import { Button, Notice, Section } from "../components/ui";
import { useOnline } from "../lib/hooks";
import { can } from "../lib/permissions";
import { useResource } from "./workload-detail";
interface Report { revision?: string; checks: { name: string; state: "pass" | "warning" | "error"; message: string }[] }
export function ResourcePreflight() {
  const resource = useResource();
  const online = useOnline();
  const check = useMutation({ mutationFn: () => request<Report>("POST", `/api/workloads/${resource.id}/preflight`), networkMode: "always" });
  return <><Section width="form" title="Deployment preflight" description="Inspect the saved configuration on its worker before deploying. This can fetch Git source and image manifests, but does not build images or start, stop, or replace containers.">
    <div className="space-y-4">
      <Button disabled={!online || !can(resource.permissions, "edit")} pending={check.isPending} onClick={() => check.mutate()}>Run preflight</Button>
      {check.isPending && <p className="text-sm text-fg-muted">Checking the worker, source, Compose configuration, mounts, and image access. This can take two minutes.</p>}
      {check.error && <Notice title="Preflight unavailable" tone="error">{errorMessage(check.error)}</Notice>}
      {check.data && <><p className="text-xs text-fg-muted">Inspection snapshot. Configuration and worker state can change after this check.</p>{check.data.revision && <p className="break-all font-mono text-xs">Git revision: {check.data.revision}</p>}<ul className="divide-y divide-line">{check.data.checks.map((item,index) => <li key={index} className="py-3"><p className="text-sm font-medium">{item.name}: {item.state === "pass" ? "Passed" : item.state === "error" ? "Failed" : "Review"}</p><p className="text-sm text-fg-muted">{item.message}</p></li>)}</ul></>}
    </div>
  </Section>
  <Section width="form" title="Capacity" description="Compare the configured CPU and memory limits of every service and replica with what the node has available now. Read-only: nothing is queued, built or changed. Deploy runs this check too.">
    <CapacityPanel workloadId={resource.id} disabled={!online} />
  </Section></>;
}

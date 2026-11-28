import { Select } from "../components/Select";
import { formText } from "../lib/form-data";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { useId, useState, type FormEvent } from "react";
import { errorMessage, isStatus, request } from "../api/client";
import { keys, workloadOperationsQuery, workloadsQuery } from "../api/queries";
import { Button, Field, Notice, QueryError, Section } from "../components/ui";
import { useOnline } from "../lib/hooks";
import { actionLabels, activeOperation, stateLabels } from "../lib/operations";
import { can } from "../lib/permissions";
import { inputClass, linkClass } from "../lib/styles";
import { useResource } from "./workload-detail";
import { formatAbsolute } from "../lib/format";
import { ReasonField } from "../components/ReasonField";
import { reasonError, withReason } from "../lib/operations";
import { useEnvironmentHold } from "../components/EnvironmentSuspension";
import { holdReason } from "../lib/suspension";
import { useIdleState } from "../components/IdleSleep";
import { idleReason } from "../lib/idle";

// The server rejects promotion while either resource has a queued or running operation, so the form
// waits for both. An unknown status (loading or failed) also blocks, because it may hide an active one.
function useOperationGate(workloadId: string, label: string) {
 const operations = useQuery({ ...workloadOperationsQuery(workloadId), enabled: Boolean(workloadId) });
 const active = operations.data ? activeOperation(operations.data, workloadId) : undefined;
 const blocked = Boolean(workloadId) && (!operations.data || operations.isError || Boolean(active));
 const status = !workloadId ? null
  : operations.isError ? <QueryError what={`${label} operations`} error={operations.error} onRetry={() => void operations.refetch()} retrying={operations.isFetching} />
  : operations.isPending ? <p className="text-xs text-fg-muted">Checking for active operations on the {label}…</p>
  : active ? <p className="flex flex-wrap gap-x-3 gap-y-1 text-xs text-fg-muted"><span>The {label} has a {actionLabels[active.action].toLowerCase()} operation {stateLabels[active.state].toLowerCase()}. Promotion is available when it finishes.</span><Link to="/operations/$operationId" params={{ operationId: active.id }} className={linkClass}>View operation</Link></p>
  : null;
 return { blocked, status };
}

export function Promotion() {
 const target = useResource();
 const client = useQueryClient();
 const online = useOnline();
 const [source, setSource] = useState("");
 const [reason, setReason] = useState("");
 const statusId = useId();
 const resources = useQuery(workloadsQuery);
 const releases = useQuery({ queryKey: ["promotion-releases", source], queryFn: ({ signal }) => request<{ releases: { id: string; created_at: string }[] }>("GET", `/api/workloads/${source}/releases`, undefined, signal), enabled: Boolean(source) });
 const sourceGate = useOperationGate(source, "source resource");
 const targetGate = useOperationGate(target.id, "target resource");
 const idle = useIdleState(target);
 const held = holdReason(useEnvironmentHold(target).hold, "promote_release", "Promoting a release") ?? idleReason(idle.hold, "promote_release", "Promoting a release");
 const promote = useMutation({
  mutationFn: (input: { source_id: string; release: string; registry_repository: string; reason?: string }) => request<{ id: string }>("POST", `/api/workloads/${target.id}/promote`, input),
  onSuccess: async () => { await client.invalidateQueries({ queryKey: keys.operations }); },
  // A 409 means an operation started after the last poll; refresh so the form shows it.
  onError: async error => { if (isStatus(error, 409)) await client.invalidateQueries({ queryKey: keys.operations }); },
 });
 function submit(event: FormEvent<HTMLFormElement>) { event.preventDefault(); if (reasonError(reason)) return; const data = new FormData(event.currentTarget); promote.mutate(withReason({ source_id: source, release: formText(data, "release"), registry_repository: formText(data, "repository").trim() }, reason)); }
 return <Section width="form" title="Promote a release" description="Deploy the exact service images from another resource in this project. Both resources must have identical Compose service names. The target keeps its own Compose settings, variables, networks, routes, and storage. Images are never rebuilt or substituted if unavailable.">
  <form onSubmit={submit} className="space-y-4">
   <Notice tone="warn">Promotion deploys every service image and may restart containers. It does not copy database contents or undo schema changes. Review target settings and backups before promoting.</Notice>
   <Field id="promotion-source" label="Source resource"><Select id="promotion-source" className={inputClass} value={source} onChange={event => setSource(event.target.value)} required><option value="">Select resource</option>{resources.data?.filter(item => item.id !== target.id && item.project_id === target.project_id && item.kind !== "job" && can(item.permissions, "edit")).map(item => <option key={item.id} value={item.id}>{item.environment ?? "Project resource"} / {item.name}</option>)}</Select></Field>
   <Field id="promotion-release" label="Source release"><Select id="promotion-release" name="release" key={source} className={inputClass} required disabled={!releases.data}><option value="">Select successful retained release</option>{releases.data?.releases.map(release => <option key={release.id} value={release.id}>{formatAbsolute(release.created_at)} · {release.id}</option>)}</Select></Field>
   <Field id="promotion-repository" label="Registry repository" optional hint="For images built locally and promotion to another node, enter a repository such as your-org/cove-releases. Cove publishes to the registry assigned to the source and uses immutable digests. Leave empty for same-node promotion or images already published."><input id="promotion-repository" name="repository" className={inputClass} maxLength={200} /></Field>
   <ReasonField id="promotion-reason" value={reason} onChange={setReason} />
   {(resources.isError || releases.isError) && <Notice tone="error">{errorMessage(resources.error ?? releases.error)}</Notice>}
   {promote.isError && <Notice tone={isStatus(promote.error, 409) ? "warn" : "error"}>{errorMessage(promote.error)}</Notice>}
   {promote.data && <Notice title="Promotion queued"><Link to="/operations/$operationId" params={{ operationId: promote.data.id }} className="underline">View deployment</Link></Notice>}
   <div id={statusId} className="space-y-2 empty:hidden">{held && <p className="text-xs text-fg-muted">{held}</p>}{sourceGate.status}{targetGate.status}</div>
   <Button type="submit" pending={promote.isPending} disabled={!online || !source || !can(target.permissions, "edit") || Boolean(reasonError(reason)) || sourceGate.blocked || targetGate.blocked || Boolean(held)} aria-describedby={statusId} variant="primary">Promote release</Button>
   {promote.isPending && <p className="text-sm text-fg-muted">Preparing immutable image references. Publishing images can take several minutes.</p>}
  </form>
 </Section>;
}

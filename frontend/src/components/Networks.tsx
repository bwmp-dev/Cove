import { Select } from "./Select";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Plus, Trash2 } from "lucide-react";
import { useId, useState, type FormEvent } from "react";
import { errorMessage, request } from "../api/client";
import { keys } from "../api/queries";
import type { Workload } from "../api/types";
import { useOnline } from "../lib/hooks";
import { currentTeam, managesTeam } from "../lib/permissions";
import { useSession } from "../lib/session";
import { inputClass } from "../lib/styles";
import { Button, Field, LoadingRows, Notice, QueryError, Section } from "./ui";

interface Network { id: string; name: string; node_id: string }
interface Connection { network_id: string; service: string; alias: string }
const networksKey = ["networks"] as const;

export function Networks({ workload }: { workload: Workload }) {
  const session = useSession();
  const allowed = session?.is_admin || managesTeam(currentTeam(session));
  return <Section title="Private networks" description="Shared networks connect every service on them, on all ports and in both directions, across environments in this team on the same worker. For access to one port in one direction, use a private connection in the environment's resource graph.">
    {allowed ? <NetworkSettings workload={workload} /> : <p className="text-[13px] text-fg-muted">Ask a team owner or admin to manage shared connections between environments.</p>}
  </Section>;
}

function NetworkSettings({ workload }: { workload: Workload }) {
  const networks = useQuery({ queryKey: networksKey, queryFn: ({ signal }) => request<Network[]>("GET", "/api/networks", undefined, signal) });
  const connections = useQuery({ queryKey: [...keys.workload(workload.id), "networks"], queryFn: ({ signal }) => request<Connection[]>("GET", `/api/workloads/${workload.id}/networks`, undefined, signal) });
  if (networks.isPending || connections.isPending) return <LoadingRows label="Loading networks" rows={2} />;
  if (networks.isError || connections.isError) return <QueryError what="networks" error={networks.error ?? connections.error} onRetry={() => { void networks.refetch(); void connections.refetch(); }} />;
  return <div className="space-y-5">
    <Connections key={workload.id} workload={workload} networks={networks.data.filter((network) => network.node_id === workload.node_id)} initial={connections.data} />
    <CreateNetwork nodeId={workload.node_id} />
  </div>;
}

function CreateNetwork({ nodeId }: { nodeId: string }) {
  const id = useId();
  const [name, setName] = useState("");
  const online = useOnline();
  const client = useQueryClient();
  const create = useMutation({ mutationFn: () => request("POST", "/api/networks", { name: name.trim(), node_id: nodeId }), onSuccess: async () => { setName(""); await client.invalidateQueries({ queryKey: networksKey }); } });
  return <form className="space-y-3 border-t border-line pt-4" onSubmit={(event) => { event.preventDefault(); if (!create.isPending && online && name.trim()) create.mutate(); }}>
    <Field id={id} label="New network name" hint="Creates a team-owned network on this resource's worker. Other environments can join it."><input id={id} className={inputClass} value={name} onChange={(event) => setName(event.target.value)} maxLength={120} required /></Field>
    {create.isError && <Notice tone="error" title="Could not create network">{errorMessage(create.error)}</Notice>}
    <Button type="submit" disabled={!online || !name.trim()} pending={create.isPending}>Create private network</Button>
  </form>;
}

function Connections({ workload, networks, initial }: { workload: Workload; networks: Network[]; initial: Connection[] }) {
  const id = useId();
  const online = useOnline();
  const client = useQueryClient();
  const [rows, setRows] = useState(() => initial.map((connection) => ({ ...connection, key: crypto.randomUUID() })));
  const save = useMutation({ mutationFn: () => request("PUT", `/api/workloads/${workload.id}/networks`, { networks: rows.map(({ network_id, service, alias }) => ({ network_id, service: service.trim(), alias: alias.trim() })) }), onSuccess: () => client.invalidateQueries({ queryKey: [...keys.workload(workload.id), "networks"] }) });
  function submit(event: FormEvent) { event.preventDefault(); if (online && !save.isPending) save.mutate(); }
  return <form onSubmit={submit} className="space-y-3">
    <p className="text-xs text-fg-muted">Use the DNS alias and the container port, such as database:5432. Changes apply on the next deploy. Existing public routes or Compose port mappings are unchanged. Other workers cannot join these networks.</p>
    <fieldset disabled={save.isPending} className="space-y-3">
      {rows.map((row, index) => <div key={row.key} className="grid items-end gap-2 sm:grid-cols-3">
        <Field id={`${id}-${row.key}-network`} label="Network"><Select id={`${id}-${row.key}-network`} className={inputClass} value={row.network_id} required onChange={(event) => setRows(rows.map((item) => item.key === row.key ? { ...item, network_id: event.target.value } : item))}><option value="">Select network</option>{networks.map((network) => <option key={network.id} value={network.id}>{network.name}</option>)}</Select></Field>
        <Field id={`${id}-${row.key}-service`} label="Compose service"><input id={`${id}-${row.key}-service`} className={inputClass} value={row.service} required onChange={(event) => setRows(rows.map((item) => item.key === row.key ? { ...item, service: event.target.value } : item))} /></Field>
        <div className="flex items-end gap-2"><div className="min-w-0 flex-1"><Field id={`${id}-${row.key}-alias`} label="DNS alias"><input id={`${id}-${row.key}-alias`} className={inputClass} value={row.alias} required maxLength={63} pattern="[a-z0-9]([a-z0-9-]*[a-z0-9])?" onChange={(event) => setRows(rows.map((item) => item.key === row.key ? { ...item, alias: event.target.value } : item))} /></Field></div><Button variant="ghost" aria-label={`Remove connection ${index + 1}`} onClick={() => setRows(rows.filter((item) => item.key !== row.key))}><Trash2 aria-hidden className="size-4" /></Button></div>
      </div>)}
      <Button disabled={networks.length === 0 || rows.length >= 32} onClick={() => setRows([...rows, { key: crypto.randomUUID(), network_id: "", service: "", alias: "" }])}><Plus aria-hidden className="size-3.5" />Add connection</Button>
    </fieldset>
    {save.isError && <Notice tone="error" title="Could not save connections">{errorMessage(save.error)}</Notice>}
    {save.isSuccess && <Notice title="Connections saved">Deploy this resource to apply the changes. A rollback to different network connections is blocked.</Notice>}
    <Button type="submit" pending={save.isPending} disabled={!online}>Save connections</Button>
  </form>;
}

import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useState, type FormEvent } from "react";
import { request, errorMessage } from "../api/client";
import { formText } from "../lib/form-data";
import { useOnline } from "../lib/hooks";
import { inputClass } from "../lib/styles";
import { Button, Field, Notice, Section } from "./ui";
import { CopyButton } from "./CopyButton";
import { formatAbsolute } from "../lib/format";

interface Lease { id: string; service: string; port: number; expires_at: string }
interface Connection { id: string; expires_at: string; host: string; ssh_port: number; host_key: string }

export function DatabaseAccess({ id }: { id: string }) {
 const client = useQueryClient();
 const online = useOnline();
 const [connection, setConnection] = useState<Connection | null>(null);
 const key = ["database-access", id];
 const path = `/api/workloads/${id}/database-access`;
 const leases = useQuery({ queryKey: key, queryFn: ({ signal }) => request<Lease[]>("GET", path, undefined, signal), refetchInterval: 10000 });
 const open = useMutation({ mutationFn: (body: { service: string; port: number; public_key: string }) => request<Connection>("POST", path, body), onSuccess: data => { setConnection(data); return client.invalidateQueries({ queryKey: key }); } });
 const close = useMutation({ mutationFn: (lease: string) => request("DELETE", path, { id: lease }), onSuccess: () => { setConnection(null); return client.invalidateQueries({ queryKey: key }); } });
 function submit(event: FormEvent<HTMLFormElement>) { event.preventDefault(); const data = new FormData(event.currentTarget); open.mutate({ service: formText(data, "service"), port: Number(data.get("port")), public_key: formText(data, "public_key") }); }
 const quote = (value: string) => `'${value.replaceAll("'", "'\\''")}'`;
 const command = connection ? `cove connect --host ${quote(connection.host)} --ssh-port ${connection.ssh_port} --host-key ${quote(connection.host_key.trim())} --expires ${quote(connection.expires_at)} --local-port 15432 --identity ./cove-access-key` : "";
 return <Section width="form" title="Temporary database access" description="Open a 15-minute SSH connection to one running service. No database port is published. Expiry and revocation close active connections. The database still requires its own credentials.">
  <div className="space-y-4">
   <p className="text-sm text-fg-muted">Generate a dedicated Ed25519 key with ssh-keygen, then paste its public key here. Run the Cove CLI locally with OpenSSH available. It binds only 127.0.0.1 and pins the worker's host key. This requires an SSH-enrolled worker reachable from your computer.</p>
   <form onSubmit={submit} className="space-y-3">
    <div className="grid gap-3 sm:grid-cols-2"><Field id="access-service" label="Database service"><input id="access-service" name="service" className={inputClass} required /></Field><Field id="access-port" label="Container port"><input id="access-port" name="port" className={inputClass} type="number" min={1} max={65535} defaultValue={5432} required /></Field></div>
    <Field id="access-key" label="Ed25519 public key"><textarea id="access-key" name="public_key" className={inputClass} rows={2} maxLength={8192} required /></Field>
    <Button type="submit" disabled={!online} pending={open.isPending}>Open temporary access</Button>
   </form>
   {(open.isError || close.isError || leases.isError) && <Notice tone="error">{errorMessage(open.error ?? close.error ?? leases.error)}</Notice>}
   {connection && <div className="space-y-2"><p className="text-sm">Run locally, replacing the identity path and local port if needed:</p><pre role="region" aria-label="Local database connection command" className="overflow-x-auto rounded border border-line p-3 text-xs" tabIndex={0}>{command}</pre><CopyButton text={command} label="Copy connection command" /></div>}
   <ul className="divide-y divide-line">{leases.data?.map(lease => <li key={lease.id} className="flex flex-wrap items-center justify-between gap-3 py-2"><div className="text-sm">{lease.service}:{lease.port}<p className="text-xs text-fg-muted">Expires {formatAbsolute(lease.expires_at)}</p></div><Button size="sm" disabled={!online || close.isPending} onClick={() => close.mutate(lease.id)}>Revoke access</Button></li>)}</ul>
  </div>
 </Section>;
}

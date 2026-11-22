import { Select } from "./Select";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useState, type FormEvent } from "react";
import { errorMessage, request } from "../api/client";
import { connectionsQuery, type Connection, type ConnectionKind } from "../api/connections";
import { nodesQuery } from "../api/queries";
import { useOnline } from "../lib/hooks";
import { currentTeam, managesTeam } from "../lib/permissions";
import { useSession } from "../lib/session";
import { choiceClass, inputClass } from "../lib/styles";
import { Checkbox } from "./Checkbox";
import { ConfirmDialog } from "./ConfirmDialog";
import { Button, EmptyState, Field, LoadingRows, Notice, QueryError, Section, SwitchField } from "./ui";

function ConnectionForm({ kind, initial, close }: { kind: ConnectionKind; initial: Connection | null; close: () => void }) {
  const client = useQueryClient();
  const online = useOnline();
  const [protocol, setProtocol] = useState(initial?.storage?.protocol ?? "s3");
  const save = useMutation({ gcTime: 0, mutationFn: (body: Omit<Connection, "id">) => request(initial ? "PUT" : "POST", `/api/${kind}${initial ? `/${initial.id}` : ""}`, body), onSuccess: async () => { await client.invalidateQueries({ queryKey: ["connections", kind] }); close(); } });
  function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    const data = new FormData(event.currentTarget);
    const text = (key: string) => { const value = data.get(key); return typeof value === "string" ? value : ""; };
    const body: Omit<Connection, "id"> = { name: text("name").trim() };
    if (kind === "registries") body.registry = { server: text("server").trim(), username: text("username"), password: text("password") };
    if (kind === "storage-locations") body.storage = { ca_certificate: text("ca_certificate"), protocol, endpoint: text("endpoint").trim(), path: text("path").trim(), username: text("username"), password: text("password"), access_key: text("access_key"), secret_key: text("secret_key"), region: text("region"), host_key: text("host_key").trim(), tls_mode: text("tls_mode") };
    if (kind === "notifications") body.notification = { provider: text("provider"), url: text("url").trim(), enabled: data.has("enabled"), success: data.has("success"), failure: data.has("failure"), health: data.has("health") };
    save.mutate(body);
  }
  function field(name: string, label: string, value = "", secret = false, required = true, hint?: string) {
    return <Field key={name} id={`connection-${name}`} label={label} hint={hint} optional={!required}><input className={inputClass} id={`connection-${name}`} name={name} defaultValue={secret ? "" : value} type={secret ? "password" : "text"} autoComplete="off" required={required && !(secret && initial)} /></Field>;
  }
  return <div className="max-w-4xl"><Section title={initial ? `Edit ${initial.name}` : "New connection"}><form className="space-y-4" onSubmit={submit}>
    {field("name", "Name", initial?.name)}
    {initial && <p className="text-sm text-fg-muted">Leave secrets blank to keep them for the same destination and username. Changing the destination requires new credentials.</p>}
    {kind === "registries" && <>{field("server", "Registry host", initial?.registry?.server, false, true, "For example ghcr.io, docker.io, or registry.example.com:5000. HTTPS is required.")}{field("username", "Username", initial?.registry?.username)}{field("password", "Password or access token", "", true)}</>}
    {kind === "storage-locations" && <>
      <Field id="connection-protocol" label="Protocol"><Select className={inputClass} id="connection-protocol" value={protocol} onChange={(e) => setProtocol(e.target.value)}><option value="s3">S3</option><option value="sftp">SFTP</option><option value="ftps">FTPS</option><option value="webdav">WebDAV</option></Select></Field>
      {field("endpoint", "Endpoint", initial?.storage?.endpoint, false, true, protocol === "sftp" ? "sftp://host:22" : protocol === "ftps" ? "ftps://host:21" : "https://host (include the base path for WebDAV)")}
      {field("path", protocol === "s3" ? "Bucket and path" : "Directory", initial?.storage?.path, false, true, "Cove adds a resource-specific directory. Backups are encrypted with the resource's backup password.")}
      {protocol === "s3" ? <>{field("access_key", "Access key", initial?.storage?.access_key)}{field("secret_key", "Secret key", "", true)}{field("region", "Region", initial?.storage?.region, false, false)}</> : <>{field("username", "Username", initial?.storage?.username)}{field("password", "Password", "", true)}</>}
      {protocol === "sftp" && field("host_key", "SSH host public key", initial?.storage?.host_key, false, true, "Verify this key with your server administrator. Example: ssh-ed25519 AAAA…")}
      {protocol !== "sftp" && <Field id="connection-ca" label="Private CA certificate" optional hint="PEM CA certificate for a private endpoint. Leave empty for public certificate authorities."><textarea id="connection-ca" name="ca_certificate" className={inputClass} rows={3} defaultValue={initial?.storage?.ca_certificate ?? ""} /></Field>}
      {protocol === "ftps" && <Field id="connection-tls" label="TLS mode"><Select id="connection-tls" name="tls_mode" className={inputClass} defaultValue={initial?.storage?.tls_mode ?? "explicit"}><option value="explicit">Explicit TLS (usually port 21)</option><option value="implicit">Implicit TLS (usually port 990)</option></Select></Field>}
    </>}
    {kind === "notifications" && <>
      <Field id="connection-provider" label="Provider"><Select className={inputClass} id="connection-provider" name="provider" defaultValue={initial?.notification?.provider ?? "webhook"}><option value="webhook">JSON webhook</option><option value="discord">Discord</option><option value="slack">Slack</option></Select></Field>
      {field("url", "Webhook URL", "", true, true, "An HTTPS endpoint reachable at a public address. The URL is stored encrypted.")}
      <SwitchField id="connection-enabled" name="enabled" defaultChecked={initial?.notification?.enabled ?? true} label="Enable notifications" />
      <fieldset className="space-y-2"><legend className="mb-2 text-[13px] font-medium text-fg">Events</legend><div className="flex flex-wrap gap-1.5"><label className={choiceClass}><Checkbox name="success" defaultChecked={initial?.notification?.success ?? false} />Successful operations</label><label className={choiceClass}><Checkbox name="failure" defaultChecked={initial?.notification?.failure ?? true} />Failed or canceled operations</label><label className={choiceClass}><Checkbox name="health" defaultChecked={initial?.notification?.health ?? false} />Health alerts and recovery</label></div></fieldset>
    </>}
    {save.isError && <Notice tone="error">{errorMessage(save.error)}</Notice>}
    <div className="flex gap-2"><Button type="submit" variant="primary" pending={save.isPending} disabled={!online}>Save</Button><Button onClick={close} disabled={save.isPending}>Cancel</Button></div>
  </form></Section></div>;
}

function ConnectionTest({ kind, connection, close }: { kind: ConnectionKind; connection: Connection; close: () => void }) {
  const nodes = useQuery({ ...nodesQuery, enabled: kind !== "notifications" });
  const [node, setNode] = useState("");
  const online = useOnline();
  const test = useMutation({ mutationFn: () => request("POST", `/api/${kind}/${connection.id}/test`, kind === "notifications" ? {} : { node_id: node }) });
  return <Section title={`Test ${connection.name}`}><div className="max-w-xl space-y-4">
    {kind === "notifications" ? <p className="text-sm text-fg-muted">This sends a test message to the saved webhook, even if notifications are disabled.</p> : <><p className="text-sm text-fg-muted">Tests run from the selected node. Preparing the storage helper for the first test can take up to three minutes.</p><Field id="test-node" label="Node"><Select id="test-node" className={inputClass} value={node} onChange={(e) => setNode(e.target.value)}><option value="">Select a node</option>{nodes.data?.filter((n) => n.state === "ready" && n.capabilities?.infrastructure).map((n) => <option key={n.id} value={n.id}>{n.name}</option>)}</Select></Field>{nodes.isError && <Notice tone="error">{errorMessage(nodes.error)}</Notice>}</>}
    {test.isError && <Notice tone="error">{errorMessage(test.error)}</Notice>}{test.isSuccess && <Notice tone="success">Connection test succeeded.</Notice>}
    <div className="flex gap-2"><Button variant="primary" onClick={() => test.mutate()} pending={test.isPending} disabled={!online || (kind !== "notifications" && !node)}>Run test</Button><Button onClick={close} disabled={test.isPending}>Close</Button></div>
  </div></Section>;
}

export function Connections({ kind }: { kind: ConnectionKind }) {
  const session = useSession();
  const manager = Boolean(session?.is_admin) || managesTeam(currentTeam(session));
  const query = useQuery(connectionsQuery(kind));
  const client = useQueryClient();
  const online = useOnline();
  const [edit, setEdit] = useState<Connection | "new" | null>(null);
  const [test, setTest] = useState<Connection | null>(null);
  const [remove, setRemove] = useState<Connection | null>(null);
  const deletion = useMutation({ mutationFn: (id: string) => request("DELETE", `/api/${kind}/${id}`), onSuccess: async () => { setRemove(null); await client.invalidateQueries({ queryKey: ["connections", kind] }); } });
  if (query.isPending) return <LoadingRows label="Loading connections" />;
  if (query.isError) return <QueryError what="connections" error={query.error} onRetry={() => void query.refetch()} />;
  if (edit) return <ConnectionForm key={edit === "new" ? "new" : edit.id} kind={kind} initial={edit === "new" ? null : edit} close={() => setEdit(null)} />;
  if (test) return <ConnectionTest kind={kind} connection={test} close={() => setTest(null)} />;
  return <div className="min-w-0 space-y-4 px-4 pt-5 sm:px-6">
    {manager && <Button variant="primary" disabled={!online} onClick={() => setEdit("new")}>Add {kind === "registries" ? "registry" : kind === "notifications" ? "notification" : "storage location"}</Button>}
    {deletion.isError && <Notice tone="error">{errorMessage(deletion.error)}</Notice>}
    {query.data.length === 0 ? <EmptyState title="No connections yet">{manager ? "Add a connection for this team." : "A team administrator can add connections."}</EmptyState> : <ul className="divide-y divide-line">{query.data.map((connection) => <li key={connection.id} className="flex flex-wrap items-center justify-between gap-3 py-4"><div className="min-w-0"><h2 className="text-sm font-medium">{connection.name}</h2><p className="break-all text-xs text-fg-muted">{connection.registry?.server ?? (connection.storage ? `${connection.storage.protocol.toUpperCase()} · ${connection.storage.endpoint} · ${connection.storage.path}` : `${connection.notification?.provider} · ${connection.notification?.enabled ? "Enabled" : "Disabled"}`)}</p></div>{manager && <div className="flex gap-2"><Button size="sm" disabled={!online} onClick={() => setTest(connection)}>Test</Button><Button size="sm" disabled={!online} onClick={() => setEdit(connection)}>Edit</Button><Button size="sm" disabled={!online || deletion.isPending} onClick={() => setRemove(connection)}>Delete</Button></div>}</li>)}</ul>}
    <ConfirmDialog open={remove !== null} title={`Delete ${remove?.name ?? "connection"}?`} confirmLabel="Delete" danger onCancel={() => setRemove(null)} onConfirm={() => { if (remove) deletion.mutate(remove.id); }}>Connections in use by a resource cannot be deleted. Removing a notification also removes its delivery history.</ConfirmDialog>
  </div>;
}

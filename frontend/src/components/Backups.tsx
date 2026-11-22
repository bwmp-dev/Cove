import { Select } from "./Select";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useState, type FormEvent } from "react";
import { Link } from "@tanstack/react-router";
import { errorMessage, request } from "../api/client";
import { connectionsQuery } from "../api/connections";
import { keys } from "../api/queries";
import { useOnline } from "../lib/hooks";
import { calendarRetention, calendarTiers, KEEP_LAST_MAX, retentionSummary, usesCalendarRetention, type CalendarRetention } from "../lib/backups";
import { inputClass } from "../lib/styles";
import { ConfirmDialog, Dialog } from "./ConfirmDialog";
import { RelativeTime } from "./RelativeTime";
import { Button, Field, Notice, QueryError } from "./ui";

interface DatabaseBackup { engine: string; service: string; name: string }
interface Policy { drill_interval_hours?: number; database?: DatabaseBackup; storage_id?: string; repository: string; keep_last: number; keep_daily?: number; keep_weekly?: number; keep_monthly?: number; interval_hours: number; ca_certificate?: string }
interface Snapshot { verified_at?: string; verification?: string; id: string; created_at: string; repository: string; database?: DatabaseBackup }

// Unknown when the worker's capabilities cannot be read; the API still refuses unsupported rules.
export interface RetentionSupport { supported: boolean | undefined; nodeName?: string }

function PolicyForm({ workloadId, initial, retention, onClose }: { workloadId: string; initial: Policy | null; retention: RetentionSupport; onClose: () => void }) {
  const client = useQueryClient();
  const online = useOnline();
  const locations = useQuery(connectionsQuery("storage-locations"));
  const [storage, setStorage] = useState(initial?.storage_id ?? "");
  const [engine, setEngine] = useState(initial?.database?.engine ?? "");
  const [retentionError, setRetentionError] = useState<string>();
  const savedCalendar = calendarRetention(initial);
  const save = useMutation({
    gcTime: 0,
    mutationFn: (backup: (Policy & { password: string; access_key: string; secret_key: string }) | null) => request("PUT", `/api/workloads/${workloadId}/backup-policy`, { backup }),
    onSuccess: async () => { await client.invalidateQueries({ queryKey: ["backup-policy", workloadId] }); onClose(); },
  });
  function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    const values = new FormData(event.currentTarget);
    const text = (name: string) => { const value = values.get(name); return typeof value === "string" ? value : ""; };
    const calendar: CalendarRetention = { keep_daily: Number(text("keep-daily")), keep_weekly: Number(text("keep-weekly")), keep_monthly: Number(text("keep-monthly")) };
    if (retention.supported === false && usesCalendarRetention(calendar)) {
      setRetentionError(`Set these to 0, or upgrade and verify ${retention.nodeName ?? "this resource's worker"} first.`);
      const control = event.currentTarget.elements.namedItem(calendar.keep_daily > 0 ? "keep-daily" : calendar.keep_weekly > 0 ? "keep-weekly" : "keep-monthly");
      if (control instanceof HTMLElement) control.focus();
      return;
    }
    setRetentionError(undefined);
    save.mutate({ database: engine ? { engine, service: text("db-service"), name: text("db-name") } : undefined, storage_id: storage, repository: text("repository").trim(), password: text("password"), access_key: text("access"), secret_key: text("secret"), keep_last: Number(text("keep")), ...calendar, interval_hours: Number(text("interval")), drill_interval_hours: Number(text("drill-interval")), ca_certificate: text("ca") });
  }
  return <form className="space-y-4" onSubmit={submit}>
    <Field id="backup-mode" label="Backup method"><Select id="backup-mode" className={inputClass} value={engine} onChange={event => setEngine(event.target.value)}><option value="">Consistent volume snapshot (stops containers)</option><option value="postgresql">PostgreSQL logical dump (online)</option><option value="mariadb">MariaDB logical dump (online, InnoDB only)</option></Select></Field>
    {engine && <><Field id="backup-db-service" label="Database Compose service"><input id="backup-db-service" name="db-service" className={inputClass} defaultValue={initial?.database?.service} required /></Field><Field id="backup-db-name" label="Database name"><input id="backup-db-name" name="db-name" className={inputClass} defaultValue={initial?.database?.name} required /></Field><Notice>Uses the database container’s standard PostgreSQL or MariaDB credentials. Backups include one database, not server roles. Use volume snapshots for TimescaleDB, ClickHouse, Redis, or custom database images.</Notice></>}
    <Field id="backup-storage" label="Storage location"><Select id="backup-storage" className={inputClass} value={storage} onChange={(event) => setStorage(event.target.value)}><option value="">Local or custom S3 repository</option>{locations.data?.map((item) => <option key={item.id} value={item.id}>{item.name}</option>)}</Select></Field>
    {locations.isError && <Notice tone="error">{errorMessage(locations.error)}</Notice>}
    {!storage && <Field id="backup-repository" label="Repository" hint="Use local, or s3:https://endpoint/bucket/path. Local backups do not protect against losing this node."><input className={inputClass} id="backup-repository" name="repository" defaultValue={initial?.storage_id ? "local" : initial?.repository ?? "local"} required /></Field>}
    <Field id="backup-drill-interval" label="Restore verification interval (hours)" hint="0 disables automatic drills. Logical database dumps are restored into isolated containers; volume snapshots are restored and checksum-verified in temporary storage without testing application startup."><input id="backup-drill-interval" name="drill-interval" type="number" min="0" max="8760" step="1" className={inputClass} defaultValue={initial?.drill_interval_hours ?? 0} required /></Field>
    <Field id="backup-password" label="Encryption password" hint={initial ? "Leave blank to keep the saved password for the same repository." : "At least 16 characters. Keep a copy outside Cove for disaster recovery."}><input className={inputClass} id="backup-password" name="password" type="password" autoComplete="new-password" minLength={16} required={!initial} /></Field>
    {!storage && <><Field id="backup-access" label="S3 access key" optional><input className={inputClass} id="backup-access" name="access" type="password" autoComplete="off" /></Field>
    <Field id="backup-secret" label="S3 secret key" optional hint="Leave credentials blank to keep saved values for the same repository."><input className={inputClass} id="backup-secret" name="secret" type="password" autoComplete="off" /></Field>
    <Field id="backup-ca" label="Private CA certificate" optional hint="PEM certificate for a private S3 endpoint. Public providers use the system trust store."><textarea className={inputClass} id="backup-ca" name="ca" defaultValue={initial?.ca_certificate ?? ""} rows={3} /></Field></>}
    <Field id="backup-interval" label="Interval in hours" hint="0 means manual backups only."><input className={inputClass} id="backup-interval" name="interval" type="number" min={0} max={8760} defaultValue={initial?.interval_hours ?? 24} required aria-describedby="backup-interval-hint" /></Field>
    <fieldset className="min-w-0 space-y-3" aria-describedby="backup-retention-help">
      <legend className="text-[13px] font-semibold">Retention</legend>
      <p id="backup-retention-help" className="text-xs text-fg-muted">Cove keeps every snapshot matched by any rule: the latest snapshots, plus the newest snapshot from each of the most recent days, ISO weeks and calendar months (UTC) that have backups. Periods without backups do not use a slot. Set a period to 0 to turn it off. Older snapshots in this repository are pruned only after a successful backup.</p>
      {retention.supported === false && <Notice tone="warn" title="Daily, weekly and monthly retention needs an upgraded worker">Upgrade and verify {retention.nodeName ?? "this resource's worker"} to keep snapshots by day, week or month. Latest-snapshot retention works on every worker.</Notice>}
      <div className="grid gap-4 sm:grid-cols-2">
        <Field id="backup-keep" label="Latest snapshots" hint={`Always kept, 1 to ${KEEP_LAST_MAX}.`}><input className={inputClass} id="backup-keep" name="keep" type="number" min={1} max={KEEP_LAST_MAX} step={1} defaultValue={initial?.keep_last ?? 7} required aria-describedby="backup-keep-hint" /></Field>
        {calendarTiers.map((tier) => {
          const id = `backup-${tier.key.replace("_", "-")}`;
          return <Field key={tier.key} id={id} label={tier.label} hint={`Last 0 to ${tier.max} ${tier.unit} with backups.`}><input className={inputClass} id={id} name={tier.key.replace("_", "-")} type="number" min={0} max={tier.max} step={1} defaultValue={savedCalendar[tier.key]} required aria-invalid={retentionError ? true : undefined} aria-describedby={retentionError ? `${id}-hint backup-retention-error` : `${id}-hint`} /></Field>;
        })}
      </div>
      {retentionError && <p id="backup-retention-error" className="text-xs text-bad">{retentionError}</p>}
    </fieldset>
    {save.isError && <Notice tone="error">{errorMessage(save.error)}</Notice>}
    <div className="flex flex-wrap gap-2"><Button type="submit" variant="primary" pending={save.isPending} disabled={!online}>Save policy</Button><Button onClick={onClose} disabled={save.isPending}>Cancel</Button>{initial && <Button disabled={!online || save.isPending} onClick={() => save.mutate(null)}>Disable backups</Button>}</div>
  </form>;
}

export function Backups({ workloadId, busy, canOperate, canEdit, retention }: { workloadId: string; busy: boolean; canOperate: boolean; canEdit: boolean; retention: RetentionSupport }) {
 const [copy, setCopy] = useState<Snapshot | null>(null);
  const [editing, setEditing] = useState(false);
  const [restore, setRestore] = useState<Snapshot | null>(null);
  const online = useOnline();
  const client = useQueryClient();
  const policy = useQuery({ queryKey: ["backup-policy", workloadId], queryFn: ({ signal }) => request<{ backup: Policy | null }>("GET", `/api/workloads/${workloadId}/backup-policy`, undefined, signal) });
  const snapshots = useQuery({ queryKey: ["backups", workloadId], queryFn: ({ signal }) => request<{ backups: Snapshot[] }>("GET", `/api/workloads/${workloadId}/backups`, undefined, signal), refetchInterval: busy ? 3000 : 15000 });
  const action = useMutation({ mutationFn: (input: { action: "backup" | "restore" | "drill"; backup_id?: string }) => request("POST", `/api/workloads/${workloadId}/operations`, input), onSuccess: async () => { setRestore(null); await client.invalidateQueries({ queryKey: keys.operations }); } });
  if (policy.isError) return <QueryError what="backup policy" error={policy.error} onRetry={() => void policy.refetch()} />;
  if (!policy.data) return <p className="text-sm text-fg-muted">Loading backup policy…</p>;
  if (editing) return <PolicyForm workloadId={workloadId} initial={policy.data.backup} retention={retention} onClose={() => setEditing(false)} />;
  const saved = policy.data.backup;
  return <div className="space-y-4">
    <div className="flex flex-wrap items-center gap-3 empty:hidden">{canEdit && <Button disabled={!online || busy} onClick={() => setEditing(true)}>{saved ? "Edit policy" : "Configure backups"}</Button>}{saved && canOperate && <Button disabled={!online || busy} pending={action.isPending} onClick={() => action.mutate({ action: "backup" })}>Back up now</Button>}</div>
    {!saved && !canEdit && <p className="text-sm text-fg-muted">No backup policy is configured. Editor access is needed to configure one.</p>}
    {saved && <p className="text-xs text-fg-muted">{saved.repository} · {retentionSummary(saved)} · {saved.interval_hours ? `Every ${saved.interval_hours} hours` : "Manual only"}</p>}
    {action.isError && <Notice tone="error">{errorMessage(action.error)}</Notice>}
    {copy && <RestoreCopyForm workloadId={workloadId} snapshot={copy} onClose={() => setCopy(null)} />}
    {snapshots.isError ? <QueryError what="backups" error={snapshots.error} onRetry={() => void snapshots.refetch()} /> : snapshots.data?.backups.length ? <ul className="divide-y divide-line">{snapshots.data.backups.map((snapshot) => <li key={snapshot.id} className="flex flex-wrap items-center justify-between gap-3 py-3"><div><RelativeTime value={snapshot.created_at} /><p className="text-xs text-fg-muted break-all">{snapshot.id}</p>{snapshot.verified_at && <p className="text-xs text-fg-muted">{snapshot.verification === "database" ? "Database restore verified" : "File restore verified"} <RelativeTime value={snapshot.verified_at} /></p>}</div>{canEdit && <div className="flex flex-wrap gap-2"><Button size="sm" disabled={!online || busy || action.isPending || saved?.repository !== snapshot.repository} onClick={() => setCopy(snapshot)}>Restore as new resource</Button><Button size="sm" disabled={!online || busy || action.isPending || saved?.repository !== snapshot.repository} onClick={() => action.mutate({ action: "drill", backup_id: snapshot.id })}>Verify restore</Button><Button size="sm" disabled={!online || busy || action.isPending || saved?.repository !== snapshot.repository} onClick={() => setRestore(snapshot)}>Restore</Button></div>}</li>)}</ul> : <p className="text-sm text-fg-muted">No backups yet.</p>}
    <ConfirmDialog open={restore !== null} title="Restore persistent data?" confirmLabel="Restore data" danger onCancel={() => setRestore(null)} onConfirm={() => { if (restore) action.mutate({ action: "restore", backup_id: restore.id }); }}>
      {restore?.database ? "Stop all applications that write to this database before restoring. Cove saves a logical safety backup. PostgreSQL replaces backed-up objects transactionally; objects absent from the backup are not removed. MariaDB replaces the database. A failed restore stops the database for recovery. Server roles must already exist." : "Containers will stop. Cove saves a safety snapshot, replaces data in the current volumes, and restarts the current application version. Files added since this backup are removed. A failed restore leaves containers stopped for recovery."}
    </ConfirmDialog>
  </div>;
}

function RestoreCopyForm({ workloadId, snapshot, onClose }: { workloadId: string; snapshot: Snapshot; onClose: () => void }) {
 const [name, setName] = useState("");
 const online = useOnline();
 const client = useQueryClient();
 const copy = useMutation({ mutationFn: () => request<{ id: string; resource_id: string }>("POST", `/api/workloads/${workloadId}/restore-copy`, { name, backup_id: snapshot.id }), onSuccess: async () => { await client.invalidateQueries(); } });
 return <Dialog open title="Restore as a separate resource" onClose={onClose}>
  <form className="space-y-4" onSubmit={event => { event.preventDefault(); copy.mutate(); }}>
   <Notice>The new resource stays offline on this worker, with fresh storage, an isolated network, and no public routes. It copies the backup release's saved credentials. External read-only mounts, mounted secrets, and privileged configurations require a manual recovery configuration.</Notice>
   {snapshot.database && <p className="text-sm text-fg-muted">Only this database's dump is restored. Other service volumes start empty. PostgreSQL ownership and grants are not copied; review access before deploying.</p>}
   <Field id="restore-copy-name" label="New resource name"><input id="restore-copy-name" className={inputClass} value={name} onChange={event => setName(event.target.value)} required maxLength={120} disabled={copy.isPending || copy.isSuccess} /></Field>
   {copy.isError && <Notice tone="error">{errorMessage(copy.error)}</Notice>}
   {copy.data ? <div className="flex flex-wrap gap-3"><Link to="/operations/$operationId" params={{ operationId: copy.data.id }} onClick={onClose}>View restore progress</Link><Link to="/resources/$resourceId" params={{ resourceId: copy.data.resource_id }} onClick={onClose}>Open new resource</Link></div> : <Button type="submit" disabled={!online} pending={copy.isPending}>Restore separate resource</Button>}
   <Button disabled={copy.isPending} onClick={onClose}>Close</Button>
  </form>
 </Dialog>;
}

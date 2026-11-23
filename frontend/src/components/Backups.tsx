import { Select } from "./Select";
import { useMutation, useQuery, useQueryClient, type UseQueryResult } from "@tanstack/react-query";
import { useState, type FormEvent, type ReactNode } from "react";
import { Link } from "@tanstack/react-router";
import { errorMessage, request } from "../api/client";
import { backupPolicyQuery, type BackupPolicy } from "../api/backups";
import { connectionsQuery } from "../api/connections";
import { keys } from "../api/queries";
import { useOnline } from "../lib/hooks";
import { backupDownloadUrl } from "../api/backup-transfers";
import { backupScopeLabel, calendarRetention, downloadFileName, matchesPolicy, calendarTiers, databaseEngine, databaseEngines, INSTANCE_SCOPE_NAME, KEEP_LAST_MAX, parseSnapshotSpace, retentionSummary, SNAPSHOT_SPACE_NAME, SNAPSHOT_SWITCH_NAME, usesCalendarRetention, volumeMethodLabel, type CalendarRetention, type DatabaseBackup, type VolumeSnapshotPolicy } from "../lib/backups";
import { buttonClass, inputClass } from "../lib/styles";
import { BackupImport } from "./BackupImport";
import { ConfirmDialog, Dialog } from "./ConfirmDialog";
import { RelativeTime } from "./RelativeTime";
import { Button, Field, Notice, QueryError } from "./ui";
import { VolumeSnapshotSettings } from "./VolumeSnapshotSettings";

type Policy = BackupPolicy;
interface Snapshot { verified_at?: string; verification?: string; id: string; created_at: string; repository: string; database?: DatabaseBackup; volume_method?: string; fallback_reason?: string }

// Each flag is unknown when the worker's capabilities cannot be read; the API still refuses unsupported policies.
export interface WorkerSupport { retention: boolean | undefined; instanceEngines: boolean | undefined; transfers?: boolean | undefined; snapshots?: boolean | undefined; nodeName?: string }

function EngineNotice({ engine }: { engine: string }) {
  if (engine === "mongodb") return <Notice title="MongoDB requirements">
    <p>Backs up a non-sharded replica set with <code className="font-mono">mongodump</code> and <code className="font-mono">mongosh</code> from the database container, as one archive of the complete instance including the oplog. Standalone servers cannot be backed up. If authentication is on, the container needs <code className="font-mono">MONGO_INITDB_ROOT_USERNAME</code> and <code className="font-mono">MONGO_INITDB_ROOT_PASSWORD</code> (or their <code className="font-mono">_FILE</code> variants).</p>
    <p className="mt-1.5">The source keeps running. Each archive (up to 64 GiB) is restored into an isolated container with 2 GiB of memory before it is published; a failed check publishes no backup. Restoring in place needs a standalone server or a writable single-member replica set, so restore backups of larger replica sets as a new resource.</p>
  </Notice>;
  if (engine === "redis" || engine === "valkey") {
    const name = engine === "redis" ? "Redis" : "Valkey";
    const prefix = engine === "redis" ? "REDIS" : "VALKEY";
    return <Notice title={`${name} requirements`}>
      <p>Streams an RDB snapshot of every logical database with <code className="font-mono">{engine}-cli --rdb</code> from the database container. If authentication is on, the container needs <code className="font-mono">{prefix}_PASSWORD</code> (or <code className="font-mono">{prefix}_PASSWORD_FILE</code>).</p>
      <p className="mt-1.5">The source keeps running. Each snapshot (up to 64 GiB) is restored into an isolated container with 2 GiB of memory before it is published; a failed check publishes no backup. Restoring needs persistent <code className="font-mono">/data</code> with the default <code className="font-mono">dump.rdb</code>, <code className="font-mono">appendonly.aof</code> and <code className="font-mono">appendonlydir</code> paths; custom paths are refused.</p>
    </Notice>;
  }
  return <Notice>Uses the database container’s standard PostgreSQL or MariaDB credentials. Backups include one database, not server roles. Use volume snapshots for TimescaleDB, ClickHouse, or custom database images.</Notice>;
}

function restoreWarning(engine: string | undefined) {
  switch (engine) {
    case undefined: return "Containers will stop. Cove saves a safety snapshot, replaces data in the current volumes, and restarts the current application version. Files added since this backup are removed. A failed restore leaves containers stopped for recovery.";
    case "postgresql": return "Stop all applications that write to this database before restoring. Cove saves a logical safety backup. PostgreSQL replaces backed-up objects transactionally; objects absent from the backup are not removed. A failed restore stops the database for recovery. Server roles must already exist.";
    case "mariadb": return "Stop all applications that write to this database before restoring. Cove saves a logical safety backup, then MariaDB replaces the database. A failed restore stops the database for recovery. Server roles must already exist.";
    case "mongodb": return "Cove saves a logical safety backup and stops this resource's other containers, then replaces every user database with the archive. The target must be a standalone server or a writable single-member replica set; restore backups of larger replica sets as a new resource. Root credentials in the backup must match the configured root user. A failed restore stops the database for recovery, and retrying from the safety backup restarts only the containers that were running.";
    case "redis":
    case "valkey": return "Cove saves a safety backup and pauses this resource's containers, then replaces every logical database with the verified RDB snapshot and regenerates the append-only file from it. Data must be in persistent /data at the default dump.rdb, appendonly.aof and appendonlydir paths. A failed restore stops the database for recovery, and retrying from the safety backup restarts only the containers that were running.";
    default: return "Cove saves a logical safety backup and replaces the database. A failed restore stops the database for recovery.";
  }
}

function restoreCopyScope(database: DatabaseBackup) {
  const engine = databaseEngine(database.engine);
  return engine?.scope === "instance" ? `The complete ${engine.name} instance snapshot is restored into the database service.` : "Only this database's dump is restored.";
}

function DownloadDialog({ workloadId, snapshot, onClose }: { workloadId: string; snapshot: Snapshot; onClose: () => void }) {
  return <Dialog open title="Download unencrypted backup?" onClose={onClose} footer={<>
    <Button variant="ghost" onClick={onClose}>Cancel</Button>
    <a href={backupDownloadUrl(workloadId, snapshot.id)} target="_blank" rel="noopener" className={buttonClass("primary")} onClick={onClose}>Download</a>
  </>}>
    <p>{snapshot.database ? <>The file is the {backupScopeLabel(snapshot.database)} dump (<span className="font-mono">{snapshot.database.service}</span>) from this backup.</> : "The file contains the persistent volume data from this snapshot. It does not include the release, Compose configuration or environment variables."}</p>
    <p className="mt-2">Cove decrypts it for the download, so anyone with the file can read the data. Store it somewhere private.</p>
    <p className="mt-2">Your browser saves <span className="font-mono break-all">{downloadFileName(snapshot.id, snapshot.database)}</span> and shows its progress. Cove checks the size and SHA-256 checksum while sending and stops a download that fails, so an incomplete file shows as failed in the browser. If the download cannot start, the new tab explains why.</p>
  </Dialog>;
}

function PolicyForm({ workloadId, initial, support, onClose }: { workloadId: string; initial: Policy | null; support: WorkerSupport; onClose: () => void }) {
  const client = useQueryClient();
  const online = useOnline();
  const locations = useQuery(connectionsQuery("storage-locations"));
  const [storage, setStorage] = useState(initial?.storage_id ?? "");
  const [engine, setEngine] = useState(initial?.database?.engine ?? "");
  const [retentionError, setRetentionError] = useState<string>();
  const [engineError, setEngineError] = useState<string>();
  const [spaceError, setSpaceError] = useState<string>();
  const selected = databaseEngine(engine);
  const instanceScope = selected?.scope === "instance";
  const engineBlocked = instanceScope && support.instanceEngines === false;
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
    if (engineBlocked) {
      setEngineError(`Choose another method, or upgrade and verify ${support.nodeName ?? "this resource's worker"} first.`);
      document.getElementById("backup-mode")?.focus();
      return;
    }
    setEngineError(undefined);
    if (support.retention === false && usesCalendarRetention(calendar)) {
      setRetentionError(`Set these to 0, or upgrade and verify ${support.nodeName ?? "this resource's worker"} first.`);
      const control = event.currentTarget.elements.namedItem(calendar.keep_daily > 0 ? "keep-daily" : calendar.keep_weekly > 0 ? "keep-weekly" : "keep-monthly");
      if (control instanceof HTMLElement) control.focus();
      return;
    }
    setRetentionError(undefined);
    let volumeSnapshot: VolumeSnapshotPolicy | null = null;
    if (!engine && values.has(SNAPSHOT_SWITCH_NAME)) {
      const parsed = parseSnapshotSpace(text(SNAPSHOT_SPACE_NAME), initial?.volume_snapshot);
      if ("error" in parsed) {
        setSpaceError(parsed.error);
        document.getElementById("backup-snapshot-space")?.focus();
        return;
      }
      volumeSnapshot = { max_bytes: parsed.bytes };
    }
    setSpaceError(undefined);
    save.mutate({ volume_snapshot: volumeSnapshot, database: engine ? { engine, service: text("db-service"), name: instanceScope ? INSTANCE_SCOPE_NAME : text("db-name") } : undefined, storage_id: storage, repository: text("repository").trim(), password: text("password"), access_key: text("access"), secret_key: text("secret"), keep_last: Number(text("keep")), ...calendar, interval_hours: Number(text("interval")), drill_interval_hours: Number(text("drill-interval")), ca_certificate: text("ca") });
  }
  return <form className="space-y-4" onSubmit={submit}>
    <Field id="backup-mode" label="Backup method" error={engineError}><Select id="backup-mode" className={inputClass} value={engine} aria-invalid={engineError ? true : undefined} aria-describedby={engineError ? "backup-mode-error" : undefined} onChange={event => { setEngine(event.target.value); setEngineError(undefined); }}><option value="">Volume snapshot</option>{databaseEngines.map((item) => <option key={item.value} value={item.value}>{item.option}</option>)}</Select></Field>
    {engineBlocked && <Notice tone="warn" title={`${selected.name} backups need an upgraded worker`}>Upgrade and verify {support.nodeName ?? "this resource's worker"} to back up MongoDB, Redis or Valkey online. Volume snapshots, PostgreSQL and MariaDB work on this worker.</Notice>}
    {!selected && <VolumeSnapshotSettings workloadId={workloadId} initial={initial?.volume_snapshot} supported={support.snapshots} nodeName={support.nodeName} spaceError={spaceError} onSpaceChange={() => setSpaceError(undefined)} />}
    {selected && <><div className={instanceScope ? undefined : "grid gap-4 sm:grid-cols-2"}><Field id="backup-db-service" label="Database Compose service"><input id="backup-db-service" name="db-service" className={inputClass} defaultValue={initial?.database?.service} required /></Field>{!instanceScope && <Field id="backup-db-name" label="Database name"><input id="backup-db-name" name="db-name" className={inputClass} defaultValue={initial?.database?.name === INSTANCE_SCOPE_NAME ? "" : initial?.database?.name} required /></Field>}</div>
    {instanceScope && <div className="min-w-0"><p className="text-[13px] font-medium text-fg">Backup scope</p><p className="mt-0.5 text-xs text-fg-muted">Complete instance: every {selected.value === "mongodb" ? "database" : "logical database"}. {selected.name} backups cannot be limited to one database.</p></div>}
    <EngineNotice engine={selected.value} /></>}
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
      {support.retention === false && <Notice tone="warn" title="Daily, weekly and monthly retention needs an upgraded worker">Upgrade and verify {support.nodeName ?? "this resource's worker"} to keep snapshots by day, week or month. Latest-snapshot retention works on every worker.</Notice>}
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

function VolumeMethod({ snapshot }: { snapshot: Snapshot }) {
  const label = volumeMethodLabel(snapshot.volume_method);
  if (!label) return null;
  return <>
    <p className="text-xs text-fg-muted">{label}</p>
    {snapshot.fallback_reason && <p className="text-xs break-words text-fg-muted">Snapshots unavailable: {snapshot.fallback_reason}</p>}
  </>;
}

function SnapshotRow({ snapshot, actions }: { snapshot: Snapshot; actions: ReactNode }) {
  return <li className="flex flex-wrap items-center justify-between gap-3 py-3"><div><RelativeTime value={snapshot.created_at} /><p className="text-xs text-fg-muted break-all">{snapshot.id}</p>{snapshot.database ? <p className="text-xs text-fg-muted">{backupScopeLabel(snapshot.database)}</p> : <VolumeMethod snapshot={snapshot} />}{snapshot.verified_at && <p className="text-xs text-fg-muted">{snapshot.verification === "database" ? "Database restore verified" : "File restore verified"} <RelativeTime value={snapshot.verified_at} /></p>}</div>{actions}</li>;
}

interface SnapshotActionsProps { disabled: boolean; restoreDisabled: boolean; downloadDisabled: boolean; onDownload: () => void; onCopy: () => void; onDrill: () => void; onRestore: () => void }

function SnapshotActions({ disabled, restoreDisabled, downloadDisabled, onDownload, onCopy, onDrill, onRestore }: SnapshotActionsProps) {
  return <div className="flex flex-wrap gap-2">
    <Button size="sm" disabled={disabled || downloadDisabled} onClick={onDownload}>Download</Button>
    <Button size="sm" disabled={disabled || restoreDisabled} onClick={onCopy}>Restore as new resource</Button>
    <Button size="sm" disabled={disabled || restoreDisabled} onClick={onDrill}>Verify restore</Button>
    <Button size="sm" disabled={disabled || restoreDisabled} onClick={onRestore}>Restore</Button>
  </div>;
}

function PolicySummary({ saved, canEdit, transfersBlocked, nodeName, interrupted }: { saved: Policy | null; canEdit: boolean; transfersBlocked: boolean; nodeName?: string; interrupted: boolean }) {
  return <>
    {saved && canEdit && transfersBlocked && <Notice tone="warn" title="Downloads and imports need an upgraded worker">Upgrade and verify {nodeName ?? "this resource's worker"} to download backups or import database dumps. Backups and restores still work.</Notice>}
    {interrupted && <Notice tone="warn" title="Import cancelled in this browser">If the file had already been received, the worker may still finish and store the verified backup. Check the backup list before trying again.</Notice>}
    {!saved && !canEdit && <p className="text-sm text-fg-muted">No backup policy is configured. Editor access is needed to configure one.</p>}
    {saved && <p className="text-xs text-fg-muted break-words">{saved.database ? `${backupScopeLabel(saved.database)} (${saved.database.service})` : saved.volume_snapshot ? "Filesystem snapshot, stopped fallback" : "Volume snapshot, stops containers"} · {saved.repository} · {retentionSummary(saved)} · {saved.interval_hours ? `Every ${saved.interval_hours} hours` : "Manual only"}</p>}
  </>;
}

function SnapshotList({ snapshots, row }: { snapshots: UseQueryResult<{ backups: Snapshot[] }>; row: (snapshot: Snapshot) => ReactNode }) {
  if (snapshots.isError) return <QueryError what="backups" error={snapshots.error} onRetry={() => void snapshots.refetch()} />;
  if (!snapshots.data?.backups.length) return <p className="text-sm text-fg-muted">No backups yet.</p>;
  return <ul className="divide-y divide-line">{snapshots.data.backups.map(row)}</ul>;
}

export function Backups({ workloadId, busy, canOperate, canEdit, support }: { workloadId: string; busy: boolean; canOperate: boolean; canEdit: boolean; support: WorkerSupport }) {
 const [copy, setCopy] = useState<Snapshot | null>(null);
  const [download, setDownload] = useState<Snapshot | null>(null);
  const [importing, setImporting] = useState(false);
  const [interrupted, setInterrupted] = useState(false);
  const [editing, setEditing] = useState(false);
  const [restore, setRestore] = useState<Snapshot | null>(null);
  const online = useOnline();
  const client = useQueryClient();
  const policy = useQuery(backupPolicyQuery(workloadId));
  const snapshots = useQuery({ queryKey: ["backups", workloadId], queryFn: ({ signal }) => request<{ backups: Snapshot[] }>("GET", `/api/workloads/${workloadId}/backups`, undefined, signal), refetchInterval: busy ? 3000 : 15000 });
  const action = useMutation({ mutationFn: (input: { action: "backup" | "restore" | "drill"; backup_id?: string }) => request("POST", `/api/workloads/${workloadId}/operations`, input), onSuccess: async () => { setRestore(null); await client.invalidateQueries({ queryKey: keys.operations }); } });
  if (policy.isError) return <QueryError what="backup policy" error={policy.error} onRetry={() => void policy.refetch()} />;
  if (!policy.data) return <p className="text-sm text-fg-muted">Loading backup policy…</p>;
  if (editing) return <PolicyForm workloadId={workloadId} initial={policy.data.backup} support={support} onClose={() => setEditing(false)} />;
  const saved = policy.data.backup;
  const transfersBlocked = support.transfers === false;
  const findSnapshot = (id: string) => snapshots.data?.backups.find((item) => item.id === id);
  return <div className="space-y-4">
    <div className="flex flex-wrap items-center gap-3 empty:hidden">{canEdit && <Button disabled={!online || busy} onClick={() => setEditing(true)}>{saved ? "Edit policy" : "Configure backups"}</Button>}{saved && canOperate && <Button disabled={!online || busy} pending={action.isPending} onClick={() => action.mutate({ action: "backup" })}>Back up now</Button>}{saved?.database && canEdit && <Button disabled={!online || busy || transfersBlocked} onClick={() => { setInterrupted(false); setImporting(true); }}>Import dump</Button>}</div>
    <PolicySummary saved={saved} canEdit={canEdit} transfersBlocked={transfersBlocked} nodeName={support.nodeName} interrupted={interrupted} />
    {action.isError && <Notice tone="error">{errorMessage(action.error)}</Notice>}
    {copy && <RestoreCopyForm workloadId={workloadId} snapshot={copy} onClose={() => setCopy(null)} />}
    <SnapshotList snapshots={snapshots} row={(snapshot) => <SnapshotRow key={snapshot.id} snapshot={snapshot} actions={canEdit && <SnapshotActions disabled={!online || busy} restoreDisabled={action.isPending || saved?.repository !== snapshot.repository} downloadDisabled={transfersBlocked || !matchesPolicy(saved, snapshot)} onDownload={() => setDownload(snapshot)} onCopy={() => setCopy(snapshot)} onDrill={() => action.mutate({ action: "drill", backup_id: snapshot.id })} onRestore={() => setRestore(snapshot)} />} />} />
    {download && <DownloadDialog workloadId={workloadId} snapshot={download} onClose={() => setDownload(null)} />}
    {importing && saved?.database && <BackupImport workloadId={workloadId} database={saved.database} importedRow={findSnapshot} canRestore={canEdit && !busy && !action.isPending} onRestore={(id) => { const row = findSnapshot(id); setImporting(false); if (row) setRestore(row); }} onClose={(wasInterrupted) => { setImporting(false); setInterrupted(wasInterrupted); }} />}
    <ConfirmDialog open={restore !== null} title={restore?.database ? `Restore ${backupScopeLabel(restore.database)}?` : "Restore persistent data?"} confirmLabel="Restore data" danger onCancel={() => setRestore(null)} onConfirm={() => { if (restore) action.mutate({ action: "restore", backup_id: restore.id }); }}>
      {restoreWarning(restore?.database?.engine)}
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
   {snapshot.database && <p className="text-sm text-fg-muted">{restoreCopyScope(snapshot.database)} Other service volumes start empty.{snapshot.database.engine === "postgresql" && " PostgreSQL ownership and grants are not copied; review access before deploying."}</p>}
   <Field id="restore-copy-name" label="New resource name"><input id="restore-copy-name" className={inputClass} value={name} onChange={event => setName(event.target.value)} required maxLength={120} disabled={copy.isPending || copy.isSuccess} /></Field>
   {copy.isError && <Notice tone="error">{errorMessage(copy.error)}</Notice>}
   {copy.data ? <div className="flex flex-wrap gap-3"><Link to="/operations/$operationId" params={{ operationId: copy.data.id }} onClick={onClose}>View restore progress</Link><Link to="/resources/$resourceId" params={{ resourceId: copy.data.resource_id }} onClick={onClose}>Open new resource</Link></div> : <Button type="submit" disabled={!online} pending={copy.isPending}>Restore separate resource</Button>}
   <Button disabled={copy.isPending} onClick={onClose}>Close</Button>
  </form>
 </Dialog>;
}

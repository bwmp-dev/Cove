import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { Eye, EyeOff } from "lucide-react";
import { useId, useState, type FormEvent, type ReactNode } from "react";
import { backupPolicyQuery, type BackupPolicy } from "../api/backups";
import { ApiError, errorMessage, request } from "../api/client";
import { connectionsQuery } from "../api/connections";
import { pitrQuery, type PITRBase, type PITRInfo, type PITRRestoreInput, type PITRRestoreResult, type PITRSettings, type PITRStatus } from "../api/pitr";
import { keys } from "../api/queries";
import type { PITRRights } from "../lib/permissions";
import { useNow, useOnline } from "../lib/hooks";
import { defaultPITRSettings, formatUtc, knownTime, parseRecoveryTime, parseSettingsDraft, PITR_DAYS_MAX, PITR_INTERVAL_MAX, PITR_KEEP_MAX, PITR_OBJECT_LIMIT, pitrStateLabel, recoveryWindow, settingsDraft, sortedBases, statusIsStale, usesInlineRepository, utcInputValue, type SettingsDraft, type SettingsErrors } from "../lib/pitr";
import { inputClass, linkClass } from "../lib/styles";
import { formatBinaryBytes } from "../lib/telemetry";
import { ConfirmDialog, Dialog } from "./ConfirmDialog";
import { CopyButton } from "./CopyButton";
import { RelativeTime } from "./RelativeTime";
import { StatusLabel } from "./status";
import { Button, Field, Meter, Notice, QueryError, SettingRow, SettingRows } from "./ui";

type Configured = Extract<PITRInfo, { configured: true }>;

function invalidate(client: ReturnType<typeof useQueryClient>, workloadId: string) {
  return Promise.all([client.invalidateQueries({ queryKey: ["pitr", workloadId] }), client.invalidateQueries({ queryKey: keys.operations })]);
}

function OperationLink({ id, children }: { id: string; children: ReactNode }) {
  return <Link to="/operations/$operationId" params={{ operationId: id }} className={linkClass}>{children}</Link>;
}

function Requirements() {
  return <Notice title="Source requirements">
    <ul className="list-disc space-y-1 pl-4">
      <li>A dedicated, writable PostgreSQL 15 to 18 primary on port 5432 with the standard <code className="font-mono">/var/run/postgresql</code> socket.</li>
      <li>PGDATA on a persistent named volume, with the image’s internal configuration and standard 16 MiB WAL segments.</li>
      <li>Administrator credentials in the standard <code className="font-mono">POSTGRES_*</code> variables or their <code className="font-mono">_FILE</code> variants.</li>
      <li>No external tablespaces or other replication slots, and databases that fit inside the size cap.</li>
    </ul>
    <p className="mt-1.5">Every database and role in the server is captured, not one database. Unsupported sources fail before anything changes, and the operation explains why.</p>
  </Notice>;
}

function storageText(storageId: string, policy: BackupPolicy | null | undefined, locations: { id: string; name: string }[] | undefined) {
  if (storageId) {
    const name = locations?.find((item) => item.id === storageId)?.name;
    return name ? `A separate prefix in ${name}` : "A separate prefix in the configured storage location";
  }
  if (policy && !policy.storage_id && policy.repository !== "local") return "A separate prefix in the backup policy’s storage";
  return "A separate local Docker volume on this node";
}

interface SettingsFormProps { workloadId: string; info: PITRInfo; policy: BackupPolicy | null; onClose: () => void; onQueued: (id: string) => void }

function SettingsForm({ workloadId, info, policy, onClose, onQueued }: SettingsFormProps) {
  const client = useQueryClient();
  const online = useOnline();
  const saved = info.configured ? info.settings : null;
  const [draft, setDraft] = useState<SettingsDraft>(() => settingsDraft(saved ?? { ...defaultPITRSettings, service: policy?.database?.engine === "postgresql" ? policy.database.service : "" }));
  const [errors, setErrors] = useState<SettingsErrors>({});
  const [pending, setPending] = useState<PITRSettings | null>(null);
  const storageId = info.configured ? info.storage_id : policy?.storage_id ?? "";
  const locations = useQuery({ ...connectionsQuery("storage-locations"), enabled: storageId !== "" });
  const save = useMutation({
    gcTime: 0,
    mutationFn: (settings: PITRSettings) => request<{ id: string }>("PUT", `/api/workloads/${workloadId}/pitr`, { settings, confirm_restart: true }),
    onSuccess: async (result) => { setPending(null); onQueued(result.id); await invalidate(client, workloadId); onClose(); },
  });
  const set = (key: keyof SettingsDraft) => (value: string) => { setDraft((current) => ({ ...current, [key]: value })); setErrors((current) => ({ ...current, [key]: undefined })); };
  function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    const parsed = parseSettingsDraft(draft, saved);
    if ("errors" in parsed) {
      setErrors(parsed.errors);
      const first = (["service", "interval_hours", "keep_last", "retention_days", "max_mib"] as const).find((key) => parsed.errors[key]);
      if (first) document.getElementById(`pitr-${first.replace("_", "-")}`)?.focus();
      return;
    }
    save.reset();
    setPending(parsed.settings);
  }
  const field = (key: keyof SettingsDraft, label: string, hint: string, props: { inputMode?: "numeric"; className?: string; readOnly?: boolean } = {}) => {
    const id = `pitr-${key.replace("_", "-")}`;
    const error = errors[key];
    return <Field id={id} label={label} hint={hint} error={error}>
      <input id={id} className={`${inputClass} ${props.className ?? ""}`} inputMode={props.inputMode} readOnly={props.readOnly} value={draft[key]} required spellCheck={false} autoComplete="off"
        aria-invalid={error ? true : undefined} aria-describedby={error ? `${id}-error` : `${id}-hint`} onChange={(event) => set(key)(event.target.value)} />
    </Field>;
  };
  return <form className="space-y-4" onSubmit={submit} noValidate>
    {!saved && <Requirements />}
    {field("service", "PostgreSQL Compose service", saved ? "Fixed while this history exists. Disable capture and delete the history to capture another service." : "The PostgreSQL service in this resource’s deployed Compose file. The worker checks it before changing anything.", { className: "font-mono", readOnly: Boolean(saved) })}
    <div className="grid gap-4 sm:grid-cols-2">
      {field("interval_hours", "Base backup interval (hours)", `1 to ${PITR_INTERVAL_MAX}. WAL is captured continuously between bases.`, { inputMode: "numeric" })}
      {field("keep_last", "Base backups to keep", `1 to ${PITR_KEEP_MAX}. The newest base is always kept.`, { inputMode: "numeric" })}
      {field("retention_days", "Keep history for (days)", `1 to ${PITR_DAYS_MAX}. Older bases and their WAL are pruned.`, { inputMode: "numeric" })}
      {field("max_mib", "Archive size cap (MiB)", "1,024 to 65,536 MiB. Capture pauses when the archive reaches it.", { inputMode: "numeric" })}
    </div>
    <p className="text-xs text-fg-muted">Stored in: {storageText(storageId, policy, locations.data)}. Changing the backup policy later does not move this history.{saved && storageId && " After rotating the storage location’s credentials, save these settings again so capture uses them."}</p>
    {save.isError && !pending && <Notice tone="error" title="Could not save point-in-time recovery">{errorMessage(save.error)}</Notice>}
    <div className="flex flex-wrap justify-end gap-2">
      <Button variant="ghost" onClick={onClose} disabled={save.isPending}>Cancel</Button>
      <Button type="submit" variant="primary" disabled={!online}>{saved ? "Save settings" : "Enable point-in-time recovery"}</Button>
    </div>
    <ConfirmDialog open={pending !== null} title={saved ? "Save capture settings?" : "Enable point-in-time recovery?"} confirmLabel={saved ? "Save settings" : "Restart and enable"} pending={save.isPending} onCancel={() => { setPending(null); save.reset(); }} onConfirm={() => { if (pending) save.mutate(pending); }}>
      {saved
        ? <p>Cove restarts PostgreSQL only if its capture configuration has to change. Otherwise base backups and WAL capture stay online.</p>
        : <p>PostgreSQL restarts once so Cove can add a private socket for capture. After that, base backups and WAL capture run online, and Cove schedules the first base backup shortly after capture starts. While capture is enabled, deploys and rollbacks of this resource are refused; disable capture first.</p>}
      {save.isError && <div className="mt-3"><Notice tone="error">{errorMessage(save.error)}</Notice></div>}
    </ConfirmDialog>
  </form>;
}

function UsageMeter({ status, settings }: { status: PITRStatus; settings: PITRSettings }) {
  const measured = knownTime(status.measured_at);
  if (!measured) {
    return <div className="min-w-0">
      <p className="text-xs text-fg-muted">Archive storage</p>
      <p className="mt-1 text-lg font-semibold text-fg">Unknown</p>
      <p className="mt-1.5 text-xs text-fg-subtle">Not measured yet. Cap {formatBinaryBytes(settings.max_bytes)}.</p>
    </div>;
  }
  return <Meter label="Archive storage" value={status.bytes} limit={settings.max_bytes} format={formatBinaryBytes}
    detail={<>{status.objects.toLocaleString("en")} of {PITR_OBJECT_LIMIT.toLocaleString("en")} objects · measured <RelativeTime value={measured} /></>} />;
}

function StatusNotices({ status, stale, supported, nodeName, refreshError }: { status: PITRStatus; stale: boolean; supported: boolean; nodeName?: string; refreshError: unknown }) {
  const checked = knownTime(status.checked_at);
  return <>
    {refreshError !== null && <Notice tone="warn" title="Could not refresh the status"><p>{errorMessage(refreshError)}</p><p className="mt-1">The values below are from the last successful reading.</p></Notice>}
    {!supported && <Notice tone="warn" title="This worker no longer reports point-in-time recovery">Upgrade and verify {nodeName ?? "this resource’s worker"} to change capture, take base backups or restore. The retained history is unchanged.</Notice>}
    {stale && <Notice tone="warn" title="Status unknown">{checked ? <>The worker’s last capture heartbeat was <RelativeTime value={checked} />.</> : "The worker has not reported a capture heartbeat yet."} The state below may no longer be current.</Notice>}
    {status.last_error && <Notice tone="error" title="Capture reported an error"><span className="break-words">{status.last_error}</span></Notice>}
    {status.state === "paused" && <Notice tone="warn" title="Capture is paused">The archive reached its size cap or the {PITR_OBJECT_LIMIT.toLocaleString("en")}-object limit, so Cove released the PostgreSQL replication slot. WAL written while capture is paused cannot be recovered. Raise the size cap or shorten retention.</Notice>}
    {status.gaps && <Notice tone="warn" title="The WAL history has gaps">Only the windows listed with each base can be restored.</Notice>}
    {status.needs_base && status.enabled && <Notice tone="warn" title="A new base backup is needed">Times after the last gap cannot be restored until a new base backup finishes.</Notice>}
  </>;
}

function BaseRow({ base, now, newest, canRestore, disabledReason, onRestore }: { base: PITRBase; now: number; newest: boolean; canRestore: boolean; disabledReason?: string; onRestore: () => void }) {
  const window = recoveryWindow(base, now);
  return <li className="flex flex-wrap items-start justify-between gap-x-4 gap-y-2 py-3">
    <div className="min-w-0 space-y-0.5">
      <p className="text-[13px] text-fg">Base from <RelativeTime value={base.created_at} />{newest && <span className="ml-1.5 text-xs text-fg-subtle">Newest</span>}</p>
      {window
        ? <p className="text-xs text-fg-muted">Recoverable from <time dateTime={new Date(window.from).toISOString()}>{formatUtc(new Date(window.from).toISOString())}</time> to <time dateTime={new Date(window.to).toISOString()}>{formatUtc(new Date(window.to).toISOString())}</time></p>
        : <p className="text-xs text-fg-muted">No known continuous recovery window. This base cannot be restored to a point in time.</p>}
      <p className="text-xs text-fg-muted"><span title={`${base.bytes.toLocaleString("en")} bytes`}>{formatBinaryBytes(base.bytes)}</span> base · PostgreSQL {base.postgres_major}</p>
    </div>
    {canRestore && <Button size="sm" disabled={!window || disabledReason !== undefined} title={disabledReason} onClick={onRestore}>Restore to new resource</Button>}
  </li>;
}

// Shown once from the creation response and held only in this component's memory.
function OneTimeCredentials({ credentials }: { credentials: PITRRestoreResult["credentials"] }) {
  const id = useId();
  const [visible, setVisible] = useState(false);
  return <div className="space-y-3">
    <Notice tone="warn" title="Save the administrator password now">It will not be shown again. The new resource’s environment variables store it encrypted and never display it.</Notice>
    <Field id={`${id}-user`} label="Administrator username">
      <div className="flex flex-wrap items-center gap-2">
        <input id={`${id}-user`} className={`${inputClass} min-w-0 flex-1 basis-56 font-mono`} value={credentials.username} readOnly autoComplete="off" spellCheck={false} />
        <CopyButton text={credentials.username} label="Copy username" />
      </div>
    </Field>
    <Field id={`${id}-password`} label="Administrator password">
      <div className="flex flex-wrap items-center gap-2">
        <input id={`${id}-password`} type={visible ? "text" : "password"} className={`${inputClass} min-w-0 flex-1 basis-56 font-mono`} value={credentials.password} readOnly autoComplete="off" spellCheck={false} data-1p-ignore data-lpignore="true" />
        <Button aria-pressed={visible} onClick={() => setVisible((current) => !current)}>{visible ? <EyeOff aria-hidden className="size-3.5" /> : <Eye aria-hidden className="size-3.5" />}{visible ? "Hide" : "Show"}</Button>
        <CopyButton text={credentials.password} label="Copy password" />
      </div>
    </Field>
  </div>;
}

function RestoreDialog({ workloadId, base, managed, onClose }: { workloadId: string; base: PITRBase; managed: boolean; onClose: () => void }) {
  const id = useId();
  const online = useOnline();
  const now = useNow(15_000);
  const client = useQueryClient();
  const window = recoveryWindow(base, now);
  const [name, setName] = useState("");
  const [time, setTime] = useState(() => (window ? utcInputValue(window.to).replace("T", " ") : ""));
  const [timeError, setTimeError] = useState<string>();
  const restore = useMutation({
    gcTime: 0,
    mutationFn: (input: PITRRestoreInput) => request<PITRRestoreResult>("POST", `/api/workloads/${workloadId}/pitr/restore-copy`, input),
    onSuccess: () => invalidate(client, workloadId),
  });
  function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    const parsed = parseRecoveryTime(time, recoveryWindow(base, Date.now()));
    if ("error" in parsed) {
      setTimeError(parsed.error);
      document.getElementById(`${id}-time`)?.focus();
      return;
    }
    restore.mutate({ name: name.trim(), base: base.id, timestamp: parsed.timestamp });
  }
  const locked = restore.isPending || restore.isSuccess;
  // Dropping the mutation result discards the one-time password as soon as the dialog is dismissed.
  const close = () => { restore.reset(); onClose(); };
  return <Dialog open title="Restore to a new PostgreSQL resource" onClose={close}>
    <form className="space-y-4" onSubmit={submit} noValidate>
      {!restore.data && <>
        <p>{managed ? "Cove creates a new managed PostgreSQL resource with its own private hostname in the same project, environment and node." : "Cove creates a new, dedicated PostgreSQL resource in the same project, environment and node."} This resource keeps running and is not changed.</p>
        <ul className="list-disc space-y-1 pl-4 text-xs">
          <li>Every database and role is recovered as it was at the chosen time. Other services are not copied.</li>
          <li>The copy uses fresh volumes and the retained source image. Cove generates a new administrator password, shows it once when the copy is accepted and stores it encrypted in the new resource’s {managed ? "POSTGRES_PASSWORD, which later password rotations use" : "environment variables"}. Check which credentials your applications need before connecting them.</li>
          {managed
            ? <li>The copy is private: TLS, public access and the connection pooler are off, and this resource’s TLS and pooler settings are not copied. It stays offline until you review and deploy it.</li>
            : <li>It has no public ports or routes, uses an internal network and stays offline until you review and deploy it.</li>}
          <li>PostgreSQL replays WAL to the chosen time and is checked in an isolated 2 GiB container before the restore succeeds. If it fails, the new resource stays blocked with its volume kept for inspection.</li>
          <li>Recovery can take up to 30 minutes and needs free space on this node of twice the archive size cap plus 256 MiB.</li>
        </ul>
        <Field id={`${id}-name`} label="New resource name">
          <input id={`${id}-name`} className={inputClass} value={name} onChange={(event) => setName(event.target.value)} required maxLength={120} autoComplete="off" disabled={locked} />
        </Field>
        <Field id={`${id}-time`} label="Recovery time (UTC)" error={timeError} hint={window ? `YYYY-MM-DD HH:MM:SS, from ${formatUtc(new Date(window.from).toISOString())} to ${formatUtc(new Date(window.to).toISOString())}. The newest commits may not be archived yet.` : undefined}>
          <input id={`${id}-time`} className={`${inputClass} font-mono`} value={time} required disabled={locked} placeholder="YYYY-MM-DD HH:MM:SS" autoComplete="off" spellCheck={false}
            aria-invalid={timeError ? true : undefined} aria-describedby={timeError ? `${id}-time-error` : `${id}-time-hint`}
            onChange={(event) => { setTime(event.target.value); setTimeError(undefined); }} />
        </Field>
      </>}
      {restore.isError && <Notice tone="error" title="Could not start the restore">{errorMessage(restore.error)}</Notice>}
      {restore.data && <Notice tone="success" title="Restore queued">
        <span className="flex flex-wrap gap-x-4 gap-y-1">
          <OperationLink id={restore.data.id}>View restore progress</OperationLink>
          <Link to="/resources/$resourceId" params={{ resourceId: restore.data.resource_id }} className={linkClass}>Open new resource</Link>
        </span>
      </Notice>}
      {restore.data && <OneTimeCredentials credentials={restore.data.credentials} />}
      <div className="flex flex-wrap justify-end gap-2">
        <Button variant="ghost" onClick={close} disabled={restore.isPending}>{restore.isSuccess ? "Close" : "Cancel"}</Button>
        {!restore.isSuccess && <Button type="submit" variant="primary" disabled={!online || name.trim() === "" || !window} pending={restore.isPending}>Create recovery copy</Button>}
      </div>
    </form>
  </Dialog>;
}

function DeleteHistoryDialog({ workloadId, service, onClose, onQueued }: { workloadId: string; service: string; onClose: () => void; onQueued: (id: string) => void }) {
  const id = useId();
  const online = useOnline();
  const client = useQueryClient();
  const [typed, setTyped] = useState("");
  const remove = useMutation({
    gcTime: 0,
    mutationFn: () => request<{ id: string }>("DELETE", `/api/workloads/${workloadId}/pitr`, { confirm_delete: true }),
    onSuccess: async (result) => { onQueued(result.id); await invalidate(client, workloadId); onClose(); },
  });
  return <Dialog open title="Delete point-in-time recovery history?" onClose={onClose}>
    <form className="space-y-4" onSubmit={(event) => { event.preventDefault(); remove.mutate(); }}>
      <p>Every PITR base backup and archived WAL segment for this resource is permanently deleted. Point-in-time recovery becomes unconfigured. Normal backups and the PostgreSQL data are not changed.</p>
      <Field id={id} label={`Type ${service} to confirm`}>
        <input id={id} className={`${inputClass} font-mono`} value={typed} onChange={(event) => setTyped(event.target.value)} autoComplete="off" spellCheck={false} required />
      </Field>
      {remove.isError && <Notice tone="error" title="Could not delete the history">{errorMessage(remove.error)}</Notice>}
      <div className="flex flex-wrap justify-end gap-2">
        <Button variant="ghost" onClick={onClose} disabled={remove.isPending}>Cancel</Button>
        <Button type="submit" variant="danger" disabled={!online || typed !== service} pending={remove.isPending}>Delete history</Button>
      </div>
    </form>
  </Dialog>;
}

function CaptureActions({ workloadId, settings, rights, blocked, captureHeld, onEdit, onQueued }: { workloadId: string; settings: PITRSettings; rights: PITRRights; blocked?: string; captureHeld?: string; onEdit: () => void; onQueued: (id: string) => void }) {
  const client = useQueryClient();
  const [toggle, setToggle] = useState(false);
  const capture = useMutation({
    gcTime: 0,
    mutationFn: () => request<{ id: string }>("POST", `/api/workloads/${workloadId}/pitr/backup`, {}),
    onSuccess: async (result) => { onQueued(result.id); await invalidate(client, workloadId); },
  });
  const change = useMutation({
    gcTime: 0,
    mutationFn: (enabled: boolean) => request<{ id: string }>("PUT", `/api/workloads/${workloadId}/pitr`, { settings: { ...settings, enabled }, confirm_restart: true }),
    onSuccess: async (result) => { setToggle(false); onQueued(result.id); await invalidate(client, workloadId); },
  });
  // The API refuses capture changes and base backups while the environment is paused or resuming, or the app is asleep.
  const changeBlocked = blocked ?? captureHeld;
  return <>
    <div className="flex flex-wrap gap-2">
      {rights.configure && <Button disabled={changeBlocked !== undefined} title={changeBlocked} onClick={onEdit}>Edit settings</Button>}
      {settings.enabled && rights.backup && <Button disabled={changeBlocked !== undefined} title={changeBlocked} pending={capture.isPending} onClick={() => capture.mutate()}>Take base backup now</Button>}
      {rights.configure && <Button disabled={changeBlocked !== undefined} title={changeBlocked} onClick={() => { change.reset(); setToggle(true); }}>{settings.enabled ? "Disable capture" : "Enable capture"}</Button>}
    </div>
    {changeBlocked && <p className="w-full text-xs text-fg-muted">{changeBlocked}</p>}
    {capture.isError && <div className="w-full"><Notice tone="error" title="Could not queue a base backup">{errorMessage(capture.error)}</Notice></div>}
    {capture.isSuccess && <div className="w-full"><Notice tone="success" title="Base backup queued">The result is reported in the operation and through backup notifications.</Notice></div>}
    <ConfirmDialog open={toggle} title={settings.enabled ? "Disable capture?" : "Enable capture?"} confirmLabel={settings.enabled ? "Restart and disable" : "Restart and enable"} danger={settings.enabled} pending={change.isPending} onCancel={() => setToggle(false)} onConfirm={() => change.mutate(!settings.enabled)}>
      {settings.enabled
        ? <p>PostgreSQL restarts once so Cove can remove the private capture socket. Existing base backups and WAL are kept and retention keeps pruning them. Times after capture stops cannot be restored. Deploys and rollbacks of this resource are accepted again once capture is disabled.</p>
        : <p>PostgreSQL restarts once so Cove can add the private capture socket. Cove schedules a base backup shortly after capture starts. While capture is enabled, deploys and rollbacks of this resource are refused.</p>}
      {change.isError && <div className="mt-3"><Notice tone="error">{errorMessage(change.error)}</Notice></div>}
    </ConfirmDialog>
  </>;
}

function CaptureSummary({ status, settings, storage }: { status: PITRStatus; settings: PITRSettings; storage: string }) {
  return <SettingRows>
    <SettingRow label="Archive usage" description="Encrypted archive bytes as Cove stores them, not your storage provider’s bill."><div className="w-full @lg:max-w-64"><UsageMeter status={status} settings={settings} /></div></SettingRow>
    <SettingRow label="Last complete WAL" description="Excludes the segment PostgreSQL is still writing.">{knownTime(status.last_archive_at) ? <RelativeTime value={status.last_archive_at ?? ""} /> : <span className="text-fg-muted">Unknown</span>}</SettingRow>
    <SettingRow label="WAL receiver">{status.receiver ? <StatusLabel tone="ok" label="Streaming" /> : <StatusLabel tone={status.enabled ? "warn" : "neutral"} label="Not streaming" />}</SettingRow>
    <SettingRow label="Capture settings" description={storage}>
      <span className="break-words text-fg-muted @lg:text-right"><span className="font-mono text-fg">{settings.service}</span> · base every {settings.interval_hours} h · keep {settings.keep_last} {settings.keep_last === 1 ? "base" : "bases"} and {settings.retention_days} {settings.retention_days === 1 ? "day" : "days"} · cap <span title={`${settings.max_bytes.toLocaleString("en")} bytes`}>{formatBinaryBytes(settings.max_bytes)}</span></span>
    </SettingRow>
  </SettingRows>;
}

function BaseList({ workloadId, bases, now, canRestore, blocked, managed }: { workloadId: string; bases: PITRBase[]; now: number; canRestore: boolean; blocked?: string; managed: boolean }) {
  const [restoring, setRestoring] = useState<PITRBase | null>(null);
  return <div className="border-t border-line pt-4">
    <h3 className="text-[13px] font-semibold">Base backups</h3>
    {bases.length === 0
      ? <p className="mt-1 text-xs text-fg-muted">No base backups yet. Times can be restored only after a base backup finishes.</p>
      : <ul aria-label="Base backups" className="divide-y divide-line">{bases.map((base, index) => <BaseRow key={base.id} base={base} now={now} newest={index === 0} canRestore={canRestore} disabledReason={blocked} onRestore={() => setRestoring(base)} />)}</ul>}
    <p className="mt-2 text-xs text-fg-muted">Windows end at the latest point Cove has confirmed in archived WAL, checked about once a minute, so the newest commits may not be recoverable yet. Windows are observations, not a guarantee. Normal backups above are separate.</p>
    {canRestore && blocked && bases.length > 0 && <p className="mt-2 text-xs text-fg-muted">{blocked}</p>}
    {restoring && <RestoreDialog workloadId={workloadId} base={restoring} managed={managed} onClose={() => setRestoring(null)} />}
  </div>;
}

function DeleteHistory({ workloadId, service, blocked, onQueued }: { workloadId: string; service: string; blocked?: string; onQueued: (id: string) => void }) {
  const [deleting, setDeleting] = useState(false);
  return <div className="space-y-2 border-t border-line pt-4">
    <h3 className="text-[13px] font-semibold">Delete history</h3>
    <p className="text-xs text-fg-muted">Capture is disabled, and retention keeps pruning the remaining history. Delete it to capture another service or start over. This resource cannot be removed while PITR history exists.</p>
    <Button variant="danger" disabled={blocked !== undefined} title={blocked} onClick={() => setDeleting(true)}>Delete PITR history</Button>
    {deleting && <DeleteHistoryDialog workloadId={workloadId} service={service} onClose={() => setDeleting(false)} onQueued={onQueued} />}
  </div>;
}

function blockReason(online: boolean, supported: boolean, busy: boolean) {
  if (!online) return "Reconnect to make changes.";
  if (!supported) return "This worker needs an upgrade.";
  if (busy) return "Wait for the active operation to finish.";
  return undefined;
}

interface ConfiguredViewProps { workloadId: string; managed: boolean; info: Configured; policy: BackupPolicy | null | undefined; busy: boolean; rights: PITRRights; captureHeld?: string; restoreHeld?: string; nodeName?: string; refreshError: unknown; onEdit: () => void; onQueued: (id: string) => void; queued?: string }

function ConfiguredView({ workloadId, managed, info, policy, busy, rights, captureHeld, restoreHeld, nodeName, refreshError, onEdit, onQueued, queued }: ConfiguredViewProps) {
  const online = useOnline();
  const now = useNow(5_000);
  const locations = useQuery({ ...connectionsQuery("storage-locations"), enabled: info.storage_id !== "" });
  const { settings, status } = info;
  const stale = statusIsStale(status, now);
  const checked = knownTime(status.checked_at);
  const state = stale ? { tone: "warn" as const, label: "Status unknown" } : pitrStateLabel(status.state);
  const blocked = blockReason(online, info.supported, busy);
  const operation = queued ?? info.operation_id;
  return <div className="space-y-4">
    <div className="flex flex-wrap items-center justify-between gap-x-4 gap-y-2">
      <div className="flex flex-wrap items-center gap-x-3 gap-y-1">
        <StatusLabel tone={state.tone} label={state.label} pulse={!stale && state.tone === "info"} />
        <span className="text-xs text-fg-muted">{checked ? <>Checked <RelativeTime value={checked} /></> : "Not checked yet"}</span>
        {operation && <span className="text-xs"><OperationLink id={operation}>Latest PITR operation</OperationLink></span>}
      </div>
      {(rights.configure || (rights.backup && settings.enabled)) && <CaptureActions workloadId={workloadId} settings={settings} rights={rights} blocked={blocked} captureHeld={captureHeld} onEdit={onEdit} onQueued={onQueued} />}
    </div>
    <StatusNotices status={status} stale={stale} supported={info.supported} nodeName={nodeName} refreshError={refreshError} />
    {settings.enabled && <Notice title="Deploys and rollbacks are paused">Cove refuses to deploy or roll back this resource while capture is enabled, so the archived image and cluster stay consistent. Disable capture first; the retained history stays available.</Notice>}
    <CaptureSummary status={status} settings={settings} storage={storageText(info.storage_id, policy, locations.data)} />
    <BaseList workloadId={workloadId} bases={sortedBases(status.bases)} now={now} canRestore={rights.restore} blocked={blocked ?? restoreHeld} managed={managed} />
    {rights.forget && !settings.enabled && <DeleteHistory workloadId={workloadId} service={settings.service} blocked={blocked} onQueued={onQueued} />}
  </div>;
}

function prerequisiteNotice(info: PITRInfo, policy: BackupPolicy | null, canConfigure: boolean, nodeName: string | undefined) {
  if (!info.supported) return <Notice tone="warn" title="Point-in-time recovery needs an upgraded worker">Upgrade and verify {nodeName ?? "this resource’s worker"} to capture PostgreSQL WAL continuously.</Notice>;
  if (!policy) return <Notice title="Save a backup policy first">{canConfigure ? "Point-in-time recovery stores its history beside this resource’s backups, in a separate prefix of the same storage location or a separate local volume. Configure backups above, then enable it." : "No backup policy is configured. Configuring one needs Manage backups, Edit configuration and Read secrets."}</Notice>;
  if (usesInlineRepository(policy)) return <Notice tone="warn" title="Use a configured storage location">This backup policy uses an inline S3 repository. Point-in-time recovery needs a storage location from Storage, or local storage.</Notice>;
  return null;
}

function LoadError({ query }: { query: { error: unknown; isFetching: boolean; refetch: () => unknown } }) {
  if (query.error instanceof ApiError && query.error.status === 502) {
    return <Notice tone="warn" title="Capture status unavailable" action={<Button variant="ghost" size="sm" pending={query.isFetching} onClick={() => void query.refetch()}>Retry</Button>}>
      <p>{errorMessage(query.error)}</p>
      <p className="mt-1">Settings and base backups are shown once the worker responds. The retained history is not affected.</p>
    </Notice>;
  }
  return <QueryError what="point-in-time recovery" error={query.error} retrying={query.isFetching} onRetry={() => void query.refetch()} />;
}

function UnconfiguredView({ workloadId, info, busy, canConfigure, captureHeld, nodeName, queued, onSetup }: { workloadId: string; info: PITRInfo; busy: boolean; canConfigure: boolean; captureHeld?: string; nodeName?: string; queued?: string; onSetup: () => void }) {
  const policy = useQuery(backupPolicyQuery(workloadId));
  const prerequisite = policy.data ? prerequisiteNotice(info, policy.data.backup, canConfigure, nodeName) : null;
  const setupBlocked = busy ? "Wait for the active operation to finish." : captureHeld;
  const ready = policy.data !== undefined && prerequisite === null;
  return <div className="space-y-4">
    <p className="text-[13px] text-fg-muted">Not configured. Point-in-time recovery keeps periodic base backups of the whole PostgreSQL server plus its continuously archived WAL, so you can restore any time inside a recovered window to a new resource.</p>
    {policy.isError && <QueryError what="backup policy" error={policy.error} retrying={policy.isFetching} onRetry={() => void policy.refetch()} />}
    {policy.isPending && <p className="text-xs text-fg-muted">Checking the backup policy…</p>}
    {prerequisite}
    {queued && <p className="text-xs"><OperationLink id={queued}>Latest PITR operation</OperationLink></p>}
    {ready && canConfigure && <Button variant="primary" disabled={setupBlocked !== undefined} title={setupBlocked} onClick={onSetup}>Set up point-in-time recovery</Button>}
    {ready && canConfigure && captureHeld && <p className="text-xs text-fg-muted">{captureHeld}</p>}
    {ready && !canConfigure && <p className="text-xs text-fg-muted">Configuring point-in-time recovery needs Manage backups, Edit configuration and Read secrets.</p>}
  </div>;
}

export function PostgresPITR({ workloadId, managed = false, busy, rights, nodeName, captureHeld, restoreHeld }: { workloadId: string; managed?: boolean; busy: boolean; rights: PITRRights; nodeName?: string; captureHeld?: string; restoreHeld?: string }) {
  const pitr = useQuery(pitrQuery(workloadId, busy));
  const policy = useQuery(backupPolicyQuery(workloadId));
  const [editing, setEditing] = useState(false);
  const [queued, setQueued] = useState<string>();
  if (pitr.isPending) return <p className="text-sm text-fg-muted">Loading point-in-time recovery…</p>;
  if (pitr.isError && !pitr.data) return <LoadError query={pitr} />;
  const info = pitr.data;
  if (editing) {
    if (policy.isPending) return <p className="text-sm text-fg-muted">Loading backup policy…</p>;
    return <SettingsForm workloadId={workloadId} info={info} policy={policy.data?.backup ?? null} onClose={() => setEditing(false)} onQueued={setQueued} />;
  }
  if (!info.configured) return <UnconfiguredView workloadId={workloadId} info={info} busy={busy} canConfigure={rights.configure} captureHeld={captureHeld} nodeName={nodeName} queued={queued} onSetup={() => setEditing(true)} />;
  return <ConfiguredView workloadId={workloadId} managed={managed} info={info} policy={policy.data?.backup} busy={busy} rights={rights} captureHeld={captureHeld} restoreHeld={restoreHeld} nodeName={nodeName} refreshError={pitr.isError ? pitr.error : null} onEdit={() => setEditing(true)} onQueued={setQueued} queued={queued} />;
}

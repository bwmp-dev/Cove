import { useQuery } from "@tanstack/react-query";
import { HardDriveDownload, RotateCw } from "lucide-react";
import { useState, type FormEvent } from "react";
import { errorMessage } from "../api/client";
import { connectionsQuery } from "../api/connections";
import { panelRecoveryQuery, usePanelRecoveryAction, useSavePanelBackup, type PanelJobState, type PanelRecovery as Recovery, type PanelSnapshot } from "../api/panel-recovery";
import { formatAbsolute, shortId } from "../lib/format";
import { useNow, useOnline } from "../lib/hooks";
import type { Tone } from "../lib/nodes";
import {
  MAX_BACKUP_BYTES,
  MIN_BACKUP_BYTES,
  RESUME_COMMAND,
  SETUP_COMMAND,
  actionBlockReason,
  configDraft,
  draftChanged,
  hostStatus,
  hostStatusLabels,
  type HostStatus,
  jobActionLabels,
  needsNewPassword,
  parseConfigDraft,
  restoreBlockReason,
  sortedSnapshots,
  type ConfigDraft,
  type ConfigErrors,
} from "../lib/panel-recovery";
import { inputClass, tableHeadClass, tableRowClass } from "../lib/styles";
import { formatBinaryBytes } from "../lib/telemetry";
import { ConfirmDialog, Dialog } from "./ConfirmDialog";
import { CopyButton } from "./CopyButton";
import { RelativeTime } from "./RelativeTime";
import { SaveBar, UnsavedChangesGuard } from "./SaveBar";
import { Select } from "./Select";
import { StatusLabel } from "./status";
import { Button, DefinitionList, Field, LoadingRows, Notice, QueryError, Section, SettingRow, SettingRows, Subsection, SwitchField } from "./ui";

const title = "Panel recovery";
const description = "Encrypted backups of this panel, taken by a timer on the panel host, and rollback to a retained snapshot.";

const jobTones: Record<PanelJobState, Tone> = { queued: "neutral", running: "info", succeeded: "ok", failed: "bad" };
const jobStateLabels: Record<PanelJobState, string> = { queued: "Queued", running: "Running", succeeded: "Succeeded", failed: "Failed" };

function HostSetup() {
  return (
    <div className="space-y-2">
      <p className="text-xs text-fg-muted">
        Run this as root on the panel host, from the Cove release checkout. Replace <span className="font-mono">/opt/cove</span> if Cove is installed elsewhere; the installation and its parent directories must be owned by root and not writable by group or others. The auto-update installer also installs it.
      </p>
      <div className="flex flex-wrap items-center gap-2">
        <code className="min-w-0 flex-1 basis-64 rounded-md border border-line-input bg-panel px-2.5 py-1.5 font-mono text-xs break-all text-fg">{SETUP_COMMAND}</code>
        <CopyButton text={SETUP_COMMAND} label="Copy command" />
      </div>
    </div>
  );
}

function RefreshNotice({ recovery, error }: { recovery: Recovery; error: unknown }) {
  const restoring = recovery.jobs?.some((job) => job.action === "restore" && (job.state === "queued" || job.state === "running"));
  if (restoring) {
    return (
      <Notice tone="warn" title="The panel is restoring a snapshot" action={<Button size="sm" onClick={() => window.location.reload()}>Reload</Button>}>
        The dashboard is unavailable while the host restores and restarts the panel. Reload once it is back; you may need to sign in again because sessions and tokens can change with the restored data. Do not repeat the restore.
      </Notice>
    );
  }
  return (
    <Notice tone="warn" title="Could not refresh panel recovery status">
      Showing the last status received. {errorMessage(error)}
    </Notice>
  );
}

function HostTimer({ recovery, status }: { recovery: Recovery; status: HostStatus }) {
  const { tone, label } = hostStatusLabels[status];
  return (
    <Subsection layout="row" title="Host timer" description="Runs as root on the panel host and checks in every minute. The panel container never gets host access or the repository password.">
      <div className="space-y-1">
        <StatusLabel tone={tone} label={label} />
        {recovery.checked_at && (
          <p className="text-xs text-fg-muted">
            Last contact <RelativeTime value={recovery.checked_at} />
            {status === "quiet" && ". A long backup can delay check-ins, so this does not mean the host is offline."}
          </p>
        )}
        {recovery.agent_id && <p className="font-mono text-xs break-all text-fg-subtle">{recovery.agent_id}</p>}
      </div>
      {status !== "connected" && <HostSetup />}
    </Subsection>
  );
}

function HostNotices({ recovery, blocked, onResume }: { recovery: Recovery; blocked: string | undefined; onResume: () => void }) {
  return (
    <>
      {recovery.pending_recovery && (
        <Notice tone="warn" title="The host is recovering an unfinished action">
          The host retries its journal until it completes; new actions are refused meanwhile. If it keeps failing, inspect the host and correct its configuration. The host does not roll back to the current database automatically.
        </Notice>
      )}
      {recovery.updates_paused && (
        <Notice
          tone="warn"
          title="Panel updates are paused"
          action={
            <Button size="sm" disabled={blocked !== undefined} title={blocked} onClick={onResume}>
              Resume updates
            </Button>
          }
        >
          Updates stay paused after a restore so the restored version is not upgraded straight away. Review workers and configuration, then resume.
        </Notice>
      )}
      {recovery.offsite_pending && (
        <Notice tone="info" title="An offsite copy is waiting to be retried">
          Local backups and restores still work, and that snapshot is kept until the copy succeeds. The host retries before its next remote write. You can correct credentials for the same storage location, but not change the destination until it succeeds.
        </Notice>
      )}
      {recovery.last_error && (
        <Notice tone="warn" title="Last host error">
          <span className="break-words">{recovery.last_error}</span>
        </Notice>
      )}
    </>
  );
}

function Status({ recovery, refreshError, blocked }: { recovery: Recovery; refreshError: unknown; blocked: string | undefined }) {
  const now = useNow(15_000);
  const online = useOnline();
  const action = usePanelRecoveryAction();
  const [resuming, setResuming] = useState(false);
  const status = hostStatus(recovery, now);
  const blockedHint = (status === "quiet" || !online) && !recovery.pending_recovery ? blocked : undefined;
  const backingUp = action.isPending && action.variables.action === "backup";

  return (
    <Section
      title={title}
      description={description}
      flush
      actions={
        <Button disabled={blocked !== undefined} pending={backingUp} onClick={() => action.mutate({ action: "backup" })} title={blocked}>
          {!backingUp && <HardDriveDownload aria-hidden className="size-3.5" />}
          Back up now
        </Button>
      }
    >
      <div className="divide-y divide-line">
        {refreshError !== null && (
          <div className="p-4">
            <RefreshNotice recovery={recovery} error={refreshError} />
          </div>
        )}
        <HostTimer recovery={recovery} status={status} />
        <div className="space-y-3 p-4 empty:hidden">
          <HostNotices recovery={recovery} blocked={blocked} onResume={() => setResuming(true)} />
          {blockedHint && <p className="text-xs text-fg-muted">{blockedHint}</p>}
          {action.isError && (
            <Notice tone="error" title="Could not start the recovery action">
              {errorMessage(action.error)}
            </Notice>
          )}
          {action.isSuccess && (
            <Notice tone="success" title={`${jobActionLabels[action.variables.action]} queued`}>
              The host picks it up within a minute. It keeps running if you close this page.
            </Notice>
          )}
        </div>
      </div>
      <ConfirmDialog
        open={resuming}
        title="Resume panel updates?"
        confirmLabel="Resume updates"
        pending={action.isPending && action.variables.action === "resume_updates"}
        onCancel={() => setResuming(false)}
        onConfirm={() => action.mutate({ action: "resume_updates", confirm: true }, { onSettled: () => setResuming(false) })}
      >
        The host updater may install a newer panel version on its next check, replacing the restored one. Every update first takes a verified snapshot. Resume only after reviewing workers and configuration.
      </ConfirmDialog>
    </Section>
  );
}

type NumberKey = "interval_hours" | "keep_last" | "keep_days" | "max_mib";

function NumberField({ field, label, hint, min, max, value, error, onChange }: { field: NumberKey; label: string; hint: string; min: number; max: number; value: string; error?: string; onChange: (value: string) => void }) {
  const id = `panel-backup-${field}`;
  return (
    <Field id={id} label={label} hint={hint} error={error} layout="row">
      <input
        id={id}
        type="number"
        inputMode="numeric"
        min={min}
        max={max}
        step={1}
        className={inputClass}
        value={value}
        onChange={(event) => onChange(event.target.value)}
        aria-invalid={error ? true : undefined}
        aria-describedby={error ? `${id}-hint ${id}-error` : `${id}-hint`}
      />
    </Field>
  );
}

function OffsiteFields({ recovery, draft, error, onChange }: { recovery: Recovery; draft: ConfigDraft; error?: string; onChange: (patch: Partial<ConfigDraft>) => void }) {
  const locations = useQuery(connectionsQuery("storage-locations"));
  const saved = recovery.config;
  const storageOptions = (locations.data ?? []).filter((location) => location.storage);
  const savedMissing = saved.storage_id !== "" && locations.isSuccess && !storageOptions.some((location) => location.id === saved.storage_id);
  const newPassword = needsNewPassword(draft, saved, recovery.password_set);
  return (
    <>
      <Field
        id="panel-backup-storage"
        label="Offsite copy"
        layout="row"
        hint={locations.isError ? `Could not load storage locations: ${errorMessage(locations.error)}` : "Copies each verified snapshot to a separate cove-panel folder in a storage location of the current team."}
      >
        <Select id="panel-backup-storage" value={draft.storage_id} aria-describedby="panel-backup-storage-hint" disabled={locations.isPending} onChange={(event) => onChange({ storage_id: event.target.value, password: "" })}>
          <option value="">Local only</option>
          {savedMissing && <option value={saved.storage_id}>Unavailable storage location</option>}
          {storageOptions.map((location) => (
            <option key={location.id} value={location.id}>
              {location.name} ({location.storage?.protocol})
            </option>
          ))}
        </Select>
        {draft.storage_id !== saved.storage_id && recovery.offsite_pending && <p className="text-xs text-warn">A deferred offsite copy must succeed before the destination can change.</p>}
      </Field>
      {draft.storage_id && (
        <Field
          id="panel-backup-password"
          label="Offsite encryption password"
          optional={!newPassword}
          error={error}
          layout="row"
          hint={
            newPassword
              ? "16 to 4,096 bytes, independent of the local repository password. Keep a copy outside Cove: it is needed to recover from the offsite copy and is never shown again."
              : "Saved. Leave blank to keep it, or enter a new one."
          }
        >
          <input
            id="panel-backup-password"
            type="password"
            autoComplete="new-password"
            className={inputClass}
            value={draft.password}
            onChange={(event) => onChange({ password: event.target.value })}
            aria-invalid={error ? true : undefined}
            aria-describedby={error ? "panel-backup-password-hint panel-backup-password-error" : "panel-backup-password-hint"}
          />
        </Field>
      )}
    </>
  );
}

function Schedule({ recovery }: { recovery: Recovery }) {
  const save = useSavePanelBackup();
  const online = useOnline();
  const [draft, setDraft] = useState<ConfigDraft>(() => configDraft(recovery.config));
  const [errors, setErrors] = useState<ConfigErrors>({});
  const saved = recovery.config;
  const dirty = draftChanged(draft, saved);
  const update = (patch: Partial<ConfigDraft>) => setDraft((current) => ({ ...current, ...patch }));

  function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    const parsed = parseConfigDraft(draft, saved, recovery.password_set);
    if ("errors" in parsed) {
      setErrors(parsed.errors);
      const first = (["interval_hours", "keep_last", "keep_days", "max_mib", "password"] as const).find((key) => parsed.errors[key]);
      if (first) document.getElementById(`panel-backup-${first}`)?.focus();
      return;
    }
    setErrors({});
    save.mutate(parsed.input, { onSuccess: (result) => setDraft(configDraft(result.config)) });
  }

  return (
    <form onSubmit={submit} noValidate autoComplete="off">
      <UnsavedChangesGuard dirty={dirty && !save.isPending} />
      <Section
        title="Panel backup schedule"
        description="Saved settings apply on the host's next check-in. Old snapshots are pruned only after a new verified backup or restore, never when you save."
        width="form"
        footer={
          <SaveBar
            dirty={dirty}
            pending={save.isPending}
            blocked={!online}
            onDiscard={() => {
              setDraft(configDraft(saved));
              setErrors({});
              save.reset();
            }}
            status={save.isSuccess ? "Saved. The host uses these settings on its next check-in." : undefined}
          />
        }
      >
        <div className="space-y-4">
          <SettingRows>
            <SwitchField
              id="panel-backup-enabled"
              layout="row"
              label="Scheduled backups"
              description="The verified snapshot taken before each panel update runs even when this is off."
              checked={draft.enabled}
              onChange={(event) => update({ enabled: event.target.checked })}
            />
            <NumberField field="interval_hours" label="Back up every (hours)" hint="From 1 to 8,760 hours." min={1} max={8760} value={draft.interval_hours} error={errors.interval_hours} onChange={(value) => update({ interval_hours: value })} />
            <NumberField field="keep_last" label="Keep at least (snapshots)" hint="From 1 to 100." min={1} max={100} value={draft.keep_last} error={errors.keep_last} onChange={(value) => update({ keep_last: value })} />
            <NumberField
              field="keep_days"
              label="Keep for (days)"
              hint="From 1 to 365. A snapshot is kept while it is within both limits; the newest is always kept."
              min={1}
              max={365}
              value={draft.keep_days}
              error={errors.keep_days}
              onChange={(value) => update({ keep_days: value })}
            />
            <NumberField
              field="max_mib"
              label="Largest database dump (MiB)"
              hint={`Currently ${formatBinaryBytes(saved.max_bytes)}. Allowed ${formatBinaryBytes(MIN_BACKUP_BYTES)} to ${formatBinaryBytes(MAX_BACKUP_BYTES)}.`}
              min={MIN_BACKUP_BYTES / 1048576}
              max={MAX_BACKUP_BYTES / 1048576}
              value={draft.max_mib}
              error={errors.max_mib}
              onChange={(value) => update({ max_mib: value })}
            />
            <OffsiteFields recovery={recovery} draft={draft} error={errors.password} onChange={update} />
            <SettingRow label="Local repository" description="Encrypted with a private password file beside it on the host. Keep a copy of that password elsewhere; Cove cannot show or download it.">
              <span className="font-mono text-xs break-all">&lt;installation&gt;/.cove-panel-recovery/repository</span>
            </SettingRow>
          </SettingRows>
          <ul className="list-disc space-y-1 pl-5 text-xs text-fg-muted">
            <li>Each backup contains the PostgreSQL dump, runtime secrets and configuration, the installed Compose and Traefik configuration, and ACME certificates. The panel keeps running while it is taken.</li>
            <li>Every dump is restored into an isolated PostgreSQL with 2 GiB of memory and 2 GiB of temporary space before it is kept. Larger databases fail verification and nothing is published.</li>
            <li>The host needs free space of at least twice the dump limit plus 256 MiB before it starts. Raising the limit does not add space.</li>
            <li>Resource and worker data, and the database's physical files, are not included. Recovering after losing the host, or from the offsite copy, is done by an operator on the host.</li>
          </ul>
          {!online && <Notice tone="warn">You are offline. Reconnect to save.</Notice>}
          {save.isError && (
            <Notice tone="error" title="Could not save panel backup settings">
              {errorMessage(save.error)}
            </Notice>
          )}
        </div>
      </Section>
    </form>
  );
}

function RestoreDialog({ snapshot, publicOrigin, onClose }: { snapshot: PanelSnapshot; publicOrigin: string; onClose: () => void }) {
  const action = usePanelRecoveryAction();
  const online = useOnline();
  const [typed, setTyped] = useState("");
  const code = shortId(snapshot.id);
  const id = "panel-restore-confirm";

  function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    if (typed.trim() !== code) return;
    action.mutate({ action: "restore", snapshot_id: snapshot.id, confirm: true }, { onSuccess: onClose });
  }

  return (
    <Dialog open title="Roll back the panel to this snapshot?" onClose={onClose}>
      <form onSubmit={submit} className="space-y-4">
        <DefinitionList
          compact
          items={[
            { term: "Taken", value: formatAbsolute(snapshot.created_at) },
            { term: "Version", value: <span className="font-mono text-xs break-all">{snapshot.version || "Unknown"}</span> },
            { term: "Snapshot", value: <span className="font-mono text-xs break-all">{snapshot.id}</span> },
            { term: "Address", value: <span className="break-all">{publicOrigin}</span> },
          ]}
        />
        <ul className="list-disc space-y-1 pl-5 text-xs">
          <li>The panel database, images, settings, secrets and certificates return to exactly this snapshot. A fresh verified safety snapshot is taken first.</li>
          <li>The dashboard is down while it restores. Accounts, settings and audit records changed since then are lost, and current sessions may end.</li>
          <li>Resource and worker data is not rolled back. Unfinished actions from the snapshot are cancelled and previews are pinned for review.</li>
          <li>The host refuses if resource operations are still queued or running once the panel stops, and restarts the current panel. If a restore fails, the host keeps retrying it.</li>
          <li>
            Panel updates stay paused afterwards until you resume them. If the restored version has no panel recovery settings, resume on the host with{" "}
            <code className="font-mono break-all">{RESUME_COMMAND}</code>.
          </li>
        </ul>
        <Field id={id} label={`Type ${code} to confirm`}>
          <input id={id} className={`${inputClass} font-mono`} value={typed} onChange={(event) => setTyped(event.target.value)} autoComplete="off" spellCheck={false} required />
        </Field>
        {action.isError && (
          <Notice tone="error" title="Could not start the restore">
            {errorMessage(action.error)}
          </Notice>
        )}
        <div className="flex flex-wrap justify-end gap-2">
          <Button variant="ghost" onClick={onClose} disabled={action.isPending}>
            Cancel
          </Button>
          <Button type="submit" variant="danger" disabled={!online || typed.trim() !== code} pending={action.isPending}>
            Restore snapshot
          </Button>
        </div>
      </form>
    </Dialog>
  );
}

function SnapshotStatus({ snapshot }: { snapshot: PanelSnapshot }) {
  return (
    <div className="flex flex-col gap-0.5">
      <StatusLabel tone={snapshot.verified ? "ok" : "warn"} label={snapshot.verified ? "Verified" : "Not verified"} />
      <span className="text-xs text-fg-muted" title={snapshot.offsite ? "Confirmed when it was written. Offsite retention can remove it later." : undefined}>
        {snapshot.offsite ? "Offsite copy confirmed" : "Offsite copy not confirmed"}
      </span>
    </div>
  );
}

function Snapshots({ recovery, blocked }: { recovery: Recovery; blocked: string | undefined }) {
  const [restoring, setRestoring] = useState<PanelSnapshot | null>(null);
  const snapshots = sortedSnapshots(recovery.snapshots);
  return (
    <Section
      title="Panel snapshots"
      description="Verified snapshots in the local repository. Snapshots protected for a restore or a pending offsite copy are kept even beyond the retention limits."
      flush
    >
      {snapshots.length === 0 ? (
        <p className="px-4 py-6 text-[13px] text-fg-muted">{recovery.agent_id ? "No snapshots yet. Back up now or wait for the schedule." : "Snapshots appear here once the host timer is installed and has taken a backup."}</p>
      ) : (
        <table className="w-full text-left">
          <caption className="sr-only">Panel snapshots, newest first</caption>
          <thead>
            <tr className={tableHeadClass}>
              <th scope="col" className="py-2 pr-4 pl-4">Taken</th>
              <th scope="col" className="hidden py-2 pr-4 md:table-cell">Version</th>
              <th scope="col" className="hidden py-2 pr-4 text-right sm:table-cell">Size</th>
              <th scope="col" className="hidden py-2 pr-4 sm:table-cell">Status</th>
              <th scope="col" className="py-2 pr-4 text-right">
                <span className="sr-only">Actions</span>
              </th>
            </tr>
          </thead>
          <tbody>
            {snapshots.map((snapshot) => {
              const reason = restoreBlockReason(snapshot, recovery.public_origin);
              return (
                <tr key={snapshot.id} className={tableRowClass}>
                  <td className="py-2.5 pr-4 pl-4 align-top">
                    <RelativeTime value={snapshot.created_at} className="text-[13px] text-fg" />
                    <span className="ml-2 font-mono text-xs text-fg-subtle">{shortId(snapshot.id)}</span>
                    <p className="mt-0.5 font-mono text-xs break-all text-fg-muted md:hidden">
                      {snapshot.version || "Unknown version"}
                      <span className="font-sans sm:hidden"> · {formatBinaryBytes(snapshot.bytes)}</span>
                    </p>
                    <div className="mt-1 sm:hidden">
                      <SnapshotStatus snapshot={snapshot} />
                    </div>
                  </td>
                  <td className="hidden py-2.5 pr-4 align-top md:table-cell">
                    <span className="font-mono text-xs break-all text-fg-muted">{snapshot.version || "Unknown"}</span>
                    {snapshot.revision && <span className="ml-2 font-mono text-xs text-fg-subtle" title={snapshot.revision}>{snapshot.revision.slice(0, 7)}</span>}
                  </td>
                  <td className="hidden py-2.5 pr-4 text-right align-top text-xs text-fg-muted tabular-nums sm:table-cell">{formatBinaryBytes(snapshot.bytes)}</td>
                  <td className="hidden py-2.5 pr-4 align-top sm:table-cell">
                    <SnapshotStatus snapshot={snapshot} />
                  </td>
                  <td className="py-2.5 pr-4 text-right align-top">
                    {reason ? (
                      <span className="inline-block max-w-32 text-xs wrap-anywhere text-fg-muted sm:max-w-48">{reason}</span>
                    ) : (
                      <Button size="sm" variant="ghost" disabled={blocked !== undefined} title={blocked} onClick={() => setRestoring(snapshot)}>
                        <RotateCw aria-hidden className="size-3.5" />
                        Restore<span className="sr-only"> snapshot from {formatAbsolute(snapshot.created_at)}</span>
                      </Button>
                    )}
                  </td>
                </tr>
              );
            })}
          </tbody>
        </table>
      )}
      {restoring && <RestoreDialog snapshot={restoring} publicOrigin={recovery.public_origin} onClose={() => setRestoring(null)} />}
    </Section>
  );
}

function History({ recovery }: { recovery: Recovery }) {
  const jobs = recovery.jobs ?? [];
  return (
    <Section title="Recovery history" description="The 100 most recent backup, restore and resume actions." flush>
      {jobs.length === 0 ? (
        <p className="px-4 py-6 text-[13px] text-fg-muted">No recovery actions yet.</p>
      ) : (
        <table className="w-full text-left">
          <caption className="sr-only">Panel recovery actions, newest first</caption>
          <thead>
            <tr className={tableHeadClass}>
              <th scope="col" className="py-2 pr-4 pl-4">Action</th>
              <th scope="col" className="py-2 pr-4">Status</th>
              <th scope="col" className="hidden py-2 pr-4 text-right sm:table-cell">Finished</th>
              <th scope="col" className="py-2 pr-4 text-right">Requested</th>
            </tr>
          </thead>
          <tbody>
            {jobs.map((job) => (
              <tr key={job.id} className={tableRowClass}>
                <td className="py-2.5 pr-4 pl-4 align-top">
                  <span className="text-[13px] text-fg">{jobActionLabels[job.action] ?? job.action}</span>
                  {job.snapshot_id && <span className="ml-2 font-mono text-xs text-fg-subtle">{shortId(job.snapshot_id)}</span>}
                  {job.error && <p className="mt-0.5 text-xs break-words text-bad">{job.error}</p>}
                </td>
                <td className="py-2.5 pr-4 align-top">
                  <StatusLabel tone={jobTones[job.state] ?? "neutral"} label={jobStateLabels[job.state] ?? job.state} pulse={job.state === "running" || job.state === "queued"} />
                </td>
                <td className="hidden py-2.5 pr-4 text-right align-top text-xs text-fg-muted tabular-nums sm:table-cell">{job.finished_at ? <RelativeTime value={job.finished_at} /> : "Not finished"}</td>
                <td className="py-2.5 pr-4 text-right align-top text-xs text-fg-muted tabular-nums">
                  <RelativeTime value={job.requested_at} />
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </Section>
  );
}

function Loaded({ recovery, refreshError }: { recovery: Recovery; refreshError: unknown }) {
  const now = useNow(15_000);
  const online = useOnline();
  const blocked = actionBlockReason(recovery, now, online) ?? (refreshError !== null ? "The current status could not be refreshed." : undefined);
  return (
    <>
      <Status recovery={recovery} refreshError={refreshError} blocked={blocked} />
      <Schedule recovery={recovery} />
      <Snapshots recovery={recovery} blocked={blocked} />
      <History recovery={recovery} />
    </>
  );
}

export function PanelRecovery() {
  const recovery = useQuery(panelRecoveryQuery);
  if (recovery.isPending) {
    return (
      <Section title={title} description={description} flush>
        <LoadingRows label="Loading panel recovery" rows={2} />
      </Section>
    );
  }
  if (!recovery.data) {
    return (
      <Section title={title} description={description}>
        <QueryError what="panel recovery" error={recovery.error} onRetry={() => void recovery.refetch()} retrying={recovery.isFetching} />
      </Section>
    );
  }
  return <Loaded recovery={recovery.data} refreshError={recovery.isError ? recovery.error : null} />;
}

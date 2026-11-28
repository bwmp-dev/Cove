import { useQuery, useQueryClient } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { ArrowRightLeft, Eye, EyeOff, KeyRound, RotateCcw, Trash2 } from "lucide-react";
import { useState, type FormEvent, type ReactNode } from "react";
import { errorMessage, isStatus } from "../api/client";
import {
  databaseDetailsQuery,
  databaseKeys,
  databaseMaintenanceQuery,
  usePreflight,
  useRetryMaintenance,
  useRevealCredentials,
  useStartMaintenance,
  type DatabaseCredentials,
  type DatabaseDetails,
  type Endpoint,
  type MaintenanceKind,
  type MaintenanceTask,
} from "../api/databases";
import { workloadOperationsQuery } from "../api/queries";
import type { Workload } from "../api/types";
import { Checkbox } from "../components/Checkbox";
import { CopyDialog, CutoverDialog, DiscardDialog, RuntimeInspection } from "../components/DatabaseCopy";
import { DatabaseSettings, Dependents, SettingsStatus } from "../components/DatabaseSettings";
import { Dialog } from "../components/ConfirmDialog";
import { CopyButton } from "../components/CopyButton";
import { RelativeTime } from "../components/RelativeTime";
import { StatusLabel } from "../components/status";
import { Button, DefinitionList, EmptyState, LoadingRows, Notice, QueryError, Section, Subsection } from "../components/ui";
import { engineName, engineNotes, exportLabels, extensionNeedsVariant, extensionNotes, inspectableEngines } from "../lib/databases";
import { useOnline, useReviewed } from "../lib/hooks";
import type { Tone } from "../lib/nodes";
import { activeOperation } from "../lib/operations";
import { can } from "../lib/permissions";
import { buttonClass, linkClass } from "../lib/styles";
import { useResource } from "./workload-detail";

export function ResourceDatabase() {
  const workload = useResource();
  if (!workload.database) {
    return <EmptyState title="Not a managed database">Connection details and maintenance apply to database resources created from the Database option.</EmptyState>;
  }
  return <DatabasePanels workload={workload} />;
}

function DatabasePanels({ workload }: { workload: Workload }) {
  const details = useQuery({
    ...databaseDetailsQuery(workload.id),
    // A rotation or upgrade replaces the reviewed configuration when it succeeds, which changes the revision.
    refetchInterval: (query) => {
      const data = query.state.data;
      return data?.maintenance_locked || data?.settings_state === "queued" || data?.settings_state === "running" ? 3_000 : false;
    },
  });
  if (details.isPending) return <LoadingRows label="Loading database details" rows={4} />;
  if (details.isError) {
    return (
      <div className="px-4 pt-5 sm:px-6">
        <QueryError what="database details" error={details.error} onRetry={() => void details.refetch()} retrying={details.isFetching} />
      </div>
    );
  }
  const data = details.data;
  // Credentials, settings and maintenance all need Edit configuration and Read secrets; the API enforces the same pair.
  const editor = can(workload.permissions, "edit") && can(workload.permissions, "read_secrets");
  return (
    <>
      {data.maintenance_locked && (
        <div className="px-4 pt-5 sm:px-6">
          <Notice tone="warn" title="Maintenance is not settled">
            Credential reads, bound credential resolution and conflicting changes are blocked until the recorded maintenance succeeds. Retry a failed task below; do not start another change to recover.
          </Notice>
        </div>
      )}
      {data.database.settings_operation && (
        <div className="px-4 pt-5 empty:hidden sm:px-6">
          <SettingsStatus details={data} />
        </div>
      )}
      <Section title="Connection" description="Generated when the database was created. Applications reach it only through a private connection." flush>
        <div className="divide-y divide-line">
          <Subsection title="Details">
            <DefinitionList
              items={[
                { term: "Engine", value: `${engineName(data.database.engine)} ${data.database.version}${data.database.variant ? ` (${data.database.variant === "postgis" ? "PostGIS" : "pgvector"} image)` : ""}` },
                { term: "Image", value: <span className="font-mono text-xs break-all">{data.database.image}</span> },
                ...(data.database.public_access
                  ? [
                      { term: "TLS hostname", value: <span className="font-mono text-xs break-all">{data.host}</span> },
                      { term: "Private hostname", value: <span className="font-mono text-xs break-all">{data.database.private_host ?? data.host}</span> },
                    ]
                  : [{ term: "Private hostname", value: <span className="font-mono text-xs break-all">{data.host}</span> }]),
                { term: "Service and port", value: <span className="font-mono text-xs">{data.database.service}:{data.database.port}</span> },
                { term: "User", value: <span className="font-mono text-xs">{data.user}</span> },
                { term: "Database", value: <span className="font-mono text-xs">{data.name}</span> },
              ]}
            />
            {engineNotes(data.database.engine) && <p className="text-xs text-fg-muted">{engineNotes(data.database.engine)}</p>}
          </Subsection>
          <Subsection title="Use from another resource" description="A binding shares values, a private connection grants network access. Both are needed.">
            <ol className="list-decimal space-y-1.5 pl-5 text-[13px] text-fg">
              <li>
                On the consuming resource, open <span className="font-medium">Environment variables</span> and bind the exported keys from this database under Resource variable bindings.
              </li>
              <li>
                Create a private connection from the consumer to service <span className="font-mono text-xs">{data.database.service}</span>, port{" "}
                <span className="font-mono text-xs">{data.database.port}</span>
                {data.database.engine === "clickhouse" && <> (or HTTP port <span className="font-mono text-xs">8123</span>)</>} and hostname{" "}
                <span className="font-mono text-xs break-all">{data.host}</span>. The connection form fills these in when this database is the target.
              </li>
              <li>Deploy the consumer. Bound values are resolved at deploy time and the connection is usable after both sides apply it.</li>
            </ol>
            {data.database.pooler && <p className="text-xs text-fg-muted">To use the pooler instead, bind the DB_POOL_* keys and choose the pooler endpoint when creating the private connection.</p>}
            <ExportList exports={data.exports} />
          </Subsection>
          {editor ? (
            <Credentials key={`${data.revision}:${data.maintenance_locked}:${data.settings_state ?? ""}`} workloadId={workload.id} locked={data.maintenance_locked} />
          ) : (
            <Subsection title="Credentials">
              <p className="text-[13px] text-fg-muted">Edit configuration and Read secrets permissions are needed to reveal credentials. Bindings deliver them to applications without revealing them.</p>
            </Subsection>
          )}
        </div>
      </Section>
      <Dependents details={data} />
      <DatabaseSettings workload={workload} details={data} editor={editor} />
      <Maintenance workload={workload} details={data} editor={editor} />
    </>
  );
}

function ExportList({ exports }: { exports: DatabaseDetails["exports"] }) {
  return (
    <ul aria-label="Exported variables" className="divide-y divide-line rounded-md border border-line">
      {exports.map((item) => (
        <li key={item.key} className="flex flex-wrap items-center justify-between gap-x-4 gap-y-0.5 px-3 py-1.5">
          <span className="font-mono text-xs text-fg">{item.key}</span>
          <span className="text-xs text-fg-muted">
            {exportLabels[item.key] ?? item.key}
            {item.secret ? " · Secret" : ""}
          </span>
        </li>
      ))}
    </ul>
  );
}

// Credentials live only in this component's state. Remounting it (a new configuration revision, navigation) discards them.
function Credentials({ workloadId, locked }: { workloadId: string; locked: boolean }) {
  const reveal = useRevealCredentials(workloadId);
  const online = useOnline();
  const values = reveal.data;
  return (
    <Subsection
      title="Credentials"
      description="Each reveal is recorded in the audit log. Prefer bindings over copying credentials into other configuration."
      actions={
        values ? (
          <Button onClick={() => reveal.reset()}>
            <EyeOff aria-hidden className="size-3.5" />
            Hide credentials
          </Button>
        ) : (
          <Button disabled={!online || locked} pending={reveal.isPending} onClick={() => reveal.mutate()}>
            <KeyRound aria-hidden className="size-3.5" />
            Reveal credentials
          </Button>
        )
      }
    >
      {locked && !values && <p className="text-xs text-fg-muted">Unavailable until maintenance succeeds.</p>}
      {reveal.isError && (
        <Notice tone="error" title="Could not reveal credentials">
          {isStatus(reveal.error, 403) ? "Edit configuration and Read secrets permissions are needed to read credentials." : errorMessage(reveal.error)}
        </Notice>
      )}
      {values && (
        <div className="space-y-3">
          <CredentialGroup title={values.public ? "Private network" : undefined} values={values} />
          {values.pooler && <CredentialGroup title="Pooler (transaction mode)" values={values.pooler} />}
          {values.public && <CredentialGroup title="Public TLS" values={values.public} />}
        </div>
      )}
    </Subsection>
  );
}

function CredentialGroup({ title, values }: { title?: string; values: Endpoint }) {
  if (!title) return <CredentialRows values={values} />;
  return (
    <div className="space-y-1.5">
      <h4 className="text-xs font-medium text-fg">{title}</h4>
      <CredentialRows values={values} />
    </div>
  );
}

function CredentialRows({ values }: { values: Endpoint | DatabaseCredentials }) {
  const rows: { label: string; value: string; secret?: boolean }[] = [
    { label: "Host", value: values.host },
    { label: "Port", value: String(values.port) },
    { label: "User", value: values.user },
    { label: "Database", value: values.database },
    { label: "Password", value: values.password, secret: true },
    { label: "Connection URL", value: values.url, secret: true },
  ];
  return (
    <dl className="divide-y divide-line rounded-md border border-line">
      {rows.map((row) => (
        <CredentialRow key={row.label} {...row} />
      ))}
    </dl>
  );
}

function CredentialRow({ label, value, secret = false }: { label: string; value: string; secret?: boolean }) {
  const [shown, setShown] = useState(!secret);
  return (
    <div className="grid gap-x-4 gap-y-1 px-3 py-2 sm:grid-cols-[9rem_minmax(0,1fr)_auto] sm:items-center">
      <dt className="text-xs text-fg-muted">{label}</dt>
      <dd className="min-w-0 font-mono text-xs break-all text-fg">{shown ? value : <span aria-label={`${label} hidden`}>••••••••••••</span>}</dd>
      <dd className="flex flex-wrap items-center gap-2">
        {secret && (
          <Button size="sm" variant="ghost" aria-pressed={shown} aria-label={shown ? `Hide ${label.toLowerCase()}` : `Show ${label.toLowerCase()}`} onClick={() => setShown(!shown)}>
            {shown ? <EyeOff aria-hidden className="size-3.5" /> : <Eye aria-hidden className="size-3.5" />}
            {shown ? "Hide" : "Show"}
          </Button>
        )}
        <CopyButton text={value} label={`Copy ${label.toLowerCase()}`} variant="ghost" />
      </dd>
    </div>
  );
}


const kindLabels: Record<MaintenanceKind, string> = { rotate: "Credential rotation", extension: "Extension change", upgrade: "Upgrade copy" };
const taskStates: Record<MaintenanceTask["state"], { tone: Tone; label: string }> = {
  pending: { tone: "info", label: "Pending" },
  failed: { tone: "bad", label: "Failed, retry required" },
  succeeded: { tone: "ok", label: "Succeeded" },
  discarded: { tone: "neutral", label: "Discarded" },
};
const failedOperation = (task: MaintenanceTask) => task.operation_state === "failed" || task.operation_state === "interrupted";
const retryable = (task: MaintenanceTask) => (task.state === "pending" || task.state === "failed") && failedOperation(task);
const discardable = (task: MaintenanceTask) => task.kind === "upgrade" && task.state === "failed" && (failedOperation(task) || task.operation_state === "cancelled");
const cutoverReady = (task: MaintenanceTask) => task.kind === "upgrade" && task.state === "succeeded" && !task.cutover_confirmed;

function Maintenance({ workload, details, editor }: { workload: Workload; details: DatabaseDetails; editor: boolean }) {
  const operations = useQuery({ ...workloadOperationsQuery(workload.id), enabled: editor });
  const online = useOnline();
  const active = Boolean(operations.data && activeOperation(operations.data, workload.id));
  const reason = blockedReason(details.maintenance_locked, details.maintenance_available !== false, active, online);
  const preflight = usePreflight(workload.id);
  // A result for an older revision describes a configuration that has since changed.
  const inspection = preflight.data?.revision === details.revision ? preflight.data : undefined;
  return (
    <Section
      title="Maintenance"
      description="Each change runs as a deployment operation against the reviewed configuration. Approvals, freezes and deployment windows still apply. Queued maintenance cannot be cancelled."
      flush
    >
      <div className="divide-y divide-line">
        {editor && <MaintenanceChanges workload={workload} details={details} reason={reason} />}
        {editor && details.database.engine === "postgresql" && <Extensions workload={workload} details={details} reason={reason} installed={inspection ? (inspection.runtime.extensions ?? []) : undefined} />}
        {editor && inspectableEngines.has(details.database.engine) && <RuntimeInspection workloadId={workload.id} details={details} disabled={reason} preflight={preflight} />}
        <MaintenanceHistory workload={workload} details={details} editor={editor} />
      </div>
    </Section>
  );
}

function blockedReason(locked: boolean, available: boolean, active: boolean, online: boolean) {
  if (locked) return "Settle the recorded maintenance first.";
  if (!available) return "Deploy the database, and start it if it is stopped, before running maintenance.";
  if (active) return "Wait for the active operation to finish.";
  if (!online) return "You are offline.";
  return null;
}

type MaintenanceRequestState = { kind: "rotate" } | { kind: "extension"; extension: string; enabled: boolean };

function MaintenanceChanges({ workload, details, reason }: { workload: Workload; details: DatabaseDetails; reason: string | null }) {
  const [dialog, setDialog] = useState<"rotate" | "copy" | null>(null);
  const { engine, version } = details.database;
  const targets = details.copy_targets ?? [];
  const copyNote = targets.length > 0 ? `Upgrade copies are offered to ${targets.map((target) => target.version).join(", ")}.` : "No upgrade copy is offered from this version.";
  return (
    <Subsection
      title="Changes"
      description={`${engineName(engine)} ${version}. ${copyNote}`}
      actions={
        <>
          <Button disabled={reason !== null} onClick={() => setDialog("rotate")}>
            Rotate password
          </Button>
          {targets.length > 0 && (
            <Button disabled={reason !== null} onClick={() => setDialog("copy")}>
              Create upgrade copy
            </Button>
          )}
        </>
      }
    >
      {reason && <p className="text-xs text-fg-muted">{reason}</p>}
      {dialog === "rotate" && <MaintenanceDialog request={{ kind: "rotate" }} workload={workload} details={details} onClose={() => setDialog(null)} />}
      {dialog === "copy" && <CopyDialog workload={workload} details={details} onClose={() => setDialog(null)} />}
    </Subsection>
  );
}

function extensionStatus(managed: boolean, installed: Set<string> | undefined, extension: string): { tone: Tone; label: string } {
  if (installed) {
    if (installed.has(extension)) return { tone: "ok", label: managed ? "Installed, managed by Cove" : "Installed, not managed" };
    return { tone: managed ? "warn" : "neutral", label: managed ? "Managed but not installed" : "Not installed" };
  }
  return managed ? { tone: "ok", label: "Managed by Cove, not inspected" } : { tone: "neutral", label: "Not managed, not inspected" };
}

// Without an inspection only Cove's managed configuration is known; the engine may hold other extensions.
function Extensions({ workload, details, reason, installed: runtime }: { workload: Workload; details: DatabaseDetails; reason: string | null; installed?: string[] }) {
  const installed = runtime ? new Set(runtime) : undefined;
  const [dialog, setDialog] = useState<MaintenanceRequestState | null>(null);
  const enabled = new Set(details.database.extensions);
  const options = details.extension_options ?? [];
  return (
    <Subsection
      title="Extensions"
      description={`Extensions supplied by the pinned image. Enabling runs CREATE EXTENSION IF NOT EXISTS; disabling uses RESTRICT and is refused while other objects depend on the extension. ${installed ? "Installed state comes from the last runtime inspection." : "Inspect the runtime below to see what the engine has installed."}`}
    >
      {options.length === 0 ? (
        <p className="text-[13px] text-fg-muted">No extensions are offered for this database.</p>
      ) : (
        <ul aria-label="Extensions" className="divide-y divide-line rounded-md border border-line">
          {options.map((extension) => {
            const managed = enabled.has(extension);
            const on = managed || Boolean(installed?.has(extension));
            const status = extensionStatus(managed, installed, extension);
            const needsVariant = !on && extensionNeedsVariant(extension, details.database.variant);
            return (
              <li key={extension} className="flex flex-wrap items-center justify-between gap-x-4 gap-y-1.5 px-3 py-2">
                <div className="min-w-0 flex-1 basis-56 space-y-0.5">
                  <div className="flex flex-wrap items-center gap-x-3 gap-y-1">
                    <span className="font-mono text-xs text-fg">{extension}</span>
                    <StatusLabel tone={status.tone} label={status.label} />
                  </div>
                  {extensionNotes[extension] && <p className="text-xs text-fg-muted">{extensionNotes[extension]}</p>}
                </div>
                {needsVariant ? (
                  <span className="text-xs text-fg-muted">Needs a copy to its image variant</span>
                ) : (
                  <Button size="sm" variant={on ? "ghost" : "secondary"} disabled={reason !== null} onClick={() => setDialog({ kind: "extension", extension, enabled: !on })} aria-label={`${on ? "Disable" : "Enable"} ${extension}`}>
                    {on ? "Disable" : "Enable"}
                  </Button>
                )}
              </li>
            );
          })}
        </ul>
      )}
      {dialog && <MaintenanceDialog request={dialog} workload={workload} details={details} onClose={() => setDialog(null)} />}
    </Subsection>
  );
}

function MaintenanceHistory({ workload, details, editor }: { workload: Workload; details: DatabaseDetails; editor: boolean }) {
  const tasks = useQuery(databaseMaintenanceQuery(workload.id));
  let body: ReactNode;
  if (tasks.isPending) body = <p className="text-[13px] text-fg-muted">Loading maintenance history</p>;
  else if (tasks.isError) body = <QueryError what="maintenance history" error={tasks.error} onRetry={() => void tasks.refetch()} retrying={tasks.isFetching} />;
  else if (tasks.data.length === 0) body = <p className="text-[13px] text-fg-muted">No maintenance has run.</p>;
  else
    body = (
      <ul aria-label="Maintenance tasks" className="divide-y divide-line rounded-md border border-line">
        {tasks.data.map((task) => (
          <TaskRow key={task.id} task={task} workload={workload} details={details} editor={editor} />
        ))}
      </ul>
    );
  return (
    <Subsection title="History" description="The 50 most recent maintenance tasks for this database, including copies made from it.">
      {body}
    </Subsection>
  );
}

function TaskRow({ task, workload, details, editor }: { task: MaintenanceTask; workload: Workload; details: DatabaseDetails; editor: boolean }) {
  const canCutover = task.can_cutover === true;
  const retry = useRetryMaintenance(workload.id);
  const online = useOnline();
  const [dialog, setDialog] = useState<"discard" | "cutover" | "retry" | null>(null);
  const state = taskStates[task.state];
  const copy = task.kind === "upgrade";
  const other = task.kind === "upgrade" ? (task.resource_id === workload.id ? task.source_id : task.resource_id) : "";
  return (
    <li className="space-y-1.5 px-3 py-2.5">
      <div className="flex flex-wrap items-center justify-between gap-x-4 gap-y-1">
        <div className="flex min-w-0 flex-wrap items-center gap-x-3 gap-y-1 text-[13px]">
          <span className="font-medium text-fg">{kindLabels[task.kind]}</span>
          <StatusLabel tone={state.tone} label={state.label} pulse={task.state === "pending"} />
          {task.cutover_confirmed && <StatusLabel tone="ok" label="Cut over" />}
          <RelativeTime value={task.created_at} className="text-xs text-fg-muted" />
        </div>
        <div className="flex flex-wrap items-center gap-3 text-xs">
          <Link to="/operations/$operationId" params={{ operationId: task.id }} className={linkClass}>
            View operation
          </Link>
          {other && (
            <Link to="/resources/$resourceId/database" params={{ resourceId: other }} className={linkClass}>
              {task.resource_id === workload.id ? "Open source" : "Open copy"}
            </Link>
          )}
          {editor && retryable(task) && (
            <Button size="sm" disabled={!online} pending={!copy && retry.isPending} onClick={() => (copy ? setDialog("retry") : retry.mutate({ task: task.id }))}>
              <RotateCcw aria-hidden className="size-3.5" />
              Retry
            </Button>
          )}
          {editor && discardable(task) && (
            <Button size="sm" variant="danger" disabled={!online} onClick={() => setDialog("discard")}>
              <Trash2 aria-hidden className="size-3.5" />
              Discard copy
            </Button>
          )}
          {cutoverReady(task) && (
            <Button size="sm" disabled={!online || !canCutover} onClick={() => setDialog("cutover")}>
              <ArrowRightLeft aria-hidden className="size-3.5" />
              Cut over
            </Button>
          )}
        </div>
      </div>
      {task.state !== "succeeded" && task.error && <p className="text-xs text-fg-muted">{task.error}</p>}
      {cutoverReady(task) && (
        <p className="text-xs text-fg-muted">
          The copy is verified. Consumers still use the source until a cut-over switches bindings and private connections.
          {canCutover ? "" : " Cut-over needs a team owner or admin who can also edit both the source and the copy."}
        </p>
      )}
      {editor && discardable(task) && <p className="text-xs text-fg-muted">The source was unlocked automatically. Retry the copy, or discard it to remove its data.</p>}
      {!copy && retry.isError && (
        <p role="alert" className="text-xs text-bad">
          Could not retry: {retryError(retry.error)}
        </p>
      )}
      {!copy && retry.isSuccess && (
        <p role="status" className="text-xs text-fg-muted">
          Retry queued with the same recorded change. Follow it on the operation page.
        </p>
      )}
      {dialog === "retry" && <RetryCopyDialog workload={workload} task={task} retry={retry} onClose={() => setDialog(null)} />}
      {dialog === "discard" && <DiscardDialog workload={workload} task={task} onClose={() => setDialog(null)} />}
      {dialog === "cutover" && <CutoverDialog workload={workload} details={details} task={task} onClose={() => setDialog(null)} />}
    </li>
  );
}

// The control plane refuses a retry while an operation is queued or running on the source or the copy.
function retryError(error: unknown) {
  const message = errorMessage(error);
  return isStatus(error, 409) ? `${message}. Wait for active operations on the source and the copy to finish, then retry.` : message;
}

function RetryCopyDialog({ workload: liveWorkload, task: liveTask, retry, onClose }: { workload: Workload; task: MaintenanceTask; retry: ReturnType<typeof useRetryMaintenance>; onClose: () => void }) {
  const workload = useReviewed(liveWorkload);
  const task = useReviewed(liveTask);
  const online = useOnline();
  const [writesStopped, setWritesStopped] = useState(false);
  const source = task.source_id === workload.id ? workload.name : "the source";

  function submit(event: FormEvent) {
    event.preventDefault();
    if (!writesStopped || !online) return;
    retry.mutate({ task: task.id, writesStopped: true });
  }

  return (
    <Dialog open title="Retry the upgrade copy?" onClose={onClose}>
      {retry.isSuccess ? (
        <div className="mt-3 space-y-4">
          <Notice tone="success" title="Retry queued">
            The same recorded copy runs again as an operation and takes a new snapshot of {source}. It is not ready until the operation succeeds.
          </Notice>
          <div className="flex flex-wrap items-center gap-3">
            <Link to="/operations/$operationId" params={{ operationId: task.id }} className={buttonClass("primary")} onClick={onClose}>
              View operation
            </Link>
            <Button variant="ghost" onClick={onClose}>
              Close
            </Button>
          </div>
        </div>
      ) : (
        <form className="mt-3 space-y-4" onSubmit={submit}>
          <p>Runs the same recorded copy again with its retained credentials and takes a new snapshot of {source}. The source stays unchanged.</p>
          <label className="flex items-start gap-2.5 text-[13px] text-fg">
            <Checkbox checked={writesStopped} onChange={(event) => setWritesStopped(event.target.checked)} />
            <span>Applications and any external writers to {source} are stopped now, and will stay stopped through validation and cut-over. Cove cannot detect writers outside it, and a snapshot taken during writes may be inconsistent.</span>
          </label>
          {retry.isError && (
            <Notice tone="error" title="Could not retry the copy">
              {retryError(retry.error)}
            </Notice>
          )}
          <div className="flex justify-end gap-2 border-t border-line pt-3">
            <Button variant="ghost" onClick={onClose} disabled={retry.isPending}>
              Cancel
            </Button>
            <Button type="submit" variant="primary" disabled={!writesStopped || !online} pending={retry.isPending}>
              Retry copy
            </Button>
          </div>
        </form>
      )}
    </Dialog>
  );
}

function dialogTitle(request: MaintenanceRequestState, engine: string) {
  if (request.kind === "rotate") return `Rotate the ${engineName(engine)} password?`;
  return `${request.enabled ? "Enable" : "Disable"} ${request.extension}?`;
}

function MaintenanceDialog({ request: liveRequest, workload: liveWorkload, details: liveDetails, onClose }: { request: MaintenanceRequestState; workload: Workload; details: DatabaseDetails; onClose: () => void }) {
  const request = useReviewed(liveRequest);
  const workload = useReviewed(liveWorkload);
  const details = useReviewed(liveDetails);
  const start = useStartMaintenance(workload.id);
  return (
    <Dialog open title={dialogTitle(request, details.database.engine)} onClose={onClose}>
      {start.data ? <MaintenanceQueued operationId={start.data.id} onClose={onClose} /> : <MaintenanceForm request={request} workload={workload} details={details} start={start} onClose={onClose} />}
    </Dialog>
  );
}

function MaintenanceQueued({ operationId, onClose }: { operationId: string; onClose: () => void }) {
  return (
    <div className="mt-3 space-y-4">
      <Notice tone="success" title="Maintenance queued">
        The change is recorded and runs as an operation. It is not complete until the operation succeeds.
      </Notice>
      <div className="flex flex-wrap items-center gap-3">
        <Link to="/operations/$operationId" params={{ operationId }} className={buttonClass("primary")} onClick={onClose}>
          View operation
        </Link>
        <Button variant="ghost" onClick={onClose}>
          Close
        </Button>
      </div>
    </div>
  );
}

function MaintenanceEffects({ request, workload, details }: { request: MaintenanceRequestState; workload: Workload; details: DatabaseDetails }) {
  const { engine, version } = details.database;
  const label = `${engineName(engine)} ${version}`;
  if (request.kind === "extension") {
    const note = extensionNotes[request.extension];
    if (request.enabled) {
      return (
        <>
          <p>
            Runs CREATE EXTENSION IF NOT EXISTS {request.extension} in database {details.name} on {label}. Only modules supplied by the image are offered.
          </p>
          {note && <p>{note}</p>}
        </>
      );
    }
    return (
      <>
        <p>
          Drops {request.extension} from database {details.name} on {label} with RESTRICT. If tables, columns, indexes or functions depend on it, the change is refused before it is queued; nothing is dropped with CASCADE. Applications that call its functions or types will fail.
        </p>
        {request.extension === "postgis" && <p>postgis_topology, postgis_tiger_geocoder and fuzzystrmatch are removed in the same transaction. The PostGIS image stays in use.</p>}
        {request.extension === "pg_stat_statements" && <p>Removes the library from shared_preload_libraries and restarts PostgreSQL. Clients are disconnected while it restarts.</p>}
      </>
    );
  }
  return (
    <>
      <p>
        Cove generates a new application password for {workload.name} ({label}) and reconnects the database. Clients may be interrupted. Existing authenticated sessions can stay open.
        {engine === "mysql" || engine === "mariadb" ? " The root credential is unchanged." : ""}
        {details.database.pooler ? " Pooled credentials change too." : ""}
      </p>
      <p>After the operation succeeds, deploy each bound resource again so its password and URL bindings resolve to the new value. Until then, credential reads and bound deployments are blocked.</p>
    </>
  );
}

function confirmationText(request: MaintenanceRequestState, label: string) {
  if (request.kind === "rotate") return "I understand clients may disconnect and consumers need a new deploy.";
  if (request.enabled) return `Enable ${request.extension} on ${label}.`;
  return `Disable ${request.extension} on ${label}. Applications that use it will fail.`;
}

function MaintenanceForm({ request, workload, details, start, onClose }: { request: MaintenanceRequestState; workload: Workload; details: DatabaseDetails; start: ReturnType<typeof useStartMaintenance>; onClose: () => void }) {
  const client = useQueryClient();
  const online = useOnline();
  const [confirmed, setConfirmed] = useState(false);
  const ready = online && confirmed;
  const destructive = request.kind === "rotate" || !request.enabled;

  function submit(event: FormEvent) {
    event.preventDefault();
    if (!ready) return;
    const options = request.kind === "extension" ? { extension: request.extension, enabled: request.enabled } : {};
    start.mutate(
      { kind: request.kind, revision: details.revision, confirm: true, ...options },
      {
        // A stale revision means the configuration changed; reload what the person reviews.
        onError: (error) => {
          if (isStatus(error, 409)) void client.invalidateQueries({ queryKey: databaseKeys.details(workload.id) });
        },
      },
    );
  }

  let submitLabel = "Rotate password";
  if (request.kind === "extension") submitLabel = request.enabled ? "Enable extension" : "Disable extension";
  return (
    <form className="mt-3 space-y-4" onSubmit={submit}>
      <div className="space-y-2">
        <MaintenanceEffects request={request} workload={workload} details={details} />
      </div>
      <label className="flex items-start gap-2.5 text-[13px] text-fg">
        <Checkbox checked={confirmed} onChange={(event) => setConfirmed(event.target.checked)} />
        <span>{confirmationText(request, `${engineName(details.database.engine)} ${details.database.version}`)}</span>
      </label>
      {start.isError && (
        <Notice tone="error" title="Could not start maintenance">
          {errorMessage(start.error)}
          {isStatus(start.error, 409) ? " The current details have been reloaded; close this dialog and review them before trying again." : ""}
        </Notice>
      )}
      <div className="flex justify-end gap-2 border-t border-line pt-3">
        <Button variant="ghost" onClick={onClose} disabled={start.isPending}>
          Cancel
        </Button>
        <Button type="submit" variant={destructive ? "danger" : "primary"} disabled={!ready} pending={start.isPending}>
          {submitLabel}
        </Button>
      </div>
    </form>
  );
}

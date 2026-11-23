import { useQuery, useQueryClient } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { useId, useState, type FormEvent } from "react";
import { errorMessage, isStatus } from "../api/client";
import {
  databaseDetailsQuery,
  databaseKeys,
  useCutover,
  useDiscardCopy,
  usePreflight,
  useStartMaintenance,
  type DatabaseDetails,
  type MaintenanceTask,
  type PreflightResult,
} from "../api/databases";
import { workloadQuery } from "../api/queries";
import type { Workload } from "../api/types";
import { engineName, versionLabel } from "../lib/databases";
import { useFirstLoaded, useOnline, useReviewed } from "../lib/hooks";
import { buttonClass, choiceClass, inputClass } from "../lib/styles";
import { Checkbox } from "./Checkbox";
import { Dialog } from "./ConfirmDialog";
import { Button, DefinitionList, Field, Notice, Subsection } from "./ui";

function RuntimeFacts({ engine, result, managed }: { engine: string; result: PreflightResult; managed?: string[] }) {
  const installed = result.runtime.extensions ?? [];
  const items = [
    { term: "Running version", value: <span className="font-mono text-xs">{result.runtime.version}</span> },
    { term: "Architecture", value: <span className="font-mono text-xs">{result.runtime.arch}</span> },
  ];
  if (result.runtime.feature_compatibility) items.push({ term: "Feature compatibility", value: <span className="font-mono text-xs">{result.runtime.feature_compatibility}</span> });
  if (engine === "postgresql") {
    items.push({ term: "Installed extensions", value: installed.length ? <span className="font-mono text-xs break-words">{installed.join(", ")}</span> : <span className="text-fg-muted">None</span> });
    if (managed) items.push({ term: "Managed by Cove", value: managed.length ? <span className="font-mono text-xs break-words">{managed.join(", ")}</span> : <span className="text-fg-muted">None</span> });
  }
  return <DefinitionList compact items={items} />;
}

// Inspection uses SSH on the worker, so it only runs on this explicit action, never on load or polling.
export function RuntimeInspection({ workloadId, details, disabled, preflight }: { workloadId: string; details: DatabaseDetails; disabled: string | null; preflight: ReturnType<typeof usePreflight> }) {
  const client = useQueryClient();
  const current = preflight.data?.revision === details.revision ? preflight.data : undefined;
  const inspect = () =>
    preflight.mutate(
      { revision: details.revision },
      {
        onError: (error) => {
          if (isStatus(error, 409)) void client.invalidateQueries({ queryKey: databaseKeys.details(workloadId) });
        },
      },
    );
  return (
    <Subsection
      title="Runtime inspection"
      description="Reads the running engine's version, architecture and installed extensions. Read-only; it changes nothing."
      actions={
        <Button disabled={disabled !== null} pending={preflight.isPending} onClick={inspect}>
          {current ? "Inspect again" : "Inspect runtime"}
        </Button>
      }
    >
      {preflight.isError && (
        <Notice tone="error" title="Could not inspect the runtime">
          {errorMessage(preflight.error)}
        </Notice>
      )}
      {current && <RuntimeFacts engine={details.database.engine} result={current} managed={details.database.extensions ?? []} />}
      {current && details.database.engine === "postgresql" && <p className="text-xs text-fg-muted">Managed extensions are those changed through Cove. The installed list comes from the running database and may include others.</p>}
    </Subsection>
  );
}

export function CopyDialog({ workload: liveWorkload, details: liveDetails, onClose }: { workload: Workload; details: DatabaseDetails; onClose: () => void }) {
  const workload = useReviewed(liveWorkload);
  const details = useReviewed(liveDetails);
  const start = useStartMaintenance(workload.id);
  return (
    <Dialog open title="Create an upgrade copy?" onClose={onClose}>
      {start.data ? (
        <div className="mt-3 space-y-4">
          <Notice tone="success" title="Copy queued">
            The copy runs as an operation on the new resource. It is not ready until the operation succeeds. The source is unchanged and keeps serving clients.
          </Notice>
          <div className="flex flex-wrap items-center gap-3">
            <Link to="/operations/$operationId" params={{ operationId: start.data.id }} className={buttonClass("primary")} onClick={onClose}>
              View operation
            </Link>
            <Link to="/resources/$resourceId/database" params={{ resourceId: start.data.resource_id }} className={buttonClass("secondary")} onClick={onClose}>
              Open copy
            </Link>
            <Button variant="ghost" onClick={onClose}>
              Close
            </Button>
          </div>
        </div>
      ) : (
        <CopyForm workload={workload} details={details} start={start} onClose={onClose} />
      )}
    </Dialog>
  );
}

function CopyForm({ workload, details, start, onClose }: { workload: Workload; details: DatabaseDetails; start: ReturnType<typeof useStartMaintenance>; onClose: () => void }) {
  const id = useId();
  const client = useQueryClient();
  const online = useOnline();
  const preflight = usePreflight(workload.id);
  const targets = details.copy_targets ?? [];
  const { engine, version } = details.database;
  const [target, setTarget] = useState(targets[0]?.version ?? "");
  const [name, setName] = useState(() => `${workload.name} ${targets[0]?.version.split(/[.-]/)[0] ?? "copy"}`.slice(0, 120));
  const [confirmed, setConfirmed] = useState(false);
  const [writesStopped, setWritesStopped] = useState(false);
  const selected = targets.find((item) => item.version === target);
  const checked = preflight.data && preflight.variables?.version === target ? preflight.data : undefined;
  const trimmed = name.trim();
  const ready = online && Boolean(selected) && Boolean(checked) && confirmed && writesStopped && trimmed.length > 0 && trimmed.length <= 120;
  const reload = (error: unknown) => {
    if (isStatus(error, 409)) void client.invalidateQueries({ queryKey: databaseKeys.details(workload.id) });
  };

  function choose(value: string) {
    setTarget(value);
    preflight.reset();
  }

  function submit(event: FormEvent) {
    event.preventDefault();
    if (!ready || !selected) return;
    start.mutate({ kind: "upgrade", revision: details.revision, confirm: true, confirm_writes_stopped: true, version: selected.version, name: trimmed }, { onError: reload });
  }

  return (
    <form className="mt-3 space-y-4" onSubmit={submit}>
      <p>
        Creates a separate resource on the same node from a checksum-verified logical snapshot of {workload.name} ({engineName(engine)} {version}). The copy gets new storage and credentials, starts private, and keeps the pooler setting. The source is not changed.
      </p>
      <fieldset className="space-y-1.5">
        <legend className="mb-1.5 text-[13px] font-medium text-fg">Target version</legend>
        {targets.map((item) => (
          <label key={item.version} className={`${choiceClass} items-start`}>
            <input type="radio" name={`${id}-target`} className="mt-0.5 accent-fg" checked={target === item.version} onChange={() => choose(item.version)} />
            <span className="min-w-0">
              <span className="font-medium text-fg">{versionLabel(engine, item)}</span>
              <span className="block font-mono text-[11px] break-all text-fg-muted">{item.image}</span>
              {item.warning && <span className="mt-1 block text-xs text-warn">{item.warning}</span>}
            </span>
          </label>
        ))}
      </fieldset>
      <Field id={`${id}-name`} label="Name of the new resource">
        <input id={`${id}-name`} className={inputClass} value={name} maxLength={120} required onChange={(event) => setName(event.target.value)} autoComplete="off" />
      </Field>
      <div className="space-y-2">
        <div className="flex flex-wrap items-center gap-2">
          <Button disabled={!selected || !online} pending={preflight.isPending} onClick={() => preflight.mutate({ revision: details.revision, version: target }, { onError: reload })}>
            Check compatibility
          </Button>
          {checked && (
            <span role="status" className="text-xs text-ok">
              Compatible with {selected ? versionLabel(engine, selected) : target}
            </span>
          )}
        </div>
        {!checked && !preflight.isError && <p className="text-xs text-fg-muted">The running source is inspected before a copy can be created. The control plane checks again when the copy is queued.</p>}
        {preflight.isError && (
          <Notice tone="error" title="Not compatible or not inspectable">
            {errorMessage(preflight.error)}
          </Notice>
        )}
        {checked && (
          <>
            <RuntimeFacts engine={engine} result={checked} />
            <Notice tone="warn" title="Scope of the copy">
              {checked.scope}
            </Notice>
          </>
        )}
      </div>
      <label className="flex items-start gap-2.5 text-[13px] text-fg">
        <Checkbox checked={confirmed} onChange={(event) => setConfirmed(event.target.checked)} />
        <span>Create a one-time snapshot copy. Writes after the snapshot are not copied, and consumers keep using the source until a cut-over.</span>
      </label>
      <label className="flex items-start gap-2.5 text-[13px] text-fg">
        <Checkbox checked={writesStopped} onChange={(event) => setWritesStopped(event.target.checked)} />
        <span>Applications and any external writers to {workload.name} are stopped now, and will stay stopped through validation and cut-over. Cove cannot detect writers outside it, and a snapshot taken during writes may be inconsistent.</span>
      </label>
      {start.isError && (
        <Notice tone="error" title="Could not create the copy">
          {errorMessage(start.error)}
          {isStatus(start.error, 409) ? " The current details have been reloaded; close this dialog and review them before trying again." : ""}
          {isStatus(start.error, 400) ? " Nothing was copied and no snapshot was taken." : ""}
        </Notice>
      )}
      <div className="flex justify-end gap-2 border-t border-line pt-3">
        <Button variant="ghost" onClick={onClose} disabled={start.isPending}>
          Cancel
        </Button>
        <Button type="submit" variant="primary" disabled={!ready} pending={start.isPending}>
          Create copy
        </Button>
      </div>
    </form>
  );
}

// Discarding needs the failed copy's current name, which is only known on its own page or through its detail.
export function DiscardDialog({ workload: liveWorkload, task: liveTask, onClose }: { workload: Workload; task: MaintenanceTask; onClose: () => void }) {
  const workload = useReviewed(liveWorkload);
  const task = useReviewed(liveTask);
  const own = task.resource_id === workload.id;
  const target = useQuery({ ...workloadQuery(task.resource_id), enabled: !own });
  const targetName = useFirstLoaded(own ? workload.name : target.data?.name);
  const discard = useDiscardCopy(workload.id);
  const online = useOnline();
  const id = useId();
  const [typed, setTyped] = useState("");
  const ready = online && targetName !== undefined && typed === targetName;

  function submit(event: FormEvent) {
    event.preventDefault();
    if (!ready) return;
    discard.mutate({ task: task.id, name: typed });
  }

  return (
    <Dialog open title="Discard the failed copy?" onClose={onClose}>
      {discard.isSuccess ? (
        <div className="mt-3 space-y-4">
          <Notice tone="success" title="Failed copy discarded">
            Its data and helpers were removed. The stopped resource record remains and can be deleted from its Settings page. The source is unchanged.
          </Notice>
          <div className="flex justify-end">
            <Button onClick={onClose}>Close</Button>
          </div>
        </div>
      ) : (
        <form className="mt-3 space-y-4" onSubmit={submit}>
          <p>Permanently removes the data and helpers of the failed copy. The source database is retained and unchanged. Rotations and extension changes cannot be discarded.</p>
          {!own && target.isPending && <p className="text-xs text-fg-muted">Reading the copy's name</p>}
          {!own && target.isError && (
            <Notice tone="error" title="Could not read the copy">
              {errorMessage(target.error)}
            </Notice>
          )}
          {targetName !== undefined && (
            <Field id={`${id}-name`} label={`Type ${targetName} to confirm`}>
              <input id={`${id}-name`} className={inputClass} value={typed} onChange={(event) => setTyped(event.target.value)} autoComplete="off" spellCheck={false} />
            </Field>
          )}
          {discard.isError && (
            <Notice tone="error" title="Could not discard the copy">
              {errorMessage(discard.error)}
              {isStatus(discard.error, 502) ? " Retry the same action; the worker did not confirm cleanup." : ""}
            </Notice>
          )}
          <div className="flex justify-end gap-2 border-t border-line pt-3">
            <Button variant="ghost" onClick={onClose} disabled={discard.isPending}>
              Cancel
            </Button>
            <Button type="submit" variant="danger" disabled={!ready} pending={discard.isPending}>
              Discard copy
            </Button>
          </div>
        </form>
      )}
    </Dialog>
  );
}

// Cut-over is checked against the source's current revision, which this page only has when it is the source.
export function CutoverDialog({ workload: liveWorkload, details: liveDetails, task: liveTask, onClose }: { workload: Workload; details: DatabaseDetails; task: MaintenanceTask; onClose: () => void }) {
  const workload = useReviewed(liveWorkload);
  const details = useReviewed(liveDetails);
  const task = useReviewed(liveTask);
  const isSource = task.source_id === workload.id;
  const source = useQuery({ ...databaseDetailsQuery(task.source_id), enabled: !isSource });
  const revision = useFirstLoaded(isSource ? details.revision : source.data?.revision);
  const cutover = useCutover(workload.id);
  const client = useQueryClient();
  const online = useOnline();
  const [stopped, setStopped] = useState(false);
  const [fresh, setFresh] = useState(false);
  const ready = online && revision !== undefined && stopped && fresh;

  function submit(event: FormEvent) {
    event.preventDefault();
    if (!ready) return;
    cutover.mutate(
      { task: task.id, revision },
      {
        onError: (error) => {
          if (isStatus(error, 409)) {
            void client.invalidateQueries({ queryKey: databaseKeys.details(task.source_id) });
            void client.invalidateQueries({ queryKey: databaseKeys.details(task.resource_id) });
          }
        },
      },
    );
  }

  return (
    <Dialog open title="Cut over to the copy?" onClose={onClose}>
      {cutover.data ? (
        <div className="mt-3 space-y-4">
          <Notice tone="success" title="Cut-over recorded">
            Variable bindings and private connections now point at the copy. The old source is retained and still running. Deploy each bound consumer so it resolves the copy's credentials; Cove has not redeployed them.
          </Notice>
          <div className="flex flex-wrap items-center gap-3">
            <Link to="/resources/$resourceId/database" params={{ resourceId: task.resource_id }} className={buttonClass("primary")} onClick={onClose}>
              Open copy and its bound resources
            </Link>
            <Button variant="ghost" onClick={onClose}>
              Close
            </Button>
          </div>
        </div>
      ) : (
        <form className="mt-3 space-y-4" onSubmit={submit}>
          <p>Switches every variable binding and canonical private connection from the source to the verified copy in one step. Consumers keep their current values until they are deployed again. The source is not deleted or stopped.</p>
          <Notice tone="warn" title="The copy is a snapshot, not a sync">
            Writes made to the source after the snapshot are not in the copy. Stop application writes and review the copy's data before switching. Cluster accounts, grants and other databases were not copied.
          </Notice>
          {!isSource && source.isPending && <p className="text-xs text-fg-muted">Reading the source's current configuration</p>}
          {!isSource && source.isError && (
            <Notice tone="error" title="Could not read the source">
              {errorMessage(source.error)}
            </Notice>
          )}
          <label className="flex items-start gap-2.5 text-[13px] text-fg">
            <Checkbox checked={stopped} onChange={(event) => setStopped(event.target.checked)} />
            <span>Application writes to the source have stopped.</span>
          </label>
          <label className="flex items-start gap-2.5 text-[13px] text-fg">
            <Checkbox checked={fresh} onChange={(event) => setFresh(event.target.checked)} />
            <span>I reviewed the copy and its data is current enough to switch.</span>
          </label>
          {cutover.isError && (
            <Notice tone="error" title="Could not cut over">
              {errorMessage(cutover.error)}
            </Notice>
          )}
          <div className="flex justify-end gap-2 border-t border-line pt-3">
            <Button variant="ghost" onClick={onClose} disabled={cutover.isPending}>
              Cancel
            </Button>
            <Button type="submit" variant="danger" disabled={!ready} pending={cutover.isPending}>
              Switch to copy
            </Button>
          </div>
        </form>
      )}
    </Dialog>
  );
}

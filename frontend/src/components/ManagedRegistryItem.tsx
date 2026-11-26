import { Link } from "@tanstack/react-router";
import { useState, type ReactNode } from "react";
import { errorMessage } from "../api/client";
import { inventoryFailure, inventoryRefusal, useDeleteManagedRegistryRecord, useManagedRegistryOperation, useSaveManagedRegistry, type ManagedRegistry, type ManagedRegistryOperation } from "../api/managed-registries";
import type { OperationState } from "../api/types";
import { editable, operationActive, recordDeletable, registryState, registryWorking, retentionSummary } from "../lib/managed-registries";
import { operationTones } from "../lib/operations";
import { linkClass } from "../lib/styles";
import { ManagedRegistryCertificate } from "./ManagedRegistryCertificate";
import { DeleteDataDialog, DeleteRecordDialog, GarbageCollectionDialog, InventoryFailureDetails, RemoveDialog } from "./ManagedRegistryDialogs";
import { RetentionForm } from "./ManagedRegistryRetention";
import { OperationStateLabel, StatusLabel } from "./status";
import { Button, DefinitionList, Notice, Subsection } from "./ui";

interface Props {
  registry: ManagedRegistry;
  canManage: boolean;
  blocked?: string;
  nodeName?: string;
  projectName?: string;
}

type DialogKind = "gc" | "remove" | "delete-data" | "delete-record";

const outcomes: Record<ManagedRegistryOperation["kind"], string> = {
  gc: "Garbage collection requested",
  retry: "Provisioning retry requested",
  remove: "Removal requested",
};

// Garbage collection and data deletion keep their confirmation open until the API answers. These are the refusals
// that confirmation explains itself, with the typed hostname kept.
function explainedInDialog(operation: ManagedRegistryOperation | undefined, error: unknown) {
  if (operation?.kind === "gc") return inventoryFailure(error) !== null;
  if (operation?.kind === "remove" && operation.deleteData) return inventoryRefusal(error) !== null || inventoryFailure(error) !== null;
  return false;
}

// Keyed by registry id in the list, so its mutations and outcome survive refetches that replace the forms below.
export function ManagedRegistryItem({ registry, canManage, blocked, nodeName, projectName }: Props) {
  const operation = useManagedRegistryOperation(registry.id);
  const retention = useSaveManagedRegistry(registry.id);
  const certificate = useSaveManagedRegistry(registry.id);
  const record = useDeleteManagedRegistryRecord(registry.id);
  const [outcome, setOutcome] = useState<string | null>(null);
  const [dialog, setDialog] = useState<DialogKind | null>(null);
  const state = registryState(registry.state);
  const actionBlocked = blocked ?? (operationActive(registry) ? "An operation is in progress." : undefined);

  // Other refusals close the confirmation and are shown on the registry as before.
  function run(next: ManagedRegistryOperation) {
    const keepsOpen = next.kind === "gc" || (next.kind === "remove" && next.deleteData);
    if (!keepsOpen) setDialog(null);
    setOutcome(null);
    operation.mutate(next, {
      onSuccess: () => {
        if (keepsOpen) setDialog(null);
        setOutcome(outcomes[next.kind]);
      },
      onError: (error) => {
        if (keepsOpen && !explainedInDialog(next, error)) setDialog(null);
      },
    });
  }

  const explained = explainedInDialog(operation.variables, operation.error);

  // A refusal that only the confirmation explains is not repeated on the registry once that confirmation is closed.
  // One that arrives after the confirmation was closed is shown on the registry with what it names.
  function cancelDialog() {
    setDialog(null);
    if (explained) operation.reset();
  }

  // A new confirmation reads the nodes afresh, so an earlier refusal no longer applies to it.
  function openDialog(next: DialogKind) {
    if (explained) operation.reset();
    setDialog(next);
  }

  // On success the registry leaves the list and this item unmounts, so only a refusal needs to be shown.
  function deleteRecord() {
    setOutcome(null);
    record.mutate(undefined, { onSettled: () => setDialog(null) });
  }

  return (
    <div id={`managed-registry-${registry.id}`} className="divide-y divide-line">
      <Subsection title={registry.name || "Unnamed registry"} actions={<StatusLabel tone={state.tone} label={state.label} pulse={registryWorking(registry)} />}>
        <RegistryDetails registry={registry} nodeName={nodeName} projectName={projectName} />
        <div className="space-y-2 empty:hidden" aria-live="polite">
          {outcome && <Notice tone="success" title={outcome} />}
          <RefusalNotice title="The registry operation was not requested" error={dialog && explained ? null : operation.error} />
          <RefusalNotice title="The registry record was not deleted" error={record.error} />
          <RegistryNotices registry={registry} canManage={canManage} />
        </div>
      </Subsection>
      {canManage && registry.state === "failed" && <ManagedRegistryCertificate registry={registry} save={certificate} blocked={actionBlocked} onOutcome={setOutcome} />}
      {canManage && editable(registry) && <RetentionForm key={JSON.stringify(registry.retention)} registryId={registry.id} retention={registry.retention} save={retention} blocked={actionBlocked} onOutcome={setOutcome} />}
      {canManage && <RegistryActions registry={registry} blocked={actionBlocked} pending={operation.isPending ? operation.variables.kind : record.isPending ? "delete-record" : null} onRetry={() => run({ kind: "retry" })} onOpen={openDialog} />}
      <RegistryDialog dialog={dialog} registry={registry} recordPending={record.isPending} operationPending={operation.isPending} operationError={operation.error} onRun={run} onDeleteRecord={deleteRecord} onCancel={cancelDialog} />
    </div>
  );
}

interface DialogProps {
  dialog: DialogKind | null;
  registry: ManagedRegistry;
  recordPending: boolean;
  operationPending: boolean;
  operationError: Error | null;
  onRun: (operation: ManagedRegistryOperation) => void;
  onDeleteRecord: () => void;
  onCancel: () => void;
}

function RegistryDialog({ dialog, registry, recordPending, operationPending, operationError, onRun, onDeleteRecord, onCancel }: DialogProps) {
  switch (dialog) {
    case "gc":
      return <GarbageCollectionDialog registry={registry} pending={operationPending} error={operationError} onConfirm={onRun} onCancel={onCancel} />;
    case "remove":
      return <RemoveDialog registry={registry} pending={operationPending} error={operationError} onConfirm={onRun} onCancel={onCancel} />;
    case "delete-data":
      return <DeleteDataDialog registry={registry} pending={operationPending} error={operationError} onConfirm={onRun} onCancel={onCancel} />;
    case "delete-record":
      return <DeleteRecordDialog registry={registry} pending={recordPending} onConfirm={onDeleteRecord} onCancel={onCancel} />;
    case null:
      return null;
  }
}

function RefusalNotice({ title, error }: { title: string; error: Error | null }) {
  if (!error) return null;
  const nodes = inventoryRefusal(error);
  if (nodes) {
    return (
      <Notice tone="error" title="Data deletion was not requested">
        <p>The unavailable nodes changed before Cove accepted the request, so nothing was changed. Deleting the data needs a new confirmation that reviews the nodes listed now.</p>
        {nodes.length === 0 ? (
          <p>No nodes are unavailable now.</p>
        ) : (
          <ul aria-label="Nodes unavailable now" className="mt-1 list-disc pl-5">
            {nodes.map((node) => (
              <li key={node.id} className="break-words text-fg">
                {node.name}
              </li>
            ))}
          </ul>
        )}
      </Notice>
    );
  }
  const failure = inventoryFailure(error);
  if (failure) {
    return (
      <Notice tone="error" title={title}>
        <p>Cove could not check every reference to this registry, so nothing was queued. A ready node could not list what it keeps that uses this registry.</p>
        <InventoryFailureDetails failure={failure} />
      </Notice>
    );
  }
  return (
    <Notice tone="error" title={title}>
      <p className="break-words">{errorMessage(error)}</p>
    </Notice>
  );
}

const muted = (text: string) => <span className="text-fg-muted">{text}</span>;
const mono = (text: string) => <span className="font-mono text-xs break-all">{text}</span>;

function isOperationState(state: string): state is OperationState {
  return state in operationTones;
}

function LatestOperation({ registry }: { registry: ManagedRegistry }) {
  if (!registry.operation_id) return muted("None");
  return (
    <span className="flex flex-wrap items-center gap-x-3 gap-y-1">
      {isOperationState(registry.operation_state) ? <OperationStateLabel state={registry.operation_state} /> : muted(registry.operation_state ? `Unknown state (${registry.operation_state})` : "State not reported")}
      <Link to="/operations/$operationId" params={{ operationId: registry.operation_id }} className={linkClass}>
        View operation
      </Link>
    </span>
  );
}

function StorageValue({ registry }: { registry: ManagedRegistry }) {
  return (
    <span className="block space-y-1">
      {registry.storage.hard_cap_verified ? <StatusLabel tone="neutral" label="Reported as verified by the panel" /> : <StatusLabel tone="warn" label="No verified hard cap" />}
      {registry.storage.reason && <span className="block text-xs text-fg-muted">{registry.storage.reason}</span>}
    </span>
  );
}

function connectionValue(registry: ManagedRegistry) {
  if (registry.connection_id) return "Generated team registry connection, listed with the registry connections on this page";
  if (registry.state === "removed") return muted("Removed");
  return muted("Created only after Cove verifies authenticated HTTPS");
}

function RetentionValue({ registry }: { registry: ManagedRegistry }) {
  const summary = retentionSummary(registry.retention);
  return summary ? `${summary.charAt(0).toUpperCase()}${summary.slice(1)}` : muted("Not reported");
}

function RegistryDetails({ registry, nodeName, projectName }: { registry: ManagedRegistry; nodeName?: string; projectName?: string }) {
  const placement = [
    { term: "Hostname", value: mono(registry.hostname) },
    { term: "Node", value: nodeName ?? mono(registry.node_id) },
    { term: "Project", value: projectName ?? mono(registry.project_id) },
  ];
  if (registry.state === "removed") {
    return <DefinitionList items={[...placement, { term: "Data", value: registry.data_retained ? "Data volume retained on the node" : muted("No retained data reported") }, { term: "Latest operation", value: <LatestOperation registry={registry} /> }]} />;
  }
  return (
    <DefinitionList
      items={[
        ...placement,
        { term: "Image", value: <span className="block space-y-0.5">{mono(registry.image || "Not reported")}<span className="block text-xs text-fg-muted">Fixed by this Cove release</span></span> },
        { term: "Retention", value: <RetentionValue registry={registry} /> },
        { term: "Storage cap", value: <StorageValue registry={registry} /> },
        { term: "Registry connection", value: connectionValue(registry) },
        { term: "Latest operation", value: <LatestOperation registry={registry} /> },
      ]}
    />
  );
}

const workingText: Record<string, string> = {
  provisioning: "The provisioning deploy is queued or running. A queued deploy may be waiting for approval, a deployment window or the worker.",
  maintenance: "Garbage collection is queued or running. A queued run may be waiting for approval, a deployment window or the worker. While it runs, the registry is stopped and Cove builds and deploys that use it are refused.",
  removing: "Removal is queued or running. A queued removal may be waiting for approval, a deployment window or the worker. Cove then removes the registry service, its route and its generated connection.",
};

function RegistryNotices({ registry, canManage }: { registry: ManagedRegistry; canManage: boolean }) {
  const notices: ReactNode[] = [];
  if (registryWorking(registry)) {
    notices.push(
      <Notice key="working" tone="info" title="An operation is in progress">
        {workingText[registry.state] ?? "Follow it from the latest operation above."}
      </Notice>,
    );
  }
  if (registry.state === "failed") {
    notices.push(
      <Notice key="failed" tone="error" title="Provisioning failed">
        <p className="break-words">{registry.last_error || "Provisioning did not complete."}</p>
        <p className="mt-1">The registry is not used by builds or deploys until it is repaired and authenticated HTTPS works.{canManage ? "" : " A team administrator can repair it."}</p>
      </Notice>,
    );
  } else if (registry.last_error) {
    notices.push(
      <Notice key="error" tone="warn" title="The last registry operation did not finish">
        <p className="break-words">{registry.last_error}</p>
        <p className="mt-1">Manifests that garbage collection deleted before it stopped are not restored.</p>
      </Notice>,
    );
  }
  if (registry.state === "removed" && registry.data_retained) {
    notices.push(
      <Notice key="retained" tone="info" title="Registry data retained">
        The owned data volume is still on the node. The registry is not running and has no connection.{canManage ? " Delete the data once no retained release, latest build or preview references it." : ""}
      </Notice>,
    );
  }
  return notices;
}

const actionHints: Record<string, string> = {
  failed: "Retry repeats the saved configuration and credentials, then checks authenticated HTTPS again.",
  ready: "Garbage collection runs only when you start it.",
};

interface ActionsProps {
  registry: ManagedRegistry;
  blocked?: string;
  pending: ManagedRegistryOperation["kind"] | "delete-record" | null;
  onRetry: () => void;
  onOpen: (dialog: DialogKind) => void;
}

function RegistryActions({ registry, blocked, pending, onRetry, onOpen }: ActionsProps) {
  const disabled = Boolean(blocked) || pending !== null;
  let buttons: ReactNode;
  if (registry.state === "ready") {
    buttons = (
      <>
        <Button onClick={() => onOpen("gc")} disabled={disabled} pending={pending === "gc"}>
          Run garbage collection
        </Button>
        <Button variant="danger" onClick={() => onOpen("remove")} disabled={disabled} pending={pending === "remove"}>
          Remove registry
        </Button>
      </>
    );
  } else if (registry.state === "failed") {
    buttons = (
      <>
        <Button variant="danger" onClick={() => onOpen("remove")} disabled={disabled} pending={pending === "remove"}>
          Remove registry
        </Button>
        <Button variant="primary" onClick={onRetry} disabled={disabled} pending={pending === "retry"}>
          Retry provisioning
        </Button>
      </>
    );
  } else if (registry.state === "removed" && registry.data_retained) {
    buttons = (
      <Button variant="danger" onClick={() => onOpen("delete-data")} disabled={disabled} pending={pending === "remove"}>
        Delete retained data
      </Button>
    );
  } else if (recordDeletable(registry)) {
    buttons = (
      <Button onClick={() => onOpen("delete-record")} disabled={disabled} pending={pending === "delete-record"}>
        Delete registry record
      </Button>
    );
  } else {
    return null;
  }
  return (
    <div className="flex flex-wrap items-center justify-end gap-x-3 gap-y-2 p-4">
      <p className="mr-auto text-xs text-fg-muted">{blocked ? `${blocked} Registry actions are paused.` : actionHints[registry.state] ?? ""}</p>
      {buttons}
    </div>
  );
}

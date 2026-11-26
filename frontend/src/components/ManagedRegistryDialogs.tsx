import { useQuery } from "@tanstack/react-query";
import { useId, useState, type FormEvent, type ReactNode } from "react";
import { inventoryFailure, inventoryRefusal, removalInventoryQuery, type ManagedRegistry, type ManagedRegistryInventoryFailure, type ManagedRegistryNode, type ManagedRegistryOperation } from "../api/managed-registries";
import { retentionSummary } from "../lib/managed-registries";
import { choiceClass, inputClass } from "../lib/styles";
import { Checkbox } from "./Checkbox";
import { ConfirmDialog, Dialog } from "./ConfirmDialog";
import { Button, DefinitionList, Field, Notice, QueryError, Spinner } from "./ui";

interface Props {
  registry: ManagedRegistry;
  onConfirm: (operation: ManagedRegistryOperation) => void;
  onCancel: () => void;
}

// Confirmations that stay open while the request runs, so a refusal can be shown with the draft kept. Cancel stays
// available; a refusal that arrives afterwards is shown on the registry.
interface PendingProps extends Props {
  pending: boolean;
  error: Error | null;
}

function keptByRetention(registry: ManagedRegistry) {
  const summary = retentionSummary(registry.retention);
  return summary ? `the ${summary}` : "the manifests the saved retention settings keep";
}

export function GarbageCollectionDialog({ registry, pending, error, onConfirm, onCancel }: PendingProps) {
  return (
    <Dialog
      open
      title={`Run garbage collection on ${registry.name}?`}
      onClose={onCancel}
      footer={
        <>
          <Button variant="ghost" onClick={onCancel}>
            Cancel
          </Button>
          <Button variant="primary" onClick={() => onConfirm({ kind: "gc" })} pending={pending}>
            Run garbage collection
          </Button>
        </>
      }
    >
      <div className="space-y-2">
        <InventoryFailureNotice error={error} action="Garbage collection" />
        <p>The registry is stopped while this runs, and Cove builds and deploys that use it are refused until it finishes. Running application containers are not stopped or deleted.</p>
        <p>
          <span className="font-medium text-fg">Kept:</span> images referenced by retained releases, previews and latest builds; tags that Cove builds did not create; and {keptByRetention(registry)}.
        </p>
        <p>
          <span className="font-medium text-fg">Deleted:</span> every other Cove build manifest. Storage that no kept manifest references is then reclaimed. Cove does not measure how much space this frees.
        </p>
        <p>If it is interrupted or cancelled, manifests already deleted are not restored and cleanup can stay incomplete until the worker recovers the registry. Cove refuses to start while any team operation is queued or running, or when it cannot list every reference, including while a node that may hold references is unavailable. Nodes that never enrolled hold none. This is manual maintenance; Cove never runs it on a schedule.</p>
        <p>The project&apos;s approval, freeze and deployment window rules apply, so the operation can wait in the queue before it runs.</p>
      </div>
    </Dialog>
  );
}

export function InventoryFailureDetails({ failure }: { failure: ManagedRegistryInventoryFailure }) {
  return (
    <div className="mt-1 space-y-2">
      <DefinitionList
        compact
        items={[
          { term: "Resource", value: failure.resource.name },
          { term: "Node", value: failure.node.name },
        ]}
      />
      <p className="break-words">{failure.reason}</p>
    </div>
  );
}

// Shown in the open confirmation. Nothing was queued and Cove does not retry, so confirming again is a new request.
function InventoryFailureNotice({ error, action, keepDataHint = false }: { error: Error | null; action: string; keepDataHint?: boolean }) {
  const failure = inventoryFailure(error);
  if (!failure) return null;
  return (
    <Notice tone="error" title="Cove could not check every reference to this registry">
      <p>{action} was not requested and nothing was queued. A ready node could not list what it keeps that uses this registry.</p>
      <InventoryFailureDetails failure={failure} />
      <p className="mt-2">Confirm again once the node can be checked, or cancel.{keepDataHint ? " You can still remove the registry and keep its data." : ""}</p>
    </Notice>
  );
}

interface TypedConfirmationProps {
  registry: ManagedRegistry;
  title: string;
  confirmLabel: string;
  children: ReactNode;
  onSubmit: () => void;
  onCancel: () => void;
  requireTyping: boolean;
  // Set while something else must happen first, such as acknowledging unavailable nodes.
  blocked?: boolean;
  pending?: boolean;
}

function TypedConfirmation({ registry, title, confirmLabel, children, onSubmit, onCancel, requireTyping, blocked = false, pending = false }: TypedConfirmationProps) {
  const id = useId();
  const [typed, setTyped] = useState("");
  const ready = !blocked && !pending && (!requireTyping || typed === registry.hostname);
  function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    if (ready) onSubmit();
  }
  return (
    <Dialog open title={title} onClose={onCancel}>
      <form onSubmit={submit} className="space-y-4">
        {children}
        {requireTyping && (
          <Field id={id} label={`Type ${registry.hostname} to confirm`}>
            <input id={id} className={`${inputClass} font-mono`} value={typed} onChange={(event) => setTyped(event.target.value)} autoComplete="off" spellCheck={false} autoCapitalize="none" />
          </Field>
        )}
        <div className="flex flex-wrap justify-end gap-2">
          <Button variant="ghost" onClick={onCancel}>
            Cancel
          </Button>
          <Button type="submit" variant="danger" disabled={!ready} pending={pending}>
            {confirmLabel}
          </Button>
        </div>
      </form>
    </Dialog>
  );
}

export function RemoveDialog({ registry, pending, error, onConfirm, onCancel }: PendingProps) {
  const id = useId();
  const [deleteData, setDeleteData] = useState(false);
  const inventory = useUnavailableNodes(registry.id, deleteData);
  function submit() {
    onConfirm(deleteData ? { kind: "remove", deleteData, acknowledgeUnavailableNodes: inventory.acknowledged } : { kind: "remove", deleteData });
  }
  function toggleDeleteData(checked: boolean) {
    setDeleteData(checked);
    if (!checked) inventory.clear();
  }
  return (
    <TypedConfirmation registry={registry} title={`Remove ${registry.name}?`} confirmLabel={deleteData ? "Remove and delete data" : "Remove registry"} requireTyping={deleteData} blocked={deleteData && !inventory.ready} pending={pending} onCancel={onCancel} onSubmit={submit}>
      {deleteData && <InventoryFailureNotice error={error} action="Data deletion" keepDataHint />}
      <div className="space-y-2">
        <p>This removes the registry service, its HTTPS route and the generated registry connection. Disconnect resources and registry webhooks that use it first; Cove refuses otherwise. Certificates are not revoked and nothing outside the node is cleaned up.</p>
        <p>By default the registry&apos;s data volume is kept on the node, where it can be deleted later.</p>
        <p>The project&apos;s approval, freeze and deployment window rules apply, so the operation can wait in the queue before it runs.</p>
      </div>
      <label htmlFor={`${id}-delete`} className={choiceClass}>
        <Checkbox id={`${id}-delete`} checked={deleteData} onChange={(event) => toggleDeleteData(event.target.checked)} />
        Also delete the registry data permanently
      </label>
      {deleteData && (
        <Notice tone="warn" title="Every image in this registry will be deleted">
          This cannot be undone. Cove refuses while retained releases, latest builds or previews still reference the registry.
        </Notice>
      )}
      {deleteData && <UnavailableNodes inventory={inventory} error={error} keepDataHint />}
    </TypedConfirmation>
  );
}

export function DeleteRecordDialog({ registry, pending, onConfirm, onCancel }: { registry: ManagedRegistry; pending: boolean; onConfirm: () => void; onCancel: () => void }) {
  return (
    <ConfirmDialog open danger title={`Delete the record of ${registry.name}?`} confirmLabel="Delete registry record" pending={pending} onCancel={onCancel} onConfirm={onConfirm}>
      <div className="space-y-2">
        <p>This deletes the empty registry resource and its operation history from Cove. Its data was already deleted, so no images, volumes or running services are affected.</p>
        <p>This cannot be undone.</p>
      </div>
    </ConfirmDialog>
  );
}

export function DeleteDataDialog({ registry, pending, error, onConfirm, onCancel }: PendingProps) {
  const inventory = useUnavailableNodes(registry.id, true);
  return (
    <TypedConfirmation
      registry={registry}
      title={`Delete the retained data of ${registry.name}?`}
      confirmLabel="Delete data permanently"
      requireTyping
      blocked={!inventory.ready}
      pending={pending}
      onCancel={onCancel}
      onSubmit={() => onConfirm({ kind: "remove", deleteData: true, acknowledgeUnavailableNodes: inventory.acknowledged })}
    >
      <InventoryFailureNotice error={error} action="Data deletion" />
      <p>This permanently deletes the registry&apos;s data volume on the node, including every image in it. It cannot be undone. Cove refuses while retained releases, latest builds or previews still reference the registry.</p>
      <p>The project&apos;s approval, freeze and deployment window rules apply, so the operation can wait in the queue before it runs.</p>
      <UnavailableNodes inventory={inventory} error={error} />
    </TypedConfirmation>
  );
}

const nodeSignature = (nodes: ManagedRegistryNode[] | undefined) => (nodes ? nodes.map((node) => node.id).sort().join("\n") : null);

// The acknowledgement holds the exact node IDs listed when it was checked. Any change to the listed set clears it.
function useUnavailableNodes(registryId: string, enabled: boolean) {
  const query = useQuery({ ...removalInventoryQuery(registryId), enabled });
  const nodes = query.data?.unavailable_nodes;
  const signature = nodeSignature(nodes);
  const [acknowledged, setAcknowledged] = useState<string[] | undefined>();
  const [seen, setSeen] = useState(signature);
  if (seen !== signature) {
    setSeen(signature);
    setAcknowledged(undefined);
  }
  // A failed refresh keeps the last list in the cache, but it must not unlock deletion.
  const loaded = enabled && query.status === "success" && nodes !== undefined;
  return {
    query,
    nodes,
    acknowledged,
    ready: loaded && (nodes.length === 0 || acknowledged !== undefined),
    acknowledge: (checked: boolean) => setAcknowledged(checked && nodes ? nodes.map((node) => node.id) : undefined),
    clear: () => setAcknowledged(undefined),
  };
}

type Inventory = ReturnType<typeof useUnavailableNodes>;

function UnavailableNodes({ inventory, error, keepDataHint = false }: { inventory: Inventory; error: Error | null; keepDataHint?: boolean }) {
  const { query, nodes } = inventory;
  const refusal = inventoryRefusal(error) !== null && (
    <Notice tone="error" title="The unavailable nodes changed">
      Data deletion was not requested. Review the nodes listed now and confirm again.
    </Notice>
  );
  if (query.status === "error") {
    return (
      <div className="space-y-2">
        {refusal}
        <QueryError what="the nodes Cove could not check" error={query.error} onRetry={() => void query.refetch()} retrying={query.isFetching} />
        <p className="text-xs">Data cannot be deleted until this list loads.{keepDataHint ? " You can still remove the registry and keep its data." : ""}</p>
      </div>
    );
  }
  if (!nodes) {
    return (
      <div role="status" className="flex items-start gap-2">
        <Spinner className="mt-0.5 size-3.5 shrink-0" />
        <span>Checking which nodes may hold references to this registry.</span>
      </div>
    );
  }
  if (nodes.length === 0) return refusal || null;
  return (
    <div className="space-y-2">
      {refusal}
      <UnavailableNodeList nodes={nodes} />
      <UnavailableAcknowledgement inventory={inventory} />
    </div>
  );
}

function UnavailableNodeList({ nodes }: { nodes: ManagedRegistryNode[] }) {
  return (
    <Notice tone="warn" title={nodes.length === 1 ? "A node that may hold references is unavailable" : `${nodes.length} nodes that may hold references are unavailable`}>
      <p>Cove could not check these nodes for retained releases or builds that use this registry. Deleting the data may make them unrecoverable. References Cove can see still block deletion.</p>
      <ul aria-label="Unavailable nodes" className="mt-1 list-disc pl-5">
        {nodes.map((node) => (
          <li key={node.id} className="break-words text-fg">
            {node.name}
          </li>
        ))}
      </ul>
    </Notice>
  );
}

function UnavailableAcknowledgement({ inventory }: { inventory: Inventory }) {
  const id = useId();
  return (
    <label htmlFor={id} className={choiceClass}>
      <Checkbox id={id} checked={inventory.acknowledged !== undefined} onChange={(event) => inventory.acknowledge(event.target.checked)} />
      Delete the data even if releases or builds on these nodes become unrecoverable
    </label>
  );
}

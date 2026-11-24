import { useId, useState, type FormEvent, type ReactNode } from "react";
import type { ManagedRegistry, ManagedRegistryOperation } from "../api/managed-registries";
import { retentionSummary } from "../lib/managed-registries";
import { choiceClass, inputClass } from "../lib/styles";
import { Checkbox } from "./Checkbox";
import { ConfirmDialog, Dialog } from "./ConfirmDialog";
import { Button, Field, Notice } from "./ui";

interface Props {
  registry: ManagedRegistry;
  onConfirm: (operation: ManagedRegistryOperation) => void;
  onCancel: () => void;
}

function keptByRetention(registry: ManagedRegistry) {
  const summary = retentionSummary(registry.retention);
  return summary ? `the ${summary}` : "the manifests the saved retention settings keep";
}

export function GarbageCollectionDialog({ registry, onConfirm, onCancel }: Props) {
  return (
    <ConfirmDialog open title={`Run garbage collection on ${registry.name}?`} confirmLabel="Run garbage collection" onCancel={onCancel} onConfirm={() => onConfirm({ kind: "gc" })}>
      <div className="space-y-2">
        <p>The registry is stopped while this runs, and Cove builds and deploys that use it are refused until it finishes. Running application containers are not stopped or deleted.</p>
        <p>
          <span className="font-medium text-fg">Kept:</span> images referenced by retained releases, previews and latest builds; tags that Cove builds did not create; and {keptByRetention(registry)}.
        </p>
        <p>
          <span className="font-medium text-fg">Deleted:</span> every other Cove build manifest. Storage that no kept manifest references is then reclaimed. Cove does not measure how much space this frees.
        </p>
        <p>If it is interrupted or cancelled, manifests already deleted are not restored and cleanup can stay incomplete until the worker recovers the registry. Cove refuses to start while any team operation is queued or running, or when it cannot list every reference. This is manual maintenance; Cove never runs it on a schedule.</p>
      </div>
    </ConfirmDialog>
  );
}

function TypedConfirmation({ registry, title, confirmLabel, children, onSubmit, onCancel, requireTyping }: { registry: ManagedRegistry; title: string; confirmLabel: string; children: ReactNode; onSubmit: () => void; onCancel: () => void; requireTyping: boolean }) {
  const id = useId();
  const [typed, setTyped] = useState("");
  function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    if (!requireTyping || typed === registry.hostname) onSubmit();
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
          <Button type="submit" variant="danger" disabled={requireTyping && typed !== registry.hostname}>
            {confirmLabel}
          </Button>
        </div>
      </form>
    </Dialog>
  );
}

export function RemoveDialog({ registry, onConfirm, onCancel }: Props) {
  const id = useId();
  const [deleteData, setDeleteData] = useState(false);
  return (
    <TypedConfirmation registry={registry} title={`Remove ${registry.name}?`} confirmLabel={deleteData ? "Remove and delete data" : "Remove registry"} requireTyping={deleteData} onCancel={onCancel} onSubmit={() => onConfirm({ kind: "remove", deleteData })}>
      <div className="space-y-2">
        <p>This removes the registry service, its HTTPS route and the generated registry connection. Disconnect resources and registry webhooks that use it first; Cove refuses otherwise. Certificates are not revoked and nothing outside the node is cleaned up.</p>
        <p>By default the registry&apos;s data volume is kept on the node, where it can be deleted later.</p>
      </div>
      <label htmlFor={`${id}-delete`} className={choiceClass}>
        <Checkbox id={`${id}-delete`} checked={deleteData} onChange={(event) => setDeleteData(event.target.checked)} />
        Also delete the registry data permanently
      </label>
      {deleteData && (
        <Notice tone="warn" title="Every image in this registry will be deleted">
          This cannot be undone. Cove refuses while retained releases, latest builds or previews still reference the registry.
        </Notice>
      )}
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

export function DeleteDataDialog({ registry, onConfirm, onCancel }: Props) {
  return (
    <TypedConfirmation registry={registry} title={`Delete the retained data of ${registry.name}?`} confirmLabel="Delete data permanently" requireTyping onCancel={onCancel} onSubmit={() => onConfirm({ kind: "remove", deleteData: true })}>
      <p>This permanently deletes the registry&apos;s data volume on the node, including every image in it. It cannot be undone. Cove refuses while retained releases, latest builds or previews still reference the registry.</p>
    </TypedConfirmation>
  );
}

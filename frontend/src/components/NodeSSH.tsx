import { useQuery, useQueryClient } from "@tanstack/react-query";
import { KeyRound, RefreshCw, RotateCw, X } from "lucide-react";
import { useState } from "react";
import { bastionsQuery, identityPending, nodeBastionQuery, nodeIdentityQuery, useIdentityAction, useSaveNodeBastion, type Bastion, type IdentityAction, type NodeBastion, type NodeIdentity } from "../api/bastions";
import { ApiError, errorMessage } from "../api/client";
import type { Node } from "../api/types";
import type { useVerifyNode } from "../api/mutations";
import { bastionEndpoint, identityActions, identityStates } from "../lib/bastions";
import { useOnline } from "../lib/hooks";
import { ConfirmDialog } from "./ConfirmDialog";
import { RelativeTime } from "./RelativeTime";
import { Select } from "./Select";
import { StatusLabel } from "./status";
import { Button, Field, LoadingRows, Notice, QueryError, Section, Subsection } from "./ui";

type VerifyMutation = ReturnType<typeof useVerifyNode>;

const title = "SSH connection";

// Client and server errors are definite answers; anything else may have been applied before the
// response was lost.
function ambiguous(error: unknown) {
  return !(error instanceof ApiError) || error.status >= 500;
}

function HostKeyAlert({ message, node, verify }: { message: string; node: Node; verify: VerifyMutation }) {
  const online = useOnline();
  return (
    <Notice
      tone="error"
      title="SSH host identity mismatch"
      action={
        <Button size="sm" onClick={() => verify.mutate(node.id)} pending={verify.isPending} disabled={!online}>
          {!verify.isPending && <RefreshCw aria-hidden className="size-3.5" />}
          Verify
        </Button>
      }
    >
      <p className="font-mono text-xs break-all text-fg">{message}</p>
      <p className="mt-1.5">
        Cove refuses to connect and keeps the saved host keys unchanged. Check the host from a trusted console and reconcile its host key there. Cove never accepts the key it observed. A bastion with a new verified key must be recreated after its nodes are detached. Verify the node once the host presents its pinned key again.
      </p>
    </Notice>
  );
}

function RouteForm({ node, saved, bastions }: { node: Node; saved: NodeBastion; bastions: Bastion[] }) {
  const save = useSaveNodeBastion(node.id);
  const online = useOnline();
  const [choice, setChoice] = useState(saved.bastion_id);
  const known = saved.bastion_id === "" || bastions.some((bastion) => bastion.id === saved.bastion_id);
  const changed = choice !== saved.bastion_id;
  const selected = bastions.find((bastion) => bastion.id === choice);
  const hint = selected
    ? `The bastion at ${bastionEndpoint(selected)} must permit forwarding to ${node.address}:${node.port}, resolved from the bastion.`
    : "The main node opens SSH to the worker address itself.";

  return (
    <Subsection title="Route" description="How the main node reaches this worker. Saving resets readiness; verify the node afterwards.">
      <form
        className="space-y-3"
        onSubmit={(event) => {
          event.preventDefault();
          if (changed) save.mutate(choice);
        }}
      >
        <Field id={`node-${node.id}-route`} label="Connect through" hint={hint}>
          <Select
            id={`node-${node.id}-route`}
            value={choice}
            disabled={save.isPending}
            aria-describedby={`node-${node.id}-route-hint`}
            onChange={(event) => {
              setChoice(event.target.value);
              save.reset();
            }}
          >
            <option value="">Direct from the main node</option>
            {!known && <option value={saved.bastion_id}>Saved bastion (not listed)</option>}
            {bastions.map((bastion) => (
              <option key={bastion.id} value={bastion.id}>
                {`${bastion.name} (${bastionEndpoint(bastion)})`}
              </option>
            ))}
          </Select>
        </Field>
        {bastions.length === 0 && <p className="text-xs text-fg-muted">No bastions are saved. Add one on the Nodes page to route through a jump host.</p>}
        {save.isError && (
          <Notice tone="error" title="Could not save the route">
            {errorMessage(save.error)}
            {ambiguous(save.error) && " The saved route was reloaded; check it before trying again."}
          </Notice>
        )}
        {save.isSuccess && !changed && (
          <Notice tone="info" title="Route saved">
            Readiness was reset. Update the bastion's PermitOpen lists if needed, then verify the node.
          </Notice>
        )}
        {!online && <Notice tone="warn">You are offline. Reconnect to change the route.</Notice>}
        <div className="flex justify-end gap-2">
          {changed && (
            <Button variant="ghost" onClick={() => setChoice(saved.bastion_id)} disabled={save.isPending}>
              Discard
            </Button>
          )}
          <Button type="submit" variant="primary" pending={save.isPending} disabled={!changed || !online}>
            Save route
          </Button>
        </div>
      </form>
    </Subsection>
  );
}

const actionCopy: Record<IdentityAction, { label: string; title: string; confirm: string; body: string }> = {
  rotate: {
    label: "Rotate key",
    title: "Rotate this node's SSH key?",
    confirm: "Rotate key",
    body: "Cove adds a new restricted worker key while the current key keeps working, proves a fresh SSH and worker connection with it, saves it, and only then removes the old key from the worker. If the worker or panel is unreachable the rotation stays pending and resumes; it never removes the old key before the new one is saved. Host keys do not change. New setup commands are refused until it finishes.",
  },
  retry: {
    label: "Retry now",
    title: "Retry the pending step?",
    confirm: "Retry now",
    body: "Cove attempts the pending step again on its next pass instead of waiting for the scheduled retry. It does not mean the step has finished; this page shows the result.",
  },
  cancel: {
    label: "Cancel rotation",
    title: "Cancel the key rotation?",
    confirm: "Cancel rotation",
    body: "Cove removes only the new staged key from the worker. The current key stays saved and in use. Cancelling also waits for the worker to be reachable.",
  },
};

function identityBlocker(node: Node, online: boolean) {
  if (!online) return "You are offline. Reconnect to change the SSH key.";
  if (node.state === "pending") return "Finish setting up the node before rotating its key.";
  // A failed probe stores no capabilities, which says nothing about the worker's version.
  if (!node.capabilities) return "Cove has no current probe result for this worker. A pending rotation resumes when Cove can connect; verify the node before changing it.";
  if (!node.capabilities.ssh_identity) return "This worker cannot rotate its SSH key. Upgrade the worker and verify the node.";
  return undefined;
}

function IdentityStatus({ identity }: { identity: NodeIdentity }) {
  const info = identityStates[identity.state];
  return (
    <div className="space-y-1">
      <StatusLabel tone={info.tone} label={info.label} pulse={identityPending(identity.state)} />
      <p className="text-xs text-fg-muted">{info.description}</p>
      {identity.updated_at && (
        <p className="text-xs text-fg-subtle">
          Updated <RelativeTime value={identity.updated_at} />
        </p>
      )}
    </div>
  );
}

function IdentityRotation({ node }: { node: Node }) {
  const client = useQueryClient();
  const identity = useQuery(nodeIdentityQuery(client, node.id));
  const act = useIdentityAction(node.id);
  const online = useOnline();
  const [confirming, setConfirming] = useState<IdentityAction | null>(null);
  const blocker = identityBlocker(node, online);
  const description = "The private key the main node uses to sign in to this worker. It is never shown or downloaded.";

  if (identity.isPending) return <Subsection title="SSH key" description={description}><LoadingRows label="Loading SSH key status" rows={1} /></Subsection>;
  if (identity.isError && !identity.data) {
    return (
      <Subsection title="SSH key" description={description}>
        <QueryError what="SSH key status" error={identity.error} onRetry={() => void identity.refetch()} retrying={identity.isFetching} />
      </Subsection>
    );
  }
  const state = identity.data.state;
  const pending = identityPending(state);
  const actions = identityActions(state);
  const copy = confirming ? actionCopy[confirming] : null;

  return (
    <Subsection
      title="SSH key"
      description={description}
      actions={actions.map((action) => (
        <Button key={action} variant={action === "cancel" ? "danger" : action === "rotate" ? "secondary" : "primary"} disabled={blocker !== undefined || act.isPending} onClick={() => setConfirming(action)}>
          {action === "rotate" ? <KeyRound aria-hidden className="size-3.5" /> : action === "retry" ? <RotateCw aria-hidden className="size-3.5" /> : <X aria-hidden className="size-3.5" />}
          {actionCopy[action].label}
        </Button>
      ))}
    >
      <IdentityStatus identity={identity.data} />
      {identity.isRefetchError && <QueryError what="the latest SSH key status" error={identity.error} onRetry={() => void identity.refetch()} retrying={identity.isFetching} />}
      {identity.data.error && (
        <Notice tone="warn" title={pending ? "The last attempt did not finish" : "Last error"}>
          <span className="break-words">{identity.data.error}</span>
          {pending && " Cove retries pending steps about once a minute."}
        </Notice>
      )}
      {state === "switched" && <Notice title="Cleanup cannot be cancelled">The new key is already saved. Cove keeps retrying removal of the old key until the worker confirms it.</Notice>}
      {blocker && <p className="text-xs text-fg-muted">{blocker}</p>}
      {act.isError && (
        <Notice tone="error" title="The request did not complete">
          {errorMessage(act.error)}
          {ambiguous(act.error) && " The status above was refreshed; the action may or may not have been saved. Check it before trying again."}
        </Notice>
      )}
      <ConfirmDialog
        open={copy !== null}
        title={copy?.title ?? ""}
        confirmLabel={copy?.confirm ?? ""}
        danger={confirming === "cancel"}
        pending={act.isPending}
        onCancel={() => setConfirming(null)}
        onConfirm={() => {
          if (!confirming) return;
          act.mutate(confirming, { onSettled: () => setConfirming(null) });
        }}
      >
        {copy?.body}
      </ConfirmDialog>
    </Subsection>
  );
}

export function NodeSSH({ node, verify }: { node: Node; verify: VerifyMutation }) {
  const saved = useQuery({ ...nodeBastionQuery(node.id), enabled: node.mode === "ssh" });
  const bastions = useQuery({ ...bastionsQuery, enabled: node.mode === "ssh" });

  if (node.mode !== "ssh") {
    return (
      <Section title={title}>
        <p className="text-[13px] text-fg-muted">This node uses the local socket on the main node. Bastions and SSH key rotation do not apply.</p>
      </Section>
    );
  }
  return (
    <Section title={title} description="Both the bastion's and the worker's host keys are pinned. Cove never connects without them." flush>
      <div className="divide-y divide-line">
        {saved.data?.host_key_error && (
          <div className="p-4">
            <HostKeyAlert message={saved.data.host_key_error} node={node} verify={verify} />
          </div>
        )}
        {saved.isPending || bastions.isPending ? (
          <LoadingRows label="Loading SSH route" rows={2} />
        ) : saved.isError || bastions.isError ? (
          <div className="p-4">
            <QueryError
              what="the SSH route"
              error={saved.error ?? bastions.error}
              onRetry={() => void Promise.all([saved.refetch(), bastions.refetch()])}
              retrying={saved.isFetching || bastions.isFetching}
            />
          </div>
        ) : (
          <RouteForm node={node} saved={saved.data} bastions={bastions.data} />
        )}
        <IdentityRotation node={node} />
      </div>
    </Section>
  );
}

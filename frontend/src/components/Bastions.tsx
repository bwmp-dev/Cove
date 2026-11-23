import { useQuery } from "@tanstack/react-query";
import { ChevronRight, Plus, Trash2 } from "lucide-react";
import { useState, type FormEvent } from "react";
import { bastionsQuery, maxBastions, useCreateBastion, useDeleteBastion, type Bastion } from "../api/bastions";
import { errorMessage } from "../api/client";
import { authorizedKeyLine, bastionEndpoint, sshdMatchBlock, validateBastion, type BastionErrors } from "../lib/bastions";
import { useOnline } from "../lib/hooks";
import { describedBy, inputClass } from "../lib/styles";
import { Dialog } from "./ConfirmDialog";
import { CopyButton } from "./CopyButton";
import { Button, Field, LoadingRows, Notice, QueryError, Section } from "./ui";

const description = "Jump hosts the main node can use to reach SSH workers on private networks. Cove pins both the bastion's and the worker's host keys and never falls back to an unpinned or direct connection.";

export function PublicValue({ label, value, copyLabel }: { label: string; value: string; copyLabel: string }) {
  return (
    <div className="min-w-0 space-y-1.5">
      <div className="flex flex-wrap items-center justify-between gap-2">
        <p className="text-xs font-medium text-fg">{label}</p>
        <CopyButton text={value} label={copyLabel} variant="ghost" />
      </div>
      <pre className="overflow-x-auto rounded-md border border-line bg-panel px-2.5 py-2 font-mono text-xs break-all whitespace-pre-wrap text-fg-muted">{value}</pre>
    </div>
  );
}

function BastionSetup({ bastion }: { bastion: Bastion }) {
  return (
    <div className="space-y-4 border-t border-line px-4 py-4">
      <p className="text-[13px] text-fg-muted">
        Cove does not configure the bastion. On the bastion, create the dedicated account <span className="font-mono text-xs text-fg">{bastion.user}</span> for forwarding only, install the public key below for it, and apply the restrictions with OpenSSH. Copying these values changes nothing on the host. Then select this bastion on each node's page and verify the node.
      </p>
      <PublicValue label="Generated public key" value={bastion.public_key} copyLabel="Copy public key" />
      <PublicValue label="Pinned bastion host key" value={bastion.host_key} copyLabel="Copy host key" />
      <p className="text-xs text-fg-muted">
        Host key fingerprint <span className="font-mono break-all text-fg">{bastion.fingerprint}</span>
      </p>
      <div className="space-y-2">
        <h4 className="text-[13px] font-semibold text-fg">Required restrictions</h4>
        <p className="text-xs text-fg-muted">
          Allow local TCP forwarding only to the listed worker destinations. Refuse remote forwarding, agent forwarding, X11, stream-local forwarding, a PTY and a shell. Put this block at the end of <span className="font-mono">/etc/ssh/sshd_config</span>, check it with <span className="font-mono">sshd -t</span> and reload OpenSSH. The authorized_keys options alone do not refuse remote forwarding; keep both.
        </p>
        {bastion.limited && (
          <Notice tone="warn" title="Destination list clipped">
            More nodes use this bastion than the API lists. Add their SSH addresses to both PermitOpen lists yourself.
          </Notice>
        )}
        {bastion.targets.length === 0 && (
          <Notice title="No destinations yet">
            No node uses this bastion, so the configuration below permits no forwarding. Update both lines after you select this bastion for a node; the node's SSH address and port become a destination.
          </Notice>
        )}
        <PublicValue label="sshd_config" value={sshdMatchBlock(bastion)} copyLabel="Copy sshd_config block" />
        <PublicValue label={`authorized_keys for ${bastion.user}`} value={authorizedKeyLine(bastion)} copyLabel="Copy authorized_keys line" />
      </div>
    </div>
  );
}

function BastionRow({ bastion, open, onToggle, onDelete }: { bastion: Bastion; open: boolean; onToggle: () => void; onDelete: () => void }) {
  const detailsId = `bastion-${bastion.id}-setup`;
  const count = bastion.targets.length;
  const nodes = `${bastion.limited ? "More than " : ""}${count.toLocaleString("en")} ${count === 1 && !bastion.limited ? "node" : "nodes"}`;
  return (
    <li>
      <div className="flex flex-wrap items-center gap-x-4 gap-y-2 px-4 py-2.5 hover:bg-raised">
        <button type="button" aria-expanded={open} aria-controls={detailsId} onClick={onToggle} className="-mx-1 flex min-w-0 flex-1 basis-64 items-start gap-1.5 rounded px-1 text-left">
          <ChevronRight aria-hidden className={`mt-0.5 size-4 shrink-0 text-fg-muted transition-transform ${open ? "rotate-90" : ""}`} />
          <span className="min-w-0">
            <span className="block text-[13px] font-medium break-words text-fg">{bastion.name}</span>
            <span className="block font-mono text-xs break-all text-fg-muted">{bastionEndpoint(bastion)}</span>
            <span className="block font-mono text-xs break-all text-fg-subtle">{bastion.fingerprint}</span>
          </span>
        </button>
        <span className="text-xs text-fg-muted">{nodes}</span>
        <Button variant="ghost" size="sm" onClick={onDelete} aria-label={`Delete ${bastion.name}`}>
          <Trash2 aria-hidden className="size-3.5" />
          Delete
        </Button>
      </div>
      <div id={detailsId} hidden={!open}>
        {open && <BastionSetup bastion={bastion} />}
      </div>
    </li>
  );
}

const emptyForm = { name: "", address: "", port: "22", user: "", host_key: "" };

function CreateBastionDialog({ open, onClose, onCreated }: { open: boolean; onClose: () => void; onCreated: (bastion: Bastion) => void }) {
  const create = useCreateBastion();
  const online = useOnline();
  const [values, setValues] = useState(emptyForm);
  const [errors, setErrors] = useState<BastionErrors>({});
  const field = (key: keyof typeof emptyForm) => ({
    id: `bastion-${key}`,
    value: values[key],
    onChange: (event: { target: { value: string } }) => {
      setValues((current) => ({ ...current, [key]: event.target.value }));
      create.reset();
    },
    "aria-invalid": errors[key] ? true : undefined,
    autoComplete: "off",
  });
  const close = () => {
    setValues(emptyForm);
    setErrors({});
    create.reset();
    onClose();
  };
  const submit = (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    const result = validateBastion(values);
    setErrors(result.errors);
    if (!result.input) return;
    create.mutate(result.input, {
      onSuccess: (bastion) => {
        setValues(emptyForm);
        setErrors({});
        create.reset();
        onCreated(bastion);
      },
    });
  };
  const userHint = "A dedicated account used only for forwarding, never root or the worker's cove account.";
  const keyHint = "One host public key line, read from the bastion's trusted console (for example /etc/ssh/ssh_host_ed25519_key.pub) or from your provisioning records. Do not copy it from ssh-keyscan or a first connection.";
  return (
    <Dialog open={open} title="Add bastion" onClose={close}>
      <form onSubmit={submit} noValidate className="space-y-4">
        <fieldset disabled={create.isPending} className="space-y-4">
          <Field id="bastion-name" label="Name" error={errors.name}>
            <input {...field("name")} className={inputClass} maxLength={120} aria-describedby={describedBy("bastion-name", errors.name)} />
          </Field>
          <div className="grid gap-4 sm:grid-cols-[1fr_6rem]">
            <Field id="bastion-address" label="SSH address" error={errors.address}>
              <input {...field("address")} className={`${inputClass} font-mono`} autoCapitalize="none" spellCheck={false} placeholder="bastion.example.net" aria-describedby={describedBy("bastion-address", errors.address)} />
            </Field>
            <Field id="bastion-port" label="SSH port" error={errors.port}>
              <input {...field("port")} className={`${inputClass} font-mono`} inputMode="numeric" aria-describedby={describedBy("bastion-port", errors.port)} />
            </Field>
          </div>
          <Field id="bastion-user" label="Forwarding user" error={errors.user} hint={userHint}>
            <input {...field("user")} className={`${inputClass} font-mono`} autoCapitalize="none" spellCheck={false} placeholder="cove-jump" aria-describedby={describedBy("bastion-user", errors.user, userHint)} />
          </Field>
          <Field id="bastion-host_key" label="Verified bastion host public key" error={errors.host_key} hint={keyHint}>
            <textarea {...field("host_key")} className={`${inputClass} font-mono`} rows={3} spellCheck={false} placeholder="ssh-ed25519 AAAA…" aria-describedby={describedBy("bastion-host_key", errors.host_key, keyHint)} />
          </Field>
        </fieldset>
        <p className="text-xs text-fg-muted">Cove generates the bastion login key and keeps its private half sealed on the panel. Only the public key is shown.</p>
        {!online && <Notice tone="warn">You are offline. Reconnect to add a bastion.</Notice>}
        {create.isError && (
          <Notice tone="error" title="Could not add the bastion">
            {errorMessage(create.error)}
          </Notice>
        )}
        <div className="flex justify-end gap-2 border-t border-line pt-3">
          <Button variant="ghost" onClick={close} disabled={create.isPending}>
            Cancel
          </Button>
          <Button type="submit" variant="primary" pending={create.isPending} disabled={!online}>
            Add bastion
          </Button>
        </div>
      </form>
    </Dialog>
  );
}

function DeleteBastionDialog({ bastion, onClose }: { bastion: Bastion | null; onClose: () => void }) {
  const remove = useDeleteBastion();
  const online = useOnline();
  const assigned = (bastion?.targets.length ?? 0) > 0;
  const close = () => {
    remove.reset();
    onClose();
  };
  return (
    <Dialog
      open={bastion !== null}
      title={`Delete ${bastion?.name ?? "bastion"}?`}
      onClose={close}
      footer={
        <>
          <Button variant="ghost" onClick={close} disabled={remove.isPending}>
            Cancel
          </Button>
          <Button variant="danger" pending={remove.isPending} disabled={assigned || !online} onClick={() => bastion && remove.mutate(bastion.id, { onSuccess: close })}>
            Delete bastion
          </Button>
        </>
      }
    >
      <div className="space-y-3">
        <p>Cove deletes the saved bastion and its sealed login key. Nodes are never deleted, and nothing changes on the bastion host: remove the authorized key and account there yourself.</p>
        {assigned && (
          <Notice tone="warn" title="Nodes still use this bastion">
            Switch each node to another route on its page first. This bastion cannot be deleted while nodes use it.
          </Notice>
        )}
        {!online && <Notice tone="warn">You are offline. Reconnect to delete the bastion.</Notice>}
        {remove.isError && (
          <Notice tone="error" title="Could not delete the bastion">
            {errorMessage(remove.error)}
          </Notice>
        )}
      </div>
    </Dialog>
  );
}

export function Bastions() {
  const bastions = useQuery(bastionsQuery);
  const online = useOnline();
  const [creating, setCreating] = useState(false);
  const [expanded, setExpanded] = useState<string | null>(null);
  const [deleting, setDeleting] = useState<Bastion | null>(null);
  const full = (bastions.data?.length ?? 0) >= maxBastions;

  return (
    <Section
      title="SSH bastions"
      description={description}
      flush
      actions={
        <Button onClick={() => setCreating(true)} disabled={!bastions.data || full || !online} title={full ? `At most ${maxBastions} bastions are supported` : undefined}>
          <Plus aria-hidden className="size-3.5" />
          Add bastion
        </Button>
      }
    >
      {bastions.isPending ? (
        <LoadingRows label="Loading bastions" rows={2} />
      ) : bastions.isError && !bastions.data ? (
        <div className="p-3">
          <QueryError what="bastions" error={bastions.error} onRetry={() => void bastions.refetch()} retrying={bastions.isFetching} />
        </div>
      ) : (
        <>
          {full && (
            <div className="border-b border-line px-4 py-3">
              <Notice>This instance has the maximum of {maxBastions} bastions. Delete an unused bastion to add another.</Notice>
            </div>
          )}
          {bastions.data.length === 0 ? (
            <p className="px-4 py-4 text-[13px] text-fg-muted">No bastions. Nodes connect directly from the main node. Add a bastion when a worker is only reachable through a jump host.</p>
          ) : (
            <ul className="divide-y divide-line" aria-label="SSH bastions">
              {bastions.data.map((bastion) => (
                <BastionRow key={bastion.id} bastion={bastion} open={expanded === bastion.id} onToggle={() => setExpanded(expanded === bastion.id ? null : bastion.id)} onDelete={() => setDeleting(bastion)} />
              ))}
            </ul>
          )}
        </>
      )}
      <CreateBastionDialog
        open={creating}
        onClose={() => setCreating(false)}
        onCreated={(bastion) => {
          setCreating(false);
          setExpanded(bastion.id);
        }}
      />
      <DeleteBastionDialog bastion={deleting} onClose={() => setDeleting(null)} />
    </Section>
  );
}

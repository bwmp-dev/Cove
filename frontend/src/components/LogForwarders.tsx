import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Plus } from "lucide-react";
import { useId, useState, type FormEvent } from "react";
import { errorMessage, request } from "../api/client";
import { logForwardersQuery, type ForwarderKind, type LogForwarder, type LogForwarderInput } from "../api/outputs";
import { useOnline } from "../lib/hooks";
import { forwarderFromForm, forwarderKindOrder, forwarderKinds, isForwarderKind, type ForwarderField } from "../lib/outputs";
import { canManageTeamDestinations } from "../lib/permissions";
import { useSession } from "../lib/session";
import { describedBy, inputClass } from "../lib/styles";
import { ConfirmDialog } from "./ConfirmDialog";
import { RelativeTime } from "./RelativeTime";
import { Select } from "./Select";
import { StatusLabel } from "./status";
import { Button, Field, LoadingRows, Notice, QueryError, Section, SwitchField } from "./ui";

const maxForwarders = 8;
const description =
  "Send newly stored, redacted service logs to an external receiver over public TLS. Batches of up to 100 lines are queued, sealed, and retried up to six times over 24 hours; delivery is at least once. HTTP receivers can deduplicate on the X-Cove-Delivery header; TLS syslog has no such header and can receive part of a batch again when it is retried. Disabling a forwarder discards its queue.";

type FieldName = ForwarderField;

function ReceiverFields({ kind, saved, fieldId, message }: { kind: ForwarderKind; saved: { endpoint: boolean; token: boolean; username: boolean }; fieldId: (field: FieldName) => string; message: (field: FieldName) => string | undefined }) {
  const spec = forwarderKinds[kind];
  const aria = (field: FieldName) => ({ "aria-invalid": message(field) ? true : undefined, "aria-describedby": describedBy(fieldId(field), message(field), true) });
  const savedHint = (label: string) => `The saved ${label.toLowerCase()} is hidden. Leave this blank to keep it, or enter a new one to replace it.`;
  return (
    <>
      <Field id={fieldId("endpoint")} label={spec.endpointLabel} optional={saved.endpoint} error={message("endpoint")} hint={saved.endpoint ? savedHint(spec.endpointLabel) : spec.endpointHint}>
        <input id={fieldId("endpoint")} name="endpoint" className={`${inputClass} font-mono`} placeholder={saved.endpoint ? "Saved" : spec.endpointPlaceholder} maxLength={4096} autoComplete="off" spellCheck={false} required={!saved.endpoint} {...aria("endpoint")} />
      </Field>
      {spec.token !== "none" && (
        <div className="grid gap-4 sm:grid-cols-2">
          {spec.username && (
            <Field id={fieldId("username")} label="Username" optional error={message("username")} hint={saved.username ? savedHint("username") : "Set for Basic authentication; the token becomes the password."}>
              <input id={fieldId("username")} name="username" className={inputClass} placeholder={saved.username ? "Saved" : undefined} maxLength={256} autoComplete="off" spellCheck={false} {...aria("username")} />
            </Field>
          )}
          <Field id={fieldId("token")} label={spec.tokenLabel} optional={spec.token === "bearer" || saved.token} error={message("token")} hint={saved.token ? savedHint(spec.tokenLabel) : spec.tokenHint}>
            <input id={fieldId("token")} name="token" type="password" className={inputClass} placeholder={saved.token ? "Saved" : undefined} maxLength={4096} autoComplete="new-password" spellCheck={false} {...aria("token")} />
          </Field>
        </div>
      )}
    </>
  );
}

function KindField({ id, value, replacing, onChange }: { id: string; value: ForwarderKind; replacing: boolean; onChange: (value: ForwarderKind) => void }) {
  return (
    <Field id={id} label="Receiver" hint={replacing ? "Changing the receiver type needs a new endpoint and credentials." : undefined}>
      <Select id={id} value={value} onChange={(event) => isForwarderKind(event.target.value) && onChange(event.target.value)} aria-describedby={replacing ? `${id}-hint` : undefined}>
        {forwarderKindOrder.map((kind) => <option key={kind} value={kind}>{forwarderKinds[kind].label}</option>)}
      </Select>
    </Field>
  );
}

// Request bodies carry credentials, so the mutation is dropped from the cache as soon as the form closes.
function useSaveForwarder(id: string | undefined, onSaved: () => void) {
  const client = useQueryClient();
  return useMutation({
    gcTime: 0,
    mutationFn: (input: LogForwarderInput) => (id ? request<{ id: string }>("PUT", `/api/notifications/forwarders/${encodeURIComponent(id)}`, input) : request<{ id: string }>("POST", "/api/notifications/forwarders", input)),
    onSuccess: async () => {
      await client.invalidateQueries({ queryKey: logForwardersQuery.queryKey });
      onSaved();
    },
  });
}

function ForwarderForm({ initial, close }: { initial?: LogForwarder; close: () => void }) {
  const id = useId();
  const online = useOnline();
  const [kind, setKind] = useState<ForwarderKind>(initial?.kind ?? "loki");
  const [error, setError] = useState<{ field: FieldName; message: string }>();
  // Saved values are kept by the API only while the receiver type stays the same.
  const keeps = initial !== undefined && initial.kind === kind;
  const endpointSaved = keeps && initial.endpoint_set;
  const tokenSaved = keeps && initial.token_set;
  const save = useSaveForwarder(initial?.id, close);
  const fieldId = (field: FieldName) => `${id}-${field}`;
  const message = (field: FieldName) => (error?.field === field ? error.message : undefined);

  function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    const input = forwarderFromForm(new FormData(event.currentTarget), kind, { endpoint: endpointSaved, token: tokenSaved });
    if ("field" in input) {
      setError(input);
      document.getElementById(fieldId(input.field))?.focus();
      return;
    }
    setError(undefined);
    save.mutate(input);
  }

  return (
    <form onSubmit={submit} noValidate className="space-y-4 border-b border-line p-4">
      <h3 className="text-[13px] font-semibold text-fg">{initial ? `Edit ${initial.name}` : "New log forwarder"}</h3>
      <fieldset disabled={save.isPending} className="min-w-0 space-y-4">
        <div className="grid gap-4 sm:grid-cols-2">
          <Field id={fieldId("name")} label="Name" error={message("name")}>
            <input id={fieldId("name")} name="name" className={inputClass} defaultValue={initial?.name ?? ""} maxLength={120} autoComplete="off" required aria-invalid={message("name") ? true : undefined} aria-describedby={describedBy(fieldId("name"), message("name"))} />
          </Field>
          <KindField id={`${id}-kind`} value={kind} replacing={initial !== undefined && !keeps} onChange={(value) => { setKind(value); setError(undefined); }} />
        </div>
        {/* Keyed by kind so values typed for one receiver type never carry over to another. */}
        <ReceiverFields key={kind} kind={kind} saved={{ endpoint: endpointSaved, token: tokenSaved, username: keeps && initial.username_set }} fieldId={fieldId} message={message} />
        {keeps && <p className="text-xs text-fg-muted">Saved credentials cannot be cleared on their own. Replace them, or delete the forwarder and create a new one.</p>}
        <SwitchField id={`${id}-enabled`} name="enabled" label="Enabled" defaultChecked={initial?.enabled ?? true} description="New log lines are only queued while enabled. Turning it off discards queued batches." />
        {save.isError && <Notice tone="error" title="Could not save the forwarder">{errorMessage(save.error)}</Notice>}
      </fieldset>
      <div className="flex flex-wrap justify-end gap-2">
        <Button variant="ghost" onClick={close} disabled={save.isPending}>Cancel</Button>
        <Button type="submit" variant="primary" pending={save.isPending} disabled={!online}>{initial ? "Save forwarder" : "Create forwarder"}</Button>
      </div>
    </form>
  );
}

function deliveryState(forwarder: LogForwarder) {
  if (!forwarder.enabled) return { tone: "neutral", label: "Disabled" } as const;
  if (forwarder.last_error) return { tone: "bad", label: "Failing" } as const;
  if (forwarder.last_success) return { tone: "ok", label: "Delivering" } as const;
  return { tone: "neutral", label: "No deliveries yet" } as const;
}

function ForwarderRow({ forwarder, busy, testing, tested, testError, onTest, onEdit, onDelete }: { forwarder: LogForwarder; busy: boolean; testing: boolean; tested: boolean; testError?: string; onTest: () => void; onEdit: () => void; onDelete: () => void }) {
  const state = deliveryState(forwarder);
  return (
    <li className="space-y-1.5 px-4 py-2.5 hover:bg-raised">
      <div className="flex flex-wrap items-start justify-between gap-x-4 gap-y-2">
        <div className="min-w-0 flex-1 basis-56">
          <p className="text-[13px] font-medium break-words text-fg">{forwarder.name}</p>
          <p className="text-xs text-fg-muted">
            {forwarderKinds[forwarder.kind]?.label ?? forwarder.kind} · {forwarder.endpoint_set ? "Endpoint saved" : "No endpoint"}
            {forwarder.token_set && " · Credentials saved"}
            {forwarder.username_set && " · Username saved"}
          </p>
        </div>
        <StatusLabel tone={state.tone} label={state.label} />
        <div className="flex flex-wrap gap-1">
          <Button size="sm" variant="ghost" onClick={onTest} disabled={busy} pending={testing} aria-label={`Send test to ${forwarder.name}`}>Send test</Button>
          <Button size="sm" variant="ghost" onClick={onEdit} disabled={busy} aria-label={`Edit ${forwarder.name}`}>Edit</Button>
          <Button size="sm" variant="ghost" onClick={onDelete} disabled={busy} aria-label={`Delete ${forwarder.name}`}>Delete</Button>
        </div>
      </div>
      <p className="text-xs text-fg-muted tabular-nums">
        {forwarder.sent_entries} sent · {forwarder.pending_batches} {forwarder.pending_batches === 1 ? "batch" : "batches"} queued
        {forwarder.dropped_entries > 0 && <span className="text-warn"> · {forwarder.dropped_entries} dropped</span>}
        {forwarder.last_success && <> · Last delivered <RelativeTime value={forwarder.last_success} /></>}
      </p>
      {forwarder.last_error && <p className="text-xs break-words text-bad">{forwarder.last_error}</p>}
      {forwarder.dropped_entries > 0 && <p className="text-xs text-fg-muted">Dropped lines were discarded after retries ran out, the queue limit was reached, or the forwarder was disabled.</p>}
      <p role="status" className="text-xs empty:hidden">
        {tested && <span className="text-ok">Test message delivered.</span>}
        {testError && <span className="text-bad">{testError}</span>}
      </p>
    </li>
  );
}

function ManagedForwarders() {
  const client = useQueryClient();
  const online = useOnline();
  const forwarders = useQuery(logForwardersQuery);
  // undefined is the new-forwarder form; null shows no form.
  const [editing, setEditing] = useState<LogForwarder | undefined | null>(null);
  const [removing, setRemoving] = useState<LogForwarder | null>(null);
  const test = useMutation({ gcTime: 0, mutationFn: (forwarder: LogForwarder) => request<{ ok: boolean }>("POST", `/api/notifications/forwarders/${encodeURIComponent(forwarder.id)}/test`) });
  const deletion = useMutation({
    mutationFn: (forwarder: LogForwarder) => request("DELETE", `/api/notifications/forwarders/${encodeURIComponent(forwarder.id)}`),
    onSuccess: async () => {
      setRemoving(null);
      await client.invalidateQueries({ queryKey: logForwardersQuery.queryKey });
    },
  });
  const count = forwarders.data?.length ?? 0;
  const action = forwarders.isSuccess && editing === null && (
    <Button onClick={() => setEditing(undefined)} disabled={!online || count >= maxForwarders}>
      <Plus aria-hidden className="size-3.5" />
      New forwarder
    </Button>
  );

  let body;
  if (forwarders.isPending) {
    body = <LoadingRows label="Loading log forwarders" rows={2} />;
  } else if (forwarders.isError && !forwarders.data) {
    body = <div className="p-4"><QueryError what="log forwarders" error={forwarders.error} onRetry={() => void forwarders.refetch()} retrying={forwarders.isFetching} /></div>;
  } else {
    body = (
      <>
        {editing !== null && <ForwarderForm key={editing?.id ?? "new"} initial={editing} close={() => setEditing(null)} />}
        {count >= maxForwarders && <div className="border-b border-line p-4"><Notice tone="warn">This team has the maximum of {maxForwarders} log forwarders. Delete one to add another.</Notice></div>}
        {deletion.isError && <div className="border-b border-line p-4"><Notice tone="error" title="Could not delete the forwarder">{errorMessage(deletion.error)}</Notice></div>}
        {count === 0 ? (
          <p className="px-4 py-4 text-[13px] text-fg-muted">No log forwarders. Logs stay in Cove only.</p>
        ) : (
          <ul aria-label="Log forwarders" className="divide-y divide-line">
            {forwarders.data.map((forwarder) => {
              const mine = test.variables?.id === forwarder.id;
              return (
                <ForwarderRow
                  key={forwarder.id}
                  forwarder={forwarder}
                  busy={!online || deletion.isPending || test.isPending}
                  testing={mine && test.isPending}
                  tested={mine && test.isSuccess}
                  testError={mine && test.isError ? errorMessage(test.error) : undefined}
                  onTest={() => test.mutate(forwarder)}
                  onEdit={() => setEditing(forwarder)}
                  onDelete={() => { deletion.reset(); setRemoving(forwarder); }}
                />
              );
            })}
          </ul>
        )}
      </>
    );
  }

  return (
    <Section title="Log forwarding" description={description} actions={action} flush>
      {body}
      <ConfirmDialog
        open={removing !== null}
        title={`Delete ${removing?.name ?? "forwarder"}?`}
        confirmLabel="Delete forwarder"
        danger
        pending={deletion.isPending}
        onCancel={() => setRemoving(null)}
        onConfirm={() => removing && deletion.mutate(removing)}
      >
        Its saved endpoint and credentials are removed and any queued batches are discarded without being sent.
      </ConfirmDialog>
    </Section>
  );
}

export function LogForwarders() {
  const session = useSession();
  if (!canManageTeamDestinations(session)) {
    return (
      <Section title="Log forwarding" description={description}>
        <Notice title="Team owners and admins manage log forwarding">Forwarders also need team-wide Edit configuration and Read secrets. Ask a team owner or admin to add or change forwarders.</Notice>
      </Section>
    );
  }
  return <ManagedForwarders />;
}

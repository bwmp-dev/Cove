import { useQuery, type UseQueryResult } from "@tanstack/react-query";
import { ClipboardCheck, Plus, RotateCw, Server, Trash2, X } from "lucide-react";
import { useId, useRef, useState, type FormEvent } from "react";
import { ApiError, errorMessage } from "../api/client";
import {
  changeActive,
  listenerStatusQuery,
  useApplyListeners,
  useInitializeListeners,
  usePlanListeners,
  type ListenerBinding,
  type ListenerChange,
  type ListenerPlan,
  type ListenerSettings,
  type ListenerStatus,
  type PublicBindings,
} from "../api/ingress-listeners";
import { workloadsQuery } from "../api/queries";
import type { Node } from "../api/types";
import { useOnline } from "../lib/hooks";
import {
  bindingDraft,
  draftFrom,
  entrypointName,
  formatBinding,
  listenerDraft,
  maxBindings,
  maxListeners,
  phaseInfo,
  protocolLabels,
  publicChange,
  settingsFrom,
  settingsKey,
  validateDraft,
  visibilityLabels,
  type BindingDraft,
  type FieldErrors,
  type ListenerDraft,
  type ListenerDraftSettings,
} from "../lib/ingress-listeners";
import { choiceClass, describedBy, inputClass, tableHeadClass, tableRowClass } from "../lib/styles";
import { ConfirmDialog } from "./ConfirmDialog";
import { RelativeTime } from "./RelativeTime";
import { UnsavedChangesGuard } from "./SaveBar";
import { Select } from "./Select";
import { Button, DefinitionList, Field, LoadingRows, Notice, QueryError, Section, Spinner, Subsection, SwitchField } from "./ui";

type Plan = ReturnType<typeof usePlanListeners>;
type Apply = ReturnType<typeof useApplyListeners>;

interface Reviewed {
  settings: ListenerSettings;
  plan: ListenerPlan;
}

function BindingText({ bindings }: { bindings?: ListenerBinding[] }) {
  if (!bindings?.length) return <span className="text-fg-muted">None</span>;
  return <span className="font-mono text-xs break-all">{bindings.map(formatBinding).join(", ")}</span>;
}

function noReply(error: unknown) {
  return error instanceof ApiError && (error.isNetwork || error.status === 502);
}

function ChangeNotice({ change }: { change: ListenerChange }) {
  const info = phaseInfo(change.phase, change.committed);
  return (
    <Notice tone={info.tone} title={`Listener change: ${info.label}`}>
      <p>
        Started <RelativeTime value={change.started_at} />
        {change.finished_at && (
          <>
            {", finished "}
            <RelativeTime value={change.finished_at} />
          </>
        )}
        .{" "}
        {info.active
          ? "Editing is paused until the worker finishes. It continues or restores the previous proxy after a restart."
          : change.phase === "completed" && change.committed
            ? "The proxy runs with the saved settings below."
            : "Check the saved settings and the proxy state below before planning again."}
      </p>
    </Notice>
  );
}

type StatusQuery = UseQueryResult<ListenerStatus>;

function blockedReason(online: boolean, supported: boolean, status: ListenerStatus, applying: boolean) {
  if (!online) return "You are offline.";
  if (!supported) return "This worker does not support managed listeners.";
  if (changeActive(status.change)) return "A listener change is in progress.";
  if (applying) return "Applying the reviewed plan.";
  return undefined;
}

// A finished, committed change is summarised in the description unless this page just applied it.
function changeToShow(change: ListenerChange | undefined, applied?: ListenerStatus) {
  if (!change) return undefined;
  const settledSuccess = !changeActive(change) && change.phase === "completed" && change.committed;
  return settledSuccess && applied?.change?.id !== change.id ? undefined : change;
}

export function ManagedListeners({ node }: { node: Node }) {
  const supported = node.capabilities?.ingress_exposures === true;
  const online = useOnline();
  const status = useQuery(listenerStatusQuery(node.id));
  const plan = usePlanListeners(node.id);
  const apply = useApplyListeners(node.id);
  const change = status.data?.change;
  const shown = changeToShow(change, apply.data);
  return (
    <Section
      title="Listeners and published ports"
      description={
        <>
          The shared proxy's public HTTP and HTTPS publications, HTTP/3 and extra listeners.
          {change && !shown && (
            <>
              {" Last change applied "}
              <RelativeTime value={change.finished_at ?? change.started_at} />.
            </>
          )}
        </>
      }
      width="form"
      flush
      actions={
        <Button variant="ghost" onClick={() => void status.refetch()} pending={status.isFetching} disabled={status.isPending}>
          {!status.isFetching && <RotateCw aria-hidden className="size-3.5" />}
          Reload status
        </Button>
      }
    >
      <div className="space-y-2 p-4 pb-0" aria-live="polite">
        <Notice tone="warn" title="Applying listener changes restarts the shared proxy">
          Cove validates the new settings in an isolated proxy, then briefly stops and recreates this node's proxy. Every connection through it is interrupted, for every team and for the dashboard if it is served here. Applications keep running, and the previous proxy is restored automatically if the new one cannot be committed. Editing dynamic configuration files does not restart the proxy.
        </Notice>
        <SupportNotices node={node} supported={supported} online={online} status={status.data} />
        {shown && <ChangeNotice change={shown} />}
        <ApplyFailure apply={apply} onReload={() => void status.refetch()} />
      </div>
      <ListenerBody node={node} status={status} plan={plan} apply={apply} online={online} supported={supported} />
    </Section>
  );
}

function SupportNotices({ node, supported, online, status }: { node: Node; supported: boolean; online: boolean; status?: ListenerStatus }) {
  return (
    <>
      {!node.capabilities && (
        <Notice tone="warn" title="Worker capabilities unknown">
          {node.name} has not reported its capabilities, so managed listener support cannot be confirmed. Verify the node first. Saved settings stay read-only.
        </Notice>
      )}
      {node.capabilities && !supported && (
        <Notice tone="warn" title="This worker cannot manage listeners">
          Only Linux workers that report managed listener support can change them. Upgrade and verify the worker. Saved settings stay read-only.
        </Notice>
      )}
      {!online && <Notice tone="warn" title="You are offline">Planning and applying are paused until the connection returns.</Notice>}
      {status?.proxy_present && !status.running && (
        <Notice tone="warn" title="The proxy is stopped">
          Applying a listener change checks that the host ports are available now without starting Traefik, and the proxy stays stopped. The ports are not reserved while it is stopped, so another program can take them before it starts. Cove does not start a proxy that was stopped.
        </Notice>
      )}
    </>
  );
}

function ListenerBody({ node, status, plan, apply, online, supported }: { node: Node; status: StatusQuery; plan: Plan; apply: Apply; online: boolean; supported: boolean }) {
  if (status.isPending) return <LoadingRows label="Loading listener settings" rows={3} />;
  if (status.isError) {
    return (
      <div className="p-4">
        <QueryError what="the listener settings" error={status.error} onRetry={() => void status.refetch()} retrying={status.isFetching} />
      </div>
    );
  }
  const blocked = blockedReason(online, supported, status.data, apply.isPending);
  if (!status.data.proxy_present) return <InitializeProxy node={node} blocked={blocked} />;
  return <ListenerForm key={settingsKey(status.data.settings)} node={node} status={status.data} plan={plan} apply={apply} blocked={blocked} />;
}

function ApplyFailure({ apply, onReload }: { apply: Apply; onReload: () => void }) {
  if (!apply.isError) return null;
  const lost = noReply(apply.error);
  return (
    <Notice
      tone="error"
      title={lost ? "No reply from the worker" : "The listener change was not applied"}
      action={
        <Button size="sm" onClick={onReload}>
          Reload status
        </Button>
      }
    >
      <p className="break-words">{lost ? "The worker may still finish or restore this change. Reload the status before planning again." : errorMessage(apply.error)}</p>
      <p className="mt-1">Your edits are kept. Review a fresh plan before applying again.</p>
    </Notice>
  );
}

function InitializeProxy({ node, blocked }: { node: Node; blocked?: string }) {
  const initialize = useInitializeListeners(node.id);
  const [confirming, setConfirming] = useState(false);
  return (
    <div className="space-y-3 p-4">
      <div className="flex flex-wrap items-start justify-between gap-x-4 gap-y-2">
        <div className="min-w-0 flex-1 basis-64">
          <h3 className="text-[13px] font-semibold text-fg">No managed proxy on this node</h3>
          <p className="mt-0.5 text-xs text-fg-muted">The proxy is usually created by the first routed resource. Create it here to configure listeners before that. It is created stopped and is not started from this page.</p>
        </div>
        <Button onClick={() => setConfirming(true)} disabled={!!blocked || initialize.isPending} pending={initialize.isPending}>
          {!initialize.isPending && <Server aria-hidden className="size-3.5" />}
          Create proxy
        </Button>
      </div>
      {blocked && <p className="text-xs text-fg-muted">{blocked}</p>}
      {initialize.isError && (
        <Notice tone="error" title="The proxy was not created">
          {noReply(initialize.error) ? "No reply from the worker. Reload the status to see whether it was created." : errorMessage(initialize.error)}
        </Notice>
      )}
      <ConfirmDialog
        open={confirming}
        title={`Create the proxy on ${node.name}?`}
        confirmLabel="Create stopped proxy"
        pending={initialize.isPending}
        onCancel={() => setConfirming(false)}
        onConfirm={() => initialize.mutate(undefined, { onSettled: () => setConfirming(false) })}
      >
        If this node has saved listener settings, Cove recreates the proxy with them; otherwise it creates its stock proxy. Any existing proxy network and data are kept. The new container stays stopped, so nothing is published until it is started.
      </ConfirmDialog>
    </div>
  );
}

interface FormProps {
  node: Node;
  status: ListenerStatus;
  plan: Plan;
  apply: Apply;
  blocked?: string;
}

function ListenerForm({ node, status, plan, apply, blocked }: FormProps) {
  const savedKey = settingsKey(status.settings);
  const [draft, setDraft] = useState<ListenerDraftSettings>(() => draftFrom(status));
  const [showErrors, setShowErrors] = useState(false);
  const [reviewed, setReviewed] = useState<Reviewed | null>(null);
  const formRef = useRef<HTMLFormElement>(null);
  const settings = settingsFrom(draft);
  const dirty = settingsKey(settings) !== savedKey;
  const errors = showErrors ? validateDraft(draft, status.public_bindings) : {};
  const editable = !blocked;

  // Any edit expires the reviewed plan and the outcome of earlier requests.
  function edit(next: ListenerDraftSettings) {
    setDraft(next);
    setReviewed(null);
    plan.reset();
  }

  function review(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    setShowErrors(true);
    if (Object.keys(validateDraft(draft, status.public_bindings)).length > 0) {
      requestAnimationFrame(() => formRef.current?.querySelector<HTMLElement>("[aria-invalid=true]")?.focus());
      return;
    }
    apply.reset();
    plan.mutate(settings, { onSuccess: (result) => setReviewed({ settings, plan: result }) });
  }

  function discard() {
    edit(draftFrom(status));
    setShowErrors(false);
  }

  return (
    <>
      <UnsavedChangesGuard dirty={dirty} />
      <form ref={formRef} onSubmit={review} aria-label="Listener settings" noValidate>
        <fieldset disabled={!editable} className="min-w-0 divide-y divide-line">
          <legend className="sr-only">Listener settings</legend>
          <PublicSettings draft={draft} actual={status.public_bindings} errors={errors} onChange={edit} />
          <ExtraListeners draft={draft} ports={status.ports} errors={errors} onChange={edit} />
        </fieldset>
        <div className="flex flex-wrap items-center justify-end gap-x-3 gap-y-2 border-t border-line px-4 py-3">
          <p role="status" className="mr-auto text-xs text-fg-muted">
            {plan.isPending ? "Planning against the current proxy" : blocked ? blocked : dirty ? "Unsaved changes. Review a plan to apply them." : "No unsaved changes"}
          </p>
          <Button variant="ghost" onClick={discard} disabled={!dirty || plan.isPending || apply.isPending}>
            Discard changes
          </Button>
          <Button type="submit" variant="primary" disabled={!dirty || !editable || !!reviewed} pending={plan.isPending}>
            {!plan.isPending && <ClipboardCheck aria-hidden className="size-3.5" />}
            Review changes
          </Button>
        </div>
      </form>
      {plan.isError && (
        <div className="border-t border-line p-4">
          <Notice tone="error" title="The plan was refused">
            <p className="break-words">{noReply(plan.error) ? "No reply from the worker. Nothing was changed. Reload the status and try again." : errorMessage(plan.error)}</p>
            <p className="mt-1">Your edits are kept.</p>
          </Notice>
        </div>
      )}
      {reviewed && <PlanReview node={node} reviewed={reviewed} apply={apply} blocked={blocked} onCancel={() => setReviewed(null)} onApplied={() => setReviewed(null)} />}
    </>
  );
}

function PublicSettings({ draft, actual, errors, onChange }: { draft: ListenerDraftSettings; actual?: PublicBindings; errors: FieldErrors; onChange: (draft: ListenerDraftSettings) => void }) {
  const name = useId();
  const http3Id = useId();
  const change = publicChange(draft, actual);
  const choose = (publicMode: ListenerDraftSettings["publicMode"]) => onChange({ ...draft, publicMode });
  return (
    <Subsection title="Public HTTP and HTTPS" description="Host addresses and ports Docker publishes to the proxy's HTTP (80) and HTTPS (443) entry points. Cove's routes and the dashboard use them.">
      <DefinitionList
        compact
        items={[
          { term: "HTTP now", value: actual ? <BindingText bindings={actual.http} /> : <span className="text-fg-muted">Not reported</span> },
          { term: "HTTPS now", value: actual ? <BindingText bindings={actual.https} /> : <span className="text-fg-muted">Not reported</span> },
        ]}
      />
      <fieldset className="min-w-0">
        <legend className="mb-1.5 text-[13px] font-medium text-fg">Public bindings</legend>
        <div className="grid gap-2 sm:grid-cols-2">
          <label className={`${choiceClass} items-start`}>
            <input type="radio" name={name} className="mt-0.5 accent-fg" checked={draft.publicMode === "retain"} onChange={() => choose("retain")} />
            <span className="min-w-0 font-medium text-fg">
              Keep current publications
              <span className="block text-xs font-normal text-fg-muted">Docker's port publications stay exactly as they are.</span>
            </span>
          </label>
          <label className={`${choiceClass} items-start`}>
            <input type="radio" name={name} className="mt-0.5 accent-fg" checked={draft.publicMode === "explicit"} onChange={() => choose("explicit")} />
            <span className="min-w-0 font-medium text-fg">
              Set explicitly
              <span className="block text-xs font-normal text-fg-muted">Choose each address and host port. Wildcard, loopback and specific addresses are allowed.</span>
            </span>
          </label>
        </div>
      </fieldset>
      {draft.publicMode === "explicit" && (
        <div className="space-y-4">
          <BindingList label="HTTP bindings" bindings={draft.http} errors={errors} listError={errors.http} emptyText="No public HTTP. Plain HTTP requests and HTTP certificate challenges stop reaching the proxy." onChange={(http) => onChange({ ...draft, http })} />
          <BindingList label="HTTPS bindings" bindings={draft.https} errors={errors} listError={errors.https} emptyText="No public HTTPS. Routed resources and the dashboard stop being reachable over HTTPS." onChange={(https) => onChange({ ...draft, https })} />
          {change.removesAll ? (
            <Notice tone="error" title="This removes every public binding">
              Nothing on this node will be reachable through the proxy's public HTTP or HTTPS, including the dashboard if it is served here. Make sure you can reach the panel another way before applying.
            </Notice>
          ) : (change.removesHttp || change.removesHttps) ? (
            <Notice tone="warn" title={`This removes public ${change.removesHttps ? "HTTPS" : "HTTP"}`}>
              Resources and the dashboard served through this proxy lose that publication. Make sure you can reach the panel another way before applying.
            </Notice>
          ) : change.changed ? (
            <Notice tone="warn" title="This changes how the proxy is reached">
              The dashboard, if served here, and every routed resource move to the bindings above. A wildcard address (0.0.0.0 or ::) overlaps every private listener on the same port.
            </Notice>
          ) : null}
        </div>
      )}
      <SwitchField
        id={http3Id}
        label="HTTP/3"
        checked={draft.http3}
        onChange={(event) => onChange({ ...draft, http3: event.target.checked })}
        aria-invalid={errors.http3 ? true : undefined}
        description={
          <>
            Also publishes UDP on every public HTTPS binding and advertises it to browsers. Needs at least one HTTPS binding, all on the same host port.
            {errors.http3 && <span className="mt-1 block text-bad">{errors.http3}</span>}
          </>
        }
      />
    </Subsection>
  );
}

function ExtraListeners({ draft, ports, errors, onChange }: { draft: ListenerDraftSettings; ports: Record<string, number> | null; errors: FieldErrors; onChange: (draft: ListenerDraftSettings) => void }) {
  const full = draft.listeners.length >= maxListeners;
  const replace = (key: string, next: ListenerDraft | null) => onChange({ ...draft, listeners: draft.listeners.flatMap((item) => (item.key === key ? (next ? [next] : []) : [item])) });
  return (
    <Subsection
      title="Extra listeners"
      description={
        <>
          Each listener becomes a Traefik entry point named <span className="font-mono">cove-listener-&lt;name&gt;</span>, so custom dynamic configuration files can route to it. TCP and UDP listeners carry raw traffic without HTTP middleware or certificates. Binding to a LAN or Tailscale address does not restrict who can connect; use a firewall or Tailscale ACLs for that. IPv6 bindings need IPv6 support on the node and in Docker.
        </>
      }
      actions={
        <Button onClick={() => onChange({ ...draft, listeners: [...draft.listeners, listenerDraft()] })} disabled={full} title={full ? `At most ${maxListeners} listeners` : undefined}>
          <Plus aria-hidden className="size-3.5" />
          Add listener
        </Button>
      }
    >
      {draft.listeners.length === 0 ? (
        <p className="text-[13px] text-fg-muted">No extra listeners.</p>
      ) : (
        <ul className="divide-y divide-line rounded-md border border-line" aria-label="Extra listeners">
          {draft.listeners.map((listener, index) => (
            <li key={listener.key} className="min-w-0 p-3">
              <ListenerFields listener={listener} index={index} port={ports?.[listener.id.trim()]} errors={errors} onChange={(next) => replace(listener.key, next)} onRemove={() => replace(listener.key, null)} />
            </li>
          ))}
        </ul>
      )}
    </Subsection>
  );
}

function ListenerFields({ listener, index, port, errors, onChange, onRemove }: { listener: ListenerDraft; index: number; port?: number; errors: FieldErrors; onChange: (listener: ListenerDraft) => void; onRemove: () => void }) {
  const id = useId();
  const name = listener.id.trim() || `listener ${index + 1}`;
  const idError = errors[`${listener.key}.id`];
  const visibilityError = errors[`${listener.key}.visibility`];
  return (
    <fieldset className="min-w-0 space-y-3">
      <legend className="sr-only">Listener {name}</legend>
      <div className="grid gap-3 sm:grid-cols-[minmax(0,1.4fr)_minmax(0,1fr)_minmax(0,1fr)]">
        <Field id={`${id}-name`} label="Name" error={idError} hint={<span className="break-all font-mono">{entrypointName(listener.id.trim() || "name")}</span>}>
          <input
            id={`${id}-name`}
            className={`${inputClass} font-mono`}
            value={listener.id}
            maxLength={32}
            autoComplete="off"
            spellCheck={false}
            autoCapitalize="none"
            onChange={(event) => onChange({ ...listener, id: event.target.value })}
            aria-invalid={idError ? true : undefined}
            aria-describedby={describedBy(`${id}-name`, idError, true)}
          />
        </Field>
        <Field id={`${id}-protocol`} label="Protocol">
          <Select id={`${id}-protocol`} value={listener.protocol} aria-labelledby={`${id}-protocol-label`} onChange={(event) => onChange({ ...listener, protocol: event.target.value as ListenerDraft["protocol"] })}>
            {Object.entries(protocolLabels).map(([value, label]) => (
              <option key={value} value={value}>
                {label}
              </option>
            ))}
          </Select>
        </Field>
        <Field id={`${id}-visibility`} label="Visibility" error={visibilityError}>
          <Select
            id={`${id}-visibility`}
            value={listener.visibility}
            aria-labelledby={`${id}-visibility-label`}
            aria-invalid={visibilityError ? true : undefined}
            aria-describedby={visibilityError ? `${id}-visibility-error` : undefined}
            onChange={(event) => onChange({ ...listener, visibility: event.target.value as ListenerDraft["visibility"] })}
          >
            {Object.entries(visibilityLabels).map(([value, label]) => (
              <option key={value} value={value}>
                {label}
              </option>
            ))}
          </Select>
        </Field>
      </div>
      <BindingList label={`Bindings for ${name}`} visibleLabel="Bindings" bindings={listener.bindings} errors={errors} listError={errors[`${listener.key}.bindings`]} onChange={(bindings) => onChange({ ...listener, bindings })} />
      <div className="flex flex-wrap items-center justify-between gap-2">
        <p className="text-xs text-fg-muted">{port ? `Internal port ${port}, assigned by Cove.` : "Cove assigns the internal port when the change is planned."}</p>
        <Button size="sm" variant="ghost" onClick={onRemove} aria-label={`Remove listener ${name}`}>
          <Trash2 aria-hidden className="size-3.5" />
          Remove listener
        </Button>
      </div>
    </fieldset>
  );
}

interface BindingListProps {
  label: string;
  visibleLabel?: string;
  bindings: BindingDraft[];
  errors: FieldErrors;
  listError?: string;
  emptyText?: string;
  onChange: (bindings: BindingDraft[]) => void;
}

function BindingList({ label, visibleLabel = label, bindings, errors, listError, emptyText, onChange }: BindingListProps) {
  const id = useId();
  const full = bindings.length >= maxBindings;
  const update = (key: string, patch: Partial<BindingDraft>) => onChange(bindings.map((item) => (item.key === key ? { ...item, ...patch } : item)));
  return (
    <fieldset className="min-w-0 space-y-2" aria-describedby={listError ? `${id}-error` : undefined}>
      <legend className="text-[13px] font-medium text-fg">
        <span aria-hidden={visibleLabel !== label || undefined}>{visibleLabel}</span>
        {visibleLabel !== label && <span className="sr-only">{label}</span>}
      </legend>
      {bindings.length === 0 && emptyText && <p className="text-xs text-fg-muted">{emptyText}</p>}
      {bindings.length > 0 && (
        <ul className="space-y-3 @sm:space-y-2">
          {bindings.map((item, index) => {
            const addressError = errors[`${item.key}.address`];
            const portError = errors[`${item.key}.port`];
            const errorId = `${id}-${item.key}-error`;
            return (
              <li key={item.key} className="min-w-0">
                <div className="grid grid-cols-[minmax(0,1fr)_auto] items-center gap-2 @sm:grid-cols-[minmax(0,1fr)_5.5rem_auto]">
                  <input
                    className={`${inputClass} col-span-2 font-mono @sm:col-span-1`}
                    value={item.address}
                    placeholder="Address, e.g. 192.168.1.20"
                    autoComplete="off"
                    spellCheck={false}
                    autoCapitalize="none"
                    aria-label={`${label}: address ${index + 1}`}
                    aria-invalid={addressError ? true : undefined}
                    aria-describedby={addressError || portError ? errorId : undefined}
                    onChange={(event) => update(item.key, { address: event.target.value })}
                  />
                  <input
                    className={`${inputClass} font-mono tabular-nums`}
                    value={item.port}
                    placeholder="Port"
                    inputMode="numeric"
                    autoComplete="off"
                    aria-label={`${label}: port ${index + 1}`}
                    aria-invalid={portError ? true : undefined}
                    aria-describedby={addressError || portError ? errorId : undefined}
                    onChange={(event) => update(item.key, { port: event.target.value })}
                  />
                  <Button variant="ghost" className="px-2" aria-label={`Remove ${label.toLowerCase()} ${index + 1}`} title="Remove binding" onClick={() => onChange(bindings.filter((other) => other.key !== item.key))}>
                    <X aria-hidden className="size-3.5" />
                  </Button>
                </div>
                {(addressError || portError) && (
                  <p id={errorId} className="mt-1 text-xs break-words text-bad">
                    {[addressError, portError].filter(Boolean).join(" ")}
                  </p>
                )}
              </li>
            );
          })}
        </ul>
      )}
      {listError && (
        <p id={`${id}-error`} className="text-xs text-bad">
          {listError}
        </p>
      )}
      <Button size="sm" onClick={() => onChange([...bindings, bindingDraft()])} disabled={full} title={full ? `At most ${maxBindings} bindings` : undefined} aria-label={`Add to ${label.toLowerCase()}`}>
        <Plus aria-hidden className="size-3.5" />
        Add binding
      </Button>
    </fieldset>
  );
}

// Stable, so the heading is focused once when the plan appears and not again on later renders.
const focusOnMount = (element: HTMLElement | null) => element?.focus();

interface ReviewProps {
  node: Node;
  reviewed: Reviewed;
  apply: Apply;
  blocked?: string;
  onCancel: () => void;
  onApplied: () => void;
}

function PlanReview({ node, reviewed, apply, blocked, onCancel, onApplied }: ReviewProps) {
  const { plan } = reviewed;
  const planned = plan.settings.public;
  return (
    <section aria-labelledby="listener-plan-title" className="space-y-3 border-t border-line p-4">
      <div className="min-w-0">
        <h3 id="listener-plan-title" tabIndex={-1} ref={focusOnMount} className="text-[13px] font-semibold text-fg">
          Review listener change
        </h3>
        <p className="mt-0.5 text-xs text-fg-muted">Planned against the proxy as it is now. Any edit, or a change to the proxy or routes on the node, makes this plan stale.</p>
      </div>
      <PlanImpact node={node} plan={plan} />
      <DefinitionList
        items={[
          { term: "Public HTTP", value: <ChangeValue current={plan.public_bindings.http} next={planned?.http} /> },
          { term: "Public HTTPS", value: <ChangeValue current={plan.public_bindings.https} next={planned?.https} /> },
          { term: "HTTP/3", value: plan.settings.http3 ? "On, UDP on every public HTTPS binding" : "Off" },
        ]}
      />
      <EntryPoints plan={plan} />
      {plan.warnings && plan.warnings.length > 0 && (
        <Notice tone="warn" title="Warnings">
          <ul className="mt-1 list-disc space-y-1 pl-4">
            {plan.warnings.map((warning) => (
              <li key={warning} className="break-words">
                {warning}
              </li>
            ))}
          </ul>
        </Notice>
      )}
      <PlanActions node={node} reviewed={reviewed} apply={apply} blocked={blocked} onCancel={onCancel} onApplied={onApplied} />
    </section>
  );
}

function PlanImpact({ node, plan }: { node: Node; plan: ListenerPlan }) {
  const affected = plan.affected_workloads ?? [];
  // The plan names resources by ID; show names for resources in the current team and IDs for the rest.
  const workloads = useQuery({ ...workloadsQuery, enabled: affected.length > 0 });
  const names = new Map(workloads.data?.map((workload) => [workload.id, workload.name]));
  if (!plan.requires_restart) {
    return (
      <Notice tone="success" title="No change to apply">
        The proxy already runs with these settings, so nothing needs to be applied.
      </Notice>
    );
  }
  return (
    <Notice tone="warn" title={`Applying interrupts every connection through the proxy on ${node.name}`}>
      <p>
        {affected.length === 0
          ? "No routed resources are on this proxy, but it is still stopped and recreated."
          : `${affected.length} routed ${affected.length === 1 ? "resource loses" : "resources lose"} connections briefly. Their containers are not replaced.`}
      </p>
      {affected.length > 0 && (
        <ul className="mt-1 flex flex-wrap gap-x-3 gap-y-0.5">
          {affected.map((id) => (
            <li key={id} className="font-mono text-xs break-all text-fg">
              {names.get(id) ?? id}
            </li>
          ))}
        </ul>
      )}
    </Notice>
  );
}

function EntryPoints({ plan }: { plan: ListenerPlan }) {
  const listeners = plan.settings.listeners;
  return (
    <div className="overflow-x-auto rounded-md border border-line">
      <table className="w-full text-left text-[13px]">
        <caption className="sr-only">Listener entry points after applying</caption>
        <thead>
          <tr className={tableHeadClass}>
            <th scope="col" className="px-3">
              Entry point
            </th>
            <th scope="col" className="px-3">
              Transport
            </th>
            <th scope="col" className="hidden px-3 sm:table-cell">
              Bindings
            </th>
            <th scope="col" className="px-3 text-right">
              Internal port
            </th>
          </tr>
        </thead>
        <tbody>
          {listeners.length === 0 && (
            <tr className={tableRowClass}>
              <td colSpan={4} className="px-3 py-2 text-fg-muted">
                No extra listeners.
              </td>
            </tr>
          )}
          {listeners.map((listener) => (
            <tr key={listener.id} className={tableRowClass}>
              <td className="px-3 py-2 align-top">
                <span className="font-mono text-xs break-all text-fg">{entrypointName(listener.id)}</span>
                <p className="mt-0.5 sm:hidden">
                  <BindingText bindings={listener.bindings} />
                </p>
              </td>
              <td className="px-3 py-2 align-top whitespace-nowrap text-fg-muted">
                {protocolLabels[listener.protocol] ?? listener.protocol} · {visibilityLabels[listener.visibility] ?? listener.visibility}
              </td>
              <td className="hidden px-3 py-2 align-top sm:table-cell">
                <BindingText bindings={listener.bindings} />
              </td>
              <td className="px-3 py-2 text-right align-top font-mono text-xs tabular-nums">{plan.ports?.[listener.id] ?? "—"}</td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}

function PlanActions({ node, reviewed, apply, blocked, onCancel, onApplied }: ReviewProps) {
  const [confirming, setConfirming] = useState(false);
  const restart = reviewed.plan.requires_restart;
  const affected = reviewed.plan.affected_workloads?.length ?? 0;
  return (
    <div className="flex flex-wrap items-center justify-end gap-2">
      {apply.isPending && (
        <p role="status" className="mr-auto flex items-center gap-2 text-xs text-fg-muted">
          <Spinner />
          Validating, then recreating the proxy. This can take a few minutes.
        </p>
      )}
      <Button variant="ghost" onClick={onCancel} disabled={apply.isPending}>
        {restart ? "Cancel" : "Close"}
      </Button>
      {restart && (
        <Button variant="primary" onClick={() => setConfirming(true)} disabled={!!blocked} pending={apply.isPending}>
          Apply and restart proxy
        </Button>
      )}
      <ConfirmDialog
        open={confirming}
        title={`Restart the proxy on ${node.name}?`}
        confirmLabel="Interrupt connections and apply"
        danger
        pending={apply.isPending}
        onCancel={() => setConfirming(false)}
        onConfirm={() =>
          apply.mutate(reviewed, {
            onSettled: () => {
              setConfirming(false);
              onApplied();
            },
          })
        }
      >
        The proxy stops briefly and every open connection through it drops, including the dashboard if it is served through this proxy.
        {affected > 0 && ` ${affected} routed ${affected === 1 ? "resource is" : "resources are"} affected.`} Applications keep running. If the new proxy cannot be committed, Cove restores the previous one.
      </ConfirmDialog>
    </div>
  );
}

function ChangeValue({ current, next }: { current: ListenerBinding[]; next?: ListenerBinding[] }) {
  if (!next) {
    return (
      <span>
        <BindingText bindings={current} />
        <span className="text-xs text-fg-muted"> · kept as is</span>
      </span>
    );
  }
  const same = current.map(formatBinding).sort().join(",") === next.map(formatBinding).sort().join(",");
  if (same) {
    return (
      <span>
        <BindingText bindings={next} />
        <span className="text-xs text-fg-muted"> · unchanged</span>
      </span>
    );
  }
  return (
    <span className="block space-y-0.5">
      <span className="block">
        <span className="text-xs text-fg-muted">Now </span>
        <BindingText bindings={current} />
      </span>
      <span className="block">
        <span className="text-xs text-fg-muted">After </span>
        <BindingText bindings={next} />
      </span>
    </span>
  );
}

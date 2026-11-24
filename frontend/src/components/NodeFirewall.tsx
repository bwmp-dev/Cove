import { useIsMutating, useQuery } from "@tanstack/react-query";
import { Eye, Pencil, Plus, RotateCcw, RotateCw, Search, Trash2 } from "lucide-react";
import { useId, useRef, useState, type FormEvent } from "react";
import { errorMessage } from "../api/client";
import { firewallMutationKey, firewallQuery, useApplyFirewall, usePreviewFirewall, useRevertFirewall, type FirewallPort, type FirewallResult, type FirewallRuleInput } from "../api/firewall";
import type { Node } from "../api/types";
import { definiteRefusal, draftsFromRules, firewallBlocker, firewallState, maxFirewallRules, portKey, portLabel, removedPorts, selectablePorts, validateDrafts, type RuleDraft } from "../lib/firewall";
import { formatAbsolute } from "../lib/format";
import { useNow, useOnline } from "../lib/hooks";
import { describedBy, inputClass } from "../lib/styles";
import { ConfirmDialog, Dialog } from "./ConfirmDialog";
import { Select } from "./Select";
import { StatusLabel } from "./status";
import { Button, Field, LoadingRows, Notice, QueryError, Section, Subsection } from "./ui";

const description = "Cove's own allow-list for application ports that Cove resources publish and for ports of the shared proxy's extra listeners on this host. Inspecting reads the host without changing it.";

const scope =
  "Cove changes only its own nftables table for currently published Cove application ports. UFW, other nftables tables, default policies, SSH, the panel port and ports 80 and 443 are never changed. Existing connections stay open, and other firewall rules on the host can still deny traffic.";

const sourcesHint = "One IPv4 or IPv6 CIDR prefix per line, such as 192.0.2.0/24 or 2001:db8::/32. Leave empty to deny new connections from everywhere.";

type PreviewSnapshot = {
  rules: FirewallRuleInput[];
  revision: string;
  notice: string;
};
type Outcome = { kind: "apply" | "revert"; error?: unknown };

function Sources({ rule }: { rule: FirewallRuleInput }) {
  if (rule.sources.length === 0) return <span className="text-warn">Denies all new connections</span>;
  return (
    <ul className="space-y-0.5 font-mono text-xs break-all text-fg">
      {rule.sources.map((source) => (
        <li key={source}>{source}</li>
      ))}
    </ul>
  );
}

function RuleList({ rules, label }: { rules: FirewallRuleInput[]; label: string }) {
  return (
    <ul aria-label={label} className="divide-y divide-line rounded-md border border-line">
      {rules.map((rule) => (
        <li key={portKey(rule)} className="grid gap-x-4 gap-y-1 px-3 py-2 text-[13px] sm:grid-cols-[7rem_minmax(0,1fr)]">
          <span className="font-mono text-xs text-fg sm:pt-px">{portLabel(rule)}</span>
          <div className="min-w-0">
            <span className="sr-only">Allowed sources: </span>
            <Sources rule={rule} />
          </div>
        </li>
      ))}
    </ul>
  );
}

function PortList({ ports, empty }: { ports: string[]; empty: string }) {
  if (ports.length === 0) return <p className="text-xs text-fg-muted">{empty}</p>;
  return (
    <ul className="flex flex-wrap gap-x-3 gap-y-1 font-mono text-xs text-fg">
      {ports.map((port) => (
        <li key={port}>{port}</li>
      ))}
    </ul>
  );
}

function HostOutput({ label, text }: { label: string; text: string }) {
  return (
    <details className="text-xs">
      <summary className="w-fit cursor-pointer rounded text-fg-muted hover:text-fg">{label}</summary>
      {/* Focusable so the bounded output can be scrolled with the keyboard in every browser. */}
      <div role="region" aria-label={label} tabIndex={0} className="mt-2 max-h-80 overflow-auto rounded-md border border-line bg-panel px-3 py-2">
        <pre className="font-mono text-xs whitespace-pre text-fg">{text || "No output."}</pre>
      </div>
    </details>
  );
}

function PendingChange({ deadline, disabled, onRevert }: { deadline: string; disabled: boolean; onRevert: () => void }) {
  const now = useNow(15_000);
  const passed = Date.parse(deadline) <= now;
  return (
    <Notice
      tone="warn"
      title="Unconfirmed firewall change"
      action={
        <Button size="sm" variant="danger" disabled={disabled} onClick={onRevert}>
          <RotateCcw aria-hidden className="size-3.5" />
          Revert change
        </Button>
      }
    >
      <p>
        The host applied a change that the panel has not confirmed. Cove's independent timer on the host reverts it by {formatAbsolute(deadline)} unless it is confirmed. This page does not count down;
        inspect again to see the current state.
      </p>
      {passed && <p className="mt-1">That deadline has passed. Inspect again to check that the host reverted the change.</p>}
      <p className="mt-1">Editing is unavailable until the change is confirmed or reverted.</p>
    </Notice>
  );
}

function OutcomeNotice({ outcome }: { outcome: Outcome }) {
  const action = outcome.kind === "apply" ? "firewall change" : "revert";
  if (outcome.error === undefined) {
    if (outcome.kind === "revert") {
      return (
        <Notice tone="success" title="Unconfirmed change reverted">
          The previous Cove policy was restored. The status above was inspected again.
        </Notice>
      );
    }
    return (
      <Notice tone="success" title="Firewall change confirmed">
        <p>The panel reached the worker over a fresh connection, so the host kept the change. Cove's protected boot service restores this policy when the host restarts.</p>
        <p className="mt-1">If a new management port is discovered later, rules for that port are removed during recovery so it is never restricted.</p>
      </Notice>
    );
  }
  if (definiteRefusal(outcome.error)) {
    return (
      <Notice tone="error" title={`The ${action} was refused`}>
        {errorMessage(outcome.error)}
      </Notice>
    );
  }
  return (
    <Notice tone="error" title={`The ${action} could not be confirmed`}>
      <p>{errorMessage(outcome.error)}</p>
      <p className="mt-1">It may or may not have been applied. The status above was inspected again; check it for an unconfirmed change before trying again. Cove does not retry automatically.</p>
    </Notice>
  );
}

function RuleRow({
  draft,
  ports,
  errors,
  disabled,
  onChange,
  onRemove,
}: {
  draft: RuleDraft;
  ports: FirewallPort[];
  errors: { port?: string; sources?: string } | undefined;
  disabled: boolean;
  onChange: (draft: RuleDraft) => void;
  onRemove: () => void;
}) {
  const id = useId();
  const listed = ports.some((port) => portKey(port) === draft.port);
  const label = draft.port ? draft.port.replace("/", " ").toUpperCase() : "new rule";
  return (
    <li className="grid gap-3 p-3 sm:grid-cols-[12rem_minmax(0,1fr)_auto]">
      <Field id={`${id}-port`} label="Port" error={errors?.port}>
        <Select
          id={`${id}-port`}
          value={draft.port}
          disabled={disabled}
          aria-invalid={errors?.port ? true : undefined}
          aria-describedby={describedBy(`${id}-port`, errors?.port)}
          onChange={(event) => onChange({ ...draft, port: event.target.value })}
        >
          <option value="">Choose a port</option>
          {!listed && draft.port && <option value={draft.port}>{`${label} (no longer published)`}</option>}
          {ports.map((port) => (
            <option key={portKey(port)} value={portKey(port)}>
              {portLabel(port)}
            </option>
          ))}
        </Select>
      </Field>
      <Field id={`${id}-sources`} label="Allowed sources" hint={sourcesHint} error={errors?.sources}>
        <textarea
          id={`${id}-sources`}
          className={`${inputClass} font-mono`}
          rows={Math.min(6, Math.max(2, draft.sources.split("\n").length))}
          value={draft.sources}
          disabled={disabled}
          spellCheck={false}
          autoComplete="off"
          aria-invalid={errors?.sources ? true : undefined}
          aria-describedby={describedBy(`${id}-sources`, errors?.sources, sourcesHint)}
          onChange={(event) => onChange({ ...draft, sources: event.target.value })}
        />
      </Field>
      <div className="sm:pt-6">
        <Button variant="ghost" size="sm" disabled={disabled} onClick={onRemove} aria-label={`Remove rule for ${label}`} title={`Remove rule for ${label}`}>
          <Trash2 aria-hidden className="size-3.5" />
          <span className="sm:hidden">Remove</span>
        </Button>
      </div>
    </li>
  );
}

function ApplyDialog({
  node,
  current,
  preview,
  online,
  onCancel,
  onApply,
}: {
  node: Node;
  current: FirewallRuleInput[];
  preview: PreviewSnapshot;
  online: boolean;
  onCancel: () => void;
  onApply: (preview: PreviewSnapshot) => void;
}) {
  const id = useId();
  const [typed, setTyped] = useState("");
  const removed = removedPorts(current, preview.rules);
  const confirmed = typed === node.name;
  function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    if (!confirmed || !online) return;
    onApply(preview);
  }
  return (
    <Dialog open title="Apply this firewall policy?" onClose={onCancel}>
      <form onSubmit={submit} className="space-y-4">
        <div className="space-y-2">
          <p className="font-medium text-fg">Policy after the change</p>
          {preview.rules.length === 0 ? (
            <p>No Cove rules. Cove will not restrict any published application port.</p>
          ) : (
            <div className="max-h-64 overflow-auto">
              <RuleList rules={preview.rules} label="Previewed Cove rules" />
            </div>
          )}
        </div>
        {removed.length > 0 && (
          <div className="space-y-1">
            <p className="font-medium text-fg">Rules removed</p>
            <p>
              Cove will stop restricting <span className="font-mono text-xs text-fg">{removed.map(portLabel).join(", ")}</span>. Other firewall rules on the host still apply.
            </p>
          </div>
        )}
        {preview.notice && <Notice tone="warn">{preview.notice}</Notice>}
        <p>
          The host arms an independent two-minute revert before applying. The panel then checks a fresh connection to the worker and keeps the change only if that succeeds. Existing connections stay
          open; UFW, other tables, default policies and management ports are not changed.
        </p>
        <Field id={id} label={`Type ${node.name} to confirm`}>
          <input id={id} className={`${inputClass} font-mono`} value={typed} onChange={(event) => setTyped(event.target.value)} autoComplete="off" spellCheck={false} required />
        </Field>
        {!online && <Notice tone="warn">You are offline. Reconnect to apply the change.</Notice>}
        <div className="flex flex-wrap justify-end gap-2">
          <Button variant="ghost" onClick={onCancel}>
            Cancel
          </Button>
          <Button type="submit" variant="primary" disabled={!confirmed || !online}>
            Apply firewall change
          </Button>
        </div>
      </form>
    </Dialog>
  );
}

function RuleEditor({
  node,
  current,
  ports,
  online,
  onCancel,
  onApply,
}: {
  node: Node;
  current: FirewallRuleInput[];
  ports: FirewallPort[];
  online: boolean;
  onCancel: () => void;
  onApply: (preview: PreviewSnapshot) => void;
}) {
  const preview = usePreviewFirewall(node.id);
  const [initial] = useState(() => draftsFromRules(current));
  const [drafts, setDrafts] = useState(initial);
  const nextId = useRef(initial.length);
  const [attempted, setAttempted] = useState(false);
  const [snapshot, setSnapshot] = useState<PreviewSnapshot | null>(null);
  const validation = validateDrafts(drafts, ports);
  const unchanged = JSON.stringify(drafts.map(({ port, sources }) => [port, sources])) === JSON.stringify(initial.map(({ port, sources }) => [port, sources]));
  const used = new Set(drafts.map((draft) => draft.port));
  const free = ports.find((port) => !used.has(portKey(port)));

  // Any edit discards an earlier preview, so only the exact previewed policy can be applied.
  function edit(next: RuleDraft[]) {
    setDrafts(next);
    setSnapshot(null);
    preview.reset();
  }

  function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    setAttempted(true);
    if (!validation.rules || unchanged || preview.isPending) return;
    preview.mutate(validation.rules, {
      onSuccess: (result: FirewallResult) =>
        setSnapshot({
          rules: firewallState(result).rules,
          revision: result.revision,
          notice: result.notice,
        }),
    });
  }

  return (
    <>
      <form onSubmit={submit} className="space-y-3" aria-label="Edit Cove firewall rules">
        {drafts.length === 0 ? (
          <p className="text-[13px] text-fg-muted">No rules. Applying an empty policy removes Cove's restrictions from every published port.</p>
        ) : (
          <ul className="divide-y divide-line rounded-md border border-line">
            {drafts.map((draft) => (
              <RuleRow
                key={draft.id}
                draft={draft}
                ports={ports}
                errors={attempted ? validation.errors.get(draft.id) : undefined}
                disabled={preview.isPending}
                onChange={(changed) => edit(drafts.map((item) => (item.id === changed.id ? changed : item)))}
                onRemove={() => edit(drafts.filter((item) => item.id !== draft.id))}
              />
            ))}
          </ul>
        )}
        {validation.formError && <p className="text-xs text-bad">{validation.formError}</p>}
        {preview.isError && (
          <Notice tone="error" title="Could not preview the change">
            {errorMessage(preview.error)}
          </Notice>
        )}
        <div className="flex flex-wrap items-center justify-between gap-2">
          <Button
            disabled={preview.isPending || drafts.length >= maxFirewallRules || !free}
            onClick={() => {
              if (!free) return;
              edit([...drafts, { id: nextId.current, port: portKey(free), sources: "" }]);
              nextId.current += 1;
            }}
          >
            <Plus aria-hidden className="size-3.5" />
            Add rule
          </Button>
          <div className="flex flex-wrap gap-2">
            <Button variant="ghost" onClick={onCancel} disabled={preview.isPending}>
              Cancel
            </Button>
            <Button type="submit" variant="primary" pending={preview.isPending} disabled={unchanged || !online}>
              {!preview.isPending && <Eye aria-hidden className="size-3.5" />}
              Preview change
            </Button>
          </div>
        </div>
        {unchanged && <p className="text-right text-xs text-fg-muted">Change a rule to preview it.</p>}
      </form>
      {snapshot && (
        <ApplyDialog
          node={node}
          current={current}
          preview={snapshot}
          online={online}
          onCancel={() => {
            setSnapshot(null);
            preview.reset();
          }}
          onApply={onApply}
        />
      )}
    </>
  );
}

function StatusNotices({
  result,
  apply,
  revert,
  outcome,
  revertDisabled,
  onRevert,
}: {
  result: FirewallResult;
  apply: boolean;
  revert: boolean;
  outcome: Outcome | null;
  revertDisabled: boolean;
  onRevert: () => void;
}) {
  const busy = apply || revert;
  return (
    // Always mounted so changes are announced; it collapses when it has nothing to show.
    <div className="space-y-2 p-4 empty:hidden" aria-live="polite">
      {!result.supported && (
        <Notice tone="warn" title="Firewall changes are unavailable on this host">
          {result.notice || "The host does not meet the prerequisites for Cove's firewall management."} Current rules and host output are shown read-only.
        </Notice>
      )}
      {result.pending && !busy && <PendingChange deadline={result.pending.deadline} disabled={revertDisabled} onRevert={onRevert} />}
      {apply && (
        <Notice title="Applying the firewall change">
          The host is applying the change and the panel is checking a fresh connection to the worker. If the panel cannot confirm it, the host reverts the change by itself within two minutes.
        </Notice>
      )}
      {revert && <Notice title="Reverting the unconfirmed change">Waiting for the host to restore the previous policy.</Notice>}
      {outcome && !busy && <OutcomeNotice outcome={outcome} />}
      {result.last_error && !result.pending && (
        <Notice tone="warn" title="Last recovery note">
          <span className="break-words">{result.last_error}</span>
        </Notice>
      )}
    </div>
  );
}

function HostInventory({ result }: { result: FirewallResult }) {
  const { available, reserved } = firewallState(result);
  const protectedPorts = new Set(reserved);
  return (
    <>
      <div className="grid gap-0 divide-y divide-line md:grid-cols-2 md:divide-x md:divide-y-0">
        <Subsection title="Published application ports" description="Host ports published by running Cove resources and by the shared proxy's extra listeners. Protected ports cannot be restricted.">
          <ul className="flex flex-wrap gap-x-4 gap-y-1 text-xs">
            {available.length === 0 && <li className="text-fg-muted">None found.</li>}
            {available.map((port) => (
              <li key={portKey(port)} className="font-mono text-fg">
                {portLabel(port)}
                {protectedPorts.has(port.port) && <span className="ml-1.5 font-sans text-fg-muted">(protected)</span>}
              </li>
            ))}
          </ul>
        </Subsection>
        <Subsection title="Protected management ports" description="SSH, bastion, panel and proxy ports. Cove never restricts them.">
          <PortList ports={reserved.map(String)} empty="Not reported. Firewall changes stay unavailable until the host reports its management ports." />
        </Subsection>
      </div>
      <Subsection title="Host firewall state" description="Read-only output from the host. Cove does not change UFW or other nftables tables.">
        <div className="space-y-2">
          <HostOutput label="UFW status" text={result.ufw} />
          <HostOutput label="nftables ruleset" text={result.nftables} />
        </div>
        {result.limited && <p className="text-xs text-warn">Host output was clipped at the inspection limit or could not be read completely.</p>}
      </Subsection>
    </>
  );
}

function CurrentRules({ rules, pending, nothingToRestrict }: { rules: FirewallRuleInput[]; pending: boolean; nothingToRestrict: boolean }) {
  if (rules.length > 0) return <RuleList rules={rules} label={pending ? "Unconfirmed Cove rules" : "Current Cove rules"} />;
  return (
    <>
      <p className="text-[13px] text-fg-muted">No Cove rules. Cove does not restrict any published application port on this host.</p>
      {nothingToRestrict && !pending && <p className="text-xs text-fg-muted">No Cove resource publishes a host port that can be restricted. Rules become available after a resource publishes one.</p>}
    </>
  );
}

function Inspection({ node, result, blocker, online, refresh }: { node: Node; result: FirewallResult; blocker: string | undefined; online: boolean; refresh: () => void }) {
  const apply = useApplyFirewall(node.id);
  const revert = useRevertFirewall(node.id);
  const [editing, setEditing] = useState(false);
  const [reverting, setReverting] = useState(false);
  const [outcome, setOutcome] = useState<Outcome | null>(null);
  const { rules, available, reserved } = firewallState(result);
  const ports = selectablePorts(available, reserved);
  const busy = apply.isPending || revert.isPending;
  const pending = result.pending;
  const editBlocked = blocker !== undefined || !result.supported || pending !== undefined || busy || (ports.length === 0 && rules.length === 0);

  function settle(kind: Outcome["kind"]) {
    return {
      onSuccess: () => setOutcome({ kind }),
      onError: (error: unknown) => setOutcome({ kind, error }),
      onSettled: refresh,
    };
  }

  return (
    <div className="divide-y divide-line">
      <StatusNotices result={result} apply={apply.isPending} revert={revert.isPending} outcome={outcome} revertDisabled={blocker !== undefined} onRevert={() => setReverting(true)} />
      <Subsection
        title="Cove application-port rules"
        description="Each rule restricts new connections to one published port to the listed sources. Ports without a rule are not restricted by Cove."
        actions={
          !editing && (
            <Button
              disabled={editBlocked}
              onClick={() => {
                setOutcome(null);
                setEditing(true);
              }}
            >
              <Pencil aria-hidden className="size-3.5" />
              Edit rules
            </Button>
          )
        }
      >
        <p className="text-xs text-fg-muted">Cove table on the host: {result.active ? <StatusLabel tone="ok" label="Present" /> : <StatusLabel tone="neutral" label="Not present" />}</p>
        {editing ? (
          <RuleEditor
            node={node}
            current={rules}
            ports={ports}
            online={online}
            onCancel={() => setEditing(false)}
            onApply={(preview) => {
              setEditing(false);
              setOutcome(null);
              apply.mutate({ rules: preview.rules, revision: preview.revision }, settle("apply"));
            }}
          />
        ) : (
          <CurrentRules rules={rules} pending={pending !== undefined} nothingToRestrict={result.supported && ports.length === 0} />
        )}
      </Subsection>
      <HostInventory result={result} />
      <ConfirmDialog
        open={reverting}
        title="Revert the unconfirmed firewall change?"
        confirmLabel="Revert change"
        danger
        onCancel={() => setReverting(false)}
        onConfirm={() => {
          if (!pending || busy || blocker !== undefined) return;
          setReverting(false);
          setOutcome(null);
          revert.mutate(undefined, settle("revert"));
        }}
      >
        Cove restores the previous application-port policy and stops the timed recovery. The status is inspected again afterwards.
      </ConfirmDialog>
    </div>
  );
}

export function NodeFirewall({ node }: { node: Node }) {
  const online = useOnline();
  const status = useQuery(firewallQuery(node.id));
  // The worker serializes firewall requests, so inspecting while a preview, apply or revert runs would only be refused.
  const changing = useIsMutating({ mutationKey: firewallMutationKey(node.id) }) > 0;
  const blocker = firewallBlocker(node, online);
  const inspected = status.data !== undefined || status.isError;
  const refresh = () => void status.refetch();
  // A manual inspection starts a fresh view: earlier outcomes and unsent edits describe a state that may no longer exist.
  const [inspection, setInspection] = useState(0);
  const inspect = () => {
    setInspection(inspection + 1);
    refresh();
  };

  let body;
  if (status.isFetching && !status.data) {
    body = <LoadingRows label="Inspecting the host firewall" rows={3} />;
  } else if (status.data) {
    body = (
      <>
        {status.isError && (
          <div className="border-b border-line p-4">
            <QueryError what="the latest firewall status" error={status.error} onRetry={refresh} retrying={status.isFetching} />
          </div>
        )}
        <Inspection key={inspection} node={node} result={status.data} blocker={blocker} online={online} refresh={refresh} />
      </>
    );
  } else if (status.isError) {
    body = (
      <div className="p-4">
        <QueryError what="the firewall status" error={status.error} onRetry={refresh} retrying={status.isFetching} />
      </div>
    );
  } else if (!blocker) {
    body = (
      <p className="p-4 text-[13px] text-fg-muted">
        Not inspected yet. Inspecting reads Cove's rules, published ports, protected ports and the host's UFW and nftables output without changing anything.
      </p>
    );
  }

  return (
    <Section
      title="Host firewall"
      description={description}
      flush
      actions={
        <Button onClick={inspect} pending={status.isFetching} disabled={blocker !== undefined || changing}>
          {!status.isFetching && (inspected ? <RotateCw aria-hidden className="size-3.5" /> : <Search aria-hidden className="size-3.5" />)}
          {inspected ? "Inspect again" : "Inspect"}
        </Button>
      }
    >
      {blocker && <p className={`px-4 py-3 text-[13px] text-fg-muted ${body ? "border-b border-line" : ""}`}>{blocker}</p>}
      {body}
      <p className="border-t border-line px-4 py-3 text-xs text-fg-muted">{scope}</p>
    </Section>
  );
}

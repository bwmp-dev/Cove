import { useQuery } from "@tanstack/react-query";
import { useId, useState, type FormEvent } from "react";
import { cleanupLimits, cleanupPolicyQuery, usePreviewCleanup, useRunCleanup, useSaveCleanupPolicy, type CleanupItemKind, type CleanupPlan, type CleanupPolicy, type CleanupState } from "../api/cleanup";
import { errorMessage, isStatus } from "../api/client";
import type { Node } from "../api/types";
import { formatAbsolute } from "../lib/format";
import { useOnline } from "../lib/hooks";
import { inputClass, tableHeadClass, tableRowClass } from "../lib/styles";
import { formatBinaryBytes } from "../lib/telemetry";
import { Dialog } from "./ConfirmDialog";
import { RelativeTime } from "./RelativeTime";
import { SaveBar, UnsavedChangesGuard } from "./SaveBar";
import { Button, DefinitionList, Field, LoadingRows, Notice, QueryError, Section, SettingRows, Subsection, SwitchField } from "./ui";

const title = "Docker cleanup";
const description = "Removes unused images, Cove build cache and, only when allowed, unused volumes. Stopped containers are never removed and nothing is force-removed. Retained release images, volumes of recorded resources and Cove's ingress, link and builder storage are always kept.";

const kindLabels: Record<CleanupItemKind, [string, string]> = {
  image: ["image", "images"],
  volume: ["volume", "volumes"],
  cache: ["build cache record", "build cache records"],
};

function count(value: number, kind: CleanupItemKind) {
  return `${value.toLocaleString("en")} ${kindLabels[kind][value === 1 ? 0 : 1]}`;
}

function planSummary(plan: CleanupPlan) {
  const counts: Record<CleanupItemKind, number> = { image: 0, volume: 0, cache: 0 };
  for (const item of plan.items) counts[item.kind]++;
  return { counts, text: (["image", "cache", "volume"] as const).filter((kind) => counts[kind] > 0).map((kind) => count(counts[kind], kind)).join(", ") };
}

function estimate(plan: CleanupPlan) {
  const unknown = plan.items.filter((item) => item.bytes === null).length;
  const known = `About ${formatBinaryBytes(plan.estimated_bytes)}`;
  return unknown > 0 ? `${known} from items with a reported size; ${unknown.toLocaleString("en")} ${unknown === 1 ? "item has" : "items have"} an unknown size` : known;
}

function draftFrom(policy: CleanupPolicy) {
  return { ...policy, interval_hours: String(policy.interval_hours), minimum_age_hours: String(policy.minimum_age_hours) };
}

type PolicyDraft = ReturnType<typeof draftFrom>;
type HoursField = keyof typeof cleanupLimits;

function parseHours(draft: PolicyDraft) {
  const errors: Partial<Record<HoursField, string>> = {};
  const values: Partial<Record<HoursField, number>> = {};
  for (const key of Object.keys(cleanupLimits) as HoursField[]) {
    const { min, max } = cleanupLimits[key];
    const text = draft[key].trim();
    const number = Number(text);
    if (!/^\d+$/.test(text) || number < min || number > max) errors[key] = `Enter a whole number of hours from ${min.toLocaleString("en")} to ${max.toLocaleString("en")}.`;
    else values[key] = number;
  }
  if (values.interval_hours === undefined || values.minimum_age_hours === undefined) return { errors };
  const policy: CleanupPolicy = { ...draft, interval_hours: values.interval_hours, minimum_age_hours: values.minimum_age_hours };
  return { errors, policy };
}

function samePolicy(a: PolicyDraft, b: PolicyDraft) {
  return (Object.keys(a) as (keyof PolicyDraft)[]).every((key) => a[key] === b[key]);
}

function LastRun({ state }: { state: CleanupState }) {
  return (
    <Subsection title="Last run">
      <DefinitionList
        compact
        items={[
          { term: "Finished", value: state.last_run ? <RelativeTime value={state.last_run} /> : "Never" },
          { term: "Removed", value: <span className="tabular-nums">{state.removed.toLocaleString("en")} items</span> },
          { term: "Estimated space", value: <span className="tabular-nums">About {formatBinaryBytes(state.estimated_bytes)}</span> },
        ]}
      />
      {state.last_error && <Notice tone="warn" title="The last run stopped early">{state.last_error}</Notice>}
      <p className="text-xs text-fg-muted">Space estimates come from Docker's reported sizes. Shared layers and cache mean the space actually freed on disk may be smaller.</p>
    </Subsection>
  );
}

function PolicyForm({ nodeId, policy, onDirty, onSaved }: { nodeId: string; policy: CleanupPolicy; onDirty: (dirty: boolean) => void; onSaved: () => void }) {
  const id = useId();
  const online = useOnline();
  const save = useSaveCleanupPolicy(nodeId);
  const initial = draftFrom(policy);
  const [draft, setDraft] = useState<PolicyDraft>(initial);
  const [errors, setErrors] = useState<Partial<Record<HoursField, string>>>({});
  const [confirm, setConfirm] = useState<CleanupPolicy | null>(null);
  const dirty = !samePolicy(draft, initial);

  const change = (next: PolicyDraft) => {
    setDraft(next);
    save.reset();
    onDirty(!samePolicy(next, initial));
  };

  const commit = (next: CleanupPolicy) => {
    save.mutate(next, {
      onSuccess: (envelope) => {
        setDraft(draftFrom(envelope.state.policy));
        setConfirm(null);
        onDirty(false);
        onSaved();
      },
      onError: () => setConfirm(null),
    });
  };

  const submit = (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    const parsed = parseHours(draft);
    setErrors(parsed.errors);
    if (!parsed.policy) return;
    const broadened = (parsed.policy.volumes && !policy.volumes) || (parsed.policy.non_cove && !policy.non_cove);
    if (broadened) setConfirm(parsed.policy);
    else commit(parsed.policy);
  };

  const hours = (key: HoursField, label: string, hint: string) => {
    const fieldId = `${id}-${key}`;
    return (
      <Field id={fieldId} label={label} hint={`${hint} ${cleanupLimits[key].min.toLocaleString("en")} to ${cleanupLimits[key].max.toLocaleString("en")} hours.`} error={errors[key]} layout="row">
        <input
          id={fieldId}
          type="number"
          inputMode="numeric"
          min={cleanupLimits[key].min}
          max={cleanupLimits[key].max}
          step={1}
          className={`${inputClass} tabular-nums`}
          value={draft[key]}
          onChange={(event) => change({ ...draft, [key]: event.target.value })}
          aria-invalid={errors[key] ? true : undefined}
          aria-describedby={errors[key] ? `${fieldId}-error` : `${fieldId}-hint`}
        />
      </Field>
    );
  };

  const toggle = (key: "enabled" | "images" | "build_cache" | "volumes" | "non_cove", label: string, text: string) => (
    <SwitchField id={`${id}-${key}`} layout="row" label={label} description={text} checked={draft[key]} onChange={(event) => change({ ...draft, [key]: event.target.checked })} />
  );

  return (
    <form onSubmit={submit} noValidate>
      <UnsavedChangesGuard dirty={dirty && !save.isPending} />
      <Subsection title="Policy" description="The schedule only controls automatic runs. Previews and manual runs below always use the saved policy.">
        <fieldset disabled={save.isPending} className="min-w-0 space-y-4">
          <SettingRows>
            {toggle("enabled", "Run on a schedule", "Apply the saved policy automatically.")}
            {hours("interval_hours", "Run every", "How often scheduled cleanup runs.")}
            {hours("minimum_age_hours", "Minimum age", "Only resources unused for at least this long are eligible.")}
            {toggle("images", "Unused images", "Images no container uses and no retained release needs.")}
            {toggle("build_cache", "Build cache", "Reclaimable records in Cove's private builder cache only.")}
            {toggle("volumes", "Unused volumes", "Deletes the data stored in volumes that no container uses and no recorded resource owns. This cannot be undone.")}
            {toggle("non_cove", "Include resources Cove did not create", "Widens image and volume selection to other tools on this host. Build cache stays Cove-only.")}
          </SettingRows>
          {draft.volumes && <Notice tone="warn" title="Volume cleanup is on">Volumes not attached to a container and not owned by a recorded resource are deleted with their data.</Notice>}
          {save.isError && <Notice tone="error" title="Could not save the cleanup policy">{errorMessage(save.error)}</Notice>}
        </fieldset>
        <SaveBar
          dirty={dirty}
          pending={save.isPending}
          blocked={!online}
          label="Save policy"
          onDiscard={() => {
            setDraft(initial);
            setErrors({});
            save.reset();
            onDirty(false);
          }}
          status={save.isSuccess ? "Policy saved." : undefined}
        />
      </Subsection>
      <Dialog
        open={confirm !== null}
        title="Allow cleanup to remove more?"
        onClose={() => setConfirm(null)}
        footer={
          <>
            <Button variant="ghost" onClick={() => setConfirm(null)} disabled={save.isPending}>Cancel</Button>
            <Button variant="danger" pending={save.isPending} disabled={!online} onClick={() => confirm && commit(confirm)}>Save policy</Button>
          </>
        }
      >
        <ul className="list-disc space-y-1.5 pl-4">
          {confirm?.volumes && !policy.volumes && <li>Unused volumes will be deleted with their data by scheduled and manual runs. Deleted volume data cannot be recovered.</li>}
          {confirm?.non_cove && !policy.non_cove && <li>Images{confirm.volumes ? " and volumes" : ""} created by other tools on this host become eligible when unused.</li>}
        </ul>
        {confirm && !confirm.enabled && <p className="mt-3">The schedule is off, so this only affects manual runs until it is turned on.</p>}
      </Dialog>
    </form>
  );
}

function PlanTable({ plan }: { plan: CleanupPlan }) {
  return (
    <div className="max-h-80 overflow-y-auto rounded-md border border-line" tabIndex={0} role="region" aria-label="Items in the cleanup preview">
      <table className="w-full table-fixed text-left">
        <caption className="sr-only">Items the cleanup would remove</caption>
        <thead>
          <tr className={tableHeadClass}>
            <th scope="col" className="w-28 py-2 pr-3 pl-3 font-medium">Type</th>
            <th scope="col" className="py-2 pr-3 font-medium">ID</th>
            <th scope="col" className="w-24 py-2 pr-3 text-right font-medium">Size</th>
          </tr>
        </thead>
        <tbody>
          {plan.items.map((item) => (
            <tr key={`${item.kind}:${item.id}`} className={tableRowClass}>
              <td className={`py-2 pr-3 pl-3 align-top text-xs ${item.kind === "volume" ? "text-warn" : "text-fg-muted"}`}>{kindLabels[item.kind][0].replace(/^./, (letter) => letter.toUpperCase())}</td>
              <td className="py-2 pr-3 align-top font-mono text-xs break-all text-fg">{item.id}</td>
              <td className="py-2 pr-3 text-right align-top text-xs text-fg-muted tabular-nums">{item.bytes === null ? "Unknown" : formatBinaryBytes(item.bytes)}</td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}

function RunDialog({ node, plan, open, pending, onCancel, onConfirm }: { node: Node; plan: CleanupPlan; open: boolean; pending: boolean; onCancel: () => void; onConfirm: () => void }) {
  const id = useId();
  const online = useOnline();
  const [typed, setTyped] = useState("");
  const { counts, text } = planSummary(plan);
  const volumes = counts.volume > 0;
  const close = () => {
    setTyped("");
    onCancel();
  };
  return (
    <Dialog open={open} title={`Run cleanup on ${node.name}?`} onClose={close}>
      <form
        className="space-y-3"
        onSubmit={(event) => {
          event.preventDefault();
          onConfirm();
        }}
      >
        <p>
          Cove removes the {text} in the preview from {formatAbsolute(plan.sampled_at)}. {estimate(plan)}. Docker checks each item again and the run stops if the preview no longer matches.
        </p>
        {volumes && (
          <Notice tone="warn" title={`${count(counts.volume, "volume")} will be deleted with their data`}>
            Volume data cannot be recovered after cleanup.
          </Notice>
        )}
        {volumes && (
          <Field id={id} label={`Type ${node.name} to confirm`}>
            <input id={id} className={`${inputClass} font-mono`} value={typed} onChange={(event) => setTyped(event.target.value)} autoComplete="off" spellCheck={false} required />
          </Field>
        )}
        <div className="flex flex-wrap justify-end gap-2 pt-1">
          <Button variant="ghost" onClick={close} disabled={pending}>Cancel</Button>
          <Button type="submit" variant="danger" pending={pending} disabled={!online || (volumes && typed !== node.name)}>Run cleanup</Button>
        </div>
      </form>
    </Dialog>
  );
}

function CleanupRun({ node, policyDirty, plan, setPlan }: { node: Node; policyDirty: boolean; plan: CleanupPlan | null; setPlan: (plan: CleanupPlan | null) => void }) {
  const online = useOnline();
  const preview = usePreviewCleanup(node.id);
  const run = useRunCleanup(node.id);
  const [confirming, setConfirming] = useState(false);

  const refresh = () => {
    run.reset();
    preview.mutate(undefined, { onSuccess: (envelope) => setPlan(envelope.plan) });
  };

  const apply = () => {
    if (!plan) return;
    run.mutate(plan.revision, {
      onSettled: () => {
        // The preview is spent either way: a finished or partial run changes what remains.
        setConfirming(false);
        setPlan(null);
      },
    });
  };

  const summary = plan ? planSummary(plan) : null;
  const blocked = !online ? "Unavailable while offline." : policyDirty ? "Save or discard the policy changes first; the preview uses the saved policy." : undefined;

  return (
    <Subsection
      title="Preview and run"
      description="Nothing is removed until you review a preview and confirm it. A manual run uses the saved policy even when the schedule is off."
      actions={
        <>
          <Button onClick={refresh} disabled={blocked !== undefined || run.isPending} pending={preview.isPending}>
            {plan ? "Refresh preview" : "Preview cleanup"}
          </Button>
          {plan && plan.items.length > 0 && (
            <Button variant="danger" onClick={() => setConfirming(true)} disabled={blocked !== undefined || preview.isPending} pending={run.isPending}>
              Run cleanup
            </Button>
          )}
        </>
      }
    >
      {blocked && <p className="text-xs text-fg-muted">{blocked}</p>}
      <div aria-live="polite" className="space-y-3">
        {preview.isError && (
          <Notice tone={isStatus(preview.error, 502) || isStatus(preview.error, 409) ? "warn" : "error"} title="Could not preview cleanup">
            {errorMessage(preview.error)}
          </Notice>
        )}
        {run.isError && (
          <Notice tone="error" title="Cleanup did not finish">
            {errorMessage(run.error)} Some items may already have been removed. The status above was read again; preview again before another run.
          </Notice>
        )}
        {run.isSuccess && (
          <Notice tone="success" title="Cleanup finished">
            Removed {run.data.state.removed.toLocaleString("en")} items, about {formatBinaryBytes(run.data.state.estimated_bytes)} estimated. Preview again to see what remains.
          </Notice>
        )}
        {preview.isPending && <LoadingRows label="Reading cleanup candidates from the node" rows={2} />}
        {plan && summary && !preview.isPending && (
          <div className="space-y-3">
            <p className="text-xs text-fg-muted">
              Preview from <time dateTime={plan.sampled_at}>{formatAbsolute(plan.sampled_at)}</time>. Eligibility is checked again when the run starts.
            </p>
            {plan.items.length === 0 ? (
              <p className="text-[13px] text-fg-muted">Nothing is eligible for cleanup under the saved policy.</p>
            ) : (
              <>
                <p className="text-[13px] text-fg">
                  {summary.text}. <span className="text-fg-muted">{estimate(plan)}.</span>
                </p>
                <PlanTable plan={plan} />
              </>
            )}
            {plan.skipped_cache > 0 && (
              <p className="text-xs text-fg-muted">
                {count(plan.skipped_cache, "cache")} kept because Docker did not report when {plan.skipped_cache === 1 ? "it was" : "they were"} last used.
              </p>
            )}
          </div>
        )}
      </div>
      {plan && <RunDialog node={node} plan={plan} open={confirming} pending={run.isPending} onCancel={() => setConfirming(false)} onConfirm={apply} />}
    </Subsection>
  );
}

export function NodeCleanup({ node }: { node: Node }) {
  const state = useQuery(cleanupPolicyQuery(node.id));
  const [policyDirty, setPolicyDirty] = useState(false);
  const [plan, setPlan] = useState<CleanupPlan | null>(null);

  if (state.isPending) {
    return (
      <Section title={title} description={description} flush>
        <LoadingRows label="Loading cleanup policy" rows={3} />
      </Section>
    );
  }
  if (state.isError) {
    const unavailable = isStatus(state.error, 409) || isStatus(state.error, 502) || isStatus(state.error, 503);
    return (
      <Section title={title} description={description}>
        {unavailable ? (
          <Notice tone="warn" title="Cleanup controls unavailable" action={<Button variant="ghost" size="sm" onClick={() => void state.refetch()} pending={state.isFetching}>Retry</Button>}>
            {errorMessage(state.error)}
          </Notice>
        ) : (
          <QueryError what="the cleanup policy" error={state.error} onRetry={() => void state.refetch()} retrying={state.isFetching} />
        )}
      </Section>
    );
  }
  return (
    <Section title={title} description={description} flush>
      <div className="divide-y divide-line">
        <LastRun state={state.data} />
        <PolicyForm nodeId={node.id} policy={state.data.policy} onDirty={setPolicyDirty} onSaved={() => setPlan(null)} />
        <CleanupRun node={node} policyDirty={policyDirty} plan={plan} setPlan={setPlan} />
      </div>
    </Section>
  );
}

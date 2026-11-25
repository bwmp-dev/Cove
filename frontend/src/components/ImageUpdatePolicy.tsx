import { useQuery, type UseQueryResult } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { Plus, RotateCw, X } from "lucide-react";
import { useId, useRef, useState, type FormEvent } from "react";
import { errorMessage } from "../api/client";
import { imageUpdatePolicyQuery, useSaveImageUpdatePolicy, type ImageUpdateCandidate, type ImageUpdateMode, type ImageUpdatePolicyStatus } from "../api/image-updates";
import type { Workload } from "../api/types";
import { useOnline } from "../lib/hooks";
import {
  enableBlocker,
  maxImageUpdateRules,
  maxRangeLength,
  modeLabels,
  ownershipText,
  policyDraftFrom,
  policyInput,
  policySaveState,
  ruleDraft,
  ruleProblem,
  updateState,
  validatePolicyDraft,
  windowSummary,
  type PolicyAccess,
  type PolicyDraft,
  type PolicyErrors,
  type RuleDraft,
} from "../lib/image-updates";
import { can } from "../lib/permissions";
import { isManagedPreview } from "../lib/previews";
import { inputClass, linkClass } from "../lib/styles";
import { ConfirmDialog } from "./ConfirmDialog";
import { RelativeTime } from "./RelativeTime";
import { SaveBar, UnsavedChangesGuard } from "./SaveBar";
import { Select } from "./Select";
import { StatusLabel } from "./status";
import { Button, DefinitionList, Field, LoadingRows, Notice, QueryError, Section, Subsection, SwitchField } from "./ui";

const description =
  "Looks for newer images of selected services during Check images and the hourly image check, then notifies you or deploys them. Checks never pull images or start containers. While the policy is on, registry webhook pushes request a fresh check instead of deploying directly.";

export function ImageUpdatePolicy({ workload }: { workload: Workload }) {
  if (workload.kind === "job" || isManagedPreview(workload)) {
    return (
      <Section title="Image update policy" description={description} width="form">
        <p className="text-[13px] text-fg-muted">
          {workload.kind === "job" ? "Jobs are never updated automatically. Image update policies are available for services." : "Managed previews do not use image update policies."}
        </p>
      </Section>
    );
  }
  return <PolicySection workload={workload} />;
}

function PolicySection({ workload }: { workload: Workload }) {
  const status = useQuery(imageUpdatePolicyQuery(workload.id));
  const online = useOnline();
  // Owned here, not by the form: a successful save replaces the form with its saved state, and the mutation's
  // per-call callbacks only run while the component that called mutate is still mounted.
  const save = useSaveImageUpdatePolicy(workload.id);
  const [outcome, setOutcome] = useState<string | null>(null);
  return (
    <Section
      title="Image update policy"
      description={description}
      width="form"
      flush
      actions={
        <Button variant="ghost" onClick={() => void status.refetch()} pending={status.isFetching} disabled={status.isPending}>
          {!status.isFetching && <RotateCw aria-hidden className="size-3.5" />}
          Reload
        </Button>
      }
    >
      <PolicyBody workload={workload} status={status} save={save} online={online} outcome={outcome} onOutcome={setOutcome} />
    </Section>
  );
}

// Unknown or stale status never unlocks a save: the control plane refuses saving the policy on during an operation,
// so the panel waits until it can read the current state. That hold is applied by the form, since saving it off is
// allowed at any time.
function saveBlocker(status: UseQueryResult<ImageUpdatePolicyStatus>, online: boolean): string | undefined {
  if (!online) return "You are offline.";
  if (status.isError) return "The latest status could not be read.";
  return undefined;
}

type Save = ReturnType<typeof useSaveImageUpdatePolicy>;

interface BodyProps {
  workload: Workload;
  status: UseQueryResult<ImageUpdatePolicyStatus>;
  save: Save;
  online: boolean;
  outcome: string | null;
  onOutcome: (outcome: string | null) => void;
}

function PolicyBody({ workload, status, save, online, outcome, onOutcome }: BodyProps) {
  if (status.isPending) return <LoadingRows label="Loading image update policy" rows={3} />;
  if (status.isError && !status.data) {
    return (
      <div className="p-4">
        <QueryError what="the image update policy" error={status.error} onRetry={() => void status.refetch()} retrying={status.isFetching} />
      </div>
    );
  }
  const data = status.data;
  const stale = status.isError;
  const access: PolicyAccess = { canEdit: can(workload.permissions, "edit_configuration"), canOperate: can(workload.permissions, "operate") };
  const canEdit = access.canEdit;
  const blocked = canEdit ? saveBlocker(status, online) : undefined;
  return (
    <div className="divide-y divide-line">
      <div className="space-y-2 p-4 empty:hidden" aria-live="polite">
        {stale && (
          <Notice
            tone="error"
            title="Could not refresh the image update policy"
            action={
              <Button variant="ghost" size="sm" onClick={() => void status.refetch()} pending={status.isFetching}>
                {!status.isFetching && <RotateCw aria-hidden className="size-3.5" />}
                Retry
              </Button>
            }
          >
            <p className="break-words">{errorMessage(status.error)}</p>
            <p className="mt-1">{canEdit ? "Showing the last status that loaded. Saving is paused until the current status loads." : "Showing the last status that loaded."}</p>
          </Notice>
        )}
        {canEdit && !online && <Notice tone="warn" title="You are offline">Saving is paused until the connection returns.</Notice>}
        {data.active_operation_id && <ActiveOperation id={data.active_operation_id} canEdit={canEdit} />}
        {outcome && <Notice tone="success" title={outcome} />}
        <OwnershipNotice status={data} access={access} />
      </div>
      {data.configuration && <PolicyStatus status={data} unreadable={stale} />}
      <PolicySettings save={save} status={data} access={access} blocked={blocked} onOutcome={onOutcome} />
    </div>
  );
}

function OwnershipNotice({ status, access }: { status: ImageUpdatePolicyStatus; access: PolicyAccess }) {
  if (!status.configuration || !status.ownership_required) return null;
  return (
    <Notice tone="warn" title={status.configuration.enabled ? "Image update policy suspended" : "No authorised owner"}>
      {ownershipText(access)}
    </Notice>
  );
}

function ActiveOperation({ id, canEdit }: { id: string; canEdit: boolean }) {
  return (
    <Notice
      tone="warn"
      title="An operation is in progress"
      action={
        <Link to="/operations/$operationId" params={{ operationId: id }} className={linkClass}>
          View operation
        </Link>
      }
    >
      {canEdit
        ? "Saving the policy on is refused until it finishes. You can still turn the policy off, which also cancels automatic deploys it queued that have not finished."
        : "The status below may change when it finishes."}
    </Notice>
  );
}

const none = <span className="text-fg-muted">None yet</span>;

function OperationLink({ id, label }: { id: string; label: string }) {
  return (
    <Link to="/operations/$operationId" params={{ operationId: id }} className={linkClass}>
      {label}
    </Link>
  );
}

function LastCheck({ status }: { status: ImageUpdatePolicyStatus }) {
  if (!status.last_check_id) return none;
  return (
    <span className="flex flex-wrap items-center gap-x-3 gap-y-1">
      {status.checked_at ? <RelativeTime value={status.checked_at} /> : <span className="text-fg-muted">Time not recorded</span>}
      <OperationLink id={status.last_check_id} label="View check" />
    </span>
  );
}

function ResultLabel({ status, previous, unreadable }: { status: ImageUpdatePolicyStatus; previous: boolean; unreadable: boolean }) {
  if (!status.updates) return <span className="text-fg-muted">{status.last_check_id ? "Not recorded by the last check" : "No result yet"}</span>;
  const state = updateState(status.updates.state);
  return <StatusLabel tone={unreadable || previous ? "neutral" : state.tone} label={previous ? `${state.label} (previous result)` : state.label} />;
}

function DeployRequest({ id }: { id: string | null }) {
  if (!id) return none;
  return (
    <span className="flex flex-wrap items-center gap-x-3 gap-y-1">
      <span className="text-fg-muted">Queued by this policy</span>
      <OperationLink id={id} label="View operation" />
    </span>
  );
}

function PolicyStatus({ status, unreadable }: { status: ImageUpdatePolicyStatus; unreadable: boolean }) {
  const configuration = status.configuration;
  if (!configuration) return null;
  const updates = status.updates;
  const previous = status.stale && updates !== null;
  return (
    <Subsection title="Status">
      <DefinitionList
        items={[
          { term: "Policy", value: configuration.enabled ? `On · ${modeLabels[configuration.mode] ?? configuration.mode}` : "Off" },
          { term: "Maintenance window", value: windowSummary(configuration.window_start, configuration.window_end) },
          { term: "Last policy check", value: <LastCheck status={status} /> },
          { term: "Result", value: <ResultLabel status={status} previous={previous} unreadable={unreadable} /> },
          { term: "Last deploy request", value: <DeployRequest id={status.last_operation_id} /> },
        ]}
      />
      {!updates && !status.last_check_id && configuration.enabled && <p className="text-xs text-fg-muted">The next Check images or hourly image check looks for updates with these rules.</p>}
      {previous && (
        <Notice tone="warn" title="Result is from before the latest change">
          The policy, the saved configuration or the running release changed after this check, so it may not describe what is deployed now. The next Check images or hourly image check replaces it.
        </Notice>
      )}
      {updates && <UpdateResult state={updates.state} message={updates.message} candidates={updates.candidates} mode={configuration.mode} previous={previous} />}
      {status.last_error && (
        // The control plane also records holds here, such as waiting for the maintenance window.
        <Notice tone="warn" title="Latest policy message">
          <p className="break-words">{status.last_error}</p>
        </Notice>
      )}
    </Subsection>
  );
}

function UpdateResult({ state, message, candidates, mode, previous }: { state: string; message?: string; candidates: ImageUpdateCandidate[]; mode: ImageUpdateMode; previous: boolean }) {
  const available = state === "available" && !previous;
  return (
    <div className="space-y-2">
      {state !== "available" && state !== "current" && (
        <Notice tone="warn" title="Update status unknown">
          {message && <p className="break-words">{message}</p>}
          <p className={message ? "mt-1" : undefined}>Nothing is notified or deployed from an unknown result.</p>
        </Notice>
      )}
      {state === "available" && message && <p className="text-xs break-words text-fg-muted">{message}</p>}
      {candidates.length > 0 && <CandidateList candidates={candidates} label={previous ? "Previous update candidates" : "Update candidates"} />}
      {available && (
        <p className="text-xs text-fg-muted">
          {mode === "auto"
            ? "Each candidate is deployed once, as its exact digest, inside the maintenance window. A queued deploy request has not deployed anything until its operation succeeds."
            : "Containers are unchanged. Each candidate is notified once."}
        </p>
      )}
    </div>
  );
}

function Reference({ label, value }: { label: string; value: string }) {
  return (
    <p className="grid gap-x-3 text-xs @sm:grid-cols-[4rem_minmax(0,1fr)]">
      <span className="text-fg-muted">{label}</span>
      {value ? <span className="min-w-0 font-mono break-all text-fg">{value}</span> : <span className="text-fg-muted">Not reported</span>}
    </p>
  );
}

function CandidateList({ candidates, label }: { candidates: ImageUpdateCandidate[]; label: string }) {
  return (
    <ul aria-label={label} className="divide-y divide-line rounded-md border border-line">
      {candidates.map((candidate) => (
        <li key={`${candidate.service}:${candidate.target}`} className="min-w-0 space-y-1 px-3 py-2">
          <p className="text-[13px] font-medium break-words text-fg">{candidate.service || "Unnamed service"}</p>
          <Reference label="Current" value={candidate.current} />
          <Reference label="Target" value={candidate.target} />
        </li>
      ))}
    </ul>
  );
}

function PolicySettings({ save, status, access, blocked, onOutcome }: { save: Save; status: ImageUpdatePolicyStatus; access: PolicyAccess; blocked?: string; onOutcome: (outcome: string | null) => void }) {
  const configuration = status.configuration;
  if (!configuration && !status.eligible) {
    return (
      <Subsection>
        <p className="text-[13px] break-words text-fg-muted">{status.unavailable_reason || "Image updates require an image-only service outside managed previews."}</p>
      </Subsection>
    );
  }
  if (!access.canEdit) {
    return (
      <Subsection title="Settings">
        <p className="text-xs text-fg-muted">Changing the image update policy needs Edit configuration access. Settings are read-only.</p>
        {configuration ? <RuleList rules={configuration.rules} /> : <p className="text-[13px] text-fg-muted">No policy has been saved.</p>}
      </Subsection>
    );
  }
  return <PolicyForm key={JSON.stringify(configuration)} save={save} status={status} access={access} blocked={blocked} onOutcome={onOutcome} />;
}

function RuleList({ rules }: { rules: { service: string; range: string }[] }) {
  return (
    <ul aria-label="Policy services" className="divide-y divide-line rounded-md border border-line">
      {rules.map((rule) => (
        <li key={rule.service} className="flex min-w-0 flex-wrap justify-between gap-x-4 gap-y-0.5 px-3 py-2 text-xs">
          <span className="font-mono break-all text-fg">{rule.service}</span>
          <span className={rule.range ? "font-mono break-all text-fg" : "text-fg-muted"}>{rule.range || "Configured tag"}</span>
        </li>
      ))}
    </ul>
  );
}

function PolicyForm({ save, status, access, blocked, onOutcome }: { save: Save; status: ImageUpdatePolicyStatus; access: PolicyAccess; blocked?: string; onOutcome: (outcome: string | null) => void }) {
  const formRef = useRef<HTMLFormElement>(null);
  const existing = status.configuration;
  const saved = policyDraftFrom(existing);
  const [draft, setDraft] = useState<PolicyDraft>(saved);
  const [showErrors, setShowErrors] = useState(false);
  const [confirming, setConfirming] = useState(false);
  const { changed, claims, dirty, confirms, cancelsDeploys, enableError, enableLocked, held } = policySaveState(status, saved, draft, access);
  const saveBlocked = blocked ?? (held ? "An operation is in progress." : undefined);
  const errors = showErrors ? validatePolicyDraft(draft) : {};

  function edit(next: PolicyDraft) {
    setDraft(next);
    save.reset();
    onOutcome(null);
  }

  function send() {
    save.mutate(policyInput(draft), {
      onSuccess: () => {
        onOutcome(existing ? "Policy saved" : "Policy created");
        save.reset();
      },
    });
  }

  function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    setShowErrors(true);
    if (enableError || Object.keys(validatePolicyDraft(draft)).length > 0) {
      requestAnimationFrame(() => formRef.current?.querySelector<HTMLElement>("[aria-invalid=true]")?.focus());
      return;
    }
    if (confirms) setConfirming(true);
    else send();
  }

  return (
    <form ref={formRef} onSubmit={submit} aria-label="Image update policy settings" noValidate>
      <UnsavedChangesGuard dirty={changed} />
      <fieldset disabled={save.isPending} className="min-w-0">
        <legend className="sr-only">Image update policy settings</legend>
        <PolicyFields status={status} draft={draft} errors={errors} enableError={enableError} enableLocked={enableLocked} onChange={edit} />
      </fieldset>
      {save.isError && <SaveError error={save.error} existing={!!existing} />}
      <div className="space-y-2 border-t border-line px-4 py-3">
        {cancelsDeploys && <p className="text-xs text-fg-muted">Saving cancels automatic deploys this policy queued earlier that have not started, including any whose start could not be confirmed.</p>}
        <SaveBar dirty={dirty} pending={save.isPending} blocked={Boolean(saveBlocked)} label={existing ? "Save policy" : "Create policy"} onDiscard={() => edit(policyDraftFrom(existing))} status={saveBlocked ? `${saveBlocked} Saving is paused.` : "No unsaved changes"} />
      </div>
      <SaveDialog
        open={confirming}
        draft={draft}
        claims={claims}
        pending={save.isPending}
        onCancel={() => setConfirming(false)}
        onConfirm={() => {
          setConfirming(false);
          send();
        }}
      />
    </form>
  );
}

function SaveError({ error, existing }: { error: unknown; existing: boolean }) {
  return (
    <div className="px-4 pb-4">
      <Notice tone="error" title={existing ? "The policy was not saved" : "The policy was not created"}>
        <p className="break-words">{errorMessage(error)}</p>
        <p className="mt-1">Your changes are kept.</p>
      </Notice>
    </div>
  );
}

function SaveDialog({ open, draft, claims, pending, onCancel, onConfirm }: { open: boolean; draft: PolicyDraft; claims: boolean; pending: boolean; onCancel: () => void; onConfirm: () => void }) {
  return (
    <ConfirmDialog
      open={open}
      title={claims ? "Take ownership of the image update policy?" : "Turn on automatic image updates?"}
      confirmLabel={claims ? "Take ownership" : "Turn on automatic updates"}
      pending={pending}
      onCancel={onCancel}
      onConfirm={onConfirm}
    >
      {draft.mode === "auto" ? (
        <>
          When a check finds an image that matches these rules, Cove queues a deploy of its exact digest and replaces the resource&apos;s containers. Maintenance window: {windowSummary(draft.windowStart.trim(), draft.windowEnd.trim())}. Saved Compose and image tags are not changed. Approval, freezes, deployment windows and quotas still apply, and the window is checked again when the deploy is admitted.{" "}
        </>
      ) : (
        "When a check finds an image that matches these rules, it is notified once and containers stay unchanged. "
      )}
      You become the policy&apos;s owner, so version checks, notifications and automatic deploys continue only while you keep Edit configuration and Operate access.
    </ConfirmDialog>
  );
}

interface FieldsProps {
  status: ImageUpdatePolicyStatus;
  draft: PolicyDraft;
  errors: PolicyErrors;
  enableError?: string;
  enableLocked: boolean;
  onChange: (draft: PolicyDraft) => void;
}

function PolicyFields({ status, draft, errors, enableError, enableLocked, onChange }: FieldsProps) {
  const id = useId();
  return (
    <Subsection title={status.configuration ? "Settings" : "Create a policy"}>
      <SwitchField
        id={`${id}-enabled`}
        label="Enable image update policy"
        checked={draft.enabled}
        disabled={enableLocked}
        onChange={(event) => onChange({ ...draft, enabled: event.target.checked })}
        aria-invalid={enableError ? true : undefined}
        description={enableError ? <span className="text-bad">{enableError}</span> : enableLocked ? (enableBlocker(status) ?? "Turning the policy on needs Operate access as well as Edit configuration.") : "Off keeps these settings but stops looking for updates. Check images still compares the configured tags."}
      />
      <Field id={`${id}-mode`} label="When an update is found" hint={<ModeHint mode={draft.mode} />}>
        <Select id={`${id}-mode`} value={draft.mode} aria-labelledby={`${id}-mode-label`} aria-describedby={`${id}-mode-hint`} onChange={(event) => onChange({ ...draft, mode: event.target.value as ImageUpdateMode })}>
          <option value="notify">{modeLabels.notify}</option>
          <option value="auto">{modeLabels.auto}</option>
        </Select>
      </Field>
      <RuleEditor status={status} rules={draft.rules} errors={errors} onChange={(rules) => onChange({ ...draft, rules })} />
      <WindowFields draft={draft} error={errors.window} onChange={onChange} />
    </Subsection>
  );
}

function ModeHint({ mode }: { mode: ImageUpdateMode }) {
  if (mode === "auto") {
    return (
      <>
        Cove queues a deploy that pins every service to an exact image: the verified candidate digest for updated services, and the exact local image for the rest. Candidates are verified for the node&apos;s platform, saved Compose and image tags are never edited, and an image is never replaced by a lower version.
      </>
    );
  }
  return (
    <>
      Containers stay unchanged. Candidates are shown here, and each is sent once as an Image update available event to enabled notification connections that select it under Specific events. By result routing does not include it. Choose it in{" "}
      <Link to="/notifications" className={linkClass}>
        Notifications
      </Link>
      .
    </>
  );
}

function serviceOptions(status: ImageUpdatePolicyStatus, rules: RuleDraft[]) {
  const known = new Map(status.services.map((service) => [service.service, service]));
  const names = new Set([...known.keys(), ...rules.map((rule) => rule.service).filter(Boolean)]);
  return [...names].sort().map((name) => {
    const service = known.get(name);
    const note = !service ? " (not in Compose)" : !service.mutable ? " (no plain tag)" : "";
    return { value: name, label: `${name}${note}` };
  });
}

function RuleEditor({ status, rules, errors, onChange }: { status: ImageUpdatePolicyStatus; rules: RuleDraft[]; errors: PolicyErrors; onChange: (rules: RuleDraft[]) => void }) {
  const id = useId();
  const full = rules.length >= maxImageUpdateRules;
  const options = serviceOptions(status, rules);
  const update = (key: string, patch: Partial<RuleDraft>) => onChange(rules.map((rule) => (rule.key === key ? { ...rule, ...patch } : rule)));
  return (
    <fieldset className="min-w-0 space-y-2" aria-describedby={`${id}-hint`}>
      <legend className="text-[13px] font-medium text-fg">Services and version ranges</legend>
      <div id={`${id}-hint`} className="space-y-1 text-xs text-fg-muted">
        <p>Leave the range empty to follow the service&apos;s configured tag and use a new digest when that tag moves.</p>
        <p>
          A range such as <span className="font-mono">^1.2.0</span> or <span className="font-mono">~1.4</span> picks the highest tag above the deployed version that matches it. Tags must be complete versions such as 1.2.3 or v1.2.3, prereleases need a range that names a prerelease, and Cove checks the range when you save.
        </p>
      </div>
      <ul className="space-y-3 @sm:space-y-2">
        {rules.map((rule, index) => (
          <RuleRow
            key={rule.key}
            rule={rule}
            index={index}
            options={options}
            problem={ruleProblem(rule.service.trim(), status)}
            serviceError={errors[`${rule.key}.service`]}
            rangeError={errors[`${rule.key}.range`]}
            removable={rules.length > 1}
            onChange={(patch) => update(rule.key, patch)}
            onRemove={() => onChange(rules.filter((item) => item.key !== rule.key))}
          />
        ))}
      </ul>
      {errors.rules && <p className="text-xs text-bad">{errors.rules}</p>}
      <Button size="sm" onClick={() => onChange([...rules, ruleDraft()])} disabled={full} title={full ? `At most ${maxImageUpdateRules} services` : undefined}>
        <Plus aria-hidden className="size-3.5" />
        Add service
      </Button>
    </fieldset>
  );
}

interface RuleRowProps {
  rule: RuleDraft;
  index: number;
  options: { value: string; label: string }[];
  problem: string | null;
  serviceError?: string;
  rangeError?: string;
  removable: boolean;
  onChange: (patch: Partial<RuleDraft>) => void;
  onRemove: () => void;
}

function RuleRow({ rule, index, options, problem, serviceError, rangeError, removable, onChange, onRemove }: RuleRowProps) {
  const messageId = useId();
  const errorText = [serviceError, rangeError].filter(Boolean).join(" ");
  const describedBy = errorText || problem ? messageId : undefined;
  return (
    <li className="min-w-0">
      <div className="grid grid-cols-[minmax(0,1fr)_auto] items-center gap-2 @sm:grid-cols-[minmax(0,12rem)_minmax(0,1fr)_auto]">
        <div className="col-span-2 min-w-0 @sm:col-span-1">
          <Select value={rule.service} aria-label={`Service ${index + 1}`} aria-invalid={serviceError ? true : undefined} aria-describedby={describedBy} onChange={(event) => onChange({ service: event.target.value })}>
            <option value="">Choose a service</option>
            {options.map((option) => (
              <option key={option.value} value={option.value}>
                {option.label}
              </option>
            ))}
          </Select>
        </div>
        <input
          className={`${inputClass} font-mono`}
          value={rule.range}
          placeholder="Configured tag"
          maxLength={maxRangeLength}
          autoComplete="off"
          spellCheck={false}
          autoCapitalize="none"
          aria-label={`Version range ${index + 1}`}
          aria-invalid={rangeError ? true : undefined}
          aria-describedby={describedBy}
          onChange={(event) => onChange({ range: event.target.value })}
        />
        <Button variant="ghost" className="px-2" aria-label={`Remove service ${index + 1}`} title="Remove" disabled={!removable} onClick={onRemove}>
          <X aria-hidden className="size-3.5" />
        </Button>
      </div>
      {describedBy && (
        <div id={messageId} className="mt-1 space-y-0.5 text-xs break-words">
          {errorText && <p className="text-bad">{errorText}</p>}
          {problem && <p className="text-warn">{problem}</p>}
        </div>
      )}
    </li>
  );
}

function WindowFields({ draft, error, onChange }: { draft: PolicyDraft; error?: string; onChange: (draft: PolicyDraft) => void }) {
  const id = useId();
  const describedBy = error ? `${id}-error` : `${id}-hint`;
  return (
    <fieldset className="min-w-0 space-y-2" aria-describedby={describedBy}>
      <legend className="text-[13px] font-medium text-fg">Maintenance window (UTC)</legend>
      <div className="grid gap-4 sm:grid-cols-2">
        <WindowTime id={`${id}-start`} label="Starts" value={draft.windowStart} invalid={Boolean(error)} describedBy={describedBy} onChange={(windowStart) => onChange({ ...draft, windowStart })} />
        <WindowTime id={`${id}-end`} label="Ends" value={draft.windowEnd} invalid={Boolean(error)} describedBy={describedBy} onChange={(windowEnd) => onChange({ ...draft, windowEnd })} />
      </div>
      {error ? (
        <p id={`${id}-error`} className="text-xs text-bad">
          {error}
        </p>
      ) : (
        <p id={`${id}-hint`} className="text-xs text-fg-muted">
          Automatic deploys start only inside this daily window, which can run past midnight (for example 23:00 to 02:00). It is checked again when a deploy is admitted, including after an approval delay. Checks and notifications run at any time. Clear both times for any time of day. Team, project and environment deployment windows, in their own time zones, also apply. Currently: {windowSummary(draft.windowStart.trim(), draft.windowEnd.trim())}.
        </p>
      )}
    </fieldset>
  );
}

function WindowTime({ id, label, value, invalid, describedBy, onChange }: { id: string; label: string; value: string; invalid: boolean; describedBy: string; onChange: (value: string) => void }) {
  return (
    <div className="min-w-0 space-y-1.5">
      <label htmlFor={id} className="block text-[13px] font-medium text-fg">
        {label}
      </label>
      <input id={id} type="time" className={inputClass} value={value} aria-invalid={invalid ? true : undefined} aria-describedby={describedBy} onChange={(event) => onChange(event.target.value)} />
    </div>
  );
}

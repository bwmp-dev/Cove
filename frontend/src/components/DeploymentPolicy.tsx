import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Plus, Trash2 } from "lucide-react";
import { useMemo, useState, type FormEvent } from "react";
import { errorMessage, isStatus, request } from "../api/client";
import { useCurrentTeam } from "../lib/session";
import { managesTeam } from "../lib/permissions";
import { useOnline } from "../lib/hooks";
import { choiceClass, inputClass } from "../lib/styles";
import { formatAbsolute } from "../lib/format";
import {
  draftErrors,
  fromPolicy,
  newFreeze,
  newWindow,
  policyLimit,
  toPolicy,
  weekdays,
  type DeploymentPolicyValue,
  type PolicyDraft,
} from "../lib/deployment-policy";
import { Checkbox } from "./Checkbox";
import { SaveBar, UnsavedChangesGuard } from "./SaveBar";
import { Select } from "./Select";
import { Button, Field, Notice, Section, Subsection, SwitchField } from "./ui";

export type PolicyScope = "team" | "project" | "environment";

function policySummary(policy: DeploymentPolicyValue | undefined, failed: boolean) {
  if (failed) return "Could not load the policy";
  if (!policy) return "Loading policy";
  const parts = [policy.approval ? "Approval required" : "No approval required"];
  const now = Date.now();
  const freezes = [...(policy.freeze_from && policy.freeze_until ? [{ from: policy.freeze_from, until: policy.freeze_until }] : []), ...policy.freezes];
  const active = freezes.find((freeze) => Date.parse(freeze.from) <= now && now < Date.parse(freeze.until));
  if (active) parts.push(`Frozen until ${formatAbsolute(active.until)}`);
  else {
    const upcoming = freezes.filter((freeze) => now < Date.parse(freeze.from)).sort((a, b) => Date.parse(a.from) - Date.parse(b.from))[0];
    if (upcoming) parts.push(`Freeze from ${formatAbsolute(upcoming.from)}`);
  }
  const zone = policy.timezone || "UTC";
  if (policy.window_start && policy.window_end) parts.push(`Daily ${policy.window_start}–${policy.window_end} ${zone}`);
  if (policy.recurring_windows.length) parts.push(`${policy.recurring_windows.length} weekly window${policy.recurring_windows.length === 1 ? "" : "s"} (${zone})`);
  if (!(policy.window_start && policy.window_end) && !policy.recurring_windows.length) parts.push("Any time of day");
  return parts.join(" · ");
}

const scopeDescriptions: Record<PolicyScope, string> = {
  team: "Applies to every project and environment in this team. A deployment must satisfy the team, project and environment policies together.",
  project: "A deployment must satisfy the team, project and environment policies together. Team owners and admins edit policies and approve queued deployments.",
  environment: "A deployment must satisfy the team, project and environment policies together. Team owners and admins edit policies and approve queued deployments.",
};

const timeZones = (() => {
  try {
    return ["UTC", ...Intl.supportedValuesOf("timeZone").filter((zone) => zone !== "UTC")];
  } catch {
    return ["UTC"];
  }
})();

function browserTimeZone() {
  try {
    return Intl.DateTimeFormat().resolvedOptions().timeZone;
  } catch {
    return "";
  }
}

function saveError(error: unknown) {
  if (isStatus(error, 403)) return "Only team owners and admins can change deployment policies.";
  if (isStatus(error, 503)) return "Deployment policies are temporarily unavailable. Try again shortly.";
  return errorMessage(error);
}

type PolicyEditorProps = {
  value: PolicyDraft;
  shown: Record<string, string>;
  disabled: boolean;
  admin: boolean;
  prefix: string;
  zone: string;
  update: (change: (current: PolicyDraft) => PolicyDraft) => void;
};

function ScheduleEditor({ value, shown, disabled, prefix, zone, update }: PolicyEditorProps) {
  const browserZone = browserTimeZone();
  return (
    <Subsection title="Time zone and daily window" description="Daily and weekly windows use local wall-clock time in this zone, including across daylight-saving changes: a skipped hour never occurs and a repeated hour applies twice. Freezes below are absolute UTC times and ignore this zone.">
      <div className="grid gap-4 sm:grid-cols-2">
        <Field id={`${prefix}-timezone`} label="Time zone" hint={browserZone && browserZone !== value.timezone ? <>IANA name, such as Europe/Berlin. <button type="button" className="rounded underline decoration-fg-subtle underline-offset-4 hover:text-fg disabled:no-underline" disabled={disabled} onClick={() => update((current) => ({ ...current, timezone: browserZone }))}>Use {browserZone}</button></> : "IANA name, such as Europe/Berlin."} error={shown.timezone}>
          <input id={`${prefix}-timezone`} className={inputClass} list={`${prefix}-timezones`} value={value.timezone} disabled={disabled} autoComplete="off" spellCheck={false} aria-invalid={shown.timezone ? true : undefined} aria-describedby={shown.timezone ? `${prefix}-timezone-error` : `${prefix}-timezone-hint`} onChange={(event) => update((current) => ({ ...current, timezone: event.target.value }))} />
          <datalist id={`${prefix}-timezones`}>{timeZones.map((name) => <option key={name} value={name} />)}</datalist>
        </Field>
        <Field id={`${prefix}-outside`} label="Outside allowed windows" hint="What happens to a deployment queued outside the windows.">
          <Select id={`${prefix}-outside`} value={value.outside_window} disabled={disabled} aria-describedby={`${prefix}-outside-hint`} onChange={(event) => update((current) => ({ ...current, outside_window: event.target.value === "approval" ? "approval" : "block" }))}>
            <option value="block">Wait for the next allowed window</option>
            <option value="approval">Require approval to start now</option>
          </Select>
        </Field>
        <Field id={`${prefix}-window-start`} label={`Daily window starts (${zone})`} optional error={shown.daily}>
          <input id={`${prefix}-window-start`} className={inputClass} type="time" value={value.window_start} disabled={disabled} aria-invalid={shown.daily ? true : undefined} aria-describedby={shown.daily ? `${prefix}-window-start-error` : undefined} onChange={(event) => update((current) => ({ ...current, window_start: event.target.value }))} />
        </Field>
        <Field id={`${prefix}-window-end`} label={`Daily window ends (${zone})`} optional>
          <input id={`${prefix}-window-end`} className={inputClass} type="time" value={value.window_end} disabled={disabled} aria-invalid={shown.daily ? true : undefined} onChange={(event) => update((current) => ({ ...current, window_end: event.target.value }))} />
        </Field>
      </div>
      <p className="text-xs text-fg-muted">Clear both times to allow any time of day. A window may cross midnight. If you set both a daily window and weekly windows, a deployment must fall inside the daily window and at least one weekly window.</p>
    </Subsection>
  );
}

function WeeklyWindowsEditor({ value, shown, disabled, admin, prefix, zone, update }: PolicyEditorProps) {
  return (
    <Subsection
      title="Weekly windows"
      description={`Deployments may start inside any of these windows (${zone}). The start time is included and the end time is not. A window that crosses midnight belongs to the day it starts. Leave empty to allow every day.`}
      actions={admin && <Button size="sm" disabled={disabled || value.windows.length >= policyLimit} onClick={() => update((current) => ({ ...current, windows: [...current.windows, newWindow()] }))}><Plus aria-hidden className="size-3.5" />Add window</Button>}
    >
      {shown.windowCount && <p className="text-xs text-bad">{shown.windowCount}</p>}
      {value.windows.length === 0 ? (
        <p className="text-[13px] text-fg-muted">No weekly windows.</p>
      ) : (
        <ol aria-label="Weekly windows" className="divide-y divide-line rounded-md border border-line">
          {value.windows.map((window, index) => {
            const rowId = `${prefix}-window-${window.id}`;
            const error = shown[`window-${window.id}`];
            return (
              <li key={window.id} className="space-y-3 p-3">
                <fieldset className="min-w-0" aria-describedby={error ? `${rowId}-error` : undefined}>
                  <legend className="mb-1.5 text-[13px] font-medium text-fg">Window {index + 1} days</legend>
                  <div className="flex flex-wrap gap-1.5">
                    {weekdays.map((day) => (
                      <label key={day.value} className={`${choiceClass} min-w-14 justify-center`}>
                        <Checkbox
                          checked={window.days.includes(day.value)}
                          disabled={disabled}
                          aria-label={day.long}
                          onChange={(event) => update((current) => ({
                            ...current,
                            windows: current.windows.map((item) => item.id !== window.id ? item : { ...item, days: event.target.checked ? [...item.days, day.value].sort((a, b) => a - b) : item.days.filter((value) => value !== day.value) }),
                          }))}
                        />
                        <span aria-hidden>{day.short}</span>
                      </label>
                    ))}
                  </div>
                </fieldset>
                <div className="grid items-end gap-3 sm:grid-cols-[1fr_1fr_auto]">
                  <Field id={`${rowId}-start`} label={`Window ${index + 1} starts`}>
                    <input id={`${rowId}-start`} className={inputClass} type="time" value={window.start} disabled={disabled} aria-invalid={error ? true : undefined} onChange={(event) => update((current) => ({ ...current, windows: current.windows.map((item) => item.id === window.id ? { ...item, start: event.target.value } : item) }))} />
                  </Field>
                  <Field id={`${rowId}-end`} label={`Window ${index + 1} ends`}>
                    <input id={`${rowId}-end`} className={inputClass} type="time" value={window.end} disabled={disabled} aria-invalid={error ? true : undefined} onChange={(event) => update((current) => ({ ...current, windows: current.windows.map((item) => item.id === window.id ? { ...item, end: event.target.value } : item) }))} />
                  </Field>
                  {admin && <Button variant="ghost" disabled={disabled} aria-label={`Remove window ${index + 1}`} title={`Remove window ${index + 1}`} onClick={() => update((current) => ({ ...current, windows: current.windows.filter((item) => item.id !== window.id) }))}><Trash2 aria-hidden className="size-3.5" />Remove</Button>}
                </div>
                {error && <p id={`${rowId}-error`} className="text-xs text-bad">{error}</p>}
              </li>
            );
          })}
        </ol>
      )}
    </Subsection>
  );
}

function FreezesEditor({ value, shown, disabled, admin, prefix, update }: PolicyEditorProps) {
  return (
    <Subsection
      title="Freezes"
      description="No deployments start between these absolute times, entered in UTC. The policy time zone does not apply. Running deployments continue; stop and recovery operations remain available."
      actions={admin && <Button size="sm" disabled={disabled || value.freezes.length >= policyLimit} onClick={() => update((current) => ({ ...current, freezes: [...current.freezes, newFreeze()] }))}><Plus aria-hidden className="size-3.5" />Add freeze</Button>}
    >
      {shown.freezeCount && <p className="text-xs text-bad">{shown.freezeCount}</p>}
      {(value.legacyFrom || value.legacyUntil || value.freezes.length === 0) && (
        <div className="space-y-2">
          <p className="text-xs text-fg-muted">{value.freezes.length || value.legacyFrom || value.legacyUntil ? "Original freeze, kept as saved. Clear both times to remove it." : "No freezes. Set a single freeze here or add several."}</p>
          <div className="grid gap-4 sm:grid-cols-2">
            <Field id={`${prefix}-freeze-from`} label="Freeze starts (UTC)" optional error={shown.legacy}>
              <input id={`${prefix}-freeze-from`} className={inputClass} type="datetime-local" value={value.legacyFrom} disabled={disabled} aria-invalid={shown.legacy ? true : undefined} aria-describedby={shown.legacy ? `${prefix}-freeze-from-error` : undefined} onChange={(event) => update((current) => ({ ...current, legacyFrom: event.target.value }))} />
            </Field>
            <Field id={`${prefix}-freeze-until`} label="Freeze ends (UTC)" optional>
              <input id={`${prefix}-freeze-until`} className={inputClass} type="datetime-local" value={value.legacyUntil} disabled={disabled} aria-invalid={shown.legacy ? true : undefined} onChange={(event) => update((current) => ({ ...current, legacyUntil: event.target.value }))} />
            </Field>
          </div>
        </div>
      )}
      {value.freezes.length > 0 && (
        <ol aria-label="Freezes" className="divide-y divide-line rounded-md border border-line">
          {value.freezes.map((freeze, index) => {
            const rowId = `${prefix}-freeze-${freeze.id}`;
            const error = shown[`freeze-${freeze.id}`];
            return (
              <li key={freeze.id} className="space-y-3 p-3">
                <div className="grid items-end gap-3 sm:grid-cols-[1fr_1fr_auto]">
                  <Field id={`${rowId}-from`} label={`Freeze ${index + 1} starts (UTC)`}>
                    <input id={`${rowId}-from`} className={inputClass} type="datetime-local" value={freeze.from} disabled={disabled} aria-invalid={error ? true : undefined} onChange={(event) => update((current) => ({ ...current, freezes: current.freezes.map((item) => item.id === freeze.id ? { ...item, from: event.target.value } : item) }))} />
                  </Field>
                  <Field id={`${rowId}-until`} label={`Freeze ${index + 1} ends (UTC)`}>
                    <input id={`${rowId}-until`} className={inputClass} type="datetime-local" value={freeze.until} disabled={disabled} aria-invalid={error ? true : undefined} onChange={(event) => update((current) => ({ ...current, freezes: current.freezes.map((item) => item.id === freeze.id ? { ...item, until: event.target.value } : item) }))} />
                  </Field>
                  {admin && <Button variant="ghost" disabled={disabled} aria-label={`Remove freeze ${index + 1}`} title={`Remove freeze ${index + 1}`} onClick={() => update((current) => ({ ...current, freezes: current.freezes.filter((item) => item.id !== freeze.id) }))}><Trash2 aria-hidden className="size-3.5" />Remove</Button>}
                </div>
                <Field id={`${rowId}-reason`} label="Reason" optional hint="Shown to people whose deployment waits. Do not include secrets.">
                  <input id={`${rowId}-reason`} className={inputClass} value={freeze.reason} disabled={disabled} aria-describedby={`${rowId}-reason-hint`} onChange={(event) => update((current) => ({ ...current, freezes: current.freezes.map((item) => item.id === freeze.id ? { ...item, reason: event.target.value } : item) }))} />
                </Field>
                {error && <p className="text-xs text-bad">{error}</p>}
              </li>
            );
          })}
        </ol>
      )}
    </Subsection>
  );
}

// Loads the policy and keeps the unsaved draft; saved rows keep their keys until someone edits them.
function usePolicyDraft(scope: PolicyScope, id: string) {
  const client = useQueryClient();
  const key = ["deployment-policy", scope, id];
  const path = `/api/${scope === "team" ? "teams" : `${scope}s`}/${encodeURIComponent(id)}/deployment-policy`;
  const query = useQuery({ queryKey: key, queryFn: ({ signal }) => request<DeploymentPolicyValue>("GET", path, undefined, signal) });
  const [draft, setDraft] = useState<PolicyDraft | null>(null);
  const [attempted, setAttempted] = useState(false);
  const save = useMutation({
    mutationFn: (policy: DeploymentPolicyValue) => request<DeploymentPolicyValue>("PUT", path, policy),
    onSuccess: (policy) => {
      client.setQueryData(key, policy);
      setDraft(null);
      setAttempted(false);
      return client.invalidateQueries({ queryKey: key });
    },
  });
  const saved = useMemo(() => (query.data ? fromPolicy(query.data) : null), [query.data]);
  const value = draft ?? saved;
  const dirty = Boolean(draft && saved && JSON.stringify(toPolicy(draft)) !== JSON.stringify(toPolicy(saved)));
  const errors = value ? draftErrors(value) : {};
  const invalid = attempted && Object.keys(errors).length > 0;

  function update(change: (current: PolicyDraft) => PolicyDraft) {
    if (!value) return;
    save.reset();
    setDraft(change(value));
  }

  function submit() {
    if (!value) return;
    setAttempted(true);
    if (Object.keys(errors).length) return;
    save.mutate(toPolicy(value));
  }

  function discard() {
    setDraft(null);
    setAttempted(false);
    save.reset();
  }

  return { query, save, value, dirty, shown: attempted ? errors : {}, invalid, update, submit, discard };
}

function PolicyFields(props: Omit<PolicyEditorProps, "zone">) {
  const { value, disabled, admin, prefix, update } = props;
  const editor = { ...props, zone: value.timezone.trim() || "UTC" };
  return (
    <>
      {!admin && <div className="px-4 pt-4"><Notice>Only team owners and admins can change this policy.</Notice></div>}
      <div className="divide-y divide-line">
        <Subsection>
          <SwitchField id={`${prefix}-approval`} checked={value.approval} disabled={disabled} onChange={(event) => update((current) => ({ ...current, approval: event.target.checked }))} label="Require approval for deployments and rollbacks" description="Approval authorizes the queued deployment, not an immutable review of branch contents or shared variables. Approvals already required stay required if this is turned off." />
        </Subsection>

        <ScheduleEditor {...editor} />

        <WeeklyWindowsEditor {...editor} />

        <FreezesEditor {...editor} />

        <Subsection>
          <p className="text-xs text-fg-muted">
            Windows, freezes and approval apply to manual, webhook, sequence, promotion and preview deployments and rollbacks when they are admitted. They do not delay operations that have already started, stop, start and restart, route-only updates, backups, restores and drills, preview preparation and removal, or node, panel and worker maintenance. A team owner or admin can override them for one queued operation from its page, with a recorded reason.
          </p>
        </Subsection>
      </div>
    </>
  );
}

function SaveFailure({ error }: { error: unknown }) {
  return (
    <div className="border-t border-line p-4">
      {error ? <Notice tone="error" title="Could not save the policy">{saveError(error)}</Notice> : <Notice tone="error" title="Fix the highlighted fields">The policy was not saved.</Notice>}
    </div>
  );
}

export function DeploymentPolicy({ scope, id }: { scope: PolicyScope; id: string }) {
  const admin = managesTeam(useCurrentTeam());
  const online = useOnline();
  const { query, save, value, dirty, shown, invalid, update, submit, discard } = usePolicyDraft(scope, id);
  const disabled = !admin || save.isPending;

  function onSubmit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    if (admin) submit();
  }

  return (
    // Always placed in a SectionGrid, which removes the page padding from the sections it contains directly; the
    // form sits between the grid and the Section, so it removes that padding itself.
    <form onSubmit={onSubmit} noValidate aria-label={`${scope} deployment policy`} className="min-w-0 [&>section]:px-0 [&>section]:pt-0">
      <Section
        title="Deployment policy"
        collapsed={{ summary: dirty ? "Unsaved changes" : policySummary(query.data, query.isError) }}
        description={scopeDescriptions[scope]}
        flush
        footer={admin && value ? (
          <SaveBar
            dirty={dirty}
            pending={save.isPending}
            blocked={!online}
            label="Save deployment policy"
            onDiscard={discard}
            status={save.isSuccess ? "Deployment policy saved." : undefined}
          />
        ) : undefined}
      >
        {query.isPending && <p className="p-4 text-[13px] text-fg-muted">Loading policy</p>}
        {query.isError && <div className="p-4"><Notice tone="error" title="Could not load the policy">{errorMessage(query.error)}</Notice></div>}
        {value && <PolicyFields value={value} shown={shown} disabled={disabled} admin={admin} prefix={`policy-${scope}-${id}`} update={update} />}
        {(save.isError || invalid) && <SaveFailure error={save.error} />}
      </Section>
      {admin && <UnsavedChangesGuard dirty={dirty} />}
    </form>
  );
}

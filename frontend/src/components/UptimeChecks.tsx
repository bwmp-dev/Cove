import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Plus } from "lucide-react";
import { useId, useState, type FormEvent } from "react";
import { errorMessage, request } from "../api/client";
import { uptimeChecksQuery, type UptimeCheck, type UptimeCheckInput } from "../api/outputs";
import type { Workload } from "../api/types";
import { useNow, useOnline } from "../lib/hooks";
import { wildcardCovers } from "../lib/certificates";
import { formatInterval, formatMs, uptimeCheckFromForm, uptimeDefaults, uptimeState, type UptimeField } from "../lib/outputs";
import { can } from "../lib/permissions";
import { describedBy, inputClass } from "../lib/styles";
import { ConfirmDialog } from "./ConfirmDialog";
import { RelativeTime } from "./RelativeTime";
import { Select } from "./Select";
import { StatusLabel } from "./status";
import { Button, Field, LoadingRows, Notice, QueryError, Section, SwitchField } from "./ui";

const description =
  "Periodic HTTPS requests from the Cove panel to each domain's public address, without following redirects. Results reflect the panel's network, not every visitor's. Enabled checks can drive Uptime threshold rules on the Notifications page and appear on the project's public status page when they have a public label.";

function UptimeForm({ workloadId, domains, initial, close }: { workloadId: string; domains: string[]; initial?: UptimeCheck; close: () => void }) {
  const id = useId();
  const client = useQueryClient();
  const online = useOnline();
  const [error, setError] = useState<{ field: UptimeField; message: string }>();
  const [resetsHistory, setResetsHistory] = useState(false);
  const base = `/api/workloads/${encodeURIComponent(workloadId)}/uptime`;
  const save = useMutation({
    mutationFn: (input: UptimeCheckInput) => (initial ? request<{ id: string }>("PUT", `${base}/${encodeURIComponent(initial.id)}`, input) : request<{ id: string }>("POST", base, input)),
    onSuccess: async () => {
      await client.invalidateQueries({ queryKey: uptimeChecksQuery(workloadId).queryKey });
      close();
    },
  });
  const values = initial ?? { ...uptimeDefaults, domain: domains[0] ?? "" };
  const [domain, setDomain] = useState(values.domain);
  const fieldId = (field: UptimeField) => `${id}-${field}`;
  const message = (field: UptimeField) => (error?.field === field ? error.message : undefined);
  const aria = (field: UptimeField, hint = false) => ({ "aria-invalid": message(field) ? true : undefined, "aria-describedby": describedBy(fieldId(field), message(field), hint) });

  function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    const input = uptimeCheckFromForm(new FormData(event.currentTarget));
    if ("field" in input) {
      setError(input);
      document.getElementById(fieldId(input.field))?.focus();
      return;
    }
    setError(undefined);
    save.mutate(input);
  }

  return (
    <form
      onSubmit={submit}
      onChange={(event) => {
        if (!initial) return;
        const data = new FormData(event.currentTarget);
        setResetsHistory(data.get("path") !== initial.path || data.get("expected_min") !== String(initial.expected_min) || data.get("expected_max") !== String(initial.expected_max));
      }}
      noValidate
      className="space-y-4 border-b border-line p-4"
    >
      <h3 className="text-[13px] font-semibold text-fg">{initial ? `Edit check for ${initial.domain}` : "New uptime check"}</h3>
      <fieldset disabled={save.isPending} className="min-w-0 space-y-4">
        <div className="grid gap-4 sm:grid-cols-2">
          {initial ? (
            <>
              <input type="hidden" name="domain" value={initial.domain} />
              <Field id={fieldId("domain")} label="Domain" hint="A check's domain cannot change. Create a check for another domain instead.">
                <input id={fieldId("domain")} className={inputClass} value={initial.domain} readOnly aria-describedby={`${fieldId("domain")}-hint`} />
              </Field>
            </>
          ) : (
            <Field id={fieldId("domain")} label="Domain" hint="One check per configured domain, or per hostname under a wildcard domain." error={message("domain")}>
              <Select id={fieldId("domain")} name="domain" defaultValue={values.domain} onChange={(event) => setDomain(event.target.value)} required {...aria("domain", true)}>
                {domains.map((option) => <option key={option} value={option}>{option}</option>)}
              </Select>
            </Field>
          )}
          {!initial && domain.startsWith("*.") && (
            <Field id={fieldId("host_label")} label="Hostname to check" hint={`One label under ${domain}, such as shop for shop${domain.slice(1)}. The wildcard itself is never requested.`} error={message("host_label")}>
              <div className="flex min-w-0 items-center gap-2">
                <input id={fieldId("host_label")} name="host_label" className={`${inputClass} font-mono`} maxLength={63} autoComplete="off" autoCapitalize="none" spellCheck={false} required {...aria("host_label", true)} />
                <span className="min-w-0 font-mono text-[13px] break-all text-fg-muted">{domain.slice(1)}</span>
              </div>
            </Field>
          )}
          <Field id={fieldId("path")} label="Path" hint="Starts with /. No query string or fragment." error={message("path")}>
            <input id={fieldId("path")} name="path" className={`${inputClass} font-mono`} defaultValue={values.path} maxLength={2048} autoComplete="off" spellCheck={false} required {...aria("path", true)} />
          </Field>
          <Field id={fieldId("interval_seconds")} label="Interval (seconds)" hint="60 to 86400." error={message("interval_seconds")}>
            <input id={fieldId("interval_seconds")} name="interval_seconds" type="number" inputMode="numeric" min={60} max={86400} step={1} className={inputClass} defaultValue={values.interval_seconds} required {...aria("interval_seconds", true)} />
          </Field>
          <Field id={fieldId("timeout_seconds")} label="Timeout (seconds)" hint="1 to 10." error={message("timeout_seconds")}>
            <input id={fieldId("timeout_seconds")} name="timeout_seconds" type="number" inputMode="numeric" min={1} max={10} step={1} className={inputClass} defaultValue={values.timeout_seconds} required {...aria("timeout_seconds", true)} />
          </Field>
          <Field id={fieldId("expected_min")} label="Lowest healthy status" hint="100 to 599. Redirects are not followed." error={message("expected_min")}>
            <input id={fieldId("expected_min")} name="expected_min" type="number" inputMode="numeric" min={100} max={599} step={1} className={inputClass} defaultValue={values.expected_min} required {...aria("expected_min", true)} />
          </Field>
          <Field id={fieldId("expected_max")} label="Highest healthy status" hint="Inclusive, up to 599." error={message("expected_max")}>
            <input id={fieldId("expected_max")} name="expected_max" type="number" inputMode="numeric" min={100} max={599} step={1} className={inputClass} defaultValue={values.expected_max} required {...aria("expected_max", true)} />
          </Field>
        </div>
        <Field id={fieldId("public_label")} label="Public label" optional hint="Shown on the project's public status page, if one is enabled. Leave blank to keep this check private." error={message("public_label")}>
          <input id={fieldId("public_label")} name="public_label" className={inputClass} defaultValue={values.public_label} maxLength={120} autoComplete="off" {...aria("public_label", true)} />
        </Field>
        <SwitchField id={`${id}-enabled`} name="enabled" label="Enabled" defaultChecked={values.enabled} description="Disabled checks are not run. An alert that is already open stays open; no recovery notice is sent." />
        {initial && (resetsHistory ? (
          <Notice tone="warn" title="This clears the check's history">
            A new path or healthy status range measures a different endpoint, so saving removes the recorded results, including the 24-hour ratio on the public status page.
          </Notice>
        ) : (
          <p className="text-xs text-fg-muted">Saving clears the latest result; the next check runs within one interval. Changing the path or healthy status range also clears the recorded history; other changes keep it.</p>
        ))}
        {save.isError && <Notice tone="error" title="Could not save the check">{errorMessage(save.error)}</Notice>}
      </fieldset>
      <div className="flex flex-wrap justify-end gap-2">
        <Button variant="ghost" onClick={close} disabled={save.isPending}>Cancel</Button>
        <Button type="submit" variant="primary" pending={save.isPending} disabled={!online}>{initial ? "Save check" : "Create check"}</Button>
      </div>
    </form>
  );
}

function CheckRow({ check, routed, editable, busy, onEdit, onDelete }: { check: UptimeCheck; routed: boolean; editable: boolean; busy: boolean; onEdit: () => void; onDelete: () => void }) {
  const now = useNow(15_000);
  const state = uptimeState(check, now);
  const detail = [
    check.status !== null && state.label !== "Disabled" ? `HTTP ${check.status}` : "",
    check.latency_ms !== null && state.label !== "Disabled" ? formatMs(check.latency_ms) : "",
    check.consecutive_failures > 0 ? `${check.consecutive_failures} consecutive ${check.consecutive_failures === 1 ? "failure" : "failures"}` : "",
  ].filter(Boolean);
  return (
    <li className="flex flex-col gap-x-4 gap-y-1.5 px-4 py-2.5 hover:bg-raised sm:flex-row sm:items-start">
      <div className="min-w-0 flex-1">
        <p className="text-[13px] font-medium break-all text-fg">
          {check.domain}
          <span className="ml-1 font-mono text-xs font-normal text-fg-muted">{check.path}</span>
        </p>
        <p className="text-xs text-fg-muted">
          Every {formatInterval(check.interval_seconds)}, {check.timeout_seconds} s timeout, healthy {check.expected_min}–{check.expected_max}
          {check.public_label ? ` · Public as “${check.public_label}”` : " · Private"}
        </p>
        {check.enabled && check.error && <p className="text-xs break-words text-bad">{check.error}</p>}
        {!routed && <p className="text-xs text-warn">This domain is no longer configured on the service. Delete the check, or add the domain again.</p>}
      </div>
      <div className="flex flex-col items-start gap-0.5 sm:items-end sm:text-right">
        <StatusLabel tone={state.tone} label={state.label} />
        <p className="text-xs text-fg-muted tabular-nums sm:whitespace-nowrap">
          {detail.join(" · ")}
          {check.checked_at && <>{detail.length > 0 && " · "}<RelativeTime value={check.checked_at} /></>}
        </p>
      </div>
      {editable && (
        <div className="-ml-2 flex gap-1 sm:ml-0">
          <Button size="sm" variant="ghost" onClick={onEdit} disabled={busy || !routed} aria-label={`Edit check for ${check.domain}`}>Edit</Button>
          <Button size="sm" variant="ghost" onClick={onDelete} disabled={busy} aria-label={`Delete check for ${check.domain}`}>Delete</Button>
        </div>
      )}
    </li>
  );
}

function CheckList({ checks, routed, editable, busy, onEdit, onDelete }: { checks: UptimeCheck[]; routed: Set<string>; editable: boolean; busy: boolean; onEdit: (check: UptimeCheck) => void; onDelete: (check: UptimeCheck) => void }) {
  if (checks.length === 0) {
    return <p className="px-4 py-4 text-[13px] text-fg-muted">{routed.size === 0 ? "Add a domain above before creating an uptime check." : "No uptime checks. Nothing is probed until you create one."}</p>;
  }
  return (
    <ul aria-label="Uptime checks" className="divide-y divide-line">
      {checks.map((check) => (
        <CheckRow key={check.id} check={check} routed={routed.has(check.domain)} editable={editable} busy={busy} onEdit={() => onEdit(check)} onDelete={() => onDelete(check)} />
      ))}
    </ul>
  );
}

function NewCheckAction({ blocker, disabled, onClick }: { blocker?: string; disabled: boolean; onClick: () => void }) {
  return (
    <>
      {blocker && <span className="text-xs text-fg-muted">{blocker}</span>}
      <Button onClick={onClick} disabled={disabled || blocker !== undefined}>
        <Plus aria-hidden className="size-3.5" />
        New check
      </Button>
    </>
  );
}

function newCheckBlocker(configured: string[], available: string[]) {
  if (configured.length === 0) return "Add a domain first.";
  if (available.length === 0) return "Every domain already has a check.";
  return undefined;
}

export function UptimeChecks({ workload }: { workload: Workload }) {
  const client = useQueryClient();
  const online = useOnline();
  const checks = useQuery(uptimeChecksQuery(workload.id));
  const editable = can(workload.permissions, "edit");
  // undefined is the new-check form; null shows no form.
  const [editing, setEditing] = useState<UptimeCheck | undefined | null>(null);
  const [removing, setRemoving] = useState<UptimeCheck | null>(null);
  const deletion = useMutation({
    mutationFn: (check: UptimeCheck) => request("DELETE", `/api/workloads/${encodeURIComponent(workload.id)}/uptime/${encodeURIComponent(check.id)}`),
    onSuccess: async () => {
      setRemoving(null);
      await client.invalidateQueries({ queryKey: uptimeChecksQuery(workload.id).queryKey });
    },
  });

  const configured = [...new Set((workload.routes ?? []).map((route) => route.domain))].sort();
  const checked = new Set((checks.data ?? []).map((check) => check.domain));
  // A wildcard domain stays available: each check under it names its own hostname.
  const available = configured.filter((domain) => domain.startsWith("*.") || !checked.has(domain));
  const covered = (checks.data ?? []).map((check) => check.domain).filter((domain) => configured.some((route) => wildcardCovers(route, domain)));
  const routed = new Set([...configured, ...covered]);

  const unavailable = newCheckBlocker(configured, available);
  const action = editable && checks.isSuccess && editing === null && <NewCheckAction blocker={unavailable} disabled={!online} onClick={() => setEditing(undefined)} />;

  let body;
  if (checks.isPending) {
    body = <LoadingRows label="Loading uptime checks" rows={2} />;
  } else if (checks.isError && !checks.data) {
    body = <div className="p-4"><QueryError what="uptime checks" error={checks.error} onRetry={() => void checks.refetch()} retrying={checks.isFetching} /></div>;
  } else {
    body = (
      <>
        {editing !== null && <UptimeForm key={editing?.id ?? "new"} workloadId={workload.id} domains={available} initial={editing} close={() => setEditing(null)} />}
        {deletion.isError && <div className="border-b border-line p-4"><Notice tone="error" title="Could not delete the check">{errorMessage(deletion.error)}</Notice></div>}
        <CheckList
          checks={checks.data}
          routed={routed}
          editable={editable}
          busy={!online || deletion.isPending}
          onEdit={setEditing}
          onDelete={(check) => { deletion.reset(); setRemoving(check); }}
        />
      </>
    );
  }

  return (
    <Section title="Uptime checks" description={description} actions={action} flush width="form">
      {body}
      <ConfirmDialog
        open={removing !== null}
        title={`Delete the check for ${removing?.domain ?? "this domain"}?`}
        confirmLabel="Delete check"
        danger
        pending={deletion.isPending}
        onCancel={() => setRemoving(null)}
        onConfirm={() => removing && deletion.mutate(removing)}
      >
        Its history is removed and it leaves the public status page. Uptime rules for this domain stop receiving results; an alert that is already open is not resolved and no recovery notice is sent.
      </ConfirmDialog>
    </Section>
  );
}

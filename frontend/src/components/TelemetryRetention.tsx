import { useQuery } from "@tanstack/react-query";
import { useState, type FormEvent } from "react";
import { errorMessage, isStatus } from "../api/client";
import { operationRetentionQuery, retentionQuery, storageUsageQuery, useRunOperationRetention, useUpdateRetention, type OperationRetentionStatus, type RetentionSettings, type StorageCategory } from "../api/telemetry";
import { useOnline } from "../lib/hooks";
import { inputClass, tableHeadClass, tableRowClass } from "../lib/styles";
import { formatBinaryBytes, isByteLimit, parseRetention, retentionChanges, retentionDraft, retentionKeys, retentionLimits, retentionValueLabel, type RetentionDraft, type RetentionErrors } from "../lib/telemetry";
import { ConfirmDialog } from "./ConfirmDialog";
import { RelativeTime } from "./RelativeTime";
import { StatusLabel } from "./status";
import { SaveBar, UnsavedChangesGuard } from "./SaveBar";
import { Button, Field, LoadingRows, Meter, MeterGroup, Notice, QueryError, Section, SettingRows, Subsection } from "./ui";

type RetentionKey = keyof RetentionSettings;

const labels: Record<RetentionKey, { label: string; hint: string }> = {
  metric_rows: { label: "Stored samples", hint: "Rows across all nodes and resources." },
  metric_bytes: { label: "Accounted size", hint: "MiB of sample payload plus a per-row allowance." },
  raw_days: { label: "Per-minute samples", hint: "Days. Used for the last hour and last 24 hours." },
  hour_days: { label: "Hourly samples", hint: "Days, at least the per-minute period. Used for 7 and 30 days." },
  day_days: { label: "Daily samples", hint: "Days, at least the hourly period. Used for 365 days." },
  log_rows: { label: "Stored lines", hint: "Lines across all resources." },
  log_bytes: { label: "Accounted size", hint: "MiB of line text plus a per-row allowance." },
  log_days: { label: "Keep lines for", hint: "Days." },
  audit_days: { label: "Keep entries for", hint: "Days. This is the same setting as Keep audit events for in Audit export; saving either changes both." },
  operation_rows: { label: "Eligible records", hint: "Finished operation records across all teams." },
  operation_days: { label: "Keep records for", hint: "Days." },
};

const groups: { title: string; description: string; keys: RetentionKey[] }[] = [
  { title: "Metric history", description: "Node and container samples behind the usage charts.", keys: ["metric_rows", "metric_bytes", "raw_days", "hour_days", "day_days"] },
  { title: "Log history", description: "Stored container log lines.", keys: ["log_rows", "log_bytes", "log_days"] },
  {
    title: "Audit log",
    description: "Security and change events across all teams. Entries inside this period and entries still waiting for audit export delivery are always kept, so there is no row limit and a delivery backlog can grow stored audit data.",
    keys: ["audit_days"],
  },
  {
    title: "Operation history",
    description: "Finished operation records and their worker journals. Records that recovery, releases, backups or unfinished work still refer to are kept, so stored history can exceed these limits.",
    keys: ["operation_rows", "operation_days"],
  },
];

const groupNames = Object.fromEntries(groups.flatMap((group) => group.keys.map((key) => [key, group.title]))) as Record<RetentionKey, string>;

function hint(key: RetentionKey) {
  const { min, max } = retentionLimits[key];
  return `${labels[key].hint} Allowed ${min.toLocaleString("en")} to ${max.toLocaleString("en")}${isByteLimit(key) ? " MiB" : ""}.`;
}

function RetentionForm({ saved }: { saved: RetentionSettings }) {
  const update = useUpdateRetention();
  const online = useOnline();
  const [draft, setDraft] = useState<RetentionDraft>(() => retentionDraft(saved));
  const [errors, setErrors] = useState<RetentionErrors>({});
  const [pending, setPending] = useState<Partial<RetentionSettings> | null>(null);
  const [auditDays, setAuditDays] = useState(saved.audit_days);
  if (saved.audit_days !== auditDays) {
    // Audit export saves the same period. Follow it so this form never sends the old value back.
    setAuditDays(saved.audit_days);
    setDraft({ ...draft, audit_days: String(saved.audit_days) });
  }
  const initial = retentionDraft(saved);
  const dirty = retentionKeys.some((key) => draft[key] !== initial[key]);

  const submit = (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    const parsed = parseRetention(draft, saved);
    setErrors(parsed.errors);
    if (!parsed.values) return;
    const changes = retentionChanges(saved, parsed.values);
    if (Object.keys(changes).length > 0) setPending(changes);
  };

  const confirm = () => {
    if (!pending) return;
    update.mutate(pending, {
      onSuccess: (result) => {
        setDraft(retentionDraft(result));
        setPending(null);
      },
      onError: () => setPending(null),
    });
  };

  const changed = pending ? retentionKeys.filter((key) => pending[key] !== undefined) : [];

  return (
    <form onSubmit={submit} noValidate>
      <UnsavedChangesGuard dirty={dirty && !update.isPending} />
      <Section
        title="Telemetry retention"
        description="Limits for stored metrics, log history, the audit log and operation history across every team. Cove removes the oldest eligible data first when a limit is reached."
        width="form"
        flush
        footer={
          <SaveBar
            dirty={dirty}
            pending={update.isPending}
            blocked={!online}
            onDiscard={() => {
              setDraft(initial);
              setErrors({});
              update.reset();
            }}
            status={update.isSuccess ? "Saved. Telemetry beyond the new limits was removed; operation history is pruned by the next node passes." : undefined}
          />
        }
      >
        <div className="divide-y divide-line">
          {update.isError && (
            <div className="p-4">
              <Notice tone="error" title="Could not save retention">
                {errorMessage(update.error)}
              </Notice>
            </div>
          )}
          {groups.map((group) => (
            <Subsection key={group.title} title={group.title} description={group.description}>
              <SettingRows>
                {group.keys.map((key) => {
                  const id = `retention-${key}`;
                  return (
                    <Field key={key} id={id} label={labels[key].label} hint={hint(key)} error={errors[key]} layout="row">
                      <input
                        id={id}
                        name={key}
                        type="number"
                        inputMode={isByteLimit(key) ? "decimal" : "numeric"}
                        min={retentionLimits[key].min}
                        max={retentionLimits[key].max}
                        step={isByteLimit(key) ? "any" : 1}
                        className={`${inputClass} tabular-nums`}
                        value={draft[key]}
                        onChange={(event) => setDraft({ ...draft, [key]: event.target.value })}
                        aria-invalid={errors[key] ? true : undefined}
                        aria-describedby={errors[key] ? `${id}-error` : `${id}-hint`}
                      />
                    </Field>
                  );
                })}
              </SettingRows>
            </Subsection>
          ))}
        </div>
      </Section>
      <ConfirmDialog open={pending !== null} title="Save and delete data beyond the new limits?" confirmLabel="Save and delete" danger pending={update.isPending} onCancel={() => setPending(null)} onConfirm={confirm}>
        <p>Saving applies immediately. Stored samples, log lines and audit entries that are older than or beyond the new limits are permanently deleted. Audit entries still waiting for audit export delivery are kept until delivered. Raising a limit later does not bring them back.</p>
        <ul className="mt-3 space-y-1 text-fg">
          {changed.map((key) => (
            <li key={key}>
              {groupNames[key]}, {labels[key].label.toLowerCase()}: {retentionValueLabel(key, saved[key])} to {retentionValueLabel(key, pending?.[key] ?? saved[key])}
            </li>
          ))}
        </ul>
        <p className="mt-3">Eligible operation records and their worker journals beyond the operation limits are also permanently deleted, in bounded passes over the next minutes. Records that recovery, releases, backups or unfinished work refer to are kept.</p>
      </ConfirmDialog>
    </form>
  );
}

export function TelemetryRetention() {
  const retention = useQuery(retentionQuery);
  if (retention.isPending) {
    return (
      <Section title="Telemetry retention" width="form" flush>
        <LoadingRows label="Loading retention settings" rows={3} />
      </Section>
    );
  }
  if (retention.isError) {
    return (
      <Section title="Telemetry retention" width="form">
        <QueryError what="retention settings" error={retention.error} onRetry={() => void retention.refetch()} retrying={retention.isFetching} />
      </Section>
    );
  }
  return <RetentionForm saved={retention.data} />;
}

function rows(count: number) {
  return count.toLocaleString("en");
}

function basis(category: StorageCategory) {
  return category.basis ? category.basis.charAt(0).toUpperCase() + category.basis.slice(1) : "Not reported";
}

export function StorageUsage() {
  const usage = useQuery(storageUsageQuery);
  const retention = useQuery(retentionQuery);
  const limits = retention.data;

  let body;
  if (usage.isPending) {
    body = <LoadingRows label="Loading storage usage" rows={4} />;
  } else if (usage.isError) {
    body = (
      <div className="p-3">
        {isStatus(usage.error, 503) ? (
          <Notice tone="warn" title="Storage usage unavailable" action={<Button variant="ghost" size="sm" onClick={() => void usage.refetch()} pending={usage.isFetching}>Retry</Button>}>
            {errorMessage(usage.error)}
          </Notice>
        ) : (
          <QueryError what="storage usage" error={usage.error} onRetry={() => void usage.refetch()} retrying={usage.isFetching} />
        )}
      </div>
    );
  } else {
    const data = usage.data;
    const items = [
      { key: "metrics", label: "Metric samples", category: data.metrics, limit: limits ? `${rows(limits.metric_rows)} rows, ${formatBinaryBytes(limits.metric_bytes)}` : undefined },
      { key: "logs", label: "Log lines", category: data.logs, limit: limits ? `${rows(limits.log_rows)} rows, ${formatBinaryBytes(limits.log_bytes)}, ${limits.log_days} days` : undefined },
      { key: "audit", label: "Audit entries", category: data.audit, limit: limits ? `${limits.audit_days} days, plus pending deliveries` : undefined },
      { key: "operations", label: "Operations and worker journals", category: data.operations, limit: limits ? `${rows(limits.operation_rows)} eligible rows, ${limits.operation_days} days` : undefined },
      ...[
        { key: "traffic", label: "Traffic access entries", category: data.traffic },
        { key: "uptime", label: "Uptime check results", category: data.uptime },
        { key: "forwarding", label: "Log forwarding queue", category: data.forwarding },
      ].flatMap(({ category, ...item }) => (category ? [{ ...item, category, limit: category.limit }] : [])),
    ];
    body = (
      <>
        {limits && (
          <div className="border-b border-line p-4">
            <MeterGroup label="Accounted telemetry size" columns={3}>
              <Meter label="Metric history" value={data.metrics.bytes} limit={limits.metric_bytes} format={formatBinaryBytes} detail={`${rows(data.metrics.rows)} of ${rows(limits.metric_rows)} rows`} />
              <Meter label="Log history" value={data.logs.bytes} limit={limits.log_bytes} format={formatBinaryBytes} detail={`${rows(data.logs.rows)} of ${rows(limits.log_rows)} rows`} />
            </MeterGroup>
          </div>
        )}
        <table className="w-full text-left">
          <caption className="sr-only">Stored data by category</caption>
          <thead>
            <tr className={tableHeadClass}>
              <th scope="col" className="py-2 pr-4 pl-4 font-medium">
                Data
              </th>
              <th scope="col" className="py-2 pr-4 text-right font-medium">
                Rows
              </th>
              <th scope="col" className="py-2 pr-4 text-right font-medium">
                Size
              </th>
              <th scope="col" className="hidden py-2 pr-4 font-medium md:table-cell">
                Measured as
              </th>
              <th scope="col" className="hidden py-2 pr-4 font-medium lg:table-cell">
                Limit
              </th>
            </tr>
          </thead>
          <tbody>
            {items.map((item) => (
              <tr key={item.key} className={tableRowClass}>
                <td className="py-2.5 pr-4 pl-4 text-[13px] text-fg">
                  {item.label}
                  <span className="mt-0.5 block text-xs text-fg-muted md:hidden">{basis(item.category)}</span>
                  <span className="mt-0.5 block text-xs text-fg-muted lg:hidden">Limit: {item.limit ?? "Unknown"}</span>
                </td>
                <td className="py-2.5 pr-4 text-right text-[13px] text-fg-muted tabular-nums">{rows(item.category.rows)}</td>
                <td className="py-2.5 pr-4 text-right text-[13px] whitespace-nowrap text-fg-muted tabular-nums">{formatBinaryBytes(item.category.bytes)}</td>
                <td className="hidden py-2.5 pr-4 text-xs text-fg-muted md:table-cell">{basis(item.category)}</td>
                <td className="hidden py-2.5 pr-4 text-xs text-fg-muted lg:table-cell">{item.limit ?? "Unknown"}</td>
              </tr>
            ))}
          </tbody>
        </table>
      </>
    );
  }

  return (
    <Section title="Stored data" description="Totals across every team." flush>
      {body}
      <div className="space-y-1.5 border-t border-line px-4 py-3 text-xs text-fg-muted">
        <p>Metric and log sizes are Cove's accounted payload plus a per-row allowance, not database file size. Space freed by pruning is reused by PostgreSQL; database files and indexes do not shrink until it is reclaimed.</p>
        <p>Audit and operation sizes are PostgreSQL table and index size. Audit entries older than the audit period are pruned about once a minute, except entries still waiting for audit export delivery.</p>
        <p>Operation records and worker journals are pruned in bounded batches: each ready node is checked at most every five minutes, and offline, busy or older workers are skipped until a later pass. Limits apply to eligible history only: records that recovery, releases, backups or unfinished work refer to are kept, so operations can exceed their limits.</p>
      </div>
    </Section>
  );
}

function PassState({ status }: { status: OperationRetentionStatus }) {
  if (!status.checked_at) return <StatusLabel tone="neutral" label="Not checked yet" />;
  if (status.error) return <StatusLabel tone="warn" label="Deferred" />;
  return <StatusLabel tone="ok" label="Checked" />;
}

export function OperationRetentionPasses() {
  const statuses = useQuery(operationRetentionQuery);
  const run = useRunOperationRetention();
  const online = useOnline();
  const [confirming, setConfirming] = useState(false);

  let body;
  if (statuses.isPending) {
    body = <LoadingRows label="Loading operation retention status" rows={3} />;
  } else if (statuses.isError) {
    body = (
      <div className="p-3">
        {isStatus(statuses.error, 503) ? (
          <Notice tone="warn" title="Retention status unavailable" action={<Button variant="ghost" size="sm" onClick={() => void statuses.refetch()} pending={statuses.isFetching}>Retry</Button>}>
            {errorMessage(statuses.error)}
          </Notice>
        ) : (
          <QueryError what="operation retention status" error={statuses.error} onRetry={() => void statuses.refetch()} retrying={statuses.isFetching} />
        )}
      </div>
    );
  } else if (statuses.data.length === 0) {
    body = <p className="px-4 py-6 text-[13px] text-fg-muted">No nodes are registered, so there is no operation history to prune.</p>;
  } else {
    body = (
      <table className="w-full text-left">
        <caption className="sr-only">Last operation retention pass by node</caption>
        <thead>
          <tr className={tableHeadClass}>
            <th scope="col" className="py-2 pr-4 pl-4 font-medium">Node</th>
            <th scope="col" className="hidden py-2 pr-4 font-medium sm:table-cell">Last pass</th>
            <th scope="col" className="py-2 pr-4 text-right font-medium">Pruned</th>
            <th scope="col" className="py-2 pr-4 text-right font-medium">Kept</th>
          </tr>
        </thead>
        <tbody>
          {statuses.data.map((status) => (
            <tr key={status.node_id} className={tableRowClass}>
              <td className="py-2.5 pr-4 pl-4 align-top text-[13px] text-fg">
                <span className="break-words">{status.name}</span>
                <span className="mt-1 block text-xs"><PassState status={status} /></span>
                <span className="mt-0.5 block text-xs text-fg-muted sm:hidden">{status.checked_at ? <RelativeTime value={status.checked_at} /> : "Never"}</span>
                {status.error && <span className="mt-0.5 block text-xs break-words text-warn">{status.error}</span>}
              </td>
              <td className="hidden py-2.5 pr-4 align-top text-xs text-fg-muted sm:table-cell">{status.checked_at ? <RelativeTime value={status.checked_at} /> : "Never"}</td>
              <td className="py-2.5 pr-4 text-right align-top text-[13px] text-fg-muted tabular-nums">{rows(status.pruned)}</td>
              <td className="py-2.5 pr-4 text-right align-top text-[13px] text-fg-muted tabular-nums">{rows(status.protected)}</td>
            </tr>
          ))}
        </tbody>
      </table>
    );
  }

  return (
    <Section
      title="Operation retention"
      description="The last bounded pass on each node. Pruned and kept are counts from that pass only, not totals; kept counts journals the worker protected for recovery and is not a census of every protected record."
      actions={
        <Button onClick={() => { run.reset(); setConfirming(true); }} disabled={!online || statuses.isPending} pending={run.isPending}>
          Run retention pass
        </Button>
      }
      flush
    >
      {(run.isError || run.isSuccess) && (
        <div className="border-b border-line p-4" aria-live="polite">
          {run.isError ? (
            <Notice tone="error" title="Retention pass did not finish">
              {errorMessage(run.error)} Some nodes may already have been pruned; the status below was read again. Check it before running another pass.
            </Notice>
          ) : (
            <Notice tone="success" title="Retention pass finished">Up to eight nodes were checked. Nodes that were offline, busy or running an older worker are deferred to a later pass.</Notice>
          )}
        </div>
      )}
      {body}
      <ConfirmDialog
        open={confirming}
        title="Run an operation retention pass now?"
        confirmLabel="Run pass and delete"
        danger
        pending={run.isPending}
        onCancel={() => setConfirming(false)}
        onConfirm={() => run.mutate(undefined, { onSettled: () => setConfirming(false) })}
      >
        <p>Up to eight ready nodes are checked now. Eligible finished operation records and their worker journals beyond the configured limits are permanently deleted.</p>
        <p className="mt-3">Records that recovery, releases, backups or unfinished work refer to are kept. Offline, busy and older workers are skipped until a later pass.</p>
      </ConfirmDialog>
    </Section>
  );
}

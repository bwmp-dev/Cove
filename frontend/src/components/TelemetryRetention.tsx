import { useQuery } from "@tanstack/react-query";
import { useState, type FormEvent } from "react";
import { errorMessage, isStatus } from "../api/client";
import { retentionQuery, storageUsageQuery, useUpdateRetention, type RetentionSettings, type StorageCategory } from "../api/telemetry";
import { useOnline } from "../lib/hooks";
import { inputClass, tableHeadClass, tableRowClass } from "../lib/styles";
import { formatBinaryBytes, isByteLimit, parseRetention, retentionChanges, retentionDraft, retentionKeys, retentionLimits, retentionValueLabel, type RetentionDraft, type RetentionErrors } from "../lib/telemetry";
import { ConfirmDialog } from "./ConfirmDialog";
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
  audit_rows: { label: "Stored entries", hint: "Entries across all teams." },
  audit_days: { label: "Keep entries for", hint: "Days." },
};

const groups: { title: string; description: string; keys: RetentionKey[] }[] = [
  { title: "Metric history", description: "Node and container samples behind the usage charts.", keys: ["metric_rows", "metric_bytes", "raw_days", "hour_days", "day_days"] },
  { title: "Log history", description: "Stored container log lines.", keys: ["log_rows", "log_bytes", "log_days"] },
  { title: "Audit log", description: "Security and change events.", keys: ["audit_rows", "audit_days"] },
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
        description="Limits for stored metrics, log history and the audit log across every team. Cove removes the oldest data first when a limit is reached."
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
            status={update.isSuccess ? "Saved. Data beyond the new limits was removed." : undefined}
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
        <p>Saving applies immediately. Stored samples, log lines and audit entries that are older than or beyond the new limits are permanently deleted. Raising a limit later does not bring them back.</p>
        <ul className="mt-3 space-y-1 text-fg">
          {changed.map((key) => (
            <li key={key}>
              {groupNames[key]}, {labels[key].label.toLowerCase()}: {retentionValueLabel(key, saved[key])} to {retentionValueLabel(key, pending?.[key] ?? saved[key])}
            </li>
          ))}
        </ul>
        <p className="mt-3">Operation records and worker journals are not affected.</p>
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
      { key: "audit", label: "Audit entries", category: data.audit, limit: limits ? `${rows(limits.audit_rows)} rows, ${limits.audit_days} days` : undefined },
      { key: "operations", label: "Operations and worker journals", category: data.operations, limit: "Not pruned" },
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
        <p>Audit and operation sizes are PostgreSQL table and index size. The audit log can briefly exceed its row limit between pruning passes, which run about once a minute.</p>
        <p>Operation records and worker journals are never pruned by these settings, because recovery, releases and backups refer to them.</p>
      </div>
    </Section>
  );
}

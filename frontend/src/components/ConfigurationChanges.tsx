import { ArrowRight } from "lucide-react";
import type { ConfigurationChange, FieldDiff } from "../api/project-configuration";
import { formatDiffValue } from "../lib/project-configuration";

// Field names plus the safe before/after values the server chose to reveal. Fields without a safe value
// (variables, files, credentials) are listed by name only.
export function FieldChanges({ fields, diffs }: { fields: readonly string[] | null; diffs: readonly FieldDiff[] | null }) {
  const shown = diffs ?? [];
  const withValues = new Set(shown.map((diff) => diff.field));
  const nameOnly = (fields ?? []).filter((field) => !withValues.has(field));
  if (shown.length === 0 && nameOnly.length === 0) return null;
  return (
    <div className="min-w-0 space-y-2">
      {shown.length > 0 && (
        <dl className="space-y-1.5 text-xs">
          {shown.map((diff) => (
            <div key={diff.field} className="grid min-w-0 gap-x-3 gap-y-0.5 sm:grid-cols-[minmax(0,10rem)_minmax(0,1fr)]">
              <dt className="font-mono break-all text-fg-muted">{diff.field}</dt>
              <dd className="min-w-0 font-mono break-all">
                <span className="text-fg-muted line-through decoration-fg-subtle">{formatDiffValue(diff.before)}</span>
                <ArrowRight aria-hidden className="mx-1.5 inline size-3 text-fg-subtle" />
                <span className="sr-only"> changes to </span>
                <span className="text-fg">{formatDiffValue(diff.after)}</span>
              </dd>
            </div>
          ))}
        </dl>
      )}
      {nameOnly.length > 0 && (
        <p className="text-xs text-fg-muted">
          {shown.length > 0 ? "Also changed: " : "Changed: "}
          <span className="font-mono break-all text-fg">{nameOnly.join(", ")}</span>
          <span> (values not shown)</span>
        </p>
      )}
    </div>
  );
}

export function ChangeList({ changes, label }: { changes: readonly ConfigurationChange[] | null; label: string }) {
  const items = changes ?? [];
  if (items.length === 0) return <p className="text-[13px] text-fg-muted">No declaration changes.</p>;
  return (
    <ul aria-label={label} className="divide-y divide-line rounded-md border border-line">
      {items.map((change) => (
        <li key={`${change.action}:${change.resource}`} className="min-w-0 space-y-1.5 px-3 py-2.5">
          <p className="flex flex-wrap items-baseline gap-x-2 text-[13px]">
            <span className="font-mono text-xs break-all text-fg">{change.resource}</span>
            <span className={`text-xs ${change.action === "create" ? "text-ok" : "text-info"}`}>{change.action === "create" ? "Created" : "Updated"}</span>
          </p>
          <FieldChanges fields={change.fields} diffs={change.diffs} />
        </li>
      ))}
    </ul>
  );
}

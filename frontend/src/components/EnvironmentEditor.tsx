import { Eye, EyeOff, Plus, Trash2 } from "lucide-react";
import { useState, type ClipboardEvent } from "react";
import { createRow, parseDotenv, type EnvRow } from "../lib/workload-form";
import { Button } from "./ui";
import { inputClass } from "../lib/styles";

interface EnvironmentEditorProps {
  rows: EnvRow[];
  errors?: Record<string, string>;
  onChange: (rows: EnvRow[]) => void;
}

export function EnvironmentEditor({ rows, errors = {}, onChange }: EnvironmentEditorProps) {
  const [revealed, setRevealed] = useState<ReadonlySet<string>>(() => new Set());

  function update(id: string, patch: Partial<EnvRow>) {
    onChange(rows.map((row) => (row.id === id ? { ...row, ...patch } : row)));
  }

  function remove(id: string) {
    onChange(rows.filter((row) => row.id !== id));
  }

  function toggle(id: string) {
    setRevealed((current) => {
      const next = new Set(current);
      if (next.has(id)) next.delete(id);
      else next.add(id);
      return next;
    });
  }

  // Pasting a multi-line .env file into a name field expands into one row per variable.
  function paste(event: ClipboardEvent<HTMLInputElement>, row: EnvRow) {
    const text = event.clipboardData.getData("text");
    if (!text.includes("\n") && !text.includes("=")) return;
    const entries = parseDotenv(text);
    if (!entries) return;
    event.preventDefault();
    const index = rows.findIndex((item) => item.id === row.id);
    const replacement = entries.map((entry) => createRow(entry.key, entry.value));
    const keepCurrent = row.key !== "" || row.value !== "";
    const next = [...rows];
    next.splice(keepCurrent ? index + 1 : index, keepCurrent ? 0 : 1, ...replacement);
    onChange(next);
  }

  return (
    <div className="space-y-2">
      {rows.length > 0 && (
        <div className="hidden grid-cols-[minmax(0,2fr)_minmax(0,3fr)_4.5rem] gap-2 text-xs text-fg-subtle sm:grid">
          <span>Name</span>
          <span>Value</span>
        </div>
      )}
      <ul className="space-y-2">
        {rows.map((row, index) => {
          const error = errors[row.id];
          const shown = revealed.has(row.id);
          const label = row.key || `variable ${index + 1}`;
          return (
            <li key={row.id}>
              <div className="grid grid-cols-[minmax(0,1fr)_4.5rem] gap-2 sm:grid-cols-[minmax(0,2fr)_minmax(0,3fr)_4.5rem]">
                <input
                  aria-label={`Name of ${label}`}
                  className={`${inputClass} font-mono`}
                  value={row.key}
                  onChange={(event) => update(row.id, { key: event.target.value })}
                  onPaste={(event) => paste(event, row)}
                  placeholder="DATABASE_URL"
                  autoComplete="off"
                  autoCapitalize="none"
                  spellCheck={false}
                  aria-invalid={error ? true : undefined}
                  aria-describedby={error ? `${row.id}-error` : undefined}
                />
                <input
                  aria-label={`Value of ${label}`}
                  className={`${inputClass} col-start-1 row-start-2 font-mono sm:col-start-auto sm:row-start-auto`}
                  type={shown ? "text" : "password"}
                  value={row.value}
                  onChange={(event) => update(row.id, { value: event.target.value })}
                  autoComplete="off"
                  spellCheck={false}
                  data-1p-ignore
                  data-lpignore="true"
                />
                <div className="col-start-2 row-span-2 row-start-1 flex items-start gap-1 sm:col-start-auto sm:row-span-1 sm:row-start-auto">
                  <button
                    type="button"
                    onClick={() => toggle(row.id)}
                    aria-label={shown ? `Hide value of ${label}` : `Show value of ${label}`}
                    aria-pressed={shown}
                    title={shown ? "Hide value" : "Show value"}
                    className="inline-flex size-8 items-center justify-center rounded-md text-fg-muted hover:bg-hover hover:text-fg"
                  >
                    {shown ? <EyeOff aria-hidden className="size-4" /> : <Eye aria-hidden className="size-4" />}
                  </button>
                  <button
                    type="button"
                    onClick={() => remove(row.id)}
                    aria-label={`Remove ${label}`}
                    title="Remove variable"
                    className="inline-flex size-8 items-center justify-center rounded-md text-fg-muted hover:bg-hover hover:text-bad"
                  >
                    <Trash2 aria-hidden className="size-4" />
                  </button>
                </div>
              </div>
              {error && (
                <p id={`${row.id}-error`} className="mt-1 text-xs text-bad">
                  {error}
                </p>
              )}
            </li>
          );
        })}
      </ul>
      <div className="flex flex-wrap items-center gap-3 pt-1">
        <Button variant="secondary" onClick={() => onChange([...rows, createRow()])}>
          <Plus aria-hidden className="size-3.5" />
          Add variable
        </Button>
        <span className="text-xs text-fg-subtle">Paste a .env file into a name field to add several at once.</span>
      </div>
    </div>
  );
}

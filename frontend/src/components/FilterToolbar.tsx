import { Search, X } from "lucide-react";
import type { ReactNode } from "react";
import { inputClass } from "../lib/styles";
import { Select } from "./Select";
import { Button } from "./ui";

interface FilterToolbarProps {
  label: string;
  search: string;
  onSearch: (value: string) => void;
  searchPlaceholder: string;
  // Extra filters, usually FilterSelect.
  children?: ReactNode;
  active: boolean;
  onReset: () => void;
  // Counts what is shown, for example "3 of 12 operations".
  summary: ReactNode;
}

// Filters over rows that are already loaded, placed at the top of a flush Section above its table.
export function FilterToolbar({ label, search, onSearch, searchPlaceholder, children, active, onReset, summary }: FilterToolbarProps) {
  const searchId = `${label.toLowerCase().replace(/\W+/g, "-")}-search`;
  return (
    <search aria-label={label} className="flex flex-wrap items-center gap-2 border-b border-line px-3 py-2.5">
      <div className="relative w-full sm:w-64">
        <label htmlFor={searchId} className="sr-only">
          {searchPlaceholder}
        </label>
        <Search aria-hidden className="pointer-events-none absolute top-1/2 left-2.5 size-3.5 -translate-y-1/2 text-fg-subtle" />
        <input id={searchId} type="search" className={`${inputClass} min-h-9 pl-8 pointer-coarse:min-h-11`} placeholder={searchPlaceholder} value={search} onChange={(event) => onSearch(event.target.value)} autoComplete="off" />
      </div>
      {children}
      {active && (
        <Button variant="ghost" size="sm" onClick={onReset}>
          <X aria-hidden className="size-3.5" />
          Reset filters
        </Button>
      )}
      <p role="status" className="ml-auto text-xs text-fg-muted tabular-nums">
        {summary}
      </p>
    </search>
  );
}

export function FilterSelect({ id, label, value, onChange, children }: { id: string; label: string; value: string; onChange: (value: string) => void; children: ReactNode }) {
  return (
    <div className="min-w-0 flex-1 basis-36 sm:w-48 sm:flex-none sm:basis-auto">
      <label htmlFor={id} className="sr-only">
        {label}
      </label>
      <Select id={id} aria-label={label} className={inputClass} value={value} onChange={(event) => onChange(event.target.value)}>
        {children}
      </Select>
    </div>
  );
}

export function NoMatches({ what, scope, onReset }: { what: string; scope?: string; onReset: () => void }) {
  return (
    <div className="flex flex-wrap items-center justify-between gap-3 px-4 py-6">
      <div className="min-w-0">
        <p className="text-[13px] font-medium text-fg">No matching {what}</p>
        <p className="mt-0.5 text-xs text-fg-muted">{scope ?? `No ${what} match the search and filters.`}</p>
      </div>
      <Button size="sm" onClick={onReset}>
        Reset filters
      </Button>
    </div>
  );
}

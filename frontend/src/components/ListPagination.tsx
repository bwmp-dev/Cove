import { ChevronLeft, ChevronRight } from "lucide-react";
import { useState, type ReactNode } from "react";
import { pageWindow } from "../lib/organization";
import { Button } from "./ui";

interface PaginationControlsProps {
  label: string;
  summary: ReactNode;
  page: number;
  hasPrevious: boolean;
  hasNext: boolean;
  onPage: (page: number) => void;
  pageCount?: number;
}

export function PaginationControls({ label, summary, page, hasPrevious, hasNext, onPage, pageCount }: PaginationControlsProps) {
  return (
    <nav aria-label={label} className="flex flex-wrap items-center justify-between gap-2 border-t border-line px-3 py-2">
      <p className="text-xs text-fg-muted tabular-nums" aria-live="polite">
        {summary}
      </p>
      <div className="flex items-center gap-2">
        <Button size="sm" variant="ghost" onClick={() => onPage(page - 1)} disabled={!hasPrevious}>
          <ChevronLeft aria-hidden className="size-3.5" />
          Previous
        </Button>
        <span className="text-xs text-fg-muted tabular-nums">{pageCount ? `Page ${page} of ${pageCount}` : `Page ${page}`}</span>
        <Button size="sm" variant="ghost" onClick={() => onPage(page + 1)} disabled={!hasNext}>
          Next
          <ChevronRight aria-hidden className="size-3.5" />
        </Button>
      </div>
    </nav>
  );
}

interface ClientPaginationProps<T> {
  rows: T[];
  pageSize?: number;
  // Changing it (for example when filters change) returns to the first page.
  resetKey?: string;
  label: string;
  noun: string;
  children: (rows: T[]) => ReactNode;
}

// Pages through rows that are already loaded and filtered; it never fetches or implies server-wide results.
export function ClientPagination<T>({ rows, pageSize = 25, resetKey = "", label, noun, children }: ClientPaginationProps<T>) {
  const [state, setState] = useState({ key: resetKey, page: 1 });
  const requested = state.key === resetKey ? state.page : 1;
  const { page, pageCount, start, end } = pageWindow(rows.length, pageSize, requested);
  const visible = rows.length > pageSize ? rows.slice(start, end) : rows;
  return (
    <>
      {children(visible)}
      {rows.length > pageSize && (
        <PaginationControls
          label={label}
          summary={`Showing ${start + 1}-${end} of ${rows.length} ${noun}`}
          page={page}
          pageCount={pageCount}
          hasPrevious={page > 1}
          hasNext={page < pageCount}
          onPage={(next) => setState({ key: resetKey, page: next })}
        />
      )}
    </>
  );
}

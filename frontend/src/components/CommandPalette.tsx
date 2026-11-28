import { useQuery } from "@tanstack/react-query";
import { useNavigate } from "@tanstack/react-router";
import { Activity, ArrowDown, ArrowUp, Boxes, CornerDownLeft, File, Folder, Keyboard, Layers, Search, Server, type LucideIcon } from "lucide-react";
import { Fragment, useEffect, useId, useRef, useState, type KeyboardEvent, type ReactNode } from "react";
import { errorMessage } from "../api/client";
import { searchQuery, type SearchItem, type SearchKind } from "../api/search";
import { withBasePath } from "../lib/base-path";
import { closePalette, openHelp } from "../lib/command-palette";
import { isSearchPath, maxSearchBytes, searchBytes } from "../lib/search";
import { Button, Spinner } from "./ui";

const debounceMs = 200;

const kindGroups: Record<SearchKind, { label: string; icon: LucideIcon }> = {
  workload: { label: "Resources", icon: Boxes },
  project: { label: "Projects", icon: Folder },
  environment: { label: "Environments", icon: Layers },
  node: { label: "Nodes", icon: Server },
  operation: { label: "Operations", icon: Activity },
  page: { label: "Pages", icon: File },
};

type PaletteItem = { type: "result"; key: string; group: string; icon: LucideIcon; name: string; detail: string; path: string } | { type: "help"; key: string; group: string; icon: LucideIcon; name: string; detail: string };

const helpItem: PaletteItem = { type: "help", key: "help", group: "Commands", icon: Keyboard, name: "Keyboard shortcuts", detail: "Show shortcuts and turn them on or off" };

function matchesHelp(text: string) {
  const needle = text.trim().toLowerCase();
  return needle === "" || ["keyboard shortcuts", "help", "shortcuts"].some((word) => word.includes(needle));
}

function toItems(results: SearchItem[]): PaletteItem[] {
  return results
    .filter((item) => isSearchPath(item.path))
    .map((item) => ({ type: "result", key: `${item.kind}:${item.path}:${item.name}`, group: kindGroups[item.kind].label, icon: kindGroups[item.kind].icon, name: item.name, detail: item.detail, path: item.path }));
}

export function CommandPalette({ team, open }: { team: string; open: boolean }) {
  const ref = useRef<HTMLDialogElement>(null);
  const titleId = useId();

  useEffect(() => {
    const dialog = ref.current;
    if (!dialog) return;
    if (open && !dialog.open) {
      dialog.showModal();
      dialog.querySelector("input")?.focus();
    }
    if (!open && dialog.open) dialog.close();
  }, [open]);

  return (
    <dialog
      ref={ref}
      aria-labelledby={titleId}
      onClose={() => closePalette()}
      className="mx-auto mt-16 mb-auto w-[calc(100%-2rem)] max-w-xl rounded-lg border border-line-strong bg-panel p-0 text-fg shadow-2xl"
    >
      <h2 id={titleId} className="sr-only">
        Search Cove
      </h2>
      {/* Mounted only while open, so text and results are discarded on close and on team switch. */}
      {open && <PaletteContent key={team} team={team} />}
    </dialog>
  );
}

function PaletteContent({ team }: { team: string }) {
  const navigate = useNavigate();
  const listId = useId();
  const statusId = useId();
  const [text, setText] = useState("");
  const [debounced, setDebounced] = useState("");
  const [active, setActive] = useState(0);
  const tooLong = searchBytes(text.trim()) > maxSearchBytes;

  useEffect(() => {
    const timer = window.setTimeout(() => setDebounced(text.trim()), debounceMs);
    return () => window.clearTimeout(timer);
  }, [text]);

  const results = useQuery({ ...searchQuery(team, debounced), enabled: !tooLong && searchBytes(debounced) <= maxSearchBytes });
  const settled = debounced === text.trim();
  const waiting = !tooLong && (!settled || results.isFetching);
  const items: PaletteItem[] = [...(tooLong || !settled || !results.data ? [] : toItems(results.data)), ...(matchesHelp(text) ? [helpItem] : [])];
  const current = items.length === 0 ? -1 : Math.min(active, items.length - 1);
  const optionId = (index: number) => `${listId}-${index}`;

  const choose = (item: PaletteItem | undefined) => {
    if (!item) return;
    if (item.type === "help") {
      openHelp();
      return;
    }
    closePalette(false);
    void navigate({ href: withBasePath(item.path) });
  };

  const onKeyDown = (event: KeyboardEvent<HTMLInputElement>) => {
    if (items.length === 0) return;
    if (event.key === "ArrowDown" || event.key === "ArrowUp") {
      event.preventDefault();
      const step = event.key === "ArrowDown" ? 1 : -1;
      const next = (current + step + items.length) % items.length;
      setActive(next);
      document.getElementById(optionId(next))?.scrollIntoView({ block: "nearest" });
    } else if (event.key === "Enter") {
      event.preventDefault();
      choose(items[current]);
    }
  };

  // Counts are announced only; loading, empty and limit states are also shown.
  let status = "";
  let showStatus = true;
  if (tooLong) status = `Search text can be at most ${maxSearchBytes} bytes.`;
  else if (waiting) status = "Searching…";
  else if (results.isError) status = "";
  else if (results.data && items.length === 0) status = debounced ? `No matches for “${debounced}”.` : "Nothing to suggest yet.";
  else if (results.data) {
    status = `${items.length} ${items.length === 1 ? "result" : "results"}.`;
    showStatus = false;
  }

  return (
    <div>
      <div className="flex items-center gap-2.5 border-b border-line px-3">
        <Search aria-hidden className="size-4 shrink-0 text-fg-subtle" />
        <input
          type="text"
          role="combobox"
          aria-label="Search projects, resources, nodes and pages"
          aria-expanded={items.length > 0}
          aria-controls={listId}
          aria-activedescendant={current >= 0 ? optionId(current) : undefined}
          aria-autocomplete="list"
          aria-describedby={statusId}
          aria-invalid={tooLong || undefined}
          autoComplete="off"
          spellCheck={false}
          value={text}
          onChange={(event) => {
            setText(event.target.value);
            setActive(0);
          }}
          onKeyDown={onKeyDown}
          placeholder="Search projects, resources, nodes and pages"
          className="h-11 min-w-0 flex-1 bg-transparent text-[13px] text-fg placeholder:text-fg-subtle focus:outline-none focus-visible:outline-none"
        />
        {waiting && <Spinner className="size-3.5 text-fg-subtle" />}
      </div>

      <div className="max-h-[min(60vh,28rem)] overflow-y-auto p-1.5">
        {results.isError && !waiting && !tooLong && (
          <div role="alert" className="flex flex-wrap items-center justify-between gap-2 px-2.5 py-2 text-[13px]">
            <span className="min-w-0 text-bad">{errorMessage(results.error)}</span>
            <Button variant="ghost" size="sm" onClick={() => void results.refetch()}>
              Retry
            </Button>
          </div>
        )}
        <div id={listId} role="listbox" aria-label="Search results">
          {items.map((item, index) => {
            const Icon = item.icon;
            return (
              <Fragment key={item.key}>
                {item.group !== items[index - 1]?.group && (
                  <div role="presentation" className="px-2.5 pt-2 pb-1 text-[11px] font-medium text-fg-subtle">
                    {item.group}
                  </div>
                )}
                <div
                  id={optionId(index)}
                  role="option"
                  tabIndex={-1}
                  aria-selected={index === current}
                  onPointerMove={() => setActive(index)}
                  onClick={() => choose(item)}
                  onKeyDown={(event) => {
                    if (event.key === "Enter" || event.key === " ") {
                      event.preventDefault();
                      choose(item);
                    }
                  }}
                  className={`flex min-h-9 cursor-default items-center gap-2.5 rounded-md px-2.5 py-1.5 text-[13px] pointer-coarse:min-h-11 ${index === current ? "bg-hover text-fg" : "text-fg-muted"}`}
                >
                  <Icon aria-hidden className="size-4 shrink-0 text-fg-subtle" />
                  <span className="min-w-0 flex-1">
                    <span className="block truncate text-fg" title={item.name}>
                      {item.name}
                    </span>
                    {item.detail && <span className="block truncate text-xs text-fg-muted">{item.detail}</span>}
                  </span>
                  {index === current && <CornerDownLeft aria-hidden className="size-3.5 shrink-0 text-fg-subtle" />}
                </div>
              </Fragment>
            );
          })}
        </div>
        <p id={statusId} role="status" className={status && showStatus ? "px-2.5 py-2 text-xs text-fg-muted" : "sr-only"}>
          {status}
        </p>
      </div>

      <div className="flex flex-wrap items-center gap-x-4 gap-y-1 border-t border-line px-3 py-2 text-xs text-fg-subtle">
        <span>
          <Key label="Up arrow">
            <ArrowUp aria-hidden className="size-3" />
          </Key>{" "}
          <Key label="Down arrow">
            <ArrowDown aria-hidden className="size-3" />
          </Key>{" "}
          to move
        </span>
        <span>
          <Key>Enter</Key> to open
        </span>
        <span>
          <Key>Esc</Key> to close
        </span>
      </div>
    </div>
  );
}

export function Key({ children, label }: { children: ReactNode; label?: string }) {
  return (
    <kbd aria-label={label} className="inline-flex h-5 min-w-5 items-center justify-center rounded-md border border-line-strong bg-raised px-1 align-middle font-sans text-[11px] text-fg-muted">
      {children}
    </kbd>
  );
}

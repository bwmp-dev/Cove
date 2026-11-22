import { Settings } from "lucide-react";
import { useCallback, useId, useRef, useState, type KeyboardEvent } from "react";
import { createPortal } from "react-dom";
import { floatingPanelClass, useFloatingPanel } from "./useFloatingPanel";

export interface MenuAction {
  label: string;
  onSelect: () => void;
  danger?: boolean;
  // Explains why the action is unavailable; the item stays focusable so the reason can be read.
  disabledReason?: string;
}

// Menu button following the WAI-ARIA menu pattern: arrow keys, Home/End, Escape and Tab.
export function ActionMenu({ label, actions }: { label: string; actions: MenuAction[] }) {
  const id = useId();
  const [open, setOpen] = useState(false);
  const [portalHost, setPortalHost] = useState<HTMLElement | null>(null);
  const button = useRef<HTMLButtonElement>(null);
  const menu = useRef<HTMLDivElement>(null);
  const items = useRef<(HTMLButtonElement | null)[]>([]);

  const dismiss = useCallback(() => setOpen(false), []);
  useFloatingPanel({ open, id, trigger: button, panel: menu, onClose: dismiss, align: "end", width: 256 });

  function focusItem(index: number) {
    const count = actions.length;
    items.current[((index % count) + count) % count]?.focus();
  }

  function show(index: number) {
    setPortalHost(button.current?.closest("dialog") ?? document.body);
    setOpen(true);
    requestAnimationFrame(() => focusItem(index));
  }

  function close(returnFocus: boolean) {
    setOpen(false);
    if (returnFocus) button.current?.focus();
  }

  function onButtonKey(event: KeyboardEvent<HTMLButtonElement>) {
    if (event.key === "ArrowDown" || event.key === "ArrowUp") {
      event.preventDefault();
      show(event.key === "ArrowDown" ? 0 : actions.length - 1);
    }
  }

  function onMenuKey(event: KeyboardEvent<HTMLDivElement>) {
    const current = items.current.findIndex((item) => item === document.activeElement);
    const moves: Record<string, number> = { ArrowDown: current + 1, ArrowUp: current - 1, Home: 0, End: actions.length - 1 };
    if (event.key in moves) {
      event.preventDefault();
      focusItem(moves[event.key] ?? 0);
    } else if (event.key === "Escape") {
      event.preventDefault();
      close(true);
    } else if (event.key === "Tab") {
      close(true);
    }
  }

  return (
    <div
      className="relative"
      onBlur={(event) => {
        if (open && !event.currentTarget.contains(event.relatedTarget) && !menu.current?.contains(event.relatedTarget)) setOpen(false);
      }}
    >
      <button
        ref={button}
        type="button"
        aria-label={label}
        title={label}
        aria-haspopup="menu"
        aria-expanded={open}
        aria-controls={open ? id : undefined}
        onClick={() => (open ? close(false) : show(0))}
        onKeyDown={onButtonKey}
        className="inline-flex size-8 shrink-0 items-center justify-center rounded-md border border-line-strong bg-raised text-fg-muted hover:bg-hover hover:text-fg aria-expanded:bg-hover aria-expanded:text-fg"
      >
        <Settings aria-hidden className="size-4" />
      </button>
      {open && createPortal(
        <div
          ref={menu}
          id={id}
          role="menu"
          aria-label={label}
          tabIndex={-1}
          onKeyDown={onMenuKey}
          // Keeps focus on the menu while clicking so browsers that do not focus buttons on click still deliver the click.
          onMouseDown={(event) => event.preventDefault()}
          className={floatingPanelClass}
        >
          {actions.map((action, index) => (
            <button
              key={action.label}
              ref={(element) => {
                items.current[index] = element;
              }}
              type="button"
              role="menuitem"
              tabIndex={-1}
              aria-disabled={action.disabledReason ? true : undefined}
              onClick={() => {
                if (action.disabledReason) return;
                close(true);
                action.onSelect();
              }}
              className={`block min-h-9 w-full rounded-md px-2.5 py-2 text-left text-[13px] focus:bg-hover focus:outline-none focus-visible:outline-2 focus-visible:-outline-offset-2 focus-visible:outline-info aria-disabled:cursor-not-allowed ${
                action.danger ? "text-bad" : "text-fg"
              } ${action.disabledReason ? "opacity-60" : "hover:bg-hover"}`}
            >
              {action.label}
              {action.disabledReason && <span className="mt-0.5 block text-xs text-fg-muted">{action.disabledReason}</span>}
            </button>
          ))}
        </div>, portalHost ?? document.body
      )}
    </div>
  );
}

import { AlertTriangle, CheckCircle2, ChevronDown, ChevronRight, Info, LoaderCircle, RotateCw, XCircle } from "lucide-react";
import { Children, isValidElement, useId, useState, type ButtonHTMLAttributes, type ComponentProps, type ReactNode } from "react";
import { errorMessage } from "../api/client";
import { buttonClass, type Size, type Variant } from "../lib/styles";
import { ErrorRemediation } from "./ErrorRemediation";
import { Switch } from "./Switch";

interface ButtonProps extends ButtonHTMLAttributes<HTMLButtonElement> {
  variant?: Variant;
  size?: Size;
  pending?: boolean;
}

export function Button({ variant = "secondary", size = "md", pending = false, className = "", children, disabled, type = "button", ...props }: ButtonProps) {
  return (
    <button type={type} className={buttonClass(variant, className, size)} disabled={disabled || pending} aria-busy={pending || undefined} {...props}>
      {pending && <Spinner className={size === "sm" ? "size-3" : "size-3.5"} />}
      {children}
    </button>
  );
}

export function Spinner({ className = "size-3.5" }: { className?: string }) {
  return <LoaderCircle aria-hidden className={`${className} animate-spin`} />;
}

interface FieldProps {
  id: string;
  label: string;
  hint?: ReactNode;
  error?: string;
  children: ReactNode;
  optional?: boolean;
  // "row" puts the label and hint on the left and the control on the right, for settings forms inside SettingRows.
  layout?: "stack" | "row";
}

// Two columns once the surrounding Section body is at least 32rem wide; one column below that.
const settingRowClass = "grid min-w-0 gap-x-8 gap-y-2 @lg:grid-cols-[minmax(0,2fr)_minmax(0,3fr)]";

function FieldLabel({ id, label, optional }: { id: string; label: string; optional?: boolean }) {
  return (
    <label id={`${id}-label`} htmlFor={id} className="block text-[13px] font-medium text-fg">
      {label}
      {optional && <span className="ml-1.5 font-normal text-fg-subtle">Optional</span>}
    </label>
  );
}

export function Field({ id, label, hint, error, children, optional, layout = "stack" }: FieldProps) {
  if (layout === "row") {
    return (
      <div className={settingRowClass}>
        <div className="min-w-0 @lg:pt-1.5">
          <FieldLabel id={id} label={label} optional={optional} />
          {hint && (
            <p id={`${id}-hint`} className="mt-0.5 text-xs text-fg-muted">
              {hint}
            </p>
          )}
        </div>
        <div className="min-w-0 space-y-1.5">
          {children}
          {error && (
            <p id={`${id}-error`} className="text-xs text-bad">
              {error}
            </p>
          )}
        </div>
      </div>
    );
  }
  return (
    <div className="min-w-0 space-y-1.5">
      <FieldLabel id={id} label={label} optional={optional} />
      {children}
      {error ? (
        <p id={`${id}-error`} className="text-xs text-bad">
          {error}
        </p>
      ) : (
        hint && (
          <p id={`${id}-hint`} className="text-xs text-fg-muted">
            {hint}
          </p>
        )
      )}
    </div>
  );
}

type Tone = "info" | "warn" | "error" | "success";

const tones: Record<Tone, { className: string; icon: typeof Info }> = {
  info: { className: "border-info-line bg-info-tint text-fg", icon: Info },
  warn: { className: "border-warn-line bg-warn-tint text-fg", icon: AlertTriangle },
  error: { className: "border-bad-line bg-bad-tint text-fg", icon: XCircle },
  success: { className: "border-ok-line bg-ok-tint text-fg", icon: CheckCircle2 },
};

const iconTones: Record<Tone, string> = { info: "text-info", warn: "text-warn", error: "text-bad", success: "text-ok" };

export function Notice({ tone = "info", title, children, action }: { tone?: Tone; title?: string; children?: ReactNode; action?: ReactNode }) {
  const { className, icon: Icon } = tones[tone];
  return (
    <div role={tone === "error" ? "alert" : undefined} className={`flex flex-wrap items-start gap-x-2.5 gap-y-2 rounded-md border px-3 py-2.5 text-[13px] ${className}`}>
      <Icon aria-hidden className={`mt-0.5 size-4 shrink-0 ${iconTones[tone]}`} />
      <div className="min-w-0 flex-1 basis-48 space-y-0.5">
        {title && <p className="font-medium text-fg">{title}</p>}
        {children && <div className="text-fg-muted">{children}</div>}
      </div>
      {action && <div className="shrink-0">{action}</div>}
    </div>
  );
}

export function QueryError({ error, onRetry, retrying, what }: { error: unknown; onRetry: () => void; retrying?: boolean; what: string }) {
  return (
    <Notice
      tone="error"
      title={`Could not load ${what}`}
      action={
        <Button variant="ghost" size="sm" onClick={onRetry} pending={retrying}>
          {!retrying && <RotateCw aria-hidden className="size-3.5" />}
          Retry
        </Button>
      }
    >
      {errorMessage(error)}
      <ErrorRemediation error={error} />
    </Notice>
  );
}

// Pass links for ancestors and plain text for the current page, which is marked as the current location.
export function Breadcrumbs({ children }: { children: ReactNode }) {
  const items = Children.toArray(children);
  return (
    <nav aria-label="Breadcrumb" className="min-w-0">
      <ol className="flex min-w-0 flex-wrap items-center gap-x-1 gap-y-0.5 text-xs text-fg-muted">
        {items.map((item, index) => {
          const last = index === items.length - 1;
          return (
            <li key={isValidElement(item) ? item.key : typeof item === "string" || typeof item === "number" ? `text-${item}` : "crumb"} className="flex min-w-0 items-center gap-1">
              {index > 0 && <ChevronRight aria-hidden className="size-3.5 shrink-0 text-fg-subtle" />}
              {last && !isValidElement(item) ? (
                <span aria-current="page" className="truncate text-fg">
                  {item}
                </span>
              ) : (
                <span className="truncate">{item}</span>
              )}
            </li>
          );
        })}
      </ol>
    </nav>
  );
}

interface PageHeaderProps {
  title: ReactNode;
  description?: ReactNode;
  actions?: ReactNode;
  breadcrumbs?: ReactNode;
  icon?: ReactNode;
  // Section navigation rendered along the bottom edge of the header.
  tabs?: ReactNode;
}

export function PageHeader({ title, description, actions, breadcrumbs, icon, tabs }: PageHeaderProps) {
  return (
    <header className={`page-bleed border-b border-line ${tabs ? "" : "pb-4"}`}>
      <div className="mx-auto max-w-page px-4 pt-4 sm:px-6">
        {breadcrumbs && <div className="mb-3">{breadcrumbs}</div>}
        <div className="flex flex-wrap items-center justify-between gap-x-6 gap-y-3">
          <div className="flex min-w-0 flex-1 basis-72 items-center gap-3">
            {icon && <div className="flex size-10 shrink-0 items-center justify-center self-start rounded-md border border-line-strong bg-surface text-fg-muted [&>svg]:size-4.5">{icon}</div>}
            <div className="min-w-0">
              <h1 className="truncate text-xl leading-7 font-semibold tracking-tight text-fg">{title}</h1>
              {description && <div className="mt-0.5 text-[13px] text-fg-muted">{description}</div>}
            </div>
          </div>
          {actions && <div className="flex max-w-full flex-wrap items-center gap-2">{actions}</div>}
        </div>
        {tabs && <div className="mt-4">{tabs}</div>}
      </div>
    </header>
  );
}

export function TabNav({ label, children }: { label: string; children: ReactNode }) {
  return (
    <nav aria-label={label} className="-mb-px overflow-x-auto">
      <div className="flex w-max min-w-full gap-1">{children}</div>
    </nav>
  );
}

export function EmptyState({ title, children, action }: { title: string; children?: ReactNode; action?: ReactNode }) {
  return (
    <div className="px-4 py-12 sm:px-6">
      <div className="max-w-md">
        <h2 className="text-sm font-medium text-fg">{title}</h2>
        {children && <div className="mt-1.5 text-[13px] leading-relaxed text-fg-muted">{children}</div>}
        {action && <div className="mt-4 flex flex-wrap gap-2">{action}</div>}
      </div>
    </div>
  );
}

export function LoadingRows({ label, rows = 4 }: { label: string; rows?: number }) {
  return (
    <div role="status" aria-label={label} className="divide-y divide-line">
      {Array.from({ length: rows }, (_, index) => (
        <div key={index} className="flex items-center gap-4 px-4 py-3.5 sm:px-6">
          <div className="h-3 w-40 animate-pulse rounded bg-raised" />
          <div className="h-3 w-24 animate-pulse rounded bg-raised" />
          <div className="ml-auto hidden h-3 w-20 animate-pulse rounded bg-raised sm:block" />
        </div>
      ))}
    </div>
  );
}

export function SectionGrid({ children }: { children: ReactNode }) {
  return <div className="grid grid-cols-[repeat(auto-fit,minmax(min(100%,30rem),1fr))] items-start gap-5 px-4 pt-5 sm:px-6 [&>div]:min-w-0 [&>div]:px-0 [&>div]:pt-0 [&>div>section]:px-0 [&>div>section]:pt-0 [&>section]:px-0 [&>section]:pt-0">{children}</div>;
}

interface SectionProps {
  title: string;
  description?: ReactNode;
  children: ReactNode;
  actions?: ReactNode;
  // Tables and lists that run to the edges of the group.
  flush?: boolean;
  tone?: "default" | "danger";
  // Occasional settings start collapsed behind a one-line summary of their current state.
  collapsed?: { summary: ReactNode };
  // Forms stop at a readable width instead of stretching their fields across the page.
  width?: "full" | "form";
  // Form actions, usually SaveBar, in a strip along the bottom edge of the surface.
  footer?: ReactNode;
}

export function Section({ title, description, children, actions, flush = false, tone = "default", collapsed, width = "full", footer }: SectionProps) {
  const border = tone === "danger" ? "border-bad-line" : "border-line";
  const id = useId();
  const bodyId = useId();
  const [open, setOpen] = useState(!collapsed);
  const titleClass = `text-sm font-semibold ${tone === "danger" ? "text-bad" : "text-fg"}`;
  return (
    <section aria-labelledby={id} className={`min-w-0 px-4 pt-5 sm:px-6 ${width === "form" ? "max-w-3xl" : ""}`}>
      <div className="mb-2.5 flex flex-wrap items-end justify-between gap-2">
        <div className="min-w-0">
          {collapsed ? (
            <h2 className={titleClass}>
              <button
                id={id}
                type="button"
                aria-expanded={open}
                aria-controls={bodyId}
                onClick={() => setOpen(!open)}
                className="-mx-1 inline-flex items-center gap-1.5 rounded px-1 hover:text-fg-muted"
              >
                <ChevronDown aria-hidden className={`size-4 shrink-0 text-fg-muted transition-transform ${open ? "" : "-rotate-90"}`} />
                {title}
              </button>
            </h2>
          ) : (
            <h2 id={id} className={titleClass}>
              {title}
            </h2>
          )}
          {collapsed && !open ? (
            <p className="mt-0.5 pl-5.5 text-xs text-fg-muted">{collapsed.summary}</p>
          ) : (
            description && <p className={`mt-0.5 max-w-3xl text-xs text-fg-muted ${collapsed ? "pl-5.5" : ""}`}>{description}</p>
          )}
        </div>
        {actions && (!collapsed || open) && <div className="flex flex-wrap items-center gap-2">{actions}</div>}
      </div>
      <div id={bodyId} hidden={!open} className={`@container min-w-0 rounded-lg border ${border} bg-surface ${flush ? "overflow-x-auto" : ""}`}>
        {flush ? children : <div className="p-4">{children}</div>}
        {footer && <div className="border-t border-line px-4 py-3">{footer}</div>}
      </div>
    </section>
  );
}

// A titled part of a flush Section. Stack several inside <div className="divide-y divide-line"> so related
// settings share one surface and are separated by dividers instead of nested cards.
// layout="row" is a settings row: title and description on the left, the control and its actions on the right.
export function Subsection({ title, description, actions, children, layout = "stack" }: { title?: ReactNode; description?: ReactNode; actions?: ReactNode; children?: ReactNode; layout?: "stack" | "row" }) {
  if (layout === "row") {
    return (
      <div className={`${settingRowClass} p-4`}>
        <div className="min-w-0">
          {title && <h3 className="text-[13px] font-semibold text-fg">{title}</h3>}
          {description && <p className="mt-0.5 text-xs text-fg-muted">{description}</p>}
        </div>
        <div className="min-w-0 space-y-3">
          {children}
          {actions && <div className="flex flex-wrap items-center justify-end gap-2">{actions}</div>}
        </div>
      </div>
    );
  }
  return (
    <div className="min-w-0 space-y-3 p-4">
      {(title || actions) && (
        <div className="flex flex-wrap items-start justify-between gap-x-4 gap-y-2">
          <div className="min-w-0 flex-1 basis-56">
            {title && <h3 className="text-[13px] font-semibold text-fg">{title}</h3>}
            {description && <p className="mt-0.5 text-xs text-fg-muted">{description}</p>}
          </div>
          {actions && <div className="flex flex-wrap items-center gap-2">{actions}</div>}
        </div>
      )}
      {children}
    </div>
  );
}

// One on/off setting. The description stays outside the label so the switch's accessible name is just the setting.
// layout="row" puts the switch at the end of the row, as in a settings list.
export function SwitchField({ id, label, description, layout = "stack", ...props }: { id: string; label: ReactNode; description?: ReactNode; layout?: "stack" | "row" } & Omit<ComponentProps<typeof Switch>, "id">) {
  return (
    <div className={`flex items-start ${layout === "row" ? "flex-row-reverse justify-between gap-6" : "gap-3"}`}>
      <Switch id={id} aria-describedby={description ? `${id}-description` : undefined} {...props} />
      <div className="min-w-0">
        <label htmlFor={id} className={`block text-[13px] font-medium ${props.disabled ? "text-fg-muted" : "text-fg"}`}>
          {label}
        </label>
        {description && (
          <p id={`${id}-description`} className="mt-0.5 text-xs text-fg-muted">
            {description}
          </p>
        )}
      </div>
    </div>
  );
}

// Terms sit beside their values only when the list itself is wide enough for short values to stay on one line;
// in narrow columns each value goes under its term and uses the full width.
export function DefinitionList({ items, compact = false }: { items: { term: string; value: ReactNode }[]; compact?: boolean }) {
  return (
    <div className="@container min-w-0">
      <dl className={`grid grid-cols-1 gap-y-3 text-[13px] ${compact ? "gap-x-4 @sm:grid-cols-[7rem_minmax(0,1fr)]" : "gap-x-8 @md:grid-cols-[10rem_minmax(0,1fr)]"}`}>
        {items.map((item) => (
          <div key={item.term} className="contents">
            <dt className="text-fg-muted">{item.term}</dt>
            <dd className={`-mt-2.5 min-w-0 break-words text-fg ${compact ? "@sm:mt-0" : "@md:mt-0"}`}>{item.value}</dd>
          </div>
        ))}
      </dl>
    </div>
  );
}

// Metadata items separated by middle dots that wrap as whole items. Each item draws its dot in the gap before it,
// and the wrapper clips the dot of whichever item starts a line, so no line begins with a separator.
// Each item is a flex item beside its dot, so content that wraps never falls under the clipped dot, and an item that
// sets min-w-0 can shrink and truncate inside the line.
export function MetaList({ children, className = "" }: { children: ReactNode; className?: string }) {
  const items = Children.toArray(children).filter(Boolean);
  return (
    <span className={`-mx-1 -my-1 block overflow-hidden px-1 py-1 ${className}`}>
      <span className="-ml-4 flex flex-wrap items-center gap-y-1">
        {items.map((item, index) => (
          <span key={isValidElement(item) && item.key !== null ? item.key : index} className="flex min-w-0 items-baseline before:w-4 before:shrink-0 before:text-center before:text-fg-subtle before:content-['·']">
            {item}
          </span>
        ))}
      </span>
    </span>
  );
}

// Settings rows inside a Section body, separated by hairlines. Children are SettingRow, or Field and SwitchField with layout="row".
export function SettingRows({ children }: { children: ReactNode }) {
  return <div className="divide-y divide-line [&>*]:py-4 [&>*:first-child]:pt-0 [&>*:last-child]:pb-0">{children}</div>;
}

// A settings row whose right side is a value or an action rather than a single labelled input.
export function SettingRow({ label, description, children }: { label: ReactNode; description?: ReactNode; children: ReactNode }) {
  return (
    <div className={settingRowClass}>
      <div className="min-w-0">
        <p className="text-[13px] font-medium text-fg">{label}</p>
        {description && <p className="mt-0.5 text-xs text-fg-muted">{description}</p>}
      </div>
      <div className="flex min-w-0 flex-wrap items-center gap-2 text-[13px] @lg:justify-end">{children}</div>
    </div>
  );
}

// Usage against a limit: label, value, limit and a thin bar. Without a limit only the value is shown, so an
// unlimited quota never gets an invented scale. The text carries the reading; the bar colour only reinforces it.
// A value that is already a percentage (CPU) passes limit={100} and percentValue, so the limit is not repeated as text.
export function Meter({ label, value, limit, format = String, detail, percentValue = false }: { label: string; value: number; limit?: number; format?: (value: number) => string; detail?: ReactNode; percentValue?: boolean }) {
  const bounded = limit !== undefined && limit > 0;
  const percent = bounded ? Math.round((value / limit) * 100) : 0;
  const tone = percent >= 100 ? "bg-bad" : percent >= 80 ? "bg-warn" : "bg-fg-muted";
  return (
    <div className="min-w-0">
      <div className="flex items-baseline justify-between gap-3">
        <span className="truncate text-xs text-fg-muted">{label}</span>
        {bounded && !percentValue && <span className="text-xs text-fg-subtle tabular-nums">{percent}%</span>}
      </div>
      <p className="mt-1 text-lg font-semibold text-fg tabular-nums">
        {format(value)}
        {!percentValue && <span className="inline-block text-[13px] font-normal whitespace-pre text-fg-muted">{bounded ? ` / ${format(limit)}` : " · no limit"}</span>}
      </p>
      {bounded && (
        <div
          role="meter"
          aria-label={label}
          aria-valuemin={0}
          aria-valuemax={limit}
          aria-valuenow={Math.min(value, limit)}
          aria-valuetext={percentValue ? format(value) : `${format(value)} of ${format(limit)}, ${percent}%`}
          className="mt-2 h-1 overflow-hidden rounded-full bg-raised"
        >
          <div className={`h-full rounded-full ${tone}`} style={{ width: `${Math.min(percent, 100)}%` }} />
        </div>
      )}
      {detail && <p className="mt-1.5 text-xs text-fg-subtle">{detail}</p>}
    </div>
  );
}

const meterColumns = { 3: "@lg:grid-cols-3", 4: "@3xl:grid-cols-4" } as const;

// Meters side by side in columns, without cards: two columns in narrow sections, one row once all fit.
export function MeterGroup({ label, columns, children }: { label: string; columns: keyof typeof meterColumns; children: ReactNode }) {
  return (
    <div role="group" aria-label={label} className={`grid grid-cols-1 gap-x-8 gap-y-5 min-[360px]:grid-cols-2 ${meterColumns[columns]}`}>
      {children}
    </div>
  );
}

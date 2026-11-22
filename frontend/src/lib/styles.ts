export type Variant = "primary" | "secondary" | "ghost" | "danger";

// "sm" is for buttons inside table rows, list rows and dense toolbars; "md" is everywhere else.
export type Size = "sm" | "md";

const variants: Record<Variant, string> = {
  primary: "border border-fg bg-fg text-canvas hover:border-white hover:bg-white disabled:border-line disabled:bg-raised disabled:text-fg-disabled",
  secondary: "border border-line-strong bg-raised text-fg hover:bg-hover disabled:border-line disabled:bg-surface disabled:text-fg-disabled",
  ghost: "text-fg-muted hover:bg-hover hover:text-fg disabled:bg-transparent disabled:text-fg-disabled",
  danger: "border border-bad-line text-bad hover:bg-bad-tint disabled:border-line disabled:bg-transparent disabled:text-fg-disabled",
};

const sizes: Record<Size, string> = {
  sm: "h-7 gap-1 px-2 text-xs",
  md: "h-8 gap-1.5 px-3 text-[13px]",
};

export function buttonClass(variant: Variant = "secondary", extra = "", size: Size = "md") {
  return `inline-flex shrink-0 items-center justify-center rounded-md font-medium whitespace-nowrap transition-colors disabled:cursor-not-allowed pointer-coarse:min-h-10 ${sizes[size]} ${variants[variant]} ${extra}`;
}

export const inputClass =
  "block min-w-0 w-full rounded-md border border-line-input bg-panel px-2.5 py-1.5 text-[13px] text-fg placeholder:text-fg-subtle hover:border-fg-subtle focus:border-info focus:outline-none focus-visible:outline-none focus:ring-1 focus:ring-info disabled:border-line disabled:text-fg-disabled aria-[invalid=true]:border-bad";

export function describedBy(id: string, error?: string, hint?: unknown) {
  if (error) return `${id}-error`;
  return hint ? `${id}-hint` : undefined;
}

export const tabLinkClass =
  "inline-flex h-9 shrink-0 items-center gap-1.5 border-b-2 border-transparent px-2.5 text-[13px] text-fg-muted hover:text-fg focus-visible:-outline-offset-2 data-[status=active]:border-fg data-[status=active]:font-medium data-[status=active]:text-fg";

// In-content links stay underlined so they are identifiable without hover.
export const linkClass = "rounded font-medium text-fg underline decoration-fg-subtle underline-offset-4 hover:decoration-fg";

// Entity titles in tables and lists, where position and weight already identify the link.
export const listLinkClass = "rounded font-medium text-fg hover:underline hover:decoration-fg-subtle hover:underline-offset-4";

// Sidebar and resource navigation. Inactive items and their icons stay low-contrast so the one selected
// item, the only raised row, reads first; data-section marks a parent section.
export const navItemClass =
  "flex h-8 min-w-0 items-center gap-2.5 rounded-md px-2 text-[13px] text-fg-muted transition-colors hover:text-fg pointer-coarse:min-h-10 [&>svg]:text-fg-subtle hover:[&>svg]:text-fg-muted data-[status=active]:bg-raised data-[status=active]:font-medium data-[status=active]:text-fg data-[status=active]:[&>svg]:text-fg data-[section=true]:bg-raised data-[section=true]:font-medium data-[section=true]:text-fg data-[section=true]:[&>svg]:text-fg";

// Small label above a group of navigation items or list rows.
export const groupLabelClass = "px-2 pb-1 text-[11px] font-medium text-fg-subtle";

// Choice rows for multi-selection checkboxes; the checked row stays visibly selected.
export const choiceClass =
  "flex min-h-9 min-w-0 cursor-pointer items-center gap-2.5 rounded-md border border-line px-2.5 py-1.5 text-[13px] text-fg-muted transition-colors hover:border-line-strong hover:text-fg has-[:checked]:border-info-line has-[:checked]:bg-info-tint has-[:checked]:text-fg has-[:disabled]:cursor-not-allowed has-[:disabled]:opacity-60 pointer-coarse:min-h-11";

// Header row of a hairline table: no fill, small muted labels. Cells set their own alignment;
// numeric columns are right-aligned with tabular figures.
export const tableHeadClass = "border-b border-line text-xs text-fg-subtle [&>th]:h-9 [&>th]:font-medium";

// Body rows are at least 2.75rem tall. The identifying column is text-fg; dates and metadata use text-fg-muted.
export const tableRowClass = "h-11 border-b border-line last:border-b-0 hover:bg-raised";

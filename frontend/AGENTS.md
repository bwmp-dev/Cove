# Frontend conventions

Read [../docs/DESIGN.md](../docs/DESIGN.md) for the visual direction and product constraints. This file describes how the dashboard implements that direction. Follow it when you add or change UI, and update it when you change a shared convention.

## Tokens

All tokens are defined in `src/index.css` (`@theme`). Use the token utilities and never use raw colours.

- The neutral scale gives each step one role (see the comments in `index.css`): `bg-canvas` app background (shell, sidebar) → `bg-panel` subtle background (page panel, fields) → `bg-surface` component at rest (section bodies) → `bg-raised` component hover (hovered rows, raised controls, the selected navigation item) → `bg-hover` component active.
- Lines: `border-line` subtle border for dividers and table hairlines, `border-line-strong` border for control and section edges, and `border-line-input` strong border for form fields (keeps 3:1 contrast).
- Text: `text-fg` high contrast, then `text-fg-muted` low contrast for descriptions and metadata, then `text-fg-subtle` for the quietest readable text (also the neutral dot). `text-fg-disabled` is for disabled controls only.
- Status: `ok`, `warn`, `bad` and `info` are the solid step (dots, icons, text). Each has a `-tint` background step and a `-line` border step for notices and tinted selection (`bg-info-tint border-info-line`). Do not use opacity modifiers such as `bg-info/10` for tints. `info` is also the focus and checked colour.
- Series: `series-1` to `series-6` are categorical colours for chart lines and per-container log labels only (`containerColour` and `seriesColour` in `lib/telemetry.ts`). They carry no status meaning and always sit beside a text label.
- Type scale: page titles are `text-xl font-semibold`. Section titles are `text-sm font-semibold`. Subsection titles are `text-[13px] font-semibold`. Body and controls are `text-[13px]`. Descriptions and metadata are `text-xs`. Group labels are `text-[11px]`. Use `font-mono` only for identifiers, keys, commits, Compose and similar technical values.
- Radius: controls and small surfaces use `rounded-md`, sections use `rounded-lg`, and the inset page panel uses `rounded-xl`. Status dots, meter bars and the switch track are the only `rounded-full` elements.

## Page layout

- `AppShell` places the sidebar directly on the canvas and the page in one inset panel (`bg-panel`, hairline border, `rounded-xl`) from `lg`; below `lg` the panel runs edge to edge under the mobile header. It renders `<main className="page-grid">`. Every direct child of `main` sits in a centred column capped at `max-w-page` (80rem). Do not add another page-wide max width.
- `PageHeader` is marked `page-bleed`, so its bottom border spans the whole panel while its content stays aligned to the column. Render `PageHeader` as a direct child of the page (or of a layout fragment). Do not wrap it in a `div`. Top-level pages pass the same Lucide `icon` as their navigation item; the header shows it in a tile beside the title and one-line description, with secondary actions before the primary one.
- Page body blocks use `px-4 sm:px-6` with `pt-5`. `Section` already applies this.
- `Section` is the unit of grouping: a title, an optional description and actions, and one `bg-surface` body.
  - `flush` is for tables, lists and stacked `Subsection`s that run to the edges of the body.
  - `width="form"` (max-w-3xl) is for settings forms, so fields do not stretch across wide screens. Leave tables, lists, logs and overviews at full width.
  - `footer` holds the form actions (usually `SaveBar`) in a strip along the bottom of the surface. Wrap the `<form>` around the `Section` so the footer buttons submit it.
  - `collapsed={{ summary }}` is for occasional settings, such as shared variables and deployment policy.
  - `tone="danger"` is for destructive areas.
- `SectionGrid` places independent sections side by side once each column can be at least 30rem wide (Integrations, team settings, project and environment settings). Use it in place of one long column of unrelated sections.
- Inside a flush section, stack `Subsection`s in `<div className="divide-y divide-line">` to separate identity, connection, access and sharing parts. Do not put cards inside sections.
- Inside forms, put short related fields in `grid gap-4 sm:grid-cols-2`. Long text, textareas and editors stay full width.
- Settings forms (account, team, quotas, sign-in settings, resource General and Deployment safety) use settings rows: wrap `Field layout="row"`, `SwitchField layout="row"` and `SettingRow` (a value or action instead of an input) in `SettingRows`. The label and help text sit on the left and the control on the right once the section body is at least 32rem wide (a container query), and they stack below that. `Subsection layout="row"` does the same inside flush sections. Keep multi-field editors (variables, routes, limits, grants, secrets) in their stacked layout.
- Put form actions at the end of the form, right-aligned, with the primary action last.

## Navigation

- Use `navItemClass` and `groupLabelClass` from `src/lib/styles.ts` for both the sidebar and the resource sub-navigation. Inactive items are muted with subtle icons and only brighten their text on hover. The active item is the only raised row: `bg-raised`, full-strength text and a full-strength icon. Set `data-section` to mark a parent section as active.
- Resource pages (`routes/workload-detail.tsx`) keep identity and status in the header: breadcrumbs, type, node and its state, latest operation with time, first domain, the manual-stop warning, and the deploy and lifecycle actions. Sub-pages are grouped as **Monitor** (Overview, Metrics, Logs, Operations, Troubleshooting), **Configure** (General, Source, Environment variables, Domains, Networking, Registry, Resource limits), **Deploy** (Preflight, Deployment safety, Hooks or Schedule, Previews, Promote release, Change history) and **Manage** (Backups, Settings). Below `lg` they collapse into a disclosure that names the current section.
- Project pages use `TabNav` (Overview, one tab per environment people created, then Previews) under the header. Preview environments are not environment tabs: they are listed on the Previews tab (`routes/project-previews.tsx`), their pages keep the Previews tab active and add a Previews breadcrumb, and `productionEnvironments` (`lib/hierarchy.ts`) is the list to offer wherever people choose an environment.

## Lists and tables

- Prefer compact rows (`divide-y divide-line`, `px-3 py-2`/`py-2.5`, hover `bg-raised`) with the name first, metadata in `text-xs text-fg-muted` and actions at the end of the row.
- `PlacementRow` (`components/EnvironmentList.tsx`) is the environment and project-resources row used on Projects and on the project overview. Its columns (name, resources, nodes, count) align from `md`.
- Tables are hairline tables: `tableHeadClass` (no fill, small muted labels, `h-9`) and `tableRowClass` (at least `h-11`, hover `bg-raised`). The identifying column is `text-fg`; dates and metadata are `text-fg-muted`; numbers and times are right-aligned with `tabular-nums`. Hide secondary columns at small widths and repeat their key value under the name instead.
- Dense lists (All resources, Operations, Audit log) put `FilterToolbar` (`components/FilterToolbar.tsx`) at the top of their flush section: a search field, `FilterSelect`s, a Reset filters action while filtering and a count of what is shown. It filters rows that are already loaded, so say what the filters cover when the API returns a limited window (for example the 100 most recent operations). Show `NoMatches` with a reset when nothing matches.
- Show status with `StatusLabel`, `NodeStateLabel` and `OperationStateLabel` (a dot plus text, `StatusDot` when the text sits elsewhere). Never show colour alone. Lifecycle tones come from `operationTones` (`lib/operations.ts`), `nodeStates` and `connectionStates`: `ok` healthy or done, `info` in progress, `warn` needs attention, `bad` failed, `neutral` idle, stopped or not started.
- Show history over time with `MetricChart` (`components/MetricChart.tsx`): an SVG line chart drawn with presentation attributes (no style attributes), a legend with each line's latest and peak reading, axis labels, and a samples table with real timestamps. Unknown readings break the line and read "Unknown"; never plot them as zero or interpolate across gaps. Cumulative counters are charted as average rates between samples, with gaps at resets.
- Show usage against a limit with `Meter` inside `MeterGroup` (team quotas, node CPU, memory and disk): label, value, limit and a thin bar with `role="meter"`. Without a limit (zero means unlimited) show the value only; never invent a scale.

## Resource graph

- Environment and project-resource sections offer a Graph/List toggle (`components/PrivateConnections.tsx`). The list keeps `ResourceTable` and adds the connection rows. It stays the default unless the section has connections and the screen is at least `md` wide. The graph scrolls inside its section, never the page.
- Graph nodes are `rounded-md` bordered buttons with the resource name, a status dot plus label and the node name. Resources outside the current environment use a dashed border and name their project and environment; endpoints the viewer cannot read show as "Resource you cannot view".
- Connections are arrows from source to target. Their label is a focusable button (`hostname:port` in `font-mono` with a status dot) that selects the connection; the editor appears below the graph. Pending and revoking arrows are dashed, errors use `bad`. Shared networks are dashed `warn` lines with a warning notice, because they allow more than any connection shown.
- Every drag interaction has a keyboard and touch equivalent: the connect handle starts a "choose the target" mode, and New connection opens the same form.

## Controls

- `Button` variants: `primary` for the one main action in a view, `secondary` (the default) for other actions, `ghost` for low-emphasis actions inside rows and for Cancel or Discard, and `danger` for destructive actions. Sizes: `size="sm"` (h-7, `text-xs`) for buttons inside table rows, list rows and dense toolbars, and the default `md` (h-8) everywhere else; both share the radius, weight and focus ring. For links that look like buttons, use `buttonClass(variant, extra, size)`.
- Text inputs use `inputClass`. Wrap them in `Field` so they get a label, an optional marker, a hint and an error message, and pass `describedBy` or the `${id}-hint` id to `aria-describedby`.
- Use `Select` (`components/Select.tsx`), never a native `<select>`. It keeps a hidden native select, so forms, `FormData` and validation still work.
- Use `Checkbox` when choosing several items from a set (teams, domains, nodes, event types) and for one-off options in a form, such as "Copy environment variables" or "Create a production environment". Wrap choices that are part of a set in a `label` with `choiceClass`, so the checked row stays tinted.
- Use `Switch` through `SwitchField` for a single on/off setting that is saved by the surrounding form (enable notifications, require approval, guarded deployment, previews, Tailscale, Cloudflare DNS automation). It is a native checkbox with `role="switch"`, so `name`, `checked`, `defaultChecked` and `FormData.has()` behave as before. Its description stays outside the label, so the accessible name is just the setting.
- Use `ActionMenu` for secondary entity actions (clone, delete). It follows the WAI-ARIA menu pattern and positions itself inside the viewport. A disabled item stays focusable and shows its reason.
- Use `ConfirmDialog` or `Dialog` for destructive or multi-step actions. Actions that discard data people created (deleting a preview, replacing its data) ask for the entity's name to be typed, as `PreviewActions` does. Use `SaveBar` with `UnsavedChangesGuard` for forms that edit saved settings. Keep one overall Save for a form that edits several items, such as team access across installations or domain sharing across teams.
- Use `Notice` for page-level and section-level state (info, warn, error, success), `QueryError` for failed queries (it includes Retry), `LoadingRows` for loading lists and `EmptyState` for empty pages.
- Use Lucide icons only, imported individually, at `size-3.5` inside buttons and `size-4` in navigation. Give icon-only controls `aria-label` and `title`.

## Responsive behaviour and accessibility

- Check every change at 320, 390, 768, 1280 and 1920px wide. The page must never scroll horizontally. Long names wrap (`break-words` or `break-all` for hosts) or truncate with the full value in `title`.
- On coarse pointers, controls get larger targets: `pointer-coarse:min-h-10` for buttons and navigation, and `min-h-11` for options.
- Keep the global `:focus-visible` ring. Do not remove outlines unless you replace them with an equivalent `ring`.
- Respect `prefers-reduced-motion`, which is handled globally. Motion is limited to colour transitions, chevron rotation and the switch thumb.

## Verification

- Run `pnpm lint`, `pnpm typecheck`, `pnpm test` and `pnpm build` in `frontend/`.
- Run browser checks with `python scripts/test-browser.py` from the repository root (see [development documentation](../docs/DEVELOPMENT.md)). The responsive test in `e2e/panel.spec.ts` visits every page at each width above and writes screenshots to `frontend/test-results/`. Look at the screenshots, not just the pass/fail result.
- After React changes, run `npx react-doctor@latest --verbose --scope changed` and fix any regressions.

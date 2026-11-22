# Cove interface direction

The dashboard is a React and TanStack (Router, Query) app written in TypeScript with Tailwind and Lucide icons. This file records the visual direction and product constraints. Component, layout and styling conventions that implement it are in [frontend/AGENTS.md](../frontend/AGENTS.md). Read both before changing the interface.

## Direction

The interface is a dark, neutral infrastructure console. It should be dense enough to be useful, and its grouping and selected state should always be clear. Each reference below contributes one thing:

- **Linear: hierarchy and navigation.** Inactive navigation is muted so the selected item reads first. Icons are small. Headers and actions sit in predictable places. ([Behind the latest design refresh](https://linear.app/now/behind-the-latest-design-refresh))
- **Resend: compact operational lists.** Aligned rows separated by hairlines, restrained status colour and actions at the end of each row. Use this for resources, environments, domains, connections and operations. ([Transactional emails](https://resend.com/products/transactional-emails))
- **Railway and Render: resource context.** A resource page always shows where the resource lives (project and environment breadcrumbs), what it is, its node and status, and its deploy actions. Its configuration is split into pages that are grouped by purpose: Monitor, Configure, Deploy and Manage. ([Railway basics](https://docs.railway.com/platform/the-basics), [Render dashboard refresh](https://render.com/blog/introducing-renders-new-cli-and-refreshed-dashboard))
- **Tailscale: scan-friendly status.** Node lists lead with identity, address, version and connectivity, and dark surfaces stay calm and low-contrast. ([The heart of dark mode](https://tailscale.com/blog/heart-of-dark-mode))
- **Vercel Geist: consistent controls.** Buttons, inputs, selects, checkboxes, switches and menus share the same heights, borders, radius and focus ring. Buttons come in two sizes, and status dots use one colour per deployment state. ([Geist](https://vercel.com/geist/introduction))
- **Catalyst and Supabase: settings and tables.** The page content is one inset panel beside the sidebar. Tables are hairline rows without a header fill. Settings forms are one surface with label-left, control-right rows and a footer for the actions. ([Catalyst](https://catalyst.tailwindui.com), [Supabase design system](https://supabase.com/design-system))
- **Radix Colors: a role per step.** Neutral and semantic colours are scales in which each step has one job. ([Radix Colors](https://www.radix-ui.com/colors))
- **Tremor: usage and filters.** Quotas and host usage are thin meters with the value and limit in text, and dense tables have a search and filter toolbar above them. ([Tremor](https://tremor.so))

These products are references only. Do not copy their branding, assets or layouts.

## Principles

- Content sets the width. Pages sit in one column no wider than `max-w-page` (80rem). Forms stop at a readable width (`Section width="form"`). Short related fields go side by side (`sm:grid-cols-2`). Do not stretch a narrow form across a wide card, and do not leave a wide panel mostly empty.
- Group with dividers, not boxes. A section is a single surface. Its parts are separated by `divide-y divide-line` (`Subsection`) and its rows by hairlines. Never nest cards inside cards. Settings forms put the label and help text on the left and the control on the right, with the actions at the end.
- Make selection obvious. Active navigation is the only raised row, with full-strength text. Checked choices tint their row with `info`. Tabs use an underline. Never use colour alone: every status dot has a text label, and each lifecycle stage has one tone (healthy green, in progress blue, needs attention amber, failed red, idle grey).
- Every colour step has one role. Use the token for the role instead of an opacity modifier or a neighbouring step.
- Keep identity, status and actions visible. Show project → environment → resource breadcrumbs, the type, the node and its state, the latest operation and the primary action in every resource page header.
- No decoration. Do not use gradients, gloss, glow, blur, decorative backgrounds, fabricated metrics or filler cards.

## Product constraints

- Hierarchy is team → project → environment → resources. Projects hold resources directly ("Project resources") or inside optional environments. Every resource belongs to a project; there are no unassigned resources.
- The primary navigation has three groups. Workspace: dashboard, projects, all resources, operations, and the terminal for administrators. Infrastructure: nodes, integrations, Docker registries and storage locations. Manage: team settings, notifications, API keys, audit log, account, and the users and settings pages for instance administrators. Show only the controls the current account's permissions allow, and enforce those permissions separately in the API.
- Use real API data only, and design each state on purpose: loading, empty, error, offline, pending, interrupted, read-only and success.
- Tables must be readable, actions meaningful, focus visible and labels descriptive. Every page must be usable with the keyboard and must work from 320px phones to 1920px desktops without horizontal page scroll.
- An enrollment command is a short-lived credential. Never persist it to browser storage or put it in a URL.
- Never put administrator bearer tokens in browser code. The browser uses HttpOnly cookie sessions.
- Do not commit, branch or deploy without an explicit request.

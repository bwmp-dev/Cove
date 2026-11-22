# Cove dashboard

React, TypeScript, TanStack Router/Query, Tailwind, and Lucide. Use pnpm.

Before changing the interface, read [AGENTS.md](AGENTS.md) for layout, component and styling conventions and [../docs/DESIGN.md](../docs/DESIGN.md) for the design direction.

```sh
pnpm install
pnpm dev        # Vite dev server with /api and /auth proxied to the control plane
pnpm typecheck
pnpm lint
pnpm test
pnpm build      # writes dist/, served by the Go control plane (COVE_FRONTEND_DIR overrides)
```

## Development against a running control plane

The control plane only accepts state-changing requests whose `Origin` equals `COVE_PUBLIC_URL`, and sets a `__Host-` cookie that browsers keep only on secure origins (`http://localhost` counts as secure in current Chromium and Firefox).

Set these in `frontend/.env.local`:

- `COVE_DEV_BACKEND`: control-plane listen address, default `http://127.0.0.1:8080`.
- `COVE_DEV_PUBLIC_URL`: the control plane's `COVE_PUBLIC_URL`. The dev proxy rewrites `Origin` to this value on proxied requests that carry one. Production builds are unaffected, and the backend's origin policy is never relaxed.

Open `http://localhost:5173` and sign in with the administrator password.

## Security constraints

- The browser authenticates only with the HttpOnly session cookie. Never add bearer tokens or credentials to frontend code.
- Setup commands, environment values, and repository tokens exist only in component state and in mutations that use `gcTime: 0`. Never write them to browser storage, URLs, or logs.
- The production build makes no third-party requests and uses no inline scripts or style attributes, so it satisfies the control plane's `'self'` CSP.

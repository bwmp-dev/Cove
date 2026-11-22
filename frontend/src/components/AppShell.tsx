import { Select } from "./Select";
import { useIsFetching } from "@tanstack/react-query";
import { useQuery } from "@tanstack/react-query";
import { Link, Outlet, useNavigate, useRouterState } from "@tanstack/react-router";
import { Bell, Container, HardDrive, Activity, Boxes, Folder, LayoutDashboard, Plug, Settings, SquareTerminal, KeyRound, LogOut, Menu, ScrollText, Server, ShieldCheck, UserRound, Users, WifiOff, X } from "lucide-react";
import { useEffect, useId, useRef, useState, type ReactNode } from "react";
import { errorMessage } from "../api/client";
import { useLogout } from "../api/mutations";
import { workloadQuery } from "../api/queries";
import { useOnline } from "../lib/hooks";
import { currentTeam, managesTeam } from "../lib/permissions";
import { useSession, useSwitchTeam } from "../lib/session";
import { groupLabelClass, inputClass, navItemClass } from "../lib/styles";
import { Spinner } from "./ui";

function Wordmark() {
  return (
    <Link to="/dashboard" className="flex items-center gap-2 rounded-md text-[15px] font-semibold tracking-tight text-fg">
      <svg aria-hidden viewBox="0 0 32 32" className="size-5">
        <rect width="32" height="32" rx="7" className="fill-raised" />
        <path d="M21.5 11.2A7 7 0 1 0 21.5 20.8" fill="none" className="stroke-fg" strokeWidth="3" strokeLinecap="round" />
      </svg>
      Cove
    </Link>
  );
}

function SignedInAs() {
  const session = useSession();
  return <p className="truncate px-2 pb-1.5 text-xs text-fg-subtle">Signed in as {session?.email || session?.name || "administrator"}</p>;
}

function SignOut() {
  const logout = useLogout();
  const navigate = useNavigate();
  return (
    <div className="space-y-1">
      <button
        type="button"
        onClick={() => logout.mutate(undefined, { onSuccess: () => void navigate({ to: "/login" }) })}
        disabled={logout.isPending}
        className="flex h-8 w-full items-center gap-2.5 rounded-md px-2 text-[13px] text-fg-muted transition-colors hover:text-fg disabled:opacity-60 pointer-coarse:min-h-10 [&>svg]:text-fg-subtle"
      >
        {logout.isPending ? <Spinner className="size-4" /> : <LogOut aria-hidden className="size-4" />}
        Sign out
      </button>
      {logout.isError && (
        <p role="alert" className="px-2 text-xs text-bad">
          {errorMessage(logout.error)}
        </p>
      )}
    </div>
  );
}

// A resource placed in a project belongs to the Projects section, matching its breadcrumbs.
// The resource page has already loaded it, so this reads the cache rather than fetching.
function usePlacedResourceOpen() {
  const resourceId = useRouterState({ select: (state) => /^\/resources\/([0-9a-f]{32})(?:\/|$)/.exec(state.location.pathname)?.[1] ?? "" });
  const resource = useQuery({ ...workloadQuery(resourceId), enabled: resourceId !== "" });
  return resourceId !== "" && Boolean(resource.data?.project_id);
}

function NavGroup({ label, children }: { label: string; children: ReactNode }) {
  return (
    <div className="space-y-0.5">
      <p className={groupLabelClass}>{label}</p>
      {children}
    </div>
  );
}

function TeamSwitcher() {
  const id = useId();
  const session = useSession();
  const switchTeam = useSwitchTeam();
  const team = currentTeam(session);
  if (!session) return null;
  const options = team && !session.teams.some((item) => item.id === team.id) ? [team, ...session.teams] : session.teams;
  if (options.length === 0) {
    return (
      <Link to="/team" className="block rounded-md border border-dashed border-line-strong px-2.5 py-2 text-[13px] text-fg-muted hover:text-fg">
        No team yet. Create or join one.
      </Link>
    );
  }
  return (
    <div>
      <label htmlFor={id} className={`block ${groupLabelClass}`}>
        Team
      </label>
      <Select id={id} className={`${inputClass} bg-canvas font-medium`} value={team?.id ?? ""} onChange={(event) => switchTeam(event.target.value)}>
        {options.map((item) => (
          <option key={item.id} value={item.id}>
            {item.name}
          </option>
        ))}
      </Select>
    </div>
  );
}

function Navigation() {
  const session = useSession();
  const team = currentTeam(session);
  const manager = managesTeam(team) || Boolean(session?.is_admin);
  const placedResource = usePlacedResourceOpen();
  const apiDocs = useRouterState({ select: (state) => state.location.pathname === "/api-docs" });
  return (
    <nav aria-label="Primary" className="space-y-5 px-2 py-3">
      <TeamSwitcher />
      <NavGroup label="Workspace">
        <Link to="/dashboard" className={navItemClass}><LayoutDashboard aria-hidden className="size-4 shrink-0" />Dashboard</Link>
        <Link to="/projects" className={navItemClass} data-section={placedResource || undefined}>
          <Folder aria-hidden className="size-4 shrink-0" />
          Projects
        </Link>
        <Link to="/resources" className={navItemClass} activeOptions={{ exact: placedResource }}>
          <Boxes aria-hidden className="size-4 shrink-0" />
          All resources
        </Link>
        <Link to="/operations" className={navItemClass}>
          <Activity aria-hidden className="size-4 shrink-0" />
          Operations
        </Link>
        {session?.is_admin && <Link to="/terminal" className={navItemClass}><SquareTerminal aria-hidden className="size-4 shrink-0" />Terminal</Link>}
      </NavGroup>
      <NavGroup label="Infrastructure">
        <Link to="/nodes" className={navItemClass}>
          <Server aria-hidden className="size-4 shrink-0" />
          Nodes
        </Link>
        <Link to="/integrations" className={navItemClass}><Plug aria-hidden className="size-4 shrink-0" />Integrations</Link>
        <Link to="/registries" className={navItemClass}><Container aria-hidden className="size-4 shrink-0" />Docker registries</Link>
        <Link to="/storage" className={navItemClass}><HardDrive aria-hidden className="size-4 shrink-0" />Storage locations</Link>
      </NavGroup>
      <NavGroup label="Manage">
        <Link to="/team" className={navItemClass}>
          <Users aria-hidden className="size-4 shrink-0" />
          Team settings
        </Link>
        {manager && <Link to="/notifications" className={navItemClass}><Bell aria-hidden className="size-4 shrink-0" />Notifications</Link>}
        <Link to="/tokens" className={navItemClass} data-section={apiDocs || undefined}>
          <KeyRound aria-hidden className="size-4 shrink-0" />
          API keys and tokens
        </Link>
        {manager && (
          <Link to="/audit" className={navItemClass}>
            <ScrollText aria-hidden className="size-4 shrink-0" />
            Audit log
          </Link>
        )}

        <Link to="/account" className={navItemClass}>
          <UserRound aria-hidden className="size-4 shrink-0" />
          Account
        </Link>
        {session?.is_admin && (
          <>
            <Link to="/users" className={navItemClass}><ShieldCheck aria-hidden className="size-4 shrink-0" />Users</Link>
            <Link to="/admin" className={navItemClass}><Settings aria-hidden className="size-4 shrink-0" />Settings</Link>
          </>
        )}
      </NavGroup>
    </nav>
  );
}

function MobileHeader({ fetching }: { fetching: boolean }) {
  // The menu belongs to the location it was opened on, so choosing a destination closes it.
  const location = useRouterState({ select: (state) => state.location.href });
  const [openOn, setOpenOn] = useState<string | null>(null);
  const open = openOn === location;
  const trigger = useRef<HTMLButtonElement>(null);
  // Escape closes the menu from anywhere inside it and returns focus to the trigger.
  useEffect(() => {
    if (!open) return;
    const closeOnEscape = (event: KeyboardEvent) => {
      if (event.key !== "Escape") return;
      setOpenOn(null);
      trigger.current?.focus();
    };
    document.addEventListener("keydown", closeOnEscape);
    return () => document.removeEventListener("keydown", closeOnEscape);
  }, [open]);
  return (
    <header className="sticky top-0 z-20 border-b border-line bg-canvas lg:hidden">
      <div className="flex h-12 items-center justify-between gap-3 px-4">
        <Wordmark />
        <div className="flex items-center gap-2">
          {fetching && <Spinner className="size-3.5 text-fg-subtle" />}
          <button
            ref={trigger}
            type="button"
            aria-expanded={open}
            aria-controls="mobile-navigation"
            onClick={() => setOpenOn(open ? null : location)}
            className="inline-flex h-8 items-center gap-1.5 rounded-md border border-line-strong bg-raised px-2.5 text-[13px] text-fg hover:bg-hover"
          >
            {open ? <X aria-hidden className="size-4" /> : <Menu aria-hidden className="size-4" />}
            Menu
          </button>
        </div>
      </div>
      <div id="mobile-navigation" hidden={!open} className="max-h-[calc(100dvh-3rem)] overflow-y-auto border-t border-line">
        <Navigation />
        <div className="border-t border-line p-2">
          <SignOut />
        </div>
      </div>
    </header>
  );
}

export function AppShell() {
  const online = useOnline();
  const session = useSession();
  const fetching = useIsFetching() > 0;

  return (
    <div className="min-h-dvh bg-canvas lg:grid lg:grid-cols-[15rem_minmax(0,1fr)]">
      <a
        href="#main"
        className="sr-only z-50 rounded-md bg-fg px-3 py-2 text-canvas focus:not-sr-only focus:fixed focus:top-2 focus:left-2"
      >
        Skip to content
      </a>

      <aside className="hidden lg:sticky lg:top-0 lg:flex lg:h-dvh lg:flex-col">
        <div className="flex h-14 shrink-0 items-center justify-between px-4">
          <Wordmark />
          {fetching && <Spinner className="size-3.5 text-fg-subtle" />}
        </div>
        <div className="min-h-0 flex-1 overflow-y-auto">
          <Navigation />
        </div>
        <div className="p-2">
          <SignedInAs />
          <SignOut />
        </div>
      </aside>

      <div className="min-w-0 lg:py-2 lg:pr-2">
        <MobileHeader fetching={fetching} />
        <div className="min-h-[calc(100dvh-3rem)] bg-panel lg:min-h-[calc(100dvh-1rem)] lg:rounded-xl lg:border lg:border-line">
          {!online && (
            <div role="status" className="flex items-center gap-2 border-b border-warn-line bg-warn-tint px-4 py-2 text-[13px] text-fg sm:px-6">
              <WifiOff aria-hidden className="size-4 shrink-0 text-warn" />
              You are offline. Data shown may be out of date and actions are unavailable until the connection returns.
            </div>
          )}
          <main key={session?.team_id} id="main" tabIndex={-1} className="page-grid pb-12 focus:outline-none">
            <Outlet />
          </main>
        </div>
      </div>
    </div>
  );
}

import { useQueryClient } from "@tanstack/react-query";
import { useNavigate } from "@tanstack/react-router";
import { Keyboard, Search } from "lucide-react";
import { useEffect, useId, useRef, useState } from "react";
import { closeHelp, openHelp, openPalette, useCommandState } from "../lib/command-palette";
import { canCreateProjects } from "../lib/permissions";
import { useSession } from "../lib/session";
import { actionShortcuts, isInsideOverlay, isMac, isTypingTarget, navigationShortcuts, setShortcutsEnabled, useShortcutsEnabled } from "../lib/shortcuts";
import { CommandPalette, Key } from "./CommandPalette";
import { Dialog } from "./ConfirmDialog";
import { Button, SwitchField } from "./ui";

const chordTimeoutMs = 1500;
const chordPrefixes = new Set(["g", "r", "n"]);

function paletteKeys() {
  return isMac() ? "Cmd K" : "Ctrl K";
}

function useTeam() {
  return useSession()?.team_id ?? "";
}

export function SearchButton({ compact = false }: { compact?: boolean }) {
  const team = useTeam();
  const enabled = useShortcutsEnabled();
  const keyHint = enabled ? paletteKeys() : undefined;
  if (compact) {
    return (
      <button
        type="button"
        aria-haspopup="dialog"
        aria-label="Search"
        title={keyHint ? `Search (${keyHint})` : "Search"}
        aria-keyshortcuts={enabled ? "Control+K Meta+K" : undefined}
        onClick={() => openPalette(team)}
        className="inline-flex size-8 items-center justify-center rounded-md border border-line-strong bg-raised text-fg hover:bg-hover pointer-coarse:size-10"
      >
        <Search aria-hidden className="size-4" />
      </button>
    );
  }
  return (
    <div className="flex gap-1.5">
      <button
        type="button"
        aria-haspopup="dialog"
        aria-keyshortcuts={enabled ? "Control+K Meta+K" : undefined}
        onClick={() => openPalette(team)}
        className="flex h-8 min-w-0 flex-1 items-center gap-2 rounded-md border border-line-strong bg-panel px-2.5 text-[13px] text-fg-muted transition-colors hover:text-fg pointer-coarse:min-h-10"
      >
        <Search aria-hidden className="size-4 shrink-0 text-fg-subtle" />
        <span className="flex-1 text-left">Search</span>
        {keyHint && <Key>{keyHint}</Key>}
      </button>
      <button
        type="button"
        aria-haspopup="dialog"
        aria-label="Keyboard shortcuts"
        title="Keyboard shortcuts"
        onClick={openHelp}
        className="inline-flex size-8 shrink-0 items-center justify-center rounded-md border border-line-strong bg-panel text-fg-subtle transition-colors hover:text-fg pointer-coarse:size-10"
      >
        <Keyboard aria-hidden className="size-4" />
      </button>
    </div>
  );
}

export function CommandCenter() {
  const navigate = useNavigate();
  const client = useQueryClient();
  const session = useSession();
  const team = session?.team_id ?? "";
  const canCreate = canCreateProjects(session);
  const enabled = useShortcutsEnabled();
  const { paletteTeam, help } = useCommandState();
  const chord = useRef<{ key: string; at: number } | null>(null);
  const [announcement, setAnnouncement] = useState("");

  useEffect(() => {
    if (!enabled) return;
    const onKeyDown = (event: KeyboardEvent) => {
      if (event.defaultPrevented || event.isComposing || event.repeat) return;
      const inert = isTypingTarget(event.target) || isInsideOverlay(event.target);
      if ((event.ctrlKey || event.metaKey) && !event.altKey && !event.shiftKey && event.key.toLowerCase() === "k") {
        if (inert) return;
        event.preventDefault();
        openPalette(team);
        return;
      }
      if (event.ctrlKey || event.metaKey || event.altKey || inert) {
        chord.current = null;
        return;
      }
      if (event.key === "?") {
        event.preventDefault();
        openHelp();
        return;
      }
      const key = event.key.toLowerCase();
      const now = Date.now();
      const prefix = chord.current && now - chord.current.at <= chordTimeoutMs ? chord.current.key : null;
      chord.current = null;
      if (!prefix) {
        if (chordPrefixes.has(event.key)) chord.current = { key: event.key, at: now };
        return;
      }
      if (prefix === "g") {
        const target = navigationShortcuts.find((item) => item.key === key);
        if (!target) return;
        event.preventDefault();
        void navigate({ to: target.path });
      } else if (prefix === "r" && key === "r") {
        event.preventDefault();
        setAnnouncement("Refreshing the current view.");
        void client.invalidateQueries({ type: "active" }).then(() => setAnnouncement("Current view refreshed."));
      } else if (prefix === "n" && key === "r" && canCreate) {
        event.preventDefault();
        void navigate({ to: "/resources/new" });
      }
    };
    document.addEventListener("keydown", onKeyDown);
    return () => document.removeEventListener("keydown", onKeyDown);
  }, [enabled, navigate, client, team, canCreate]);

  return (
    <>
      <CommandPalette team={team} open={paletteTeam !== null && paletteTeam === team} />
      <ShortcutHelp open={help} />
      <p role="status" className="sr-only">
        {announcement}
      </p>
    </>
  );
}

function ShortcutHelp({ open }: { open: boolean }) {
  const id = useId();
  const enabled = useShortcutsEnabled();
  return (
    <Dialog
      open={open}
      title="Keyboard shortcuts"
      onClose={closeHelp}
      footer={
        <Button variant="secondary" onClick={closeHelp}>
          Close
        </Button>
      }
    >
      <div className="space-y-4">
        <SwitchField
          id={`${id}-enabled`}
          label="Use keyboard shortcuts"
          description={`When on, Cove takes ${paletteKeys()}, ? and the letter chords below whenever you are not typing or in a dialog. Leave this off if those keys belong to your browser or assistive technology. Search and this help stay available from their buttons.`}
          checked={enabled}
          onChange={(event) => setShortcutsEnabled(event.target.checked)}
        />
        <p>Shortcuts only navigate, refresh or open existing forms. They never deploy, stop or change anything.</p>
        <table className="w-full text-[13px]">
          <caption className="sr-only">Available shortcuts</caption>
          <thead className="sr-only">
            <tr>
              <th scope="col">Action</th>
              <th scope="col">Keys</th>
            </tr>
          </thead>
          <tbody className="divide-y divide-line">
            <ShortcutRow action="Search" keys={[paletteKeys()]} />
            <ShortcutRow action="Show keyboard shortcuts" keys={["?"]} />
            {navigationShortcuts.map((item) => (
              <ShortcutRow key={item.key} action={`Go to ${item.label}`} keys={["g", item.key]} then />
            ))}
            {actionShortcuts.map((item) => (
              <ShortcutRow key={item.label} action={item.label} keys={[...item.keys]} then />
            ))}
          </tbody>
        </table>
      </div>
    </Dialog>
  );
}

function ShortcutRow({ action, keys, then = false }: { action: string; keys: string[]; then?: boolean }) {
  return (
    <tr className="h-9">
      <td className="pr-3 text-fg">{action}</td>
      <td className="text-right whitespace-nowrap text-xs text-fg-subtle">
        {keys.map((key, index) => (
          <span key={`${index}-${key}`}>
            {index > 0 && (then ? " then " : " ")}
            <Key>{key}</Key>
          </span>
        ))}
      </td>
    </tr>
  );
}

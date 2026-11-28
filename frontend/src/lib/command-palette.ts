import { useSyncExternalStore } from "react";

interface CommandState {
  // The team the palette was opened for. Switching teams leaves it closed, and its text and results are discarded.
  paletteTeam: string | null;
  help: boolean;
}

let state: CommandState = { paletteTeam: null, help: false };
let returnFocus: HTMLElement | null = null;
const listeners = new Set<() => void>();

function update(next: CommandState) {
  state = next;
  listeners.forEach((listener) => listener());
}

function rememberFocus() {
  const active = document.activeElement;
  returnFocus = active instanceof HTMLElement && active !== document.body ? active : null;
}

function restoreFocus() {
  const target = returnFocus;
  returnFocus = null;
  if (target?.isConnected) target.focus();
}

export function openPalette(team: string) {
  if (state.paletteTeam === null && !state.help) rememberFocus();
  update({ paletteTeam: team, help: false });
}

// After navigating, the page that held the trigger may be gone, so focus is only restored when asked.
export function closePalette(restore = true) {
  if (state.paletteTeam === null) return;
  update({ ...state, paletteTeam: null });
  if (restore) restoreFocus();
  else returnFocus = null;
}

export function openHelp() {
  if (state.paletteTeam === null && !state.help) rememberFocus();
  update({ paletteTeam: null, help: true });
}

export function closeHelp() {
  if (!state.help) return;
  update({ ...state, help: false });
  restoreFocus();
}

function subscribe(listener: () => void) {
  listeners.add(listener);
  return () => listeners.delete(listener);
}

export function useCommandState() {
  return useSyncExternalStore(subscribe, () => state, () => state);
}

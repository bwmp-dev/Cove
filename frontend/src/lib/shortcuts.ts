import { useSyncExternalStore } from "react";

// Off until someone opts in, so Cove never takes keys that browsers or assistive technology use.
const storageKey = "cove.shortcuts";
const listeners = new Set<() => void>();
let enabled = read();

function read() {
  try {
    return window.localStorage.getItem(storageKey) === "on";
  } catch {
    return false;
  }
}

export function setShortcutsEnabled(value: boolean) {
  enabled = value;
  try {
    if (value) window.localStorage.setItem(storageKey, "on");
    else window.localStorage.removeItem(storageKey);
  } catch {
    // Storage can be unavailable (private windows, blocked site data); the choice then lasts for this page load.
  }
  listeners.forEach((listener) => listener());
}

function subscribe(listener: () => void) {
  listeners.add(listener);
  return () => listeners.delete(listener);
}

export function useShortcutsEnabled() {
  return useSyncExternalStore(subscribe, () => enabled, () => false);
}

export const navigationShortcuts = [
  { key: "d", label: "Dashboard", path: "/dashboard" },
  { key: "p", label: "Projects", path: "/projects" },
  { key: "r", label: "All resources", path: "/resources" },
  { key: "o", label: "Operations", path: "/operations" },
  { key: "n", label: "Nodes", path: "/nodes" },
] as const;

// Chords that only refresh data or open existing creation UI; nothing here queues an operation.
export const actionShortcuts = [
  { keys: ["r", "r"], label: "Refresh the current view" },
  { keys: ["n", "r"], label: "Open New resource, when you can create resources" },
] as const;

export function isMac() {
  return typeof navigator !== "undefined" && /Mac|iPhone|iPad/.test(navigator.userAgent);
}

export function isTypingTarget(target: EventTarget | null) {
  if (!(target instanceof Element)) return false;
  if (target instanceof HTMLElement && target.isContentEditable) return true;
  return target.closest("input, textarea, select, [contenteditable=''], [contenteditable='true'], [role='textbox'], [role='combobox'], .cm-editor, .xterm") !== null;
}

export function isInsideOverlay(target: EventTarget | null) {
  if (typeof document !== "undefined" && document.querySelector("dialog[open]")) return true;
  return target instanceof Element && target.closest("dialog, [role='dialog'], [role='menu'], [role='listbox']") !== null;
}

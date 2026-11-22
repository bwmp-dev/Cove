import { useSyncExternalStore } from "react";

// Cron expressions and scheduled datetimes are always evaluated in UTC on the server; only display follows the preference.
export const scheduleTimeZone = "UTC";

// Undefined until the signed-in preference loads, so timestamps keep the browser's zone instead of another team's.
let displayTimeZone: string | undefined;
const listeners = new Set<() => void>();

export function getDisplayTimeZone() {
  return displayTimeZone;
}

export function setDisplayTimeZone(zone: string | undefined) {
  const next = zone && isValidTimeZone(zone) ? zone : undefined;
  if (next === displayTimeZone) return;
  displayTimeZone = next;
  listeners.forEach((listener) => listener());
}

function subscribe(callback: () => void) {
  listeners.add(callback);
  return () => {
    listeners.delete(callback);
  };
}

export function useDisplayTimeZone() {
  return useSyncExternalStore(subscribe, getDisplayTimeZone, getDisplayTimeZone);
}

export function browserTimeZone() {
  return new Intl.DateTimeFormat().resolvedOptions().timeZone;
}

// Without a loaded preference (signed out, loading or the preference API unavailable) times use the browser's zone,
// and the label says so rather than presenting it as the chosen setting.
export function displayTimeZoneLabel(zone: string | undefined) {
  return zone ?? `${browserTimeZone()} (this browser's zone; preference not loaded)`;
}

export function isValidTimeZone(zone: string) {
  try {
    new Intl.DateTimeFormat("en", { timeZone: zone });
    return true;
  } catch {
    return false;
  }
}

export function timeZoneOptions(current: readonly string[] = []) {
  const zones = new Set<string>(["UTC"]);
  try {
    Intl.supportedValuesOf("timeZone").forEach((zone) => zones.add(zone));
  } catch {
    // Older engines without supportedValuesOf still offer UTC and the zones already saved.
  }
  current.filter(Boolean).forEach((zone) => zones.add(zone));
  return ["UTC", ...[...zones].filter((zone) => zone !== "UTC").sort()];
}

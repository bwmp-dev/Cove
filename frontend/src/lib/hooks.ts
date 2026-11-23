import { useState, useSyncExternalStore } from "react";

function subscribeOnline(callback: () => void) {
  window.addEventListener("online", callback);
  window.addEventListener("offline", callback);
  return () => {
    window.removeEventListener("online", callback);
    window.removeEventListener("offline", callback);
  };
}

export function useOnline() {
  return useSyncExternalStore(
    subscribeOnline,
    () => navigator.onLine,
    () => true,
  );
}

interface Clock {
  subscribe: (callback: () => void) => () => void;
  getSnapshot: () => number;
}

function createClock(interval: number): Clock {
  let now = Date.now();
  let timer: number | undefined;
  const listeners = new Set<() => void>();
  return {
    subscribe(callback) {
      listeners.add(callback);
      if (timer === undefined) {
        now = Date.now();
        timer = window.setInterval(() => {
          now = Date.now();
          listeners.forEach((listener) => listener());
        }, interval);
      }
      return () => {
        listeners.delete(callback);
        if (listeners.size === 0 && timer !== undefined) {
          window.clearInterval(timer);
          timer = undefined;
        }
      };
    },
    getSnapshot: () => now,
  };
}

const clocks = new Map<number, Clock>();

function clockFor(interval: number) {
  let clock = clocks.get(interval);
  if (!clock) {
    clock = createClock(interval);
    clocks.set(interval, clock);
  }
  return clock;
}

// Shared ticking clock so relative times across a list update together without per-row timers.
export function useNow(interval: number) {
  const clock = clockFor(interval);
  return useSyncExternalStore(clock.subscribe, clock.getSnapshot, clock.getSnapshot);
}

// The value when the component mounted. Confirmation dialogs mount when they open, so a background refresh cannot
// change what the person reviewed or what the dialog will submit.
export function useReviewed<T>(value: T): T {
  const [reviewed] = useState(value);
  return reviewed;
}

// The first defined value, for reviewed data that finishes loading after a dialog opens.
export function useFirstLoaded<T>(value: T | undefined): T | undefined {
  const [first, setFirst] = useState(value);
  if (first === undefined && value !== undefined) setFirst(value);
  return first ?? value;
}

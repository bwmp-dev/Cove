import { useSyncExternalStore } from "react";

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

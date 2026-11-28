import type { LiveLogEntry, LiveLogSample } from "../api/advice";

export const maxLiveEntries = 2000;
// Each read starts this far before the newest line already shown, so lines written in the same second are not lost.
const cursorOverlap = 1000;

export interface LiveTail {
  entries: LiveLogEntry[];
  // Worker-side time to read from next; null until the first read.
  cursor: string | null;
  sampledAt: string | null;
  // The latest read hit the worker's catch-up, line or size limits, so lines may be missing before it.
  limited: boolean;
  // Older lines removed from the screen to keep it within maxLiveEntries.
  trimmed: number;
  // IDs from the latest read, so overlapping lines stay hidden after Clear or trimming.
  recent: string[];
}

export const emptyTail: LiveTail = { entries: [], cursor: null, sampledAt: null, limited: false, trimmed: 0, recent: [] };

// Go writes RFC 3339 times with up to nine fractional digits, which Date.parse truncates to milliseconds.
function timeKey(timestamp: string): [number, number] {
  const fraction = /\.(\d+)/.exec(timestamp)?.[1] ?? "";
  return [Date.parse(timestamp), Number(fraction.padEnd(9, "0").slice(3, 9))];
}

export function compareTimestamps(a: string, b: string) {
  const [aMs, aNs] = timeKey(a);
  const [bMs, bNs] = timeKey(b);
  return aMs - bMs || aNs - bNs;
}

function shiftBack(timestamp: string, ms: number) {
  const parsed = Date.parse(timestamp);
  return Number.isNaN(parsed) ? null : new Date(parsed - ms).toISOString();
}

export function mergeTail(previous: LiveTail, sample: LiveLogSample): LiveTail {
  const seen = new Set([...previous.recent, ...previous.entries.map((entry) => entry.id)]);
  const added = sample.entries.filter((entry) => {
    if (seen.has(entry.id)) return false;
    seen.add(entry.id);
    return true;
  });
  // A stable sort keeps the worker's order for lines with the same timestamp.
  const merged = added.length ? [...previous.entries, ...added].sort((a, b) => compareTimestamps(a.timestamp, b.timestamp)) : previous.entries;
  const dropped = Math.max(0, merged.length - maxLiveEntries);
  const entries = dropped ? merged.slice(dropped) : merged;
  // The worker's read time moves the cursor even when containers are quiet, so the next read never reaches back
  // past the worker's catch-up window. A limited read is reported instead of re-reading the same span.
  const newest = sample.entries[sample.entries.length - 1];
  const cursor = shiftBack(sample.sampled_at, cursorOverlap) ?? (newest && shiftBack(newest.timestamp, cursorOverlap)) ?? previous.cursor;
  return { entries, cursor, sampledAt: sample.sampled_at, limited: sample.limited, trimmed: previous.trimmed + dropped, recent: sample.entries.map((entry) => entry.id) };
}

// Clearing keeps the cursor so the next read continues from where the tail was instead of re-reading recent output.
export function clearTail(tail: LiveTail): LiveTail {
  return { ...emptyTail, cursor: tail.cursor, sampledAt: tail.sampledAt, recent: tail.recent };
}

export function tailContainers(entries: LiveLogEntry[], selected: string) {
  const names = new Set(entries.map((entry) => entry.container));
  if (selected) names.add(selected);
  return [...names].sort((a, b) => a.localeCompare(b));
}

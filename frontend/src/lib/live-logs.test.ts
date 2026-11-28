import { expect, it } from "vitest";
import type { LiveLogEntry } from "../api/advice";
import { clearTail, compareTimestamps, emptyTail, maxLiveEntries, mergeTail, tailContainers } from "./live-logs";

const line = (id: string, timestamp: string, container = "web-1"): LiveLogEntry => ({ id, container, timestamp, message: id, clipped: false });

it("orders nanosecond timestamps that Date.parse cannot tell apart", () => {
  expect(compareTimestamps("2026-11-22T12:00:00.1Z", "2026-11-22T12:00:00.12Z")).toBeLessThan(0);
  expect(compareTimestamps("2026-11-22T12:00:00.000000002Z", "2026-11-22T12:00:00.000000001Z")).toBeGreaterThan(0);
  expect(compareTimestamps("2026-11-22T13:00:00+01:00", "2026-11-22T12:00:00Z")).toBe(0);
});

it("merges reads in timestamp order, drops overlapping lines by ID and moves the cursor one second before the worker read", () => {
  const first = mergeTail(emptyTail, { entries: [line("a", "2026-11-22T12:00:01Z"), line("b", "2026-11-22T12:00:02Z", "db-1")], sampled_at: "2026-11-22T12:00:03Z", limited: false });
  expect(first.cursor).toBe("2026-11-22T12:00:02.000Z");
  const second = mergeTail(first, { entries: [line("b", "2026-11-22T12:00:02Z", "db-1"), line("c", "2026-11-22T12:00:01.5Z")], sampled_at: "2026-11-22T12:00:05Z", limited: true });
  expect(second.entries.map((entry) => entry.id)).toEqual(["a", "c", "b"]);
  expect(second.limited).toBe(true);
  expect(second.cursor).toBe("2026-11-22T12:00:04.000Z");
});

it("keeps the newest lines within the bound and counts what was removed", () => {
  const entries = Array.from({ length: maxLiveEntries + 5 }, (_, index) => line(String(index), new Date(Date.UTC(2026, 10, 22, 12, 0, 0, index)).toISOString()));
  const tail = mergeTail(emptyTail, { entries, sampled_at: "2026-11-22T12:01:00Z", limited: false });
  expect(tail.entries).toHaveLength(maxLiveEntries);
  expect(tail.entries[0]?.id).toBe("5");
  expect(tail.trimmed).toBe(5);
});

it("clears lines but keeps the cursor and does not bring back overlapping lines", () => {
  const tail = mergeTail(emptyTail, { entries: [line("a", "2026-11-22T12:00:01Z")], sampled_at: "2026-11-22T12:00:02Z", limited: false });
  const cleared = clearTail(tail);
  expect(cleared.entries).toEqual([]);
  expect(cleared.cursor).toBe(tail.cursor);
  const next = mergeTail(cleared, { entries: [line("a", "2026-11-22T12:00:01Z"), line("d", "2026-11-22T12:00:03Z")], sampled_at: "2026-11-22T12:00:04Z", limited: false });
  expect(next.entries.map((entry) => entry.id)).toEqual(["d"]);
});

it("lists containers including the selected one", () => {
  expect(tailContainers([line("a", "2026-11-22T12:00:01Z", "web-1")], "db-1")).toEqual(["db-1", "web-1"]);
});

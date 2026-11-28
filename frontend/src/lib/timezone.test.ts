import { afterEach, describe, expect, it } from "vitest";
import { formatAbsolute } from "./format";
import { getDisplayTimeZone, setDisplayTimeZone, timeZoneOptions } from "./timezone";

afterEach(() => setDisplayTimeZone(undefined));

describe("formatAbsolute with a display time zone", () => {
  it("formats UTC exactly", () => {
    expect(formatAbsolute("2026-03-08T06:30:00Z", "UTC")).toBe("Mar 8, 2026, 6:30:00 AM");
  });

  it("follows daylight saving changes in the preferred zone", () => {
    // New York moves from EST (UTC-5) to EDT (UTC-4) at 07:00 UTC on 8 March 2026.
    expect(formatAbsolute("2026-03-08T06:30:00Z", "America/New_York")).toBe("Mar 8, 2026, 1:30:00 AM");
    expect(formatAbsolute("2026-03-08T07:30:00Z", "America/New_York")).toBe("Mar 8, 2026, 3:30:00 AM");
    // And back on 1 November 2026, when 01:30 local happens twice.
    expect(formatAbsolute("2026-11-01T05:30:00Z", "America/New_York")).toBe("Nov 1, 2026, 1:30:00 AM");
    expect(formatAbsolute("2026-11-01T06:30:00Z", "America/New_York")).toBe("Nov 1, 2026, 1:30:00 AM");
  });

  it("uses the loaded preference by default, including a personal override", () => {
    setDisplayTimeZone("Europe/London");
    expect(formatAbsolute("2026-07-01T12:00:00Z")).toBe("Jul 1, 2026, 1:00:00 PM");
    setDisplayTimeZone("Asia/Kolkata");
    expect(formatAbsolute("2026-07-01T12:00:00Z")).toBe("Jul 1, 2026, 5:30:00 PM");
  });

  it("ignores zones the browser cannot format", () => {
    setDisplayTimeZone("Europe/London");
    setDisplayTimeZone("Not/AZone");
    expect(getDisplayTimeZone()).toBeUndefined();
  });

  it("keeps the unknown-time fallback", () => {
    expect(formatAbsolute("not a date", "UTC")).toBe("Unknown time");
  });
});

it("offers UTC first and keeps saved zones", () => {
  const options = timeZoneOptions(["Europe/London"]);
  expect(options[0]).toBe("UTC");
  expect(options).toContain("Europe/London");
  expect(new Set(options).size).toBe(options.length);
});

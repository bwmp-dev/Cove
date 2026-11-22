import { describe, expect, it } from "vitest";
import { activityLink, pageWindow, parseTags, summarizeBulk } from "./organization";

describe("parseTags", () => {
  it("lowercases, deduplicates and sorts tags", () => {
    expect(parseTags("Web, api  web\nedge.v2")).toEqual({ tags: ["api", "edge.v2", "web"] });
  });

  it("rejects tags the server would refuse", () => {
    expect(parseTags("-bad").error).toContain('"-bad"');
    expect(parseTags("a".repeat(33)).error).toBeDefined();
    expect(parseTags(Array.from({ length: 13 }, (_, index) => `t${index}`).join(" ")).error).toBe("Use at most 12 tags.");
  });

  it("allows clearing every tag", () => {
    expect(parseTags("  ")).toEqual({ tags: [] });
  });
});

describe("activityLink", () => {
  it("maps the server's link shapes to routes", () => {
    expect(activityLink("/operations/abc")).toEqual({ to: "/operations/$operationId", params: { operationId: "abc" } });
    expect(activityLink("/resources/r1")).toEqual({ to: "/resources/$resourceId", params: { resourceId: "r1" } });
    expect(activityLink("/projects/p1")).toEqual({ to: "/projects/$projectId", params: { projectId: "p1" } });
  });

  it("refuses other paths", () => {
    expect(activityLink("//evil.example/x")).toBeNull();
    expect(activityLink("/resources/r1/settings")).toBeNull();
    expect(activityLink("https://example.com")).toBeNull();
  });
});

describe("pageWindow", () => {
  it("clamps the page into range", () => {
    expect(pageWindow(51, 25, 1)).toEqual({ page: 1, pageCount: 3, start: 0, end: 25 });
    expect(pageWindow(51, 25, 9)).toEqual({ page: 3, pageCount: 3, start: 50, end: 51 });
    expect(pageWindow(0, 25, 2)).toEqual({ page: 1, pageCount: 1, start: 0, end: 0 });
  });
});

it("counts queued operations separately from failures", () => {
  expect(summarizeBulk([{ id: "a", status: 202, operation_id: "o" }, { id: "b", status: 403, error: "denied" }, { id: "c", status: 409, error: "busy" }])).toEqual({ queued: 1, failed: 2 });
});

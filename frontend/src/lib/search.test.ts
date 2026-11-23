import { expect, it } from "vitest";
import { isSearchPath, maxSearchBytes, searchBytes } from "./search";

it("accepts only known in-app destinations", () => {
  for (const path of ["/dashboard", "/projects/default", "/projects/p1/environments/e_1", "/resources/abc-123", "/nodes", "/nodes/quota-node", "/operations/op1", "/resources/new", "/api-docs", "/resources/abc-123/logs", "/resources/abc-123/metrics", "/resources/abc-123/files", "/resources/abc-123/backups", "/resources/abc-123/troubleshooting"]) {
    expect(isSearchPath(path)).toBe(true);
  }
  for (const path of ["https://example.com", "//example.com", "/resources/a/../../admin", "/resources/", "/resources/a?x=1", "javascript:alert(1)", "/unknown", "/projects/a/environments/", "/resources/a/terminal", "/resources/a/logs/x", "/status/slug", "/shared/token"]) {
    expect(isSearchPath(path)).toBe(false);
  }
});

it("counts the limit in UTF-8 bytes", () => {
  expect(searchBytes("é")).toBe(2);
  expect(searchBytes("a".repeat(maxSearchBytes))).toBe(maxSearchBytes);
  expect(searchBytes("é".repeat(101))).toBeGreaterThan(maxSearchBytes);
});

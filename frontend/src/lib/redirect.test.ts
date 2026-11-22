import { describe, expect, it } from "vitest";
import { safeRedirect } from "./redirect";

describe("safeRedirect", () => {
  it.each([
    [undefined, "/dashboard"],
    ["/nodes?x=1", "/nodes?x=1"],
    ["//evil.example", "/dashboard"],
    ["/\\evil.example", "/dashboard"],
    ["https://evil.example/", "/dashboard"],
    ["/login?redirect=/nodes", "/dashboard"],
  ])("%s -> %s", (input, expected) => {
    expect(safeRedirect(input)).toBe(expected);
  });
});

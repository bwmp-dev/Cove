import { describe, expect, it } from "vitest";
import { withBasePath } from "./base-path";
import { safeRedirect } from "./redirect";

describe("safeRedirect", () => {
  it.each([
    [undefined, "/dashboard"],
    ["", "/dashboard"],
    ["/nodes?x=1#a", "/nodes?x=1#a"],
    ["/projects", "/projects"],
    ["/api/session", "/api/session"],
    ["/control/cove/nodes", "/control/cove/nodes"],
    ["//evil.example", "/dashboard"],
    ["/\\evil.example", "/dashboard"],
    ["/\t/evil.example", "/dashboard"],
    ["/\n/evil.example", "/dashboard"],
    ["/.//evil.example", "/dashboard"],
    ["https://evil.example/", "/dashboard"],
    ["javascript:alert(1)", "/dashboard"],
    ["nodes", "/dashboard"],
    ["/login?redirect=/nodes", "/dashboard"],
    ["/nodes/../login", "/dashboard"],
  ])("%j -> %s", (input, expected) => {
    expect(safeRedirect(input)).toBe(expected);
  });
});

// Router locations are logical, so the destination is prefixed exactly once whatever the prefix looks like.
describe("post-login address under a panel prefix", () => {
  it.each([
    ["", "/projects?tab=a", "/projects?tab=a"],
    ["/control/cove", "/projects?tab=a", "/control/cove/projects?tab=a"],
    ["/projects", "/projects", "/projects/projects"],
    ["/projects", "/projects/p1", "/projects/projects/p1"],
    ["/api", "/api/session", "/api/api/session"],
    ["/api", "/login", "/api/dashboard"],
  ])("under %j sends %s to %s", (base, destination, expected) => {
    expect(withBasePath(safeRedirect(destination), base)).toBe(expected);
  });
});

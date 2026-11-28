import { describe, expect, it } from "vitest";
import { devProxyPath, parseDevPublicURL } from "./dev-proxy";

describe("parseDevPublicURL", () => {
  it("returns null when unset", () => {
    expect(parseDevPublicURL(undefined)).toBeNull();
    expect(parseDevPublicURL("")).toBeNull();
  });

  it.each([
    ["https://cove.example", "https://cove.example", ""],
    ["https://cove.example/", "https://cove.example", ""],
    ["https://cove.example:8443/control/cove", "https://cove.example:8443", "/control/cove"],
    ["https://cove.example/control/cove/", "https://cove.example", "/control/cove"],
  ])("splits %s into origin and base path", (input, origin, basePath) => {
    expect(parseDevPublicURL(input)).toEqual({ origin, basePath });
  });

  it.each(["cove.example/control", "https://cove.example/a%2fb", "https://cove.example/cove?x=1", "https://cove.example/cove#x", "https://user@cove.example/cove", "https://cove.example/a//b"])(
    "rejects %s",
    (input) => {
      expect(() => parseDevPublicURL(input)).toThrow(/COVE_DEV_PUBLIC_URL/);
    },
  );
});

describe("devProxyPath", () => {
  it("leaves paths unchanged without a configured URL or at the root", () => {
    expect(devProxyPath("/api/session?x=1", null)).toBe("/api/session?x=1");
    expect(devProxyPath("/api/session?x=1", { origin: "https://cove.example", basePath: "" })).toBe("/api/session?x=1");
  });

  it("prefixes the base path and keeps the query", () => {
    const target = { origin: "https://cove.example", basePath: "/control/cove" };
    expect(devProxyPath("/api/projects?limit=5&q=a%2Fb", target)).toBe("/control/cove/api/projects?limit=5&q=a%2Fb");
    expect(devProxyPath("/auth/github/callback", target)).toBe("/control/cove/auth/github/callback");
  });
});

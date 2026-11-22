import { afterEach, describe, expect, it, vi } from "vitest";
import { parseBasePath, stripBasePath, withBasePath } from "./base-path";

describe("parseBasePath", () => {
  it.each([
    [null, ""],
    ["", ""],
    ["/", ""],
    ["/control/cove", "/control/cove"],
    ["/cove_1-a", "/cove_1-a"],
    ["/control/cove/", ""],
    ["control", ""],
    ["//evil.example", ""],
    ["/a/../b", ""],
    ["/a b", ""],
    ["https://evil.example/cove", ""],
  ])("%s -> %s", (input, expected) => {
    expect(parseBasePath(input)).toBe(expected);
  });
});

describe("withBasePath", () => {
  it("leaves paths unchanged at the root", () => {
    expect(withBasePath("/api/session", "")).toBe("/api/session");
  });

  it.each([
    ["/control/cove", "/api/session", "/control/cove/api/session"],
    ["/control/cove", "/control/covet", "/control/cove/control/covet"],
    ["/api", "/api/session", "/api/api/session"],
    ["/api", "/api", "/api/api"],
    ["/projects", "/projects", "/projects/projects"],
    ["/projects", "/projects/p1?tab=a#x", "/projects/projects/p1?tab=a#x"],
  ])("under %s prefixes logical %s even when it starts with the prefix", (base, path, expected) => {
    expect(withBasePath(path, base)).toBe(expected);
  });

  it.each(["https://evil.example/api", "//evil.example/api", "/\\evil.example", "api/session", "javascript:alert(1)", "/\t/evil.example", "/\n/evil.example", "/api\u0000"])("rejects %s", (path) => {
    expect(() => withBasePath(path, "/control/cove")).toThrow();
    expect(() => withBasePath(path, "")).toThrow();
  });
});

describe("stripBasePath", () => {
  it.each([
    ["/control/cove/projects", "/projects"],
    ["/control/cove", "/"],
    ["/control/cove/", "/"],
    ["/control/cove?x=1", "/?x=1"],
    ["/control/covet", "/control/covet"],
    ["/projects", "/projects"],
  ])("%s -> %s", (input, expected) => {
    expect(stripBasePath(input, "/control/cove")).toBe(expected);
  });

  it.each([
    ["/api", "/api/api/session", "/api/session"],
    ["/api", "/api", "/"],
    ["/projects", "/projects/projects", "/projects"],
    ["/projects", "/projects/projects/p1", "/projects/p1"],
  ])("under %s reduces browser address %s to %s", (base, input, expected) => {
    expect(stripBasePath(input, base)).toBe(expected);
    expect(withBasePath(expected, base)).toBe(input === base ? `${base}/` : input);
  });

  it("does nothing at the root", () => {
    expect(stripBasePath("/control/cove/projects", "")).toBe("/control/cove/projects");
  });
});

describe("injected base path", () => {
  afterEach(() => {
    document.head.querySelector('meta[name="cove-base-path"]')?.remove();
    vi.resetModules();
  });

  it("reads the prefix the control plane injects", async () => {
    document.head.insertAdjacentHTML("afterbegin", '<meta name="cove-base-path" content="/control/cove">');
    vi.resetModules();
    const module = await import("./base-path");
    expect(module.basePath).toBe("/control/cove");
    expect(module.withBasePath("/auth/login")).toBe("/control/cove/auth/login");
  });

  it("ignores an invalid prefix", async () => {
    document.head.insertAdjacentHTML("afterbegin", '<meta name="cove-base-path" content="//evil.example">');
    vi.resetModules();
    const module = await import("./base-path");
    expect(module.basePath).toBe("");
  });
});


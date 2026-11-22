import { afterEach, describe, expect, it, vi } from "vitest";
import { captureFragmentToken, forgetFragmentToken, fragmentToken } from "./fragment";

function capture(pathname: string, hash: string) {
  const history = { state: null, replaceState: vi.fn() };
  captureFragmentToken({ pathname, search: "", hash }, history);
  return history.replaceState;
}

describe("fragment tokens", () => {
  afterEach(forgetFragmentToken);

  it("moves a token into memory and scrubs the address", () => {
    const replace = capture("/invite", "#secret-token");
    expect(fragmentToken("/invite")).toBe("secret-token");
    expect(fragmentToken("/verify")).toBeNull();
    expect(replace).toHaveBeenCalledWith(null, "", "/invite");
  });

  it("accepts a token parameter", () => {
    capture("/reset-password", "#token=abc");
    expect(fragmentToken("/reset-password")).toBe("abc");
  });

  it("ignores fragments on other pages", () => {
    const replace = capture("/projects", "#section");
    expect(replace).not.toHaveBeenCalled();
    expect(fragmentToken("/projects")).toBeNull();
  });
});

import { describe, expect, it } from "vitest";
import { isGitHubAuthorizeURL } from "./auth";

describe("GitHub sign-in redirect", () => {
  it("accepts only the GitHub authorization endpoint", () => {
    expect(isGitHubAuthorizeURL("https://github.com/login/oauth/authorize?client_id=x&state=y")).toBe(true);
    expect(isGitHubAuthorizeURL("http://github.com/login/oauth/authorize")).toBe(false);
    expect(isGitHubAuthorizeURL("https://github.com.example.test/login/oauth/authorize")).toBe(false);
    expect(isGitHubAuthorizeURL("https://example.test/login/oauth/authorize")).toBe(false);
    expect(isGitHubAuthorizeURL("https://github.com/settings")).toBe(false);
    expect(isGitHubAuthorizeURL("https://user@github.com/login/oauth/authorize")).toBe(false);
    expect(isGitHubAuthorizeURL("not a url")).toBe(false);
  });
});

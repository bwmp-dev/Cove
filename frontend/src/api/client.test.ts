import { afterEach, describe, expect, it, vi } from "vitest";
import { rememberTeam } from "../lib/team";
import { ApiError, enrolmentRequiredMessage, request } from "./client";

function respond(status: number, body: string, type: string, headers: Record<string, string> = {}) {
  vi.stubGlobal("fetch", vi.fn(() => Promise.resolve(new Response(body, { status, headers: { "content-type": type, ...headers } }))));
}

afterEach(() => {
  vi.unstubAllGlobals();
  rememberTeam(null);
});

describe("request", () => {
  it("sends same-origin credentials and JSON bodies without bearer tokens", async () => {
    respond(200, '{"id":"a"}', "application/json");
    await expect(request("POST", "/api/workloads", { name: "web" })).resolves.toEqual({ id: "a" });
    const [path, init] = vi.mocked(fetch).mock.calls[0] as [string, RequestInit];
    expect(path).toBe("/api/workloads");
    expect(init.credentials).toBe("same-origin");
    expect(init.body).toBe('{"name":"web"}');
    expect(new Headers(init.headers).get("authorization")).toBeNull();
  });

  it("sends the selected team only once one is chosen", async () => {
    respond(200, "[]", "application/json");
    await request("GET", "/api/projects");
    rememberTeam("team-b");
    await request("GET", "/api/projects");
    const teams = (vi.mocked(fetch).mock.calls as [string, RequestInit][]).map(([, init]) => new Headers(init.headers).get("x-cove-team"));
    expect(teams).toEqual([null, "team-b"]);
  });

  it("explains authorization refusals without a server message", async () => {
    respond(403, "", "text/plain");
    await expect(request("GET", "/api/audit")).rejects.toMatchObject({ status: 403, message: "Access denied. Your account does not have permission for this action." });
  });

  it("explains the second-factor enrolment refusal", async () => {
    respond(403, '{"error":"second_factor_enrolment_required"}', "application/json");
    await expect(request("GET", "/api/projects")).rejects.toMatchObject({ status: 403, message: enrolmentRequiredMessage });
  });

  it("uses short plain-text server errors", async () => {
    respond(409, "workload already has an active operation\n", "text/plain; charset=utf-8");
    await expect(request("POST", "/x")).rejects.toMatchObject({ status: 409, message: "Workload already has an active operation" });
  });

  it("replaces HTML error pages with a generic message", async () => {
    respond(502, "<html><body>Bad gateway</body></html>", "text/html");
    await expect(request("GET", "/x")).rejects.toMatchObject({ status: 502, message: "The worker could not be reached." });
  });

  it("reads Retry-After for rate limiting", async () => {
    respond(429, "too many attempts", "text/plain", { "retry-after": "60" });
    const error = (await request("POST", "/auth/login").catch((caught: unknown) => caught)) as ApiError;
    expect(error.retryAfter).toBe(60);
  });

  it("reports network failures with status 0", async () => {
    vi.stubGlobal("fetch", vi.fn().mockRejectedValue(new TypeError("Failed to fetch")));
    const error = (await request("GET", "/api/nodes").catch((caught: unknown) => caught)) as ApiError;
    expect(error).toBeInstanceOf(ApiError);
    expect(error.isNetwork).toBe(true);
  });
});

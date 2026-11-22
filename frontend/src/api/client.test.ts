import { afterEach, describe, expect, it, vi } from "vitest";
import { rememberTeam } from "../lib/team";
import { backupDownloadUrl } from "./backup-transfers";
import { ApiError, request, uploadFile } from "./client";

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

describe("uploadFile", () => {
  it("sends the file itself as the body with its size and the team, not JSON", async () => {
    respond(201, '{"backup_id":"b1","size":5,"sha256":"aa","verified":true}', "application/json");
    rememberTeam("team-b");
    const file = new File(["hello"], "dump.rdb");
    await expect(uploadFile("/api/workloads/w1/backups/import", file)).resolves.toMatchObject({ backup_id: "b1" });
    const [path, init] = vi.mocked(fetch).mock.calls[0] as [string, RequestInit];
    const headers = new Headers(init.headers);
    expect(path).toBe("/api/workloads/w1/backups/import");
    expect(init.method).toBe("POST");
    expect(init.body).toBe(file);
    expect(init.credentials).toBe("same-origin");
    expect(headers.get("content-type")).toBe("application/octet-stream");
    expect(headers.get("x-cove-size")).toBe("5");
    expect(headers.get("x-cove-team")).toBe("team-b");
    expect(headers.get("authorization")).toBeNull();
  });

  it("reports the server's refusal like other requests", async () => {
    respond(413, "dump exceeds the configured backup transfer limit", "text/plain");
    await expect(uploadFile("/x", new Blob(["a"]))).rejects.toMatchObject({ status: 413, message: "Dump exceeds the configured backup transfer limit" });
  });
});

describe("backupDownloadUrl", () => {
  it("is a same-origin path that adds only the selected team as context", () => {
    expect(backupDownloadUrl("w1", "b 1")).toBe("/api/workloads/w1/backups/b%201/download");
    rememberTeam("team-b");
    expect(backupDownloadUrl("w1", "b1")).toBe("/api/workloads/w1/backups/b1/download?team_id=team-b");
  });
});

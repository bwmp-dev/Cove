import { afterEach, describe, expect, it, vi } from "vitest";
import { rememberTeam } from "../lib/team";
import { backupDownloadUrl } from "./backup-transfers";
import { ApiError, enrolmentRequiredMessage, request, requestFile, requestForTeam, requestPublic, uploadFile } from "./client";

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

  it("names an explicit team without changing the selected team", async () => {
    respond(200, "[]", "application/json");
    rememberTeam("team-a");
    await requestForTeam("team-b", "GET", "/api/projects");
    await request("GET", "/api/projects");
    const teams = (vi.mocked(fetch).mock.calls as [string, RequestInit][]).map(([, init]) => new Headers(init.headers).get("x-cove-team"));
    expect(teams).toEqual(["team-b", "team-a"]);
  });

  it("keeps structured JSON error bodies with a generic message", async () => {
    respond(409, '{"version":"v2","blockers":["Detach networks"]}', "application/json");
    await expect(request("POST", "/x")).rejects.toMatchObject({ status: 409, message: "The request conflicts with the current state.", details: { version: "v2", blockers: ["Detach networks"] } });
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

describe("requestFile", () => {
  it("returns the attachment and its server-chosen name without caching", async () => {
    respond(200, "PK", "application/zip", { "content-disposition": 'attachment; filename="shop-configuration.zip"' });
    const { blob, filename } = await requestFile("POST", "/api/projects/p/configuration/export", {});
    expect(filename).toBe("shop-configuration.zip");
    expect(await blob.text()).toBe("PK");
    const [, init] = vi.mocked(fetch).mock.calls[0] as [string, RequestInit];
    expect(init.cache).toBe("no-store");
    expect(init.body).toBe("{}");
  });

  it("reports plain-text errors like JSON requests", async () => {
    respond(409, "required shared variable is missing", "text/plain");
    await expect(requestFile("POST", "/api/projects/p/configuration/export", {})).rejects.toEqual(new ApiError(409, "Required shared variable is missing"));
  });
});

describe("requestPublic", () => {
  it("omits the session cookie and the selected team", async () => {
    respond(200, '{"title":"Shop"}', "application/json");
    rememberTeam("team-a");
    await expect(requestPublic("/api/public/projects/token")).resolves.toEqual({ title: "Shop" });
    const [path, init] = vi.mocked(fetch).mock.calls[0] as [string, RequestInit];
    expect(path).toBe("/api/public/projects/token");
    expect(init.method).toBe("GET");
    expect(init.credentials).toBe("omit");
    expect(new Headers(init.headers).get("x-cove-team")).toBeNull();
  });

  it("reports errors like other requests", async () => {
    respond(404, "not found", "text/plain");
    await expect(requestPublic("/api/public/projects/token")).rejects.toEqual(new ApiError(404, "Not found"));
  });
});

describe("requests under a panel prefix", () => {
  afterEach(() => {
    document.head.querySelector('meta[name="cove-base-path"]')?.remove();
    vi.resetModules();
  });

  it("prefixes API, auth, public and download requests once", async () => {
    document.head.insertAdjacentHTML("afterbegin", '<meta name="cove-base-path" content="/control/cove">');
    vi.resetModules();
    const client = await import("./client");
    respond(200, "{}", "application/json");
    await client.request("GET", "/api/session");
    await client.request("POST", "/auth/login", {});
    await client.requestPublic("/api/public/projects/token");
    await client.requestPublic("/api/status/shop");
    await client.requestFile("POST", "/api/projects/p/configuration/export", {});
    await client.uploadFile("/api/workloads/w1/backups/import", new Blob(["a"]));
    await client.requestText("/api/workloads/w1/log-history?download=1");
    await client.requestBlob("/api/workloads/w1/files/download?kind=volume");
    const paths = (vi.mocked(fetch).mock.calls as [string, RequestInit][]).map(([path]) => path);
    expect(paths).toEqual([
      "/control/cove/api/session",
      "/control/cove/auth/login",
      "/control/cove/api/public/projects/token",
      "/control/cove/api/status/shop",
      "/control/cove/api/projects/p/configuration/export",
      "/control/cove/api/workloads/w1/backups/import",
      "/control/cove/api/workloads/w1/log-history?download=1",
      "/control/cove/api/workloads/w1/files/download?kind=volume",
    ]);
  });

  it("prefixes browser-handled backup download links", async () => {
    document.head.insertAdjacentHTML("afterbegin", '<meta name="cove-base-path" content="/control/cove">');
    vi.resetModules();
    const transfers = await import("./backup-transfers");
    expect(transfers.backupDownloadUrl("w1", "b1")).toBe("/control/cove/api/workloads/w1/backups/b1/download");
  });

  it("prefixes browser-handled file download links", async () => {
    document.head.insertAdjacentHTML("afterbegin", '<meta name="cove-base-path" content="/control/cove">');
    vi.resetModules();
    const files = await import("./files");
    expect(files.fileDownloadUrl("w1", { kind: "volume", target: "data", path: "/a b.txt" })).toMatch(/^\/control\/cove\/api\/workloads\/w1\/files\/download\?kind=volume&target=data&path=%2Fa\+b\.txt/);
  });

  it.each(["/api", "/projects"])("prefixes requests under %s even when the path starts with it", async (base) => {
    document.head.insertAdjacentHTML("afterbegin", `<meta name="cove-base-path" content="${base}">`);
    vi.resetModules();
    const client = await import("./client");
    respond(200, "{}", "application/json");
    await client.request("GET", "/api/session");
    await client.request("GET", "/api/projects");
    const paths = (vi.mocked(fetch).mock.calls as [string, RequestInit][]).map(([path]) => path);
    expect(paths).toEqual([`${base}/api/session`, `${base}/api/projects`]);
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

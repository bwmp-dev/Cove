import { execFileSync, spawn } from "node:child_process";
import { request as httpsRequest } from "node:https";
import { Duplex } from "node:stream";
import { connect } from "node:tls";
import { expect, test, type APIResponse, type Page, type TestInfo } from "@playwright/test";

const password = process.env.COVE_BROWSER_PASSWORD ?? "";

test.afterEach(() => {
  const database = process.env.COVE_BROWSER_DATABASE;
  if (!database || !/^cove-browser-[a-f0-9]+-db$/.test(database)) throw new Error("Isolated browser database required for cleanup");
  const resources = execFileSync("docker", ["exec", database, "psql", "-U", "postgres", "-d", "cove", "-Atc", "UPDATE workloads w SET manually_stopped=true WHERE NOT EXISTS(SELECT 1 FROM operations o WHERE o.workload_id=w.id AND o.state IN ('queued','running')) RETURNING w.id"], { encoding: "utf8" }).split(/\s+/).filter((value) => /^[a-f0-9]{32}$/.test(value));
  for (const resource of resources) {
    const ids = execFileSync("docker", ["ps", "-aq", "--filter", `label=com.docker.compose.project=cove-${resource}`], { encoding: "utf8" }).split(/\s+/).filter(Boolean);
    if (ids.length) execFileSync("docker", ["rm", "-f", "-v", ...ids]);
    const networks = execFileSync("docker", ["network", "ls", "-q", "--filter", `label=com.docker.compose.project=cove-${resource}`], { encoding: "utf8" }).split(/\s+/).filter(Boolean);
    if (networks.length) execFileSync("docker", ["network", "rm", ...networks]);
  }
  // The maintenance, error page and forward-auth helper joins the ingress network. Remove only helpers owned by this
  // fixture's worker, identified by its full build owner; helpers of any other worker on this Docker host are left alone.
  const worker = process.env.COVE_BROWSER_WORKER;
  if (!worker || !/^cove-browser-[a-f0-9]+-worker$/.test(worker)) throw new Error("Isolated browser worker required for cleanup");
  // The worker writes its owner before creating its first helper, so without the file there is nothing of ours to remove.
  const ownerFile = execFileSync("docker", ["exec", worker, "sh", "-c", "test ! -e /var/lib/cove/build-owner.json || cat /var/lib/cove/build-owner.json"], { encoding: "utf8" }).trim();
  if (ownerFile === "") return;
  const owner: unknown = JSON.parse(ownerFile);
  if (typeof owner !== "string" || !/^[a-f0-9]{32}$/.test(owner)) throw new Error("The browser worker's build owner is not a 32 character hex value");
  const helpers = execFileSync("docker", ["ps", "-aq", "--no-trunc", "--filter", "label=cove.managed=route-aux", "--filter", `label=cove.route-aux-owner=${owner}`], { encoding: "utf8" }).split(/\s+/).filter(Boolean);
  for (const helper of helpers) {
    const [managed, helperOwner] = execFileSync("docker", ["inspect", "--format", '{{index .Config.Labels "cove.managed"}}|{{index .Config.Labels "cove.route-aux-owner"}}', helper], { encoding: "utf8" }).trim().split("|");
    if (managed !== "route-aux" || helperOwner !== owner) throw new Error(`Route helper ${helper} is not owned by this browser worker`);
    execFileSync("docker", ["rm", "-f", helper]);
  }
});

async function expectNoHorizontalScroll(page: Page) {
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBe(true);
}

async function capture(page: Page, testInfo: TestInfo, name: string) {
  for (const width of [320, 390, 768, 1440]) {
    await page.setViewportSize({ width, height: 900 });
    await expectNoHorizontalScroll(page);
    await page.screenshot({ path: testInfo.outputPath(`${name}-${width}.png`), fullPage: true });
  }
  await page.setViewportSize({ width: 1280, height: 900 });
}

interface Routed {
  status: number;
  headers: Record<string, string | string[] | undefined>;
  body: string;
}

// An HTTPS request through the real Traefik ingress. The ingress publishes no host ports and busybox wget drops error
// response bodies, so Node's TLS client runs over `nc` inside the ingress container and sees every status, header and body.
function routed(hostname: string, path: string, headers: Record<string, string> = {}): Promise<Routed> {
  const tunnel = spawn("docker", ["exec", "-i", "cove-ingress", "nc", "127.0.0.1", "443"], { stdio: ["pipe", "pipe", "inherit"] });
  const pipe = new Duplex({
    read() {
      tunnel.stdout.resume();
    },
    write(chunk: Buffer, _encoding, callback) {
      tunnel.stdin.write(chunk, callback);
    },
    final(callback) {
      tunnel.stdin.end(callback);
    },
  });
  tunnel.stdout.on("data", (chunk: Buffer) => {
    if (!pipe.push(chunk)) tunnel.stdout.pause();
  });
  tunnel.stdout.on("end", () => pipe.push(null));
  const socket = connect({ socket: pipe, servername: hostname, rejectUnauthorized: false });
  return new Promise<Routed>((resolve, reject) => {
    const request = httpsRequest({ createConnection: () => socket, host: hostname, path, method: "GET", headers: { Host: hostname, Connection: "close", ...headers }, timeout: 30_000 }, (response) => {
      let body = "";
      response.setEncoding("utf8");
      response.on("data", (chunk: string) => (body += chunk));
      response.on("end", () => resolve({ status: response.statusCode ?? 0, headers: response.headers, body }));
      response.on("error", reject);
    });
    request.on("timeout", () => request.destroy(new Error(`Timed out requesting ${hostname}${path}`)));
    request.on("error", reject);
    request.end();
  }).finally(() => tunnel.kill());
}

// Shell scripts are passed through Compose, so every $ is doubled. The auth service allows requests carrying
// session=fake, adds Remote-User unless /tmp/no-identity exists, refreshes a cookie, and redirects everyone else.
const web = [
  "mkdir -p /www/cgi-bin; echo application-marker > /www/index.html",
  "cat > /www/cgi-bin/who <<'END'",
  "#!/bin/sh",
  "printf 'Content-Type: text/plain\\r\\n\\r\\nwho:%s\\n' \"$$HTTP_REMOTE_USER\"",
  "END",
  "cat > /www/cgi-bin/fail <<'END'",
  "#!/bin/sh",
  "printf 'Status: 502 Bad Gateway\\r\\nContent-Type: text/plain\\r\\n\\r\\napplication-502\\n'",
  "END",
  "chmod +x /www/cgi-bin/who /www/cgi-bin/fail",
  "exec httpd -f -p 8080 -h /www",
].join("\n");
const auth = [
  "mkdir -p /www/cgi-bin; echo auth-ready > /www/index.html",
  "cat > /www/cgi-bin/check <<'END'",
  "#!/bin/sh",
  'case "$$HTTP_COOKIE" in',
  " *session=fake*)",
  "   printf 'Status: 200 OK\\r\\n'",
  "   if ! test -f /tmp/no-identity; then printf 'Remote-User: alice\\r\\n'; fi",
  "   printf 'Set-Cookie: refreshed=fake; Path=/; HttpOnly; Secure; SameSite=Lax\\r\\nContent-Type: text/plain\\r\\n\\r\\n'",
  "   ;;",
  " *) printf 'Status: 302 Found\\r\\nLocation: https://auth.example.test/login\\r\\nContent-Type: text/plain\\r\\n\\r\\nlogin-required' ;;",
  "esac",
  "END",
  "chmod +x /www/cgi-bin/check",
  "exec httpd -f -p 8081 -h /www",
].join("\n");
const service = (script: string, port: number) =>
  [
    "    image: busybox:1.37",
    "    pull_policy: never",
    "    init: true",
    "    restart: unless-stopped",
    `    command: ${JSON.stringify(["sh", "-ec", script])}`,
    "    healthcheck:",
    `      test: [CMD, wget, -q, -O, /dev/null, 'http://127.0.0.1:${port}/']`,
    "      interval: 1s",
    "      timeout: 2s",
    "      retries: 3",
  ].join("\n");
const compose = ["services:", "  web:", service(web, 8080), "  auth:", service(auth, 8081), ""].join("\n");

interface Operation {
  id: string;
  action: string;
  state: string;
  result: { error?: string } | null;
}

interface SavedRoute {
  domain: string;
  service: string;
  port: number;
  policy?: Record<string, unknown>;
}

test("route pages and forward auth: maintenance, error pages and owned-service forward-auth configured in the dashboard are enforced by the real ingress", async ({ page }, testInfo) => {
  test.setTimeout(360_000);
  await page.goto("/projects");
  await expect(page).toHaveURL(/\/login(?:[?#]|$)/);
  await page.getByLabel("Password", { exact: true }).fill(password);
  await page.getByRole("button", { name: "Sign in", exact: true }).click();
  await expect(page).not.toHaveURL(/\/login(?:[?#]|$)/);
  const headers = { Origin: new URL(page.url()).origin };

  const nodes = (await (await page.request.get("/api/nodes")).json()) as { id: string; name: string; capabilities: { route_protocol?: number } | null }[];
  const node = nodes.find((item) => item.name === "ready-browser-worker");
  if (!node) throw new Error("The browser worker is missing");
  expect(node.capabilities?.route_protocol ?? 0).toBeGreaterThanOrEqual(2);
  const project = (await (await page.request.post("/api/projects", { headers, data: { name: "Route pages project" } })).json()) as { id: string };
  const created = await page.request.post("/api/workloads", { headers, data: { name: "route-pages", node_id: node.id, kind: "service", compose, project_id: project.id } });
  expect(created.status(), await created.text()).toBe(201);
  const { id } = (await created.json()) as { id: string };
  const hostname = `pages-${Date.now()}.cove.test`;

  const operation = async (operationId: string) => (await (await page.request.get(`/api/operations/${operationId}`)).json()) as Operation;
  const settled = async (operationId: string) => {
    await expect.poll(async () => (await operation(operationId)).state, { timeout: 180_000 }).toMatch(/^(succeeded|failed|cancelled|interrupted)$/);
    const result = await operation(operationId);
    if (result.state !== "succeeded") throw new Error(`${result.action} ${operationId} ended ${result.state}: ${result.result?.error ?? ""}\n${await (await page.request.get(`/api/operations/${operationId}/logs`)).text()}`);
  };
  const activeOperations = async () => ((await (await page.request.get(`/api/operations?workload_id=${id}`)).json()) as Operation[]).filter((item) => item.state === "queued" || item.state === "running");
  const quiet = () => expect.poll(async () => (await activeOperations()).map((item) => item.action), { timeout: 120_000 }).toEqual([]);
  const send = async (request: () => Promise<APIResponse>, expected: number[]) => {
    for (let attempt = 1; ; attempt++) {
      await quiet();
      const response = await request();
      if (attempt < 3 && response.status() === 409 && (await response.text()).includes("active operation")) continue;
      expect(expected, await response.text()).toContain(response.status());
      return response;
    }
  };
  const savedRoutes = async () => ((await (await page.request.get(`/api/workloads/${id}`)).json()) as { routes?: SavedRoute[] }).routes ?? [];

  const deploy = (await (await send(() => page.request.post(`/api/workloads/${id}/operations`, { headers, data: { action: "deploy" } }), [202])).json()) as { id: string };
  await settled(deploy.id);
  // The isolated ingress cannot reach a certificate authority, so the route uses the proxy's default certificate.
  const base = await send(() => page.request.put(`/api/workloads/${id}/routes`, { headers, data: { routes: [{ domain: hostname, service: "web", port: 8080, policy: { certificate_source: "default" } }] } }), [200, 202]);
  const baseUpdate = (await base.json()) as { id?: string };
  if (baseUpdate.id) await settled(baseUpdate.id);
  await expect.poll(async () => (await routed(hostname, "/")).body, { timeout: 60_000 }).toBe("application-marker\n");

  // Saves the Domains form and returns the exact routes body the dashboard sent.
  const saveRoutes = async () => {
    const save = page.getByRole("button", { name: "Save and apply", exact: true });
    for (let attempt = 1; ; attempt++) {
      await quiet();
      await expect(save).toBeEnabled({ timeout: 30_000 });
      const pending = page.waitForResponse((response) => new URL(response.url()).pathname === `/api/workloads/${id}/routes` && response.request().method() === "PUT");
      await save.click();
      const response = await pending;
      if (attempt < 3 && response.status() === 409 && (await response.text()).includes("active operation")) continue;
      expect(response.status(), await response.text()).toBe(202);
      await settled(((await response.json()) as { id: string }).id);
      await expect(page.getByText("Routes applied", { exact: true })).toBeVisible({ timeout: 30_000 });
      return response.request().postDataJSON() as { routes: SavedRoute[] };
    }
  };
  const openAdvanced = async () => {
    const toggle = page.getByRole("button", { name: /^Advanced routing/ });
    await expect(toggle).toBeVisible({ timeout: 30_000 });
    if ((await toggle.getAttribute("aria-expanded")) !== "true") await toggle.click();
    await expect(page.getByRole("group", { name: "Forward-auth" })).toBeVisible();
  };

  // Owned-service forward-auth and a custom 502 page, entered in the dashboard.
  await page.goto(`/resources/${id}/domains`);
  await openAdvanced();
  const authGroup = page.getByRole("group", { name: "Forward-auth" });
  await authGroup.getByRole("combobox", { name: "Provider" }).click();
  await page.getByRole("option", { name: "Service in this resource" }).click();
  await authGroup.getByLabel("Compose service").fill("auth");
  await authGroup.getByLabel("Container port").fill("8081");
  await authGroup.getByLabel(/^Path/).fill("/cgi-bin/check");
  await authGroup.getByLabel(/Identity headers/).fill("Remote-User");
  await authGroup.getByLabel(/Provider cookies/).fill("refreshed");
  const pages = page.getByRole("group", { name: "Error pages" });
  await pages.getByRole("button", { name: "Add error page" }).click();
  await pages.getByLabel("Status", { exact: true }).fill("502");
  await pages.getByLabel("HTML for status 502").fill("<h1>custom-502-marker</h1>");
  const responseHeaders = page.getByRole("group", { name: "Response headers" });
  await responseHeaders.getByRole("button", { name: "Add response header" }).click();
  await responseHeaders.getByLabel("Name", { exact: true }).fill("X-Route-Secret");
  await responseHeaders.getByLabel("Value", { exact: true }).fill("e2e-header-secret");
  await capture(page, testInfo, "route-pages-auth-form");
  const authBody = await saveRoutes();
  expect(authBody.routes).toEqual([
    {
      domain: hostname,
      service: "web",
      port: 8080,
      policy: {
        certificate_source: "default",
        error_pages: { "502": "<h1>custom-502-marker</h1>" },
        forward_auth: { service: "auth", port: 8081, path: "/cgi-bin/check", response_headers: ["Remote-User"], response_cookies: ["refreshed"] },
        response_headers: { "X-Route-Secret": "e2e-header-secret" },
      },
    },
  ]);
  // The API never returns the verification path; it stays sealed with the saved route.
  const redactedAuth = { service: "auth", port: 8081, response_headers: ["Remote-User"], response_cookies: ["refreshed"] };
  const expectRedactedAuth = async () => {
    const saved = (await savedRoutes())[0]?.policy?.forward_auth;
    expect(saved).toEqual(redactedAuth);
    expect(saved).not.toHaveProperty("path");
    expect(saved).not.toHaveProperty("url");
  };
  await expectRedactedAuth();
  // Saved header values are returned only as the reserved marker; the proxy still sends the real value.
  const hiddenHeaders = { "X-Route-Secret": "[COVE_REDACTED]" };
  expect((await savedRoutes())[0]?.policy?.response_headers).toEqual(hiddenHeaders);

  const denied = await routed(hostname, "/cgi-bin/who", { "Remote-User": "forged-admin" });
  expect(denied.status).toBe(302);
  expect(denied.headers.location).toBe("https://auth.example.test/login");
  expect(denied.body).not.toContain("who:");
  expect(denied.body).not.toContain("custom-502-marker");
  const allowed = await routed(hostname, "/cgi-bin/who", { Cookie: "session=fake", "Remote-User": "forged-admin" });
  expect(allowed.status).toBe(200);
  expect(allowed.body).toBe("who:alice\n");
  expect(String(allowed.headers["set-cookie"] ?? "")).toContain("refreshed=fake");
  expect(allowed.headers["x-route-secret"]).toBe("e2e-header-secret");
  const failing = await routed(hostname, "/cgi-bin/fail", { Cookie: "session=fake" });
  expect(failing.status).toBe(502);
  expect(failing.body).toContain("custom-502-marker");
  expect(failing.body).not.toContain("application-502");
  expect(failing.headers["cache-control"]).toBe("no-store");

  // A provider that omits the identity header must not let a client's forged value through.
  const authContainer = execFileSync("docker", ["ps", "-q", "--filter", `label=com.docker.compose.project=cove-${id}`, "--filter", "label=com.docker.compose.service=auth"], { encoding: "utf8" }).trim();
  execFileSync("docker", ["exec", authContainer, "touch", "/tmp/no-identity"]);
  const anonymous = await routed(hostname, "/cgi-bin/who", { Cookie: "session=fake", "Remote-User": "forged-admin" });
  expect(anonymous.status).toBe(200);
  expect(anonymous.body).toBe("who:\n");
  execFileSync("docker", ["exec", authContainer, "rm", "/tmp/no-identity"]);

  // Protected routes keep working while this fixture's worker container is down and after it starts again.
  const expectProtected = async () => {
    const outsider = await routed(hostname, "/cgi-bin/who", { "Remote-User": "forged-admin" });
    expect(outsider.status).toBe(302);
    expect(outsider.body).not.toContain("who:");
    const member = await routed(hostname, "/cgi-bin/who", { Cookie: "session=fake", "Remote-User": "forged-admin" });
    expect(member.status).toBe(200);
    expect(member.body).toBe("who:alice\n");
  };
  const worker = process.env.COVE_BROWSER_WORKER;
  if (!worker || !/^cove-browser-[a-f0-9]+-worker$/.test(worker)) throw new Error("Isolated browser worker required for the outage check");
  await quiet();
  execFileSync("docker", ["stop", worker]);
  try {
    expect(execFileSync("docker", ["inspect", "--format", "{{.State.Running}}", worker], { encoding: "utf8" }).trim()).toBe("false");
    await expectProtected();
  } finally {
    execFileSync("docker", ["start", worker]);
  }
  // The panel reaches the worker by its network alias over SSH; verify it again before the remaining dashboard saves.
  const verify = async () => {
    const response = await page.request.post(`/api/nodes/${node.id}/verify`, { headers });
    return response.ok() ? ((await response.json()) as { state: string; capabilities?: { route_protocol?: number } }) : { state: `http ${response.status()}` };
  };
  await expect.poll(async () => (await verify()).state, { timeout: 120_000, intervals: [2_000] }).toBe("ready");
  expect((await verify()).capabilities?.route_protocol ?? 0).toBeGreaterThanOrEqual(2);
  await expectProtected();

  // Maintenance with a custom page, switched on from the keyboard. Forward-auth still answers first.
  await page.reload();
  await openAdvanced();
  const maintenance = page.getByRole("group", { name: "Maintenance" });
  const maintenanceSwitch = maintenance.getByRole("switch", { name: "Maintenance mode" });
  await maintenanceSwitch.focus();
  await page.keyboard.press("Space");
  await expect(maintenanceSwitch).toBeChecked();
  await page.keyboard.press("Tab");
  await expect(maintenance.getByLabel(/Maintenance page/)).toBeFocused();
  await page.keyboard.type("<h1>maintenance-marker</h1>");
  await expect(page.getByRole("button", { name: /^Advanced routing/ })).toHaveAttribute("aria-expanded", "true");
  await capture(page, testInfo, "route-pages-maintenance-form");
  const maintenanceBody = await saveRoutes();
  expect(maintenanceBody.routes[0]?.policy?.maintenance).toEqual({ enabled: true, html: "<h1>maintenance-marker</h1>" });
  // The untouched path is left out so the API keeps the sealed value instead of clearing it.
  expect(maintenanceBody.routes[0]?.policy?.forward_auth).toEqual(redactedAuth);
  expect(maintenanceBody.routes[0]?.policy?.forward_auth).not.toHaveProperty("path");
  expect(maintenanceBody.routes[0]?.policy?.forward_auth).not.toHaveProperty("url");
  expect(maintenanceBody.routes[0]?.policy?.error_pages).toEqual({ "502": "<h1>custom-502-marker</h1>" });
  // The untouched hidden header value is sent back as the marker, so the API keeps the saved value.
  await expect(responseHeaders.getByLabel("Replacement value", { exact: true })).toHaveValue("");
  expect(maintenanceBody.routes[0]?.policy?.response_headers).toEqual(hiddenHeaders);

  const maintenanceDenied = await routed(hostname, "/deep/path?q=1");
  // The auth service serves 200 at "/", so this redirect proves the retained /cgi-bin/check path still answers.
  expect(maintenanceDenied.status).toBe(302);
  expect(maintenanceDenied.headers.location).toBe("https://auth.example.test/login");
  expect(maintenanceDenied.body).not.toContain("maintenance-marker");
  const inMaintenance = await routed(hostname, "/deep/path?q=1", { Cookie: "session=fake" });
  expect(inMaintenance.status).toBe(503);
  expect(inMaintenance.body).toContain("maintenance-marker");
  expect(inMaintenance.headers["retry-after"]).toBe("60");
  expect(inMaintenance.headers["cache-control"]).toBe("no-store");
  expect(String(inMaintenance.headers["content-security-policy"] ?? "")).not.toBe("");
  await expectRedactedAuth();

  await page.goto(`/resources/${id}/domains`);
  await expect(page.getByRole("button", { name: /^Advanced routing · .*maintenance on, forward-auth, 1 error page/ })).toBeVisible({ timeout: 30_000 });
  await capture(page, testInfo, "route-pages-summary");

  // Removing every setting in the dashboard sends explicit nulls, so the API does not keep the saved values.
  await openAdvanced();
  await maintenance.getByRole("switch", { name: "Maintenance mode" }).click();
  await maintenance.getByLabel(/Maintenance page/).fill("");
  await authGroup.getByRole("combobox", { name: "Provider" }).click();
  await page.getByRole("option", { name: "Off" }).click();
  await expect(page.getByText("Saving removes forward-auth", { exact: true })).toBeVisible();
  await pages.getByRole("button", { name: "Remove error page 502" }).click();
  await responseHeaders.getByRole("button", { name: "Remove response header X-Route-Secret" }).click();
  const clearedBody = await saveRoutes();
  expect(clearedBody.routes).toEqual([{ domain: hostname, service: "web", port: 8080, policy: { certificate_source: "default", maintenance: null, error_pages: null, forward_auth: null, response_headers: null } }]);
  expect(await savedRoutes()).toEqual([{ domain: hostname, service: "web", port: 8080, policy: { certificate_source: "default" } }]);
  const open = await routed(hostname, "/cgi-bin/who");
  expect(open.status).toBe(200);
  expect(open.body).toBe("who:\n");
  expect(open.headers["x-route-secret"]).toBeUndefined();
  const plainFailure = await routed(hostname, "/cgi-bin/fail");
  expect(plainFailure.status).toBe(502);
  expect(plainFailure.body).toContain("application-502");
});

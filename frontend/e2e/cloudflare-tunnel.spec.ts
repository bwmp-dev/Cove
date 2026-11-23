import { readFileSync } from "node:fs";
import { request as httpsRequest } from "node:https";
import { expect, test, type APIResponse, type Page } from "@playwright/test";
import { capture, certificateAuthority, cleanupBrowserResources, verified } from "./route-tls";

// Runs only against scripts/test-browser.py --cloudflare-tunnel: an owned fake Cloudflare API and edge reached through
// the api.cloudflare.com alias, and a fake connector. No real Cloudflare account, edge or official connector is used.
const password = process.env.COVE_BROWSER_PASSWORD ?? "";
const provider = process.env.COVE_BROWSER_TUNNEL_PROVIDER ?? "";
const providerToken = process.env.COVE_BROWSER_TUNNEL_TOKEN ?? "";
const providerRoot = process.env.COVE_BROWSER_TUNNEL_ROOT ?? "";
// The fixture provider's only zone.
const providerZone = "a".repeat(32);

test.afterEach(cleanupBrowserResources);

const web = "mkdir -p /www; echo tunnel-marker > /www/index.html; exec httpd -f -p 8080 -h /www";
const compose = [
  "services:",
  "  web:",
  "    image: busybox:1.37",
  "    pull_policy: never",
  "    init: true",
  "    restart: unless-stopped",
  `    command: ${JSON.stringify(["sh", "-ec", web])}`,
  "    healthcheck:",
  "      test: [CMD, wget, -q, -O, /dev/null, 'http://127.0.0.1:8080/']",
  "      interval: 1s",
  "      timeout: 2s",
  "      retries: 3",
  "",
].join("\n");

interface Operation {
  id: string;
  action: string;
  state: string;
  result: { error?: string } | null;
}

interface TunnelStatus {
  configuration: { zone_id: string; hostnames: string[]; image: string } | null;
  phase: string;
  cleanup_requested: boolean;
  last_error: string;
  worker: { phase: string; running: boolean; connected: boolean; last_error?: string; withheld: Record<string, string> | null; hostnames: string[] | null; image: string } | null;
  worker_error: string | null;
  zones: { id: string; domain: string; connection_kind: string }[] | null;
  available_hostnames: string[] | null;
  dns: { hostname: string; phase: string; cleanup_requested: boolean; last_error: string }[] | null;
  default_image: string;
}

interface ProviderState {
  tunnels: number;
  records: number;
  creates: number;
  deletes: number;
  dns_creates: number;
  dns_deletes: number;
}

// A request to the owned fake provider, verified against the fixture CA. TLS verification is never skipped.
function providerCall(method: string, path: string, body?: unknown): Promise<{ status: number; body: string }> {
  const ca = readFileSync(providerRoot, "utf8");
  const payload = body === undefined ? undefined : JSON.stringify(body);
  return new Promise((resolve, reject) => {
    const call = httpsRequest(
      new URL(path, provider),
      { method, ca, rejectUnauthorized: true, timeout: 30_000, headers: { Authorization: `Bearer ${providerToken}`, ...(payload ? { "Content-Type": "application/json", "Content-Length": Buffer.byteLength(payload) } : {}) } },
      (response) => {
        let text = "";
        response.setEncoding("utf8");
        response.on("data", (chunk: string) => (text += chunk));
        response.on("end", () => resolve({ status: response.statusCode ?? 0, body: text }));
        response.on("error", reject);
      },
    );
    call.on("timeout", () => call.destroy(new Error(`Timed out calling the fixture provider ${path}`)));
    call.on("error", reject);
    call.end(payload);
  });
}

async function providerState(): Promise<ProviderState> {
  const reply = await providerCall("GET", "/fixture/state");
  expect(reply.status, "fixture provider state").toBe(200);
  const parsed = JSON.parse(reply.body) as { success: boolean; result: ProviderState };
  expect(parsed.success).toBe(true);
  return parsed.result;
}

// Through the fake edge, fake connector, verified local relay and the real Traefik ingress to the app.
const throughTunnel = (hostname: string) => providerCall("POST", "/fixture/forward", { Hostname: hostname, Path: "/" });

// Only non-secret metadata, so a failure message can be printed safely.
function sanitized(status: TunnelStatus) {
  return JSON.stringify({
    phase: status.phase,
    cleanup_requested: status.cleanup_requested,
    last_error: status.last_error,
    worker_error: status.worker_error,
    worker: status.worker && { phase: status.worker.phase, running: status.worker.running, connected: status.worker.connected, last_error: status.worker.last_error, withheld: status.worker.withheld },
    dns: status.dns,
  });
}

const secretKeys = /"(tunnel_secret|secret|token|credentials?|private_key\w*|csr|comment|proof)"\s*:/i;

async function tunnelStatus(page: Page, nodeId: string) {
  const response = await page.request.get(`/api/nodes/${nodeId}/cloudflare-tunnel`);
  const text = await response.text();
  expect(response.status(), text).toBe(200);
  expect(text).not.toMatch(secretKeys);
  expect(text).not.toContain("PRIVATE KEY");
  expect(text).not.toContain("CERTIFICATE REQUEST");
  expect(text).not.toContain(providerToken);
  return JSON.parse(text) as TunnelStatus;
}

// Polls the status until it matches, failing at once on a terminal blocked state with its sanitized reason.
async function waitForTunnel(page: Page, nodeId: string, done: (status: TunnelStatus) => boolean, what: string, timeout = 300_000) {
  const deadline = Date.now() + timeout;
  let last: TunnelStatus | undefined;
  while (Date.now() < deadline) {
    last = await tunnelStatus(page, nodeId);
    if (done(last)) return last;
    if (last.phase === "blocked" && last.last_error) throw new Error(`Cloudflare Tunnel blocked while waiting for ${what}: ${sanitized(last)}`);
    await page.waitForTimeout(3_000);
  }
  throw new Error(`Timed out waiting for ${what}: ${last ? sanitized(last) : "no status"}`);
}

test("Cloudflare Tunnel: publish a stored-certificate route through the owned fake provider and connector, then disable and clean up", async ({ page }, testInfo) => {
  test.setTimeout(600_000);
  if (!provider || !providerToken || !providerRoot) throw new Error("Run scripts/test-browser.py --cloudflare-tunnel to provide the owned fake Tunnel provider.");
  const authority = certificateAuthority();
  let nodeId = "";
  let headers: Record<string, string> = {};
  try {
    await page.goto("/projects");
    await expect(page).toHaveURL(/\/login(?:[?#]|$)/);
    await page.getByLabel("Password", { exact: true }).fill(password);
    await page.getByRole("button", { name: "Sign in", exact: true }).click();
    await expect(page).not.toHaveURL(/\/login(?:[?#]|$)/);
    headers = { Origin: new URL(page.url()).origin };

    const nodes = (await (await page.request.get("/api/nodes")).json()) as { id: string; name: string; capabilities: { cloudflare_publishing?: boolean; route_protocol?: number } | null }[];
    const node = nodes.find((item) => item.name === "ready-browser-worker");
    if (!node) throw new Error("The browser worker is missing");
    expect(node.capabilities?.cloudflare_publishing).toBe(true);
    expect(node.capabilities?.route_protocol ?? 0).toBeGreaterThanOrEqual(3);
    nodeId = node.id;
    expect(await providerState()).toEqual({ tunnels: 0, records: 0, creates: 0, deletes: 0, dns_creates: 0, dns_deletes: 0 });

    // An API-token connection to the fake provider, and its only zone imported.
    const connection = await page.request.post("/api/cloudflare/connections", { headers, data: { name: "Fixture tunnel provider", token: providerToken } });
    expect(connection.status(), await connection.text()).toBe(200);
    const { id: connectionId } = (await connection.json()) as { id: string };
    const imported = await page.request.post(`/api/cloudflare/connections/${connectionId}/zones`, { headers, data: { zones: [providerZone] } });
    expect([200, 201], await imported.text()).toContain(imported.status());

    const project = (await (await page.request.post("/api/projects", { headers, data: { name: "Cloudflare Tunnel project" } })).json()) as { id: string };
    const created = await page.request.post("/api/workloads", { headers, data: { name: "cloudflare-tunnel", node_id: node.id, kind: "service", compose, project_id: project.id } });
    expect(created.status(), await created.text()).toBe(201);
    const { id } = (await created.json()) as { id: string };
    const hostname = `tunnel-${Date.now()}.example.test`;

    const operation = async (operationId: string) => (await (await page.request.get(`/api/operations/${operationId}`)).json()) as Operation;
    const settled = async (operationId: string) => {
      await expect.poll(async () => (await operation(operationId)).state, { timeout: 180_000 }).toMatch(/^(succeeded|failed|cancelled|interrupted)$/);
      const result = await operation(operationId);
      if (result.state !== "succeeded") throw new Error(`${result.action} ${operationId} ended ${result.state}: ${result.result?.error ?? ""}`);
    };
    const activeOperations = async () => ((await (await page.request.get(`/api/operations?workload_id=${id}`)).json()) as Operation[]).filter((item) => item.state === "queued" || item.state === "running");
    const quiet = () => expect.poll(async () => (await activeOperations()).map((item) => item.action), { timeout: 120_000 }).toEqual([]);
    const send = async (call: () => Promise<APIResponse>, expected: number[]) => {
      for (let attempt = 1; ; attempt++) {
        await quiet();
        const response = await call();
        if (attempt < 3 && response.status() === 409 && (await response.text()).includes("active operation")) continue;
        expect(expected, await response.text()).toContain(response.status());
        return response;
      }
    };
    const applyRoutes = async (policy: Record<string, unknown>) => {
      const response = await send(() => page.request.put(`/api/workloads/${id}/routes`, { headers, data: { routes: [{ domain: hostname, service: "web", port: 8080, policy }] } }), [200, 202]);
      const update = (await response.json()) as { id?: string };
      if (update.id) await settled(update.id);
    };

    const deploy = (await (await send(() => page.request.post(`/api/workloads/${id}/operations`, { headers, data: { action: "deploy" } }), [202])).json()) as { id: string };
    await settled(deploy.id);
    await applyRoutes({ certificate_source: "default" });

    // The route moves to a stored certificate from the test CA, which the connector's relay verifies.
    const issued = authority.issue("tunnel", hostname, [hostname]);
    const uploaded = await page.request.post(`/api/workloads/${id}/certificates`, { headers, data: { name: "Tunnel origin", certificate_pem: issued.chain, private_key_pem: issued.key } });
    const uploadedText = await uploaded.text();
    expect(uploaded.status(), uploadedText).toBe(201);
    expect(uploadedText).not.toContain("PRIVATE KEY");
    const certificate = JSON.parse(uploadedText) as { id: string; fingerprint: string };
    await applyRoutes({ certificate_source: "uploaded", certificate_id: certificate.id });
    await expect.poll(async () => (await verified(hostname, authority.ca)).body, { timeout: 60_000 }).toBe("tunnel-marker\n");

    const before = await waitForTunnel(page, node.id, (status) => (status.available_hostnames ?? []).includes(hostname), "the route to become eligible", 120_000);
    const zone = (before.zones ?? []).find((item) => item.domain === "example.test");
    if (!zone) throw new Error(`The imported zone is missing from the Tunnel status: ${JSON.stringify(before.zones)}`);
    expect(zone.connection_kind).toBe("token");
    expect(before.configuration).toBeNull();
    expect(before.default_image).toMatch(/^(?:docker\.io\/)?cloudflare\/cloudflared:\d+\.\d+\.\d+(?:-[A-Za-z0-9.-]+)?@sha256:[a-f0-9]{64}$/);

    // Publishing from the node's Proxy page.
    await page.goto(`/nodes/${node.id}/proxy`);
    const section = page.getByRole("region", { name: "Cloudflare Tunnel" });
    await expect(section.getByText("How publishing works")).toBeVisible({ timeout: 30_000 });
    await expect(section.locator("input[type=password]")).toHaveCount(0);
    const zonePicker = section.getByRole("combobox", { name: "Zone", exact: true });
    if (!(await zonePicker.textContent())?.includes("example.test")) {
      await zonePicker.click();
      await page.getByRole("option", { name: "example.test", exact: true }).click();
    }
    await expect(zonePicker).toHaveText(/example\.test/);
    const image = section.getByLabel("Connector image", { exact: true });
    await expect(image).toHaveValue(before.default_image);
    const version = /:(\d+\.\d+\.\d+(?:-[A-Za-z0-9.-]+)?)@/.exec(before.default_image)?.[1] ?? "";
    await expect(section.getByText(`Cove's pinned official image (${version}).`, { exact: false })).toBeVisible();

    const host = section.getByRole("checkbox", { name: hostname, exact: true });
    await host.focus();
    await page.keyboard.press("Space");
    await expect(host).toBeChecked();
    await capture(page, testInfo, "cloudflare-tunnel-form");

    const publish = section.getByRole("button", { name: "Publish through Cloudflare", exact: true });
    const pending = page.waitForResponse((response) => new URL(response.url()).pathname === `/api/nodes/${node.id}/cloudflare-tunnel` && response.request().method() === "PUT");
    await publish.focus();
    await page.keyboard.press("Enter");
    const put = await pending;
    expect(put.status(), await put.text()).toBe(202);
    expect(put.request().postDataJSON()).toEqual({ zone_id: zone.id, hostnames: [hostname], image: before.default_image });
    // A queued request is never shown as published.
    await expect(section.getByText(/Requested\. Follow the status above/)).toBeVisible();

    const active = await waitForTunnel(page, node.id, (status) => status.phase === "active" && !status.worker_error && status.worker?.running === true && status.worker.connected === true, "an active, connected tunnel");
    expect(active.configuration).toEqual({ zone_id: zone.id, hostnames: [hostname], image: before.default_image });
    expect(Object.keys(active.worker?.withheld ?? {})).toEqual([]);
    await expect(section.getByText("Connected to the Cloudflare edge")).toBeVisible({ timeout: 60_000 });
    await expect(section.getByText("Published", { exact: true })).toBeVisible();
    // Once the connection is confirmed, the footer settles instead of keeping the queued note.
    await expect(section.getByText(/Requested\. Follow the status above/)).toHaveCount(0);
    await expect(section.getByRole("status").filter({ hasText: "No unsaved changes" })).toHaveText("No unsaved changes");

    await expect.poll(async () => (await throughTunnel(hostname)).status, { timeout: 120_000 }).toBe(200);
    expect((await throughTunnel(hostname)).body).toBe("tunnel-marker\n");
    const published = await providerState();
    expect(published).toMatchObject({ tunnels: 1, records: 1, creates: 1, dns_creates: 1 });
    await capture(page, testInfo, "cloudflare-tunnel-connected");

    // Disabling from the dashboard asks first, then removes exactly what Cove created.
    await section.getByRole("button", { name: "Disable tunnel", exact: true }).click();
    const dialog = page.getByRole("dialog", { name: `Disable the Cloudflare Tunnel on ${node.name}?` });
    await expect(dialog).toContainText("Apps and direct routes keep running");
    const removal = page.waitForResponse((response) => new URL(response.url()).pathname === `/api/nodes/${node.id}/cloudflare-tunnel` && response.request().method() === "DELETE");
    await dialog.getByRole("button", { name: "Disable and clean up", exact: true }).click();
    const deleted = await removal;
    expect(deleted.status(), await deleted.text()).toBe(202);
    expect(deleted.request().postData()).toBeNull();

    await waitForTunnel(page, node.id, (status) => status.configuration === null && status.phase === "disabled" && !status.cleanup_requested && (status.dns ?? []).length === 0, "cleanup to finish");
    const cleaned = await providerState();
    expect(cleaned).toMatchObject({ tunnels: 0, records: 0 });
    expect(cleaned.deletes).toBe(cleaned.creates);
    expect(cleaned.dns_deletes).toBe(cleaned.dns_creates);
    expect((await throughTunnel(hostname)).status).toBe(503);
    // The direct route keeps serving with its stored certificate.
    const direct = await verified(hostname, authority.ca);
    expect(direct.fingerprint).toBe(certificate.fingerprint);
    expect(direct.body).toBe("tunnel-marker\n");
    await expect(section.getByRole("button", { name: "Disable tunnel", exact: true })).toHaveCount(0, { timeout: 60_000 });
    await expect(section.getByRole("button", { name: "Publish through Cloudflare", exact: true })).toBeVisible();
    await expect(section.getByText(/Requested\. Follow the status above/)).toHaveCount(0);
    await expect(section.getByRole("status").filter({ hasText: "No unsaved changes" })).toHaveText("No unsaved changes");
    await capture(page, testInfo, "cloudflare-tunnel-disabled");
  } finally {
    authority.dispose();
    // Removes this node's tunnel before resource cleanup, so no fake provider ownership is left behind.
    if (nodeId) {
      const status = await page.request.get(`/api/nodes/${nodeId}/cloudflare-tunnel`).then((response) => (response.ok() ? (response.json() as Promise<TunnelStatus>) : null));
      if (status && status.configuration !== null && !status.cleanup_requested) {
        const response = await page.request.delete(`/api/nodes/${nodeId}/cloudflare-tunnel`, { headers });
        if (!response.ok()) console.error(`Cleanup could not disable the Cloudflare Tunnel: HTTP ${response.status()}`);
      }
    }
  }
});

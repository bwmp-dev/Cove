import { execFileSync, spawnSync } from "node:child_process";
import { createServer } from "node:net";
import { expect, test, type APIResponse } from "@playwright/test";
import { capture, cleanupBrowserResources } from "./route-tls";

const password = process.env.COVE_BROWSER_PASSWORD ?? "";

test.use({ actionTimeout: 30_000, navigationTimeout: 60_000 });
test.afterEach(cleanupBrowserResources);

interface Binding {
  address: string;
  port: number;
}
interface Settings {
  public?: { http: Binding[]; https: Binding[] };
  http3: boolean;
  listeners: { id: string; protocol: string; visibility: string; bindings: Binding[] }[] | null;
}
interface Status {
  settings: Settings;
  ports: Record<string, number> | null;
  change?: { id: string; phase: string; committed: boolean; finished_at?: string };
}
interface Plan {
  revision: string;
  digest: string;
  ports: Record<string, number> | null;
  requires_restart: boolean;
}
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

const compose = ["services:", "  web:", "    image: busybox:1.37", "    pull_policy: never", "    init: true", "    restart: unless-stopped", `    command: ${JSON.stringify(["sh", "-ec", "mkdir -p /www/deep; echo private-app-marker > /www/index.html; echo private-app-deep > /www/deep/index.html; exec httpd -f -p 8080 -h /www"])}`, "    healthcheck:", "      test: [CMD, wget, -q, -O, /dev/null, 'http://127.0.0.1:8080/']", "      interval: 1s", "      timeout: 2s", "      retries: 3", ""].join("\n");

// The fixture proxy shares this Docker host and publishes no public host ports. Its own `cove-ingress` bridge network
// has a gateway address assigned to a host bridge interface: a genuine RFC1918 host address Docker can publish on, unlike
// 127.0.0.1, which the worker rightly refuses as a LAN binding.
function privateHostAddress() {
  const gateway = execFileSync("docker", ["network", "inspect", "--format", "{{range .IPAM.Config}}{{.Gateway}} {{end}}", "cove-ingress"], { encoding: "utf8" }).split(/\s+/).find((value) => /^\d+\.\d+\.\d+\.\d+$/.test(value));
  if (!gateway) throw new Error("The fixture's cove-ingress network has no IPv4 gateway to bind a LAN listener on");
  const [a = 0, b = 0] = gateway.split(".").map(Number);
  const lan = a === 10 || (a === 172 && b >= 16 && b <= 31) || (a === 192 && b === 168);
  if (!lan) throw new Error(`The fixture's cove-ingress gateway ${gateway} is not an RFC1918 LAN address`);
  return gateway;
}

async function freePort() {
  const reservation = createServer();
  await new Promise<void>((resolve) => reservation.listen(0, "127.0.0.1", resolve));
  const address = reservation.address();
  if (!address || typeof address === "string") throw new Error("No fixture port");
  await new Promise<void>((resolve, reject) => reservation.close((error) => (error ? reject(error) : resolve())));
  return address.port;
}

// The first HTTP status the real proxy returns for a request to its own public entrypoint with the given Host header.
// BusyBox wget exits non-zero on a 4xx, so the status is read from its response headers rather than its exit code.
function publicStatus(hostname: string, url: string) {
  const result = spawnSync("docker", ["exec", "cove-ingress", "wget", "-S", "-O", "/dev/null", "-T", "5", "--no-check-certificate", `--header=Host: ${hostname}`, url], { encoding: "utf8" });
  const status = /HTTP\/[\d.]+ (\d{3})/.exec(`${result.stderr}\n${result.stdout}`)?.[1];
  return status ? Number(status) : `no response: ${result.stderr.trim()}`;
}

// The private listener answers on the host's LAN address. The request leaves the proxy container for its bridge gateway,
// so it arrives from a private bridge address.
const privateBody = (hostname: string, address: string, port: number) => execFileSync("docker", ["exec", "cove-ingress", "wget", "-qO-", "-T", "5", "--no-check-certificate", `--header=Host: ${hostname}`, `https://${address}:${port}/`], { encoding: "utf8" }).trim();

const portBindings = () => (JSON.parse(execFileSync("docker", ["inspect", "--format", "{{json .HostConfig.PortBindings}}", "cove-ingress"], { encoding: "utf8" })) ?? {}) as Record<string, { HostIp: string; HostPort: string }[] | null>;

test("private routes: a LAN listener route is saved in the dashboard, applied by the real worker and refused on the public entrypoints", async ({ page }, testInfo) => {
  test.setTimeout(600_000);
  await page.goto("/projects");
  await expect(page).toHaveURL(/\/login(?:[?#]|$)/);
  await page.getByLabel("Password", { exact: true }).fill(password);
  await page.getByRole("button", { name: "Sign in", exact: true }).click();
  await expect(page).not.toHaveURL(/\/login(?:[?#]|$)/);
  const headers = { Origin: new URL(page.url()).origin };

  const nodes = (await (await page.request.get("/api/nodes")).json()) as { id: string; name: string; capabilities: { ingress_exposures?: boolean; route_protocol?: number } | null }[];
  const node = nodes.find((item) => item.name === "ready-browser-worker");
  if (!node) throw new Error("The browser worker is missing");
  expect(node.capabilities?.ingress_exposures).toBe(true);
  expect(node.capabilities?.route_protocol ?? 0).toBeGreaterThanOrEqual(5);
  const statusURL = `/api/nodes/${node.id}/ingress-listeners`;
  const readStatus = async () => {
    const response = await page.request.get(statusURL);
    expect(response.status(), await response.text()).toBe(200);
    return (await response.json()) as Status;
  };
  const settledChange = () => expect.poll(async () => { const change = (await readStatus()).change; return change ? `${change.phase}:${change.committed}` : "none"; }, { timeout: 300_000, intervals: [2_000] }).toBe("completed:true");
  const applyListeners = async (settings: Settings) => {
    const planned = await page.request.post(`${statusURL}/plan`, { headers, data: settings });
    expect(planned.status(), await planned.text()).toBe(200);
    const plan = (await planned.json()) as Plan;
    if (!plan.requires_restart) return plan;
    const applied = await page.request.put(statusURL, { headers, data: { settings, revision: plan.revision, digest: plan.digest, accept_interruption: true }, timeout: 300_000 });
    expect(applied.status(), await applied.text()).toBe(200);
    await settledChange();
    return plan;
  };

  const project = (await (await page.request.post("/api/projects", { headers, data: { name: "Private route project" } })).json()) as { id: string };
  const created = await page.request.post("/api/workloads", { headers, data: { name: "private-route-app", node_id: node.id, kind: "service", compose, project_id: project.id } });
  expect(created.status(), await created.text()).toBe(201);
  const { id } = (await created.json()) as { id: string };
  const operation = async (operationId: string) => (await (await page.request.get(`/api/operations/${operationId}`)).json()) as Operation;
  const settled = async (operationId: string) => {
    await expect.poll(async () => (await operation(operationId)).state, { timeout: 180_000 }).toMatch(/^(succeeded|failed|cancelled|interrupted)$/);
    const result = await operation(operationId);
    if (result.state !== "succeeded") throw new Error(`${result.action} ${operationId} ended ${result.state}: ${result.result?.error ?? ""}`);
  };
  const quiet = () => expect.poll(async () => ((await (await page.request.get(`/api/operations?workload_id=${id}`)).json()) as Operation[]).filter((item) => item.state === "queued" || item.state === "running").length, { timeout: 120_000 }).toBe(0);
  const send = async (request: () => Promise<APIResponse>, expected: number[]) => {
    await quiet();
    const response = await request();
    expect(expected, await response.text()).toContain(response.status());
    return response;
  };
  await settled(((await (await send(() => page.request.post(`/api/workloads/${id}/operations`, { headers, data: { action: "deploy" } }), [202])).json()) as { id: string }).id);

  const original = await readStatus();
  if (original.change) expect(original.change.finished_at).toBeTruthy();
  const address = privateHostAddress();
  const port = await freePort();
  const listenerID = "e2e-lan";
  const hostname = `private-${Date.now()}.cove.test`;
  const listener = { id: listenerID, protocol: "https", visibility: "lan", bindings: [{ address, port }] };
  const restoreSettings: Settings = { ...original.settings, listeners: original.settings.listeners ?? [] };

  let failure: Error | undefined;
  try {
    // An instance administrator's LAN HTTPS listener on the host's private address, through the node plan/apply API.
    const plan = await applyListeners({ ...original.settings, listeners: [...(original.settings.listeners ?? []), listener] });
    const internalPort = plan.ports?.[listenerID];
    expect(internalPort).toBeGreaterThan(0);
    expect(portBindings()[`${internalPort}/tcp`]).toEqual([{ HostIp: address, HostPort: String(port) }]);
    const listed = await page.request.get(`/api/workloads/${id}/route-listeners`);
    expect(listed.status(), await listed.text()).toBe(200);
    expect(((await listed.json()) as { listeners: unknown[] }).listeners).toContainEqual(listener);

    // The route is added, made private and given the proxy default certificate in the dashboard.
    await page.goto(`/resources/${id}/domains`);
    await page.getByRole("button", { name: "Add route", exact: true }).click();
    const row = page.getByRole("list", { name: "Routes" }).getByRole("listitem").filter({ has: page.getByLabel("Hostname", { exact: true }) }).first();
    await row.getByLabel("Hostname", { exact: true }).fill(hostname);
    await row.getByLabel("Compose service", { exact: true }).fill("web");
    await row.getByLabel("Container port", { exact: true }).fill("8080");
    await row.getByRole("button", { name: /^Advanced routing/ }).click();
    const visibility = row.getByRole("combobox", { name: "Visibility", exact: true });
    await expect(visibility).toBeEnabled();
    await visibility.click();
    await page.getByRole("option", { name: "LAN only", exact: true }).click();
    const listenerSelect = row.getByRole("combobox", { name: "Listener", exact: true });
    await expect(listenerSelect).toBeEnabled({ timeout: 30_000 });
    await listenerSelect.click();
    await page.getByRole("option", { name: `${listenerID} · ${address}:${port}`, exact: true }).click();
    await expect(row.getByText(`${address}:${port}`, { exact: true })).toBeVisible();

    // Let's Encrypt is refused for a private route next to the certificate field before anything is sent.
    const save = page.getByRole("button", { name: "Save and apply", exact: true });
    await save.click();
    await expect(row.getByText(/Private routes cannot use Let's Encrypt/)).toBeVisible();
    await row.getByRole("combobox", { name: "Certificate", exact: true }).click();
    await page.getByRole("option", { name: "Proxy default certificate", exact: true }).click();
    await expect(row.getByText(/Private routes cannot use Let's Encrypt/)).toHaveCount(0);

    // Keyboard order runs from visibility to the listener choice.
    await visibility.focus();
    await page.keyboard.press("Tab");
    await expect(listenerSelect).toBeFocused();
    await capture(page, testInfo, "route-private-editor");

    await quiet();
    await expect(save).toBeEnabled({ timeout: 30_000 });
    const pending = page.waitForResponse((response) => new URL(response.url()).pathname === `/api/workloads/${id}/routes` && response.request().method() === "PUT");
    await save.click();
    const response = await pending;
    expect(response.status(), await response.text()).toBe(202);
    const sent = response.request().postDataJSON() as { routes: SavedRoute[] };
    expect(sent).toEqual({ routes: [{ domain: hostname, service: "web", port: 8080, policy: { visibility: "lan", listener: listenerID, certificate_source: "default" } }] });
    await settled(((await response.json()) as { id: string }).id);
    await expect(page.getByText("Routes applied", { exact: true })).toBeVisible({ timeout: 30_000 });
    expect(((await (await page.request.get(`/api/workloads/${id}`)).json()) as { routes?: SavedRoute[] }).routes).toEqual(sent.routes);
    await expect(row.getByText(`LAN only via ${listenerID} (${address}:${port}). Public HTTP and HTTPS requests for this hostname are refused.`, { exact: true })).toBeVisible();

    // Every path is refused on the public HTTPS entrypoint, including paths no route names; port 80 refuses or redirects.
    for (const path of ["/", "/deep/", "/unrouted/path?query=1"]) {
      await expect.poll(() => publicStatus(hostname, `https://127.0.0.1${path}`), { timeout: 60_000 }).toBe(418);
    }
    expect([301, 302, 307, 308, 418]).toContain(publicStatus(hostname, "http://127.0.0.1/"));
    // The private listener serves the application.
    await expect.poll(() => privateBody(hostname, address, port), { timeout: 60_000 }).toBe("private-app-marker");

    await page.reload();
    await row.getByRole("button", { name: /^Advanced routing/ }).click();
    await expect(row.getByRole("combobox", { name: "Listener", exact: true })).toContainText(`${listenerID} · ${address}:${port}`, { timeout: 30_000 });
    await expect(page.getByText("Unsaved changes", { exact: true })).toHaveCount(0);
    await capture(page, testInfo, "route-private-applied");
  } catch (error) {
    failure = error instanceof Error ? error : new Error(String(error));
    try {
      await page.screenshot({ path: testInfo.outputPath("route-private-failure.png"), fullPage: true, timeout: 10_000 });
    } catch (screenshotError) {
      console.error("Could not capture the failure screenshot:", screenshotError);
    }
  }

  // Saved routes reserve the listener, so the route goes first; then the fixture's listener settings are restored.
  let cleanupFailure: Error | undefined;
  try {
    test.setTimeout(testInfo.timeout + 420_000);
    const routes = await send(() => page.request.put(`/api/workloads/${id}/routes`, { headers, data: { routes: [] } }), [200, 202]);
    const queued = (await routes.json()) as { id?: string };
    if (queued.id) await settled(queued.id);
    if ((await readStatus()).settings.listeners?.some((item) => item.id === listenerID)) await applyListeners(restoreSettings);
    expect(Object.values(portBindings()).flat().some((binding) => binding?.HostIp === address && binding.HostPort === String(port))).toBe(false);
  } catch (error) {
    cleanupFailure = error instanceof Error ? error : new Error(String(error));
  }
  if (failure !== undefined) {
    if (cleanupFailure !== undefined) {
      console.error("Restoring the fixture's routes and listener settings also failed:", cleanupFailure);
      testInfo.annotations.push({ type: "cleanup failure", description: cleanupFailure.message });
    }
    throw failure;
  }
  if (cleanupFailure !== undefined) throw cleanupFailure;
});

import { spawnSync } from "node:child_process";
import { createServer } from "node:net";
import { expect, test, type APIResponse, type Locator, type Page } from "@playwright/test";
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

// One cached BusyBox container: a static HTTP page for the HTTP route, and TCP and UDP services that answer each
// connection or datagram with a fixed marker. BusyBox nc serves one peer per run, so each runs in a restart loop.
const compose = [
  "services:",
  "  app:",
  "    image: busybox:1.37",
  "    pull_policy: never",
  "    init: true",
  "    restart: unless-stopped",
  `    command: ${JSON.stringify(["sh", "-ec", "mkdir -p /www; echo transport-http-marker > /www/index.html; httpd -p 8080 -h /www; (while true; do echo raw-tcp-marker | nc -l -p 7000 > /dev/null || sleep 1; done) & (while true; do echo raw-udp-marker | nc -u -l -w 2 -p 7001 > /dev/null || sleep 1; done) & wait"])}`,
  "",
].join("\n");

async function freePort() {
  const reservation = createServer();
  await new Promise<void>((resolve) => reservation.listen(0, "127.0.0.1", resolve));
  const address = reservation.address();
  if (!address || typeof address === "string") throw new Error("No fixture port");
  await new Promise<void>((resolve, reject) => reservation.close((error) => (error ? reject(error) : resolve())));
  return address.port;
}

// A client on the proxy's own bridge network connects to the listener's entrypoint inside the real proxy container, so
// the probe does not depend on Docker Desktop publishing host ports to this machine. It uses the cached image only.
function probe(protocol: "tcp" | "udp", port: number) {
  const command = protocol === "tcp" ? `echo probe | nc -w 3 cove-ingress ${port}` : `echo probe | nc -u -w 3 cove-ingress ${port}`;
  const result = spawnSync("docker", ["run", "--rm", "--pull", "never", "--network", "cove-ingress", "busybox:1.37", "sh", "-c", command], { encoding: "utf8", timeout: 30_000 });
  return result.stdout.trim() || `no response (exit ${result.status ?? "none"}): ${result.stderr.trim()}`;
}

async function choose(page: Page, combobox: Locator, name: string) {
  await expect(combobox).toBeEnabled({ timeout: 30_000 });
  await combobox.click();
  await page.getByRole("option", { name, exact: true }).click();
}

test("TCP and UDP routes and HTTP upstream timeouts are edited in the dashboard and served by the real worker", async ({ page }, testInfo) => {
  test.setTimeout(900_000);
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
  expect(node.capabilities?.route_protocol ?? 0).toBeGreaterThanOrEqual(6);
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

  const project = (await (await page.request.post("/api/projects", { headers, data: { name: "Transport route project" } })).json()) as { id: string };
  const created = await page.request.post("/api/workloads", { headers, data: { name: "transport-route-app", node_id: node.id, kind: "service", compose, project_id: project.id } });
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
  const savedRoutes = async () => ((await (await page.request.get(`/api/workloads/${id}`)).json()) as { routes?: unknown[] | null }).routes ?? [];
  await settled(((await (await send(() => page.request.post(`/api/workloads/${id}/operations`, { headers, data: { action: "deploy" } }), [202])).json()) as { id: string }).id);

  const original = await readStatus();
  if (original.change) expect(original.change.finished_at).toBeTruthy();
  const tcpID = "e2e-tcp";
  const udpID = "e2e-udp";
  const tcpListener = { id: tcpID, protocol: "tcp", visibility: "public", bindings: [{ address: "127.0.0.1", port: await freePort() }] };
  const udpListener = { id: udpID, protocol: "udp", visibility: "public", bindings: [{ address: "127.0.0.1", port: await freePort() }] };
  const restoreSettings: Settings = { ...original.settings, listeners: original.settings.listeners ?? [] };
  const hostname = `transport-${Date.now()}.cove.test`;
  const httpRoute = { domain: hostname, service: "app", port: 8080, policy: { certificate_source: "default" } };
  const timeouts = { dial_seconds: 5, response_header_seconds: 30, idle_connection_seconds: 120 };
  const tcpRoute = { protocol: "tcp", domain: "", service: "app", port: 7000, policy: { listener: tcpID, visibility: "public" } };
  const udpRoute = { protocol: "udp", domain: "", service: "app", port: 7001, policy: { listener: udpID, visibility: "public" } };

  const save = page.getByRole("button", { name: "Save and apply", exact: true });
  const raw = page.getByRole("list", { name: "TCP and UDP routes" });
  const rawRows = raw.getByRole("listitem").filter({ has: page.getByRole("combobox", { name: "Protocol", exact: true }) });
  const saveRoutes = async () => {
    await quiet();
    await expect(save).toBeEnabled({ timeout: 30_000 });
    const pending = page.waitForResponse((response) => new URL(response.url()).pathname === `/api/workloads/${id}/routes` && response.request().method() === "PUT");
    await save.click();
    const response = await pending;
    expect(response.status(), await response.text()).toBe(202);
    await settled(((await response.json()) as { id: string }).id);
    await expect(page.getByText("Routes applied", { exact: true })).toBeVisible({ timeout: 30_000 });
    return response.request().postDataJSON() as { routes: unknown[] };
  };

  let failure: Error | undefined;
  try {
    // An instance administrator's TCP and UDP listeners, through the node plan/apply API.
    const plan = await applyListeners({ ...original.settings, listeners: [...(original.settings.listeners ?? []), tcpListener, udpListener] });
    const tcpPort = plan.ports?.[tcpID] ?? 0;
    const udpPort = plan.ports?.[udpID] ?? 0;
    expect(tcpPort).toBeGreaterThan(0);
    expect(udpPort).toBeGreaterThan(0);

    await page.goto(`/resources/${id}/domains`);
    // An HTTP route with upstream timeouts, edited under Advanced routing.
    await page.getByRole("button", { name: "Add route", exact: true }).click();
    const httpRow = page.getByRole("list", { name: "Routes" }).getByRole("listitem").filter({ has: page.getByLabel("Hostname", { exact: true }) }).first();
    await httpRow.getByLabel("Hostname", { exact: true }).fill(hostname);
    await httpRow.getByLabel("Compose service", { exact: true }).fill("app");
    await httpRow.getByLabel("Container port", { exact: true }).fill("8080");
    await httpRow.getByRole("button", { name: /^Advanced routing/ }).click();
    await choose(page, httpRow.getByRole("combobox", { name: "Certificate", exact: true }), "Proxy default certificate");
    await httpRow.getByLabel(/^Upstream connection timeout/).fill("5");
    await httpRow.getByLabel(/^Response header timeout/).fill("30");
    await httpRow.getByLabel(/^Idle pooled connection timeout/).fill("120");
    await expect(httpRow.getByText(/no Upgrade headers need to be set/)).toBeVisible();

    // A TCP and a UDP route, each on its own listener. UDP shows no allowlist.
    for (const [index, protocol, port, visibility, listener] of [[0, "TCP", "7000", "Public", `${tcpID} · 127.0.0.1:${tcpListener.bindings[0]?.port}`], [1, "UDP", "7001", "Public", `${udpID} · 127.0.0.1:${udpListener.bindings[0]?.port}`]] as const) {
      await page.getByRole("button", { name: "Add TCP or UDP route", exact: true }).click();
      const row = rawRows.nth(index);
      if (protocol === "UDP") await choose(page, row.getByRole("combobox", { name: "Protocol", exact: true }), "UDP");
      await row.getByLabel("Compose service", { exact: true }).fill("app");
      await row.getByLabel("Container port", { exact: true }).fill(port);
      await choose(page, row.getByRole("combobox", { name: "Visibility", exact: true }), visibility);
      await choose(page, row.getByRole("combobox", { name: "Listener", exact: true }), listener);
    }
    await expect(rawRows.nth(1).getByLabel(/^Allowed source IP ranges/)).toHaveCount(0);
    await expect(rawRows.nth(0).getByText(`127.0.0.1:${tcpListener.bindings[0]?.port}`, { exact: true })).toBeVisible();

    // Keyboard order runs through a raw row's fields.
    await rawRows.nth(0).getByRole("combobox", { name: "Visibility", exact: true }).focus();
    await page.keyboard.press("Tab");
    await expect(rawRows.nth(0).getByRole("combobox", { name: "Listener", exact: true })).toBeFocused();
    await capture(page, testInfo, "route-transports-editor");

    const added = await saveRoutes();
    expect(added).toEqual({ routes: [{ ...httpRoute, policy: { ...httpRoute.policy, timeouts } }, tcpRoute, udpRoute] });
    expect(await savedRoutes()).toEqual(added.routes);
    await expect.poll(() => probe("tcp", tcpPort), { timeout: 60_000 }).toBe("raw-tcp-marker");
    await expect.poll(() => probe("udp", udpPort), { timeout: 60_000 }).toBe("raw-udp-marker");

    // Saved values come back after a reload, with no unsaved changes and no hostname link for raw routes.
    await page.reload();
    await expect(rawRows).toHaveCount(2);
    await expect(rawRows.nth(0).getByRole("combobox", { name: "Listener", exact: true })).toContainText(`${tcpID} · 127.0.0.1:`, { timeout: 30_000 });
    await expect(page.getByText("Unsaved changes", { exact: true })).toHaveCount(0);
    await expect(raw.getByRole("link")).toHaveCount(0);
    await httpRow.getByRole("button", { name: /^Advanced routing/ }).click();
    await expect(httpRow.getByLabel(/^Idle pooled connection timeout/)).toHaveValue("120");
    await capture(page, testInfo, "route-transports-saved");

    // Routes survive a restart of the resource.
    await settled(((await (await send(() => page.request.post(`/api/workloads/${id}/operations`, { headers, data: { action: "restart" } }), [202])).json()) as { id: string }).id);
    await expect.poll(() => probe("tcp", tcpPort), { timeout: 90_000 }).toBe("raw-tcp-marker");
    await expect.poll(() => probe("udp", udpPort), { timeout: 90_000 }).toBe("raw-udp-marker");

    // A TCP source allowlist that excludes the probe's bridge address refuses it.
    await page.reload();
    await rawRows.nth(0).getByLabel(/^Allowed source IP ranges/).fill("203.0.113.0/24");
    const restricted = await saveRoutes();
    expect(restricted).toEqual({ routes: [{ ...httpRoute, policy: { ...httpRoute.policy, timeouts } }, { ...tcpRoute, policy: { ...tcpRoute.policy, ip_allow_list: ["203.0.113.0/24"] } }, udpRoute] });
    await expect.poll(() => probe("tcp", tcpPort), { timeout: 60_000 }).not.toBe("raw-tcp-marker");
    await expect.poll(() => probe("udp", udpPort), { timeout: 60_000 }).toBe("raw-udp-marker");

    // Removing the TCP route and clearing the timeouts sends explicit null timeouts and keeps the UDP route.
    await page.reload();
    await page.getByRole("button", { name: `Remove TCP route ${tcpID}`, exact: true }).click();
    await httpRow.getByRole("button", { name: /^Advanced routing/ }).click();
    for (const label of [/^Upstream connection timeout/, /^Response header timeout/, /^Idle pooled connection timeout/]) await httpRow.getByLabel(label).fill("");
    const removed = await saveRoutes();
    expect(removed).toEqual({ routes: [{ ...httpRoute, policy: { ...httpRoute.policy, timeouts: null } }, udpRoute] });
    expect(await savedRoutes()).toEqual([httpRoute, udpRoute]);
    await expect.poll(() => probe("tcp", tcpPort), { timeout: 60_000 }).not.toBe("raw-tcp-marker");
    await expect.poll(() => probe("udp", udpPort), { timeout: 60_000 }).toBe("raw-udp-marker");
    await capture(page, testInfo, "route-transports-removed");
  } catch (error) {
    failure = error instanceof Error ? error : new Error(String(error));
    try {
      await page.screenshot({ path: testInfo.outputPath("route-transports-failure.png"), fullPage: true, timeout: 10_000 });
    } catch (screenshotError) {
      console.error("Could not capture the failure screenshot:", screenshotError);
    }
  }

  // Saved routes reserve the listeners, so the routes go first; then only this test's listeners are removed.
  let cleanupFailure: Error | undefined;
  try {
    test.setTimeout(testInfo.timeout + 420_000);
    const routes = await send(() => page.request.put(`/api/workloads/${id}/routes`, { headers, data: { routes: [] } }), [200, 202]);
    const queued = (await routes.json()) as { id?: string };
    if (queued.id) await settled(queued.id);
    const current = await readStatus();
    if (current.settings.listeners?.some((item) => item.id === tcpID || item.id === udpID)) await applyListeners(restoreSettings);
    expect((await readStatus()).settings.listeners?.some((item) => item.id === tcpID || item.id === udpID) ?? false).toBe(false);
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

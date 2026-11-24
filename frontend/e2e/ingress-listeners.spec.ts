import { execFileSync } from "node:child_process";
import { connect, createServer } from "node:net";
import { expect, test, type APIResponse, type Page, type TestInfo } from "@playwright/test";

const password = process.env.COVE_BROWSER_PASSWORD ?? "";

// Dashboard actions fail at the step that stalls instead of waiting out the whole test, which would leave the
// cleanup below no time and report its request rather than the real failure.
test.use({ actionTimeout: 30_000, navigationTimeout: 60_000 });

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
  proxy_present: boolean;
  running: boolean;
  public_bindings?: { http: Binding[]; https: Binding[] };
}
interface Plan {
  revision: string;
  digest: string;
  ports: Record<string, number> | null;
  requires_restart: boolean;
  affected_workloads: string[] | null;
}
interface Operation {
  id: string;
  action: string;
  state: string;
  result: { error?: string } | null;
}

const proxy = () => JSON.parse(execFileSync("docker", ["inspect", "--format", "{{json .}}", "cove-ingress"], { encoding: "utf8" })) as { Id: string; State: { Running: boolean }; HostConfig: { PortBindings: Record<string, { HostIp: string; HostPort: string }[] | null> | null } };
const portBindings = () => proxy().HostConfig.PortBindings ?? {};
const entrypoints = () => execFileSync("docker", ["exec", "cove-ingress", "wget", "-qO-", "http://127.0.0.1:8080/api/entrypoints"], { encoding: "utf8" });
const routedBody = (hostname: string) => execFileSync("docker", ["exec", "cove-ingress", "wget", "-qO-", "-T", "5", "--no-check-certificate", `--header=Host: ${hostname}`, "https://127.0.0.1/"], { encoding: "utf8" }).trim();
const appContainers = (workload: string) => {
  const ids = execFileSync("docker", ["ps", "-aq", "--no-trunc", "--filter", `label=com.docker.compose.project=cove-${workload}`], { encoding: "utf8" }).split(/\s+/).filter(Boolean);
  expect(ids.length).toBeGreaterThan(0);
  return execFileSync("docker", ["inspect", "--format", "{{.Id}}|{{.State.StartedAt}}|{{.State.Status}}", ...ids], { encoding: "utf8" }).trim().split(/\r?\n/).sort();
};

async function freeLocalPort() {
  const reservation = createServer();
  await new Promise<void>((resolve) => reservation.listen(0, "127.0.0.1", resolve));
  const address = reservation.address();
  if (!address || typeof address === "string") throw new Error("No local fixture port");
  await new Promise<void>((resolve, reject) => reservation.close((error) => (error ? reject(error) : resolve())));
  return address.port;
}

function tcpAccepts(port: number) {
  return new Promise<boolean>((resolve) => {
    const socket = connect({ host: "127.0.0.1", port, timeout: 3_000 });
    socket.once("connect", () => {
      socket.destroy();
      resolve(true);
    });
    socket.once("timeout", () => {
      socket.destroy();
      resolve(false);
    });
    socket.once("error", () => resolve(false));
  });
}

const compose = ["services:", "  web:", "    image: busybox:1.37", "    pull_policy: never", "    init: true", "    restart: unless-stopped", `    command: ${JSON.stringify(["sh", "-ec", "mkdir -p /www; echo listener-app-marker > /www/index.html; exec httpd -f -p 8080 -h /www"])}`, "    healthcheck:", "      test: [CMD, wget, -q, -O, /dev/null, 'http://127.0.0.1:8080/']", "      interval: 1s", "      timeout: 2s", "      retries: 3", ""].join("\n");

test("managed listeners: a planned listener is applied through the real worker, published by Docker and persisted without replacing applications", async ({ page }, testInfo) => {
  test.setTimeout(480_000);
  await page.goto("/nodes");
  await expect(page).toHaveURL(/\/login(?:[?#]|$)/);
  await page.getByLabel("Password", { exact: true }).fill(password);
  await page.getByRole("button", { name: "Sign in", exact: true }).click();
  await expect(page).not.toHaveURL(/\/login(?:[?#]|$)/);
  const headers = { Origin: new URL(page.url()).origin };

  const nodes = (await (await page.request.get("/api/nodes")).json()) as { id: string; name: string; capabilities: { ingress_exposures?: boolean } | null }[];
  const node = nodes.find((item) => item.name === "ready-browser-worker");
  if (!node) throw new Error("The browser worker is missing");
  expect(node.capabilities?.ingress_exposures).toBe(true);
  const statusURL = `/api/nodes/${node.id}/ingress-listeners`;
  const readStatus = async () => {
    const response = await page.request.get(statusURL);
    expect(response.status(), await response.text()).toBe(200);
    return (await response.json()) as Status;
  };

  // A routed application, so the plan reports an affected workload and replacement of its containers can be checked.
  const project = (await (await page.request.post("/api/projects", { headers, data: { name: "Listener project" } })).json()) as { id: string };
  const created = await page.request.post("/api/workloads", { headers, data: { name: "listener-app", node_id: node.id, kind: "service", compose, project_id: project.id } });
  expect(created.status(), await created.text()).toBe(201);
  const { id } = (await created.json()) as { id: string };
  const hostname = `listener-${Date.now()}.cove.test`;
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
  const deploy = (await (await send(() => page.request.post(`/api/workloads/${id}/operations`, { headers, data: { action: "deploy" } }), [202])).json()) as { id: string };
  await settled(deploy.id);
  // The isolated ingress cannot reach a certificate authority, so the route uses the proxy's default certificate. The web
  // service answers 200 on /, so it doubles as an allow-only forward-auth provider: every routed request passes through the
  // owned route helper, which must keep trusting the proxy after the listener change recreates it.
  const policy = { certificate_source: "default", forward_auth: { service: "web", port: 8080, path: "/" } };
  const routes = (await (await send(() => page.request.put(`/api/workloads/${id}/routes`, { headers, data: { routes: [{ domain: hostname, service: "web", port: 8080, policy }] } }), [200, 202])).json()) as { id?: string };
  if (routes.id) await settled(routes.id);
  const savedPolicy = async () => ((await (await page.request.get(`/api/workloads/${id}`)).json()) as { routes?: { domain: string; policy?: { forward_auth?: unknown } }[] | null }).routes?.find((route) => route.domain === hostname)?.policy?.forward_auth;
  // Reads omit the forward-auth path and URL, so only the provider metadata is compared; the routed requests passing through
  // the auth helper show the stored path is still applied.
  const savedAuth = { service: policy.forward_auth.service, port: policy.forward_auth.port };
  const expectSavedPolicy = async () => {
    const saved = await savedPolicy();
    expect(saved).toEqual(savedAuth);
    expect(saved).not.toHaveProperty("path");
    expect(saved).not.toHaveProperty("url");
  };
  await expectSavedPolicy();
  await expect.poll(() => routedBody(hostname), { timeout: 60_000 }).toBe("listener-app-marker");
  await quiet();

  const original = await readStatus();
  expect(original.proxy_present).toBe(true);
  if (original.change) expect(original.change.finished_at).toBeTruthy();
  const originalProxy = proxy();
  const originalPublic = { http: portBindings()["80/tcp"] ?? null, https: portBindings()["443/tcp"] ?? null };
  const appsBefore = appContainers(id);
  const listenerPort = await freeLocalPort();
  const listenerID = "e2e-local";
  const listener = { id: listenerID, protocol: "tcp", visibility: "public", bindings: [{ address: "127.0.0.1", port: listenerPort }] };
  const restoreSettings: Settings = { ...original.settings, listeners: original.settings.listeners ?? [] };
  const expectedSettings = { ...(original.settings.public ? { public: original.settings.public } : {}), http3: original.settings.http3, listeners: [...(original.settings.listeners ?? []), listener] };

  let failure: Error | undefined;
  try {
    // Plan in the dashboard. The original public publications are kept by leaving them on "Keep current publications".
    await test.step("open the node proxy page", async () => {
      await page.goto(`/nodes/${node.id}/proxy`);
      await expect(page.getByRole("heading", { name: "Listeners and published ports", exact: true })).toBeVisible();
      await expect(page.getByText("Applying listener changes restarts the shared proxy", { exact: true })).toBeVisible();
      await expect(page.getByRole("radio", { name: /Keep current publications/ })).toBeChecked();
    });
    await test.step("add the listener to the draft", async () => {
      const names = page.getByLabel("Name", { exact: true });
      await expect(names).toHaveCount(original.settings.listeners?.length ?? 0);
      await page.getByRole("button", { name: "Add listener", exact: true }).click();
      await expect(names, "the new listener row stays in the draft").toHaveCount((original.settings.listeners?.length ?? 0) + 1);
      await names.last().fill(listenerID);
      await page.getByRole("combobox", { name: "Visibility" }).last().click();
      await page.getByRole("option", { name: "Public", exact: true }).click();
      await page.getByLabel(`Bindings for ${listenerID}: address 1`).fill("127.0.0.1");
      await page.getByLabel(`Bindings for ${listenerID}: port 1`).fill(String(listenerPort));
      // The proxy configuration sections load independently; the draft must survive them settling.
      await expect(page.getByRole("status", { name: "Loading proxy configuration" })).toHaveCount(0);
      await expect(names.last()).toHaveValue(listenerID);
      await expect(page.getByText("Unsaved changes. Review a plan to apply them.", { exact: true })).toBeVisible();
    });
    await capture(page, testInfo, "listeners-form");

    const planned = page.waitForResponse((response) => new URL(response.url()).pathname === `${statusURL}/plan` && response.request().method() === "POST", { timeout: 120_000 });
    await page.getByRole("button", { name: "Review changes", exact: true }).click();
    const planResponse = await planned;
    expect(planResponse.status(), await planResponse.text()).toBe(200);
    expect(planResponse.request().postDataJSON()).toEqual(expectedSettings);
    const plan = (await planResponse.json()) as Plan;
    expect(plan.requires_restart).toBe(true);
    expect(plan.affected_workloads ?? []).toContain(id);
    const internalPort = plan.ports?.[listenerID];
    expect(internalPort).toBeGreaterThan(0);
    await expect(page.getByRole("heading", { name: "Review listener change", exact: true })).toBeFocused();
    await expect(page.getByText(`cove-listener-${listenerID}`, { exact: true }).last()).toBeVisible();
    await capture(page, testInfo, "listeners-plan");
    // Planning is read-only.
    expect(proxy().Id).toBe(originalProxy.Id);

    const applied = page.waitForResponse((response) => new URL(response.url()).pathname === statusURL && response.request().method() === "PUT", { timeout: 300_000 });
    await page.getByRole("button", { name: "Apply and restart proxy", exact: true }).click();
    await page.getByRole("dialog").getByRole("button", { name: "Interrupt connections and apply", exact: true }).click();
    const applyResponse = await applied;
    expect(applyResponse.status(), await applyResponse.text()).toBe(200);
    expect(applyResponse.request().postDataJSON()).toEqual({ settings: expectedSettings, revision: plan.revision, digest: plan.digest, accept_interruption: true });

    // The phase and commit flag are authoritative, not the HTTP status.
    await expect.poll(async () => { const change = (await readStatus()).change; return change ? `${change.phase}:${change.committed}` : "none"; }, { timeout: 300_000, intervals: [2_000] }).toBe("completed:true");
    await expect(page.getByText("Listener change: Applied", { exact: true })).toBeVisible({ timeout: 30_000 });
    await capture(page, testInfo, "listeners-applied");

    const recreated = proxy();
    expect(recreated.Id).not.toBe(originalProxy.Id);
    expect(recreated.State.Running).toBe(originalProxy.State.Running);
    expect(portBindings()[`${internalPort}/tcp`]).toEqual([{ HostIp: "127.0.0.1", HostPort: String(listenerPort) }]);
    expect({ http: portBindings()["80/tcp"] ?? null, https: portBindings()["443/tcp"] ?? null }).toEqual(originalPublic);
    expect(entrypoints()).toContain(`cove-listener-${listenerID}`);
    await expect.poll(() => tcpAccepts(listenerPort), { timeout: 30_000 }).toBe(true);
    expect(appContainers(id)).toEqual(appsBefore);
    await expect.poll(() => routedBody(hostname), { timeout: 60_000 }).toBe("listener-app-marker");

    // The saved settings survive a reload and a worker restart, and the publication stays in place.
    await page.reload();
    await expect(page.getByLabel("Name", { exact: true }).last()).toHaveValue(listenerID, { timeout: 30_000 });
    await expect(page.getByText(`Internal port ${internalPort}, assigned by Cove.`, { exact: true })).toBeVisible();
    const worker = process.env.COVE_BROWSER_WORKER;
    if (!worker || !/^cove-browser-[a-f0-9]+-worker$/.test(worker)) throw new Error("Isolated browser worker required for the restart check");
    execFileSync("docker", ["restart", worker]);
    const verify = async () => {
      const response = await page.request.post(`/api/nodes/${node.id}/verify`, { headers });
      return response.ok() ? ((await response.json()) as { state: string }).state : `http ${response.status()}`;
    };
    await expect.poll(verify, { timeout: 120_000, intervals: [2_000] }).toBe("ready");
    const persisted = await readStatus();
    expect(persisted.settings.listeners).toEqual(expectedSettings.listeners);
    expect(persisted.ports?.[listenerID]).toBe(internalPort);
    expect(proxy().Id).toBe(recreated.Id);
    expect(portBindings()[`${internalPort}/tcp`]).toEqual([{ HostIp: "127.0.0.1", HostPort: String(listenerPort) }]);
    expect(appContainers(id)).toEqual(appsBefore);
    await expectSavedPolicy();
    await expect.poll(() => routedBody(hostname), { timeout: 60_000 }).toBe("listener-app-marker");
  } catch (error) {
    failure = error instanceof Error ? error : new Error(String(error));
    try {
      await page.screenshot({ path: testInfo.outputPath("listeners-failure.png"), fullPage: true, timeout: 10_000 });
    } catch (screenshotError) {
      console.error("Could not capture the failure screenshot:", screenshotError);
    }
  }

  // The restore always runs, but a restore that also fails is recorded beside the original failure, not in its place.
  let cleanupFailure: Error | undefined;
  try {
    await restoreListeners();
  } catch (error) {
    cleanupFailure = error instanceof Error ? error : new Error(String(error));
  }
  if (failure !== undefined) {
    if (cleanupFailure !== undefined) {
      console.error("Restoring the fixture's listener settings also failed:", cleanupFailure);
      testInfo.annotations.push({ type: "cleanup failure", description: cleanupFailure.message });
    }
    throw failure;
  }
  if (cleanupFailure !== undefined) throw cleanupFailure;

  // Restore the fixture's original listener settings through the same plan and apply API.
  async function restoreListeners() {
    const current = await readStatus();
    if ((current.settings.listeners ?? []).some((item) => item.id === listenerID)) {
      // The restore recreates the proxy again; give it its own budget instead of whatever the test left.
      test.setTimeout(testInfo.timeout + 360_000);
      const restorePlan = await page.request.post(`${statusURL}/plan`, { headers, data: restoreSettings });
      expect(restorePlan.status(), await restorePlan.text()).toBe(200);
      const { revision, digest, requires_restart } = (await restorePlan.json()) as Plan;
      if (requires_restart) {
        const restored = await page.request.put(statusURL, { headers, data: { settings: restoreSettings, revision, digest, accept_interruption: true }, timeout: 300_000 });
        expect(restored.status(), await restored.text()).toBe(200);
        await expect.poll(async () => { const change = (await readStatus()).change; return change ? `${change.phase}:${change.committed}` : "none"; }, { timeout: 300_000, intervals: [2_000] }).toBe("completed:true");
      }
      expect(Object.values(portBindings()).flat().some((binding) => binding?.HostPort === String(listenerPort))).toBe(false);
    }
  }
});

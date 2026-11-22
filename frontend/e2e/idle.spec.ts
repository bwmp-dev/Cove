import { execFileSync, spawnSync } from "node:child_process";
import { expect, test, type APIResponse, type Page, type TestInfo } from "@playwright/test";

const password = process.env.COVE_BROWSER_PASSWORD ?? "";

// Test fixture credentials for the route's basic authentication, used only against the isolated browser panel.
// The entry is a bcrypt hash of "idle-fixture-password", which the API requires instead of a plain password.
const routeUser = "reader";
const routePassword = "idle-fixture-password";
const routeAuth = `${routeUser}:$2a$10$kzu/gIbyrXqchcmSIx.KROzjiWi0rBnNIIFo316tf7C0JrRSULhxC`;

test.afterEach(() => {
  const database = process.env.COVE_BROWSER_DATABASE;
  if (!database || !/^cove-browser-[a-f0-9]+-db$/.test(database)) throw new Error("Isolated browser database required for cleanup");
  const resources = execFileSync("docker", ["exec", database, "psql", "-U", "postgres", "-d", "cove", "-Atc", "UPDATE workloads w SET manually_stopped=true WHERE NOT EXISTS(SELECT 1 FROM operations o WHERE o.workload_id=w.id AND o.state IN ('queued','running')) RETURNING w.id"], { encoding: "utf8" }).split(/\s+/).filter((value) => /^[a-f0-9]{32}$/.test(value));
  for (const resource of resources) {
    const helpers = execFileSync("docker", ["ps", "-aq", "--filter", "label=cove.managed=idle", "--filter", `label=cove.idle-workload=${resource}`], { encoding: "utf8" }).split(/\s+/).filter(Boolean);
    if (helpers.length) execFileSync("docker", ["rm", "-f", ...helpers]);
    const ids = execFileSync("docker", ["ps", "-aq", "--filter", `label=com.docker.compose.project=cove-${resource}`], { encoding: "utf8" }).split(/\s+/).filter(Boolean);
    if (ids.length) execFileSync("docker", ["rm", "-f", "-v", ...ids]);
    removeIdleNetworks(resource);
    const networks = execFileSync("docker", ["network", "ls", "-q", "--filter", `label=com.docker.compose.project=cove-${resource}`], { encoding: "utf8" }).split(/\s+/).filter(Boolean);
    if (networks.length) execFileSync("docker", ["network", "rm", ...networks]);
  }
});

interface NetworkDetail {
  Name: string;
  Labels: Record<string, string> | null;
  Containers: Record<string, { Name: string }> | null;
}

// Removes the worker's idle front and back networks for one resource of this test after its helper and app containers are gone.
// Only this panel's ingress may still be attached to the front network; any other endpoint fails cleanup instead of being removed.
function removeIdleNetworks(resource: string) {
  const networks = execFileSync("docker", ["network", "ls", "-q", "--filter", "label=cove.managed=idle", "--filter", `label=cove.idle-workload=${resource}`], { encoding: "utf8" }).split(/\s+/).filter(Boolean);
  for (const id of networks) {
    const network = JSON.parse(execFileSync("docker", ["network", "inspect", "--format", "{{json .}}", id], { encoding: "utf8" })) as NetworkDetail;
    const role = network.Labels?.["cove.idle-role"];
    if (network.Labels?.["cove.managed"] !== "idle" || network.Labels["cove.idle-workload"] !== resource || (role !== "front" && role !== "back")) throw new Error(`Idle network ${network.Name} is not owned by resource ${resource}`);
    for (const [endpoint, { Name }] of Object.entries(network.Containers ?? {})) {
      const managed = execFileSync("docker", ["inspect", "--format", '{{index .Config.Labels "cove.managed"}}', endpoint], { encoding: "utf8" }).trim();
      if (role !== "front" || Name !== "cove-ingress" || managed !== "ingress") throw new Error(`Idle network ${network.Name} has an unexpected endpoint ${Name}`);
      execFileSync("docker", ["network", "disconnect", id, endpoint]);
    }
    execFileSync("docker", ["network", "rm", id]);
  }
}

async function expectNoHorizontalScroll(page: Page) {
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBe(true);
}

async function capture(page: Page, testInfo: TestInfo, name: string) {
  for (const width of [320, 390, 768, 1440]) {
    await page.setViewportSize({ width, height: 900 });
    await page.evaluate(() => window.scrollTo(0, 0));
    await expectNoHorizontalScroll(page);
    await page.screenshot({ path: testInfo.outputPath(`${name}-${width}.png`), fullPage: true });
  }
  await page.setViewportSize({ width: 1280, height: 900 });
}

// An HTTPS request through the real Traefik ingress, addressed by Host header so no DNS or public certificate is involved.
// The first request to a sleeping app is held while it wakes, for up to 3 minutes.
function routed(hostname: string, authorized: boolean) {
  const auth = authorized ? [`--header=Authorization: Basic ${Buffer.from(`${routeUser}:${routePassword}`).toString("base64")}`] : [];
  const result = spawnSync("docker", ["exec", "cove-ingress", "wget", "-S", "-qO-", "-T", "200", "--no-check-certificate", `--header=Host: ${hostname}`, ...auth, "https://127.0.0.1/"], { encoding: "utf8", timeout: 210_000 });
  const status = /HTTP\/[\d.]+ (\d{3})/.exec(result.stderr)?.[1];
  return { status: status ? Number(status) : undefined, body: result.stdout.trim(), stderr: result.stderr };
}

// init forwards SIGTERM so httpd stops promptly instead of waiting out Docker's stop grace period as PID 1.
const compose = [
  "services:",
  "  app:",
  "    image: busybox:1.37",
  "    pull_policy: never",
  "    init: true",
  "    restart: unless-stopped",
  "    command: [sh, -c, 'mkdir -p /www && echo awake-fixture > /www/index.html && exec httpd -f -p 8080 -h /www']",
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
  result: { error?: string; suspension?: { reason?: string; phase: string } } | null;
}

interface Runtime {
  idle?: { phase: string; active_requests: number } | null;
  suspension?: { reason: string; phase: string } | null;
}

test("idle sleep: a real worker sleeps an idle app, ignores unauthorized requests and wakes the same containers", async ({ page }, testInfo) => {
  // Deploy, route, one full idle timeout plus the worker's 15 second check, a wake, and a second sleep for the optional disable.
  test.setTimeout(360_000);
  await page.goto("/projects");
  await expect(page).toHaveURL(/\/login(?:[?#]|$)/);
  await page.getByLabel("Password", { exact: true }).fill(password);
  await page.getByRole("button", { name: "Sign in", exact: true }).click();
  await expect(page).not.toHaveURL(/\/login(?:[?#]|$)/);
  const headers = { Origin: new URL(page.url()).origin };

  const nodes = (await (await page.request.get("/api/nodes")).json()) as { id: string; name: string; capabilities: { idle_sleep?: boolean } | null }[];
  const node = nodes.find((item) => item.name === "ready-browser-worker");
  if (!node) throw new Error("The browser worker is missing");
  expect(node.capabilities?.idle_sleep).toBe(true);
  const project = (await (await page.request.post("/api/projects", { headers, data: { name: "Idle project" } })).json()) as { id: string };
  const created = await page.request.post("/api/workloads", { headers, data: { name: "idle-web", node_id: node.id, kind: "service", compose, project_id: project.id } });
  expect(created.status()).toBe(201);
  const { id } = (await created.json()) as { id: string };
  const hostname = `idle-${Date.now()}.cove.test`;

  const operation = async (operationId: string) => (await (await page.request.get(`/api/operations/${operationId}`)).json()) as Operation;
  const settled = async (operationId: string) => {
    await expect.poll(async () => (await operation(operationId)).state, { timeout: 180_000 }).toMatch(/^(succeeded|failed|cancelled|interrupted)$/);
    const result = await operation(operationId);
    if (result.state !== "succeeded") throw new Error(`${result.action} ${operationId} ended ${result.state}: ${result.result?.error ?? ""}\n${await (await page.request.get(`/api/operations/${operationId}/logs`)).text()}`);
    return result;
  };
  // The control plane queues image checks for idle deployed resources and rejects changes while any operation is active.
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

  const deploy = (await (await send(() => page.request.post(`/api/workloads/${id}/operations`, { headers, data: { action: "deploy" } }), [202])).json()) as { id: string };
  await settled(deploy.id);
  const routes = await send(() => page.request.put(`/api/workloads/${id}/routes`, { headers, data: { routes: [{ domain: hostname, service: "app", port: 8080, auth: routeAuth, policy: { certificate_source: "default" } }] } }), [200, 202]);
  const routeUpdate = (await routes.json()) as { id?: string };
  if (routeUpdate.id) await settled(routeUpdate.id);
  // Health restarts are off by default and shown as Off; enabling them lets the asleep page prove they are paused, not merely disabled.
  const restartPolicy = await send(() => page.request.put(`/api/workloads/${id}/restart-policy`, { headers, data: { enabled: true } }), [200]);
  expect(((await restartPolicy.json()) as { enabled: boolean }).enabled).toBe(true);

  const container = () => {
    const ids = execFileSync("docker", ["ps", "-aq", "--filter", `label=com.docker.compose.project=cove-${id}`, "--filter", "label=com.docker.compose.service=app"], { encoding: "utf8" }).split(/\s+/).filter(Boolean);
    expect(ids).toHaveLength(1);
    const [containerId = "", running = "", restart = "", health = ""] = execFileSync("docker", ["inspect", "--format", "{{.Id}}|{{.State.Running}}|{{.HostConfig.RestartPolicy.Name}}|{{with index .State \"Health\"}}{{.Status}}{{end}}", ids[0] ?? ""], { encoding: "utf8" }).trim().split("|");
    return { id: containerId, running: running === "true", restart, health };
  };
  await expect.poll(() => container().health, { timeout: 60_000 }).toBe("healthy");
  const app = container();
  expect(app.restart).toBe("unless-stopped");
  await expect.poll(() => routed(hostname, true).body, { timeout: 60_000 }).toBe("awake-fixture");

  // Turn idle sleep on from the dashboard with the keyboard: switch, timeout, then the request-driven assertion.
  await page.goto(`/resources/${id}/deployment-safety`);
  const section = page.getByRole("region", { name: "Idle sleep", exact: true });
  const sleepSwitch = section.getByRole("switch", { name: "Sleep when idle" });
  const timeoutField = section.getByRole("textbox", { name: "Idle timeout" });
  const assertion = section.getByRole("checkbox", { name: "Every service is a request-driven HTTP app" });
  await expect(section.getByText("Off", { exact: true })).toBeVisible();
  await expect(sleepSwitch).toBeEnabled({ timeout: 30_000 });
  await sleepSwitch.focus();
  await page.keyboard.press("Space");
  await expect(sleepSwitch).toBeChecked();
  await page.keyboard.press("Tab");
  await expect(timeoutField).toBeFocused();
  await page.keyboard.press("ControlOrMeta+A");
  await page.keyboard.type("60");
  await page.keyboard.press("Tab");
  await expect(assertion).toBeFocused();
  await page.keyboard.press("Space");
  await expect(assertion).toBeChecked();
  await capture(page, testInfo, "idle-form");
  const save = section.getByRole("button", { name: "Save changes", exact: true });
  for (let attempt = 1; ; attempt++) {
    await quiet();
    await expect(save).toBeEnabled({ timeout: 30_000 });
    const request = page.waitForResponse((response) => new URL(response.url()).pathname === `/api/workloads/${id}/idle-policy` && response.request().method() === "PUT");
    await save.click();
    const response = await request;
    if (attempt < 3 && response.status() === 409 && (await response.text()).includes("active operation")) continue;
    expect(response.status(), await response.text()).toBe(202);
    await settled(((await response.json()) as { id: string }).id);
    break;
  }
  expect(await (await page.request.get(`/api/workloads/${id}/idle-policy`)).json()).toEqual({ enabled: true, after_seconds: 60, request_driven: true });
  await expect(section.getByText("On, after 1 minute idle")).toBeVisible({ timeout: 35_000 });

  // One idle minute, the worker's check interval and the stop itself.
  const runtime = async () => (await (await page.request.get(`/api/workloads/${id}/runtime`)).json()) as Runtime;
  await expect.poll(async () => (await runtime()).idle?.phase, { timeout: 120_000, intervals: [2_000] }).toBe("sleeping");
  const asleep = container();
  expect(asleep.id).toBe(app.id);
  expect(asleep.running).toBe(false);
  expect(asleep.restart).toBe("no");
  expect((await runtime()).suspension).toMatchObject({ reason: "idle", phase: "paused" });

  await page.goto(`/resources/${id}`);
  await expect(page.getByText("App is asleep", { exact: true })).toBeVisible({ timeout: 20_000 });
  await expect(page.getByRole("button", { name: "Deploy", exact: true })).toBeDisabled();
  await expect(page.getByRole("button", { name: "Stop", exact: true })).toBeEnabled();
  await expect(page.getByRole("button", { name: "Wake now" })).toBeVisible();
  const containers = page.getByRole("region", { name: "Containers" });
  await expect(containers.getByRole("row", { name: /app/ }).getByText("Sleeping", { exact: true })).toBeVisible();
  await expect(containers.getByText(/This app is asleep after being idle/)).toBeVisible();
  await capture(page, testInfo, "idle-resource-asleep");
  expect((await page.request.post(`/api/workloads/${id}/operations`, { headers, data: { action: "deploy" } })).status()).not.toBe(202);

  await page.goto(`/resources/${id}/deployment-safety`);
  await expect(section.getByText("Sleeping", { exact: true })).toBeVisible();
  await expect(page.getByRole("region", { name: "Automatic health restarts" }).getByText("Paused", { exact: true })).toBeVisible();
  await capture(page, testInfo, "idle-settings-asleep");

  // Basic authentication answers before the idle gateway, so a rejected request never wakes the app.
  const rejected = routed(hostname, false);
  expect(rejected.status, rejected.stderr).toBe(401);
  expect(container().running).toBe(false);
  expect((await runtime()).idle?.phase).toBe("sleeping");

  // The first authorized request waits for the same container to pass its healthcheck, then gets the app's response.
  const woken = routed(hostname, true);
  expect(woken.status, woken.stderr).toBe(200);
  expect(woken.body).toBe("awake-fixture");
  const awake = container();
  expect(awake.id).toBe(app.id);
  expect(awake.running).toBe(true);
  expect(awake.restart).toBe("unless-stopped");
  expect(awake.health).toBe("healthy");
  await expect.poll(async () => (await runtime()).idle?.phase, { timeout: 30_000 }).toBe("awake");
  await page.reload();
  await expect(section.getByText("Awake", { exact: true })).toBeVisible({ timeout: 20_000 });
  await capture(page, testInfo, "idle-settings-awake");

  // Turning the policy off while asleep wakes the same containers and removes the idle helper.
  await expect.poll(async () => (await runtime()).idle?.phase, { timeout: 120_000, intervals: [2_000] }).toBe("sleeping");
  await page.reload();
  await expect(sleepSwitch).toBeChecked({ timeout: 30_000 });
  await sleepSwitch.click();
  for (let attempt = 1; ; attempt++) {
    await quiet();
    await expect(save).toBeEnabled({ timeout: 30_000 });
    const request = page.waitForResponse((response) => new URL(response.url()).pathname === `/api/workloads/${id}/idle-policy` && response.request().method() === "PUT");
    await save.click();
    const response = await request;
    if (attempt < 3 && response.status() === 409 && (await response.text()).includes("active operation")) continue;
    expect(response.status(), await response.text()).toBe(202);
    await settled(((await response.json()) as { id: string }).id);
    break;
  }
  expect(((await (await page.request.get(`/api/workloads/${id}/idle-policy`)).json()) as { enabled: boolean }).enabled).toBe(false);
  await expect.poll(() => container().running, { timeout: 120_000 }).toBe(true);
  const restored = container();
  expect(restored.id).toBe(app.id);
  expect(restored.restart).toBe("unless-stopped");
  expect(execFileSync("docker", ["ps", "-aq", "--filter", "label=cove.managed=idle", "--filter", `label=cove.idle-workload=${id}`], { encoding: "utf8" }).trim()).toBe("");
  expect((await runtime()).idle ?? null).toBeNull();
  await expect.poll(() => routed(hostname, true).body, { timeout: 60_000 }).toBe("awake-fixture");
  await page.reload();
  await expect(section.getByText(/no idle policy for this resource/)).toBeVisible({ timeout: 20_000 });
});

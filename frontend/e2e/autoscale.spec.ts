import { execFileSync } from "node:child_process";
import { expect, test, type APIResponse, type Page, type TestInfo } from "@playwright/test";

const password = process.env.COVE_BROWSER_PASSWORD ?? "";

test.afterEach(() => {
  const database = process.env.COVE_BROWSER_DATABASE;
  if (!database || !/^cove-browser-[a-f0-9]+-db$/.test(database)) throw new Error("Isolated browser database required for cleanup");
  // Postponing CPU checks first stops the panel from queuing another scaling deploy while containers are removed.
  execFileSync("docker", ["exec", database, "psql", "-U", "postgres", "-d", "cove", "-Atc", "UPDATE workloads SET next_cpu_check_at=now()+interval '1 day'"]);
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
    await page.evaluate(() => window.scrollTo(0, 0));
    await expectNoHorizontalScroll(page);
    await page.screenshot({ path: testInfo.outputPath(`${name}-${width}.png`), fullPage: true });
  }
  await page.setViewportSize({ width: 1280, height: 900 });
}

// Each replica serves HTTP and keeps one core busy, far above its 0.25 CPU limit, so every sample is above target.
// init forwards SIGTERM so the replica stops promptly during the rolling replacement and drain.
const compose = [
  "services:",
  "  app:",
  "    image: busybox:1.37",
  "    pull_policy: never",
  "    init: true",
  "    restart: unless-stopped",
  "    command: [sh, -c, 'mkdir -p /www && echo cpu-fixture > /www/index.html && httpd -p 8080 -h /www && while :; do :; done']",
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
  result: { error?: string; scale?: { generation: string; before: Record<string, number>; replicas: Record<string, number>; cpu_percent: Record<string, number> } } | null;
}

interface AutoscaleState {
  policy: { enabled: boolean; services: Record<string, { min_replicas: number; max_replicas: number; target_cpu_percent: number }> };
  samples: { sampled_at: string; replicas: Record<string, number>; cpu_percent: Record<string, number> }[] | null;
  operation_id: string;
  status: string;
  last_error: string;
}

test("CPU scaling: a real CPU-bound service gains a replica through a rolling deploy of the current release", async ({ page }, testInfo) => {
  // Deploy, 3 samples at least 30 seconds apart from a once-a-minute scheduler, the scaling deploy and its drain.
  test.setTimeout(600_000);
  await page.goto("/projects");
  await expect(page).toHaveURL(/\/login(?:[?#]|$)/);
  await page.getByLabel("Password", { exact: true }).fill(password);
  await page.getByRole("button", { name: "Sign in", exact: true }).click();
  await expect(page).not.toHaveURL(/\/login(?:[?#]|$)/);
  const headers = { Origin: new URL(page.url()).origin };

  const nodes = (await (await page.request.get("/api/nodes")).json()) as { id: string; name: string; capabilities: { cpu_autoscale?: boolean } | null }[];
  const node = nodes.find((item) => item.name === "ready-browser-worker");
  if (!node) throw new Error("The browser worker is missing");
  expect(node.capabilities?.cpu_autoscale).toBe(true);
  const project = (await (await page.request.post("/api/projects", { headers, data: { name: "CPU scaling project" } })).json()) as { id: string };
  const rollout = { strategy: "rolling", parallel_volumes: false, drain_seconds: 0, timeout_seconds: 120, canary_percent: 10 };
  // The managed route must exist before the first deploy, or that deploy falls back to guarded replacement and creates no generation to scale.
  const hostname = `cpu-${Date.now()}.cove.test`;
  const routes = [{ domain: hostname, service: "app", port: 8080, policy: { certificate_source: "default" } }];
  const created = await page.request.post("/api/workloads", { headers, data: { name: "cpu-web", node_id: node.id, kind: "service", compose, project_id: project.id, rollout, routes, limits: { app: { cpus: 0.25, memory_mb: 64 } } } });
  expect(created.status(), await created.text()).toBe(201);
  const { id } = (await created.json()) as { id: string };

  const operation = async (operationId: string) => (await (await page.request.get(`/api/operations/${operationId}`)).json()) as Operation;
  const settled = async (operationId: string, timeout = 180_000) => {
    await expect.poll(async () => (await operation(operationId)).state, { timeout }).toMatch(/^(succeeded|failed|cancelled|interrupted)$/);
    const result = await operation(operationId);
    if (result.state !== "succeeded") throw new Error(`${result.action} ${operationId} ended ${result.state}: ${result.result?.error ?? ""}\n${await (await page.request.get(`/api/operations/${operationId}/logs`)).text()}`);
    return result;
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

  const deploy = (await (await send(() => page.request.post(`/api/workloads/${id}/operations`, { headers, data: { action: "deploy" } }), [202])).json()) as { id: string };
  await settled(deploy.id);

  const replicas = () => {
    const ids = execFileSync("docker", ["ps", "-q", "--filter", `label=com.docker.compose.project=cove-${id}`, "--filter", "label=dev.cove.service=app"], { encoding: "utf8" }).split(/\s+/).filter(Boolean);
    return ids.map((containerId) => execFileSync("docker", ["inspect", "--format", '{{with index .State "Health"}}{{.Status}}{{end}}', containerId], { encoding: "utf8" }).trim());
  };
  await expect.poll(() => replicas(), { timeout: 60_000 }).toEqual(["healthy"]);

  // Turn scaling on from the dashboard: switch, service, then its bounds and target.
  await page.goto(`/resources/${id}/deployment-safety`);
  const section = page.getByRole("region", { name: "CPU scaling", exact: true });
  const scaleSwitch = section.getByRole("switch", { name: "Scale on CPU use" });
  await expect(section.getByText("Off", { exact: true })).toBeVisible({ timeout: 30_000 });
  await expect(scaleSwitch).toBeEnabled({ timeout: 30_000 });
  await scaleSwitch.focus();
  await page.keyboard.press("Space");
  await expect(scaleSwitch).toBeChecked();
  const service = section.getByRole("checkbox", { name: "Scale app" });
  await service.focus();
  await page.keyboard.press("Space");
  await expect(service).toBeChecked();
  await section.getByRole("textbox", { name: "Maximum replicas" }).fill("2");
  await section.getByRole("textbox", { name: "Target CPU (%)" }).fill("50");
  await capture(page, testInfo, "autoscale-form");
  const save = section.getByRole("button", { name: "Save changes", exact: true });
  for (let attempt = 1; ; attempt++) {
    await quiet();
    await expect(save).toBeEnabled({ timeout: 30_000 });
    const request = page.waitForResponse((response) => new URL(response.url()).pathname === `/api/workloads/${id}/autoscale-policy` && response.request().method() === "PUT");
    await save.click();
    const response = await request;
    if (attempt < 3 && response.status() === 409 && (await response.text()).includes("active operation")) continue;
    expect(response.status(), await response.text()).toBe(200);
    break;
  }
  const state = async () => (await (await page.request.get(`/api/workloads/${id}/autoscale-policy`)).json()) as AutoscaleState;
  expect((await state()).policy).toEqual({ enabled: true, services: { app: { min_replicas: 1, max_replicas: 2, target_cpu_percent: 50 } } });
  await expect(section.getByText("Saved. The panel uses it on its next check.")).toBeVisible();

  // A real sample reports the deployed replica and CPU above the limit's target.
  await expect.poll(async () => (await state()).samples?.length ?? 0, { timeout: 150_000, intervals: [5_000] }).toBeGreaterThan(0);
  const first = (await state()).samples?.[0];
  expect(first?.replicas.app).toBe(1);
  expect(first?.cpu_percent.app ?? 0).toBeGreaterThan(50);
  await page.reload();
  await expect(section.getByRole("table").getByRole("row", { name: /app/ })).toBeVisible({ timeout: 30_000 });
  await capture(page, testInfo, "autoscale-sampling");

  // Three consistent samples above target queue one deploy that adds a replica.
  await expect.poll(async () => (await state()).operation_id, { timeout: 360_000, intervals: [5_000] }).not.toBe("");
  const scaleId = (await state()).operation_id;
  const scaled = await settled(scaleId, 240_000);
  expect(scaled.action).toBe("deploy");
  expect(scaled.result?.scale).toMatchObject({ before: { app: 1 }, replicas: { app: 2 } });
  expect(scaled.result?.scale?.cpu_percent.app ?? 0).toBeGreaterThan(50);
  await expect.poll(() => replicas(), { timeout: 120_000 }).toEqual(["healthy", "healthy"]);
  await expect.poll(async () => (await state()).status, { timeout: 30_000 }).toBe("cooldown");

  await page.goto(`/operations/${scaleId}`);
  await expect(page.getByText("CPU scaling deployed the current release with new replica counts.")).toBeVisible({ timeout: 30_000 });
  await expect(page.getByText(/1 to 2 replicas/)).toBeVisible();
  await capture(page, testInfo, "autoscale-operation");

  await page.goto(`/resources/${id}/deployment-safety`);
  await expect(section.getByText("Cooling down", { exact: true })).toBeVisible({ timeout: 30_000 });
  await expect(section.getByText("Cooldown ends")).toBeVisible();
  await capture(page, testInfo, "autoscale-cooldown");

  // Turning scaling off is saved immediately and keeps the replica count the scaler chose.
  await scaleSwitch.click();
  await quiet();
  const off = page.waitForResponse((response) => new URL(response.url()).pathname === `/api/workloads/${id}/autoscale-policy` && response.request().method() === "PUT");
  await save.click();
  expect((await off).status()).toBe(200);
  await expect.poll(async () => (await state()).status, { timeout: 30_000 }).toBe("disabled");
  expect(replicas()).toHaveLength(2);
});

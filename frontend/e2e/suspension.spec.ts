import { execFileSync } from "node:child_process";
import { expect, test, type Page, type TestInfo } from "@playwright/test";

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
    const volumes = execFileSync("docker", ["volume", "ls", "-q", "--filter", `label=com.docker.compose.project=cove-${resource}`], { encoding: "utf8" }).split(/\s+/).filter(Boolean);
    if (volumes.length) execFileSync("docker", ["volume", "rm", ...volumes]);
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

interface Suspension {
  paused: boolean;
  phase: string;
  resources: { id: string; phase: string; operation_id?: string; resume_wanted: boolean; error?: string }[];
}

interface Operation {
  id: string;
  action: string;
  state: string;
  result: { error?: string; suspension?: { phase: string; captured: number; previously_running: number } } | null;
}

// busybox ignores SIGTERM as PID 1, so pausing web ends in Docker's SIGKILL (exit 137): the case the dashboard must show as Paused.
const compose = [
  "services:",
  "  init:",
  "    image: busybox:1.37",
  "    pull_policy: never",
  "    command: [sh, -c, 'echo init >> /data/count']",
  "    volumes: [data:/data]",
  "  web:",
  "    image: busybox:1.37",
  "    pull_policy: never",
  "    command: [sh, -c, 'sleep 3600']",
  "    restart: unless-stopped",
  "    volumes: [data:/data]",
  "    depends_on:",
  "      init:",
  "        condition: service_completed_successfully",
  "    healthcheck:",
  "      test: [CMD, 'true']",
  "      interval: 1s",
  "volumes:",
  "  data: {}",
  "",
].join("\n");

test("environment pause and resume: real worker keeps containers, policies and init state", async ({ page }, testInfo) => {
  test.setTimeout(480_000);
  await page.goto("/projects");
  await expect(page).toHaveURL(/\/login(?:[?#]|$)/);
  await page.getByLabel("Password", { exact: true }).fill(password);
  await page.getByRole("button", { name: "Sign in", exact: true }).click();
  await expect(page).not.toHaveURL(/\/login(?:[?#]|$)/);
  const headers = { Origin: new URL(page.url()).origin };

  const nodes = (await (await page.request.get("/api/nodes")).json()) as { id: string; name: string; capabilities: { suspensions?: boolean } | null }[];
  const node = nodes.find((item) => item.name === "ready-browser-worker");
  if (!node) throw new Error("The browser worker is missing");
  expect(node.capabilities?.suspensions).toBe(true);
  const project = (await (await page.request.post("/api/projects", { headers, data: { name: "Pause project" } })).json()) as { id: string };
  const environment = (await (await page.request.post(`/api/projects/${project.id}/environments`, { headers, data: { name: "staging" } })).json()) as { id: string };
  const created = await page.request.post("/api/workloads", { headers, data: { name: "pause-web", node_id: node.id, kind: "service", compose, project_id: project.id, environment_id: environment.id } });
  expect(created.status()).toBe(201);
  const { id } = (await created.json()) as { id: string };

  const operation = async (operationId: string) => (await (await page.request.get(`/api/operations/${operationId}`)).json()) as Operation;
  const settled = async (operationId: string) => {
    await expect.poll(async () => (await operation(operationId)).state, { timeout: 180_000 }).toMatch(/^(succeeded|failed|cancelled|interrupted)$/);
    const result = await operation(operationId);
    if (result.state !== "succeeded") throw new Error(`${result.action} ${operationId} ended ${result.state}: ${result.result?.error ?? ""}\n${await (await page.request.get(`/api/operations/${operationId}/logs`)).text()}`);
    return result;
  };
  const deploy = (await (await page.request.post(`/api/workloads/${id}/operations`, { headers, data: { action: "deploy" } })).json()) as { id: string };
  await settled(deploy.id);

  const container = (service: string) => {
    const ids = execFileSync("docker", ["ps", "-aq", "--filter", `label=com.docker.compose.project=cove-${id}`, "--filter", `label=com.docker.compose.service=${service}`], { encoding: "utf8" }).split(/\s+/).filter(Boolean);
    expect(ids).toHaveLength(1);
    const [containerId = "", running = "", startedAt = "", restart = "", health = ""] = execFileSync("docker", ["inspect", "--format", "{{.Id}}|{{.State.Running}}|{{.State.StartedAt}}|{{.HostConfig.RestartPolicy.Name}}|{{with index .State \"Health\"}}{{.Status}}{{end}}", ids[0] ?? ""], { encoding: "utf8" }).trim().split("|");
    return { id: containerId, running: running === "true", startedAt, restart, health };
  };
  await expect.poll(() => container("web").health, { timeout: 60_000 }).toBe("healthy");
  const web = container("web");
  const init = container("init");
  expect(web.restart).toBe("unless-stopped");
  expect(init.running).toBe(false);

  const suspension = async () => (await (await page.request.get(`/api/environments/${environment.id}/suspension`)).json()) as Suspension;
  const environmentUrl = `/projects/${project.id}/environments/${environment.id}`;
  await page.goto(environmentUrl);
  const section = page.getByRole("region", { name: "Pause and resume" });
  await expect(section.getByText("Active", { exact: true })).toBeVisible();
  await expect(section.getByRole("list", { name: "Pause state of resources in staging" }).getByRole("link", { name: "pause-web" })).toBeVisible();
  await capture(page, testInfo, "environment-active");

  // Pausing asks for confirmation and is only a queued request until the worker reports back.
  await section.getByRole("button", { name: "Pause environment" }).click();
  const pauseDialog = page.getByRole("dialog", { name: "Pause staging?" });
  await expect(pauseDialog.getByText(/return 503 while paused/)).toBeVisible();
  await capture(page, testInfo, "environment-pause-dialog");
  const pauseRequest = page.waitForResponse((response) => new URL(response.url()).pathname === `/api/environments/${environment.id}/suspension` && response.request().method() === "PUT");
  await pauseDialog.getByRole("button", { name: "Pause environment" }).click();
  expect((await pauseRequest).status()).toBe(202);
  await expect.poll(async () => (await suspension()).phase, { timeout: 180_000 }).toMatch(/^(paused|error)$/);
  const paused = await suspension();
  expect(paused).toMatchObject({ paused: true, phase: "paused" });
  const pauseOperation = paused.resources[0]?.operation_id ?? "";
  expect(pauseOperation).toMatch(/^[a-f0-9]{32}$/);
  const pauseResult = await settled(pauseOperation);
  expect(pauseResult.action).toBe("suspend");
  expect(pauseResult.result?.suspension).toMatchObject({ phase: "paused", previously_running: 1 });
  expect(paused.resources[0]?.resume_wanted).toBe(true);
  await expect(section.getByText("Paused", { exact: true }).first()).toBeVisible({ timeout: 35_000 });
  await expect(section.getByText("Starts on resume")).toBeVisible();
  await capture(page, testInfo, "environment-paused");

  const pausedWeb = container("web");
  expect(pausedWeb.id).toBe(web.id);
  expect(pausedWeb.running).toBe(false);
  expect(pausedWeb.restart).toBe("no");
  expect(container("init").id).toBe(init.id);

  // The resource page links the environment, blocks start and replace actions and leaves Stop available.
  await page.goto(`/resources/${id}`);
  await expect(page.getByText("Environment paused", { exact: true })).toBeVisible();
  await expect(page.getByRole("link", { name: "Open staging" })).toBeVisible();
  for (const action of ["Deploy", "Start", "Restart"]) await expect(page.getByRole("button", { name: action, exact: true })).toBeDisabled();
  await expect(page.getByRole("button", { name: "Stop", exact: true })).toBeEnabled();
  const containers = page.getByRole("region", { name: "Containers" });
  await expect(containers.getByRole("row", { name: /web/ }).getByText("Paused", { exact: true })).toBeVisible();
  await expect(containers.getByRole("row", { name: /init/ }).getByText("Completed", { exact: true })).toBeVisible();
  await expect(containers.getByText(/The environment paused this resource/)).toBeVisible();
  await capture(page, testInfo, "resource-paused");
  expect((await page.request.post(`/api/workloads/${id}/operations`, { headers, data: { action: "deploy" } })).status()).toBe(409);

  // The pause operation stays in history with its own label without replacing the latest deployment.
  await page.goto(`/resources/${id}/operations`);
  await expect(page.getByRole("link", { name: "Pause environment resource" }).first()).toBeVisible();
  await page.goto(`/operations/${pauseOperation}`);
  await expect(page.getByText("Queued by an environment pause")).toBeVisible();
  await expect(page.getByRole("button", { name: "Cancel operation" })).toHaveCount(0);
  await capture(page, testInfo, "operation-pause");

  // Resuming starts the same container with its original restart policy and does not rerun the completed init container.
  await page.goto(environmentUrl);
  await section.getByRole("button", { name: "Resume environment" }).click();
  const resumeDialog = page.getByRole("dialog", { name: "Resume staging?" });
  await expect(resumeDialog.getByText(/Jobs are not run/)).toBeVisible();
  await resumeDialog.getByRole("button", { name: "Resume environment" }).click();
  await expect.poll(async () => (await suspension()).phase, { timeout: 240_000 }).toMatch(/^(active|error)$/);
  const resumed = await suspension();
  expect(resumed).toMatchObject({ paused: false, phase: "active" });
  await expect(section.getByText("Active", { exact: true })).toBeVisible({ timeout: 35_000 });

  const operations = (await (await page.request.get(`/api/operations?workload_id=${id}`)).json()) as Operation[];
  const resumeOperation = operations.find((item) => item.action === "resume");
  if (!resumeOperation) throw new Error("No resume operation was recorded");
  expect((await settled(resumeOperation.id)).result?.suspension?.phase).toBe("resumed");
  await expect.poll(() => container("web").running, { timeout: 120_000 }).toBe(true);
  const resumedWeb = container("web");
  expect(resumedWeb.id).toBe(web.id);
  expect(resumedWeb.restart).toBe("unless-stopped");
  const resumedInit = container("init");
  expect(resumedInit.id).toBe(init.id);
  expect(resumedInit.startedAt).toBe(init.startedAt);
  expect(execFileSync("docker", ["exec", web.id, "cat", "/data/count"], { encoding: "utf8" }).trim()).toBe("init");

  await page.goto(`/resources/${id}`);
  await expect(page.getByText("Environment paused", { exact: true })).toHaveCount(0);
  await expect(page.getByRole("button", { name: "Deploy", exact: true })).toBeEnabled();
  await capture(page, testInfo, "resource-resumed");
});

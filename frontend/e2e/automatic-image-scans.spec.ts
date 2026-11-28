import { execFileSync } from "node:child_process";
import { expect, test, type Locator, type Page, type TestInfo } from "@playwright/test";
import { scanOriginLabel } from "../src/lib/image-scans";
import { cleanupBrowserResources, expectNoHorizontalScroll } from "./route-tls";

// Runs only with scripts/test-browser.py --image-scans, the same seeded offline scanner fixture as e2e/image-scans.spec.ts:
// the cached scanner at its release pin, a preseeded main database and the verified Distribution 2.8.3 image (never
// pulled), whose real Go packages carry real advisories. Nothing is mocked or intercepted, and no scan result, history
// row or capability is written by this spec. The only database writes are owned scheduling deferrals and due times.
const password = process.env.COVE_BROWSER_PASSWORD ?? "";
const enabled = process.env.COVE_BROWSER_IMAGE_SCANS === "1";
const base = process.env.COVE_BROWSER_BASE_PATH ?? "";
const panelPath = (route: string) => `${base}${route}`;

test.use({ actionTimeout: 30_000, navigationTimeout: 60_000 });

const docker = (...args: string[]) => execFileSync("docker", args, { encoding: "utf8", stdio: ["ignore", "pipe", "pipe"] }).trim();
const specKey = "cove.automatic-image-scans-spec";
const specLabel = `${specKey}=1`;
const id32 = /^[a-f0-9]{32}$/;
const sha256 = /^sha256:[a-f0-9]{64}$/;
const terminal = /^(succeeded|failed|cancelled|interrupted)$/;
// Keys that would mean raw scanner JSON, image configuration or secret output reached the API.
const forbiddenKeys = /"(password|token|secret|secrets|env|environment|labels|cmd|entrypoint|config|history|description|raw|Results|Metadata)"\s*:/;
const fixtureNote = "Controlled offline generation-change fixture: whitespace appended to the real cached metadata, rehashed with unchanged real database bytes and freshness timestamps. Not new CVEs and not a downloaded database.";

// What this spec owns: Docker objects created outside the API, resources created through it, and the scanner cache state.
const owned = { fixture: "", seed: "", images: [] as string[], workloads: [] as string[], origin: "", missing: false };
const evidence: Record<string, unknown> = {};

const redact = (text: string) => (password ? text.split(password).join("[redacted]") : text);

function fixtureOwner() {
  const owner = /^(cove-browser-[a-f0-9]{12})-worker$/.exec(process.env.COVE_BROWSER_WORKER ?? "")?.[1];
  const database = `${owner}-db`;
  if (!owner || process.env.COVE_BROWSER_DATABASE !== database) throw new Error("Isolated browser fixture required");
  return { owner, database };
}

function sql(query: string) {
  const { database } = fixtureOwner();
  return docker("exec", database, "psql", "-U", "postgres", "-d", "cove", "-Atc", query);
}

// psql also prints the command tag, so only returned IDs are compared.
const returnedIds = (output: string) => output.split(/\s+/).filter((value) => id32.test(value));

function cacheFixture(action: "rotate" | "missing" | "restore") {
  return execFileSync("python", ["../scripts/automatic_scans_fixture.py", action], { encoding: "utf8", env: process.env, stdio: ["ignore", "pipe", "pipe"] }).trim();
}

function ownedBySpec(kind: "container" | "image", name: string) {
  try {
    return docker(kind, "inspect", "--format", `{{index .Config.Labels "${specKey}"}}|{{index .Config.Labels "cove.browser-fixture"}}`, name) === `1|${owned.fixture}`;
  } catch {
    return false;
  }
}

test.afterEach(async ({ page }) => {
  const testInfo = test.info();
  const problems: string[] = [];
  const attempt = async (step: () => unknown) => {
    try {
      await step();
    } catch (error) {
      problems.push(error instanceof Error ? error.message : String(error));
    }
  };
  await attempt(() => testInfo.attach("automatic-scans-evidence.json", { body: redact(JSON.stringify(evidence, null, 2)).slice(0, 16_000), contentType: "application/json" }));
  if (owned.missing) {
    await attempt(() => {
      cacheFixture("restore");
      owned.missing = false;
    });
  }
  // Scanning settings belong to the team and node, not the resource, so they outlive the deleted resources and would
  // leak into e2e/image-scans.spec.ts in the same harness. They are reset while an owned resource still addresses them.
  const settingsWorkload = owned.workloads[0];
  if (settingsWorkload && owned.origin) {
    await attempt(async () => {
      const response = await page.request.put(panelPath(`/api/workloads/${settingsWorkload}/image-scanning`), { headers: { Origin: owned.origin }, data: { enabled: false, notify: false, automatic: false } });
      if (response.status() !== 200) throw new Error(`Resetting scanning settings returned ${response.status()}: ${redact(await response.text()).slice(0, 300)}`);
    });
  }
  // The worker refuses to remove a running resource, so each one is stopped through the API first. A stop request
  // answers 409 while another operation is active, and is retried until that settles.
  for (const id of [...owned.workloads].reverse()) {
    await attempt(async () => {
      const deadline = Date.now() + 120_000;
      let operationId = "";
      while (!operationId) {
        const response = await page.request.post(panelPath(`/api/workloads/${id}/operations`), { headers: { Origin: owned.origin }, data: { action: "stop" } });
        if (response.status() === 404) return;
        if (response.status() === 202) {
          operationId = ((await response.json()) as { id: string }).id;
          continue;
        }
        if (response.status() !== 409 || Date.now() > deadline) throw new Error(`Stopping ${id} returned ${response.status()}: ${redact(await response.text()).slice(0, 300)}`);
        await page.waitForTimeout(3_000);
      }
      for (;;) {
        const response = await page.request.get(panelPath(`/api/operations/${operationId}`));
        if (response.status() !== 200) throw new Error(`Stop operation ${operationId} for ${id} returned ${response.status()}`);
        const { state } = (await response.json()) as { state: string };
        if (state === "succeeded") return;
        if (terminal.test(state)) throw new Error(`Stop operation ${operationId} for ${id} ended ${state}`);
        if (Date.now() > deadline) throw new Error(`Stop operation ${operationId} for ${id} did not finish (${state})`);
        await page.waitForTimeout(2_000);
      }
    });
  }
  // Deleting a resource that holds a background scan lease requests its cancellation and answers 409 by contract;
  // the retry follows once the lease settles.
  for (const id of [...owned.workloads].reverse()) {
    await attempt(async () => {
      const deadline = Date.now() + 120_000;
      for (;;) {
        const response = await page.request.delete(panelPath(`/api/workloads/${id}`), { headers: { Origin: owned.origin } });
        if (response.status() === 404 || response.ok()) return;
        if (response.status() !== 409 || Date.now() > deadline) throw new Error(`DELETE ${id} returned ${response.status()}: ${redact(await response.text()).slice(0, 300)}`);
        await page.waitForTimeout(3_000);
      }
    });
  }
  await attempt(cleanupBrowserResources);
  if (owned.seed && ownedBySpec("container", owned.seed)) await attempt(() => docker("rm", "-f", "-v", owned.seed));
  for (const image of [...owned.images].reverse()) if (ownedBySpec("image", image)) await attempt(() => docker("image", "rm", image));
  Object.assign(owned, { fixture: "", seed: "", images: [], workloads: [], origin: "", missing: false });
  for (const key of Object.keys(evidence)) delete evidence[key];
  if (problems.length > 0 && testInfo.status === testInfo.expectedStatus) throw new Error(`Cleanup failed: ${problems.join("; ")}`);
});

const sizes = [320, 390, 768, 1280, 1440, 1920];

async function capture(page: Page, testInfo: TestInfo, name: string) {
  await page.mouse.move(0, 0);
  for (const width of sizes) {
    await page.setViewportSize({ width, height: 900 });
    await expectNoHorizontalScroll(page);
    await page.screenshot({ path: testInfo.outputPath(`${name}-${width}.png`), fullPage: true });
  }
  await page.setViewportSize({ width: 1280, height: 900 });
}

interface Operation {
  id: string;
  action: string;
  state: string;
  result: { error?: string } | null;
}

interface Settings {
  enabled: boolean;
  notify: boolean;
  automatic: boolean;
  automatic_supported: boolean;
  automatic_running: boolean;
  next_automatic_check_at: string | null;
  automatic_message: string;
  can_manage: boolean;
  worker_supported: boolean;
  node_ready: boolean;
  scanner_image: string;
}

interface Finding {
  id: string;
  package: string;
  installed: string;
  severity: string;
}

interface ScannedImage {
  service: string;
  image_id: string;
  config_digest: string;
  counts: Record<string, number> | null;
  findings: Finding[] | null;
}

interface Results {
  state: string;
  origin: string;
  operation_state: string | null;
  stale: boolean;
  operation_id: string | null;
  result: {
    scanner_image: string;
    scanner_version: string;
    database: { digest: string; updated_at: string; next_update: string };
    checked_at: string;
    state: string;
    images: ScannedImage[] | null;
  } | null;
}

const serviceCompose = (image: string) =>
  ["services:", "  app:", `    image: ${image}`, "    pull_policy: never", "    entrypoint: [/bin/sh]", "    command: [-c, exec sleep 86400]", ""].join("\n");

// Each run appends a line to a volume, so a scan that started the job would be visible in the count.
const jobCompose = (image: string) =>
  [
    "services:",
    "  task:",
    `    image: ${image}`,
    "    pull_policy: never",
    "    entrypoint: [/bin/sh]",
    "    command: [-c, 'echo run >> /runs/count']",
    "    volumes:",
    "      - runs:/runs",
    "volumes:",
    "  runs: {}",
    "",
  ].join("\n");

const definition = (scope: Locator, term: string) => scope.locator("dt").filter({ hasText: new RegExp(`^${term}$`) }).locator("xpath=following-sibling::dd[1]");

test("automatic image scans: opted-in scans run after deploys, job runs and database changes without touching workloads", async ({ page }, testInfo) => {
  test.skip(!enabled, "Needs scripts/test-browser.py --image-scans");
  test.setTimeout(600_000);
  const { owner } = fixtureOwner();
  owned.fixture = owner;
  const registryImage = process.env.COVE_BROWSER_REGISTRY_IMAGE ?? "";
  if (!sha256.test(registryImage)) throw new Error("The harness must provide the verified local registry image");
  expect(docker("image", "inspect", "--format", "{{.Id}}", registryImage)).toBe(registryImage);

  await page.goto(panelPath("/projects"));
  await expect(page).toHaveURL(/\/login(?:[?#]|$)/);
  await page.getByLabel("Password", { exact: true }).fill(password);
  await page.getByRole("button", { name: "Sign in", exact: true }).click();
  await expect(page).not.toHaveURL(/\/login(?:[?#]|$)/);
  const headers = { Origin: new URL(page.url()).origin };
  owned.origin = headers.Origin;

  const nodes = (await (await page.request.get(panelPath("/api/nodes"))).json()) as { id: string; name: string; capabilities: { image_scanning?: boolean; automatic_image_scans?: boolean } | null }[];
  const node = nodes.find((item) => item.name === "ready-browser-worker");
  if (!node) throw new Error("The browser worker is missing");
  expect(node.capabilities).toMatchObject({ image_scanning: true, automatic_image_scans: true });

  const read = async <T>(path: string) => {
    const response = await page.request.get(panelPath(path));
    const text = await response.text();
    expect(response.status(), redact(text).slice(0, 500)).toBe(200);
    expect(text).not.toMatch(forbiddenKeys);
    return JSON.parse(text) as T;
  };
  const operation = (id: string) => read<Operation>(`/api/operations/${id}`);
  const operations = (id: string) => read<Operation[]>(`/api/operations?workload_id=${id}`);
  const scans = async (id: string) => (await operations(id)).filter((item) => item.action === "image_scan");
  const settings = (id: string) => read<Settings>(`/api/workloads/${id}/image-scanning`);
  const results = (id: string) => read<Results>(`/api/workloads/${id}/image-scans`);

  // Cleanup deletes the resources and their logs, so a failed operation's log is attached first, bounded and redacted.
  const settled = async (id: string, timeout = 180_000) => {
    await expect.poll(async () => (await operation(id)).state, { timeout, intervals: [2_000] }).toMatch(terminal);
    const result = await operation(id);
    if (result.state !== "succeeded") {
      let diagnostic: string;
      try {
        const response = await page.request.get(panelPath(`/api/operations/${id}/logs`));
        if (response.status() !== 200) throw new Error(`log request returned ${response.status()}`);
        const logs = redact((JSON.parse(await response.text()) as { logs: string }).logs);
        diagnostic = logs.length > 4_000 ? `...${logs.slice(-4_000)}` : logs;
        await testInfo.attach(`operation-${id}-log.txt`, { body: diagnostic, contentType: "text/plain" });
      } catch (error) {
        diagnostic = `log unavailable: ${error instanceof Error ? error.message.slice(0, 300) : "unknown error"}`;
      }
      throw new Error(`${result.action} ${id} ended ${result.state}: ${redact(result.result?.error ?? "")}\n--- operation log (tail) ---\n${diagnostic}`);
    }
    return result;
  };
  const deploy = async (id: string) => {
    const response = await page.request.post(panelPath(`/api/workloads/${id}/operations`), { headers, data: { action: "deploy" } });
    expect(response.status(), redact(await response.text())).toBe(202);
    return settled(((await response.json()) as { id: string }).id);
  };

  // At most one background scan lease per node, whatever the moment. The largest seen is kept as evidence.
  let maxLeases = 0;
  const checkLeases = () => {
    const count = Number(sql(`SELECT count(*) FROM automatic_image_scans WHERE node_id='${node.id}'`));
    expect(count).toBeLessThanOrEqual(1);
    maxLeases = Math.max(maxLeases, count);
    evidence.max_leases_per_node = maxLeases;
  };
  // A lease has no history yet: one snapshot proves its operation row does not exist while it runs.
  const checkLeaseHasNoHistory = (id: string) => {
    for (const row of sql(`SELECT a.id||'|'||EXISTS(SELECT 1 FROM operations o WHERE o.id=a.id) FROM automatic_image_scans a WHERE a.workload_id='${id}'`).split(/\s+/).filter(Boolean)) {
      expect(row).toMatch(/^[^|]+\|false$/);
    }
  };

  const setDue = (ids: string[], when: "now()" | "now()+interval '1 day'") => {
    for (const id of ids) expect(id).toMatch(id32);
    const list = ids.map((id) => `'${id}'`).join(",");
    expect(returnedIds(sql(`UPDATE image_scan_automation_state SET next_check_at=${when} WHERE workload_id IN (${list}) RETURNING workload_id`)).sort()).toEqual([...ids].sort());
  };
  // The scheduler moves next_check_at a minute ahead when it looks at a resource, which proves the forced check ran.
  const waitChecked = async (id: string) => {
    await expect.poll(() => sql(`SELECT next_check_at>now()+interval '20 seconds' FROM image_scan_automation_state WHERE workload_id='${id}'`), { timeout: 60_000, intervals: [2_000] }).toBe("t");
  };

  // Waits for exactly one new automatic scan to appear in the resource's history and settle, watching leases meanwhile.
  const automaticScan = async (id: string, known: Set<string>) => {
    let found: Operation[] = [];
    await expect
      .poll(
        async () => {
          checkLeases();
          checkLeaseHasNoHistory(id);
          const status = await settings(id);
          if (status.automatic_running) evidence[`automatic_running_seen_${id}`] = true;
          found = (await scans(id)).filter((item) => !known.has(item.id));
          return found.length > 0 && found.every((item) => terminal.test(item.state));
        },
        { timeout: 180_000, intervals: [2_000] },
      )
      .toBe(true);
    expect(found).toHaveLength(1);
    const [scan] = found;
    if (!scan) throw new Error("No automatic scan was recorded");
    return settled(scan.id);
  };

  // Polls for the whole window and fails at the first new scan or lease; the only fixed wait in this spec.
  const quiet = async (ids: string[], label: string, ms = 20_000) => {
    const before = new Map<string, string[]>();
    for (const id of ids) before.set(id, (await scans(id)).map((item) => item.id).sort());
    const deadline = Date.now() + ms;
    while (Date.now() < deadline) {
      checkLeases();
      expect(Number(sql(`SELECT count(*) FROM automatic_image_scans WHERE node_id='${node.id}'`)), `${label}: no lease`).toBe(0);
      for (const id of ids) {
        expect((await scans(id)).map((item) => item.id).sort(), `${label}: no new scan for ${id}`).toEqual(before.get(id));
        expect((await settings(id)).automatic_running, `${label}: nothing running for ${id}`).toBe(false);
      }
      await page.waitForTimeout(2_000);
    }
  };

  const findingsTotal = (image: ScannedImage) => Object.values(image.counts ?? {}).reduce((sum, count) => sum + count, 0);
  const findingSet = (image: ScannedImage) => (image.findings ?? []).map((finding) => `${finding.id}|${finding.package}|${finding.installed}|${finding.severity}`).sort();
  const checkReport = (data: Results, operationId: string, origin: string, service: string) => {
    expect(data.operation_id).toBe(operationId);
    expect(data.operation_state).toBe("succeeded");
    expect(data.origin).toBe(origin);
    expect(data).toMatchObject({ state: "complete", stale: false });
    const report = data.result;
    if (!report) throw new Error("The scan recorded no report");
    expect(report.scanner_version).toMatch(/^\d+\.\d+\.\d+$/);
    expect(report.database.digest).toMatch(sha256);
    const now = Date.now();
    expect(Date.parse(report.database.updated_at)).toBeLessThanOrEqual(now);
    expect(now - Date.parse(report.database.updated_at)).toBeLessThanOrEqual(24 * 3_600_000);
    expect(Date.parse(report.database.next_update)).toBeGreaterThan(now);
    expect(report.images).toHaveLength(1);
    const image = report.images?.[0];
    if (!image) throw new Error("The report lists no image");
    expect(image.service).toBe(service);
    expect(image.image_id).toMatch(sha256);
    expect(findingsTotal(image)).toBeGreaterThan(0);
    expect(image.findings?.length ?? 0).toBeGreaterThan(0);
    for (const finding of image.findings ?? []) {
      expect(finding.id).toMatch(/^(CVE-\d{4}-\d{4,}|GHSA-[a-z0-9-]+|[A-Z][A-Z0-9-]{1,20}-[A-Za-z0-9.-]+)$/);
      expect(["CRITICAL", "HIGH", "MEDIUM", "LOW", "UNKNOWN"]).toContain(finding.severity);
    }
    return { report, image };
  };

  const containers = (id: string, service: string) => {
    const ids = docker("ps", "-aq", "--filter", `label=com.docker.compose.project=cove-${id}`, "--filter", `label=com.docker.compose.service=${service}`).split(/\s+/).filter(Boolean);
    expect(ids).toHaveLength(1);
    const [identity = "", imageId = "", started = "", finishedAt = "", restarts = "", status = "", exit = ""] = docker("inspect", "--format", "{{.Id}}|{{.Image}}|{{.State.StartedAt}}|{{.State.FinishedAt}}|{{.RestartCount}}|{{.State.Status}}|{{.State.ExitCode}}", ids[0] ?? "").split("|");
    return { id: identity, imageId, started, finished: finishedAt, restarts, status, exit };
  };
  const confined = ["--read-only", "--cap-drop", "ALL", "--security-opt", "no-new-privileges:true", "--memory", "128m", "--cpus", "1", "--pids-limit", "32"];
  // Read from the job's volume in a throwaway container of the cached image, so the job's own container never starts.
  const runCount = (id: string) => {
    const volume = docker("volume", "ls", "-q", "--filter", `label=com.docker.compose.project=cove-${id}`, "--filter", "label=com.docker.compose.volume=runs");
    if (!volume) throw new Error("The job's runs volume is missing");
    return docker("run", "--rm", "--pull", "never", "--network", "none", ...confined, "--label", `cove.browser-fixture=${owner}`, "--label", specLabel, "--entrypoint", "/bin/cat", "--mount", `type=volume,source=${volume},target=/runs,readonly`, registryImage, "/runs/count").split(/\r?\n/).length;
  };

  const project = (await (await page.request.post(panelPath("/api/projects"), { headers, data: { name: "Automatic scan project" } })).json()) as { id: string };
  const environmentReply = await page.request.post(panelPath(`/api/projects/${project.id}/environments`), { headers, data: { name: "automatic-scans" } });
  expect(environmentReply.status(), await environmentReply.text()).toBe(201);
  const environment = (await environmentReply.json()) as { id: string };
  const create = async (name: string, kind: "service" | "job", compose: string) => {
    const created = await page.request.post(panelPath("/api/workloads"), { headers, data: { name, node_id: node.id, kind, compose, project_id: project.id, environment_id: environment.id, ...(kind === "job" ? { job_service: "task" } : {}) } });
    expect(created.status(), redact(await created.text())).toBe(201);
    const { id } = (await created.json()) as { id: string };
    expect(id).toMatch(id32);
    owned.workloads.push(id);
    // Background image checks are deferred so only scans and this spec's operations run.
    expect(returnedIds(sql(`UPDATE workloads SET next_image_check_at=now()+interval '1 day' WHERE id='${id}' RETURNING id`))).toEqual([id]);
    return id;
  };
  const app = await create("auto-scan-app", "service", serviceCompose(registryImage));
  const job = await create("auto-scan-job", "job", jobCompose(registryImage));
  evidence.resources = { app, job };

  const section = page.getByRole("region", { name: "Vulnerability scans", exact: true });
  const scanButton = section.getByRole("button", { name: "Scan for vulnerabilities", exact: true });
  const enableSwitch = section.getByRole("switch", { name: "Allow vulnerability scans", exact: true });
  const automaticSwitch = section.getByRole("switch", { name: "Automatic scans", exact: true });
  const saveButton = section.getByRole("button", { name: "Save scanning settings", exact: true });
  const latestSource = definition(section, "Source");
  const settingsPut = () => page.waitForResponse((response) => new URL(response.url()).pathname === panelPath(`/api/workloads/${app}/image-scanning`) && response.request().method() === "PUT");

  // 1. Automatic scans are off by default and an ordinary deploy with scans off queues nothing.
  const initial = await settings(app);
  expect(initial).toMatchObject({ enabled: false, notify: false, automatic: false, automatic_supported: true, automatic_running: false, can_manage: true, worker_supported: true, node_ready: true });
  expect(initial.next_automatic_check_at).toBeNull();
  await deploy(app);
  await quiet([app], "scans off after deploy");
  expect(await scans(app)).toEqual([]);
  await page.goto(panelPath(`/resources/${app}/registry`));
  await expect(section.getByText("No completed scan", { exact: true })).toBeVisible({ timeout: 30_000 });
  await expect(enableSwitch).not.toBeChecked();
  await expect(automaticSwitch).not.toBeChecked();
  await capture(page, testInfo, "automatic-scans-off");

  await enableSwitch.focus();
  await page.keyboard.press("Space");
  await expect(automaticSwitch).toBeEnabled();
  await automaticSwitch.focus();
  await page.keyboard.press("Space");
  await expect(automaticSwitch).toBeChecked();
  const enabling = settingsPut();
  await saveButton.focus();
  await page.keyboard.press("Enter");
  const dialog = page.getByRole("dialog", { name: "Allow vulnerability scans on ready-browser-worker?" });
  await expect(dialog).toContainText("every resource of this team on ready-browser-worker, not only auto-scan-app");
  await expect(dialog).toContainText("Automatic scans start without anyone queuing them");
  await expect(dialog).toContainText("results do not block deploys");
  await dialog.getByRole("button", { name: "Allow scans", exact: true }).focus();
  await page.keyboard.press("Enter");
  const enabledReply = await enabling;
  expect(enabledReply.status(), await enabledReply.text()).toBe(200);
  expect(enabledReply.request().postDataJSON()).toEqual({ enabled: true, notify: false, automatic: true });
  await expect(section.getByText("Scanning settings saved", { exact: true })).toBeVisible();
  expect(await settings(app)).toMatchObject({ enabled: true, notify: false, automatic: true });
  expect(await settings(job)).toMatchObject({ enabled: true, notify: false, automatic: true });

  // The deployed service's retained image is scanned after the deploy, without deploying, restarting or replacing it.
  const appBefore = containers(app, "app");
  expect(appBefore).toMatchObject({ imageId: registryImage, status: "running" });
  const postDeploy = await automaticScan(app, new Set());
  const { report: firstReport, image: firstImage } = checkReport(await results(app), postDeploy.id, "post_deploy", "app");
  expect(firstReport.scanner_image).toBe(initial.scanner_image);
  expect(firstImage.image_id).toBe(appBefore.imageId);
  expect(containers(app, "app")).toEqual(appBefore);
  evidence.post_deploy = { operation: postDeploy.id, database: firstReport.database.digest };
  await page.goto(panelPath(`/resources/${app}/registry`));
  await expect(section.getByText(/^Scan completed\. \d+ known vulnerabilit(y|ies) in scanned packages\.$/)).toBeVisible({ timeout: 30_000 });
  await expect(latestSource).toHaveText(scanOriginLabel("post_deploy"));
  await expect(section.getByRole("table", { name: "Findings for app" })).toContainText(firstImage.findings?.[0]?.id ?? "missing finding");
  await expect(enableSwitch).toBeChecked();
  await expect(automaticSwitch).toBeChecked();
  await capture(page, testInfo, "automatic-scans-enabled");

  // The manual scan request is unchanged while automatic scans are on.
  await expect(scanButton).toBeEnabled({ timeout: 30_000 });

  // A completed job run's retained task container is scanned without running the job again.
  const jobRun = await deploy(job);
  const jobBefore = containers(job, "task");
  expect(jobBefore).toMatchObject({ imageId: registryImage, status: "exited", exit: "0" });
  expect(runCount(job)).toBe(1);
  const jobPostRun = await automaticScan(job, new Set());
  const { report: jobFirstReport, image: jobFirstImage } = checkReport(await results(job), jobPostRun.id, "post_deploy", "task");
  expect(jobFirstImage.image_id).toBe(jobBefore.imageId);
  expect(containers(job, "task")).toEqual(jobBefore);
  expect(runCount(job)).toBe(1);
  expect((await operations(job)).filter((item) => item.action === "deploy").map((item) => item.id)).toEqual([jobRun.id]);
  evidence.job_post_run = { operation: jobPostRun.id, database: jobFirstReport.database.digest };

  // 2. A fresh, successful redeploy of the same image has a current result, so its due check queues nothing.
  setDue([job], "now()+interval '1 day'");
  await deploy(app);
  setDue([app], "now()");
  await waitChecked(app);
  await quiet([app, job], "same image redeploy");
  expect((await scans(app)).map((item) => item.id)).toEqual([postDeploy.id]);
  expect(await settings(app)).toMatchObject({ automatic_message: "" });

  // 3. A changed database generation rescans both resources, one lease per node at a time, with the same real findings.
  const digest = cacheFixture("rotate");
  expect(digest).toMatch(sha256);
  expect(digest).not.toBe(firstReport.database.digest);
  evidence.rotated_database = { digest, note: fixtureNote };
  await testInfo.attach("database-generation-fixture.txt", { body: `${fixtureNote}\nNew bundle digest: ${digest}\n`, contentType: "text/plain" });
  const appKnown = new Set([postDeploy.id]);
  const jobKnown = new Set([jobPostRun.id]);
  setDue([app, job], "now()");
  const [appRescan, jobRescan] = await Promise.all([automaticScan(app, appKnown), automaticScan(job, jobKnown)]);
  // Both retained images were covered before, so a rescan caused by the database generation change is scheduled, even
  // for the service that was redeployed with the same image in step 2.
  const { report: appRescanReport, image: appRescanImage } = checkReport(await results(app), appRescan.id, "scheduled", "app");
  expect(appRescanReport.database.digest).toBe(digest);
  expect(appRescanImage.image_id).toBe(firstImage.image_id);
  expect(findingSet(appRescanImage)).toEqual(findingSet(firstImage));
  const { report: jobRescanReport, image: jobRescanImage } = checkReport(await results(job), jobRescan.id, "scheduled", "task");
  expect(jobRescanReport.database.digest).toBe(digest);
  expect(jobRescanImage.image_id).toBe(jobBefore.imageId);
  expect(findingSet(jobRescanImage)).toEqual(findingSet(jobFirstImage));
  evidence.rescans = { app: { operation: appRescan.id, origin: "scheduled" }, job: { operation: jobRescan.id, origin: "scheduled" } };
  expect(runCount(job)).toBe(1);
  expect(containers(job, "task")).toEqual(jobBefore);
  await page.goto(panelPath(`/resources/${job}/registry`));
  await expect(latestSource).toHaveText(scanOriginLabel("scheduled"), { timeout: 30_000 });
  await expect(section.getByText(digest, { exact: true })).toBeVisible();
  await capture(page, testInfo, "automatic-scans-scheduled");

  // The same generation due again does not storm.
  setDue([app, job], "now()");
  await waitChecked(app);
  await waitChecked(job);
  await quiet([app, job], "same generation due again");

  // 4. A missing database holds its refresh cooldown: coverage is unknown, nothing is queued and nothing is fetched.
  cacheFixture("missing");
  owned.missing = true;
  setDue([app, job], "now()");
  await expect.poll(async () => (await settings(app)).automatic_message, { timeout: 60_000, intervals: [2_000] }).toMatch(/^Vulnerability database coverage is unknown\./);
  await quiet([app, job], "missing database");
  const missingStatus = await settings(app);
  evidence.missing_database_status = { automatic_message: missingStatus.automatic_message, automatic_running: missingStatus.automatic_running };
  await page.goto(panelPath(`/resources/${app}/registry`));
  await expect(section.getByText(missingStatus.automatic_message, { exact: true })).toBeVisible({ timeout: 30_000 });
  await expect(section.getByText(/no known vulnerabilities/i)).toHaveCount(0);
  await expect(automaticSwitch).toBeChecked();
  await capture(page, testInfo, "automatic-scans-missing-database");
  cacheFixture("restore");
  owned.missing = false;

  // 5. Turning only automatic scans off needs no confirmation, and a changed image is then scanned only on request.
  await page.goto(panelPath(`/resources/${app}/registry`));
  await expect(automaticSwitch).toBeChecked({ timeout: 30_000 });
  await automaticSwitch.focus();
  await page.keyboard.press("Space");
  await expect(automaticSwitch).not.toBeChecked();
  const disabling = settingsPut();
  await saveButton.focus();
  await page.keyboard.press("Enter");
  await expect(page.getByRole("dialog")).toHaveCount(0);
  const disabledReply = await disabling;
  expect(disabledReply.status(), await disabledReply.text()).toBe(200);
  expect(disabledReply.request().postDataJSON()).toEqual({ enabled: true, notify: false, automatic: false });
  await expect(section.getByText("Scanning settings saved", { exact: true })).toBeVisible();
  expect(await settings(app)).toMatchObject({ enabled: true, automatic: false });

  // The variant needs only a distinct image ID, which the committed labels give it (an image ID is its config digest),
  // so the seed writes nothing and runs read-only.
  owned.seed = `${owner}-automatic-scans-seed`;
  docker("run", "--name", owned.seed, "--pull", "never", "--network", "none", ...confined, "--label", `cove.browser-fixture=${owner}`, "--label", specLabel, "--entrypoint", "/bin/sh", registryImage, "-c", "exit 0");
  const variant = `${owner}-automatic-scans-variant:fixture`;
  owned.images.push(variant);
  docker("commit", "--change", `LABEL cove.browser-fixture=${owner}`, "--change", `LABEL ${specLabel}`, owned.seed, variant);
  docker("rm", owned.seed);
  owned.seed = "";
  const updated = await page.request.put(panelPath(`/api/workloads/${app}`), { headers, data: { name: "auto-scan-app", node_id: node.id, kind: "service", compose: serviceCompose(variant) } });
  expect([200, 202], redact(await updated.text())).toContain(updated.status());
  await deploy(app);
  const appVariant = containers(app, "app");
  expect(appVariant.imageId).not.toBe(registryImage);
  expect(appVariant.status).toBe("running");
  setDue([app], "now()");
  await quiet([app, job], "automatic scans off");
  expect(sql(`SELECT next_check_at<=now() FROM image_scan_automation_state WHERE workload_id='${app}'`)).toBe("t");

  await page.goto(panelPath(`/resources/${app}/registry`));
  await expect(scanButton).toBeEnabled({ timeout: 30_000 });
  const queued = page.waitForResponse((response) => new URL(response.url()).pathname === panelPath(`/api/workloads/${app}/operations`) && response.request().method() === "POST");
  await scanButton.focus();
  await page.keyboard.press("Enter");
  const queuedReply = await queued;
  expect(queuedReply.status(), await queuedReply.text()).toBe(202);
  expect(queuedReply.request().postDataJSON()).toEqual({ action: "image_scan" });
  const manualId = ((await queuedReply.json()) as { id: string }).id;
  await expect(section.getByText("Vulnerability scan queued", { exact: true })).toBeVisible();
  await settled(manualId);
  const { image: manualImage } = checkReport(await results(app), manualId, "manual", "app");
  expect(manualImage.image_id).toBe(appVariant.imageId);
  expect(containers(app, "app")).toEqual(appVariant);
  await page.goto(panelPath(`/resources/${app}/registry`));
  await expect(latestSource).toHaveText(scanOriginLabel("manual"), { timeout: 30_000 });
  await expect(enableSwitch).toBeChecked();
  await expect(automaticSwitch).not.toBeChecked();
  await capture(page, testInfo, "automatic-scans-disabled-manual");

  // Totals: two automatic scans and one manual scan for the service, two automatic scans for the job, no lease left.
  const appActions = (await operations(app)).map((item) => item.action);
  const jobActions = (await operations(job)).map((item) => item.action);
  expect(appActions.filter((action) => action === "image_scan")).toHaveLength(3);
  expect(appActions.filter((action) => action === "deploy")).toHaveLength(3);
  expect(jobActions.filter((action) => action === "image_scan")).toHaveLength(2);
  expect(jobActions.filter((action) => action === "deploy")).toHaveLength(1);
  checkLeases();
  expect(Number(sql(`SELECT count(*) FROM automatic_image_scans WHERE node_id='${node.id}'`))).toBe(0);
  evidence.totals = { app: appActions, job: jobActions };

  // 6. The operation page names how each scan started, with the same labels as the Registry page.
  const operationSource = definition(page.locator("main"), "Scan source");
  for (const [id, origin] of [
    [postDeploy.id, "post_deploy"],
    [jobPostRun.id, "post_deploy"],
    [manualId, "manual"],
    [appRescan.id, "scheduled"],
    [jobRescan.id, "scheduled"],
  ] as const) {
    await page.goto(panelPath(`/operations/${id}`));
    await expect(operationSource).toHaveText(scanOriginLabel(origin), { timeout: 30_000 });
  }
  await capture(page, testInfo, "automatic-scans-operation");
});

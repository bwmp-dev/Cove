import { execFileSync } from "node:child_process";
import { expect, test, type Page, type TestInfo } from "@playwright/test";
import { cleanupBrowserResources, expectNoHorizontalScroll } from "./route-tls";

// Runs only with scripts/test-browser.py --image-scans, which seeds a fresh main database before the worker starts, sets
// COVE_IMAGE_SCANNING=1 and supplies the cached scanner at its release pin, so no refresh runs. It also supplies the
// verified official Distribution 2.8.3 image (never pulled), which carries real Go packages with real advisories, so the
// report is real scanner output. Nothing is mocked or intercepted.
const password = process.env.COVE_BROWSER_PASSWORD ?? "";
const enabled = process.env.COVE_BROWSER_IMAGE_SCANS === "1";

test.use({ actionTimeout: 30_000, navigationTimeout: 60_000 });

const docker = (...args: string[]) => execFileSync("docker", args, { encoding: "utf8", stdio: ["ignore", "pipe", "pipe"] }).trim();
const specKey = "cove.image-scans-spec";
const specLabel = `${specKey}=1`;

// What this spec created on the Docker host outside the API. Each item carries the fixture label as well, so the
// harness removes it even if this cleanup does not run.
const owned = { fixture: "", seed: "", images: [] as string[] };

function ownedBySpec(kind: "container" | "image", name: string) {
  try {
    return docker(kind, "inspect", "--format", `{{index .Config.Labels "${specKey}"}}|{{index .Config.Labels "cove.browser-fixture"}}`, name) === `1|${owned.fixture}`;
  } catch {
    return false;
  }
}

test.afterEach(() => {
  const testInfo = test.info();
  const problems: string[] = [];
  const attempt = (step: () => void) => {
    try {
      step();
    } catch (error) {
      problems.push(error instanceof Error ? error.message : String(error));
    }
  };
  attempt(cleanupBrowserResources);
  if (owned.seed && ownedBySpec("container", owned.seed)) attempt(() => docker("rm", "-f", "-v", owned.seed));
  for (const image of [...owned.images].reverse()) if (ownedBySpec("image", image)) attempt(() => docker("image", "rm", image));
  Object.assign(owned, { fixture: "", seed: "", images: [] });
  if (problems.length > 0 && testInfo.status === testInfo.expectedStatus) throw new Error(`Cleanup failed: ${problems.join("; ")}`);
});

function fixtureOwner() {
  const owner = /^(cove-browser-[a-f0-9]{12})-worker$/.exec(process.env.COVE_BROWSER_WORKER ?? "")?.[1];
  const database = `${owner}-db`;
  if (!owner || process.env.COVE_BROWSER_DATABASE !== database) throw new Error("Isolated browser fixture required");
  return { owner, database };
}

const sizes = [320, 390, 768, 1280, 1440, 1920];

async function capture(page: Page, testInfo: TestInfo, name: string) {
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
  can_manage: boolean;
  worker_supported: boolean;
  node_ready: boolean;
  active_operation_id: string | null;
  scanner_image: string;
}

interface Finding {
  id: string;
  package: string;
  installed: string;
  fixed?: string;
  severity: string;
}

interface ScannedImage {
  service: string;
  image_id: string;
  config_digest: string;
  state: string;
  counts: Record<string, number> | null;
  findings: Finding[] | null;
  truncated: boolean;
  coverage: { os: string; other_languages: string; java_archives: string; java_message?: string };
}

interface Results {
  state: string;
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

// The fixture image carries only real packages; pull_policy never keeps it on the cached, verified image.
const compose = (image: string) =>
  [
    "services:",
    "  app:",
    `    image: ${image}`,
    "    pull_policy: never",
    "    entrypoint: [/bin/sh]",
    "    command: [-c, exec sleep 86400]",
    "",
  ].join("\n");

const terminal = /^(succeeded|failed|cancelled|interrupted)$/;
const sha256 = /^sha256:[a-f0-9]{64}$/;
// Keys that would mean raw scanner JSON, image configuration or secret output reached the API.
const forbiddenKeys = /"(password|token|secret|secrets|env|environment|labels|cmd|entrypoint|config|history|description|raw|Results|Metadata)"\s*:/;

test("image scans: an opted-in team scans a running service's retained image and reports real findings without touching it", async ({ page }, testInfo) => {
  test.skip(!enabled, "Needs scripts/test-browser.py --image-scans");
  // The database is preseeded and each offline scan takes seconds, so a stuck locator or scan fails quickly.
  test.setTimeout(600_000);
  const { owner, database } = fixtureOwner();
  owned.fixture = owner;
  const registryImage = process.env.COVE_BROWSER_REGISTRY_IMAGE ?? "";
  if (!sha256.test(registryImage)) throw new Error("The harness must provide the verified local registry image");
  expect(docker("image", "inspect", "--format", "{{.Id}}", registryImage)).toBe(registryImage);
  const sql = (query: string) => docker("exec", database, "psql", "-U", "postgres", "-d", "cove", "-Atc", query);

  await page.goto("/projects");
  await expect(page).toHaveURL(/\/login(?:[?#]|$)/);
  await page.getByLabel("Password", { exact: true }).fill(password);
  await page.getByRole("button", { name: "Sign in", exact: true }).click();
  await expect(page).not.toHaveURL(/\/login(?:[?#]|$)/);
  const headers = { Origin: new URL(page.url()).origin };

  const nodes = (await (await page.request.get("/api/nodes")).json()) as { id: string; name: string; capabilities: { image_scanning?: boolean } | null }[];
  const node = nodes.find((item) => item.name === "ready-browser-worker");
  if (!node) throw new Error("The browser worker is missing");
  expect(node.capabilities?.image_scanning).toBe(true);

  // A second image with a Java archive, committed from the same cached image and labelled as this spec's. The archive
  // is a valid empty ZIP; only its presence in a layer matters.
  owned.seed = `${owner}-image-scans-seed`;
  docker(
    "run", "--name", owned.seed, "--pull", "never", "--network", "none",
    "--label", `cove.browser-fixture=${owner}`, "--label", specLabel,
    "--entrypoint", "/bin/sh", registryImage,
    "-c", "mkdir -p /opt/fixture && printf 'PK\\005\\006' > /opt/fixture/fixture.jar && head -c 18 /dev/zero >> /opt/fixture/fixture.jar",
  );
  const javaImage = `${owner}-image-scans-java:fixture`;
  owned.images.push(javaImage);
  docker("commit", "--change", `LABEL cove.browser-fixture=${owner}`, "--change", `LABEL ${specLabel}`, owned.seed, javaImage);
  docker("rm", owned.seed);
  owned.seed = "";

  const project = (await (await page.request.post("/api/projects", { headers, data: { name: "Image scan project" } })).json()) as { id: string };
  const create = async (name: string, image: string) => {
    const created = await page.request.post("/api/workloads", { headers, data: { name, node_id: node.id, kind: "service", compose: compose(image), project_id: project.id } });
    expect(created.status(), await created.text()).toBe(201);
    const { id } = (await created.json()) as { id: string };
    expect(id).toMatch(/^[a-f0-9]{32}$/);
    // Background image checks are deferred so only this spec's operations run.
    expect(sql(`UPDATE workloads SET next_image_check_at=now()+interval '1 day' WHERE id='${id}' RETURNING id`).split(/\s+/).filter((value) => value === id)).toEqual([id]);
    return id;
  };
  const go = await create("image-scans", registryImage);
  const java = await create("image-scans-java", javaImage);

  const operations = async (id: string) => (await (await page.request.get(`/api/operations?workload_id=${id}`)).json()) as Operation[];
  const operation = async (id: string) => (await (await page.request.get(`/api/operations/${id}`)).json()) as Operation;
  const finished = async (id: string, timeout = 240_000) => {
    await expect.poll(async () => (await operation(id)).state, { timeout, intervals: [3_000] }).toMatch(terminal);
    return operation(id);
  };
  const read = async <T>(path: string) => {
    const response = await page.request.get(path);
    const text = await response.text();
    expect(response.status(), text).toBe(200);
    expect(text).not.toMatch(forbiddenKeys);
    return JSON.parse(text) as T;
  };
  const container = (id: string) => {
    const ids = docker("ps", "-aq", "--filter", `label=com.docker.compose.project=cove-${id}`, "--filter", "label=com.docker.compose.service=app").split(/\s+/).filter(Boolean);
    expect(ids).toHaveLength(1);
    const [identity = "", imageId = "", configured = "", started = "", finishedAt = "", restarts = "", status = ""] = docker("inspect", "--format", "{{.Id}}|{{.Image}}|{{.Config.Image}}|{{.State.StartedAt}}|{{.State.FinishedAt}}|{{.RestartCount}}|{{.State.Status}}", ids[0] ?? "").split("|");
    return { id: identity, imageId, configured, started, finished: finishedAt, restarts, status };
  };

  for (const id of [go, java]) {
    const deploy = (await (await page.request.post(`/api/workloads/${id}/operations`, { headers, data: { action: "deploy" } })).json()) as { id: string };
    const deployed = await finished(deploy.id);
    expect(deployed.state, deployed.result?.error).toBe("succeeded");
  }
  const before = { go: container(go), java: container(java) };
  expect(before.go).toMatchObject({ imageId: registryImage, status: "running" });
  expect(before.java.status).toBe("running");

  const section = page.getByRole("region", { name: "Vulnerability scans", exact: true });
  const scanButton = section.getByRole("button", { name: "Scan for vulnerabilities", exact: true });

  // 1. Scans are off by default and the team/node setting is enabled through the confirmation, with the exact body.
  await page.goto(`/resources/${go}/registry`);
  await expect(section.getByText("No completed scan", { exact: true })).toBeVisible({ timeout: 30_000 });
  await expect(scanButton).toBeDisabled();
  const initial = await read<Settings>(`/api/workloads/${go}/image-scanning`);
  expect(initial).toMatchObject({ enabled: false, notify: false, can_manage: true, worker_supported: true, node_ready: true });
  expect(initial.scanner_image).toMatch(/@sha256:[a-f0-9]{64}$/);
  await section.getByRole("switch", { name: "Allow vulnerability scans", exact: true }).focus();
  await page.keyboard.press("Space");
  await capture(page, testInfo, "image-scans-settings");
  const put = page.waitForResponse((response) => new URL(response.url()).pathname === `/api/workloads/${go}/image-scanning` && response.request().method() === "PUT");
  await section.getByRole("button", { name: "Save scanning settings", exact: true }).focus();
  await page.keyboard.press("Enter");
  const dialog = page.getByRole("dialog", { name: "Allow vulnerability scans on ready-browser-worker?" });
  await expect(dialog).toContainText("every resource of this team on ready-browser-worker, not only image-scans");
  await expect(dialog).toContainText("results do not block deploys");
  await dialog.getByRole("button", { name: "Allow scans", exact: true }).focus();
  await page.keyboard.press("Enter");
  const saved = await put;
  expect(saved.status(), await saved.text()).toBe(200);
  expect(saved.request().postDataJSON()).toEqual({ enabled: true, notify: false, automatic: false });
  await expect(section.getByText("Scanning settings saved", { exact: true })).toBeVisible();

  // Queues exactly one image_scan from the keyboard and waits for its terminal state.
  const scan = async (id: string) => {
    await page.goto(`/resources/${id}/registry`);
    await expect(scanButton).toBeEnabled({ timeout: 30_000 });
    const queued = page.waitForResponse((response) => new URL(response.url()).pathname === `/api/workloads/${id}/operations` && response.request().method() === "POST");
    await scanButton.focus();
    await page.keyboard.press("Enter");
    const response = await queued;
    expect(response.status(), await response.text()).toBe(202);
    expect(response.request().postDataJSON()).toEqual({ action: "image_scan" });
    const { id: operationId } = (await response.json()) as { id: string };
    await expect(section.getByText("Vulnerability scan queued", { exact: true })).toBeVisible();
    const result = await finished(operationId, 180_000);
    expect(result.state, result.result?.error).toBe("succeeded");
    return operationId;
  };
  const findings = (image: ScannedImage) => Object.values(image.counts ?? {}).reduce((sum, count) => sum + count, 0);
  const checkReport = (results: Results, operationId: string) => {
    expect(results.operation_id).toBe(operationId);
    expect(results.operation_state).toBe("succeeded");
    const report = results.result;
    if (!report) throw new Error("The scan recorded no report");
    expect(report.scanner_image).toBe(initial.scanner_image);
    expect(report.scanner_version).toMatch(/^\d+\.\d+\.\d+$/);
    expect(report.database.digest).toMatch(sha256);
    const now = Date.now();
    expect(Date.parse(report.database.updated_at)).toBeLessThanOrEqual(now);
    expect(now - Date.parse(report.database.updated_at)).toBeLessThanOrEqual(24 * 3_600_000);
    expect(Date.parse(report.database.next_update)).toBeGreaterThan(now);
    expect(report.images).toHaveLength(1);
    const image = report.images?.[0];
    if (!image) throw new Error("The report lists no image");
    expect(image.service).toBe("app");
    expect(image.image_id).toMatch(sha256);
    expect(image.config_digest).toMatch(sha256);
    expect(findings(image)).toBeGreaterThan(0);
    expect(image.findings?.length ?? 0).toBeGreaterThan(0);
    for (const finding of image.findings ?? []) {
      expect(finding.id).toMatch(/^(CVE-\d{4}-\d{4,}|GHSA-[a-z0-9-]+|[A-Z][A-Z0-9-]{1,20}-[A-Za-z0-9.-]+)$/);
      expect(["CRITICAL", "HIGH", "MEDIUM", "LOW", "UNKNOWN"]).toContain(finding.severity);
    }
    return { report, image };
  };

  // 2. The Go image: a real, complete report with positive findings, the scanner pin and database metadata.
  const goScan = await scan(go);
  const goResults = await read<Results>(`/api/workloads/${go}/image-scans`);
  const { report: goReport, image: goImage } = checkReport(goResults, goScan);
  expect(goImage.image_id).toBe(before.go.imageId);
  expect(goImage.coverage.java_archives).toBe("not_detected");
  expect(goImage.coverage.other_languages).toBe("scanned");
  expect(goResults).toMatchObject({ state: "complete", stale: false });
  await page.goto(`/resources/${go}/registry`);
  await expect(section.getByText(/^Scan completed\. \d+ known vulnerabilit(y|ies) in scanned packages\.$/)).toBeVisible({ timeout: 30_000 });
  await expect(section.getByText(goReport.scanner_image, { exact: true }).first()).toBeVisible();
  await expect(section.getByText(goReport.database.digest, { exact: true })).toBeVisible();
  await expect(section.getByRole("table", { name: "Findings for app" })).toContainText(goImage.findings?.[0]?.id ?? "missing finding");
  await capture(page, testInfo, "image-scans-result");

  // 3. The Java image: OS and language findings are kept, but Java archive coverage is not scanned, so it stays unknown.
  const javaScan = await scan(java);
  const javaResults = await read<Results>(`/api/workloads/${java}/image-scans`);
  const { image: javaImageResult } = checkReport(javaResults, javaScan);
  expect(javaImageResult.image_id).toBe(before.java.imageId);
  expect(javaImageResult.coverage.java_archives).toBe("not_scanned");
  expect(javaImageResult.coverage.java_message ?? "").toMatch(/JAR, WAR or EAR/);
  expect(javaImageResult.state).toBe("unknown");
  expect(javaResults.state).toBe("unknown");
  await page.goto(`/resources/${java}/registry`);
  await expect(section.getByLabel("Coverage for app", { exact: true })).toContainText("Not scanned (unknown)", { timeout: 30_000 });
  await expect(section.getByText(/Java archive \(JAR, WAR or EAR\) dependencies were not scanned in 1 image/)).toBeVisible();
  await expect(section.getByText(/^Scan completed\./)).toHaveCount(0);
  await expect(section.getByText(/no known vulnerabilities/i)).toHaveCount(0);
  await capture(page, testInfo, "image-scans-java");

  // 4. Turning the setting off through the UI blocks the button and the API refuses a new scan.
  await page.goto(`/resources/${go}/registry`);
  await section.getByRole("switch", { name: "Allow vulnerability scans", exact: true }).focus();
  await page.keyboard.press("Space");
  const off = page.waitForResponse((response) => new URL(response.url()).pathname === `/api/workloads/${go}/image-scanning` && response.request().method() === "PUT");
  await section.getByRole("button", { name: "Save scanning settings", exact: true }).focus();
  await page.keyboard.press("Enter");
  await expect(page.getByRole("dialog")).toHaveCount(0);
  const disabled = await off;
  expect(disabled.status(), await disabled.text()).toBe(200);
  expect(disabled.request().postDataJSON()).toEqual({ enabled: false, notify: false, automatic: false });
  await expect(scanButton).toBeDisabled();
  const refused = await page.request.post(`/api/workloads/${go}/operations`, { headers, data: { action: "image_scan" } });
  expect(refused.status(), await refused.text()).toBe(409);
  await capture(page, testInfo, "image-scans-disabled");

  // Scans neither deployed, restarted nor replaced anything.
  expect(container(go)).toEqual(before.go);
  expect(container(java)).toEqual(before.java);
  for (const id of [go, java]) {
    const actions = (await operations(id)).map((item) => item.action);
    expect(actions.filter((action) => action === "deploy")).toHaveLength(1);
    expect(actions.filter((action) => action === "image_scan")).toHaveLength(1);
  }
});

import { execFileSync } from "node:child_process";
import { createCipheriv, randomBytes } from "node:crypto";
import { readFileSync } from "node:fs";
import { isAbsolute } from "node:path";
import { expect, test, type Locator, type Page, type TestInfo } from "@playwright/test";
import type { Operation as ProductOperation } from "../src/api/types";
import { recordedEngine, recordedPhaseMemory } from "../src/lib/build-recipes";
import { workerResult } from "../src/lib/operations";
import { cleanupBrowserResources, expectNoHorizontalScroll } from "./route-tls";

// Runs only with scripts/test-browser.py --managed-registry --buildpacks on the caller's owned nested daemon. The
// fixture replaces git on the one ready worker with a synthetic checkout of exactly
// https://git.fixture.invalid/build-recipes.git on main holding a precompiled Go HTTP server and a Procfile; no Git
// provider is contacted and nothing in the source downloads a language toolchain. The Cloud Native Buildpacks build is
// real: the worker shares its nested daemon's process, cgroup and storage view, admits the build from the memory it
// actually observes, runs the cached pinned lifecycle and builder, and pushes to the real authenticated managed
// registry over HTTPS verified against the fixture CA. No request in this spec is mocked and TLS is never skipped.
//
// Limit: the deployed app runs under its saved Compose like any other image. This spec does not claim that
// Cloud Native Buildpacks supplies static-output runtime hardening.
//
// Exactly one of the two tests runs, chosen by the admission the worker actually reports: the build test needs a node
// that meets the unchanged gate (4 GiB available memory, 24 GiB Docker storage, 10 GiB worker state), and the refusal
// test needs one that does not. Whichever does not apply is skipped with the observed figures, never silently.
const base = process.env.COVE_BROWSER_BASE_PATH ?? "";
const panelPath = (route: string) => `${base}${route}`;
const password = process.env.COVE_BROWSER_PASSWORD ?? "";
const enabled = process.env.COVE_BROWSER_BUILDPACKS === "1" && process.env.COVE_BROWSER_BUILD_RECIPES === "1" && process.env.COVE_BROWSER_MANAGED_REGISTRY === "1";

test.use({ actionTimeout: 30_000, navigationTimeout: 60_000 });

const docker = (...args: string[]) => execFileSync("docker", args, { encoding: "utf8", stdio: ["ignore", "pipe", "pipe"] }).trim();
const sizes = [320, 390, 768, 1280, 1440, 1920];
const tlsFailure = "Registry deployment finished, but authenticated TLS verification failed. Confirm DNS and the HTTPS certificate, then retry.";
const finished = /^(succeeded|failed|cancelled|interrupted)$/;
const resourceId = /^[a-f0-9]{32}$/;
const repositoryUrl = "https://git.fixture.invalid/build-recipes.git";
const source = { repository: repositoryUrl, branch: "main", compose_path: "", inline_compose: true };
const repository = "fixture/app";
const revision = "a".repeat(40);
const savedNotice = "Build recipe saved. The next build and publish uses it; the running release is unchanged.";
const marker = "cnb-offline-fixture";
const phases = ["analyzer", "detector", "restorer", "builder", "exporter"];
const mib = 1024 ** 2;
const gib = 1024 ** 3;

// The fixture's names, inferred from the worker the harness exports and checked against its database, its worker
// container and the owned nested daemon.
function fixture() {
  const owner = /^cove-browser-([a-f0-9]{12})-worker$/.exec(process.env.COVE_BROWSER_WORKER ?? "")?.[1];
  const name = `cove-browser-${owner}`;
  const database = `${name}-db`;
  if (!owner || process.env.COVE_BROWSER_DATABASE !== database) throw new Error("Isolated browser fixture required");
  const worker = `${name}-worker`;
  const daemon = process.env.COVE_BROWSER_MANAGED_DAEMON_ID ?? "";
  if (!daemon) throw new Error("The fixture must name its owned nested daemon");
  const hostname = process.env.COVE_BROWSER_MANAGED_REGISTRY_HOST ?? "";
  if (hostname !== `registry-${owner}.example.test`) throw new Error("The fixture must provide its own exact registry hostname");
  const file = (variable: string) => {
    const path = process.env[variable] ?? "";
    if (!isAbsolute(path)) throw new Error(`${variable} must be an absolute fixture path`);
    return readFileSync(path, "utf8");
  };
  file("COVE_BROWSER_MANAGED_REGISTRY_ROOT");
  return { name, database, worker, daemon, hostname, certificate: file("COVE_BROWSER_MANAGED_REGISTRY_CERT"), key: file("COVE_BROWSER_MANAGED_REGISTRY_KEY") };
}

type Operation = ProductOperation & { execution_node_id?: string };

interface Registry {
  id: string;
  state: string;
  connection_id: string;
  operation_id: string;
  operation_state: string;
  last_error: string;
  data_retained: boolean;
}

interface RegistryConnection {
  id: string;
  managed_registry_id?: string;
  registry?: { server: string; password: string };
}

interface NodeItem {
  id: string;
  name: string;
  state: string;
  capabilities: {
    build_publish?: boolean;
    build_recipes?: boolean;
    buildpacks?: boolean;
    buildpacks_reason?: string;
    buildpacks_capacity?: { memory_available: number; disk_available: number; state_available: number };
    os?: string;
  } | null;
}

interface BuildpacksRead {
  supported: boolean;
  reason: string;
  lifecycle_version: string;
  builder_version: string;
  platform_api: string;
  pinned_images: string[];
  work_storage: string;
  work_hard_cap: boolean;
  work_target_bytes: number;
  minimum_phase_memory_bytes: number;
  maximum_phase_memory_bytes: number;
  memory_safety_bytes: number;
  required_available_memory_bytes: number;
  required_docker_disk_bytes: number;
  required_worker_disk_bytes: number;
  quota_memory_mb: number;
  quota_cpu: number;
  builder_download_bytes: number;
  builder_unpacked_bytes: number;
  experimental: boolean;
}

interface RecipeSettings {
  recipe: { engine: string; service: string; directory: string; output: string; build_command: string; start_command: string };
  can_edit: boolean;
  source_ready: boolean;
  supported: boolean;
  reason: string;
  buildpacks?: BuildpacksRead;
}

const service = (lines: string[]) => ["services:", "  app:", ...lines.map((line) => `    ${line}`), ""].join("\n");
const initialCompose = service(["image: busybox:1.37", "pull_policy: never", "restart: unless-stopped", `command: [sh, -c, ${JSON.stringify("echo initial-release > /tmp/marker; exec sleep 86400")}]`]);
const deployCompose = (reference: string) => service([`image: ${JSON.stringify(reference)}`, "pull_policy: always", "restart: unless-stopped"]);
const emptyRecipe = { engine: "compose", service: "", directory: "", output: "", build_command: "", start_command: "" };
const buildpacksRecipe = { ...emptyRecipe, engine: "buildpacks", service: "app", directory: "." };

const asError = (error: unknown) => (error instanceof Error ? error : new Error("A non-error value was thrown", { cause: error }));

// Opens a Select from the keyboard and moves to the named option before choosing it with Enter.
async function chooseWithKeyboard(page: Page, combobox: Locator, option: string) {
  await combobox.focus();
  await page.keyboard.press("Space");
  const listbox = page.getByRole("listbox");
  await expect(listbox).toBeVisible();
  const target = listbox.getByRole("option", { name: option, exact: true });
  await expect(target).toHaveCount(1);
  await expect(target).not.toHaveAttribute("aria-disabled", "true");
  const targetId = await target.getAttribute("id");
  // Opening activates the selected option and ArrowDown stops at the last one, so start from the first option.
  await page.keyboard.press("Home");
  const optionCount = await listbox.getByRole("option").count();
  for (let step = 0; step < optionCount && (await combobox.getAttribute("aria-activedescendant")) !== targetId; step++) await page.keyboard.press("ArrowDown");
  await expect(combobox).toHaveAttribute("aria-activedescendant", targetId ?? "");
  await page.keyboard.press("Enter");
  await expect(combobox).toHaveText(option);
}

async function typeWithKeyboard(page: Page, field: Locator, value: string) {
  await field.focus();
  await page.keyboard.press("ControlOrMeta+A");
  await page.keyboard.press("Backspace");
  await page.keyboard.type(value);
  await expect(field).toHaveValue(value);
}

const definition = (page: Page, term: string) => page.locator("dt", { hasText: new RegExp(`^${term}$`) }).locator("xpath=following-sibling::dd[1]");

const secretMarkersFor = (key: string) => ["PRIVATE KEY", key.trim(), "COVE_REGISTRY_AUTH", "cove_registry:", "$2a$", "$2b$", "$2y$", "REGISTRY_AUTH_HTPASSWD", '"auths"'];
const noSecrets = (markers: string[]) => (text: string) => {
  for (const secret of markers) expect(text.includes(secret), `response contains secret-like material (${secret.slice(0, 20)})`).toBe(false);
};

// The worker, its owner marker and both daemon views must be the fixture's before anything runs against them.
function expectOwnedWorker({ name, worker, daemon }: { name: string; worker: string; daemon: string }) {
  expect(docker("info", "--format", "{{.ID}}")).toBe(daemon);
  expect(docker("inspect", "--format", "{{.Config.Image}}", worker)).toBe(`${worker}:development`);
  expect(docker("exec", worker, "cat", "/var/lib/cove/.browser-owner")).toBe(name);
  expect(docker("exec", worker, "docker", "info", "--format", "{{.ID}}")).toBe(daemon);
}

async function signIn(page: Page) {
  await page.goto(panelPath("/projects"));
  await expect(page).toHaveURL(/\/login(?:[?#]|$)/);
  await page.getByLabel("Password", { exact: true }).fill(password);
  await page.getByRole("button", { name: "Sign in", exact: true }).click();
  await expect(page).not.toHaveURL(/\/login(?:[?#]|$)/);
  return { Origin: new URL(page.url()).origin };
}

type Capacity = NonNullable<NonNullable<NodeItem["capabilities"]>["buildpacks_capacity"]>;

const unmetGate = (capacity: Capacity) =>
  [
    capacity.memory_available < 4 * gib && `available memory ${capacity.memory_available} bytes is below 4 GiB`,
    capacity.disk_available < 24 * gib && `Docker storage ${capacity.disk_available} bytes is below 24 GiB`,
    capacity.state_available < 10 * gib && `worker state ${capacity.state_available} bytes is below 10 GiB`,
  ].filter((item): item is string => typeof item === "string");

// Reads the one ready worker. The buildpacks capability and real capacity accounting are required either way; whether
// the node is eligible comes only from the reason and figures it reports.
async function observeBuildpacks(page: Page, expectNoSecrets: (text: string) => void) {
  const nodesReply = await page.request.get(panelPath("/api/nodes"));
  const nodesText = await nodesReply.text();
  expect(nodesReply.status(), nodesText).toBe(200);
  expectNoSecrets(nodesText);
  const nodes = JSON.parse(nodesText) as NodeItem[];
  expect(nodes.map((item) => item.name)).toEqual(["ready-browser-worker"]);
  const node = nodes[0];
  if (!node) throw new Error("The browser worker is missing");
  expect(node.state).toBe("ready");
  expect(node.capabilities?.build_publish).toBe(true);
  expect(node.capabilities?.build_recipes).toBe(true);
  expect(node.capabilities?.os).toBe("linux");
  const capacity = node.capabilities?.buildpacks_capacity;
  if (node.capabilities?.buildpacks !== true || !capacity) {
    throw new Error(`The fixture worker did not report Cloud Native Buildpacks capacity accounting: ${JSON.stringify({ buildpacks: node.capabilities?.buildpacks, reason: node.capabilities?.buildpacks_reason, capacity })}`);
  }
  const reason = node.capabilities.buildpacks_reason ?? "";
  const unmet = unmetGate(capacity);
  const summary = `ready-browser-worker reported memory_available=${capacity.memory_available} bytes, disk_available=${capacity.disk_available} bytes, state_available=${capacity.state_available} bytes, reason=${JSON.stringify(reason)}`;
  // The reason and the figures must agree; a mismatch is a defect, not a reason to pick either test.
  if (reason === "") expect(unmet, `the worker reported no refusal although ${unmet.join("; ")}`).toEqual([]);
  else expect(unmet.length, `the worker refused (${reason}) although its figures meet the gate`).toBeGreaterThan(0);
  return { node, capacity, reason, eligible: reason === "", summary };
}

function skipObserved(testInfo: TestInfo, description: string) {
  console.log(description);
  testInfo.annotations.push({ type: "observed-buildpacks-admission", description });
  test.skip(true, description);
}

test("buildpacks: a Cloud Native Buildpacks recipe builds and publishes with an admitted phase limit, deploys only on request, then Compose is restored", async ({ page }, testInfo) => {
  test.skip(!enabled, "Needs scripts/test-browser.py --managed-registry --buildpacks");
  test.setTimeout(600_000);
  const { name, database, worker, daemon, hostname, certificate, key } = fixture();
  const sql = (query: string) => docker("exec", database, "psql", "-U", "postgres", "-d", "cove", "-Atc", query);
  // Background image checks start due. Deferring them keeps every operation in this team one the spec started, because
  // recipe changes and registry removal are refused while an operation is queued or running.
  const deferImageChecks = (id: string) => {
    expect(id).toMatch(resourceId);
    expect(sql(`UPDATE workloads SET next_image_check_at=now()+interval '1 day' WHERE id='${id}' RETURNING id`).split(/\s+/).filter((value) => value === id)).toEqual([id]);
  };
  const expectNoSecrets = noSecrets(secretMarkersFor(key));
  const capture = async (label: string) => {
    expect(await page.locator("body").innerText()).not.toContain("-----BEGIN");
    for (const width of sizes) {
      await page.setViewportSize({ width, height: 900 });
      await expectNoHorizontalScroll(page);
      await page.screenshot({ path: testInfo.outputPath(`${label}-${width}.png`), fullPage: true });
    }
    await page.setViewportSize({ width: 1280, height: 900 });
  };
  const probeLabel = `dev.cove.buildpacks-probe=${name}`;

  // Nothing is created before this point, so an ineligible node skips here with nothing to clean up.
  expectOwnedWorker({ name, worker, daemon });
  const headers = await signIn(page);
  const { node, capacity, eligible, summary } = await observeBuildpacks(page, expectNoSecrets);
  if (!eligible) {
    skipObserved(testInfo, `SKIPPED packaged Cloud Native Buildpacks build: the node does not meet the unchanged admission gate, so no packaged dashboard build ran. ${summary}. Run python scripts/test-browser.py --managed-registry --buildpacks e2e/buildpacks.spec.ts on a node that meets it.`);
  }
  expect(capacity.memory_available).toBeGreaterThanOrEqual(4 * gib);
  expect(capacity.disk_available).toBeGreaterThanOrEqual(24 * gib);
  expect(capacity.state_available).toBeGreaterThanOrEqual(10 * gib);

  let registryId = "";
  let appId = "";
  let appDeleted = false;
  let registryRecordDeleted = false;
  let primary: Error | null = null;
  try {

    const operation = async (id: string) => {
      const response = await page.request.get(panelPath(`/api/operations/${id}`));
      const text = await response.text();
      expect(response.status(), text).toBe(200);
      expectNoSecrets(text);
      return JSON.parse(text) as Operation;
    };
    const operationLogs = async (id: string) => {
      const response = await page.request.get(panelPath(`/api/operations/${id}/logs`));
      const text = await response.text();
      if (response.status() !== 200) throw new Error(`log request returned ${response.status()}`);
      expectNoSecrets(text);
      return (JSON.parse(text) as { logs: string }).logs;
    };
    const settled = async (id: string, timeout = 180_000) => {
      await expect.poll(async () => (await operation(id)).state, { timeout }).toMatch(finished);
      const result = await operation(id);
      if (result.state !== "succeeded") {
        // Cleanup deletes the resources and their logs, so the failed operation's log is kept before throwing.
        let diagnostic: string;
        try {
          const logs = await operationLogs(id);
          diagnostic = logs.length > 4_000 ? `...${logs.slice(-4_000)}` : logs;
          await testInfo.attach(`operation-${id}-log.txt`, { body: diagnostic, contentType: "text/plain" });
        } catch (error) {
          diagnostic = `log unavailable: ${error instanceof Error ? error.message.slice(0, 300) : "unknown error"}`;
        }
        throw new Error(`${result.action} ${id} ended ${result.state}: ${result.result?.error ?? ""}\n--- operation log (tail) ---\n${diagnostic}`);
      }
      return result;
    };
    const teamQuiet = () =>
      expect
        .poll(async () => ((await (await page.request.get(panelPath("/api/operations"))).json()) as Operation[]).filter((item) => item.state === "queued" || item.state === "running").map((item) => `${item.action}:${item.id}`), { timeout: 120_000 })
        .toEqual([]);
    const current = async () => {
      const response = await page.request.get(panelPath("/api/managed-registries"));
      const text = await response.text();
      expect(response.status(), text).toBe(200);
      expectNoSecrets(text);
      const found = (JSON.parse(text) as { registries: Registry[] }).registries.find((item) => item.id === registryId);
      if (!found) throw new Error(`Managed registry ${registryId} is not listed`);
      return found;
    };
    const registryReaches = async (state: string) => {
      await expect.poll(async () => (await current()).state, { timeout: 240_000 }).toMatch(/^(ready|failed|removed)$/);
      const reached = await current();
      if (reached.state !== state || reached.operation_state !== "succeeded") {
        const latest = reached.operation_id ? await operation(reached.operation_id) : null;
        throw new Error(`Registry reached ${reached.state} (operation ${reached.operation_state}), expected ${state}: ${reached.last_error} ${latest?.result?.error ?? ""}`);
      }
      return reached;
    };
    const workload = async () => {
      const response = await page.request.get(panelPath(`/api/workloads/${appId}`));
      const text = await response.text();
      expect(response.status(), text).toBe(200);
      expectNoSecrets(text);
      return JSON.parse(text) as { compose: string; source: typeof source | null };
    };
    const recipeSettings = async () => {
      const response = await page.request.get(panelPath(`/api/workloads/${appId}/build-recipe`));
      const text = await response.text();
      expect(response.status(), text).toBe(200);
      expectNoSecrets(text);
      return JSON.parse(text) as RecipeSettings;
    };
    const appContainer = () => {
      const ids = docker("ps", "-q", "--filter", `label=com.docker.compose.project=cove-${appId}`, "--filter", "label=com.docker.compose.service=app").split(/\s+/).filter(Boolean);
      expect(ids).toHaveLength(1);
      const [id, image, imageId, started, restarts] = docker("inspect", "--format", "{{.Id}}|{{.Config.Image}}|{{.Image}}|{{.State.StartedAt}}|{{.RestartCount}}", ids[0] ?? "").split("|");
      return { id: id ?? "", image, imageId, started, restarts };
    };
    const respondedTo = (path: string, method: string) => page.waitForResponse((response) => new URL(response.url()).pathname === path && response.request().method() === method);
    const recipePath = () => panelPath(`/api/workloads/${appId}/build-recipe`);
    let recipeWrites = 0;
    page.on("request", (request) => {
      if (appId && request.method() !== "GET" && new URL(request.url()).pathname === recipePath()) recipeWrites++;
    });

    // A project and a managed registry on the node, repaired with the fixture leaf until it is ready.
    const projectReply = await page.request.post(panelPath("/api/projects"), { headers, data: { name: `Buildpacks ${Date.now()}` } });
    expect(projectReply.ok(), await projectReply.text()).toBe(true);
    const project = (await projectReply.json()) as { id: string };
    const retention = { keep_per_repository: 10, minimum_age_days: 30 };
    const createdReply = await page.request.post(panelPath("/api/managed-registries"), { headers, data: { name: `Buildpacks registry ${Date.now()}`, node_id: node.id, project_id: project.id, hostname, retention } });
    const createdText = await createdReply.text();
    expect(createdReply.status(), createdText).toBe(202);
    expectNoSecrets(createdText);
    registryId = (JSON.parse(createdText) as { id: string }).id;
    deferImageChecks(registryId);
    const failed = await registryReaches("failed");
    expect(failed.last_error).toBe(tlsFailure);
    const uploadReply = await page.request.post(panelPath(`/api/workloads/${registryId}/certificates`), { headers, data: { name: "Fixture registry leaf", certificate_pem: certificate, private_key_pem: key } });
    const uploadText = await uploadReply.text();
    expect(uploadReply.status(), uploadText).toBe(201);
    expectNoSecrets(uploadText);
    const uploaded = JSON.parse(uploadText) as { id: string; dns_names: string[] };
    expect(uploaded.dns_names).toEqual([hostname]);
    const selectReply = await page.request.put(panelPath(`/api/managed-registries/${registryId}`), { headers, data: { retention, certificate_id: uploaded.id } });
    expect(selectReply.status(), await selectReply.text()).toBe(200);
    const retryReply = await page.request.post(panelPath(`/api/managed-registries/${registryId}/retry`), { headers });
    expect(retryReply.status(), await retryReply.text()).toBe(202);
    const ready = await registryReaches("ready");
    expect(ready.connection_id).not.toBe("");
    const connectionsReply = await page.request.get(panelPath("/api/registries"));
    const connectionsText = await connectionsReply.text();
    expect(connectionsReply.status(), connectionsText).toBe(200);
    expectNoSecrets(connectionsText);
    const generated = (JSON.parse(connectionsText) as RegistryConnection[]).filter((item) => item.managed_registry_id);
    expect(generated.map((item) => ({ id: item.id, managed: item.managed_registry_id, server: item.registry?.server, password: item.registry?.password }))).toEqual([{ id: ready.connection_id, managed: registryId, server: hostname, password: "" }]);

    // An image-only BusyBox service whose Git source uses the saved Compose file, deployed from the cached image.
    const appReply = await page.request.post(panelPath("/api/workloads"), { headers, data: { name: "buildpacks-app", node_id: node.id, kind: "service", compose: initialCompose, source, project_id: project.id } });
    expect(appReply.status(), await appReply.text()).toBe(201);
    appId = ((await appReply.json()) as { id: string }).id;
    deferImageChecks(appId);
    const connect = await page.request.put(panelPath(`/api/workloads/${appId}/registry`), { headers, data: { registry_id: ready.connection_id } });
    expect(connect.status(), await connect.text()).toBe(200);
    expect(await (await page.request.get(panelPath(`/api/workloads/${appId}/registry`))).json()).toEqual({ registry_id: ready.connection_id });
    const saved = await workload();
    expect(saved.compose).toBe(initialCompose);
    expect(saved.compose).not.toContain("build");
    expect(saved.source).toMatchObject({ repository: repositoryUrl, branch: "main", inline_compose: true });
    const firstDeploy = await page.request.post(panelPath(`/api/workloads/${appId}/operations`), { headers, data: { action: "deploy" } });
    expect(firstDeploy.status(), await firstDeploy.text()).toBe(202);
    await settled(((await firstDeploy.json()) as { id: string }).id);
    await teamQuiet();
    const original = appContainer();
    expect(original.image).toBe("busybox:1.37");
    expect(docker("exec", original.id, "cat", "/tmp/marker")).toBe("initial-release");

    // The server's Cloud Native Buildpacks read is the only source of the figures the dashboard discloses.
    const initialSettings = await recipeSettings();
    expect({ recipe: initialSettings.recipe, source_ready: initialSettings.source_ready, can_edit: initialSettings.can_edit }).toEqual({ recipe: emptyRecipe, source_ready: true, can_edit: true });
    const cnb = initialSettings.buildpacks;
    if (!cnb) throw new Error("The build recipe read did not report Cloud Native Buildpacks eligibility");
    expect({ supported: cnb.supported, reason: cnb.reason }).toEqual({ supported: true, reason: "" });
    expect({
      lifecycle: cnb.lifecycle_version,
      builder: cnb.builder_version,
      platform: cnb.platform_api,
      storage: cnb.work_storage,
      hardCap: cnb.work_hard_cap,
      target: cnb.work_target_bytes,
      minimum: cnb.minimum_phase_memory_bytes,
      maximum: cnb.maximum_phase_memory_bytes,
      safety: cnb.memory_safety_bytes,
      available: cnb.required_available_memory_bytes,
      docker: cnb.required_docker_disk_bytes,
      state: cnb.required_worker_disk_bytes,
      cpu: cnb.quota_cpu,
      experimental: cnb.experimental,
    }).toEqual({
      lifecycle: "0.21.29",
      builder: "0.0.230",
      platform: "0.13",
      storage: "disk",
      hardCap: false,
      target: 3 * gib,
      minimum: 2 * gib,
      maximum: 4 * gib,
      safety: 2 * gib,
      available: 4 * gib,
      docker: 24 * gib,
      state: 10 * gib,
      cpu: 2.5,
      experimental: true,
    });
    expect(cnb.builder_download_bytes).toBeGreaterThan(0);
    expect(cnb.builder_unpacked_bytes).toBeGreaterThan(cnb.builder_download_bytes);
    expect(cnb.pinned_images).toHaveLength(3);
    for (const image of cnb.pinned_images) expect(image).toMatch(/@sha256:[a-f0-9]{64}$/);
    const pins = cnb.pinned_images;

    // Choose Cloud Native Buildpacks from the keyboard. Editing alone saves nothing.
    await page.goto(panelPath(`/resources/${appId}/registry`));
    const recipeForm = page.getByRole("form", { name: "Build recipe setting" });
    const engine = recipeForm.getByRole("combobox", { name: "Build engine", exact: true });
    const serviceSelect = recipeForm.getByRole("combobox", { name: "Service", exact: true });
    const directory = recipeForm.getByLabel("Source directory", { exact: true });
    await expect(engine).toHaveText("Existing Compose / Dockerfile", { timeout: 30_000 });
    await capture("buildpacks-default");
    await chooseWithKeyboard(page, engine, "Cloud Native Buildpacks");
    await chooseWithKeyboard(page, serviceSelect, "app");
    await typeWithKeyboard(page, directory, ".");
    await expect(recipeForm.getByLabel("Output directory")).toHaveCount(0);
    await expect(recipeForm.getByLabel("Build command")).toHaveCount(0);
    await expect(recipeForm.getByLabel("Start command")).toHaveCount(0);
    const recipeStatus = recipeForm.getByRole("status");
    await expect(recipeStatus).toHaveText("Unsaved changes");
    expect(recipeWrites).toBe(0);
    expect((await recipeSettings()).recipe).toEqual(emptyRecipe);

    // The disclosure reports the figures the server returned.
    const expectDisclosure = async () => {
      const needs = recipeForm.getByRole("list", { name: "Cloud Native Buildpacks resource needs" });
      await expect(needs).toContainText("Work is operation-owned disk state with a monitored 3 GiB target, not a hard storage cap.");
      await expect(needs).toContainText("There is no persistent Cloud Native Buildpacks cache");
      await expect(needs).toContainText("at least 4 GiB of actually available memory");
      await expect(needs).toContainText("available memory minus 2 GiB of safety headroom, clamped between 2 GiB and 4 GiB and rounded down to a whole MiB");
      await expect(needs).toContainText("24 GiB of free Docker storage and 10 GiB of free worker-state storage");
      await expect(needs).toContainText(`about ${(cnb.builder_download_bytes / 1e9).toFixed(2)} GB to download and about ${(cnb.builder_unpacked_bytes / 1e9).toFixed(2)} GB once unpacked`);
      await expect(needs).toContainText(`up to ${cnb.quota_memory_mb.toLocaleString("en-US")} MiB, and 2.5 CPUs including helpers`);
      await expect(recipeForm.getByText(/lifecycle's OCI-layout export is experimental upstream/)).toBeVisible();
      await expect(recipeForm.getByText(/neither reproducible nor fully pinned/)).toBeVisible();
      await expect(recipeForm.getByText(/not the toolchains or packages buildpacks download/)).toBeVisible();
      await expect(recipeForm.getByText(cnb.lifecycle_version, { exact: true })).toBeVisible();
      await expect(recipeForm.getByText(cnb.builder_version, { exact: true })).toBeVisible();
      await expect(recipeForm.getByText(cnb.platform_api, { exact: true })).toBeVisible();
      await expect(recipeForm.getByRole("list", { name: "Pinned Cloud Native Buildpacks images" }).getByRole("listitem")).toHaveText(pins);
    };
    await expectDisclosure();

    // Saving asks to confirm the engine change, then sends exactly the six recipe fields.
    const saveRecipe = recipeForm.getByRole("button", { name: "Save build recipe", exact: true });
    const confirmEngine = async (expected: typeof emptyRecipe) => {
      await expect(saveRecipe).toBeEnabled();
      await saveRecipe.focus();
      await page.keyboard.press("Enter");
      const dialog = page.getByRole("dialog", { name: "Change the build engine?" });
      await expect(dialog).toBeVisible();
      expect(recipeWrites).toBe(0);
      const reply = respondedTo(recipePath(), "PUT");
      await dialog.getByRole("button", { name: "Change build engine", exact: true }).focus();
      await page.keyboard.press("Enter");
      const response = await reply;
      expect(response.status(), await response.text()).toBe(200);
      expect(response.request().postDataJSON()).toEqual(expected);
      expect(recipeWrites).toBe(1);
      recipeWrites = 0;
      await expect(dialog).toBeHidden();
      await expect(recipeStatus).toHaveText(savedNotice);
      expect((await recipeSettings()).recipe).toEqual(expected);
      await expect(recipeStatus).toHaveText(savedNotice);
    };
    await confirmEngine(buildpacksRecipe);
    expect(appContainer()).toEqual(original);
    expect((await workload()).compose).toBe(initialCompose);
    await page.reload();
    await expect(engine).toHaveText("Cloud Native Buildpacks", { timeout: 30_000 });
    await expect(serviceSelect).toHaveText("app");
    await expect(directory).toHaveValue(".");
    await expectDisclosure();
    await capture("buildpacks-saved");
    expect(appContainer()).toEqual(original);

    // Build and publish from the keyboard with the unchanged build payload. Nothing is deployed.
    await typeWithKeyboard(page, page.getByLabel("Repository", { exact: true }), repository);
    const noCache = page.getByRole("checkbox", { name: "Bypass the build cache" });
    await noCache.focus();
    await page.keyboard.press("Space");
    await expect(noCache).toBeChecked();
    const buildButton = page.getByRole("button", { name: "Build and publish", exact: true });
    await expect(buildButton).toBeEnabled();
    const queuedReply = respondedTo(panelPath(`/api/workloads/${appId}/operations`), "POST");
    await buildButton.focus();
    await page.keyboard.press("Enter");
    const queued = await queuedReply;
    const queuedText = await queued.text();
    expect(queued.status(), queuedText).toBe(202);
    expect(queued.request().postDataJSON()).toEqual({ action: "build", build: { repository, no_cache: true } });
    const queuedBody = JSON.parse(queuedText) as { id: string; state: string };
    expect(queuedBody.state).toBe("queued");
    const buildId = queuedBody.id;
    await expect(page.getByText("Build queued", { exact: true })).toBeVisible();

    const built = await settled(buildId, 180_000);
    expect(built.action).toBe("build");
    expect(built.execution_node_id).toBe(node.id);
    const builtResult = workerResult(built);
    expect(builtResult?.revision).toBe(revision);
    const recorded = builtResult?.build_recipe;
    if (!recorded) throw new Error("The build did not record its recipe");
    expect({ engine: recorded.engine, service: recorded.service, version: recorded.version, builder: recorded.builder_version, platform: recorded.platform_api, pinned: recorded.pinned_images, runtime: recorded.static_runtime }).toEqual({
      engine: "buildpacks",
      service: "app",
      version: "0.21.29",
      builder: "0.0.230",
      platform: "0.13",
      pinned: pins,
      runtime: undefined,
    });
    // The phase limit is derived from the memory admission observed, not defaulted to the maximum. A node with 6 GiB or
    // more available validly reaches the 4 GiB ceiling, so only the exact derivation distinguishes the two.
    const phase = recorded.phase_memory_bytes;
    const observed = recorded.admission_available_memory_bytes;
    if (typeof phase !== "number" || typeof observed !== "number") throw new Error(`The build did not record its phase memory admission: ${JSON.stringify({ phase, observed })}`);
    expect(Number.isSafeInteger(phase) && Number.isSafeInteger(observed)).toBe(true);
    expect(observed).toBeGreaterThanOrEqual(4 * gib);
    expect(phase).toBeGreaterThanOrEqual(2 * gib);
    expect(phase).toBeLessThanOrEqual(4 * gib);
    expect(phase % mib).toBe(0);
    expect(phase).toBe(Math.min(4 * gib, Math.floor((observed - 2 * gib) / mib) * mib));

    const images = builtResult?.build?.images ?? [];
    expect(images.map((image) => image.service)).toEqual(["app"]);
    const [image] = images;
    if (!image) throw new Error("The build reported no published image");
    expect(image.reference).toMatch(new RegExp(`^${hostname.replace(/[.]/g, "\\.")}/${repository}@sha256:[a-f0-9]{64}$`));
    expect(image.image_id).toMatch(/^sha256:[a-f0-9]{64}$/);
    const reference = image.reference;

    const logs = await operationLogs(buildId);
    const phaseLines = [...logs.matchAll(/^Cloud Native Buildpacks phase: (\S+)$/gm)].map((match) => match[1]);
    expect(phaseLines).toEqual(phases);
    expect(logs).toContain(`Published app as ${reference}`);
    expect(appContainer()).toEqual(original);
    expect((await workload()).compose).toBe(initialCompose);

    // The worker removes the operation's lifecycle, builder and helper resources by their ownership labels.
    const owned = (kind: "container" | "volume" | "network") =>
      docker(kind, "ls", ...(kind === "container" ? ["--all"] : []), "-q", "--filter", "label=cove.managed=build-recipe", "--filter", `label=cove.operation=${buildId}`).split(/\s+/).filter(Boolean);
    await expect.poll(() => ({ containers: owned("container"), volumes: owned("volume"), networks: owned("network") }), { timeout: 30_000 }).toEqual({ containers: [], volumes: [], networks: [] });

    // The history and the operation page report the recorded engine, versions and actual phase limit.
    const engineText = recordedEngine(built);
    const memoryText = recordedPhaseMemory(built);
    expect(engineText).toBe("Cloud Native Buildpacks (lifecycle 0.21.29, builder 0.0.230, platform API 0.13) for app");
    if (!memoryText) throw new Error("No phase memory text for the recorded build");
    expect(memoryText).toMatch(/^Phase memory limit \d/);
    const buildSection = page.getByRole("region", { name: "Build and publish", exact: true });
    const expectHistory = async () => {
      await expect(buildSection.getByRole("heading", { name: "Latest successful build", exact: true })).toBeVisible({ timeout: 30_000 });
      await expect(buildSection.getByText(engineText, { exact: true }).first()).toBeVisible();
      await expect(buildSection.getByText(memoryText, { exact: true }).first()).toBeVisible();
      await expect(buildSection.getByRole("list", { name: "Published images" }).getByText(reference, { exact: true })).toBeVisible();
    };
    await page.reload();
    await expect(engine).toHaveText("Cloud Native Buildpacks", { timeout: 30_000 });
    await expect(serviceSelect).toHaveText("app");
    await expect(directory).toHaveValue(".");
    await expectHistory();
    await capture("buildpacks-history");
    await page.goto(panelPath(`/operations/${buildId}`));
    const engineValue = definition(page, "Build engine");
    await expect(engineValue.getByText(engineText, { exact: true })).toBeVisible({ timeout: 30_000 });
    await expect(engineValue.getByText(memoryText, { exact: true })).toBeVisible();
    await expect(engineValue.getByRole("list", { name: "Pinned build images" }).getByRole("listitem")).toHaveText(pins);
    expect(await engineValue.innerText()).not.toMatch(/command/i);
    await capture("buildpacks-operation");

    // Deploy the immutable reference explicitly, keeping the Git source. Saving leaves the original container running.
    const compose = deployCompose(reference);
    const savedDeploy = await page.request.put(panelPath(`/api/workloads/${appId}`), { headers, data: { name: "buildpacks-app", node_id: node.id, kind: "service", compose, source } });
    expect([200, 202], await savedDeploy.text()).toContain(savedDeploy.status());
    const afterSave = await workload();
    expect(afterSave.compose).toBe(compose);
    expect(afterSave.compose).not.toContain("command");
    expect(afterSave.source).toMatchObject({ repository: repositoryUrl, branch: "main", inline_compose: true });
    expect(appContainer()).toEqual(original);
    const deployReply = await page.request.post(panelPath(`/api/workloads/${appId}/operations`), { headers, data: { action: "deploy" } });
    expect(deployReply.status(), await deployReply.text()).toBe(202);
    await settled(((await deployReply.json()) as { id: string }).id);
    await teamQuiet();
    const deployed = appContainer();
    expect(deployed.id).not.toBe(original.id);
    expect(deployed.image).toBe(reference);

    // Probe the app's internal HTTP from an owned container on its Compose network in the same nested daemon.
    const networkName = `cove-${appId}_default`;
    expect(docker("network", "inspect", "--format", '{{index .Labels "com.docker.compose.project"}}', networkName)).toBe(`cove-${appId}`);
    const address = docker("inspect", "--format", `{{(index .NetworkSettings.Networks ${JSON.stringify(networkName)}).IPAddress}}`, deployed.id);
    expect(address).toMatch(/^\d+\.\d+\.\d+\.\d+$/);
    let probes = 0;
    const probe = () =>
      docker("run", "--rm", "--pull", "never", "--name", `${name}-buildpacks-probe-${++probes}`, "--network", networkName, "--label", `cove.browser-fixture=${name}`, "--label", probeLabel, "busybox:1.37", "sh", "-c", 'wget -q -O- "$1" 2>&1; true', "sh", `http://${address}:8080/`);
    await expect.poll(probe, { timeout: 30_000 }).toBe(marker);
    const running = appContainer();

    // Clear the recipe back to Compose from the keyboard. The recorded history still names Cloud Native Buildpacks.
    await page.goto(panelPath(`/resources/${appId}/registry`));
    await expect(engine).toHaveText("Cloud Native Buildpacks", { timeout: 30_000 });
    await chooseWithKeyboard(page, engine, "Existing Compose / Dockerfile");
    expect(recipeWrites).toBe(0);
    await confirmEngine(emptyRecipe);
    await page.reload();
    await expect(engine).toHaveText("Existing Compose / Dockerfile", { timeout: 30_000 });
    await expect(serviceSelect).toHaveCount(0);
    await expect(recipeForm.getByRole("list", { name: "Cloud Native Buildpacks resource needs" })).toHaveCount(0);
    await expectHistory();
    expect(appContainer()).toEqual(running);
    await capture("buildpacks-cleared");

    // Teardown through the product: stop and delete the app, then remove, purge and delete the registry.
    await teamQuiet();
    const stopReply = await page.request.post(panelPath(`/api/workloads/${appId}/operations`), { headers, data: { action: "stop" } });
    expect(stopReply.status(), await stopReply.text()).toBe(202);
    await settled(((await stopReply.json()) as { id: string }).id);
    await teamQuiet();
    const deleted = await page.request.delete(panelPath(`/api/workloads/${appId}`), { headers });
    expect(deleted.status(), await deleted.text()).toBe(200);
    appDeleted = true;
    await teamQuiet();
    const removeReply = await page.request.delete(panelPath(`/api/managed-registries/${registryId}`), { headers, data: { delete_data: false } });
    expect(removeReply.status(), await removeReply.text()).toBe(202);
    expect((await registryReaches("removed")).data_retained).toBe(true);
    await teamQuiet();
    const purgeReply = await page.request.delete(panelPath(`/api/managed-registries/${registryId}`), { headers, data: { delete_data: true } });
    expect(purgeReply.status(), await purgeReply.text()).toBe(202);
    expect((await registryReaches("removed")).data_retained).toBe(false);
    await teamQuiet();
    const recordReply = await page.request.delete(panelPath(`/api/managed-registries/${registryId}/record`), { headers });
    expect(recordReply.status(), await recordReply.text()).toBe(200);
    registryRecordDeleted = true;
  } catch (error) {
    primary = asError(error);
  }
  // Cleanup never replaces the first failure; its own failure fails the test only when the test otherwise passed.
  // Product APIs run first because generic database cleanup bypasses registry references.
  const cleanupFailures: string[] = [];
  const attempt = async (step: string, action: () => Promise<void> | void) => {
    try {
      await action();
    } catch (error) {
      cleanupFailures.push(`${step}: ${asError(error).message}`);
    }
  };
  const quiet = async () => {
    await expect
      .poll(async () => ((await (await page.request.get(panelPath("/api/operations"))).json()) as Operation[]).some((item) => item.state === "queued" || item.state === "running"), { timeout: 120_000 })
      .toBe(false);
  };
  if (appId && !appDeleted) {
    await attempt("app stop", async () => {
      await quiet();
      const reply = await page.request.post(panelPath(`/api/workloads/${appId}/operations`), { headers, data: { action: "stop" } });
      if (reply.status() !== 202) throw new Error(`stop returned ${reply.status()}: ${await reply.text()}`);
      await quiet();
    });
    await attempt("app delete", async () => {
      const reply = await page.request.delete(panelPath(`/api/workloads/${appId}`), { headers });
      if (reply.status() !== 200) throw new Error(`delete returned ${reply.status()}: ${await reply.text()}`);
    });
  }
  if (registryId && !registryRecordDeleted) {
    await attempt("registry removal", async () => {
      await quiet();
      const reply = await page.request.delete(panelPath(`/api/managed-registries/${registryId}`), { headers, data: { delete_data: true } });
      if (reply.status() !== 202) throw new Error(`removal returned ${reply.status()}: ${await reply.text()}`);
      await quiet();
    });
  }
  await attempt("probe containers", () => {
    for (const container of docker("ps", "-aq", "--filter", `label=${probeLabel}`, "--filter", `label=cove.browser-fixture=${name}`).split(/\s+/).filter(Boolean)) docker("rm", "-f", container);
  });
  // A failed build can leave its recipe resources pending the worker's journaled cleanup; remove only this app's.
  if (appId) {
    await attempt("recipe resources", () => {
      const filters = ["--filter", "label=cove.managed=build-recipe", "--filter", `label=cove.workload=${appId}`];
      for (const container of docker("ps", "-aq", ...filters).split(/\s+/).filter(Boolean)) docker("rm", "-f", container);
      for (const volume of docker("volume", "ls", "-q", ...filters).split(/\s+/).filter(Boolean)) docker("volume", "rm", volume);
      for (const network of docker("network", "ls", "-q", ...filters).split(/\s+/).filter(Boolean)) docker("network", "rm", network);
    });
  }
  await attempt("browser resources", () => cleanupBrowserResources());
  if (registryId) {
    await attempt("registry volume", () => {
      const volumes = docker("volume", "ls", "-q", "--filter", `label=dev.cove.managed-registry=${registryId}`).split(/\s+/).filter(Boolean);
      for (const volume of volumes) if (volume === `cove-${registryId}_data`) docker("volume", "rm", volume);
    });
  }
  if (primary !== null) {
    for (const failure of cleanupFailures) {
      testInfo.annotations.push({ type: "cleanup-failure", description: failure });
      await testInfo.attach("cleanup-failure.txt", { body: failure, contentType: "text/plain" });
    }
    throw primary;
  }
  if (cleanupFailures.length > 0) throw new Error(`Cleanup failed: ${cleanupFailures.join("; ")}`);
});

// The fixture's own generated key, held only in this process. It is never printed, attached, sent to the browser or
// the API, or passed in a command line.
function fixtureKey() {
  const text = process.env.COVE_ENCRYPTION_KEY ?? "";
  const key = Buffer.from(text, "base64");
  if (key.length !== 32 || key.toString("base64") !== text) throw new Error("COVE_ENCRYPTION_KEY must be the fixture's canonical base64 32-byte key");
  return key;
}

// Matches seal in internal/control/security.go: AES-256-GCM, a 12-byte nonce, the ciphertext, then the 16-byte tag.
function sealRecipe(key: Buffer, recipe: typeof emptyRecipe) {
  const nonce = randomBytes(12);
  const cipher = createCipheriv("aes-256-gcm", key, nonce);
  return Buffer.concat([nonce, cipher.update(JSON.stringify(recipe), "utf8"), cipher.final(), cipher.getAuthTag()]);
}

test("buildpacks: a node below the admission gate disables Cloud Native Buildpacks and refuses a saved historical recipe without starting anything", async ({ page }, testInfo) => {
  test.skip(!enabled, "Needs scripts/test-browser.py --managed-registry --buildpacks");
  test.setTimeout(600_000);
  const { name, database, worker, daemon, hostname, key } = fixture();
  const expectNoSecrets = noSecrets(secretMarkersFor(key));
  const sql = (query: string) => docker("exec", database, "psql", "-U", "postgres", "-d", "cove", "-Atc", query);
  const capture = async (label: string) => {
    expect(await page.locator("body").innerText()).not.toContain("-----BEGIN");
    for (const width of sizes) {
      await page.setViewportSize({ width, height: 900 });
      await expectNoHorizontalScroll(page);
      await page.screenshot({ path: testInfo.outputPath(`${label}-${width}.png`), fullPage: true });
    }
    await page.setViewportSize({ width: 1280, height: 900 });
  };

  // Nothing is created before this point, so an eligible node skips here with nothing to clean up.
  expectOwnedWorker({ name, worker, daemon });
  const headers = await signIn(page);
  const { node, capacity, reason, eligible, summary } = await observeBuildpacks(page, expectNoSecrets);
  if (eligible) {
    skipObserved(testInfo, `SKIPPED packaged Cloud Native Buildpacks refusal: the node meets the admission gate, so the build test covers it instead. ${summary}.`);
  }
  // The reason carries the measured figures for the first requirement that failed.
  if (capacity.memory_available < 4 * gib) expect(reason).toContain(`observed ${capacity.memory_available} bytes`);
  else expect(reason).toContain(`observed Docker=${capacity.disk_available} bytes and worker=${capacity.state_available} bytes`);
  console.log(`Running packaged Cloud Native Buildpacks refusal with real measurements: ${summary}`);
  await testInfo.attach("observed-admission.txt", { body: summary, contentType: "text/plain" });

  let appId = "";
  let connectionId = "";
  let appDeleted = false;
  let connectionDeleted = false;
  let primary: Error | null = null;
  try {
    // The sealed recipe goes only into this fixture's database, read by this fixture's panel.
    const identity = (container: string) => {
      const [containerName, image, attached = ""] = docker("inspect", "--format", "{{.Name}}|{{.Config.Image}}|{{range $network, $_ := .NetworkSettings.Networks}}{{$network}} {{end}}", container).split("|");
      return { name: containerName, image, networks: attached.split(" ").filter(Boolean) };
    };
    const panel = identity(`${name}-panel`);
    expect({ name: panel.name, image: panel.image }).toEqual({ name: `/${name}-panel`, image: `${name}-panel:development` });
    expect(panel.networks).toContain(name);
    const db = identity(database);
    expect({ name: db.name, image: db.image }).toEqual({ name: `/${database}`, image: "postgres:17-alpine" });
    expect(db.networks).toContain(name);
    expect(sql("SELECT current_database()")).toBe("cove");
    const recipeKey = fixtureKey();

    const operations = async () => {
      const response = await page.request.get(panelPath("/api/operations"));
      const text = await response.text();
      expect(response.status(), text).toBe(200);
      expectNoSecrets(text);
      return JSON.parse(text) as Operation[];
    };
    const operation = async (id: string) => {
      const response = await page.request.get(panelPath(`/api/operations/${id}`));
      const text = await response.text();
      expect(response.status(), text).toBe(200);
      expectNoSecrets(text);
      return JSON.parse(text) as Operation;
    };
    const settled = async (id: string) => {
      await expect.poll(async () => (await operation(id)).state, { timeout: 180_000 }).toMatch(finished);
      const result = await operation(id);
      if (result.state !== "succeeded") throw new Error(`${result.action} ${id} ended ${result.state}: ${result.result?.error ?? ""}`);
      return result;
    };
    const teamQuiet = () =>
      expect
        .poll(async () => (await operations()).filter((item) => item.state === "queued" || item.state === "running").map((item) => `${item.action}:${item.id}`), { timeout: 120_000 })
        .toEqual([]);
    const recipeSettings = async () => {
      const response = await page.request.get(panelPath(`/api/workloads/${appId}/build-recipe`));
      const text = await response.text();
      expect(response.status(), text).toBe(200);
      expectNoSecrets(text);
      return JSON.parse(text) as RecipeSettings;
    };
    const appContainer = () => {
      const ids = docker("ps", "-q", "--filter", `label=com.docker.compose.project=cove-${appId}`, "--filter", "label=com.docker.compose.service=app").split(/\s+/).filter(Boolean);
      expect(ids).toHaveLength(1);
      const [id, image, imageId, started, restarts] = docker("inspect", "--format", "{{.Id}}|{{.Config.Image}}|{{.Image}}|{{.State.StartedAt}}|{{.RestartCount}}", ids[0] ?? "").split("|");
      return { id: id ?? "", image, imageId, started, restarts };
    };
    const recipePath = () => panelPath(`/api/workloads/${appId}/build-recipe`);
    let recipeWrites = 0;
    page.on("request", (request) => {
      if (appId && request.method() !== "GET" && new URL(request.url()).pathname === recipePath()) recipeWrites++;
    });

    // An ordinary connection reaches the build admission branch. It names this fixture's unprovisioned .test hostname
    // with fixture-only credentials; no registry is contacted because nothing is ever built or pushed.
    const projectReply = await page.request.post(panelPath("/api/projects"), { headers, data: { name: `Buildpacks refusal ${Date.now()}` } });
    expect(projectReply.ok(), await projectReply.text()).toBe(true);
    const project = (await projectReply.json()) as { id: string };
    const connectionReply = await page.request.post(panelPath("/api/registries"), {
      headers,
      data: { name: `Buildpacks refusal connection ${Date.now()}`, registry: { server: hostname, username: "fixture", password: "fixture-only-unused" } },
    });
    const connectionText = await connectionReply.text();
    expect(connectionReply.status(), connectionText).toBe(201);
    connectionId = (JSON.parse(connectionText) as { id: string }).id;
    expect(connectionId).toMatch(resourceId);

    // An image-only BusyBox service with the synthetic Git source deploys its ordinary first release from the cache.
    const appReply = await page.request.post(panelPath("/api/workloads"), { headers, data: { name: "buildpacks-refusal-app", node_id: node.id, kind: "service", compose: initialCompose, source, project_id: project.id } });
    expect(appReply.status(), await appReply.text()).toBe(201);
    appId = ((await appReply.json()) as { id: string }).id;
    expect(appId).toMatch(resourceId);
    expect(sql(`UPDATE workloads SET next_image_check_at=now()+interval '1 day' WHERE id='${appId}' RETURNING id`).split(/\s+/).filter((value) => value === appId)).toEqual([appId]);
    const connect = await page.request.put(panelPath(`/api/workloads/${appId}/registry`), { headers, data: { registry_id: connectionId } });
    expect(connect.status(), await connect.text()).toBe(200);
    const firstDeploy = await page.request.post(panelPath(`/api/workloads/${appId}/operations`), { headers, data: { action: "deploy" } });
    expect(firstDeploy.status(), await firstDeploy.text()).toBe(202);
    await settled(((await firstDeploy.json()) as { id: string }).id);
    await teamQuiet();
    const original = appContainer();
    expect(original.image).toBe("busybox:1.37");
    expect(docker("exec", original.id, "cat", "/tmp/marker")).toBe("initial-release");

    // The server reports the node's real refusal for this resource.
    const initial = await recipeSettings();
    expect({ recipe: initial.recipe, source_ready: initial.source_ready, can_edit: initial.can_edit, reason: initial.reason }).toEqual({ recipe: emptyRecipe, source_ready: true, can_edit: true, reason: "" });
    expect({ supported: initial.buildpacks?.supported, reason: initial.buildpacks?.reason }).toEqual({ supported: false, reason });

    // Compose stays the engine, Cloud Native Buildpacks is a disabled choice and the actual reason is shown.
    await page.goto(panelPath(`/resources/${appId}/registry`));
    const recipeForm = page.getByRole("form", { name: "Build recipe setting" });
    const engine = recipeForm.getByRole("combobox", { name: "Build engine", exact: true });
    const recipeStatus = recipeForm.getByRole("status");
    const saveRecipe = recipeForm.getByRole("button", { name: "Save build recipe", exact: true });
    const disabledLabel = "Cloud Native Buildpacks (unavailable on the build node)";
    await expect(engine).toHaveText("Existing Compose / Dockerfile", { timeout: 30_000 });
    const unavailableHint = recipeForm.getByText(`Cloud Native Buildpacks is unavailable: ${reason}`, { exact: true });
    await expect(unavailableHint).toBeVisible();
    await expect(recipeStatus).toHaveText("No unsaved changes");
    await expect(saveRecipe).toBeDisabled();
    await capture("buildpacks-unavailable");

    // From the keyboard the list skips the disabled choice; End lands on the last enabled engine instead.
    await engine.focus();
    await page.keyboard.press("Space");
    const listbox = page.getByRole("listbox");
    await expect(listbox).toBeVisible();
    const disabledOption = listbox.getByRole("option", { name: disabledLabel, exact: true });
    await expect(disabledOption).toHaveAttribute("aria-disabled", "true");
    await expect(listbox.getByRole("option", { name: "Existing Compose / Dockerfile", exact: true })).toHaveAttribute("aria-selected", "true");
    const disabledId = await disabledOption.getAttribute("id");
    await page.keyboard.press("End");
    const active = await engine.getAttribute("aria-activedescendant");
    expect(active).toBeTruthy();
    expect(active).not.toBe(disabledId);
    await expect(page.locator(`[id="${active ?? ""}"]`)).not.toHaveAttribute("aria-disabled", "true");
    await expect(engine).toBeFocused();
    // Each width reopens the list from the keyboard with the trigger on screen, so the shots show the real placement
    // next to the trigger rather than a list clamped into the viewport around an anchor that scrolled away.
    for (const width of sizes) {
      if (await listbox.isVisible()) {
        await page.keyboard.press("Escape");
        await expect(listbox).toBeHidden();
      }
      await page.setViewportSize({ width, height: 900 });
      // scrollIntoViewIfNeeded treats a trigger clipped by a fraction of a pixel after the resize as already visible,
      // so the trigger is explicitly centered before the list opens.
      await engine.evaluate((element) => element.scrollIntoView({ block: "center", inline: "nearest", behavior: "instant" }));
      await engine.focus();
      await page.keyboard.press("Space");
      await expect(listbox).toBeVisible();
      await expect(disabledOption).toBeVisible();
      await expect(disabledOption).toHaveAttribute("aria-disabled", "true");
      await expect(listbox.getByRole("option", { name: "Existing Compose / Dockerfile", exact: true })).toHaveAttribute("aria-selected", "true");
      await page.keyboard.press("End");
      const activeAtWidth = await engine.getAttribute("aria-activedescendant");
      expect(activeAtWidth).toBeTruthy();
      expect(activeAtWidth).not.toBe(disabledId);
      await expect(page.locator(`[id="${activeAtWidth ?? ""}"]`)).not.toHaveAttribute("aria-disabled", "true");
      await expect(engine).toBeFocused();
      await expect(async () => {
        const trigger = await engine.boundingBox();
        const popup = await listbox.boundingBox();
        expect(trigger, "Build engine trigger box").not.toBeNull();
        expect(popup, "Build engine options box").not.toBeNull();
        if (!trigger || !popup) return;
        expect(trigger.x, `trigger left at ${width}px`).toBeGreaterThanOrEqual(0);
        expect(trigger.y, `trigger top at ${width}px`).toBeGreaterThanOrEqual(0);
        expect(trigger.x + trigger.width, `trigger right at ${width}px`).toBeLessThanOrEqual(width);
        expect(trigger.y + trigger.height, `trigger bottom at ${width}px`).toBeLessThanOrEqual(900);
        expect(popup.x, `options left at ${width}px`).toBeGreaterThanOrEqual(0);
        expect(popup.y, `options top at ${width}px`).toBeGreaterThanOrEqual(0);
        expect(popup.x + popup.width, `options right at ${width}px`).toBeLessThanOrEqual(width);
        expect(popup.y + popup.height, `options bottom at ${width}px`).toBeLessThanOrEqual(900);
        // The panel keeps an 8px viewport inset and a 6px gap from the trigger; one side must hold the whole list.
        const roomBelow = 900 - 8 - (trigger.y + trigger.height) - 6;
        const roomAbove = trigger.y - 8 - 6;
        expect(Math.max(roomBelow, roomAbove), `room for the options next to the trigger at ${width}px`).toBeGreaterThanOrEqual(popup.height);
        const gap = popup.y >= trigger.y + trigger.height / 2 ? popup.y - (trigger.y + trigger.height) : trigger.y - (popup.y + popup.height);
        expect(Math.abs(gap - 6), `options sit 6px from the trigger at ${width}px (gap ${gap})`).toBeLessThanOrEqual(1.5);
        expect(Math.min(popup.x + popup.width, trigger.x + trigger.width) - Math.max(popup.x, trigger.x), `options overlap the trigger horizontally at ${width}px`).toBeGreaterThan(0);
      }).toPass({ timeout: 10_000 });
      await expectNoHorizontalScroll(page);
      // A full-page capture resizes the viewport, which moves the fixed list, so the viewport proof is taken first.
      if (width === 320 || width === 390) {
        await page.screenshot({ path: testInfo.outputPath(`buildpacks-unavailable-options-viewport-${width}.png`), fullPage: false });
      }
      await page.screenshot({ path: testInfo.outputPath(`buildpacks-unavailable-options-${width}.png`), fullPage: true });
    }
    await page.setViewportSize({ width: 1280, height: 900 });
    if (!(await listbox.isVisible())) {
      await engine.focus();
      await page.keyboard.press("Space");
    }
    // A pointer can still try the disabled choice; the select ignores it, so nothing changes or is written.
    await disabledOption.click({ force: true });
    await expect(engine).toHaveText("Existing Compose / Dockerfile");
    await page.keyboard.press("Escape");
    await expect(listbox).toBeHidden();
    await expect(engine).toBeFocused();
    await expect(recipeStatus).toHaveText("No unsaved changes");
    await expect(saveRecipe).toBeDisabled();
    await expect(recipeForm.getByRole("list", { name: "Cloud Native Buildpacks resource needs" })).toHaveCount(0);
    expect(recipeWrites).toBe(0);
    expect((await recipeSettings()).recipe).toEqual(emptyRecipe);

    // Labelled fixture state: the API now correctly refuses to save this recipe, so only the saved recipe row of this
    // fixture resource is written directly, as a recipe saved while the node was still eligible would be. Capabilities,
    // capacity and memory are not touched. Only the sealed bytes reach SQL, over stdin.
    await teamQuiet();
    const sealed = sealRecipe(recipeKey, buildpacksRecipe).toString("hex");
    expect(sealed).toMatch(/^[a-f0-9]+$/);
    const seeded = execFileSync("docker", ["exec", "-i", database, "psql", "-U", "postgres", "-d", "cove", "-v", "ON_ERROR_STOP=1", "-Atq"], {
      input: `INSERT INTO workload_build_recipes(workload_id,configuration) SELECT w.id,decode('${sealed}','hex') FROM workloads w WHERE w.id='${appId}' AND w.name='buildpacks-refusal-app' AND current_database()='cove' RETURNING workload_id;\n`,
      encoding: "utf8",
      stdio: ["pipe", "pipe", "pipe"],
    });
    expect(seeded.split(/\s+/).filter((value) => value === appId)).toEqual([appId]);
    await testInfo.attach("historical-recipe-fixture.txt", {
      body: `Fixture state, not product API success: one sealed workload_build_recipes row was inserted for fixture resource ${appId} with the exact recipe ${JSON.stringify(buildpacksRecipe)}, using the fixture's generated key. Node capabilities, capacity and memory were not changed. ${summary}.`,
      contentType: "text/plain",
    });

    // The panel unseals it with its own key: the saved recipe is reported, with the node's refusal as its reason.
    const historical = await recipeSettings();
    expect({ recipe: historical.recipe, reason: historical.reason, supported: historical.buildpacks?.supported, buildpacksReason: historical.buildpacks?.reason }).toEqual({
      recipe: buildpacksRecipe,
      reason,
      supported: false,
      buildpacksReason: reason,
    });

    // The saved engine stays selected but blocked, and building is refused in the dashboard.
    await page.reload();
    await expect(engine).toHaveText(disabledLabel, { timeout: 30_000 });
    await expect(recipeForm.getByRole("combobox", { name: "Service", exact: true })).toHaveText("app");
    await expect(recipeForm.getByLabel("Source directory", { exact: true })).toHaveValue(".");
    await expect(recipeForm.getByText("The saved Cloud Native Buildpacks recipe cannot build", { exact: true })).toBeVisible();
    await expect(recipeForm.getByText(reason, { exact: true })).toBeVisible();
    const buildSection = page.getByRole("region", { name: "Build and publish", exact: true });
    await expect(buildSection.getByText("The build recipe cannot build", { exact: true })).toBeVisible();
    await expect(buildSection.getByText(`Cloud Native Buildpacks: ${reason} Change the build recipe above or resolve the problem to build.`)).toBeVisible();
    await expect(buildSection.getByRole("button", { name: "Build and publish", exact: true })).toBeDisabled();
    await expect(recipeStatus).toHaveText("No unsaved changes");
    await capture("buildpacks-saved-blocked");

    // Everything a build could start, read before and after both refusals.
    const owned = (kind: "container" | "volume" | "network") =>
      docker(kind, "ls", ...(kind === "container" ? ["--all"] : []), "-q", "--filter", "label=cove.managed=build-recipe")
        .split(/\s+/)
        .filter(Boolean)
        .sort();
    const state = async () => ({
      operations: (await operations())
        .filter((item) => item.workload_id === appId)
        .map((item) => `${item.action}:${item.id}:${item.state}`)
        .sort(),
      builds: sql(`SELECT count(*) FROM operations WHERE workload_id='${appId}' AND action='build'`),
      reservations: sql(`SELECT count(*) FROM buildpack_reservations b JOIN operations o ON o.id=b.operation_id WHERE o.workload_id='${appId}'`),
      recipeResources: { containers: owned("container"), volumes: owned("volume"), networks: owned("network") },
      images: [...new Set(docker("image", "ls", "-aq", "--no-trunc").split(/\s+/).filter(Boolean))].sort(),
      container: appContainer(),
    });
    const before = await state();
    expect({ builds: before.builds, reservations: before.reservations, recipeResources: before.recipeResources }).toEqual({ builds: "0", reservations: "0", recipeResources: { containers: [], volumes: [], networks: [] } });

    const putReply = await page.request.put(recipePath(), { headers, data: buildpacksRecipe });
    const putText = await putReply.text();
    expect(putReply.status(), putText).toBe(409);
    expect(putText.trim()).toBe(reason);
    const buildReply = await page.request.post(panelPath(`/api/workloads/${appId}/operations`), { headers, data: { action: "build", build: { repository, no_cache: true } } });
    const buildText = await buildReply.text();
    expect(buildReply.status(), buildText).toBe(409);
    expect(buildText.trim()).toBe(reason);

    await teamQuiet();
    expect(await state()).toEqual(before);
    expect(docker("exec", original.id, "cat", "/tmp/marker")).toBe("initial-release");
    expect((await recipeSettings()).recipe).toEqual(buildpacksRecipe);
    expect(recipeWrites).toBe(0);

    // Clearing the historical recipe to Compose from the dashboard is still allowed and leaves the release alone.
    await page.reload();
    await expect(engine).toHaveText(disabledLabel, { timeout: 30_000 });
    await chooseWithKeyboard(page, engine, "Existing Compose / Dockerfile");
    expect(recipeWrites).toBe(0);
    await expect(saveRecipe).toBeEnabled();
    await saveRecipe.focus();
    await page.keyboard.press("Enter");
    const dialog = page.getByRole("dialog", { name: "Change the build engine?" });
    await expect(dialog).toBeVisible();
    expect(recipeWrites).toBe(0);
    const clearReply = page.waitForResponse((response) => new URL(response.url()).pathname === recipePath() && response.request().method() === "PUT");
    await dialog.getByRole("button", { name: "Change build engine", exact: true }).focus();
    await page.keyboard.press("Enter");
    const cleared = await clearReply;
    expect(cleared.status(), await cleared.text()).toBe(200);
    expect(cleared.request().postDataJSON()).toEqual(emptyRecipe);
    expect(recipeWrites).toBe(1);
    await expect(dialog).toBeHidden();
    await expect(recipeStatus).toHaveText(savedNotice);
    expect((await recipeSettings()).recipe).toEqual(emptyRecipe);
    await page.reload();
    await expect(engine).toHaveText("Existing Compose / Dockerfile", { timeout: 30_000 });
    await expect(unavailableHint).toBeVisible();
    expect(appContainer()).toEqual(original);

    // Teardown through the product: stop and delete the app, then delete the ordinary connection.
    await teamQuiet();
    const stopReply = await page.request.post(panelPath(`/api/workloads/${appId}/operations`), { headers, data: { action: "stop" } });
    expect(stopReply.status(), await stopReply.text()).toBe(202);
    await settled(((await stopReply.json()) as { id: string }).id);
    await teamQuiet();
    const deleted = await page.request.delete(panelPath(`/api/workloads/${appId}`), { headers });
    expect(deleted.status(), await deleted.text()).toBe(200);
    appDeleted = true;
    const connectionDelete = await page.request.delete(panelPath(`/api/registries/${connectionId}`), { headers });
    expect(connectionDelete.status(), await connectionDelete.text()).toBe(200);
    connectionDeleted = true;
  } catch (error) {
    primary = asError(error);
  }
  // Cleanup never replaces the first failure; its own failure fails the test only when the test otherwise passed.
  const cleanupFailures: string[] = [];
  const attempt = async (step: string, action: () => Promise<void> | void) => {
    try {
      await action();
    } catch (error) {
      cleanupFailures.push(`${step}: ${asError(error).message}`);
    }
  };
  const quiet = async () => {
    await expect
      .poll(async () => ((await (await page.request.get(panelPath("/api/operations"))).json()) as Operation[]).some((item) => item.state === "queued" || item.state === "running"), { timeout: 120_000 })
      .toBe(false);
  };
  if (appId && !appDeleted) {
    await attempt("app stop", async () => {
      await quiet();
      const reply = await page.request.post(panelPath(`/api/workloads/${appId}/operations`), { headers, data: { action: "stop" } });
      if (reply.status() !== 202) throw new Error(`stop returned ${reply.status()}: ${await reply.text()}`);
      await quiet();
    });
    await attempt("app delete", async () => {
      const reply = await page.request.delete(panelPath(`/api/workloads/${appId}`), { headers });
      if (reply.status() !== 200) throw new Error(`delete returned ${reply.status()}: ${await reply.text()}`);
    });
  }
  if (connectionId && !connectionDeleted) {
    await attempt("connection delete", async () => {
      const reply = await page.request.delete(panelPath(`/api/registries/${connectionId}`), { headers });
      if (reply.status() !== 200) throw new Error(`connection delete returned ${reply.status()}: ${await reply.text()}`);
    });
  }
  await attempt("browser resources", () => cleanupBrowserResources());
  if (primary !== null) {
    for (const failure of cleanupFailures) {
      testInfo.annotations.push({ type: "cleanup-failure", description: failure });
      await testInfo.attach("cleanup-failure.txt", { body: failure, contentType: "text/plain" });
    }
    throw primary;
  }
  if (cleanupFailures.length > 0) throw new Error(`Cleanup failed: ${cleanupFailures.join("; ")}`);
});

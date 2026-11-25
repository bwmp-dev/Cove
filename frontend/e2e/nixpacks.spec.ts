import { execFileSync } from "node:child_process";
import { readFileSync } from "node:fs";
import { isAbsolute } from "node:path";
import { expect, test, type Locator, type Page } from "@playwright/test";
import type { Operation as ProductOperation } from "../src/api/types";
import { workerResult } from "../src/lib/operations";
import { cleanupBrowserResources, expectNoHorizontalScroll } from "./route-tls";

// Runs only with scripts/test-browser.py --managed-registry --nixpacks on the caller's owned nested daemon. The fixture
// replaces git on the one ready Linux amd64 worker with a synthetic checkout of exactly
// https://git.fixture.invalid/build-recipes.git on main holding a precompiled Cove HTTP binary and a nixpacks.toml with
// no providers and one offline build command that writes /app/build-proof. Nixpacks really plans the Dockerfile and the
// owned BuildKit really runs that step; the pinned official base image bytes come from the fixture's hash-verifying OCI
// mirror, so nothing downloads packages or toolchains. The image is pushed to the real authenticated managed registry
// over HTTPS verified against the fixture CA. No request in this spec is mocked and TLS is never skipped.
//
// Limits: this packaged fixture does not select or read build secrets (native acceptance covers those), and the
// deployed app runs under its saved Compose like any other image, so no static-server runtime hardening is claimed.
const password = process.env.COVE_BROWSER_PASSWORD ?? "";
const base = process.env.COVE_BROWSER_BASE_PATH ?? "";
const panelPath = (route: string) => `${base}${route}`;
const enabled = process.env.COVE_BROWSER_NIXPACKS === "1" && process.env.COVE_BROWSER_BUILD_RECIPES === "1" && process.env.COVE_BROWSER_MANAGED_REGISTRY === "1";

test.use({ actionTimeout: 30_000, navigationTimeout: 60_000 });

const docker = (...args: string[]) => execFileSync("docker", args, { encoding: "utf8", stdio: ["ignore", "pipe", "pipe"] }).trim();
const sizes = [320, 390, 768, 1280, 1440, 1920];
const tlsFailure = "Registry deployment finished, but authenticated TLS verification failed. Confirm DNS and the HTTPS certificate, then retry.";
const finished = /^(succeeded|failed|cancelled|interrupted)$/;
const resourceId = /^[a-f0-9]{32}$/;
const repositoryUrl = "https://git.fixture.invalid/build-recipes.git";
const source = { repository: repositoryUrl, branch: "main", compose_path: "", inline_compose: true };
const repository = "fixture/nixpacks";
const revision = "a".repeat(40);
const savedNotice = "Build recipe saved. The next build and publish uses it; the running release is unchanged.";
const marker = "nixpacks-offline-fixture";
const mib = 1024 ** 2;
const nixpacksVersion = "1.41.0";
const plannerChecksum = "sha256:0f55de7874507b9cf7502113120bd96f2ab6979f78d10eaf2eb2ade9207b3af6";
const baseImage = "ghcr.io/railwayapp/nixpacks@sha256:d45c89d80e13d7ad0fd555b5130f22a866d9dd10e861f589932303ef2314c7de";
// Never contacted: the proxy setting is refused before anything could use it.
const inertProxy = "http://proxy.fixture.invalid:3128";
const proxyReason = "Nixpacks is unavailable with an outbound build proxy: the shared BuildKit network cannot enforce proxy-only egress for generated build commands";
const draftCommand = "echo draft-build-override";

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
  capabilities: { build_publish?: boolean; build_recipes?: boolean; nixpacks?: boolean; os?: string; arch?: string } | null;
}

interface NixpacksRead {
  supported: boolean;
  reason: string;
  version: string;
  planner_sha256: string;
  planner_download_bytes: number;
  base_image: string;
  base_download_bytes: number;
  maintenance: boolean;
  proxy_supported: boolean;
  planner_memory_bytes: number;
  planner_cpu: number;
  planner_timeout_seconds: number;
  planner_output_bytes: number;
}

interface RecipeSettings {
  recipe: { engine: string; service: string; directory: string; output: string; build_command: string; start_command: string };
  can_edit: boolean;
  source_ready: boolean;
  supported: boolean;
  reason: string;
  nixpacks?: NixpacksRead;
}

const service = (lines: string[]) => ["services:", "  app:", ...lines.map((line) => `    ${line}`), ""].join("\n");
const initialLines = ["image: busybox:1.37", "pull_policy: never", "restart: unless-stopped", `command: [sh, -c, ${JSON.stringify("echo initial-release > /tmp/marker; exec sleep 86400")}]`];
const initialCompose = service(initialLines);
const buildCompose = service([...initialLines, "build:", "  context: .", "  network: none"]);
const proxyCompose = service([...initialLines, "build:", "  context: .", "  network: none", "  args:", `    HTTP_PROXY: ${JSON.stringify(inertProxy)}`]);
// No build, command or entrypoint: the image's own Nixpacks entrypoint and command run.
const deployCompose = (reference: string) => service([`image: ${JSON.stringify(reference)}`, "pull_policy: always", "restart: unless-stopped"]);
const emptyRecipe = { engine: "compose", service: "", directory: "", output: "", build_command: "", start_command: "" };
const nixpacksRecipe = { ...emptyRecipe, engine: "nixpacks", service: "app", directory: "." };

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
  if (value) await page.keyboard.type(value);
  await expect(field).toHaveValue(value);
}

const definition = (page: Page, term: string) => page.locator("dt", { hasText: new RegExp(`^${term}$`) }).locator("xpath=following-sibling::dd[1]");

test("nixpacks: an offline Nixpacks recipe builds and publishes, refuses a proxy, deploys only on request, then Compose is restored", async ({ page }, testInfo) => {
  test.skip(!enabled, "Needs scripts/test-browser.py --managed-registry --nixpacks");
  test.setTimeout(600_000);
  const { name, database, worker, daemon, hostname, certificate, key } = fixture();
  const sql = (query: string) => docker("exec", database, "psql", "-U", "postgres", "-d", "cove", "-Atc", query);
  // Background image checks start due. Deferring them keeps every operation in this team one the spec started, because
  // recipe changes and registry removal are refused while an operation is queued or running.
  const deferImageChecks = (id: string) => {
    expect(id).toMatch(resourceId);
    expect(sql(`UPDATE workloads SET next_image_check_at=now()+interval '1 day' WHERE id='${id}' RETURNING id`).split(/\s+/).filter((value) => value === id)).toEqual([id]);
  };
  const secretMarkers = ["PRIVATE KEY", key.trim(), "COVE_REGISTRY_AUTH", "cove_registry:", "$2a$", "$2b$", "$2y$", "REGISTRY_AUTH_HTPASSWD", '"auths"'];
  const expectNoSecrets = (text: string) => {
    for (const secret of secretMarkers) expect(text.includes(secret), `response contains secret-like material (${secret.slice(0, 20)})`).toBe(false);
  };
  // Diagnostics are kept even when they contain secret-like material, with that material replaced.
  const redact = (text: string) => secretMarkers.reduce((value, secret) => value.split(secret).join("[redacted]"), text);
  const capture = async (label: string) => {
    expect(await page.locator("body").innerText()).not.toContain("-----BEGIN");
    for (const width of sizes) {
      await page.setViewportSize({ width, height: 900 });
      await expectNoHorizontalScroll(page);
      await page.screenshot({ path: testInfo.outputPath(`${label}-${width}.png`), fullPage: true });
    }
    await page.setViewportSize({ width: 1280, height: 900 });
  };
  const probeLabel = `dev.cove.nixpacks-probe=${name}`;

  let registryId = "";
  let appId = "";
  let appDeleted = false;
  let registryRecordDeleted = false;
  let headers: Record<string, string> = {};
  let primary: Error | null = null;
  try {
    // The worker, its owner marker, both daemon views and the OCI mirror must be the fixture's before anything runs.
    expect(docker("info", "--format", "{{.ID}}")).toBe(daemon);
    expect(docker("inspect", "--format", "{{.Config.Image}}", worker)).toBe(`${worker}:development`);
    expect(docker("exec", worker, "cat", "/var/lib/cove/.browser-owner")).toBe(name);
    expect(docker("exec", worker, "docker", "info", "--format", "{{.ID}}")).toBe(daemon);
    expect(docker("inspect", "--format", '{{index .Config.Labels "cove.browser-fixture"}}|{{.State.Running}}', `${name}-nixpacks-mirror`)).toBe(`${name}|true`);

    await page.goto(panelPath("/projects"));
    await expect(page).toHaveURL(/\/login(?:[?#]|$)/);
    await page.getByLabel("Password", { exact: true }).fill(password);
    await page.getByRole("button", { name: "Sign in", exact: true }).click();
    await expect(page).not.toHaveURL(/\/login(?:[?#]|$)/);
    headers = { Origin: new URL(page.url()).origin };

    const nodesReply = await page.request.get(panelPath("/api/nodes"));
    const nodesText = await nodesReply.text();
    expect(nodesReply.status(), nodesText).toBe(200);
    expectNoSecrets(nodesText);
    const nodes = JSON.parse(nodesText) as NodeItem[];
    expect(nodes.map((item) => item.name)).toEqual(["ready-browser-worker"]);
    const node = nodes[0];
    if (!node) throw new Error("The browser worker is missing");
    expect(node.state).toBe("ready");
    expect({ publish: node.capabilities?.build_publish, recipes: node.capabilities?.build_recipes, nixpacks: node.capabilities?.nixpacks, os: node.capabilities?.os, arch: node.capabilities?.arch }).toEqual({
      publish: true,
      recipes: true,
      nixpacks: true,
      os: "linux",
      arch: "amd64",
    });

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
    const operationLogs = async (id: string) => {
      const response = await page.request.get(panelPath(`/api/operations/${id}/logs`));
      const text = await response.text();
      expect(response.status(), text).toBe(200);
      expectNoSecrets(text);
      return (JSON.parse(text) as { logs: string }).logs;
    };
    const settled = async (id: string, timeout = 180_000) => {
      await expect.poll(async () => (await operation(id)).state, { timeout }).toMatch(finished);
      const result = await operation(id);
      if (result.state !== "succeeded") {
        // Cleanup deletes the resources and their logs, so the failed operation's log tail is kept before throwing.
        let diagnostic: string;
        try {
          const response = await page.request.get(panelPath(`/api/operations/${id}/logs`));
          const text = await response.text();
          if (response.status() !== 200) throw new Error(`log request returned ${response.status()}`);
          const logs = redact((JSON.parse(text) as { logs: string }).logs);
          diagnostic = logs.length > 4_000 ? `...${logs.slice(-4_000)}` : logs;
          await testInfo.attach(`operation-${id}-log.txt`, { body: diagnostic, contentType: "text/plain" });
        } catch (error) {
          diagnostic = `log unavailable: ${redact(error instanceof Error ? error.message.slice(0, 300) : "unknown error")}`;
        }
        throw new Error(`${result.action} ${id} ended ${result.state}: ${redact(result.result?.error ?? "")}\n--- operation log (tail) ---\n${diagnostic}`);
      }
      return result;
    };
    const teamQuiet = () =>
      expect
        .poll(async () => (await operations()).filter((item) => item.state === "queued" || item.state === "running").map((item) => `${item.action}:${item.id}`), { timeout: 120_000 })
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
    // Only fields an update accepts, with the Git source preserved and no placement fields.
    const saveCompose = async (compose: string) => {
      await teamQuiet();
      const reply = await page.request.put(panelPath(`/api/workloads/${appId}`), { headers, data: { name: "nixpacks-app", node_id: node.id, kind: "service", compose, source } });
      expect(reply.status(), await reply.text()).toBe(200);
      const saved = await workload();
      expect(saved.compose).toBe(compose);
      expect(saved.source).toMatchObject({ repository: repositoryUrl, branch: "main", inline_compose: true });
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

    // 1. A project and an image-only BusyBox service with the explicit Git source, deployed from the cached image.
    const projectReply = await page.request.post(panelPath("/api/projects"), { headers, data: { name: `Nixpacks ${Date.now()}` } });
    expect(projectReply.ok(), await projectReply.text()).toBe(true);
    const project = (await projectReply.json()) as { id: string };
    const appReply = await page.request.post(panelPath("/api/workloads"), { headers, data: { name: "nixpacks-app", node_id: node.id, kind: "service", compose: initialCompose, source, project_id: project.id } });
    expect(appReply.status(), await appReply.text()).toBe(201);
    appId = ((await appReply.json()) as { id: string }).id;
    deferImageChecks(appId);
    const created = await workload();
    expect(created.compose).toBe(initialCompose);
    expect(created.source).toMatchObject({ repository: repositoryUrl, branch: "main", inline_compose: true });
    const firstDeploy = await page.request.post(panelPath(`/api/workloads/${appId}/operations`), { headers, data: { action: "deploy" } });
    expect(firstDeploy.status(), await firstDeploy.text()).toBe(202);
    await settled(((await firstDeploy.json()) as { id: string }).id);
    await teamQuiet();
    const original = appContainer();
    expect(original.image).toBe("busybox:1.37");
    expect(docker("exec", original.id, "cat", "/tmp/marker")).toBe("initial-release");

    // 2. A managed registry on the node, repaired with the fixture leaf until it is ready, selected for the service.
    const retention = { keep_per_repository: 10, minimum_age_days: 30 };
    const createdReply = await page.request.post(panelPath("/api/managed-registries"), { headers, data: { name: `Nixpacks registry ${Date.now()}`, node_id: node.id, project_id: project.id, hostname, retention } });
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
    await teamQuiet();
    const connect = await page.request.put(panelPath(`/api/workloads/${appId}/registry`), { headers, data: { registry_id: ready.connection_id } });
    expect(connect.status(), await connect.text()).toBe(200);
    expect(await (await page.request.get(panelPath(`/api/workloads/${appId}/registry`))).json()).toEqual({ registry_id: ready.connection_id });

    // The service gains a no-network build before any engine is chosen. Saving it leaves the release running.
    await saveCompose(buildCompose);
    expect(appContainer()).toEqual(original);

    // The server's Nixpacks read is the only source of the versions, pins and limits the dashboard discloses.
    const initialSettings = await recipeSettings();
    expect({ recipe: initialSettings.recipe, source_ready: initialSettings.source_ready, can_edit: initialSettings.can_edit, supported: initialSettings.supported, reason: initialSettings.reason }).toEqual({
      recipe: emptyRecipe,
      source_ready: true,
      can_edit: true,
      supported: true,
      reason: "",
    });
    const nixpacks = initialSettings.nixpacks;
    if (!nixpacks) throw new Error("The build recipe read did not report Nixpacks eligibility");
    expect(nixpacks).toEqual({
      supported: true,
      reason: "",
      version: nixpacksVersion,
      planner_sha256: plannerChecksum,
      planner_download_bytes: 3_717_141,
      base_image: baseImage,
      base_download_bytes: 138_966_568,
      maintenance: true,
      proxy_supported: false,
      planner_memory_bytes: 512 * mib,
      planner_cpu: 1,
      planner_timeout_seconds: 60,
      planner_output_bytes: 128 * mib,
    });

    // 3. Choose Nixpacks from the keyboard. Editing alone saves nothing.
    await page.goto(panelPath(`/resources/${appId}/registry`));
    const recipeForm = page.getByRole("form", { name: "Build recipe setting" });
    const engine = recipeForm.getByRole("combobox", { name: "Build engine", exact: true });
    const serviceSelect = recipeForm.getByRole("combobox", { name: "Service", exact: true });
    const directory = recipeForm.getByLabel("Source directory", { exact: true });
    const buildCommand = recipeForm.getByLabel("Build commandOptional", { exact: true });
    const startCommand = recipeForm.getByLabel("Start commandOptional", { exact: true });
    const recipeStatus = recipeForm.getByRole("status");
    const saveRecipe = recipeForm.getByRole("button", { name: "Save build recipe", exact: true });
    await expect(engine).toHaveText("Existing Compose / Dockerfile", { timeout: 30_000 });
    await expect(recipeStatus).toHaveText("No unsaved changes");
    await capture("nixpacks-default");
    await chooseWithKeyboard(page, engine, "Nixpacks");
    await chooseWithKeyboard(page, serviceSelect, "app");
    await typeWithKeyboard(page, directory, ".");
    await expect(recipeForm.getByLabel("Output directory")).toHaveCount(0);
    await expect(buildCommand).toHaveValue("");
    await expect(startCommand).toHaveValue("");
    await expect(recipeStatus).toHaveText("Unsaved changes");
    expect(recipeWrites).toBe(0);
    expect((await recipeSettings()).recipe).toEqual(emptyRecipe);

    // The disclosure reports the server's figures and the upstream limits.
    const expectDisclosure = async () => {
      await expect(recipeForm.getByText("Nixpacks is in maintenance mode", { exact: true })).toBeVisible();
      await expect(recipeForm.getByText(/Nixpacks is in maintenance mode upstream, and its maintainers recommend Railpack instead\./)).toBeVisible();
      await expect(recipeForm.getByText(/normal builds download Nix, system and language packages from upstream/)).toBeVisible();
      await expect(recipeForm.getByText(/so Nixpacks builds are neither reproducible nor fully pinned/)).toBeVisible();
      await expect(recipeForm.getByText(/Planning runs offline in an unprivileged, bounded container with no Docker socket, no Git, registry or host credentials and no Cove secret values\./)).toBeVisible();
      await expect(recipeForm.getByText(/Outbound build proxies are unavailable because the shared builder cannot enforce proxy-only egress/)).toBeVisible();
      const limits = recipeForm.getByRole("list", { name: "Nixpacks resource limits" });
      await expect(limits).toContainText("Planning is limited to 512 MiB of memory, 1 CPU and 60 seconds.");
      await expect(limits).toContainText("Up to 128 MiB of planner output is held in temporary memory within that limit");
      await expect(limits).toContainText("The planner archive is about 3.7 MB to download and the base image about 0.14 GB.");
      await expect(recipeForm.getByText(nixpacksVersion, { exact: true })).toBeVisible();
      const pins = recipeForm.locator('dl[aria-label="Nixpacks pins"]');
      await expect(pins.locator("dt")).toHaveText(["Planner archive checksum", "Base image"]);
      await expect(pins.locator("dd")).toHaveText([plannerChecksum, baseImage]);
    };
    await expectDisclosure();

    // Saving asks to confirm the engine change, then sends exactly the six recipe fields. The success notice survives
    // the dashboard's own refetch of the saved recipe.
    const confirmEngine = async (expected: typeof emptyRecipe) => {
      await expect(saveRecipe).toBeEnabled();
      await saveRecipe.focus();
      await page.keyboard.press("Enter");
      const dialog = page.getByRole("dialog", { name: "Change the build engine?" });
      await expect(dialog).toBeVisible();
      if (expected.engine === "nixpacks") {
        await expect(dialog).toContainText("Nixpacks is in maintenance mode upstream, which recommends Railpack.");
        await expect(dialog).toContainText("outbound build proxies are unavailable");
      }
      expect(recipeWrites).toBe(0);
      let saved = false;
      const reply = page.waitForResponse((response) => {
        const matches = new URL(response.url()).pathname === recipePath() && response.request().method() === "PUT";
        if (matches) saved = true;
        return matches;
      });
      const refetched = page.waitForResponse((response) => saved && new URL(response.url()).pathname === recipePath() && response.request().method() === "GET");
      await dialog.getByRole("button", { name: "Change build engine", exact: true }).focus();
      await page.keyboard.press("Enter");
      const response = await reply;
      expect(response.status(), await response.text()).toBe(200);
      expect(response.request().postDataJSON()).toEqual(expected);
      expect(recipeWrites).toBe(1);
      recipeWrites = 0;
      await expect(dialog).toBeHidden();
      await expect(recipeStatus).toHaveText(savedNotice);
      const refetch = await refetched;
      expect(refetch.status()).toBe(200);
      expect(((await refetch.json()) as RecipeSettings).recipe).toEqual(expected);
      await expect(recipeStatus).toHaveText(savedNotice);
      expect((await recipeSettings()).recipe).toEqual(expected);
    };
    await confirmEngine(nixpacksRecipe);
    expect(appContainer()).toEqual(original);
    expect((await workload()).compose).toBe(buildCompose);
    await page.reload();
    await expect(engine).toHaveText("Nixpacks", { timeout: 30_000 });
    await expect(serviceSelect).toHaveText("app");
    await expect(directory).toHaveValue(".");
    await expect(buildCommand).toHaveValue("");
    await expect(startCommand).toHaveValue("");
    await expectDisclosure();
    await capture("nixpacks-saved");
    expect(appContainer()).toEqual(original);

    // 4. Build and publish from the keyboard with the unchanged build payload. Nothing is deployed.
    const buildSection = page.getByRole("region", { name: "Build and publish", exact: true });
    const buildButton = buildSection.getByRole("button", { name: "Build and publish", exact: true });
    await typeWithKeyboard(page, page.getByLabel("Repository", { exact: true }), repository);
    const noCache = page.getByRole("checkbox", { name: "Bypass the build cache" });
    await noCache.focus();
    await page.keyboard.press("Space");
    await expect(noCache).toBeChecked();
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
    expect({ engine: recorded.engine, service: recorded.service, version: recorded.version, pinned: recorded.pinned_images, planner: recorded.planner_sha256, runtime: recorded.static_runtime }).toEqual({
      engine: "nixpacks",
      service: "app",
      version: nixpacksVersion,
      pinned: [nixpacks.base_image],
      planner: nixpacks.planner_sha256,
      runtime: undefined,
    });
    const images = builtResult?.build?.images ?? [];
    expect(images.map((image) => image.service)).toEqual(["app"]);
    const [image] = images;
    if (!image) throw new Error("The build reported no published image");
    expect(image.reference).toMatch(new RegExp(`^${hostname.replace(/[.]/g, "\\.")}/${repository}@sha256:[a-f0-9]{64}$`));
    expect(image.image_id).toMatch(/^sha256:[a-f0-9]{64}$/);
    const reference = image.reference;
    const logs = await operationLogs(buildId);
    expect(logs).toContain(`Published app as ${reference}`);
    expect(appContainer()).toEqual(original);
    expect((await workload()).compose).toBe(buildCompose);

    // The worker removes the operation's planner and build-stage resources by their ownership labels.
    const owned = (kind: "container" | "volume" | "network", operationId?: string) =>
      docker(kind, "ls", ...(kind === "container" ? ["--all"] : []), "-q", "--filter", "label=cove.managed=build-recipe", ...(operationId ? ["--filter", `label=cove.operation=${operationId}`] : []))
        .split(/\s+/)
        .filter(Boolean)
        .sort();
    await expect.poll(() => ({ containers: owned("container", buildId), volumes: owned("volume", buildId), networks: owned("network", buildId) }), { timeout: 30_000 }).toEqual({ containers: [], volumes: [], networks: [] });

    // The history and the operation page report what this build recorded, never today's settings or command text.
    const engineText = `Nixpacks ${nixpacksVersion} for app`;
    const checksumText = `Planner archive checksum ${nixpacks.planner_sha256}`;
    const expectHistory = async () => {
      await expect(buildSection.getByRole("heading", { name: "Latest successful build", exact: true })).toBeVisible({ timeout: 30_000 });
      await expect(buildSection.getByText(engineText, { exact: true }).first()).toBeVisible();
      await expect(buildSection.getByText(checksumText, { exact: true }).first()).toBeVisible();
      await expect(buildSection.getByRole("list", { name: "Published images" }).getByText(reference, { exact: true })).toBeVisible();
    };
    const expectOperation = async () => {
      await page.goto(panelPath(`/operations/${buildId}`));
      const engineValue = definition(page, "Build engine");
      await expect(engineValue.getByText(engineText, { exact: true })).toBeVisible({ timeout: 30_000 });
      await expect(engineValue.getByText(checksumText, { exact: true })).toBeVisible();
      await expect(engineValue.getByRole("list", { name: "Pinned build images" }).getByRole("listitem")).toHaveText([nixpacks.base_image]);
      expect(await engineValue.innerText()).not.toMatch(/command/i);
      await expect(page.getByRole("region", { name: "Operation logs", exact: true })).toContainText(`Published app as ${reference}`);
    };
    await page.reload();
    await expect(engine).toHaveText("Nixpacks", { timeout: 30_000 });
    await expect(serviceSelect).toHaveText("app");
    await expect(directory).toHaveValue(".");
    await expectHistory();
    await capture("nixpacks-history");
    await expectOperation();
    await capture("nixpacks-operation");

    // 5. A configured build proxy is saved with the Compose file but makes the saved recipe refuse to build.
    await saveCompose(proxyCompose);
    expect(appContainer()).toEqual(original);
    const refused = await recipeSettings();
    expect({ recipe: refused.recipe, reason: refused.reason, nixpacksSupported: refused.nixpacks?.supported }).toEqual({ recipe: nixpacksRecipe, reason: proxyReason, nixpacksSupported: true });
    await page.goto(panelPath(`/resources/${appId}/registry`));
    await expect(engine).toHaveText("Nixpacks", { timeout: 30_000 });
    const savedBlocker = recipeForm.getByText("The saved Nixpacks recipe cannot build", { exact: true });
    await expect(savedBlocker).toBeVisible();
    await expect(recipeForm.getByText(proxyReason, { exact: true })).toBeVisible();
    const buildBlocker = buildSection.getByText("The build recipe cannot build", { exact: true });
    await expect(buildBlocker).toBeVisible();
    await expect(buildSection.getByText(proxyReason, { exact: true })).toBeVisible();
    await expect(buildSection.getByText("Change the build recipe above or resolve the problem to build.", { exact: true })).toBeVisible();
    await expect(buildButton).toBeDisabled();

    // Everything a build could start, read before and after both refusals.
    const state = async () => ({
      operations: (await operations())
        .filter((item) => item.workload_id === appId)
        .map((item) => `${item.action}:${item.id}:${item.state}`)
        .sort(),
      builds: sql(`SELECT count(*) FROM operations WHERE workload_id='${appId}' AND action='build'`),
      recipeResources: { containers: owned("container"), volumes: owned("volume"), networks: owned("network") },
      images: [...new Set(docker("image", "ls", "-aq", "--no-trunc").split(/\s+/).filter(Boolean))].sort(),
      container: appContainer(),
    });
    await teamQuiet();
    const before = await state();
    expect({ builds: before.builds, recipeResources: before.recipeResources }).toEqual({ builds: "1", recipeResources: { containers: [], volumes: [], networks: [] } });
    const buildReply = await page.request.post(panelPath(`/api/workloads/${appId}/operations`), { headers, data: { action: "build", build: { repository, no_cache: true } } });
    const buildText = await buildReply.text();
    expect(buildReply.status(), buildText).toBe(400);
    expect(buildText.trim()).toBe(proxyReason);

    // Saving a command override in the dashboard is refused with the same reason and the draft is kept.
    await typeWithKeyboard(page, buildCommand, draftCommand);
    await expect(recipeStatus).toHaveText("Unsaved changes");
    expect(recipeWrites).toBe(0);
    const refusedSave = respondedTo(recipePath(), "PUT");
    await saveRecipe.focus();
    await page.keyboard.press("Enter");
    const refusedReply = await refusedSave;
    expect(refusedReply.status(), await refusedReply.text()).toBe(400);
    expect((await refusedReply.text()).trim()).toBe(proxyReason);
    expect(refusedReply.request().postDataJSON()).toEqual({ ...nixpacksRecipe, build_command: draftCommand });
    expect(recipeWrites).toBe(1);
    recipeWrites = 0;
    await expect(page.getByRole("dialog", { name: "Change the build engine?" })).toHaveCount(0);
    const saveError = recipeForm.getByText("The build recipe was not saved", { exact: true });
    await expect(saveError).toBeVisible();
    await expect(recipeForm.getByText("Your changes are kept.", { exact: true })).toBeVisible();
    await expect(recipeForm.getByText(proxyReason, { exact: true }).first()).toBeVisible();
    await expect(buildCommand).toHaveValue(draftCommand);
    await expect(recipeStatus).toHaveText("Unsaved changes");
    await expect(recipeForm.getByText(savedNotice, { exact: true })).toHaveCount(0);
    await capture("nixpacks-refused");
    await teamQuiet();
    expect(await state()).toEqual(before);
    expect(docker("exec", original.id, "cat", "/tmp/marker")).toBe("initial-release");
    expect((await recipeSettings()).recipe).toEqual(nixpacksRecipe);

    // Discarding the draft and restoring the no-network build clears the refusal without touching the release.
    await recipeForm.getByRole("button", { name: "Discard changes", exact: true }).focus();
    await page.keyboard.press("Enter");
    await expect(buildCommand).toHaveValue("");
    await expect(recipeStatus).toHaveText("No unsaved changes");
    await expect(saveError).toHaveCount(0);
    expect(recipeWrites).toBe(0);
    await saveCompose(buildCompose);
    expect((await recipeSettings()).reason).toBe("");
    await page.reload();
    await expect(engine).toHaveText("Nixpacks", { timeout: 30_000 });
    await expect(savedBlocker).toHaveCount(0);
    await expect(buildBlocker).toHaveCount(0);
    expect(appContainer()).toEqual(original);

    // 6. Deploy the immutable reference explicitly, keeping the Git source. Saving leaves the original container running.
    const compose = deployCompose(reference);
    await saveCompose(compose);
    const afterSave = await workload();
    expect(afterSave.compose).not.toMatch(/build|command|entrypoint/);
    expect(appContainer()).toEqual(original);
    const deployReply = await page.request.post(panelPath(`/api/workloads/${appId}/operations`), { headers, data: { action: "deploy" } });
    expect(deployReply.status(), await deployReply.text()).toBe(202);
    await settled(((await deployReply.json()) as { id: string }).id);
    await teamQuiet();
    const deployed = appContainer();
    expect(deployed.id).not.toBe(original.id);
    expect(deployed.image).toBe(reference);
    const containerConfig = docker("inspect", "--format", "{{json .Config.Entrypoint}}|{{json .Config.Cmd}}", deployed.id);
    expect(containerConfig).toBe(docker("image", "inspect", "--format", "{{json .Config.Entrypoint}}|{{json .Config.Cmd}}", deployed.imageId ?? ""));

    // Probe the app's internal HTTP from an owned container on its Compose network in the same nested daemon.
    const networkName = `cove-${appId}_default`;
    expect(docker("network", "inspect", "--format", '{{index .Labels "com.docker.compose.project"}}', networkName)).toBe(`cove-${appId}`);
    const address = docker("inspect", "--format", `{{(index .NetworkSettings.Networks ${JSON.stringify(networkName)}).IPAddress}}`, deployed.id);
    expect(address).toMatch(/^\d+\.\d+\.\d+\.\d+$/);
    let probes = 0;
    const probe = () =>
      docker("run", "--rm", "--pull", "never", "--name", `${name}-nixpacks-probe-${++probes}`, "--network", networkName, "--label", `cove.browser-fixture=${name}`, "--label", probeLabel, "busybox:1.37", "sh", "-c", 'wget -q -O- "$1" 2>&1; true', "sh", `http://${address}:8080/`);
    await expect.poll(probe, { timeout: 30_000 }).toBe(marker);
    const running = appContainer();

    // 7. Clear the recipe back to Compose from the keyboard. The recorded history still names Nixpacks and its checksum.
    await page.goto(panelPath(`/resources/${appId}/registry`));
    await expect(engine).toHaveText("Nixpacks", { timeout: 30_000 });
    await chooseWithKeyboard(page, engine, "Existing Compose / Dockerfile");
    expect(recipeWrites).toBe(0);
    await confirmEngine(emptyRecipe);
    await page.reload();
    await expect(engine).toHaveText("Existing Compose / Dockerfile", { timeout: 30_000 });
    await expect(serviceSelect).toHaveCount(0);
    await expect(recipeForm.getByRole("list", { name: "Nixpacks resource limits" })).toHaveCount(0);
    await expectHistory();
    expect(appContainer()).toEqual(running);
    await capture("nixpacks-cleared");
    await expectOperation();

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
      cleanupFailures.push(`${step}: ${redact(asError(error).message)}`);
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
  // Only containers carrying both this fixture's owner label and this spec's probe label are removed.
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

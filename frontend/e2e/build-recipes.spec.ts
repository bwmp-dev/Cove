import { execFileSync } from "node:child_process";
import { readFileSync } from "node:fs";
import { isAbsolute } from "node:path";
import { expect, test, type Locator, type Page } from "@playwright/test";
import { cleanupBrowserResources, expectNoHorizontalScroll } from "./route-tls";

// Runs only with scripts/test-browser.py --managed-registry --build-recipes on the caller's owned nested daemon. The
// fixture replaces git on the one ready worker with a synthetic checkout of a local directory for exactly
// https://git.fixture.invalid/build-recipes.git on main; no Git provider is contacted. The static build itself is real:
// the worker's Compose and BuildKit build the image and push it to the real authenticated managed registry over HTTPS
// verified against the fixture CA. No request in this spec is mocked and TLS verification is never skipped.
//
// Limit: this spec saves a Railpack recipe and checks what the dashboard reports for it, but never builds with Railpack.
// Real Railpack builds (offline native build, owned proxy and network none metadata) are accepted separately.
const base = process.env.COVE_BROWSER_BASE_PATH ?? "";
const panelPath = (route: string) => `${base}${route}`;
const password = process.env.COVE_BROWSER_PASSWORD ?? "";
const enabled = process.env.COVE_BROWSER_BUILD_RECIPES === "1" && process.env.COVE_BROWSER_MANAGED_REGISTRY === "1";

test.use({ actionTimeout: 30_000, navigationTimeout: 60_000 });

const docker = (...args: string[]) => execFileSync("docker", args, { encoding: "utf8", stdio: ["ignore", "pipe", "pipe"] }).trim();
const sizes = [320, 390, 768, 1280, 1440, 1920];
const tlsFailure = "Registry deployment finished, but authenticated TLS verification failed. Confirm DNS and the HTTPS certificate, then retry.";
const finished = /^(succeeded|failed|cancelled|interrupted)$/;
const resourceId = /^[a-f0-9]{32}$/;
const repositoryUrl = "https://git.fixture.invalid/build-recipes.git";
const source = { repository: repositoryUrl, branch: "main", compose_path: "", inline_compose: true };
const repository = "fixture/static";
const revision = "a".repeat(40);
const savedNotice = "Build recipe saved. The next build and publish uses it; the running release is unchanged.";

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

interface BuiltImage {
  service: string;
  image_id: string;
  reference: string;
}

interface Operation {
  id: string;
  action: string;
  state: string;
  workload_id: string;
  execution_node_id?: string;
  result: {
    error?: string;
    revision?: string;
    build?: { images: BuiltImage[] };
    build_recipe?: { engine: string; service?: string; version?: string; pinned_images?: string[]; static_runtime?: string };
  } | null;
}

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
  capabilities: { build_publish?: boolean; build_recipes?: boolean; os?: string } | null;
}

interface RecipeSettings {
  recipe: { engine: string; service: string; directory: string; output: string; build_command: string; start_command: string };
  can_edit: boolean;
  source_ready: boolean;
  supported: boolean;
  reason: string;
  railpack_version: string;
  pinned_images: string[];
}

const service = (lines: string[]) => ["services:", "  app:", ...lines.map((line) => `    ${line}`), ""].join("\n");
const initialCompose = service(["image: busybox:1.37", "pull_policy: never", "restart: unless-stopped", `command: [sh, -c, ${JSON.stringify("echo initial-release > /tmp/marker; exec sleep 86400")}]`]);
const deployCompose = (reference: string) => service([`image: ${JSON.stringify(reference)}`, "pull_policy: always", "restart: unless-stopped"]);
const emptyRecipe = { engine: "compose", service: "", directory: "", output: "", build_command: "", start_command: "" };
const staticRecipe = { ...emptyRecipe, engine: "static", service: "app", directory: ".", output: "public" };
const railpackRecipe = { ...emptyRecipe, engine: "railpack", service: "app", directory: "." };

const asError = (error: unknown) => (error instanceof Error ? error : new Error("A non-error value was thrown", { cause: error }));

// Opens a Select from the keyboard and moves to the named option before choosing it with Enter.
async function chooseWithKeyboard(page: Page, combobox: Locator, option: string) {
  await combobox.focus();
  await page.keyboard.press("Space");
  const listbox = page.getByRole("listbox");
  await expect(listbox).toBeVisible();
  const target = listbox.getByRole("option", { name: option, exact: true });
  await expect(target).toHaveCount(1);
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

test("build recipes: explicit static output builds and publishes, deploys only on request, then Railpack and Compose are saved", async ({ page }, testInfo) => {
  test.skip(!enabled, "Needs scripts/test-browser.py --managed-registry --build-recipes");
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
    for (const marker of secretMarkers) expect(text.includes(marker), `response contains secret-like material (${marker.slice(0, 20)})`).toBe(false);
  };
  const capture = async (label: string) => {
    expect(await page.locator("body").innerText()).not.toContain("-----BEGIN");
    for (const width of sizes) {
      await page.setViewportSize({ width, height: 900 });
      await expectNoHorizontalScroll(page);
      await page.screenshot({ path: testInfo.outputPath(`${label}-${width}.png`), fullPage: true });
    }
    await page.setViewportSize({ width: 1280, height: 900 });
  };
  const probeLabel = `dev.cove.recipe-probe=${name}`;

  let registryId = "";
  let appId = "";
  let appDeleted = false;
  let registryRecordDeleted = false;
  let headers: Record<string, string> = {};
  let primary: Error | null = null;
  try {
    expect(docker("info", "--format", "{{.ID}}")).toBe(daemon);
    expect(docker("inspect", "--format", "{{.Config.Image}}", worker)).toBe(`${worker}:development`);
    expect(docker("exec", worker, "cat", "/var/lib/cove/.browser-owner")).toBe(name);
    expect(docker("exec", worker, "docker", "info", "--format", "{{.ID}}")).toBe(daemon);

    await page.goto(panelPath("/projects"));
    await expect(page).toHaveURL(/\/login(?:[?#]|$)/);
    await page.getByLabel("Password", { exact: true }).fill(password);
    await page.getByRole("button", { name: "Sign in", exact: true }).click();
    await expect(page).not.toHaveURL(/\/login(?:[?#]|$)/);
    headers = { Origin: new URL(page.url()).origin };

    const nodesReply = await page.request.get(panelPath("/api/nodes"));
    expect(nodesReply.status(), await nodesReply.text()).toBe(200);
    const nodes = (await nodesReply.json()) as NodeItem[];
    expect(nodes.map((item) => item.name)).toEqual(["ready-browser-worker"]);
    const node = nodes[0];
    if (!node) throw new Error("The browser worker is missing");
    expect(node.state).toBe("ready");
    expect(node.capabilities?.build_publish).toBe(true);
    expect(node.capabilities?.build_recipes).toBe(true);
    expect(node.capabilities?.os).toBe("linux");

    const operation = async (id: string) => {
      const response = await page.request.get(panelPath(`/api/operations/${id}`));
      const text = await response.text();
      expect(response.status(), text).toBe(200);
      expectNoSecrets(text);
      return JSON.parse(text) as Operation;
    };
    const settled = async (id: string, timeout = 180_000) => {
      await expect.poll(async () => (await operation(id)).state, { timeout }).toMatch(finished);
      const result = await operation(id);
      if (result.state !== "succeeded") {
        // Cleanup deletes the resources and their logs, so the failed operation's log is kept before throwing.
        let diagnostic: string;
        try {
          const response = await page.request.get(panelPath(`/api/operations/${id}/logs`));
          const text = await response.text();
          if (response.status() !== 200) throw new Error(`log request returned ${response.status()}`);
          expectNoSecrets(text);
          const logs = (JSON.parse(text) as { logs: string }).logs;
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
    const projectReply = await page.request.post(panelPath("/api/projects"), { headers, data: { name: `Build recipes ${Date.now()}` } });
    expect(projectReply.ok(), await projectReply.text()).toBe(true);
    const project = (await projectReply.json()) as { id: string };
    const retention = { keep_per_repository: 10, minimum_age_days: 30 };
    const createdReply = await page.request.post(panelPath("/api/managed-registries"), { headers, data: { name: `Recipe registry ${Date.now()}`, node_id: node.id, project_id: project.id, hostname, retention } });
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
    const generated = (JSON.parse(connectionsText) as RegistryConnection[]).filter((item) => item.managed_registry_id);
    expect(generated.map((item) => ({ id: item.id, managed: item.managed_registry_id, server: item.registry?.server, password: item.registry?.password }))).toEqual([{ id: ready.connection_id, managed: registryId, server: hostname, password: "" }]);

    // An image-only BusyBox service whose Git source uses the saved Compose file, deployed from the cached image.
    const appReply = await page.request.post(panelPath("/api/workloads"), { headers, data: { name: "recipe-app", node_id: node.id, kind: "service", compose: initialCompose, source, project_id: project.id } });
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
    const initialSettings = await recipeSettings();
    expect({ recipe: initialSettings.recipe, source_ready: initialSettings.source_ready, supported: initialSettings.supported, reason: initialSettings.reason, can_edit: initialSettings.can_edit }).toEqual({ recipe: emptyRecipe, source_ready: true, supported: true, reason: "", can_edit: true });

    // Choose static output from the keyboard. Editing alone saves nothing.
    await page.goto(panelPath(`/resources/${appId}/registry`));
    const recipeForm = page.getByRole("form", { name: "Build recipe setting" });
    const engine = recipeForm.getByRole("combobox", { name: "Build engine", exact: true });
    await expect(engine).toHaveText("Existing Compose / Dockerfile", { timeout: 30_000 });
    await capture("build-recipes-default");
    await chooseWithKeyboard(page, engine, "Static output");
    await chooseWithKeyboard(page, recipeForm.getByRole("combobox", { name: "Service", exact: true }), "app");
    const directory = recipeForm.getByLabel("Source directory", { exact: true });
    const output = recipeForm.getByLabel("Output directory", { exact: true });
    await typeWithKeyboard(page, directory, ".");
    await typeWithKeyboard(page, output, "public");
    const recipeStatus = recipeForm.getByRole("status");
    await expect(recipeStatus).toHaveText("Unsaved changes");
    expect(recipeWrites).toBe(0);
    expect((await recipeSettings()).recipe).toEqual(emptyRecipe);

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
    await confirmEngine(staticRecipe);
    expect(appContainer()).toEqual(original);
    await page.reload();
    await expect(engine).toHaveText("Static output", { timeout: 30_000 });
    await expect(recipeForm.getByRole("combobox", { name: "Service", exact: true })).toHaveText("app");
    await expect(directory).toHaveValue(".");
    await expect(output).toHaveValue("public");
    await capture("build-recipes-static");
    expect(appContainer()).toEqual(original);

    // Build and publish from the keyboard. The build is queued only; nothing is deployed.
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
    expect(built.result?.revision).toBe(revision);
    const recorded = built.result?.build_recipe;
    expect({ engine: recorded?.engine, service: recorded?.service, version: recorded?.version, pinned: recorded?.pinned_images }).toEqual({ engine: "static", service: "app", version: undefined, pinned: undefined });
    expect(recorded?.static_runtime).toMatch(/^sha256:[a-f0-9]{64}$/);
    const images = built.result?.build?.images ?? [];
    expect(images.map((image) => image.service)).toEqual(["app"]);
    const [image] = images;
    if (!image) throw new Error("The build reported no published image");
    expect(image.reference).toMatch(new RegExp(`^${hostname.replace(/[.]/g, "\\.")}/${repository}@sha256:[a-f0-9]{64}$`));
    expect(image.image_id).toMatch(/^sha256:[a-f0-9]{64}$/);
    const reference = image.reference;
    expect(appContainer()).toEqual(original);
    expect((await workload()).compose).toBe(initialCompose);

    // The history and the operation page report the recorded engine, never command text.
    await page.reload();
    const buildSection = page.getByRole("region", { name: "Build and publish", exact: true });
    await expect(buildSection.getByRole("heading", { name: "Latest successful build", exact: true })).toBeVisible({ timeout: 30_000 });
    await expect(buildSection.getByText("Static output for app", { exact: true })).toBeVisible();
    await expect(buildSection.getByRole("list", { name: "Published images" }).getByText(reference, { exact: true })).toBeVisible();
    await capture("build-recipes-history");
    await page.goto(panelPath(`/operations/${buildId}`));
    const engineValue = definition(page, "Build engine");
    await expect(engineValue.getByText("Static output for app", { exact: true })).toBeVisible({ timeout: 30_000 });
    await expect(engineValue.getByText(recorded?.static_runtime ?? "", { exact: true })).toBeVisible();
    expect(await engineValue.innerText()).not.toMatch(/command/i);
    await capture("build-recipes-operation");

    // Deploy the immutable reference explicitly. Saving the change leaves the original container running.
    const compose = deployCompose(reference);
    const savedDeploy = await page.request.put(panelPath(`/api/workloads/${appId}`), { headers, data: { name: "recipe-app", node_id: node.id, kind: "service", compose, source } });
    expect([200, 202], await savedDeploy.text()).toContain(savedDeploy.status());
    const afterSave = await workload();
    expect(afterSave.compose).toBe(compose);
    expect(afterSave.source).toMatchObject({ repository: repositoryUrl, branch: "main", inline_compose: true });
    expect(appContainer()).toEqual(original);
    const deployReply = await page.request.post(panelPath(`/api/workloads/${appId}/operations`), { headers, data: { action: "deploy" } });
    expect(deployReply.status(), await deployReply.text()).toBe(202);
    await settled(((await deployReply.json()) as { id: string }).id);
    await teamQuiet();
    const deployed = appContainer();
    expect(deployed.id).not.toBe(original.id);
    expect(deployed.image).toBe(reference);
    const inspected = (field: string) => JSON.parse(docker("inspect", "--format", `{{json ${field}}}`, deployed.id)) as unknown;
    expect({ user: inspected(".Config.User"), readOnly: inspected(".HostConfig.ReadonlyRootfs"), capDrop: inspected(".HostConfig.CapDrop"), entrypoint: inspected(".Config.Entrypoint") }).toEqual({
      user: "65534:65534",
      readOnly: true,
      capDrop: ["ALL"],
      entrypoint: ["/cove-static", "static-site"],
    });
    expect(inspected(".HostConfig.SecurityOpt")).toContain("no-new-privileges:true");

    // Probe the app's internal HTTP from an owned container on its Compose network in the same nested daemon.
    const networkName = `cove-${appId}_default`;
    expect(docker("network", "inspect", "--format", '{{index .Labels "com.docker.compose.project"}}', networkName)).toBe(`cove-${appId}`);
    const address = docker("inspect", "--format", `{{(index .NetworkSettings.Networks ${JSON.stringify(networkName)}).IPAddress}}`, deployed.id);
    expect(address).toMatch(/^\d+\.\d+\.\d+\.\d+$/);
    let probes = 0;
    const probe = (path: string) =>
      docker("run", "--rm", "--pull", "never", "--name", `${name}-recipe-probe-${++probes}`, "--network", networkName, "--label", `cove.browser-fixture=${name}`, "--label", probeLabel, "busybox:1.37", "sh", "-c", 'wget -S -O- "$1" 2>&1; true', "sh", `http://${address}:8080${path}`);
    await expect.poll(() => probe("/"), { timeout: 30_000 }).toContain("static-browser-fixture");
    expect(probe("/")).toContain("200 OK");
    for (const path of ["/.env", "/private.txt", "/public/.env", "/Dockerfile"]) {
      const refused = probe(path);
      expect(refused, path).toContain("404 Not Found");
      expect(refused, path).not.toContain("fixture-private");
    }
    const running = appContainer();

    // Save Railpack from the keyboard. This changes future builds only; Railpack is not built here.
    await page.goto(panelPath(`/resources/${appId}/registry`));
    await expect(engine).toHaveText("Static output", { timeout: 30_000 });
    await chooseWithKeyboard(page, engine, "Railpack");
    await expect(recipeForm.getByRole("combobox", { name: "Service", exact: true })).toHaveText("app");
    await expect(directory).toHaveValue(".");
    await expect(output).toHaveCount(0);
    expect(recipeWrites).toBe(0);
    await confirmEngine(railpackRecipe);
    expect(appContainer()).toEqual(running);
    await page.reload();
    await expect(engine).toHaveText("Railpack", { timeout: 30_000 });
    await expect(recipeForm.getByRole("combobox", { name: "Service", exact: true })).toHaveText("app");
    await expect(directory).toHaveValue(".");
    await expect(recipeForm.getByLabel("Build command")).toHaveValue("");
    await expect(recipeForm.getByLabel("Start command")).toHaveValue("");
    const railpack = await recipeSettings();
    expect(railpack.railpack_version).not.toBe("");
    expect(railpack.pinned_images).toHaveLength(3);
    await expect(recipeForm.getByText(railpack.railpack_version, { exact: true })).toBeVisible();
    await expect(recipeForm.getByRole("list", { name: "Pinned Railpack images" }).getByRole("listitem")).toHaveText(railpack.pinned_images);
    await expect(recipeForm.getByText("Railpack uses the network while it plans and builds", { exact: true })).toBeVisible();
    await expect(page.getByRole("region", { name: "Build and publish", exact: true }).getByText("Static output for app", { exact: true })).toBeVisible();
    await capture("build-recipes-railpack");

    // Clear the recipe back to Compose.
    await chooseWithKeyboard(page, engine, "Existing Compose / Dockerfile");
    await confirmEngine(emptyRecipe);
    await page.reload();
    await expect(engine).toHaveText("Existing Compose / Dockerfile", { timeout: 30_000 });
    await expect(recipeForm.getByRole("combobox", { name: "Service", exact: true })).toHaveCount(0);
    expect(appContainer()).toEqual(running);
    await capture("build-recipes-cleared");

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

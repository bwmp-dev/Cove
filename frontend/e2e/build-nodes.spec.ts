import { execFileSync } from "node:child_process";
import { readFileSync } from "node:fs";
import { isAbsolute } from "node:path";
import { expect, test, type Locator, type Page, type TestInfo } from "@playwright/test";
import { cleanupBrowserResources, expectNoHorizontalScroll } from "./route-tls";

// Runs only with scripts/test-browser.py --managed-registry --build-nodes. The fixture enrolls two real workers on two
// distinct owned Docker daemons: the runtime worker on the primary nested daemon and an SSH builder whose own daemon
// runs inside it. The managed registry is reached over TLS verified against the fixture's disposable CA. No request in
// this spec is mocked. The only fixture-side writes are the synthetic build context seeded into the owned builder's
// workload directory, which stands in for source; it is not a product upload API or a Git acceptance claim.
const password = process.env.COVE_BROWSER_PASSWORD ?? "";
const enabled = process.env.COVE_BROWSER_BUILD_NODES === "1" && process.env.COVE_BROWSER_MANAGED_REGISTRY === "1";

test.use({ actionTimeout: 30_000, navigationTimeout: 60_000 });

const docker = (...args: string[]) => execFileSync("docker", args, { encoding: "utf8", stdio: ["ignore", "pipe", "pipe"] }).trim();
const dockerWithInput = (input: string | Buffer, ...args: string[]) => {
  execFileSync("docker", args, { input, stdio: ["pipe", "ignore", "pipe"] });
};
const dockerSucceeds = (...args: string[]) => {
  try {
    execFileSync("docker", args, { stdio: ["ignore", "ignore", "ignore"] });
    return true;
  } catch {
    return false;
  }
};
const sizes = [320, 390, 768, 1280, 1440, 1920];
const tlsFailure = "Registry deployment finished, but authenticated TLS verification failed. Confirm DNS and the HTTPS certificate, then retry.";
const finished = /^(succeeded|failed|cancelled|interrupted)$/;
const resourceId = /^[a-f0-9]{32}$/;
const artifact = "built-on-designated-node";
const repository = "fixture/app";

async function capture(page: Page, testInfo: TestInfo, name: string) {
  for (const width of sizes) {
    await page.setViewportSize({ width, height: 900 });
    await expectNoHorizontalScroll(page);
    await page.screenshot({ path: testInfo.outputPath(`${name}-${width}.png`), fullPage: true });
  }
  await page.setViewportSize({ width: 1280, height: 900 });
}

// The fixture's names, inferred from the runtime worker the harness exports and checked against its database and the
// ownership labels on the builder worker and its daemon.
function fixture() {
  const owner = /^cove-browser-([a-f0-9]{12})-worker$/.exec(process.env.COVE_BROWSER_WORKER ?? "")?.[1];
  const name = `cove-browser-${owner}`;
  const database = `${name}-db`;
  if (!owner || process.env.COVE_BROWSER_DATABASE !== database) throw new Error("Isolated browser fixture required");
  const worker = `${name}-worker`;
  const builder = `${name}-builder`;
  const buildDocker = `${name}-build-docker`;
  if (process.env.COVE_BROWSER_BUILDER !== builder || process.env.COVE_BROWSER_BUILD_DOCKER !== buildDocker) throw new Error("The fixture must provide its own builder worker and build daemon");
  for (const container of [builder, buildDocker]) {
    if (docker("inspect", "--format", '{{index .Config.Labels "cove.browser-fixture"}}', container) !== name) throw new Error(`${container} is not owned by this fixture`);
  }
  const daemon = process.env.COVE_BROWSER_MANAGED_DAEMON_ID ?? "";
  if (!daemon) throw new Error("The fixture must name its owned primary daemon");
  const hostname = process.env.COVE_BROWSER_MANAGED_REGISTRY_HOST ?? "";
  if (hostname !== `registry-${owner}.example.test`) throw new Error("The fixture must provide its own exact registry hostname");
  const file = (variable: string) => {
    const path = process.env[variable] ?? "";
    if (!isAbsolute(path)) throw new Error(`${variable} must be an absolute fixture path`);
    return readFileSync(path, "utf8");
  };
  return { name, database, worker, builder, buildDocker, daemon, hostname, certificate: file("COVE_BROWSER_MANAGED_REGISTRY_CERT"), key: file("COVE_BROWSER_MANAGED_REGISTRY_KEY") };
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
  result: { error?: string; build?: { images: BuiltImage[] } } | null;
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
  capabilities: { build_publish?: boolean; remote_builds?: boolean; os?: string; arch?: string } | null;
}

interface BuildNodeSettings {
  node_id: string;
  runtime_node_id: string;
  effective_node_id: string;
  nodes: { id: string; name: string; eligible: boolean; reason: string }[];
}

const service = (lines: string[]) => ["services:", "  app:", ...lines.map((line) => `    ${line}`), ""].join("\n");
const initialCompose = service(["image: busybox:1.37", "pull_policy: never", "init: true", "restart: unless-stopped", "command: [sleep, '86400']"]);
const buildCompose = service(["image: busybox:1.37", "pull_policy: never", "init: true", "restart: unless-stopped", "command: [sleep, '86400']", "build: ."]);
const deployCompose = (reference: string) =>
  service([`image: ${JSON.stringify(reference)}`, "pull_policy: always", "init: true", "restart: unless-stopped", `command: [sh, -c, ${JSON.stringify("cat /artifact; sleep 86400")}]`]);

// Synthetic build context: an exported BusyBox root filesystem plus a Dockerfile that writes the artifact. The
// .dockerignore keeps every file the worker writes into the same directory (its Compose copies) out of the context.
const dockerfile = ["FROM scratch", "ADD rootfs.tar /", `RUN ["/bin/busybox", "sh", "-c", "echo ${artifact} > /artifact"]`, 'ENTRYPOINT ["/bin/busybox"]', ""].join("\n");
const dockerignore = ["*", "!Dockerfile", "!rootfs.tar", ""].join("\n");

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
  for (let step = 0; step < optionCount &&(await combobox.getAttribute("aria-activedescendant")) !== targetId; step++) await page.keyboard.press("ArrowDown");
  await expect(combobox).toHaveAttribute("aria-activedescendant", targetId ?? "");
  await page.keyboard.press("Enter");
  await expect(combobox).toHaveText(option);
}

const definition = (page: Page, term: string) => page.locator("dt", { hasText: new RegExp(`^${term}$`) }).locator("xpath=following-sibling::dd[1]");

test("build nodes: designated builder publishes, runtime keeps running until an explicit immutable deploy, then cleanup", async ({ page }, testInfo) => {
  test.skip(!enabled, "Needs scripts/test-browser.py --managed-registry --build-nodes");
  test.setTimeout(600_000);
  const { name, database, worker, builder, buildDocker, daemon, hostname, certificate, key } = fixture();
  const sql = (query: string) => docker("exec", database, "psql", "-U", "postgres", "-d", "cove", "-Atc", query);
  // Background image checks start due. Deferring them keeps every operation in this team one the spec started, because
  // build node changes and registry removal are refused while an operation is queued or running.
  const deferImageChecks = (id: string) => {
    expect(id).toMatch(resourceId);
    expect(sql(`UPDATE workloads SET next_image_check_at=now()+interval '1 day' WHERE id='${id}' RETURNING id`).split(/\s+/).filter((value) => value === id)).toEqual([id]);
  };
  const secretMarkers = ["PRIVATE KEY", key.trim(), "COVE_REGISTRY_AUTH", "cove_registry:", "$2a$", "$2b$", "$2y$", "REGISTRY_AUTH_HTPASSWD", '"auths"'];
  const expectNoSecrets = (text: string) => {
    for (const marker of secretMarkers) expect(text.includes(marker), `response contains secret-like material (${marker.slice(0, 20)})`).toBe(false);
  };
  const seedLabel = `dev.cove.build-seed=${name}`;

  let registryId = "";
  let consumerId = "";
  let consumerDeleted = false;
  let registryRecordDeleted = false;
  let headers: Record<string, string> = {};
  let primary: Error | null = null;
  try {
    // Two independent daemons: each worker talks to its own, and neither is the other.
    const primaryDaemon = docker("info", "--format", "{{.ID}}");
    expect(primaryDaemon).toBe(daemon);
    const buildDaemon = docker("exec", buildDocker, "docker", "info", "--format", "{{.ID}}");
    expect(buildDaemon).not.toBe("");
    expect(buildDaemon).not.toBe(primaryDaemon);
    expect(docker("exec", builder, "docker", "info", "--format", "{{.ID}}")).toBe(buildDaemon);
    expect(docker("exec", worker, "docker", "info", "--format", "{{.ID}}")).toBe(primaryDaemon);
    const builderImages = docker("exec", buildDocker, "docker", "image", "ls", "--format", "{{.Repository}}:{{.Tag}}").split(/\s+/).filter(Boolean).sort();
    expect(builderImages).toEqual(["busybox:1.37", "moby/buildkit:buildx-stable-1"]);

    await page.goto("/projects");
    await expect(page).toHaveURL(/\/login(?:[?#]|$)/);
    await page.getByLabel("Password", { exact: true }).fill(password);
    await page.getByRole("button", { name: "Sign in", exact: true }).click();
    await expect(page).not.toHaveURL(/\/login(?:[?#]|$)/);
    headers = { Origin: new URL(page.url()).origin };

    const nodes = async () => {
      const response = await page.request.get("/api/nodes");
      expect(response.status(), await response.text()).toBe(200);
      return (await response.json()) as NodeItem[];
    };
    const enrolled = await nodes();
    const runtime = enrolled.find((item) => item.name === "ready-browser-worker");
    const designated = enrolled.find((item) => item.name === "ready-browser-builder");
    if (!runtime || !designated) throw new Error("Both browser workers must be enrolled");
    expect(runtime.id).not.toBe(designated.id);
    for (const node of [runtime, designated]) {
      expect(node.state, node.name).toBe("ready");
      expect(node.capabilities?.build_publish, node.name).toBe(true);
      expect(node.capabilities?.remote_builds, node.name).toBe(true);
      expect(node.capabilities?.os, node.name).toBe("linux");
    }
    expect(designated.capabilities?.arch).toBe(runtime.capabilities?.arch);
    expect(runtime.capabilities?.arch).toBeTruthy();

    const operation = async (id: string) => {
      const response = await page.request.get(`/api/operations/${id}`);
      const text = await response.text();
      expect(response.status(), text).toBe(200);
      expectNoSecrets(text);
      return JSON.parse(text) as Operation;
    };
    const settled = async (id: string) => {
      await expect.poll(async () => (await operation(id)).state, { timeout: 300_000 }).toMatch(finished);
      const result = await operation(id);
      if (result.state !== "succeeded") {
        // Cleanup deletes the remote resources and their logs, so the failed operation's log is kept before throwing.
        let diagnostic: string;
        try {
          const response = await page.request.get(`/api/operations/${id}/logs`);
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
    const operationLogs = async (id: string) => {
      const response = await page.request.get(`/api/operations/${id}/logs`);
      const text = await response.text();
      expect(response.status(), text).toBe(200);
      expectNoSecrets(text);
      return (JSON.parse(text) as { logs: string }).logs;
    };
    const teamQuiet = () =>
      expect
        .poll(async () => ((await (await page.request.get("/api/operations")).json()) as Operation[]).filter((item) => item.state === "queued" || item.state === "running").map((item) => `${item.action}:${item.id}`), { timeout: 120_000 })
        .toEqual([]);
    const current = async () => {
      const response = await page.request.get("/api/managed-registries");
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
      const response = await page.request.get(`/api/workloads/${consumerId}`);
      const text = await response.text();
      expect(response.status(), text).toBe(200);
      expectNoSecrets(text);
      return JSON.parse(text) as { id: string; name: string; node_id: string; compose: string };
    };
    const buildNode = async () => {
      const response = await page.request.get(`/api/workloads/${consumerId}/build-node`);
      const text = await response.text();
      expect(response.status(), text).toBe(200);
      return JSON.parse(text) as BuildNodeSettings;
    };
    const consumerContainer = () => {
      const ids = docker("ps", "-q", "--filter", `label=com.docker.compose.project=cove-${consumerId}`, "--filter", "label=com.docker.compose.service=app").split(/\s+/).filter(Boolean);
      expect(ids).toHaveLength(1);
      const [id, image, imageId, started, restarts] = docker("inspect", "--format", "{{.Id}}|{{.Config.Image}}|{{.Image}}|{{.State.StartedAt}}|{{.RestartCount}}", ids[0] ?? "").split("|");
      return { id, image, imageId, started, restarts };
    };
    const respondedTo = (path: string, method: string) => page.waitForResponse((response) => new URL(response.url()).pathname === path && response.request().method() === method);
    const builderFile = (path: string) => dockerSucceeds("exec", builder, "test", "-e", path);
    const runtimeFile = (path: string) => dockerSucceeds("exec", worker, "test", "-e", path);

    // A project and a managed registry on the runtime node, repaired with the fixture leaf until it is ready.
    const projectReply = await page.request.post("/api/projects", { headers, data: { name: `Build nodes ${Date.now()}` } });
    expect(projectReply.ok(), await projectReply.text()).toBe(true);
    const project = (await projectReply.json()) as { id: string };
    const retention = { keep_per_repository: 10, minimum_age_days: 30 };
    const createdReply = await page.request.post("/api/managed-registries", { headers, data: { name: `Build registry ${Date.now()}`, node_id: runtime.id, project_id: project.id, hostname, retention } });
    const createdText = await createdReply.text();
    expect(createdReply.status(), createdText).toBe(202);
    expectNoSecrets(createdText);
    registryId = (JSON.parse(createdText) as { id: string }).id;
    deferImageChecks(registryId);
    const failed = await registryReaches("failed");
    expect(failed.last_error).toBe(tlsFailure);
    const uploadReply = await page.request.post(`/api/workloads/${registryId}/certificates`, { headers, data: { name: "Fixture registry leaf", certificate_pem: certificate, private_key_pem: key } });
    const uploadText = await uploadReply.text();
    expect(uploadReply.status(), uploadText).toBe(201);
    expectNoSecrets(uploadText);
    const uploaded = JSON.parse(uploadText) as { id: string; dns_names: string[] };
    expect(uploaded.dns_names).toEqual([hostname]);
    const selectReply = await page.request.put(`/api/managed-registries/${registryId}`, { headers, data: { retention, certificate_id: uploaded.id } });
    expect(selectReply.status(), await selectReply.text()).toBe(200);
    const retryReply = await page.request.post(`/api/managed-registries/${registryId}/retry`, { headers });
    expect(retryReply.status(), await retryReply.text()).toBe(202);
    const ready = await registryReaches("ready");
    expect(ready.connection_id).not.toBe("");
    const connectionsReply = await page.request.get("/api/registries");
    const connectionsText = await connectionsReply.text();
    expect(connectionsReply.status(), connectionsText).toBe(200);
    const generated = (JSON.parse(connectionsText) as RegistryConnection[]).filter((item) => item.managed_registry_id);
    expect(generated.map((item) => ({ id: item.id, managed: item.managed_registry_id, server: item.registry?.server, password: item.registry?.password }))).toEqual([{ id: ready.connection_id, managed: registryId, server: hostname, password: "" }]);

    // An ordinary resource on the runtime node that uses the generated connection, deployed from the cached BusyBox.
    const consumerReply = await page.request.post("/api/workloads", { headers, data: { name: "build-consumer", node_id: runtime.id, kind: "service", compose: initialCompose, project_id: project.id } });
    expect(consumerReply.status(), await consumerReply.text()).toBe(201);
    consumerId = ((await consumerReply.json()) as { id: string }).id;
    deferImageChecks(consumerId);
    const connect = await page.request.put(`/api/workloads/${consumerId}/registry`, { headers, data: { registry_id: ready.connection_id } });
    expect(connect.status(), await connect.text()).toBe(200);
    expect(await (await page.request.get(`/api/workloads/${consumerId}/registry`)).json()).toEqual({ registry_id: ready.connection_id });
    const firstDeploy = await page.request.post(`/api/workloads/${consumerId}/operations`, { headers, data: { action: "deploy" } });
    expect(firstDeploy.status(), await firstDeploy.text()).toBe(202);
    await settled(((await firstDeploy.json()) as { id: string }).id);
    await teamQuiet();
    const original = consumerContainer();
    expect(original.image).toBe("busybox:1.37");

    // Save the build input. Saving does not deploy, so the running container is untouched.
    const savedBuild = await page.request.put(`/api/workloads/${consumerId}`, { headers, data: { name: "build-consumer", node_id: runtime.id, kind: "service", compose: buildCompose } });
    expect([200, 202], await savedBuild.text()).toContain(savedBuild.status());
    expect((await workload()).compose).toBe(buildCompose);
    expect(consumerContainer()).toEqual(original);

    // Seed the synthetic build context into the owned builder's workload directory, as the builder's own user.
    expect(consumerId).toMatch(resourceId);
    const context = `/var/lib/cove/workloads/${consumerId}`;
    docker("exec", "-u", "cove", builder, "mkdir", "-p", context);
    const seed = docker("create", "--pull", "never", "--label", seedLabel, "--network", "none", "busybox:1.37", "true");
    try {
      const rootfs = execFileSync("docker", ["export", seed], { stdio: ["ignore", "pipe", "pipe"], maxBuffer: 64 * 1024 * 1024 });
      dockerWithInput(rootfs, "exec", "-i", "-u", "cove", builder, "sh", "-c", 'cat > "$1"', "sh", `${context}/rootfs.tar`);
    } finally {
      for (const container of docker("ps", "-aq", "--filter", `label=${seedLabel}`).split(/\s+/).filter(Boolean)) docker("rm", "-f", container);
    }
    dockerWithInput(dockerfile, "exec", "-i", "-u", "cove", builder, "sh", "-c", 'cat > "$1"', "sh", `${context}/Dockerfile`);
    dockerWithInput(dockerignore, "exec", "-i", "-u", "cove", builder, "sh", "-c", 'cat > "$1"', "sh", `${context}/.dockerignore`);
    expect(runtimeFile(`${context}/rootfs.tar`)).toBe(false);

    // Choose the designated builder in the dashboard from the keyboard.
    await page.goto(`/resources/${consumerId}/registry`);
    const settingForm = page.getByRole("form", { name: "Build node setting" });
    const nodeSelect = settingForm.getByRole("combobox", { name: "Build node", exact: true });
    await expect(nodeSelect).toHaveText(`Runtime node (${runtime.name})`, { timeout: 30_000 });
    expect(await buildNode()).toEqual({
      node_id: "",
      runtime_node_id: runtime.id,
      effective_node_id: runtime.id,
      nodes: [
        { id: designated.id, name: designated.name, eligible: true, reason: "" },
        { id: runtime.id, name: runtime.name, eligible: true, reason: "" },
      ],
    });
    await capture(page, testInfo, "build-nodes-default");
    await chooseWithKeyboard(page, nodeSelect, designated.name);
    const saveNode = settingForm.getByRole("button", { name: "Save build node", exact: true });
    await expect(saveNode).toBeEnabled();
    const assignedReply = respondedTo(`/api/workloads/${consumerId}/build-node`, "PUT");
    await saveNode.focus();
    await page.keyboard.press("Enter");
    const assigned = await assignedReply;
    expect(assigned.status(), await assigned.text()).toBe(200);
    expect(assigned.request().postDataJSON()).toEqual({ node_id: designated.id });
    await expect(settingForm.getByRole("status")).toHaveText("Build node saved.");
    await expect(nodeSelect).toHaveText(designated.name);
    const afterAssign = await buildNode();
    expect({ node: afterAssign.node_id, runtime: afterAssign.runtime_node_id, effective: afterAssign.effective_node_id }).toEqual({ node: designated.id, runtime: runtime.id, effective: designated.id });
    expect((await workload()).node_id).toBe(runtime.id);
    expect(consumerContainer()).toEqual(original);

    // Queue the build from the existing form, from the keyboard.
    const clock = () => docker("exec", buildDocker, "date", "+%s");
    const buildStarted = clock();
    await page.getByLabel("Repository", { exact: true }).fill(repository);
    const noCache = page.getByRole("checkbox", { name: "Bypass the build cache" });
    await noCache.focus();
    await page.keyboard.press("Space");
    await expect(noCache).toBeChecked();
    const buildButton = page.getByRole("button", { name: "Build and publish", exact: true });
    await expect(buildButton).toBeEnabled();
    const queuedReply = respondedTo(`/api/workloads/${consumerId}/operations`, "POST");
    await buildButton.focus();
    await page.keyboard.press("Enter");
    const queued = await queuedReply;
    const queuedText = await queued.text();
    expect(queued.status(), queuedText).toBe(202);
    expect(queued.request().postDataJSON()).toEqual({ action: "build", build: { repository, no_cache: true } });
    const buildId = (JSON.parse(queuedText) as { id: string }).id;
    await expect(page.getByText("Build queued", { exact: true })).toBeVisible();
    await capture(page, testInfo, "build-nodes-queued");

    const built = await settled(buildId);
    const buildEnded = clock();
    expect(built.action).toBe("build");
    expect(built.execution_node_id).toBe(designated.id);
    const images = built.result?.build?.images ?? [];
    expect(images.map((image) => image.service)).toEqual(["app"]);
    const [image] = images;
    if (!image) throw new Error("The build reported no published image");
    const escapedHost = hostname.replace(/[.]/g, "\\.");
    expect(image.reference).toMatch(new RegExp(`^${escapedHost}/${repository}@sha256:[a-f0-9]{64}$`));
    expect(image.image_id).toMatch(/^sha256:[a-f0-9]{64}$/);
    const reference = image.reference;

    // The logs and journal live on the builder only; the runtime worker never held them.
    const buildLog = `/var/lib/cove/operations/${buildId}.log`;
    expect(await operationLogs(buildId)).toContain(`Published app as ${reference}`);
    expect(builderFile(buildLog)).toBe(true);
    expect(runtimeFile(buildLog)).toBe(false);
    expect(runtimeFile(`/var/lib/cove/operations/${buildId}.json`)).toBe(false);

    // The builder daemon pushed the image; the runtime daemon pushed nothing and has neither the image nor its digest.
    const pushes = (exec: string[]) => docker(...exec, "events", "--since", buildStarted, "--until", buildEnded, "--filter", "type=image", "--filter", "event=push", "--format", "{{.Actor.ID}}").split(/\s+/).filter((value) => value.startsWith(`${hostname}/${repository}`));
    expect(pushes(["exec", buildDocker, "docker"]).length).toBeGreaterThan(0);
    expect(pushes([])).toEqual([]);
    expect(dockerSucceeds("image", "inspect", image.image_id)).toBe(false);
    expect(dockerSucceeds("image", "inspect", reference)).toBe(false);

    // Building changed neither the running container nor the saved Compose file.
    expect(consumerContainer()).toEqual(original);
    expect((await workload()).compose).toBe(buildCompose);

    // The build history and the operation page name the node the build actually ran on.
    await page.reload();
    await expect(page.getByRole("heading", { name: "Latest successful build", exact: true })).toBeVisible({ timeout: 30_000 });
    await expect(page.getByText(`Built on ${designated.name}`, { exact: true })).toBeVisible();
    await expect(page.getByRole("list", { name: "Published images" }).getByText(reference, { exact: true })).toBeVisible();
    await capture(page, testInfo, "build-nodes-history");
    await page.goto(`/operations/${buildId}`);
    await expect(definition(page, "Build node").getByRole("link", { name: designated.name, exact: true })).toBeVisible({ timeout: 30_000 });
    await expect(definition(page, "Runtime node").getByRole("link", { name: runtime.name, exact: true })).toBeVisible();
    await expect(page.getByText(`Published app as ${reference}`)).toBeVisible({ timeout: 30_000 });
    await capture(page, testInfo, "build-nodes-operation");

    // Deploy the immutable reference explicitly: change the saved Compose file, then run an ordinary deployment that
    // pulls through the generated connection on the runtime node. The original container runs until then.
    const compose = deployCompose(reference);
    const savedDeploy = await page.request.put(`/api/workloads/${consumerId}`, { headers, data: { name: "build-consumer", node_id: runtime.id, kind: "service", compose } });
    expect([200, 202], await savedDeploy.text()).toContain(savedDeploy.status());
    expect((await workload()).compose).toBe(compose);
    expect(consumerContainer()).toEqual(original);
    const deployReply = await page.request.post(`/api/workloads/${consumerId}/operations`, { headers, data: { action: "deploy" } });
    expect(deployReply.status(), await deployReply.text()).toBe(202);
    const deployed = await settled(((await deployReply.json()) as { id: string }).id);
    expect(deployed.execution_node_id).toBe(runtime.id);
    await teamQuiet();
    const replaced = consumerContainer();
    expect(replaced.id).not.toBe(original.id);
    expect(replaced.image).toBe(reference);
    expect(docker("exec", replaced.id ?? "", "/bin/busybox", "cat", "/artifact")).toBe(artifact);
    expect(dockerSucceeds("image", "inspect", reference)).toBe(true);

    // Clear the assignment from the dashboard. The finished build keeps reporting the builder.
    await page.goto(`/resources/${consumerId}/registry`);
    await expect(nodeSelect).toHaveText(designated.name, { timeout: 30_000 });
    await chooseWithKeyboard(page, nodeSelect, `Runtime node (${runtime.name})`);
    await expect(saveNode).toBeEnabled();
    const clearedReply = respondedTo(`/api/workloads/${consumerId}/build-node`, "PUT");
    await saveNode.focus();
    await page.keyboard.press("Enter");
    const cleared = await clearedReply;
    expect(cleared.status(), await cleared.text()).toBe(200);
    expect(cleared.request().postDataJSON()).toEqual({ node_id: "" });
    await expect(settingForm.getByRole("status")).toHaveText("Build node saved.");
    const afterClear = await buildNode();
    expect({ node: afterClear.node_id, effective: afterClear.effective_node_id }).toEqual({ node: "", effective: runtime.id });
    await expect(page.getByText(`Built on ${designated.name}`, { exact: true })).toBeVisible();
    await capture(page, testInfo, "build-nodes-cleared");
    expect((await operation(buildId)).execution_node_id).toBe(designated.id);
    expect(await operationLogs(buildId)).toContain(`Published app as ${reference}`);

    // Restart only the builder's SSH worker. Its persisted journal still serves the old build's status and logs.
    expect(docker("inspect", "--format", '{{index .Config.Labels "cove.browser-fixture"}}', builder)).toBe(name);
    docker("restart", builder);
    await expect
      .poll(
        async () => {
          await page.request.post(`/api/nodes/${designated.id}/verify`, { headers });
          return (await nodes()).find((item) => item.id === designated.id)?.state;
        },
        { timeout: 120_000, intervals: [2_000] },
      )
      .toBe("ready");
    expect(docker("exec", buildDocker, "docker", "info", "--format", "{{.ID}}")).toBe(buildDaemon);
    expect(await operationLogs(buildId)).toContain(`Published app as ${reference}`);
    await page.goto(`/operations/${buildId}`);
    await expect(definition(page, "Build node").getByRole("link", { name: designated.name, exact: true })).toBeVisible({ timeout: 30_000 });
    await expect(page.getByText(`Published app as ${reference}`)).toBeVisible({ timeout: 30_000 });

    // Teardown through the product: stop and delete the resource, which retires its build on the builder.
    await teamQuiet();
    const stopReply = await page.request.post(`/api/workloads/${consumerId}/operations`, { headers, data: { action: "stop" } });
    expect(stopReply.status(), await stopReply.text()).toBe(202);
    await settled(((await stopReply.json()) as { id: string }).id);
    await teamQuiet();
    const deleted = await page.request.delete(`/api/workloads/${consumerId}`, { headers });
    expect(deleted.status(), await deleted.text()).toBe(200);
    consumerDeleted = true;
    expect(builderFile(buildLog)).toBe(false);

    // Remove the registry keeping its data, purge the data, then delete the empty record.
    await teamQuiet();
    const removeReply = await page.request.delete(`/api/managed-registries/${registryId}`, { headers, data: { delete_data: false } });
    expect(removeReply.status(), await removeReply.text()).toBe(202);
    expect((await registryReaches("removed")).data_retained).toBe(true);
    await teamQuiet();
    const purgeReply = await page.request.delete(`/api/managed-registries/${registryId}`, { headers, data: { delete_data: true } });
    expect(purgeReply.status(), await purgeReply.text()).toBe(202);
    expect((await registryReaches("removed")).data_retained).toBe(false);
    await teamQuiet();
    const recordReply = await page.request.delete(`/api/managed-registries/${registryId}/record`, { headers });
    expect(recordReply.status(), await recordReply.text()).toBe(200);
    registryRecordDeleted = true;
  } catch (error) {
    primary = asError(error);
  }
  // Cleanup never replaces the first failure; its own failure fails the test only when the test otherwise passed.
  // Product APIs run first because generic database cleanup bypasses build retirement and registry references.
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
      .poll(async () => ((await (await page.request.get("/api/operations")).json()) as Operation[]).some((item) => item.state === "queued" || item.state === "running"), { timeout: 120_000 })
      .toBe(false);
  };
  if (consumerId && !consumerDeleted) {
    await attempt("resource stop", async () => {
      await quiet();
      const reply = await page.request.post(`/api/workloads/${consumerId}/operations`, { headers, data: { action: "stop" } });
      if (reply.status() !== 202) throw new Error(`stop returned ${reply.status()}: ${await reply.text()}`);
      await quiet();
    });
    await attempt("resource delete", async () => {
      const reply = await page.request.delete(`/api/workloads/${consumerId}`, { headers });
      if (reply.status() !== 200) throw new Error(`delete returned ${reply.status()}: ${await reply.text()}`);
    });
  }
  if (registryId && !registryRecordDeleted) {
    await attempt("registry removal", async () => {
      await quiet();
      const reply = await page.request.delete(`/api/managed-registries/${registryId}`, { headers, data: { delete_data: true } });
      if (reply.status() !== 202) throw new Error(`removal returned ${reply.status()}: ${await reply.text()}`);
      await quiet();
    });
  }
  await attempt("seed containers", () => {
    for (const container of docker("ps", "-aq", "--filter", `label=${seedLabel}`).split(/\s+/).filter(Boolean)) docker("rm", "-f", container);
  });
  await attempt("browser resources", () => cleanupBrowserResources());
  if (registryId) {
    await attempt("registry volume", () => {
      const volumes = docker("volume", "ls", "-q", "--filter", `label=dev.cove.managed-registry=${registryId}`).split(/\s+/).filter(Boolean);
      for (const volume of volumes) if (volume === `cove-${registryId}_data`) docker("volume", "rm", volume);
    });
  }
  if (primary !== null) {
    for (const failure of cleanupFailures) testInfo.annotations.push({ type: "cleanup-failure", description: failure });
    throw primary;
  }
  if (cleanupFailures.length > 0) throw new Error(`Cleanup failed: ${cleanupFailures.join("; ")}`);
});

import { execFileSync } from "node:child_process";
import { createDecipheriv, randomBytes } from "node:crypto";
import { readFileSync } from "node:fs";
import { isAbsolute } from "node:path";
import { expect, request as apiRequest, test, type APIRequestContext, type APIResponse, type Locator, type Page } from "@playwright/test";
import { cleanupBrowserResources, expectNoHorizontalScroll, verified } from "./route-tls";

// Runs only with scripts/test-browser.py --managed-registry --ci-previews on the caller's owned nested daemon. An external
// CI producer is simulated by committing three owned images from the cached BusyBox and pushing two of them to the real
// authenticated managed registry with a throwaway Docker config inside the fixture worker. Cove never builds them: the
// preview runtime pulls the exact digests through the resource's generated registry connection after the local copies
// are removed. The synthetic Git source counts every invocation, so a CI preview that touched Git would be visible. No
// request in this spec is mocked and TLS verification is never skipped.
const password = process.env.COVE_BROWSER_PASSWORD ?? "";
const enabled = process.env.COVE_BROWSER_CI_PREVIEWS === "1" && process.env.COVE_BROWSER_MANAGED_REGISTRY === "1";

test.use({ actionTimeout: 30_000, navigationTimeout: 60_000 });

const docker = (...args: string[]) => execFileSync("docker", args, { encoding: "utf8", stdio: ["ignore", "pipe", "pipe"] }).trim();
const sizes = [320, 390, 768, 1280, 1440, 1920];
const tlsFailure = "Registry deployment finished, but authenticated TLS verification failed. Confirm DNS and the HTTPS certificate, then retry.";
const finished = /^(succeeded|failed|cancelled|interrupted)$/;
const resourceId = /^[a-f0-9]{32}$/;
const repositoryUrl = "https://git.fixture.invalid/ci-previews.git";
const source = { repository: repositoryUrl, branch: "main", compose_path: "", inline_compose: true };
const gitCalls = "/tmp/ci-preview-git-calls";
const specLabel = "dev.cove.ci-previews-spec";
const appName = "ci-app";
const previewKey = "pr42";
const markers = { production: "ci-production-fixture", one: "ci-image-one", two: "ci-image-two" };
const sentinel = "production-sentinel";
const previewMarker = "preview-data-marker";

// The fixture's names, inferred from the worker the harness exports and checked against its database, its worker
// container, the owned nested daemon, the CI fixture and the owned ingress.
function fixture() {
  const owner = /^cove-browser-([a-f0-9]{12})-worker$/.exec(process.env.COVE_BROWSER_WORKER ?? "")?.[1];
  const name = `cove-browser-${owner}`;
  const database = `${name}-db`;
  if (!owner || process.env.COVE_BROWSER_DATABASE !== database) throw new Error("Isolated browser fixture required");
  const daemon = process.env.COVE_BROWSER_MANAGED_DAEMON_ID ?? "";
  if (!daemon) throw new Error("The fixture must name its owned nested daemon");
  const hostname = process.env.COVE_BROWSER_MANAGED_REGISTRY_HOST ?? "";
  if (hostname !== `registry-${owner}.example.test`) throw new Error("The fixture must provide its own exact registry hostname");
  if (process.env.COVE_BROWSER_CI_GIT_CALLS !== gitCalls) throw new Error("The fixture must count synthetic Git calls at its own path");
  const url = process.env.COVE_BROWSER_URL ?? "";
  if (!/^https:\/\/localhost:\d+$/.test(url)) throw new Error("The fixture must expose its own local panel URL");
  const file = (variable: string) => {
    const path = process.env[variable] ?? "";
    if (!isAbsolute(path)) throw new Error(`${variable} must be an absolute fixture path`);
    return readFileSync(path, "utf8");
  };
  return { name, database, worker: `${name}-worker`, panel: `${name}-panel`, daemon, hostname, url, certificate: file("COVE_BROWSER_MANAGED_REGISTRY_CERT"), key: file("COVE_BROWSER_MANAGED_REGISTRY_KEY"), root: file("COVE_BROWSER_MANAGED_REGISTRY_ROOT") };
}

// The fixture's own generated key, held only in this process. It is never printed, attached, sent to the browser or
// the API, or passed in a command line.
function fixtureKey() {
  const text = process.env.COVE_ENCRYPTION_KEY ?? "";
  const key = Buffer.from(text, "base64");
  if (key.length !== 32 || key.toString("base64") !== text) throw new Error("COVE_ENCRYPTION_KEY must be the fixture's canonical base64 32-byte key");
  return key;
}

// Matches unseal in internal/control/security.go: AES-256-GCM, a 12-byte nonce, the ciphertext, then the 16-byte tag.
function unseal(key: Buffer, sealed: Buffer) {
  if (sealed.length < 12 + 16 + 1) throw new Error("The sealed configuration is too short");
  const decipher = createDecipheriv("aes-256-gcm", key, sealed.subarray(0, 12));
  decipher.setAuthTag(sealed.subarray(sealed.length - 16));
  return Buffer.concat([decipher.update(sealed.subarray(12, sealed.length - 16)), decipher.final()]).toString("utf8");
}

interface Operation {
  id: string;
  action: string;
  state: string;
  workload_id: string;
  result: { error?: string } | null;
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

interface PreviewResource {
  base_id: string;
  resource_id: string;
  name: string;
  source: string;
  revision: string;
  ci_images?: Record<string, string>;
  deployed: boolean;
  routes: string[];
  published: boolean;
  operation: string;
}

interface Preview {
  id: string;
  ci: boolean;
  environment_id: string;
  suffix: string;
  slug: string;
  state: string;
  stage: string;
  detail: string;
  access: string;
  resources?: PreviewResource[];
  ci_inputs?: { identity: string; revision: string; recipe_version: number; images: Record<string, Record<string, string>> };
}

interface CIStatus {
  configuration: { resource_ids: string[]; recipe_version: number; matches_scope: boolean; expires_at: string; created_at: string; last_received_at: string | null } | null;
  resources: { id: string; name: string; services: string[]; registry: { id: string; server: string } | null; eligible: boolean; reason: string }[];
  endpoint: string;
  can_manage: boolean;
  setup_reason: string;
  used_preview_keys: number;
  retained_deliveries: number;
}

interface PlanResource {
  id: string;
  included: boolean;
  blockers: { code: string; message: string }[];
  volumes: { key: string; source: string; mode: string }[];
}

interface Container {
  id: string;
  image: string;
  imageId: string;
  started: string;
  restarts: string;
  dataVolume: string;
}

const service = (lines: string[]) => ["services:", "  app:", ...lines.map((line) => `    ${line}`), "volumes:", "  data: {}", ""].join("\n");
const productionCompose = (image: string) =>
  service([
    `image: ${JSON.stringify(image)}`,
    "pull_policy: never",
    "restart: unless-stopped",
    "command: [httpd, -f, -p, '8080', -h, /www]",
    "healthcheck:",
    "  test: [CMD, wget, -q, -O, /dev/null, 'http://127.0.0.1:8080/']",
    "  interval: 2s",
    "  timeout: 2s",
    "  retries: 15",
    "volumes:",
    "  - data:/data",
  ]);

const asError = (error: unknown) => (error instanceof Error ? error : new Error("A non-error value was thrown", { cause: error }));

// The worker, its owner marker and both daemon views must be the fixture's before anything runs against them.
function expectOwnedFixture({ name, worker, daemon }: { name: string; worker: string; daemon: string }) {
  expect(docker("info", "--format", "{{.ID}}")).toBe(daemon);
  expect(docker("inspect", "--format", "{{.Config.Image}}", worker)).toBe(`${worker}:test`);
  expect(docker("exec", worker, "cat", "/var/lib/cove/.browser-owner")).toBe(name);
  expect(docker("exec", worker, "docker", "info", "--format", "{{.ID}}")).toBe(daemon);
  expect(docker("inspect", "--format", '{{index .Config.Labels "cove.browser-fixture"}}', "cove-ingress")).toBe(name);
}

function containerIdentity(container: string) {
  const [id = "", containerName = "", image = "", started = "", attached = ""] = docker("inspect", "--format", "{{.Id}}|{{.Name}}|{{.Config.Image}}|{{.State.StartedAt}}|{{range $network, $_ := .NetworkSettings.Networks}}{{$network}} {{end}}", container).split("|");
  return { id, name: containerName, image, started, networks: attached.split(" ").filter(Boolean) };
}

function imageExists(reference: string) {
  try {
    execFileSync("docker", ["image", "inspect", "--format", "{{.Id}}", reference], { encoding: "utf8", stdio: ["ignore", "pipe", "pipe"] });
    return true;
  } catch (error) {
    const stderr = (error as { stderr?: unknown }).stderr;
    if (typeof stderr === "string" && /no such image|not found/i.test(stderr)) return false;
    throw error;
  }
}

function imageLabels(reference: string) {
  return JSON.parse(docker("image", "inspect", "--format", "{{json .Config.Labels}}", reference)) as Record<string, string> | null;
}

async function tabTo(page: Page, target: Locator, limit = 12) {
  for (let step = 0; step < limit && !(await target.evaluate((element) => element === document.activeElement)); step++) await page.keyboard.press("Tab");
  await expect(target).toBeFocused();
}

async function signIn(page: Page) {
  await page.goto("/projects");
  await expect(page).toHaveURL(/\/login(?:[?#]|$)/);
  await page.getByLabel("Password", { exact: true }).fill(password);
  await page.getByRole("button", { name: "Sign in", exact: true }).click();
  await expect(page).not.toHaveURL(/\/login(?:[?#]|$)/);
  return { Origin: new URL(page.url()).origin };
}

test("CI previews: an external CI producer's registry digests become an approved private preview without Git or builds", async ({ page }, testInfo) => {
  test.skip(!enabled, "Needs scripts/test-browser.py --managed-registry --ci-previews");
  test.setTimeout(600_000);
  const { name, database, worker, panel, daemon, hostname, url, certificate, key, root } = fixture();
  const sql = (query: string) => docker("exec", database, "psql", "-U", "postgres", "-d", "cove", "-Atc", query);
  const deferImageChecks = (id: string) => {
    expect(id).toMatch(resourceId);
    expect(sql(`UPDATE workloads SET next_image_check_at=now()+interval '1 day' WHERE id='${id}' RETURNING id`).split(/\s+/).filter((value) => value === id)).toEqual([id]);
  };
  // Secrets learned during the run. Only labels are ever printed; values are redacted from anything attached.
  const secrets: { label: string; value: string }[] = [
    { label: "fixture private key", value: key.trim() },
    { label: "PEM private key header", value: "PRIVATE KEY" },
    { label: "registry htpasswd", value: "REGISTRY_AUTH_HTPASSWD" },
    { label: "Docker auth config", value: '"auths"' },
  ];
  const remember = (label: string, value: string) => {
    expect(value.length, `${label} is empty`).toBeGreaterThan(0);
    secrets.push({ label, value });
  };
  const expectNoSecrets = (text: string, where: string) => {
    for (const secret of secrets) expect(text.includes(secret.value), `${where} contains ${secret.label}`).toBe(false);
  };
  const redact = (text: string) => secrets.reduce((current, secret) => current.split(secret.value).join("[redacted]"), text);
  const capture = async (label: string) => {
    const body = await page.locator("body").innerText();
    expect(body.includes("-----BEGIN"), "page shows PEM material").toBe(false);
    expectNoSecrets(body, `page before ${label} screenshots`);
    await expect(page.getByLabel("CI credential", { exact: true })).toHaveCount(0);
    for (const width of sizes) {
      await page.setViewportSize({ width, height: 900 });
      await expectNoHorizontalScroll(page);
      await page.screenshot({ path: testInfo.outputPath(`${label}-${width}.png`), fullPage: true });
    }
    await page.setViewportSize({ width: 1280, height: 900 });
  };
  const run = randomBytes(6).toString("hex");
  const runLabel = `${specLabel}=${run}`;
  const pushDirectory = `/tmp/cove-ci-push-${randomBytes(16).toString("hex")}`;
  if (!/^\/tmp\/cove-ci-push-[a-f0-9]{32}$/.test(pushDirectory)) throw new Error("Unexpected push directory");
  const repository = `${hostname}/fixture/ci-app`;

  // Nothing is created before the fixture is proven to be this caller's.
  expectOwnedFixture({ name, worker, daemon });
  const panelBefore = containerIdentity(panel);
  expect({ name: panelBefore.name, image: panelBefore.image }).toEqual({ name: `/${panel}`, image: `${panel}:test` });
  expect(panelBefore.networks).toContain(name);
  const db = containerIdentity(database);
  expect({ name: db.name, image: db.image }).toEqual({ name: `/${database}`, image: "postgres:17-alpine" });
  expect(db.networks).toContain(name);
  expect(sql("SELECT current_database()")).toBe("cove");
  const baseImage = docker("image", "inspect", "--format", "{{.Id}}", "busybox:1.37");
  expect(docker("exec", worker, "sh", "-ec", 'test -f "$1"; test ! -L "$1"; echo ok', "sh", gitCalls)).toBe("ok");
  const gitCount = () => Number(docker("exec", worker, "sh", "-ec", 'wc -l < "$1"', "sh", gitCalls));
  const builders = () =>
    docker("ps", "-a", "--format", "{{.ID}} {{.Names}} {{.Image}} {{.Label \"cove.managed\"}}")
      .split("\n")
      .filter((line) => /buildkit|buildx|build-cache/.test(line))
      .sort();
  const buildersBefore = builders();
  const recipeKey = fixtureKey();

  let projectId = "";
  let registryId = "";
  let appId = "";
  let previewId = "";
  let previewResourceId = "";
  let appDeleted = false;
  let registryRecordDeleted = false;
  let ci: APIRequestContext | null = null;
  let primary: Error | null = null;
  try {
    const headers = await signIn(page);
    ci = await apiRequest.newContext({ baseURL: url, ignoreHTTPSErrors: true });
    const ciContext = ci;

    const get = async (path: string) => {
      const response = await page.request.get(path);
      const text = await response.text();
      expect(response.status(), `GET ${path}: ${redact(text).slice(0, 500)}`).toBe(200);
      expectNoSecrets(text, `GET ${path}`);
      return JSON.parse(text) as unknown;
    };
    const operation = async (id: string) => (await get(`/api/operations/${id}`)) as Operation;
    const operations = async () => (await get("/api/operations")) as Operation[];
    const attachLogs = async (id: string) => {
      try {
        const response = await page.request.get(`/api/operations/${id}/logs`);
        const text = await response.text();
        if (response.status() !== 200) return `log request returned ${response.status()}`;
        const logs = redact((JSON.parse(text) as { logs: string }).logs);
        const tail = logs.length > 4_000 ? `...${logs.slice(-4_000)}` : logs;
        await testInfo.attach(`operation-${id}-log.txt`, { body: tail, contentType: "text/plain" });
        return tail;
      } catch (error) {
        return `log unavailable: ${asError(error).message.slice(0, 300)}`;
      }
    };
    const settled = async (id: string) => {
      await expect.poll(async () => (await operation(id)).state, { timeout: 180_000 }).toMatch(finished);
      const result = await operation(id);
      if (result.state !== "succeeded") throw new Error(`${result.action} ${id} ended ${result.state}: ${redact(result.result?.error ?? "")}\n--- operation log (tail) ---\n${await attachLogs(id)}`);
      return result;
    };
    const teamQuiet = () =>
      expect
        .poll(async () => (await operations()).filter((item) => item.state === "queued" || item.state === "running").map((item) => `${item.action}:${item.id}`), { timeout: 120_000 })
        .toEqual([]);
    const current = async () => {
      const found = ((await get("/api/managed-registries")) as { registries: Registry[] }).registries.find((item) => item.id === registryId);
      if (!found) throw new Error(`Managed registry ${registryId} is not listed`);
      return found;
    };
    const registryReaches = async (state: string) => {
      await expect.poll(async () => (await current()).state, { timeout: 240_000 }).toMatch(/^(ready|failed|removed)$/);
      const reached = await current();
      if (reached.state !== state || reached.operation_state !== "succeeded") {
        const latest = reached.operation_id ? await operation(reached.operation_id) : null;
        throw new Error(`Registry reached ${reached.state} (operation ${reached.operation_state}), expected ${state}: ${reached.last_error} ${redact(latest?.result?.error ?? "")}`);
      }
      return reached;
    };
    const workload = async () => (await get(`/api/workloads/${appId}`)) as { compose: string; source: typeof source | null; routes?: unknown; environment?: unknown };
    const composeContainer = (resource: string): Container => {
      const ids = docker("ps", "-q", "--filter", `label=com.docker.compose.project=cove-${resource}`, "--filter", "label=com.docker.compose.service=app").split(/\s+/).filter(Boolean);
      expect(ids, `running app containers of ${resource}`).toHaveLength(1);
      const [id = "", image = "", imageId = "", started = "", restarts = "", dataVolume = ""] = docker("inspect", "--format", '{{.Id}}|{{.Config.Image}}|{{.Image}}|{{.State.StartedAt}}|{{.RestartCount}}|{{range .Mounts}}{{if eq .Destination "/data"}}{{.Name}}{{end}}{{end}}', ids[0] ?? "").split("|");
      return { id, image, imageId, started, restarts, dataVolume };
    };
    const readFile = (container: string, path: string) => docker("exec", container, "sh", "-ec", 'if test -e "$1"; then cat "$1"; else printf absent; fi', "sh", path);
    const productionState = async () => {
      const saved = await workload();
      const container = composeContainer(appId);
      return { compose: saved.compose, source: saved.source, routes: saved.routes ?? null, environment: saved.environment ?? null, container, sentinel: readFile(container.id, "/data/production-only"), index: readFile(container.id, "/www/index.html") };
    };
    const respondedTo = (path: string, method: string) => page.waitForResponse((response) => new URL(response.url()).pathname === path && response.request().method() === method);
    const ciStatus = async () => (await get(`/api/projects/${projectId}/preview-images`)) as CIStatus;
    const previews = async () => ((await get(`/api/projects/${projectId}/environment-previews`)) as { previews: Preview[] }).previews;
    const previewDetail = async (id: string) => (await get(`/api/environment-previews/${id}`)) as Preview;
    const ciResource = (preview: Preview) => {
      const found = (preview.resources ?? []).filter((resource) => resource.base_id === appId);
      expect(found, "the preview resource copied from the production app").toHaveLength(1);
      const [resource] = found;
      if (!resource) throw new Error("The preview has no resource copied from the production app");
      return resource;
    };
    const deployOperations = async (resource: string) =>
      ((await get(`/api/operations?workload_id=${resource}`)) as Operation[])
        .filter((item) => item.workload_id === resource && /deploy|rollback|build/.test(item.action))
        .map((item) => item.id)
        .sort();
    // Waits for the preview to settle, then fails at once with redacted operation logs unless it reached the target.
    const previewReaches = async (id: string, target: "ready" | "awaiting_approval") => {
      await expect.poll(async () => (await previewDetail(id)).state, { timeout: 180_000 }).toMatch(new RegExp(`^(${target}|failed)$`));
      const reached = await previewDetail(id);
      if (reached.state !== target) {
        const diagnostics: string[] = [];
        for (const resource of reached.resources ?? []) {
          const ids = new Set([resource.operation, ...((await get(`/api/operations?workload_id=${resource.resource_id}`)) as Operation[]).filter((item) => item.state !== "succeeded").map((item) => item.id)].filter(Boolean));
          for (const operationId of ids) diagnostics.push(`${resource.name} ${operationId}: ${await attachLogs(operationId)}`);
        }
        throw new Error(`Preview ${id} reached ${reached.state} at ${reached.stage}, expected ${target}: ${redact(reached.detail)}\n${diagnostics.join("\n").slice(-6_000)}`);
      }
      return reached;
    };

    // Event bodies and the hook. The CI context has no session cookie; only the bearer it is given authenticates it.
    const event = (delivery: string, sequence: number, revision: string, image: string, preview = previewKey) => ({ delivery_id: delivery, preview, sequence, revision, images: { [appId]: { app: image } } });
    const hook = (token: string | null, body: unknown, project = projectId) =>
      ciContext.post(`/hooks/preview-images/${project}`, { headers: { ...(token === null ? {} : { Authorization: `Bearer ${token}` }), "Content-Type": "application/json" }, data: typeof body === "string" ? body : JSON.stringify(body) });
    const expectStatus = async (response: APIResponse, status: number, what: string) => {
      const text = await response.text();
      expectNoSecrets(text, what);
      expect(response.status(), `${what}: ${text.slice(0, 300)}`).toBe(status);
      return text;
    };
    const expectAccepted = async (response: APIResponse, duplicate: boolean, what: string) => {
      const body = JSON.parse(await expectStatus(response, 202, what)) as Record<string, unknown>;
      expect(Object.keys(body).sort(), what).toEqual(["duplicate", "preview_id"]);
      expect(body.duplicate, what).toBe(duplicate);
      expect(body.preview_id, what).toEqual(expect.stringMatching(resourceId));
      return body.preview_id as string;
    };
    // A fresh submission while the preview has an active operation is refused with 409 without consuming its delivery
    // or sequence; the documented client behaviour is to retry once settled, which is what CI would do.
    const submitFresh = async (token: string, body: ReturnType<typeof event>, what: string) => {
      for (let attempt = 1; ; attempt++) {
        const response = await hook(token, body);
        if (attempt < 3 && response.status() === 409 && (await response.text()).includes("wait for the current preview phase")) {
          if (previewResourceId) await expect.poll(async () => ((await get(`/api/operations?workload_id=${previewResourceId}`)) as Operation[]).filter((item) => item.state === "queued" || item.state === "running").length, { timeout: 120_000 }).toBe(0);
          continue;
        }
        return expectAccepted(response, false, what);
      }
    };

    // The one ready worker supports CI image previews and managed registries.
    const nodes = (await get("/api/nodes")) as { id: string; name: string; state: string; capabilities: { managed_registry?: boolean; preview_ci_images?: boolean } | null }[];
    expect(nodes.map((item) => item.name)).toEqual(["ready-browser-worker"]);
    const node = nodes[0];
    if (!node) throw new Error("The browser worker is missing");
    expect(node.state).toBe("ready");
    expect(node.capabilities?.managed_registry).toBe(true);
    expect(node.capabilities?.preview_ci_images).toBe(true);

    // Three owned images, committed by a confined producer container from the cached BusyBox. This simulates an external
    // CI system; it is not a Cove build operation and nothing here builds or pulls.
    const produce = async (marker: string, role: string) => {
      const seed = `${name}-ci-seed-${role}-${run}`;
      docker("run", "-d", "--pull", "never", "--name", seed, "--label", `cove.browser-fixture=${name}`, "--label", runLabel, "--network", "none", "--cap-drop", "ALL", "--security-opt", "no-new-privileges:true", "--memory", "64m", "--cpus", "0.25", "--pids-limit", "32", "busybox:1.37", "sh", "-ec", 'mkdir -p /www; printf %s "$1" > /www/index.html; exec sleep 300', "sh", marker);
      await expect.poll(() => docker("exec", seed, "cat", "/www/index.html"), { timeout: 30_000 }).toBe(marker);
      const image = docker("commit", "--change", `LABEL cove.browser-fixture=${name}`, "--change", `LABEL ${runLabel}`, "--change", `LABEL ${specLabel}.role=${role}`, "--change", 'CMD ["httpd","-f","-p","8080","-h","/www"]', seed);
      expect(docker("inspect", "--format", `{{index .Config.Labels "${specLabel}"}}`, seed)).toBe(run);
      docker("rm", "-f", seed);
      expect(image).toMatch(/^sha256:[a-f0-9]{64}$/);
      // A seed entrypoint would be inherited by the commit and run the Compose command as `sh httpd ...`.
      expect(["null", "[]"], `${role} image entrypoint`).toContain(docker("image", "inspect", "--format", "{{json .Config.Entrypoint}}", image));
      expect(JSON.parse(docker("image", "inspect", "--format", "{{json .Config.Cmd}}", image)), `${role} image command`).toEqual(["httpd", "-f", "-p", "8080", "-h", "/www"]);
      expect(image).not.toBe(baseImage);
      expect(imageLabels(image)).toMatchObject({ "cove.browser-fixture": name, [specLabel]: run, [`${specLabel}.role`]: role });
      return image;
    };
    const productionImage = await produce(markers.production, "production");
    const candidateOne = await produce(markers.one, "one");
    const candidateTwo = await produce(markers.two, "two");
    expect(new Set([productionImage, candidateOne, candidateTwo, baseImage]).size).toBe(4);

    // A project and a managed registry, repaired with the fixture leaf until it is ready.
    const projectReply = await page.request.post("/api/projects", { headers, data: { name: `CI previews ${run}` } });
    expect(projectReply.ok(), await projectReply.text()).toBe(true);
    projectId = ((await projectReply.json()) as { id: string }).id;
    const environmentReply = await page.request.post(`/api/projects/${projectId}/environments`, { headers, data: { name: "Production" } });
    const environmentText = await environmentReply.text();
    expect(environmentReply.status(), environmentText).toBe(201);
    const productionEnvironment = (JSON.parse(environmentText) as { id: string }).id;
    expect(productionEnvironment).toMatch(resourceId);
    const foreignReply = await page.request.post("/api/projects", { headers, data: { name: `CI previews foreign ${run}` } });
    expect(foreignReply.ok(), await foreignReply.text()).toBe(true);
    const foreignProject = ((await foreignReply.json()) as { id: string }).id;
    const retention = { keep_per_repository: 10, minimum_age_days: 30 };
    const createdReply = await page.request.post("/api/managed-registries", { headers, data: { name: `CI registry ${run}`, node_id: node.id, project_id: projectId, hostname, retention } });
    const createdText = await createdReply.text();
    expect(createdReply.status(), createdText).toBe(202);
    registryId = (JSON.parse(createdText) as { id: string }).id;
    deferImageChecks(registryId);
    const failed = await registryReaches("failed");
    expect(failed.last_error).toBe(tlsFailure);
    const uploadReply = await page.request.post(`/api/workloads/${registryId}/certificates`, { headers, data: { name: "Fixture registry leaf", certificate_pem: certificate, private_key_pem: key } });
    const uploadText = await uploadReply.text();
    expect(uploadReply.status(), redact(uploadText)).toBe(201);
    expectNoSecrets(uploadText, "certificate upload reply");
    const uploaded = JSON.parse(uploadText) as { id: string; dns_names: string[] };
    expect(uploaded.dns_names).toEqual([hostname]);
    expect((await page.request.put(`/api/managed-registries/${registryId}`, { headers, data: { retention, certificate_id: uploaded.id } })).status()).toBe(200);
    expect((await page.request.post(`/api/managed-registries/${registryId}/retry`, { headers })).status()).toBe(202);
    const ready = await registryReaches("ready");
    const generated = ((await get("/api/registries")) as RegistryConnection[]).filter((item) => item.managed_registry_id);
    expect(generated.map((item) => ({ id: item.id, managed: item.managed_registry_id, server: item.registry?.server, password: item.registry?.password }))).toEqual([{ id: ready.connection_id, managed: registryId, server: hostname, password: "" }]);
    expect((await verified(hostname, root, "/v2/")).status).toBe(401);

    // The production app: the production image by local ID, never pulled, a named data volume, a health check, the
    // synthetic Git source with saved Compose, a default-certificate route and the generated registry connection.
    const productionHost = `ci-production-${run}.example.test`;
    const routes = [{ domain: productionHost, service: "app", port: 8080, policy: { certificate_source: "default" } }];
    const compose = productionCompose(productionImage);
    const appReply = await page.request.post("/api/workloads", { headers, data: { name: appName, node_id: node.id, kind: "service", compose, source, routes, project_id: projectId, environment_id: productionEnvironment } });
    expect(appReply.status(), await appReply.text()).toBe(201);
    appId = ((await appReply.json()) as { id: string }).id;
    deferImageChecks(appId);
    const connect = await page.request.put(`/api/workloads/${appId}/registry`, { headers, data: { registry_id: ready.connection_id } });
    expect(connect.status(), await connect.text()).toBe(200);
    expect(await get(`/api/workloads/${appId}/registry`)).toEqual({ registry_id: ready.connection_id });
    const firstDeploy = await page.request.post(`/api/workloads/${appId}/operations`, { headers, data: { action: "deploy" } });
    expect(firstDeploy.status(), await firstDeploy.text()).toBe(202);
    await settled(((await firstDeploy.json()) as { id: string }).id);
    await teamQuiet();
    const deployedContainer = composeContainer(appId);
    expect({ image: deployedContainer.image, imageId: deployedContainer.imageId }).toEqual({ image: productionImage, imageId: productionImage });
    expect(deployedContainer.dataVolume).toBe(`cove-${appId}_data`);
    expect(readFile(deployedContainer.id, "/www/index.html")).toBe(markers.production);
    docker("exec", deployedContainer.id, "sh", "-ec", 'printf %s "$1" > /data/production-only', "sh", sentinel);
    const production = await productionState();
    expect(production.compose).toBe(compose);
    expect(production.source).toMatchObject(source);
    expect(production.sentinel).toBe(sentinel);
    expect(production.index).toBe(markers.production);
    // Route publication reaches the ingress asynchronously, so the first verified request is a bounded poll.
    const served = (host: string, authorization?: string) => verified(host, root, "/", authorization).then((result) => `${result.status}:${result.body}`, (error: Error) => `rejected: ${error.message}`);
    await expect.poll(() => served(productionHost), { timeout: 30_000 }).toBe(`200:${markers.production}`);
    const gitAfterProduction = gitCount();
    expect(gitAfterProduction, "the production deploy used the synthetic Git source").toBeGreaterThan(0);
    const expectProductionUnchanged = async (when: string) => {
      expect(await productionState(), `production after ${when}`).toEqual(production);
      const served = await verified(productionHost, root, "/");
      expect({ status: served.status, body: served.body }, `production traffic after ${when}`).toEqual({ status: 200, body: markers.production });
    };

    // Trusted fixture push auth: only the generated connection's own sealed configuration, decrypted in this process.
    // The CI credential is never used or broadened for this, and the values never reach arguments, files on the host,
    // logs or attachments.
    expect(ready.connection_id).toMatch(resourceId);
    const sealedHex = sql(`SELECT encode(c.configuration,'hex') FROM infrastructure_connections c JOIN managed_registries m ON m.connection_id=c.id WHERE c.id='${ready.connection_id}' AND m.id='${registryId}' AND c.kind='registry'`);
    expect(sealedHex).toMatch(/^[a-f0-9]+$/);
    const connection = JSON.parse(unseal(recipeKey, Buffer.from(sealedHex, "hex"))) as { registry?: { server?: unknown; username?: unknown; password?: unknown } };
    const pushServer = connection.registry?.server;
    const pushUser = connection.registry?.username;
    const pushPassword = connection.registry?.password;
    if (pushServer !== hostname || typeof pushUser !== "string" || typeof pushPassword !== "string" || pushUser === "" || pushPassword === "") throw new Error("The generated registry connection does not hold usable credentials for its registry");
    remember("registry push password", pushPassword);
    const workerAsCove = (args: string[], input?: string) =>
      execFileSync("docker", ["exec", ...(input === undefined ? [] : ["-i"]), "--user", "cove", worker, ...args], { input, encoding: "utf8", stdio: [input === undefined ? "ignore" : "pipe", "pipe", "pipe"] }).trim();
    workerAsCove(["sh", "-ec", 'umask 077; mkdir "$1"; cat > "$1/config.json"; test "$(stat -c %a "$1/config.json")" = 600', "sh", pushDirectory], JSON.stringify({ auths: { [hostname]: { auth: Buffer.from(`${pushUser}:${pushPassword}`).toString("base64") } } }));
    const pushed: Record<string, string> = {};
    for (const [role, image] of [["ci-one", candidateOne], ["ci-two", candidateTwo]] as const) {
      const tag = `${repository}:${role}`;
      workerAsCove(["docker", "--config", pushDirectory, "tag", image, tag]);
      const output = workerAsCove(["docker", "--config", pushDirectory, "push", tag]);
      expectNoSecrets(output, `push of ${role}`);
      const digests = (JSON.parse(docker("image", "inspect", "--format", "{{json .RepoDigests}}", image)) as string[]).filter((reference) => reference.startsWith(`${repository}@`));
      expect(digests, `repository digests of ${role}`).toHaveLength(1);
      const reference = digests[0] ?? "";
      expect(reference).toMatch(/@sha256:[a-f0-9]{64}$/);
      expect(output).toContain(reference.slice(reference.indexOf("@") + 1));
      pushed[role] = reference;
    }
    workerAsCove(["sh", "-ec", 'test -d "$1"; test ! -L "$1"; rm -f -- "$1/config.json"; rmdir -- "$1"', "sh", pushDirectory]);
    const digestOne = pushed["ci-one"] ?? "";
    const digestTwo = pushed["ci-two"] ?? "";
    expect(digestOne).not.toBe(digestTwo);

    // Remove each candidate's local aliases and image, so the preview can only get them through the registry.
    for (const [role, image, reference] of [["ci-one", candidateOne, digestOne], ["ci-two", candidateTwo, digestTwo]] as const) {
      const tag = `${repository}:${role}`;
      expect(imageLabels(image)).toMatchObject({ "cove.browser-fixture": name, [specLabel]: run });
      expect(image).not.toBe(productionImage);
      expect(image).not.toBe(baseImage);
      expect(JSON.parse(docker("image", "inspect", "--format", "{{json .RepoTags}}", image))).toEqual([tag]);
      docker("image", "rm", tag);
      if (imageExists(reference)) docker("image", "rm", reference);
      if (imageExists(image)) {
        expect(imageLabels(image)).toMatchObject({ "cove.browser-fixture": name, [specLabel]: run });
        docker("image", "rm", image);
      }
      expect({ tag: imageExists(tag), reference: imageExists(reference), image: imageExists(image) }, `local copies of ${role}`).toEqual({ tag: false, reference: false, image: false });
    }
    expect(imageExists(productionImage)).toBe(true);
    expect(imageExists("busybox:1.37")).toBe(true);

    // A CI-only recipe: no repositories, private access, approval, two previews, the app's service and an empty data
    // volume. The plan is read from the deployed release before saving.
    const environmentId = ((await get("/api/workloads")) as { id: string; environment_id: string }[]).find((item) => item.id === appId)?.environment_id ?? "";
    expect(environmentId).toBe(productionEnvironment);
    const registryEnvironment = ((await get("/api/workloads")) as { id: string; environment_id: string }[]).find((item) => item.id === registryId)?.environment_id ?? "";
    const planFor = async () => {
      const read = (await get(`/api/projects/${projectId}/preview-recipe?environment=${environmentId}`)) as { plan: { resources: PlanResource[] } | null };
      const item = read.plan?.resources.find((resource) => resource.id === appId);
      if (!item) throw new Error("The recipe plan does not list the production app");
      return item;
    };
    const unsavedPlan = await planFor();
    expect(unsavedPlan.volumes, "the app's planned volumes").toHaveLength(1);
    const volumeKey = unsavedPlan.volumes[0]?.key ?? "";
    expect(volumeKey).toBe("volume:data");
    const resources: Record<string, unknown> = { [appId]: { services: { app: true }, volumes: { [volumeKey]: "empty" } } };
    if (registryEnvironment === environmentId) resources[registryId] = { exclude: true };
    const recipe = { enabled: true, base_environment_id: environmentId, branch_pattern: "staging/*", repositories: [], domain_template: "{resource}-{preview}.example.test", access: "private", ttl_hours: 72, max_previews: 2, require_approval: true, resources };
    const recipeReply = await page.request.put(`/api/projects/${projectId}/preview-recipe`, { headers, data: recipe });
    expect(recipeReply.status(), await recipeReply.text()).toBe(200);
    const savedPlan = await planFor();
    expect({ included: savedPlan.included, blockers: savedPlan.blockers, volumes: savedPlan.volumes.map((volume) => [volume.key, volume.mode]) }).toEqual({ included: true, blockers: [], volumes: [[volumeKey, "empty"]] });
    await expectProductionUnchanged("saving the recipe");

    // Step 2: issue the credential from the dashboard, from the keyboard.
    const previewsPath = `/projects/${projectId}/previews`;
    await page.goto(previewsPath);
    const section = page.getByRole("region", { name: "CI-built image previews", exact: true });
    const form = section.getByRole("form", { name: "CI credential settings" });
    const appChoice = form.getByRole("checkbox", { name: new RegExp(`^${appName}`) });
    await expect(appChoice).toBeEnabled({ timeout: 60_000 });
    await expect(section.getByText(`${url}/hooks/preview-images/${projectId}`, { exact: true })).toBeVisible();
    await expect(form.getByLabel("Expires after", { exact: true })).toHaveValue("30");
    await capture("ci-credential-setup");
    await appChoice.focus();
    await page.keyboard.press("Space");
    await expect(appChoice).toBeChecked();
    const issueButton = form.getByRole("button", { name: "Issue credential", exact: true });
    await tabTo(page, issueButton);
    expect(await issueButton.evaluate((element) => element.matches(":focus-visible")), "the Issue credential button shows its keyboard focus ring").toBe(true);
    await page.keyboard.press("Enter");
    const issueDialog = page.getByRole("dialog", { name: "Issue a CI credential?" });
    await expect(issueDialog).toContainText("run images of their choice in previews of the selected resources");
    await expect(issueDialog.getByRole("button", { name: "Cancel", exact: true })).toBeFocused();
    const issueConfirm = issueDialog.getByRole("button", { name: "Issue credential", exact: true });
    await page.keyboard.press("Tab");
    await expect(issueConfirm).toBeFocused();
    const credentialsPath = `/api/projects/${projectId}/preview-images`;
    const issueCredential = async (dialog: Locator, confirm: Locator, outcome: string) => {
      await expect(section.getByText(outcome, { exact: true })).toHaveCount(0);
      const reply = respondedTo(credentialsPath, "PUT");
      await page.keyboard.press("Enter");
      const response = await reply;
      expect(response.status()).toBe(201);
      expect(response.request().postDataJSON()).toEqual({ resource_ids: [appId], days: 30 });
      const body = (await response.json()) as Record<string, unknown>;
      expect(Object.keys(body).sort()).toEqual(["expires_at", "token"]);
      const token = typeof body.token === "string" ? body.token : "";
      remember(`CI token issued as ${outcome}`, token);
      expect(Number.isNaN(Date.parse(String(body.expires_at)))).toBe(false);
      await expect(dialog).toBeHidden();
      await expect(confirm).toHaveCount(0);
      await expect(section.getByText(outcome, { exact: true })).toBeVisible();
      const reveal = section.getByLabel("CI credential", { exact: true });
      await expect(reveal).toBeVisible();
      expect((await reveal.inputValue()) === token, "the revealed credential is the issued token").toBe(true);
      await expect(section.getByText("Copy it to your CI secret store before clearing this message. Cove cannot show it again.", { exact: false })).toBeVisible();
      const stored = await page.evaluate(() => JSON.stringify({ ...localStorage }) + JSON.stringify({ ...sessionStorage }) + location.href);
      expect(stored.includes(token), "browser storage or URL holds the token").toBe(false);
      expectNoSecrets(await (await page.request.get(credentialsPath)).text(), "CI settings read");
      await section.getByRole("button", { name: "Clear", exact: true }).click();
      await expect(reveal).toHaveCount(0);
      expect((await page.locator("body").innerText()).includes(token)).toBe(false);
      return token;
    };
    const tokenOne = await issueCredential(issueDialog, issueConfirm, "Credential issued");
    const issuedStatus = await ciStatus();
    expect(issuedStatus.configuration).toMatchObject({ resource_ids: [appId], matches_scope: true, last_received_at: null });
    await expect(section.getByText("Accepting submissions", { exact: true })).toBeVisible();
    await capture("ci-credential-issued");

    // Step 3: authentication, scope and completeness. Every refusal leaves no preview, no ledger entry and production as it was.
    const revisionOne = randomBytes(20).toString("hex");
    const revisionTwo = randomBytes(20).toString("hex");
    const valid = event("build-1", 1, revisionOne, digestOne);
    await expectStatus(await page.request.post(`/hooks/preview-images/${projectId}`, { headers: { ...headers, "Content-Type": "application/json" }, data: JSON.stringify(valid) }), 401, "hook with an admin session and no bearer");
    await expectStatus(await hook(null, valid), 401, "hook without a bearer");
    await expectStatus(await ciContext.get(`/api/workloads/${appId}`, { headers: { Authorization: `Bearer ${tokenOne}` } }), 401, "CI bearer reading the workload");
    await expectStatus(await ciContext.get(credentialsPath, { headers: { Authorization: `Bearer ${tokenOne}` } }), 401, "CI bearer reading CI settings");
    await expectStatus(await ciContext.post(`/api/workloads/${appId}/operations`, { headers: { Authorization: `Bearer ${tokenOne}` }, data: { action: "deploy" } }), 401, "CI bearer deploying production");
    await expectStatus(await hook(tokenOne, valid, foreignProject), 401, "CI bearer on another project");
    const digestOnly = digestOne.slice(digestOne.indexOf("@") + 1);
    const refusals: [string, unknown][] = [
      ["an empty image map", { ...valid, images: {} }],
      ["a missing service", { ...valid, images: { [appId]: {} } }],
      ["a mutable tag", { ...valid, images: { [appId]: { app: `${repository}:ci-one` } } }],
      ["a tag with a digest", { ...valid, images: { [appId]: { app: `${repository}:ci-one@${digestOnly}` } } }],
      ["another registry", { ...valid, images: { [appId]: { app: `registry.other.example.test/fixture/ci-app@${digestOnly}` } } }],
      ["an unknown field", { ...valid, environment: { DEBUG: "1" } }],
    ];
    for (const [label, body] of refusals) await expectStatus(await hook(tokenOne, body), 400, `submission with ${label}`);
    expect(await previews()).toEqual([]);
    expect(await ciStatus()).toMatchObject({ used_preview_keys: 0, retained_deliveries: 0 });
    await expectProductionUnchanged("refused submissions");

    // Step 4: one accepted submission, exact replay, conflicting reuse and a stale sequence.
    const gitBeforeCI = gitCount();
    expect(gitBeforeCI).toBe(gitAfterProduction);
    previewId = await submitFresh(tokenOne, valid, "first CI submission");
    expect(await expectAccepted(await hook(tokenOne, valid), true, "exact replay")).toBe(previewId);
    await expectStatus(await hook(tokenOne, { ...valid, revision: revisionTwo }), 409, "reused delivery ID with different inputs");
    await expectStatus(await hook(tokenOne, { ...valid, delivery_id: "build-1-again" }), 409, "new delivery with the same sequence");
    const expectWaiting = async (when: string) => {
      const listed = await previews();
      expect(listed.map((item) => ({ id: item.id, ci: item.ci, state: item.state, stage: item.stage, environment: item.environment_id, key: item.suffix })), `previews ${when}`).toEqual([{ id: previewId, ci: true, state: "awaiting_approval", stage: "approval", environment: "", key: previewKey }]);
      expect((await previewDetail(previewId)).resources ?? [], `preview resources ${when}`).toEqual([]);
      expect(gitCount(), `Git calls ${when}`).toBe(gitBeforeCI);
      expect(await ciStatus(), `CI ledger ${when}`).toMatchObject({ used_preview_keys: 1, retained_deliveries: 1 });
    };
    await expectWaiting("after the accepted submission");
    await expectProductionUnchanged("the accepted submission");

    // Restart only the guarded fixture panel; the accepted delivery and preview persist in the database.
    await teamQuiet();
    const panelNow = containerIdentity(panel);
    expect({ id: panelNow.id, name: panelNow.name, image: panelNow.image }).toEqual({ id: panelBefore.id, name: `/${panel}`, image: `${panel}:test` });
    expect(panelNow.networks).toContain(name);
    docker("restart", "--time", "10", panel);
    const panelAfter = containerIdentity(panel);
    expect({ id: panelAfter.id, name: panelAfter.name, image: panelAfter.image }).toEqual({ id: panelBefore.id, name: `/${panel}`, image: `${panel}:test` });
    expect(panelAfter.started).not.toBe(panelNow.started);
    await expect.poll(async () => page.request.get("/api/nodes").then((response) => response.status(), () => 0), { timeout: 60_000 }).toBe(200);
    await expect.poll(async () => ((await get("/api/nodes")) as { state: string }[]).map((item) => item.state), { timeout: 60_000 }).toEqual(["ready"]);
    expect(await expectAccepted(await hook(tokenOne, valid), true, "exact replay after the panel restart")).toBe(previewId);
    await expectWaiting("after the panel restart");
    await page.reload();

    // Step 5: the dashboard shows the CI preview waiting; approve it there.
    const previewList = page.getByRole("list", { name: `Previews in CI previews ${run}` });
    const waiting = await previewDetail(previewId);
    const row = previewList.getByRole("listitem").filter({ hasText: waiting.slug });
    await expect(row.getByText("Awaiting approval", { exact: true })).toBeVisible({ timeout: 60_000 });
    await expect(row.getByText("CI-built images", { exact: true })).toBeVisible();
    await capture("ci-preview-awaiting-approval");
    // The review dialog must show exactly the inputs the preview detail reports, and the approval is bound to their identity.
    const approve = async (label: string, revision: string, digest: string) => {
      const approvePath = `/api/environment-previews/${previewId}/approve`;
      const current = await previewDetail(previewId);
      const inputs = current.ci_inputs;
      if (!inputs) throw new Error(`The preview detail has no CI inputs to review for ${label}`);
      expect(inputs.identity).toMatch(/^[0-9a-f]{64}$/);
      expect({ revision: inputs.revision, images: inputs.images }).toEqual({ revision, images: { [appId]: { app: digest } } });
      expect(inputs.recipe_version).toBeGreaterThan(0);
      await row.getByRole("button", { name: "Review and approve", exact: true }).click();
      const dialog = page.getByRole("dialog", { name: `Review CI inputs for ${current.slug}`, exact: true });
      const confirm = dialog.getByRole("button", { name: "Approve CI inputs", exact: true });
      await expect(confirm).toBeEnabled();
      await expect(dialog.getByRole("button", { name: "Cancel", exact: true })).toBeFocused();
      await expect(dialog.getByText(current.environment_id ? `Approving updates the existing preview ${current.slug} to these images.` : `Approving creates the preview ${current.slug} from these images.`, { exact: true })).toBeVisible();
      await expect(dialog.getByText(inputs.revision, { exact: true })).toBeVisible();
      await expect(dialog).toContainText(`Version ${inputs.recipe_version}, frozen for this preview`);
      const shown = dialog.getByRole("list", { name: "Images to run", exact: true });
      for (const [resource, services] of Object.entries(inputs.images)) {
        await expect(shown).toContainText(resource);
        for (const [service, reference] of Object.entries(services)) await expect(shown.getByText(`${service} ${reference}`, { exact: true })).toBeVisible();
      }
      await capture(label);
      const reply = respondedTo(approvePath, "POST");
      await confirm.click();
      const response = await reply;
      expect(response.status(), await response.text()).toBe(202);
      expect(response.request().postDataJSON()).toEqual({ ci_identity: inputs.identity });
      await expect(dialog).toHaveCount(0);
    };
    await approve("ci-preview-review", revisionOne, digestOne);
    const first = await previewReaches(previewId, "ready");
    const firstResource = ciResource(first);
    previewResourceId = firstResource.resource_id;
    expect(previewResourceId).toMatch(resourceId);
    expect(previewResourceId).not.toBe(appId);
    deferImageChecks(previewResourceId);
    expect({ ci: first.ci, access: first.access, source: firstResource.source, revision: firstResource.revision, images: firstResource.ci_images, deployed: firstResource.deployed }).toEqual({ ci: true, access: "private", source: "ci", revision: revisionOne, images: { app: digestOne }, deployed: true });
    const previewHost = `${appName}-${first.slug}.example.test`;
    expect(firstResource.routes).toEqual([previewHost]);
    const previewContainer = composeContainer(previewResourceId);
    expect(previewContainer.image).toBe(digestOne);
    expect(readFile(previewContainer.id, "/www/index.html")).toBe(markers.one);
    expect(gitCount(), "Git calls after the CI preview deploy").toBe(gitBeforeCI);
    expect(builders(), "BuildKit builders after the CI preview deploy").toEqual(buildersBefore);
    expect((await operations()).filter((item) => /build/.test(item.action)).map((item) => `${item.action}:${item.id}`)).toEqual([]);
    expect(previewContainer.dataVolume).not.toBe("");
    expect(previewContainer.dataVolume).not.toBe(production.container.dataVolume);
    expect(readFile(previewContainer.id, "/data/production-only")).toBe("absent");
    docker("exec", previewContainer.id, "sh", "-ec", 'printf %s "$1" > /data/preview-marker', "sh", previewMarker);
    await expectProductionUnchanged("the first CI preview deploy");

    // The private preview route is verified against the fixture CA and needs the preview's own generated credentials,
    // read through the admin API. The CI credential cannot read them.
    await expect.poll(() => verified(previewHost, root, "/").then((result) => result.status, (error: Error) => `rejected: ${error.message}`), { timeout: 30_000 }).toBe(401);
    const routeCredentials = (await get(`/api/environment-previews/${previewId}/credentials`)) as Record<string, string>;
    expect(Object.keys(routeCredentials).sort()).toEqual(["password", "username"]);
    remember("preview route password", routeCredentials.password ?? "");
    const routeAuth = `Basic ${Buffer.from(`${routeCredentials.username ?? ""}:${routeCredentials.password ?? ""}`).toString("base64")}`;
    remember("preview route authorization", routeAuth);
    const servedOne = await verified(previewHost, root, "/", routeAuth);
    expect({ status: servedOne.status, body: servedOne.body }).toEqual({ status: 200, body: markers.one });
    await expectStatus(await ciContext.get(`/api/environment-previews/${previewId}/credentials`, { headers: { Authorization: `Bearer ${tokenOne}` } }), 401, "CI bearer reading preview credentials");

    await expect(row.getByText("Ready", { exact: true })).toBeVisible({ timeout: 60_000 });
    await capture("ci-preview-ready");
    const detailPath = `/projects/${projectId}/environments/${first.environment_id}`;
    await page.goto(detailPath);
    const previewSection = page.getByRole("region", { name: `Preview ${first.slug}`, exact: true });
    const resourceTable = previewSection.getByRole("table", { name: "Preview resources, what they run and where their data came from", exact: true });
    const visibleResourceText = (text: string, exact: boolean) => resourceTable.getByText(text, { exact }).filter({ visible: true });
    await expect(previewSection.getByText("CI-built images", { exact: true })).toBeVisible({ timeout: 30_000 });
    await expect(visibleResourceText(revisionOne, true)).toBeVisible();
    await expect(visibleResourceText(digestOne, false)).toBeVisible();
    await capture("ci-preview-detail");

    // Step 6: the second candidate, also only in the registry. Before approval the preview keeps serving the first image.
    const updateBody = event("build-2", 2, revisionTwo, digestTwo);
    expect(await submitFresh(tokenOne, updateBody, "update submission")).toBe(previewId);
    expect(await expectAccepted(await hook(tokenOne, valid), true, "first delivery replayed after the update")).toBe(previewId);
    await expectStatus(await hook(tokenOne, event("build-stale", 1, revisionOne, digestOne)), 409, "stale sequence with a new delivery ID");
    const awaitingUpdate = await previewReaches(previewId, "awaiting_approval");
    expect(awaitingUpdate.environment_id).toBe(first.environment_id);
    expect(composeContainer(previewResourceId)).toEqual(previewContainer);
    const servedBefore = await verified(previewHost, root, "/", routeAuth);
    expect({ status: servedBefore.status, body: servedBefore.body }).toEqual({ status: 200, body: markers.one });
    await expectProductionUnchanged("the update submission");
    await page.goto(previewsPath);
    await expect(row.getByText("Awaiting approval", { exact: true })).toBeVisible({ timeout: 60_000 });
    await capture("ci-preview-update-awaiting-approval");
    await approve("ci-preview-update-review", revisionTwo, digestTwo);
    const second = await previewReaches(previewId, "ready");
    const secondResource = ciResource(second);
    expect({ resource: secondResource.resource_id, source: secondResource.source, revision: secondResource.revision, images: secondResource.ci_images, routes: secondResource.routes }).toEqual({ resource: previewResourceId, source: "ci", revision: revisionTwo, images: { app: digestTwo }, routes: [previewHost] });
    const updatedContainer = composeContainer(previewResourceId);
    expect(updatedContainer.image).toBe(digestTwo);
    expect(updatedContainer.dataVolume).toBe(previewContainer.dataVolume);
    expect(readFile(updatedContainer.id, "/www/index.html")).toBe(markers.two);
    expect(readFile(updatedContainer.id, "/data/preview-marker")).toBe(previewMarker);
    expect(readFile(updatedContainer.id, "/data/production-only")).toBe("absent");
    await expect.poll(() => served(previewHost, routeAuth), { timeout: 30_000 }).toBe(`200:${markers.two}`);
    expect(gitCount(), "Git calls after the CI update").toBe(gitBeforeCI);
    expect(builders(), "BuildKit builders after the CI update").toEqual(buildersBefore);
    expect((await operations()).filter((item) => /build/.test(item.action)).map((item) => `${item.action}:${item.id}`)).toEqual([]);
    await expectProductionUnchanged("the CI update");
    await page.goto(detailPath);
    await page.reload();
    await expect(previewSection.getByText("CI-built images", { exact: true })).toBeVisible({ timeout: 30_000 });
    await expect(visibleResourceText(revisionTwo, true)).toBeVisible();
    await expect(visibleResourceText(digestTwo, false)).toBeVisible();
    await expect(page.getByText(digestOne, { exact: false })).toHaveCount(0);
    await capture("ci-preview-update-detail");

    // Step 7: replace the credential from the dashboard; the old token stops working at once.
    await page.goto(previewsPath);
    const replaceButton = form.getByRole("button", { name: "Replace credential", exact: true });
    await expect(replaceButton).toBeEnabled({ timeout: 30_000 });
    await replaceButton.focus();
    await page.keyboard.press("Enter");
    const replaceDialog = page.getByRole("dialog", { name: "Replace the CI credential?" });
    await expect(replaceDialog).toContainText("The current credential stops working immediately.");
    await expect(replaceDialog.getByRole("button", { name: "Cancel", exact: true })).toBeFocused();
    const replaceConfirm = replaceDialog.getByRole("button", { name: "Replace credential", exact: true });
    await page.keyboard.press("Tab");
    await expect(replaceConfirm).toBeFocused();
    const tokenTwo = await issueCredential(replaceDialog, replaceConfirm, "Credential replaced");
    expect(tokenTwo === tokenOne, "the replacement token differs").toBe(false);
    await expectStatus(await hook(tokenOne, event("build-old-token", 3, revisionTwo, digestTwo)), 401, "old token after replacement");

    // The same image and revision with a higher sequence is accepted but neither redeploys nor asks for approval again.
    const deploysBefore = await deployOperations(previewResourceId);
    expect(await submitFresh(tokenTwo, event("build-3", 3, revisionTwo, digestTwo), "same identity with the new token")).toBe(previewId);
    // A bounded observation window: proving that nothing starts needs time for the preview workflow to run.
    for (let check = 0; check < 10; check++) {
      const observed = await previewDetail(previewId);
      expect(observed.state, "the preview stays ready for an unchanged identity").toBe("ready");
      expect(await deployOperations(previewResourceId)).toEqual(deploysBefore);
      await page.waitForTimeout(2_000);
    }
    expect(composeContainer(previewResourceId)).toEqual(updatedContainer);
    expect(gitCount()).toBe(gitBeforeCI);
    await expectProductionUnchanged("the unchanged-identity submission");

    // Close the preview from the dashboard while the credential is active, then prove the key cannot come back.
    const previewVolumes = () => docker("volume", "ls", "-q", "--filter", `name=cove-pv-${previewResourceId}`).split(/\s+/).filter(Boolean);
    expect(previewVolumes()).toContain(updatedContainer.dataVolume);
    await row.getByRole("button", { name: `Actions for preview ${first.slug}` }).click();
    await page.getByRole("menuitem", { name: "Delete preview" }).click();
    const deleteDialog = page.getByRole("dialog", { name: `Delete ${first.slug}?` });
    const deleteButton = deleteDialog.getByRole("button", { name: "Delete preview", exact: true });
    await expect(deleteButton).toBeDisabled();
    await deleteDialog.getByLabel(`Type ${first.slug} to confirm`, { exact: true }).fill(first.slug);
    const deleteReply = respondedTo(`/api/environment-previews/${previewId}`, "DELETE");
    await deleteButton.click();
    const deleted = await deleteReply;
    expect(deleted.status(), await deleted.text()).toBe(202);
    expect(deleted.request().postDataJSON()).toEqual({ confirm: first.slug });
    await expect.poll(async () => (await page.request.get(`/api/environment-previews/${previewId}`)).status(), { timeout: 180_000 }).toBe(404);
    expect(docker("ps", "-aq", "--filter", `label=com.docker.compose.project=cove-${previewResourceId}`)).toBe("");
    expect(docker("network", "ls", "-q", "--filter", `label=com.docker.compose.project=cove-${previewResourceId}`)).toBe("");
    expect(previewVolumes()).toEqual([]);
    expect(docker("volume", "ls", "-q", "--filter", `label=com.docker.compose.project=cove-${previewResourceId}`)).toBe("");
    await expect(previewList).toHaveCount(0, { timeout: 60_000 });
    // The project header and Previews tab drop the removed preview environment and its resource without a reload.
    const projectHeader = page.locator("main header");
    await expect(projectHeader.getByText(/^\d+ previews?$/)).toHaveCount(0, { timeout: 60_000 });
    await expect(projectHeader.getByText("2 resources", { exact: true })).toBeVisible();
    await expect(projectHeader.getByRole("navigation", { name: "Project sections" }).getByRole("link", { name: /^Previews/ })).toHaveText("Previews");
    await expectStatus(await hook(tokenTwo, event("build-4", 4, revisionTwo, digestTwo)), 410, "a new submission for the closed key");
    await expectProductionUnchanged("closing the preview");

    // Revoke from the dashboard; the DELETE has no body and the replacement token stops working.
    await section.getByRole("button", { name: "Revoke credential", exact: true }).click();
    const revokeDialog = page.getByRole("dialog", { name: "Revoke the CI credential?" });
    await expect(revokeDialog).toContainText("Existing previews and submissions that were already accepted are not cancelled.");
    const revokeReply = respondedTo(credentialsPath, "DELETE");
    await revokeDialog.getByRole("button", { name: "Revoke credential", exact: true }).click();
    const revoked = await revokeReply;
    expect(revoked.status(), await revoked.text()).toBe(200);
    expect(await revoked.json()).toEqual({ ok: true });
    expect(revoked.request().postData()).toBeNull();
    await expect(section.getByText("Credential revoked", { exact: true })).toBeVisible();
    await expect(section.getByRole("button", { name: "Revoke credential", exact: true })).toHaveCount(0);
    expect((await ciStatus()).configuration).toBeNull();
    await expectStatus(await hook(tokenTwo, event("build-5", 5, revisionTwo, digestTwo, "pr43")), 401, "replacement token after revocation");
    await capture("ci-credential-revoked-preview-removed");
    await expectProductionUnchanged("revoking the credential");

    // Tear down the source consumer, then the registry: retain, purge and delete its record.
    expect((await page.request.put(`/api/workloads/${appId}/registry`, { headers, data: { registry_id: "" } })).status()).toBe(200);
    await teamQuiet();
    const stopReply = await page.request.post(`/api/workloads/${appId}/operations`, { headers, data: { action: "stop" } });
    expect(stopReply.status(), await stopReply.text()).toBe(202);
    await settled(((await stopReply.json()) as { id: string }).id);
    const deletedApp = await page.request.delete(`/api/workloads/${appId}`, { headers });
    expect(deletedApp.status(), await deletedApp.text()).toBe(200);
    appDeleted = true;
    await teamQuiet();
    const removeReply = await page.request.delete(`/api/managed-registries/${registryId}`, { headers, data: { delete_data: false } });
    expect(removeReply.status(), await removeReply.text()).toBe(202);
    const retained = await registryReaches("removed");
    expect(retained.data_retained).toBe(true);
    await teamQuiet();
    const purgeReply = await page.request.delete(`/api/managed-registries/${registryId}`, { headers, data: { delete_data: true } });
    expect(purgeReply.status(), await purgeReply.text()).toBe(202);
    expect((await registryReaches("removed")).data_retained).toBe(false);
    expect(docker("volume", "ls", "-q", "--filter", `label=dev.cove.managed-registry=${registryId}`)).toBe("");
    await teamQuiet();
    const recordReply = await page.request.delete(`/api/managed-registries/${registryId}/record`, { headers });
    expect(recordReply.status(), await recordReply.text()).toBe(200);
    expect(await recordReply.json()).toEqual({ ok: true });
    registryRecordDeleted = true;
  } catch (error) {
    primary = asError(error);
    // Cleanup deletes resources and their logs, so failed operation logs are kept first, redacted.
    try {
      const response = await page.request.get("/api/operations");
      if (response.status() === 200) {
        for (const item of (await response.json()) as Operation[]) {
          if (item.state !== "failed" && item.state !== "interrupted" && item.state !== "cancelled") continue;
          const logs = await page.request.get(`/api/operations/${item.id}/logs`);
          if (logs.status() === 200) await testInfo.attach(`failed-operation-${item.id}.txt`, { body: redact(`${item.action} ${item.state}: ${item.result?.error ?? ""}\n${(JSON.parse(await logs.text()) as { logs: string }).logs}`).slice(-6_000), contentType: "text/plain" });
        }
      }
    } catch (diagnostic) {
      testInfo.annotations.push({ type: "diagnostic-failure", description: redact(asError(diagnostic).message).slice(0, 300) });
    }
  }

  // Cleanup never replaces the first failure, and touches only resources whose labels or names prove they are this run's.
  const cleanupFailures: string[] = [];
  const attempt = async (label: string, action: () => unknown) => {
    try {
      await action();
    } catch (error) {
      cleanupFailures.push(`${label}: ${redact(asError(error).message).slice(0, 300)}`);
    }
  };
  await attempt("CI request context", () => ci?.dispose());
  await attempt("browser resources", () => cleanupBrowserResources());
  await attempt("producer containers", () => {
    for (const id of docker("ps", "-aq", "--filter", `label=${runLabel}`).split(/\s+/).filter(Boolean)) {
      expect(docker("inspect", "--format", '{{index .Config.Labels "cove.browser-fixture"}}', id)).toBe(name);
      docker("rm", "-f", id);
    }
  });
  await attempt("push configuration", () => {
    const present = docker("exec", "--user", "cove", worker, "sh", "-ec", 'if test -e "$1"; then echo present; else echo absent; fi', "sh", pushDirectory);
    if (present === "present") docker("exec", "--user", "cove", worker, "sh", "-ec", 'test -d "$1"; test ! -L "$1"; rm -f -- "$1/config.json"; rmdir -- "$1"', "sh", pushDirectory);
  });
  if (previewResourceId) {
    await attempt("preview volumes", () => {
      for (const volume of docker("volume", "ls", "-q", "--filter", `name=cove-pv-${previewResourceId}`).split(/\s+/).filter(Boolean)) if (volume.startsWith(`cove-pv-${previewResourceId}-`)) docker("volume", "rm", volume);
    });
  }
  if (appId) {
    await attempt("production data volume", () => {
      for (const volume of docker("volume", "ls", "-q", "--filter", `label=com.docker.compose.project=cove-${appId}`).split(/\s+/).filter(Boolean)) if (volume === `cove-${appId}_data`) docker("volume", "rm", volume);
    });
  }
  if (registryId && !registryRecordDeleted) {
    await attempt("registry data volume", () => {
      for (const volume of docker("volume", "ls", "-q", "--filter", `label=dev.cove.managed-registry=${registryId}`).split(/\s+/).filter(Boolean)) if (volume === `cove-${registryId}_data`) docker("volume", "rm", volume);
    });
  }
  await attempt("fixture images", () => {
    for (const id of docker("image", "ls", "-q", "--no-trunc", "--filter", `label=${runLabel}`).split(/\s+/).filter(Boolean)) {
      if (id === baseImage) throw new Error("Refusing to remove the base image");
      expect(imageLabels(id)).toMatchObject({ "cove.browser-fixture": name, [specLabel]: run });
      docker("image", "rm", id);
    }
  });
  if (!appDeleted && appId) testInfo.annotations.push({ type: "cleanup", description: `production app ${appId} was stopped by the shared browser cleanup, not deleted` });
  if (primary !== null) {
    for (const failure of cleanupFailures) testInfo.annotations.push({ type: "cleanup-failure", description: failure });
    throw primary;
  }
  if (cleanupFailures.length > 0) throw new Error(`Cleanup failed: ${cleanupFailures.join("; ")}`);
});

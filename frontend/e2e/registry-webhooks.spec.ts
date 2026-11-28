import { execFileSync } from "node:child_process";
import { mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { createServer } from "node:net";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { expect, test, type Page, type TestInfo } from "@playwright/test";
import { cleanupBrowserResources, expectNoHorizontalScroll } from "./route-tls";

// Runs only with scripts/test-browser.py --registry-webhooks. A real local Distribution registry (the harness's
// verified, pinned registry image, never pulled) receives a real docker push and notifies the panel inside the
// fixture network.
const password = process.env.COVE_BROWSER_PASSWORD ?? "";
const enabled = process.env.COVE_BROWSER_REGISTRY_HOOKS === "1";
// Set by the harness when the panel is served under a path prefix; panel pages, APIs and hooks live only under it.
const base = process.env.COVE_BROWSER_BASE_PATH ?? "";
const panelPath = (route: string) => `${base}${route}`;

const docker = (...args: string[]) => execFileSync("docker", args, { encoding: "utf8", stdio: ["ignore", "pipe", "pipe"] }).trim();
const specLabel = "cove.registry-hooks-spec=1";

// What this spec created on the Docker host. Each item also carries the fixture label, so the harness removes it
// even if this cleanup does not run.
const owned = { fixture: "", registry: "", seed: "", volume: "", tag: "", directory: "" };

function ownedBySpec(kind: "container" | "volume" | "image", name: string) {
  const labels = kind === "volume" ? ".Labels" : ".Config.Labels";
  const format = `{{index ${labels} "cove.registry-hooks-spec"}}|{{index ${labels} "cove.browser-fixture"}}`;
  return docker(kind, "inspect", "--format", format, name) === `1|${owned.fixture}`;
}

test.afterEach(() => {
  cleanupBrowserResources();
  for (const name of [owned.registry, owned.seed]) if (name && ownedBySpec("container", name)) docker("rm", "-f", "-v", name);
  if (owned.volume && ownedBySpec("volume", owned.volume)) docker("volume", "rm", owned.volume);
  if (owned.tag && ownedBySpec("image", owned.tag)) docker("image", "rm", owned.tag);
  if (owned.directory) rmSync(owned.directory, { recursive: true, force: true });
  Object.assign(owned, { fixture: "", registry: "", seed: "", volume: "", tag: "", directory: "" });
});

// The fixture's name, inferred from the worker the harness exports and checked against its database and network.
function fixtureOwner() {
  const owner = /^(cove-browser-[a-f0-9]{12})-worker$/.exec(process.env.COVE_BROWSER_WORKER ?? "")?.[1];
  const database = `${owner}-db`;
  if (!owner || process.env.COVE_BROWSER_DATABASE !== database) throw new Error("Isolated browser fixture required");
  const panel = `${owner}-panel`;
  // The panel must sit on the fixture's own network, which the registry joins to deliver notifications.
  const networks = docker("inspect", "--format", "{{range $name, $_ := .NetworkSettings.Networks}}{{$name}} {{end}}", panel).split(/\s+/);
  if (!networks.includes(owner)) throw new Error(`The fixture panel is not on network ${owner}`);
  return { owner, panel, database };
}

function freePort() {
  return new Promise<number>((resolve, reject) => {
    const server = createServer();
    server.once("error", reject);
    server.listen(0, "127.0.0.1", () => {
      const address = server.address();
      server.close(() => (typeof address === "object" && address ? resolve(address.port) : reject(new Error("No free port"))));
    });
  });
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
  created_at: string;
  approval_required?: boolean;
  approved_at?: string | null;
  reason?: string;
  result: { error?: string } | null;
}

interface HookStatus {
  configuration: { provider: string; enabled: boolean; filters: { repository: string; tag: string }[]; registry_id: string; registry_server: string } | null;
  endpoint: string | null;
  matches_saved_registry: boolean;
  last_received_at: string | null;
  last_matched_at: string | null;
  pending_at: string | null;
  last_operation: { id: string; state: string } | null;
  last_error: string;
  last_ignored_reason: string;
  ownership_required: boolean;
}

const compose = (image: string, extra: string[] = []) =>
  ["services:", "  web:", `    image: ${image}`, ...extra, "    init: true", "    restart: unless-stopped", "    command: [sh, -c, 'exec sleep 86400']", ""].join("\n");

test("registry webhooks: a real local Distribution push queues an approved pull and redeploy", async ({ page }, testInfo) => {
  test.skip(!enabled, "Needs scripts/test-browser.py --registry-webhooks");
  test.setTimeout(600_000);
  const { owner, panel, database } = fixtureOwner();
  owned.fixture = owner;
  const registryImage = process.env.COVE_BROWSER_REGISTRY_IMAGE ?? "";
  const registryPin = process.env.COVE_BROWSER_REGISTRY_PIN ?? "";
  if (!/^sha256:[a-f0-9]{64}$/.test(registryImage) || !/^registry@sha256:[a-f0-9]{64}$/.test(registryPin)) throw new Error("The harness must provide the verified local registry image");
  // The local image must be the pinned official build; it is never pulled.
  expect(docker("image", "inspect", "--format", "{{.Id}}", registryImage)).toBe(registryImage);
  // A docker save and load can drop RepoDigests but keeps the content ID, so the exact official config ID of the
  // pinned manifest is accepted without them. Any other image must still carry the pinned digest.
  const officialManifest = "registry@sha256:a3d8aaa63ed8681a604f1dea0aa03f100d5895b6a58ace528858a7b332415373";
  const officialConfig = "sha256:26b2eb03618e749084668eaff68cff8f81dda12d06ac641be7a6398b82a6f25b";
  const digests = docker("image", "inspect", "--format", "{{range .RepoDigests}}{{.}} {{end}}", registryImage).split(/\s+/).filter(Boolean).map((digest) => digest.replace(/^(?:docker\.io\/)?(?:library\/)?/, ""));
  if (digests.length > 0 || registryImage !== officialConfig || registryPin !== officialManifest) expect(digests).toContain(registryPin);
  const sql = (query: string) => docker("exec", database, "psql", "-U", "postgres", "-d", "cove", "-Atc", query);

  await page.goto(panelPath("/projects"));
  await expect(page).toHaveURL(/\/login(?:[?#]|$)/);
  await page.getByLabel("Password", { exact: true }).fill(password);
  await page.getByRole("button", { name: "Sign in", exact: true }).click();
  await expect(page).not.toHaveURL(/\/login(?:[?#]|$)/);
  const origin = new URL(page.url()).origin;
  const headers = { Origin: origin };

  const nodes = (await (await page.request.get(panelPath("/api/nodes"))).json()) as { id: string; name: string; capabilities: { image_lifecycle?: boolean } | null }[];
  const node = nodes.find((item) => item.name === "ready-browser-worker");
  if (!node) throw new Error("The browser worker is missing");
  expect(node.capabilities?.image_lifecycle).toBe(true);

  const port = await freePort();
  const server = `127.0.0.1:${port}`;
  const image = `${server}/team/app:stable`;

  // The initial release runs the cached busybox image, so /version does not exist yet.
  const project = (await (await page.request.post(panelPath("/api/projects"), { headers, data: { name: "Registry webhook project" } })).json()) as { id: string };
  const created = await page.request.post(panelPath("/api/workloads"), { headers, data: { name: "registry-hook", node_id: node.id, kind: "service", compose: compose("busybox:1.37", ["    pull_policy: never"]), project_id: project.id } });
  expect(created.status(), await created.text()).toBe(201);
  const { id } = (await created.json()) as { id: string };
  expect(id).toMatch(/^[a-f0-9]{32}$/);
  // Background image checks start due; defer this resource's so only webhook deploys touch it.
  expect(sql(`UPDATE workloads SET next_image_check_at=now()+interval '1 day' WHERE id='${id}' RETURNING id`).split(/\s+/).filter((value) => value === id)).toEqual([id]);

  const operations = async () => (await (await page.request.get(panelPath(`/api/operations?workload_id=${id}`))).json()) as Operation[];
  const deploys = async () => (await operations()).filter((item) => item.action === "deploy");
  const operation = async (operationId: string) => (await (await page.request.get(panelPath(`/api/operations/${operationId}`))).json()) as Operation;
  const finished = async (operationId: string, timeout = 240_000) => {
    await expect.poll(async () => (await operation(operationId)).state, { timeout }).toMatch(/^(succeeded|failed|cancelled|interrupted)$/);
    return operation(operationId);
  };
  // The operations list is a summary; admission marks approval on the operation itself, so read it directly.
  const awaitingApproval = async (operationId: string) => {
    await expect.poll(async () => (await operation(operationId)).approval_required === true, { timeout: 120_000 }).toBe(true);
    expect(await operation(operationId)).toMatchObject({ state: "queued", approval_required: true, approved_at: null });
  };
  const hookStatus = async () => {
    const response = await page.request.get(panelPath(`/api/workloads/${id}/registry-webhook`));
    const text = await response.text();
    expect(response.status(), text).toBe(200);
    expect(text).not.toMatch(/"(token|secret)"\s*:/);
    expect(text).not.toContain("?secret=");
    return JSON.parse(text) as HookStatus;
  };
  const container = () => {
    const ids = docker("ps", "-q", "--filter", `label=com.docker.compose.project=cove-${id}`, "--filter", "label=com.docker.compose.service=web").split(/\s+/).filter(Boolean);
    expect(ids).toHaveLength(1);
    const [identity = "", configured = ""] = docker("inspect", "--format", "{{.Id}}|{{.Config.Image}}", ids[0] ?? "").split("|");
    return { id: identity, image: configured, version: docker("exec", identity, "sh", "-c", "cat /version 2>/dev/null || echo initial") };
  };

  const first = (await (await page.request.post(panelPath(`/api/workloads/${id}/operations`), { headers, data: { action: "deploy" } })).json()) as { id: string };
  expect((await finished(first.id)).state).toBe("succeeded");
  const initial = container();
  expect(initial).toMatchObject({ image: "busybox:1.37", version: "initial" });

  // A fixture-only registry connection; the local registry has no authentication, so these credentials are never checked.
  const connection = await page.request.post(panelPath("/api/registries"), { headers, data: { name: "Fixture local registry", registry: { server, username: "fixture", password: "fixture-unused" } } });
  expect([200, 201], await connection.text()).toContain(connection.status());
  // The saved Compose now names the registry tag, which does not exist until the push below. Nothing is deployed.
  const resource = (await (await page.request.get(panelPath(`/api/workloads/${id}`))).json()) as { name: string; node_id: string };
  const updated = await page.request.put(panelPath(`/api/workloads/${id}`), { headers, data: { name: resource.name, node_id: resource.node_id, kind: "service", compose: compose(image) } });
  expect([200, 202], await updated.text()).toContain(updated.status());
  const policy = await page.request.put(panelPath(`/api/projects/${project.id}/deployment-policy`), { headers, data: { approval: true, freeze_from: null, freeze_until: null, window_start: "", window_end: "" } });
  expect([200, 204], await policy.text()).toContain(policy.status());
  const deployCount = (await deploys()).length;

  // Selecting the registry and creating the hook from the Registry page.
  await page.goto(panelPath(`/resources/${id}/registry`));
  const credentials = page.getByRole("combobox", { name: "Registry credentials", exact: true });
  await expect(credentials).toBeEnabled({ timeout: 30_000 });
  await credentials.click();
  await page.getByRole("option", { name: "Fixture local registry", exact: true }).click();
  await page.getByRole("button", { name: "Save registry", exact: true }).click();
  await expect(page.getByText("Registry saved.")).toBeVisible();

  const section = page.getByRole("region", { name: "Registry webhook", exact: true });
  await expect(section.getByRole("combobox", { name: "Provider", exact: true })).toHaveText(/Distribution registry notifications/, { timeout: 30_000 });
  await section.getByLabel("Repository 1", { exact: true }).focus();
  await page.keyboard.type("team/app");
  await page.keyboard.press("Tab");
  await page.keyboard.type("stable");
  await section.getByRole("switch", { name: "Pull and redeploy on push", exact: true }).focus();
  await page.keyboard.press("Space");
  await capture(page, testInfo, "registry-webhook-form");
  await section.getByRole("button", { name: "Create webhook", exact: true }).focus();
  await page.keyboard.press("Enter");
  const confirm = page.getByRole("dialog", { name: "Turn on automatic pull and redeploy?" });
  await expect(confirm).toContainText("Approval, freeze windows, deploy windows and quotas still apply");
  const createdHook = page.waitForResponse((response) => new URL(response.url()).pathname === panelPath(`/api/workloads/${id}/registry-webhook`) && response.request().method() === "PUT");
  await confirm.getByRole("button", { name: "Turn on automatic deploys", exact: true }).click();
  const put = await createdHook;
  expect(put.status()).toBe(201);
  expect(put.request().postDataJSON()).toEqual({ provider: "distribution", enabled: true, filters: [{ repository: "team/app", tag: "stable" }] });
  const tokenField = section.getByLabel("Webhook token", { exact: true });
  await expect(tokenField).toBeVisible();
  const token = await tokenField.inputValue();
  expect(token.length).toBeGreaterThanOrEqual(32);
  expect(await page.evaluate((value) => (JSON.stringify({ ...localStorage }) + JSON.stringify({ ...sessionStorage }) + location.href).includes(value), token)).toBe(false);
  const saved = await hookStatus();
  expect(saved.configuration).toMatchObject({ provider: "distribution", enabled: true, filters: [{ repository: "team/app", tag: "stable" }], registry_server: server });
  expect(saved.configuration?.registry_id).toMatch(/^\S+$/);
  // The administrator who saved the webhook owns it, so automatic deploys are not suspended.
  expect(saved.ownership_required).toBe(false);
  const endpoint = saved.endpoint ?? "";
  const hookPath = new URL(endpoint).pathname;
  expect(hookPath.startsWith(panelPath("/hooks/registry/"))).toBe(true);
  expect(hookPath.slice(base.length)).toMatch(/^\/hooks\/registry\/[A-Za-z0-9_-]+$/);
  await capture(page, testInfo, "registry-webhook-created");
  await section.getByRole("button", { name: "Clear", exact: true }).click();
  await expect(tokenField).toHaveCount(0);

  const deliver = (body: unknown, bearer: string) => page.request.post(hookPath, { headers: { Authorization: `Bearer ${bearer}`, "Content-Type": "application/json" }, data: body });
  const event = (eventId: string) => ({ events: [{ id: eventId, timestamp: new Date().toISOString(), action: "push", target: { mediaType: "application/vnd.docker.distribution.manifest.v2+json", repository: "team/app", tag: "stable", digest: `sha256:${"0".repeat(64)}` } }] });

  // Controlled duplicate delivery while approval holds: one deploy, and the retried event ID does not rearm it.
  const controlled = `browser-dedupe-${Date.now()}`;
  const firstDelivery = await deliver(event(controlled), token);
  expect(firstDelivery.status(), await firstDelivery.text()).toBe(202);
  expect(await firstDelivery.json()).toEqual({ status: "accepted", new_events: 1, deploy_requested: true });
  await expect.poll(async () => (await deploys()).length, { timeout: 120_000 }).toBe(deployCount + 1);
  const held = (await deploys())[0];
  if (!held) throw new Error("The controlled delivery queued no deploy");
  await awaitingApproval(held.id);
  const retry = await deliver(event(controlled), token);
  expect(retry.status(), await retry.text()).toBe(202);
  expect(await retry.json()).toEqual({ status: "accepted", new_events: 0, deploy_requested: false });
  await page.waitForTimeout(20_000);
  expect((await deploys()).length).toBe(deployCount + 1);
  expect((await hookStatus()).pending_at).toBeNull();
  const cancelled = await page.request.post(panelPath(`/api/operations/${held.id}/cancel`), { headers });
  expect(cancelled.status(), await cancelled.text()).toBe(202);
  expect((await finished(held.id)).state).toBe("cancelled");
  expect(container()).toEqual(initial);

  // The native push: an owned registry on the fixture network notifies the panel directly with the Bearer token.
  // The configuration holds the token, so it reaches an owned volume through stdin only, never an argument or host file.
  const registryConfig = [
      "version: 0.1",
      "log:",
      "  level: warn",
      "storage:",
      "  filesystem:",
      "    rootdirectory: /var/lib/registry",
      "http:",
      "  addr: :5000",
      "notifications:",
      "  endpoints:",
      "    - name: cove",
      `      url: http://${panel}:8080${hookPath}`,
      "      headers:",
      `        Authorization: [Bearer ${token}]`,
      "      timeout: 5s",
      "      threshold: 3",
      "      backoff: 1s",
      "      ignore:",
      "        actions: [pull]",
      "",
    ].join("\n");
  owned.volume = `${owner}-hook-registry-config-${port}`;
  docker("volume", "create", "--label", `cove.browser-fixture=${owner}`, "--label", specLabel, owned.volume);
  execFileSync(
    "docker",
    ["run", "--rm", "-i", "--pull", "never", "--network", "none", "--cap-drop", "ALL", "--label", `cove.browser-fixture=${owner}`, "--label", specLabel, "--mount", `type=volume,source=${owned.volume},target=/config`, "busybox:1.37", "sh", "-ec", "cat > /config/config.yml; chmod 600 /config/config.yml"],
    { input: registryConfig, stdio: ["pipe", "ignore", "pipe"] },
  );
  // An empty client configuration for the push, so no host credentials are used.
  owned.directory = mkdtempSync(join(tmpdir(), "cove-registry-hooks-"));
  const dockerConfig = mkdtempSync(join(owned.directory, "client-"));
  writeFileSync(join(dockerConfig, "config.json"), "{}");
  owned.registry = `${owner}-hook-registry-${port}`;
  docker(
    "run", "-d", "--pull", "never", "--name", owned.registry,
    "--label", `cove.browser-fixture=${owner}`, "--label", specLabel,
    "--network", owner, "-p", `127.0.0.1:${port}:5000`,
    "--read-only", "--cap-drop", "ALL", "--security-opt", "no-new-privileges:true", "--memory", "256m", "--pids-limit", "128", "--cpus", "1",
    "--tmpfs", "/var/lib/registry:size=128m", "--tmpfs", "/tmp:size=16m",
    "--mount", `type=volume,source=${owned.volume},target=/etc/docker/registry,readonly`,
    registryImage,
  );
  await expect.poll(() => {
    try {
      return docker("exec", owned.registry, "wget", "-qO-", "http://127.0.0.1:5000/v2/");
    } catch {
      return "starting";
    }
  }, { timeout: 30_000 }).toBe("{}");

  // The candidate is made from the cached busybox image: no build and no download.
  owned.seed = `${owner}-hook-seed-${port}`;
  docker("run", "--name", owned.seed, "--pull", "never", "--network", "none", "--label", `cove.browser-fixture=${owner}`, "--label", specLabel, "busybox:1.37", "sh", "-c", "echo second > /version");
  owned.tag = image;
  docker("commit", owned.seed, image);
  docker("rm", owned.seed);
  owned.seed = "";
  const before = await hookStatus();
  docker("--config", dockerConfig, "push", image);

  await expect.poll(async () => (await deploys()).length, { timeout: 120_000 }).toBe(deployCount + 2);
  const pushed = (await deploys())[0];
  if (!pushed || pushed.id === held.id) throw new Error("The native push queued no new deploy");
  await awaitingApproval(pushed.id);
  // Webhook deploys pass the ordinary admission with an automatic, audited reason.
  expect((await operation(pushed.id)).reason).toBe("Registry webhook: matching image push");
  const afterPush = await hookStatus();
  expect(afterPush.last_matched_at).not.toBeNull();
  expect(afterPush.last_matched_at).not.toBe(before.last_matched_at);
  expect(afterPush.last_operation?.id).toBe(pushed.id);
  // Held for approval: the running release is untouched.
  await page.waitForTimeout(5_000);
  expect((await operation(pushed.id)).state).toBe("queued");
  expect(container()).toEqual(initial);

  await page.goto(panelPath(`/resources/${id}/registry`));
  await expect(section.getByText("View operation")).toBeVisible({ timeout: 30_000 });
  await capture(page, testInfo, "registry-webhook-pending-approval");
  await page.goto(panelPath(`/operations/${pushed.id}`));
  const approve = page.getByRole("button", { name: "Approve deployment", exact: true });
  await expect(approve).toBeEnabled({ timeout: 30_000 });
  await approve.focus();
  await page.keyboard.press("Enter");
  const deployed = await finished(pushed.id);
  expect(deployed.state, deployed.result?.error).toBe("succeeded");
  const next = container();
  expect(next.id).not.toBe(initial.id);
  expect(next).toMatchObject({ image, version: "second" });
  expect((await hookStatus()).last_operation).toMatchObject({ id: pushed.id, state: "succeeded" });

  // Rotation from the dashboard: the old token stops working, the new one is accepted, and an empty delivery deploys nothing.
  await page.goto(panelPath(`/resources/${id}/registry`));
  await section.getByRole("button", { name: "Rotate secret", exact: true }).click();
  const rotated = page.waitForResponse((response) => new URL(response.url()).pathname === panelPath(`/api/workloads/${id}/registry-webhook/rotate`) && response.request().method() === "POST");
  await page.getByRole("dialog", { name: "Rotate the webhook secret?" }).getByRole("button", { name: "Rotate secret", exact: true }).click();
  const rotation = await rotated;
  expect(rotation.status()).toBe(200);
  expect(rotation.request().postData()).toBeNull();
  const newToken = await section.getByLabel("Webhook token", { exact: true }).inputValue();
  expect(newToken).not.toBe(token);
  expect((await deliver({ events: [] }, token)).status()).toBe(401);
  const accepted = await deliver({ events: [] }, newToken);
  expect(accepted.status(), await accepted.text()).toBe(202);
  expect(await accepted.json()).toEqual({ status: "accepted", new_events: 0, deploy_requested: false });
  await page.waitForTimeout(5_000);
  expect((await deploys()).length).toBe(deployCount + 2);
  await section.getByRole("button", { name: "Clear", exact: true }).click();

  // Removal from the dashboard clears the saved hook and its metadata.
  await section.getByRole("button", { name: "Remove webhook", exact: true }).click();
  const removed = page.waitForResponse((response) => new URL(response.url()).pathname === panelPath(`/api/workloads/${id}/registry-webhook`) && response.request().method() === "DELETE");
  await page.getByRole("dialog", { name: "Remove the registry webhook?" }).getByRole("button", { name: "Remove webhook", exact: true }).click();
  expect((await removed).status()).toBe(200);
  await expect(section.getByText("Webhook removed", { exact: true })).toBeVisible();
  expect(await hookStatus()).toMatchObject({ configuration: null, endpoint: null, last_received_at: null, last_matched_at: null, pending_at: null });
  expect((await deliver({ events: [] }, newToken)).status()).toBe(401);
  await capture(page, testInfo, "registry-webhook-removed");
});

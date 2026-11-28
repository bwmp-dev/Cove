import { execFileSync } from "node:child_process";
import { mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { createServer } from "node:net";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { expect, test, type Page, type TestInfo } from "@playwright/test";
import { cleanupBrowserResources, expectNoHorizontalScroll } from "./route-tls";

// Runs only with scripts/test-browser.py --registry-webhooks, which supplies the verified, pinned Distribution
// registry image (never pulled). The spec runs its own anonymous local registry, publishes real tags made from the
// cached busybox image and drives the image update policy from the Registry page. Nothing is mocked or intercepted.
const password = process.env.COVE_BROWSER_PASSWORD ?? "";
const enabled = process.env.COVE_BROWSER_REGISTRY_HOOKS === "1";
// Set by the harness when the panel is served under a path prefix; panel pages, APIs and hooks live only under it.
const base = process.env.COVE_BROWSER_BASE_PATH ?? "";
const panelPath = (route: string) => `${base}${route}`;

const docker = (...args: string[]) => execFileSync("docker", args, { encoding: "utf8", stdio: ["ignore", "pipe", "pipe"] }).trim();
const specKey = "cove.image-updates-spec";
const specLabel = `${specKey}=1`;

// What this spec created on the Docker host. Every item also carries the fixture label, so the harness removes it
// even if this cleanup does not run.
const owned = { fixture: "", registry: "", seed: "", images: [] as string[], directory: "" };

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
  for (const name of [owned.registry, owned.seed]) if (name && ownedBySpec("container", name)) attempt(() => docker("rm", "-f", "-v", name));
  // Tags and pulled digests are removed without force, newest first, and only while they still carry both labels.
  for (const image of [...owned.images].reverse()) if (ownedBySpec("image", image)) attempt(() => docker("image", "rm", image));
  if (owned.directory) attempt(() => rmSync(owned.directory, { recursive: true, force: true }));
  Object.assign(owned, { fixture: "", registry: "", seed: "", images: [], directory: "" });
  // A failing test keeps its own error; cleanup problems are only raised on their own.
  if (problems.length > 0 && testInfo.status === testInfo.expectedStatus) throw new Error(`Cleanup failed: ${problems.join("; ")}`);
});

function fixtureOwner() {
  const owner = /^(cove-browser-[a-f0-9]{12})-worker$/.exec(process.env.COVE_BROWSER_WORKER ?? "")?.[1];
  const database = `${owner}-db`;
  if (!owner || process.env.COVE_BROWSER_DATABASE !== database) throw new Error("Isolated browser fixture required");
  return { owner, database };
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
  result:{ error?: string } | null;
}

interface PolicyStatus {
  configuration: { enabled: boolean; mode: string; rules: { service: string; range: string }[]; window_start: string; window_end: string } | null;
  worker_supported: boolean;
  node_ready: boolean;
  active_operation_id: string | null;
  last_check_id: string | null;
  last_operation_id: string | null;
  last_error: string;
  stale: boolean;
  updates: { state: string; message?: string; candidates: { service: string; current: string; target: string }[] } | null;
  retained_images: Record<string, { configured: string; image: string }>;
}

const compose = (image: string) =>
  [
    "services:",
    "  app:",
    `    image: ${image}`,
    "    init: true",
    "    restart: unless-stopped",
    "    command: [sh, -c, 'exec sleep 86400']",
    "    healthcheck:",
    "      test: [CMD, test, -f, /version]",
    "      interval: 2s",
    "      timeout: 2s",
    "      retries: 10",
    "",
  ].join("\n");

const utc = (date: Date) => date.toISOString().slice(11, 16);
const terminal = /^(succeeded|failed|cancelled|interrupted)$/;

const appliedMessage = "The retained automatic image versions are applied.";
const rollbackPause ="Automatic image updates were turned off by a rollback request. Turn the policy on and save to resume automatic updates.";

test("image updates: a notify policy finds an in-range tag and an approved automatic policy deploys and retains its exact digest", async ({ page }, testInfo) => {
  test.skip(!enabled, "Needs scripts/test-browser.py --registry-webhooks");
  test.setTimeout(900_000);
  const { owner, database } = fixtureOwner();
  owned.fixture = owner;
  const registryImage = process.env.COVE_BROWSER_REGISTRY_IMAGE ?? "";
  const registryPin = process.env.COVE_BROWSER_REGISTRY_PIN ?? "";
  if (!/^sha256:[a-f0-9]{64}$/.test(registryImage) || !/^registry@sha256:[a-f0-9]{64}$/.test(registryPin)) throw new Error("The harness must provide the verified local registry image");
  // Same identity rule as registry-webhooks.spec.ts: the exact pinned official build, never pulled.
  expect(docker("image", "inspect", "--format", "{{.Id}}", registryImage)).toBe(registryImage);
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
  const headers = { Origin: new URL(page.url()).origin };

  const nodes = (await (await page.request.get(panelPath("/api/nodes"))).json()) as { id: string; name: string; capabilities: { image_lifecycle?: boolean; image_updates?: boolean } | null }[];
  const node = nodes.find((item) => item.name === "ready-browser-worker");
  if (!node) throw new Error("The browser worker is missing");
  expect(node.capabilities?.image_lifecycle).toBe(true);
  expect(node.capabilities?.image_updates).toBe(true);

  // An anonymous registry on loopback, using the image's default configuration and tmpfs storage.
  const port = await freePort();
  const server = `127.0.0.1:${port}`;
  const repository = `${server}/team/app`;
  owned.registry = `${owner}-image-updates-registry-${port}`;
  docker(
    "run", "-d", "--pull", "never", "--name", owned.registry,
    "--label", `cove.browser-fixture=${owner}`, "--label", specLabel,
    "--network", owner, "-p", `127.0.0.1:${port}:5000`,
    "--read-only", "--cap-drop", "ALL", "--security-opt", "no-new-privileges:true", "--memory", "256m", "--pids-limit", "128", "--cpus", "1",
    "--tmpfs", "/var/lib/registry:size=128m", "--tmpfs", "/tmp:size=16m",
    registryImage,
  );
  await expect.poll(() => {
    try {
      return docker("exec", owned.registry, "wget", "-qO-", "http://127.0.0.1:5000/v2/");
    } catch {
      return "starting";
    }
  }, { timeout: 30_000 }).toBe("{}");

  // Pushes use an empty client configuration, so no host credentials are involved.
  owned.directory = mkdtempSync(join(tmpdir(), "cove-image-updates-"));
  const dockerConfig = mkdtempSync(join(owned.directory, "client-"));
  writeFileSync(join(dockerConfig, "config.json"), "{}");

  // Each published tag is committed from the cached busybox image with a marker; no build and no download.
  const publish = (tag: string, marker: string) => {
    owned.seed = `${owner}-image-updates-seed-${port}-${marker}`;
    docker("run", "--name", owned.seed, "--pull", "never", "--network", "none", "--label", `cove.browser-fixture=${owner}`, "--label", specLabel, "busybox:1.37", "sh", "-c", `echo ${marker} > /version`);
    const image = `${repository}:${tag}`;
    owned.images.push(image);
    docker("commit", "--change", `LABEL cove.browser-fixture=${owner}`, "--change", `LABEL ${specLabel}`, owned.seed, image);
    docker("rm", owned.seed);
    owned.seed = "";
    docker("--config", dockerConfig, "push", image);
    return image;
  };

  // The versions staging used: v1.10.0 is lower than v1.12.0 by SemVer although it sorts after it as text.
  // The initial release must pull the published v1.10.0, so the local tag is removed before the first deploy.
  const initialImage = publish("v1.10.0", "first");
  docker("image", "rm", initialImage);

  const project = (await (await page.request.post(panelPath("/api/projects"), { headers, data: { name: "Image update project" } })).json()) as { id: string };
  const created = await page.request.post(panelPath("/api/workloads"), { headers, data: { name: "image-updates", node_id: node.id, kind: "service", compose: compose(initialImage), project_id: project.id } });
  expect(created.status(), await created.text()).toBe(201);
  const { id } = (await created.json()) as { id: string };
  expect(id).toMatch(/^[a-f0-9]{32}$/);
  // Defer this resource's background checks until the policy is saved; saving schedules one immediately.
  expect(sql(`UPDATE workloads SET next_image_check_at=now()+interval '1 day' WHERE id='${id}' RETURNING id`).split(/\s+/).filter((value) => value === id)).toEqual([id]);

  const operations = async () => (await (await page.request.get(panelPath(`/api/operations?workload_id=${id}`))).json()) as Operation[];
  const deploys = async () => (await operations()).filter((item) => item.action === "deploy");
  const operation = async (operationId: string) => (await (await page.request.get(panelPath(`/api/operations/${operationId}`))).json()) as Operation;
  const finished = async (operationId: string, timeout = 240_000) => {
    await expect.poll(async () => (await operation(operationId)).state, { timeout }).toMatch(terminal);
    return operation(operationId);
  };
  const policyStatus = async () => {
    const response = await page.request.get(panelPath(`/api/workloads/${id}/image-update-policy`));
    const text = await response.text();
    expect(response.status(), text).toBe(200);
    expect(text).not.toMatch(/"(password|token|secret)"\s*:/);
    return JSON.parse(text) as PolicyStatus;
  };
  const container = () => {
    const ids = docker("ps", "-q", "--filter", `label=com.docker.compose.project=cove-${id}`, "--filter", "label=com.docker.compose.service=app").split(/\s+/).filter(Boolean);
    expect(ids).toHaveLength(1);
    const [identity = "", configured = "", imageId = ""] = docker("inspect", "--format", "{{.Id}}|{{.Config.Image}}|{{.Image}}", ids[0] ?? "").split("|");
    return { id: identity, image: configured, imageId, version: docker("exec", identity, "cat", "/version") };
  };
  // A failed, cancelled or interrupted image check stops the test with its sanitised error.
  const failIfCheckFailed = async () => {
    const failed = (await operations()).find((item) => item.action === "image_check" && /^(failed|cancelled|interrupted)$/.test(item.state));
    if (failed) throw new Error(`Image check ${failed.id} ${failed.state}: ${(await operation(failed.id)).result?.error ?? "no error recorded"}`);
  };

  const first = (await (await page.request.post(panelPath(`/api/workloads/${id}/operations`), { headers, data: { action: "deploy" } })).json()) as { id: string };
  const firstResult = await finished(first.id);
  expect(firstResult.state, firstResult.result?.error).toBe("succeeded");
  const initial = container();
  expect(initial).toMatchObject({ image: initialImage, version: "first" });
  // The release was pulled from the registry, so its image keeps a registry digest.
  expect(docker("image", "inspect", "--format", "{{range .RepoDigests}}{{.}} {{end}}", initial.imageId)).toContain(`${repository}@sha256:`);
  const deployCount = (await deploys()).length;

  const policy = page.getByRole("region", { name: "Image update policy", exact: true });
  const images = page.getByRole("region", { name: "Images", exact: true });
  const savePolicy = async (button: string, expected: unknown, confirm?: string) => {
    const put = page.waitForResponse((response) => new URL(response.url()).pathname === panelPath(`/api/workloads/${id}/image-update-policy`) && response.request().method() === "PUT");
    await policy.getByRole("button", { name: button, exact: true }).focus();
    await page.keyboard.press("Enter");
    if (confirm) {
      const dialog = page.getByRole("dialog", { name: "Turn on automatic image updates?" });
      await expect(dialog).toContainText("Saved Compose and image tags are not changed");
      await dialog.getByRole("button", { name: confirm, exact: true }).focus();
      await page.keyboard.press("Enter");
    } else {
      await expect(page.getByRole("dialog")).toHaveCount(0);
    }
    const response = await put;
    expect(response.status(), await response.text()).toBe(200);
    expect(response.request().postDataJSON()).toEqual(expected);
  };
  // Waits for a fresh result, requesting a check through the Images section only when none is already running.
  const discover = async (accept: (status: PolicyStatus) => boolean, timeout = 300_000) => {
    let requested = false;
    await expect.poll(async () => {
      await failIfCheckFailed();
      const status = await policyStatus();
      if (status.last_check_id && !status.stale && status.updates && accept(status)) return "ready";
      if (!requested && !status.active_operation_id) {
        requested = true;
        await page.goto(panelPath(`/resources/${id}/registry`));
        const check = images.getByRole("button", { name: "Check images", exact: true });
        await expect(check).toBeEnabled({ timeout: 30_000 });
        const queued = page.waitForResponse((response) => new URL(response.url()).pathname === panelPath(`/api/workloads/${id}/operations`) && response.request().method() === "POST");
        await check.click();
        // A scheduled check may have started in between; the panel then refuses a second one.
        expect([201, 202, 409]).toContain((await queued).status());
      }
      return status.updates?.state ?? "waiting";
    }, { timeout, intervals: [3_000] }).toBe("ready");
    return policyStatus();
  };

  // 1. An enabled notify policy for app with ^1.10.0, created with the keyboard from the Registry page.
  await page.goto(panelPath(`/resources/${id}/registry`));
  await expect(policy.getByRole("button", { name: "Create policy", exact: true })).toBeVisible({ timeout: 30_000 });
  await policy.getByRole("switch", { name: "Enable image update policy", exact: true }).focus();
  await page.keyboard.press("Space");
  await expect(policy.getByRole("combobox", { name: "When an update is found", exact: true })).toHaveText(/Notify only/);
  await policy.getByRole("combobox", { name: "Service 1", exact: true }).focus();
  await page.keyboard.press("Enter");
  await page.getByRole("option", { name: "app", exact: true }).click();
  await policy.getByLabel("Version range 1", { exact: true }).focus();
  await page.keyboard.type("^1.10.0");
  await capture(page, testInfo, "image-updates-form");
  await savePolicy("Create policy", { enabled: true, mode: "notify", rules: [{ service: "app", range: "^1.10.0" }], window_start: "", window_end: "" });
  await expect(policy.getByText("Policy created", { exact: true })).toBeVisible();

  publish("v1.12.0", "second");
  publish("v2.0.0", "forbidden-major");

  // 2. Notify finds v1.12.0, never v2.0.0, and leaves the running container alone.
  const notified = await discover((status) => status.updates?.state === "available");
  const candidate = notified.updates?.candidates.find((item) => item.service === "app");
  expect(candidate?.target).toMatch(new RegExp(`^${repository.replace(/[.]/g, "\\.")}:v1\\.12\\.0@sha256:[a-f0-9]{64}$`));
  const target = candidate?.target ?? "";
  const targetDigest = target.split("@")[1] ?? "";
  await page.goto(panelPath(`/resources/${id}/registry`));
  await expect(policy.getByRole("list", { name: "Update candidates", exact: true })).toContainText(`v1.12.0@${targetDigest}`, { timeout: 30_000 });
  await expect(policy.getByText("Update available", { exact: true })).toBeVisible();
  await page.waitForTimeout(20_000);
  expect((await deploys()).length).toBe(deployCount);
  expect(container()).toEqual(initial);
  await capture(page, testInfo, "image-updates-notify");

  // 3. Project approval, then automatic mode with a window that is closed now.
  const approval = await page.request.put(panelPath(`/api/projects/${project.id}/deployment-policy`), { headers, data: { approval: true, freeze_from: null, freeze_until: null, window_start: "", window_end: "" } });
  expect([200, 204], await approval.text()).toContain(approval.status());
  const now = Date.now();
  const closedStart = utc(new Date(now + 60 * 60_000));
  const closedEnd = utc(new Date(now + 120 * 60_000));
  await policy.getByRole("combobox", { name: "When an update is found", exact: true }).click();
  await page.getByRole("option", { name: "Deploy automatically", exact: true }).click();
  await policy.getByLabel("Starts", { exact: true }).fill(closedStart);
  await policy.getByLabel("Ends", { exact: true }).fill(closedEnd);
  await savePolicy("Save policy", { enabled: true, mode: "auto", rules: [{ service: "app", range: "^1.10.0" }], window_start: closedStart, window_end: closedEnd }, "Turn on automatic updates");
  await discover((status) => status.updates?.state === "available");
  await expect.poll(async () => (await policyStatus()).last_error, { timeout: 180_000 }).toMatch(/maintenance window/i);
  expect((await deploys()).length).toBe(deployCount);
  expect(container()).toEqual(initial);
  await page.goto(panelPath(`/resources/${id}/registry`));
  await expect(policy.getByText(/Waiting for the image update UTC maintenance window/)).toBeVisible({ timeout: 30_000 });

  // 4. Clearing the window resets the policy and queues one ordinary deploy, held for approval.
  await policy.getByLabel("Starts", { exact: true }).fill("");
  await policy.getByLabel("Ends", { exact: true }).fill("");
  await savePolicy("Save policy", { enabled: true, mode: "auto", rules: [{ service: "app", range: "^1.10.0" }], window_start: "", window_end: "" });
  await expect.poll(async () => {
    await failIfCheckFailed();
    return (await deploys()).length;
  }, { timeout: 300_000, intervals: [3_000] }).toBe(deployCount + 1);
  const automatic = (await deploys())[0];
  if (!automatic) throw new Error("The automatic policy queued no deploy");
  await expect.poll(async () => (await operation(automatic.id)).approval_required === true, { timeout: 120_000 }).toBe(true);
  expect(await operation(automatic.id)).toMatchObject({ state: "queued", approval_required: true, approved_at: null });
  // The policy recorded this deploy as its own, with the (empty) window it was admitted under.
  expect(sql(`SELECT window_start||'|'||window_end FROM image_update_deployments WHERE operation_id='${automatic.id}'`)).toBe("|");
  expect((await policyStatus()).last_operation_id).toBe(automatic.id);
  expect(container()).toEqual(initial);
  await page.goto(panelPath(`/resources/${id}/registry`));
  await expect(policy.getByText("Queued by this policy", { exact: true })).toBeVisible({ timeout: 30_000 });
  await capture(page, testInfo, "image-updates-approval");

  await page.goto(panelPath(`/operations/${automatic.id}`));
  const approve = page.getByRole("button", { name: "Approve deployment", exact: true });
  await expect(approve).toBeEnabled({ timeout: 30_000 });
  await approve.focus();
  await page.keyboard.press("Enter");
  const deployed = await finished(automatic.id);
  expect(deployed.state, deployed.result?.error).toBe("succeeded");
  const updated = container();
  expect(updated.id).not.toBe(initial.id);
  expect(updated.version).toBe("second");
  // The deploy ran the exact pinned candidate, not a re-pulled tag: the running image ID matches an independent inspect
  // of the candidate reference, which an ordinary deploy also keeps as Config.Image. The saved Compose still names v1.10.0.
  owned.images.push(`${repository}@${targetDigest}`);
  expect(updated.imageId).toBe(docker("image", "inspect", "--format", "{{.Id}}", target));
  expect(updated.image).toBe(target);
  const resource = (await (await page.request.get(panelPath(`/api/workloads/${id}`))).json()) as { compose: string };
  expect(resource.compose).toContain(`image: ${initialImage}`);

  // 5. A later check finds nothing newer in range and queues no second deploy. The fresh result says the applied
  // target is current and no longer offers it as a candidate.
  const appliedResult = await discover((status) => status.updates?.state === "current");
  expect(appliedResult.stale).toBe(false);
  expect(appliedResult.updates).toMatchObject({ state: "current", message: appliedMessage });
  expect(appliedResult.updates?.candidates.map((item) => item.target)).not.toContain(target);
  await page.waitForTimeout(20_000);
  expect((await deploys()).length).toBe(deployCount + 1);
  expect(container()).toEqual(updated);
  await page.goto(panelPath(`/resources/${id}/registry`));
  await expect(policy.getByText("Current", { exact: true })).toBeVisible({ timeout: 30_000 });
  await expect(policy.getByText(appliedMessage, { exact: true })).toBeVisible();
  await expect(policy.getByText("Update available", { exact: true })).toHaveCount(0);
  await expect(policy.getByRole("list", { name: "Update candidates", exact: true })).toHaveCount(0);
  await capture(page, testInfo, "image-updates-updated");

  // 6. The applied digest is retained for app while the policy stays on and automatic.
  expect((await policyStatus()).retained_images).toEqual({ app: { configured: initialImage, image: target } });
  await expect(policy.getByRole("list", { name: "Retained images", exact: true })).toContainText(target);
  await capture(page, testInfo, "image-updates-retained");

  const queue = async (data: Record<string, unknown>) => {
    const response = await page.request.post(panelPath(`/api/workloads/${id}/operations`), { headers, data });
    expect(response.ok(), await response.text()).toBe(true);
    return ((await response.json()) as { id: string }).id;
  };
  // Later deploys are approved through the API the operation page uses; the page's own flow is covered above.
  const approveAndFinish = async (operationId: string) => {
    await expect.poll(async () => (await operation(operationId)).approval_required === true, { timeout: 120_000 }).toBe(true);
    const approved = await page.request.post(panelPath(`/api/operations/${operationId}/approve`), { headers, data: {} });
    expect(approved.ok(), await approved.text()).toBe(true);
    const result = await finished(operationId);
    expect(result.state, result.result?.error).toBe("succeeded");
  };
  const settled = () => expect.poll(async () => (await policyStatus()).active_operation_id, { timeout: 300_000, intervals: [3_000] }).toBeNull();
  const runsApplied = () => {
    const running = container();
    expect(running).toMatchObject({ image: target, imageId: updated.imageId, version: "second" });
    return running;
  };
  const retained = { app: { configured: initialImage, image: target } };

  // 7. An ordinary manual deploy keeps the exact applied digest; the saved Compose keeps the base tag.
  await approveAndFinish(await queue({ action: "deploy" }));
  runsApplied();
  expect(((await (await page.request.get(panelPath(`/api/workloads/${id}`))).json()) as { compose: string }).compose).toContain(`image: ${initialImage}`);
  expect((await policyStatus()).retained_images).toEqual(retained);

  // 8. An ordinary restart keeps running the applied image, not the configured tag.
  const restarted = await finished(await queue({ action: "restart" }));
  expect(restarted.state, restarted.result?.error).toBe("succeeded");
  runsApplied();

  // 9. A matching registry push of the configured tag only requests a check while the policy is on; it never downgrades.
  const registry = await page.request.post(panelPath("/api/registries"), { headers, data: { name: "Image update fixture registry", registry: { server, username: "fixture", password: "fixture-unused" } } });
  expect([200, 201], await registry.text()).toContain(registry.status());
  const selected = await page.request.put(panelPath(`/api/workloads/${id}/registry`), { headers, data: { registry_id: ((await registry.json()) as { id: string }).id } });
  expect(selected.ok(), await selected.text()).toBe(true);
  const hook = await page.request.put(panelPath(`/api/workloads/${id}/registry-webhook`), { headers, data: { provider: "distribution", enabled: true, filters: [{ repository: "team/app", tag: "v1.10.0" }] } });
  expect(hook.status(), await hook.text()).toBe(201);
  const { endpoint, token } = (await hook.json()) as { endpoint: string; token: string };
  const hookPath = new URL(endpoint).pathname;
  expect(hookPath.startsWith(panelPath("/hooks/registry/"))).toBe(true);
  const baseDigest = docker("image", "inspect", "--format", "{{range .RepoDigests}}{{.}} {{end}}", initial.imageId).split(/\s+/).find((digest) => digest.startsWith(`${repository}@`))?.split("@")[1];
  if (!baseDigest) throw new Error("The base release has no registry digest");
  const hookStatus = async () => {
    const response = await page.request.get(panelPath(`/api/workloads/${id}/registry-webhook`));
    const text = await response.text();
    expect(response.status(), text).toBe(200);
    return JSON.parse(text) as { pending_at: string | null; last_ignored_reason: string };
  };
  await settled();
  const beforePush = (await deploys()).length;
  const checkBeforePush = (await policyStatus()).last_check_id;
  const pushed = await page.request.post(hookPath, {
    headers: { Authorization: `Bearer ${token}`, "Content-Type": "application/json" },
    data: { events: [{ id: `image-updates-${Date.now()}`, timestamp: new Date().toISOString(), action: "push", target: { mediaType: "application/vnd.docker.distribution.manifest.v2+json", repository: "team/app", tag: "v1.10.0", digest: baseDigest } }] },
  });
  expect(pushed.status(), await pushed.text()).toBe(202);
  expect(await pushed.json()).toMatchObject({ status: "accepted", new_events: 1 });
  // The dispatcher hands the push to the policy, which then runs its own fresh check; both must be observed.
  await expect.poll(async () => {
    await failIfCheckFailed();
    const pushState = await hookStatus();
    const status = await policyStatus();
    return (
      pushState.pending_at === null &&
      pushState.last_ignored_reason.includes("Image update policy requested a fresh check") &&
      status.last_check_id !== null &&
      status.last_check_id !== checkBeforePush &&
      !status.stale &&
      status.updates?.state === "current"
    );
  }, { timeout: 300_000, intervals: [3_000] }).toBe(true);
  await settled();
  expect((await deploys()).length).toBe(beforePush);
  runsApplied();
  expect((await policyStatus()).retained_images).toEqual(retained);

  // 10. Saving a different image for app clears its retention, and the next deploy runs the saved tag.
  const savedTag = `${repository}:v1.12.0`;
  const current = (await (await page.request.get(panelPath(`/api/workloads/${id}`))).json()) as { name: string; node_id: string };
  const changed = await page.request.put(panelPath(`/api/workloads/${id}`), { headers, data: { name: current.name, node_id: current.node_id, kind: "service", compose: compose(savedTag) } });
  expect([200, 202], await changed.text()).toContain(changed.status());
  expect((await policyStatus()).retained_images).toEqual({});
  await settled();
  await approveAndFinish(await queue({ action: "deploy" }));
  expect(container()).toMatchObject({ image: savedTag, version: "second" });

  // One explicit Check images after the saved tag changed, with no further policy save: its own fresh result is bound
  // to the current configuration and no longer offers the applied target as available.
  await settled();
  const checkBeforeManual = (await policyStatus()).last_check_id;
  const deploysBeforeManual = (await deploys()).length;
  await page.goto(panelPath(`/resources/${id}/registry`));
  const manualCheck = images.getByRole("button", { name: "Check images", exact: true });
  await expect(manualCheck).toBeEnabled({ timeout: 30_000 });
  const policyPath = panelPath(`/api/workloads/${id}/image-update-policy`);
  const operationsPath = panelPath(`/api/workloads/${id}/operations`);
  const sent: string[] = [];
  const record = (request: { method: () => string; url: () => string }) => {
    const pathname = new URL(request.url()).pathname;
    if ((pathname === operationsPath && request.method() === "POST") || (pathname === policyPath && request.method() !== "GET")) sent.push(`${request.method()} ${pathname}`);
  };
  page.on("request", record);
  const manualQueued = page.waitForResponse((response) => new URL(response.url()).pathname === operationsPath && response.request().method() === "POST");
  await manualCheck.click();
  const manualResponse = await manualQueued;
  expect([201, 202], await manualResponse.text()).toContain(manualResponse.status());
  expect(manualResponse.request().postDataJSON()).toEqual({ action: "image_check" });
  const manualId = ((await manualResponse.json()) as { id: string }).id;
  const manualResult = await finished(manualId);
  expect(manualResult.state, manualResult.result?.error).toBe("succeeded");
  await expect.poll(async () => {
    const status = await policyStatus();
    return status.active_operation_id === null && status.last_check_id === manualId && !status.stale;
  }, { timeout: 120_000, intervals: [3_000] }).toBe(true);
  page.off("request", record);
  expect(sent).toEqual([`POST ${operationsPath}`]);
  const afterManual = await policyStatus();
  expect(afterManual.last_check_id).not.toBe(checkBeforeManual);
  expect(afterManual.configuration).toEqual({ enabled: true, mode: "auto", rules: [{ service: "app", range: "^1.10.0" }], window_start: "", window_end: "" });
  expect(afterManual.updates?.state).toBe("current");
  expect(afterManual.updates?.candidates.map((item) => item.target)).not.toContain(target);
  expect((await deploys()).length).toBe(deploysBeforeManual);
  expect(container()).toMatchObject({ image: savedTag, version: "second" });
  await page.goto(panelPath(`/resources/${id}/registry`));
  await expect(policy.getByText("Current", { exact: true })).toBeVisible({ timeout: 30_000 });
  await expect(policy.getByText("Result is from before the latest change", { exact: true })).toHaveCount(0);
  await expect(policy.getByText("Update available", { exact: true })).toHaveCount(0);

  // 11. Requesting a rollback turns the policy off at once, before approval, and says how to resume.
  await page.goto(panelPath(`/operations/${automatic.id}`));
  const rollback = page.getByRole("button", { name: "Roll back to this release", exact: true });
  await expect(rollback).toBeEnabled({ timeout: 30_000 });
  await rollback.click();
  const rollbackDialog = page.getByRole("dialog", { name: "Roll back to this release?" });
  await expect(rollbackDialog).toContainText("Requesting a rollback turns off this resource's image update policy if it is set to deploy automatically");
  await rollbackDialog.getByLabel(/Reason/).fill("Image update retention check");
  const rollbackRequest = page.waitForResponse((response) => new URL(response.url()).pathname === panelPath(`/api/workloads/${id}/operations`) && response.request().method() === "POST");
  await rollbackDialog.getByRole("button", { name: "Roll back", exact: true }).click();
  const rollbackResponse = await rollbackRequest;
  expect(rollbackResponse.ok(), await rollbackResponse.text()).toBe(true);
  const rollbackId = ((await rollbackResponse.json()) as { id: string }).id;
  await expect(page).toHaveURL(new RegExp(`/operations/${rollbackId}$`));
  const paused = await policyStatus();
  expect(paused.configuration?.enabled).toBe(false);
  expect(paused.retained_images).toEqual({});
  expect(paused.last_error).toBe(rollbackPause);
  expect(await operation(rollbackId)).toMatchObject({ action: "rollback", state: "queued" });
  await approveAndFinish(rollbackId);
  const rollbackReason = (await operation(rollbackId)).reason ?? "";
  expect(rollbackReason).toContain("Image update retention check");
  expect(rollbackReason).toContain("Rollback paused automatic image updates.");
  // Rollback recreates the release from its immutable snapshot, which names the local image ID, not the registry reference.
  expect(container()).toMatchObject({ image: updated.imageId, imageId: updated.imageId, version: "second" });
  expect((await policyStatus()).configuration?.enabled).toBe(false);

  // 12. One explicit save turns the policy back on.
  await settled();
  await page.goto(panelPath(`/resources/${id}/registry`));
  await expect(policy.getByText(rollbackPause, { exact: true })).toBeVisible({ timeout: 30_000 });
  await policy.getByRole("switch", { name: "Enable image update policy", exact: true }).focus();
  await page.keyboard.press("Space");
  await savePolicy("Save policy", { enabled: true, mode: "auto", rules: [{ service: "app", range: "^1.10.0" }], window_start: "", window_end: "" }, "Turn on automatic updates");
  expect((await policyStatus()).configuration?.enabled).toBe(true);
  await settled();
  await page.goto(panelPath(`/resources/${id}/registry`));
  await expect(policy.getByRole("button", { name: "Save policy", exact: true })).toBeVisible({ timeout: 30_000 });

  await policy.getByRole("switch", { name: "Enable image update policy", exact: true }).focus();
  await page.keyboard.press("Space");
  await savePolicy("Save policy", { enabled: false, mode: "auto", rules: [{ service: "app", range: "^1.10.0" }], window_start: "", window_end: "" });
  expect((await policyStatus()).configuration?.enabled).toBe(false);
  await capture(page, testInfo, "image-updates-disabled");
});

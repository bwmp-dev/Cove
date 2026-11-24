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
  result: { error?: string } | null;
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

test("image updates: a notify policy finds an in-range tag and an approved automatic policy deploys its exact digest", async ({ page }, testInfo) => {
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

  await page.goto("/projects");
  await expect(page).toHaveURL(/\/login(?:[?#]|$)/);
  await page.getByLabel("Password", { exact: true }).fill(password);
  await page.getByRole("button", { name: "Sign in", exact: true }).click();
  await expect(page).not.toHaveURL(/\/login(?:[?#]|$)/);
  const headers = { Origin: new URL(page.url()).origin };

  const nodes = (await (await page.request.get("/api/nodes")).json()) as { id: string; name: string; capabilities: { image_lifecycle?: boolean; image_updates?: boolean } | null }[];
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

  // The initial release must pull the published 1.0.0, so the local tag is removed before the first deploy.
  const initialImage = publish("1.0.0", "first");
  docker("image", "rm", initialImage);

  const project = (await (await page.request.post("/api/projects", { headers, data: { name: "Image update project" } })).json()) as { id: string };
  const created = await page.request.post("/api/workloads", { headers, data: { name: "image-updates", node_id: node.id, kind: "service", compose: compose(initialImage), project_id: project.id } });
  expect(created.status(), await created.text()).toBe(201);
  const { id } = (await created.json()) as { id: string };
  expect(id).toMatch(/^[a-f0-9]{32}$/);
  // Defer this resource's background checks until the policy is saved; saving schedules one immediately.
  expect(sql(`UPDATE workloads SET next_image_check_at=now()+interval '1 day' WHERE id='${id}' RETURNING id`).split(/\s+/).filter((value) => value === id)).toEqual([id]);

  const operations = async () => (await (await page.request.get(`/api/operations?workload_id=${id}`)).json()) as Operation[];
  const deploys = async () => (await operations()).filter((item) => item.action === "deploy");
  const operation = async (operationId: string) => (await (await page.request.get(`/api/operations/${operationId}`)).json()) as Operation;
  const finished = async (operationId: string, timeout = 240_000) => {
    await expect.poll(async () => (await operation(operationId)).state, { timeout }).toMatch(terminal);
    return operation(operationId);
  };
  const policyStatus = async () => {
    const response = await page.request.get(`/api/workloads/${id}/image-update-policy`);
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

  const first = (await (await page.request.post(`/api/workloads/${id}/operations`, { headers, data: { action: "deploy" } })).json()) as { id: string };
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
    const put = page.waitForResponse((response) => new URL(response.url()).pathname === `/api/workloads/${id}/image-update-policy` && response.request().method() === "PUT");
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
        await page.goto(`/resources/${id}/registry`);
        const check = images.getByRole("button", { name: "Check images", exact: true });
        await expect(check).toBeEnabled({ timeout: 30_000 });
        const queued = page.waitForResponse((response) => new URL(response.url()).pathname === `/api/workloads/${id}/operations` && response.request().method() === "POST");
        await check.click();
        // A scheduled check may have started in between; the panel then refuses a second one.
        expect([201, 202, 409]).toContain((await queued).status());
      }
      return status.updates?.state ?? "waiting";
    }, { timeout, intervals: [3_000] }).toBe("ready");
    return policyStatus();
  };

  // 1. An enabled notify policy for app with ^1.0.0, created with the keyboard from the Registry page.
  await page.goto(`/resources/${id}/registry`);
  await expect(policy.getByRole("button", { name: "Create policy", exact: true })).toBeVisible({ timeout: 30_000 });
  await policy.getByRole("switch", { name: "Enable image update policy", exact: true }).focus();
  await page.keyboard.press("Space");
  await expect(policy.getByRole("combobox", { name: "When an update is found", exact: true })).toHaveText(/Notify only/);
  await policy.getByRole("combobox", { name: "Service 1", exact: true }).focus();
  await page.keyboard.press("Enter");
  await page.getByRole("option", { name: "app", exact: true }).click();
  await policy.getByLabel("Version range 1", { exact: true }).focus();
  await page.keyboard.type("^1.0.0");
  await capture(page, testInfo, "image-updates-form");
  await savePolicy("Create policy", { enabled: true, mode: "notify", rules: [{ service: "app", range: "^1.0.0" }], window_start: "", window_end: "" });
  await expect(policy.getByText("Policy created", { exact: true })).toBeVisible();

  publish("1.1.0", "second");
  publish("2.0.0", "forbidden-major");

  // 2. Notify finds 1.1.0, never 2.0.0, and leaves the running container alone.
  const notified = await discover((status) => status.updates?.state === "available");
  const candidate = notified.updates?.candidates.find((item) => item.service === "app");
  expect(candidate?.target).toMatch(new RegExp(`^${repository.replace(/[.]/g, "\\.")}:1\\.1\\.0@sha256:[a-f0-9]{64}$`));
  const target = candidate?.target ?? "";
  const targetDigest = target.split("@")[1] ?? "";
  await page.goto(`/resources/${id}/registry`);
  await expect(policy.getByRole("list", { name: "Update candidates", exact: true })).toContainText(`1.1.0@${targetDigest}`, { timeout: 30_000 });
  await expect(policy.getByText("Update available", { exact: true })).toBeVisible();
  await page.waitForTimeout(20_000);
  expect((await deploys()).length).toBe(deployCount);
  expect(container()).toEqual(initial);
  await capture(page, testInfo, "image-updates-notify");

  // 3. Project approval, then automatic mode with a window that is closed now.
  const approval = await page.request.put(`/api/projects/${project.id}/deployment-policy`, { headers, data: { approval: true, freeze_from: null, freeze_until: null, window_start: "", window_end: "" } });
  expect([200, 204], await approval.text()).toContain(approval.status());
  const now = Date.now();
  const closedStart = utc(new Date(now + 60 * 60_000));
  const closedEnd = utc(new Date(now + 120 * 60_000));
  await policy.getByRole("combobox", { name: "When an update is found", exact: true }).click();
  await page.getByRole("option", { name: "Deploy automatically", exact: true }).click();
  await policy.getByLabel("Starts", { exact: true }).fill(closedStart);
  await policy.getByLabel("Ends", { exact: true }).fill(closedEnd);
  await savePolicy("Save policy", { enabled: true, mode: "auto", rules: [{ service: "app", range: "^1.0.0" }], window_start: closedStart, window_end: closedEnd }, "Turn on automatic updates");
  await discover((status) => status.updates?.state === "available");
  await expect.poll(async () => (await policyStatus()).last_error, { timeout: 180_000 }).toMatch(/maintenance window/i);
  expect((await deploys()).length).toBe(deployCount);
  expect(container()).toEqual(initial);
  await page.goto(`/resources/${id}/registry`);
  await expect(policy.getByText(/Waiting for the image update UTC maintenance window/)).toBeVisible({ timeout: 30_000 });

  // 4. Clearing the window resets the policy and queues one ordinary deploy, held for approval.
  await policy.getByLabel("Starts", { exact: true }).fill("");
  await policy.getByLabel("Ends", { exact: true }).fill("");
  await savePolicy("Save policy", { enabled: true, mode: "auto", rules: [{ service: "app", range: "^1.0.0" }], window_start: "", window_end: "" });
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
  await page.goto(`/resources/${id}/registry`);
  await expect(policy.getByText("Queued by this policy", { exact: true })).toBeVisible({ timeout: 30_000 });
  await capture(page, testInfo, "image-updates-approval");

  await page.goto(`/operations/${automatic.id}`);
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
  // of the candidate reference, which an ordinary deploy also keeps as Config.Image. The saved Compose still names 1.0.0.
  owned.images.push(`${repository}@${targetDigest}`);
  expect(updated.imageId).toBe(docker("image", "inspect", "--format", "{{.Id}}", target));
  expect(updated.image).toBe(target);
  const resource = (await (await page.request.get(`/api/workloads/${id}`)).json()) as { compose: string };
  expect(resource.compose).toContain(`image: ${initialImage}`);

  // 5. A later check finds nothing newer in range and queues no second deploy.
  await discover((status) => status.updates?.state === "current");
  await page.waitForTimeout(20_000);
  expect((await deploys()).length).toBe(deployCount + 1);
  expect(container()).toEqual(updated);
  await page.goto(`/resources/${id}/registry`);
  await expect(policy.getByText("Current", { exact: true })).toBeVisible({ timeout: 30_000 });
  await capture(page, testInfo, "image-updates-updated");

  await policy.getByRole("switch", { name: "Enable image update policy", exact: true }).focus();
  await page.keyboard.press("Space");
  await savePolicy("Save policy", { enabled: false, mode: "auto", rules: [{ service: "app", range: "^1.0.0" }], window_start: "", window_end: "" });
  expect((await policyStatus()).configuration?.enabled).toBe(false);
  await capture(page, testInfo, "image-updates-disabled");
});

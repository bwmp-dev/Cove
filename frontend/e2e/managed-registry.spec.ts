import { execFileSync } from "node:child_process";
import { randomBytes } from "node:crypto";
import { readFileSync } from "node:fs";
import { isAbsolute } from "node:path";
import { expect, test, type Locator, type Page, type Response, type Route, type TestInfo } from "@playwright/test";
import { cleanupBrowserResources, expectNoHorizontalScroll, verified } from "./route-tls";

// Runs only with scripts/test-browser.py --managed-registry on the caller's owned nested daemon. The fixture trusts a
// disposable CA for one exact .example.test hostname inside that daemon only. Provisioning starts on ACME against a
// closed local authority, so authenticated TLS verification really fails until the fixture leaf is uploaded. Apart from
// the one UI-only garbage collection refusal marked below, no request in this spec is mocked, and TLS verification is
// never skipped.
const password = process.env.COVE_BROWSER_PASSWORD ?? "";
const enabled = process.env.COVE_BROWSER_MANAGED_REGISTRY === "1";
const base = process.env.COVE_BROWSER_BASE_PATH ?? "";
const panelPath = (route: string) => `${base}${route}`;

test.use({ actionTimeout: 30_000, navigationTimeout: 60_000 });

const docker = (...args: string[]) => execFileSync("docker", args, { encoding: "utf8", stdio: ["ignore", "pipe", "pipe"] }).trim();
const sizes = [320, 390, 768, 1280, 1440, 1920];
// The proxy's default certificate is a transport/TLS failure, which Cove keeps retrying while the registry shows as failed.
const tlsFailure = "Registry deployment finished, but authenticated HTTPS verification is not ready. Cove will retry automatically (up to 15 minutes between attempts); check DNS, certificate issuance and the selected trust certificate if this persists.";
const finished = /^(succeeded|failed|cancelled|interrupted)$/;
const resourceId = /^[a-f0-9]{32}$/;

async function capture(page: Page, testInfo: TestInfo, name: string) {
  for (const width of sizes) {
    await page.setViewportSize({ width, height: 900 });
    await expectNoHorizontalScroll(page);
    await page.screenshot({ path: testInfo.outputPath(`${name}-${width}.png`), fullPage: true });
  }
  await page.setViewportSize({ width: 1280, height: 900 });
}

// The refusal's named resource, node and reason must be fully visible inside the modal, not only present in the DOM.
async function expectRefusalVisible(refusal: Locator, reason: string) {
  for (const text of ["Legacy service", "Ready runner", reason]) await expect(refusal.getByText(text, { exact: true })).toBeInViewport({ ratio: 1 });
}

// Each width is checked as if the dialog had just been opened there, so the modal is returned to its top first.
async function captureRefusal(page: Page, testInfo: TestInfo, dialog: Locator, refusal: Locator, reason: string) {
  for (const width of sizes) {
    await page.setViewportSize({ width, height: 900 });
    await dialog.evaluate((element) => element.scrollTo({ top: 0, behavior: "instant" }));
    await expectRefusalVisible(refusal, reason);
    await expectNoHorizontalScroll(page);
    await page.screenshot({ path: testInfo.outputPath(`managed-registry-gc-inventory-incomplete-${width}.png`), fullPage: true });
  }
  await page.setViewportSize({ width: 1280, height: 900 });
}

// The fixture's name, inferred from the worker the harness exports and checked against its database.
function fixture() {
  const owner = /^cove-browser-([a-f0-9]{12})-worker$/.exec(process.env.COVE_BROWSER_WORKER ?? "")?.[1];
  const database = `cove-browser-${owner}-db`;
  if (!owner || process.env.COVE_BROWSER_DATABASE !== database) throw new Error("Isolated browser fixture required");
  const hostname = process.env.COVE_BROWSER_MANAGED_REGISTRY_HOST ?? "";
  if (hostname !== `registry-${owner}.example.test`) throw new Error("The fixture must provide its own exact registry hostname");
  const file = (variable: string) => {
    const path = process.env[variable] ?? "";
    if (!isAbsolute(path)) throw new Error(`${variable} must be an absolute fixture path`);
    return readFileSync(path, "utf8");
  };
  return { database, hostname, certificate: file("COVE_BROWSER_MANAGED_REGISTRY_CERT"), key: file("COVE_BROWSER_MANAGED_REGISTRY_KEY"), root: file("COVE_BROWSER_MANAGED_REGISTRY_ROOT") };
}

interface Operation {
  id: string;
  action: string;
  state: string;
  workload_id: string;
  result: { error?: string; managed_registry?: { removed: boolean; data_retained: boolean } } | null;
}

interface Registry {
  id: string;
  name: string;
  hostname: string;
  state: string;
  connection_id: string;
  operation_id: string;
  operation_state: string;
  last_error: string;
  retention: { keep_per_repository: number; minimum_age_days: number };
  data_retained: boolean;
}

interface RegistryConnection {
  id: string;
  name: string;
  registry?: { server: string; username: string; password: string };
  managed_registry_id?: string;
}

const consumerCompose = ["services:", "  app:", "    image: busybox:1.37", "    pull_policy: never", "    init: true", "    restart: unless-stopped", "    command: [sleep, '86400']", ""].join("\n");

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
  for (let step = 0; step < 100 && (await combobox.getAttribute("aria-activedescendant")) !== targetId; step++) await page.keyboard.press("ArrowDown");
  await expect(combobox).toHaveAttribute("aria-activedescendant", targetId ?? "");
  await page.keyboard.press("Enter");
  await expect(combobox).toHaveText(option);
}

test("managed registry: real TLS failure, certificate repair, activation, garbage collection, removal and purge", async ({ page }, testInfo) => {
  test.skip(!enabled, "Needs scripts/test-browser.py --managed-registry");
  test.setTimeout(600_000);
  const { database, hostname, certificate, key, root } = fixture();
  const sql = (query: string) => docker("exec", database, "psql", "-U", "postgres", "-d", "cove", "-qAtc", query);
  // Background image checks start due. Deferring them keeps every operation in this team one the spec started, because
  // garbage collection and removal are refused while any team operation is queued or running.
  const deferImageChecks = (id: string) => {
    expect(id).toMatch(resourceId);
    expect(sql(`UPDATE workloads SET next_image_check_at=now()+interval '1 day' WHERE id='${id}' RETURNING id`).split(/\s+/).filter((value) => value === id)).toEqual([id]);
  };
  const secretMarkers = ["PRIVATE KEY", key.trim(), "COVE_REGISTRY_AUTH", "cove_registry:", "$2a$", "$2b$", "$2y$", "REGISTRY_AUTH_HTPASSWD"];
  const expectNoSecrets = (text: string) => {
    for (const marker of secretMarkers) expect(text.includes(marker), `response contains secret-like material (${marker.slice(0, 20)})`).toBe(false);
  };

  let registryId = "";
  let consumerId = "";
  let readyNodeId = "";
  // Fixture rows for nodes that are not ready, and the never-deployed resources placed on them by SQL.
  const unavailableNodes: string[] = [];
  const unavailableResources: string[] = [];
  let primary: Error | null = null;
  try {
    await page.goto(panelPath("/projects"));
    await expect(page).toHaveURL(/\/login(?:[?#]|$)/);
    await page.getByLabel("Password", { exact: true }).fill(password);
    await page.getByRole("button", { name: "Sign in", exact: true }).click();
    await expect(page).not.toHaveURL(/\/login(?:[?#]|$)/);
    const headers = { Origin: new URL(page.url()).origin };

    const nodes = (await (await page.request.get(panelPath("/api/nodes"))).json()) as { id: string; name: string; state: string; capabilities: { managed_registry?: boolean; file_secrets?: boolean } | null }[];
    const node = nodes.find((item) => item.name === "ready-browser-worker");
    if (!node) throw new Error("The browser worker is missing");
    expect(node.state).toBe("ready");
    expect(node.capabilities?.managed_registry).toBe(true);
    expect(node.capabilities?.file_secrets).toBe(true);
    readyNodeId = node.id;
    const projectName = `Managed registry ${Date.now()}`;
    const projectReply = await page.request.post(panelPath("/api/projects"), { headers, data: { name: projectName } });
    expect(projectReply.ok(), await projectReply.text()).toBe(true);
    const project = (await projectReply.json()) as { id: string };
    const registryName = `Browser registry ${Date.now()}`;

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
        .poll(async () => ((await (await page.request.get(panelPath("/api/operations"))).json()) as Operation[]).filter((item) => item.state === "queued" || item.state === "running").map((item) => `${item.action}:${item.id}`), { timeout: 120_000 })
        .toEqual([]);
    const registries = async () => {
      const response = await page.request.get(panelPath("/api/managed-registries"));
      const text = await response.text();
      expect(response.status(), text).toBe(200);
      expectNoSecrets(text);
      return (JSON.parse(text) as { registries: Registry[] }).registries;
    };
    const current = async () => {
      const found = (await registries()).find((item) => item.id === registryId);
      if (!found) throw new Error(`Managed registry ${registryId} is not listed`);
      return found;
    };
    // Waits for the registry to leave its transitional state, then fails at once with sanitized details unless it
    // reached the expected state through a successful operation.
    const registryReaches = async (state: string, operationState = "succeeded") => {
      await expect.poll(async () => (await current()).state, { timeout: 240_000 }).toMatch(/^(ready|failed|removed)$/);
      const reached = await current();
      if (reached.state !== state || reached.operation_state !== operationState) {
        const latest = reached.operation_id ? await operation(reached.operation_id) : null;
        throw new Error(`Registry reached ${reached.state} (operation ${reached.operation_state}), expected ${state}: ${reached.last_error} ${latest?.result?.error ?? ""}`);
      }
      return reached;
    };
    const generatedConnections = async () => {
      const response = await page.request.get(panelPath("/api/registries"));
      const text = await response.text();
      expect(response.status(), text).toBe(200);
      return (JSON.parse(text) as RegistryConnection[]).filter((item) => item.managed_registry_id);
    };
    const respondedTo = (path: string, method: string) => page.waitForResponse((response) => new URL(response.url()).pathname === path && response.request().method() === method);
    const expectNoBody = (response: Response) => expect(response.request().postData()).toBeNull();
    const consumerContainer = () => {
      const ids = docker("ps", "-q", "--filter", `label=com.docker.compose.project=cove-${consumerId}`, "--filter", "label=com.docker.compose.service=app").split(/\s+/).filter(Boolean);
      expect(ids).toHaveLength(1);
      return docker("inspect", "--format", "{{.Id}}|{{.State.StartedAt}}|{{.RestartCount}}", ids[0] ?? "");
    };

    // Create from the dashboard, mostly from the keyboard.
    await page.goto(panelPath("/registries"));
    const createForm = page.getByRole("form", { name: "Create a managed registry" });
    await expect(createForm.getByRole("button", { name: "Create managed registry" })).toBeEnabled({ timeout: 30_000 });
    await createForm.getByLabel("Name", { exact: true }).fill(registryName);
    await chooseWithKeyboard(page, createForm.getByRole("combobox", { name: "Project", exact: true }), projectName);
    await chooseWithKeyboard(page, createForm.getByRole("combobox", { name: "Node", exact: true }), node.name);
    await createForm.getByLabel("Hostname", { exact: true }).fill(hostname);
    await expect(createForm.getByLabel("Keep per repository", { exact: true })).toHaveValue("10");
    await expect(createForm.getByLabel("Minimum age in days", { exact: true })).toHaveValue("30");
    await expect(createForm.getByText("No verified hard storage cap", { exact: true })).toBeVisible();
    await capture(page, testInfo, "managed-registry-create");
    const createdReply = respondedTo(panelPath("/api/managed-registries"), "POST");
    await createForm.getByRole("button", { name: "Create managed registry" }).focus();
    await page.keyboard.press("Enter");
    const created = await createdReply;
    const createdText = await created.text();
    expect(created.status(), createdText).toBe(202);
    expect(created.request().postDataJSON()).toEqual({ name: registryName, node_id: node.id, project_id: project.id, hostname, retention: { keep_per_repository: 10, minimum_age_days: 30 } });
    expectNoSecrets(createdText);
    const createdBody = JSON.parse(createdText) as Record<string, string>;
    expect(Object.keys(createdBody).sort()).toEqual(["id", "operation_id"]);
    registryId = createdBody.id ?? "";
    expect(registryId).toMatch(resourceId);
    deferImageChecks(registryId);
    const item = page.locator(`#managed-registry-${registryId}`);
    await expect(page.getByText("Managed registry requested", { exact: true })).toBeVisible();
    await expect(item.getByText(hostname, { exact: true })).toBeVisible();

    // The real verification failure: the proxy serves its default certificate, which the fixture CA did not issue.
    const failed = await registryReaches("failed");
    expect(failed.last_error).toBe(tlsFailure);
    expect(failed.connection_id).toBe("");
    expect(await generatedConnections()).toEqual([]);
    await expect(item.getByText("Provisioning failed", { exact: true })).toBeVisible({ timeout: 30_000 });
    await expect(item.getByText(tlsFailure, { exact: true })).toBeVisible();
    await expect(item.getByText("Failed", { exact: true })).toBeVisible();
    const upload = item.getByRole("form", { name: "Upload a certificate" });
    await expect(upload).toBeVisible();
    await capture(page, testInfo, "managed-registry-failed");

    // Repair in the dashboard: upload the fixture leaf for this exact hostname, select it, then retry.
    const certificatesPath = `/api/workloads/${registryId}/certificates`;
    await upload.getByLabel("Name", { exact: true }).fill("Fixture registry leaf");
    await upload.getByLabel("Certificate chain", { exact: true }).fill(certificate);
    await upload.getByLabel("Private key", { exact: true }).fill(key);
    const uploadedReply = respondedTo(panelPath(certificatesPath), "POST");
    await upload.getByRole("button", { name: "Upload certificate" }).click();
    const uploaded = await uploadedReply;
    const uploadedText = await uploaded.text();
    expect(uploaded.status(), uploadedText).toBe(201);
    expect(uploaded.request().postDataJSON()).toEqual({ name: "Fixture registry leaf", certificate_pem: certificate, private_key_pem: key });
    expectNoSecrets(uploadedText);
    const stored = JSON.parse(uploadedText) as { id: string; dns_names: string[] };
    expect(stored.dns_names).toEqual([hostname]);
    await expect(upload.getByText("Certificate uploaded", { exact: true })).toBeVisible();
    await expect(upload.getByLabel("Certificate chain", { exact: true })).toHaveValue("");
    await expect(upload.getByLabel("Private key", { exact: true })).toHaveValue("");
    expect(await page.evaluate(() => JSON.stringify({ ...localStorage }) + JSON.stringify({ ...sessionStorage }) + location.href)).not.toContain("PRIVATE KEY");

    await chooseWithKeyboard(page, item.getByRole("combobox", { name: "Certificate", exact: true }), "Fixture registry leaf · Uploaded");
    const savedReply = respondedTo(panelPath(`/api/managed-registries/${registryId}`), "PUT");
    await item.getByRole("button", { name: "Save certificate selection" }).click();
    const saved = await savedReply;
    expect(saved.status(), await saved.text()).toBe(200);
    expect(saved.request().postDataJSON()).toEqual({ retention: { keep_per_repository: 10, minimum_age_days: 30 }, certificate_id: stored.id });
    await expect(item.getByText("Certificate selection saved. Retry provisioning to deploy it.", { exact: true })).toBeVisible();
    await capture(page, testInfo, "managed-registry-repair");

    const retryReply = respondedTo(panelPath(`/api/managed-registries/${registryId}/retry`), "POST");
    await item.getByRole("button", { name: "Retry provisioning" }).click();
    const retried = await retryReply;
    expect(retried.status(), await retried.text()).toBe(202);
    expectNoBody(retried);
    await expect(item.getByText("Provisioning retry requested", { exact: true })).toBeVisible();

    const ready = await registryReaches("ready");
    expect(ready.connection_id).not.toBe("");
    expect(ready.last_error).toBe("");
    const connections = await generatedConnections();
    expect(connections.map((connection) => ({ id: connection.id, managed: connection.managed_registry_id, server: connection.registry?.server, password: connection.registry?.password }))).toEqual([{ id: ready.connection_id, managed: registryId, server: hostname, password: "" }]);
    const generated = connections[0];
    if (!generated) throw new Error("The generated registry connection is missing");

    // The ingress serves the registry over verified TLS for this exact hostname and asks for credentials.
    const served = await verified(hostname, root, "/v2/");
    expect(served.status).toBe(401);

    await page.reload();
    await expect(item.getByText("Ready", { exact: true })).toBeVisible({ timeout: 30_000 });
    await expect(item.getByText("Generated team registry connection, listed with the registry connections on this page", { exact: true })).toBeVisible();
    await expect(item.getByRole("form", { name: "Upload a certificate" })).toHaveCount(0);
    const connectionRow = page.getByRole("listitem").filter({ has: page.getByRole("heading", { name: generated.name, exact: true }) });
    await expect(connectionRow).toHaveCount(1);
    await expect(connectionRow.getByRole("button", { name: "Test", exact: true })).toBeVisible();
    await expect(connectionRow.getByRole("button", { name: "Edit", exact: true })).toHaveCount(0);
    await expect(connectionRow.getByRole("button", { name: "Delete", exact: true })).toHaveCount(0);
    await expect(connectionRow.getByRole("link", { name: "View managed registry" })).toHaveAttribute("href", `#managed-registry-${registryId}`);
    await capture(page, testInfo, "managed-registry-ready");

    // An ordinary resource selects the generated connection and deploys. Its busybox image is cached locally and never
    // pulled, so this proves the connection is selectable and admitted, not a pull from the managed registry.
    const consumerReply = await page.request.post(panelPath("/api/workloads"), { headers, data: { name: "registry-consumer", node_id: node.id, kind: "service", compose: consumerCompose, project_id: project.id } });
    expect(consumerReply.status(), await consumerReply.text()).toBe(201);
    consumerId = ((await consumerReply.json()) as { id: string }).id;
    deferImageChecks(consumerId);
    const connect = await page.request.put(panelPath(`/api/workloads/${consumerId}/registry`), { headers, data: { registry_id: generated.id } });
    expect(connect.status(), await connect.text()).toBe(200);
    expect(await (await page.request.get(panelPath(`/api/workloads/${consumerId}/registry`))).json()).toEqual({ registry_id: generated.id });
    const deployReply = await page.request.post(panelPath(`/api/workloads/${consumerId}/operations`), { headers, data: { action: "deploy" } });
    expect(deployReply.status(), await deployReply.text()).toBe(202);
    await settled(((await deployReply.json()) as { id: string }).id);
    await teamQuiet();
    const consumerBefore = consumerContainer();

    // UI-ONLY MOCKED FAILURE, not part of the real lifecycle: the browser answers the next garbage collection POST itself
    // with the documented refusal for a ready node that could not list its references. It never reaches the panel, so
    // nothing is queued; the real run below then goes to the panel unchanged.
    const gcPath = panelPath(`/api/managed-registries/${registryId}/gc`);
    const isGc = (url: URL) => url.pathname === gcPath;
    const legacyFailure = {
      error: "registry_inventory_incomplete",
      resource: { id: "consumer", name: "Legacy service" },
      node: { id: "runner", name: "Ready runner" },
      reason: "Retained Compose snapshot is unreadable and its release cannot be proven older than this registry.",
    };
    let mockedRefusals = 0;
    const refuseOnce = async (route: Route) => {
      if (route.request().method() !== "POST" || mockedRefusals > 0) return route.fallback();
      mockedRefusals++;
      return route.fulfill({ status: 409, json: legacyFailure });
    };
    const beforeMock = await current();
    await page.route(isGc, refuseOnce);
    // Opened and confirmed at the narrowest width, so the first visibility check is the real mobile flow.
    await page.setViewportSize({ width: 320, height: 900 });
    await item.getByRole("button", { name: "Run garbage collection" }).click();
    const mockedDialog = page.getByRole("dialog", { name: `Run garbage collection on ${registryName}?` });
    await mockedDialog.getByRole("button", { name: "Run garbage collection" }).click();
    const refusal = mockedDialog.getByRole("alert");
    await expect(refusal).toContainText("Cove could not check every reference to this registry");
    await expect(refusal).toContainText("Garbage collection was not requested and nothing was queued.");
    await expect(refusal).toContainText("Legacy service");
    await expect(refusal).toContainText("Ready runner");
    await expect(refusal).toContainText(legacyFailure.reason);
    await expect(refusal).not.toContainText("registry_inventory_incomplete");
    await expectRefusalVisible(refusal, legacyFailure.reason);
    await expect(item.getByText("Garbage collection requested", { exact: true })).toHaveCount(0);
    await captureRefusal(page, testInfo, mockedDialog, refusal, legacyFailure.reason);
    await mockedDialog.getByRole("button", { name: "Cancel" }).click();
    await expect(mockedDialog).toHaveCount(0);
    await page.unroute(isGc, refuseOnce);
    expect(mockedRefusals).toBe(1);
    await expect(item.getByText("The registry operation was not requested", { exact: true })).toHaveCount(0);
    const afterMock = await current();
    expect(afterMock.state).toBe("ready");
    expect(afterMock.operation_id).toBe(beforeMock.operation_id);
    await teamQuiet();

    // Manual garbage collection from the dashboard.
    await item.getByRole("button", { name: "Run garbage collection" }).click();
    const gcDialog = page.getByRole("dialog", { name: `Run garbage collection on ${registryName}?` });
    await expect(gcDialog).toContainText("Running application containers are not stopped or deleted.");
    await capture(page, testInfo, "managed-registry-gc-confirm");
    const gcReply = respondedTo(panelPath(`/api/managed-registries/${registryId}/gc`), "POST");
    await gcDialog.getByRole("button", { name: "Run garbage collection" }).click();
    const gc = await gcReply;
    const gcText = await gc.text();
    expect(gc.status(), gcText).toBe(202);
    expectNoBody(gc);
    const gcOperation = (JSON.parse(gcText) as { operation_id: string }).operation_id;
    await expect(item.getByText("Garbage collection requested", { exact: true })).toBeVisible();
    const maintained = await settled(gcOperation);
    expect(maintained.action).toBe("registry_maintenance");
    expect(maintained.workload_id).toBe(registryId);
    const afterGc = await registryReaches("ready");
    expect(afterGc.operation_id).toBe(gcOperation);
    expect(afterGc.connection_id).toBe(generated.id);
    expect(consumerContainer()).toBe(consumerBefore);

    // Removal is refused while a resource still uses the generated connection; the registry stays ready.
    await teamQuiet();
    await item.getByRole("button", { name: "Remove registry" }).click();
    let removeDialog = page.getByRole("dialog", { name: `Remove ${registryName}?` });
    await expect(removeDialog.getByRole("checkbox", { name: "Also delete the registry data permanently" })).not.toBeChecked();
    const refusedReply = respondedTo(panelPath(`/api/managed-registries/${registryId}`), "DELETE");
    await removeDialog.getByRole("button", { name: "Remove registry", exact: true }).click();
    const refused = await refusedReply;
    expect(refused.status(), await refused.text()).toBe(409);
    expect(refused.request().postDataJSON()).toEqual({ delete_data: false });
    await expect(item.getByText("The registry operation was not requested", { exact: true })).toBeVisible();
    await expect(item.getByText("Disconnect resources and registry webhooks before removing this registry", { exact: true })).toBeVisible();
    const kept = await current();
    expect({ state: kept.state, connection: kept.connection_id }).toEqual({ state: "ready", connection: generated.id });
    expect(consumerContainer()).toBe(consumerBefore);

    // Disconnect the consumer, then remove with the default that keeps the data.
    const disconnect = await page.request.put(panelPath(`/api/workloads/${consumerId}/registry`), { headers, data: { registry_id: "" } });
    expect(disconnect.status(), await disconnect.text()).toBe(200);
    await teamQuiet();
    await item.getByRole("button", { name: "Remove registry" }).click();
    removeDialog = page.getByRole("dialog", { name: `Remove ${registryName}?` });
    const removeReply = respondedTo(panelPath(`/api/managed-registries/${registryId}`), "DELETE");
    await removeDialog.getByRole("button", { name: "Remove registry", exact: true }).click();
    const removed = await removeReply;
    expect(removed.status(), await removed.text()).toBe(202);
    expect(removed.request().postDataJSON()).toEqual({ delete_data: false });
    await expect(item.getByText("Removal requested", { exact: true })).toBeVisible();
    const retained = await registryReaches("removed");
    expect({ retained: retained.data_retained, connection: retained.connection_id }).toEqual({ retained: true, connection: "" });
    expect(await generatedConnections()).toEqual([]);
    expect(docker("volume", "ls", "-q", "--filter", `label=dev.cove.managed-registry=${registryId}`).split(/\s+/).filter(Boolean)).toEqual([`cove-${registryId}_data`]);
    // Once the route is gone the ingress either has no certificate for the hostname or no registry behind it.
    await expect.poll(() => verified(hostname, root, "/v2/").then((result) => result.status, (error: Error) => `rejected: ${error.message}`), { timeout: 60_000 }).not.toBe(401);
    await expect(page.getByRole("listitem").filter({ has: page.getByRole("heading", { name: generated.name, exact: true }) })).toHaveCount(0, { timeout: 30_000 });
    await expect(item.getByText("Registry data retained", { exact: true })).toBeVisible();
    await expect(item.getByText("Data volume retained on the node", { exact: true })).toBeVisible();
    await capture(page, testInfo, "managed-registry-retained");

    // Release the consumer and its retained releases, then purge the retained data from the dashboard.
    const stopReply = await page.request.post(panelPath(`/api/workloads/${consumerId}/operations`), { headers, data: { action: "stop" } });
    expect(stopReply.status(), await stopReply.text()).toBe(202);
    await settled(((await stopReply.json()) as { id: string }).id);
    const deleted = await page.request.delete(panelPath(`/api/workloads/${consumerId}`), { headers });
    expect(deleted.status(), await deleted.text()).toBe(200);

    // An enrolled node (pinned host key) that is not ready holds a resource of this team, so Cove cannot list its
    // references. The node is a database row on an unresolvable address; its resource never deployed and is created on
    // the ready worker, then moved there.
    const addUnavailableNode = async (label: string) => {
      const id = randomBytes(16).toString("hex");
      const name = `unavailable-${label}-${id.slice(0, 6)}`;
      expect(sql(`INSERT INTO nodes(id,name,address,port,public_key,private_key,host_key,state,mode) VALUES('${id}','${name}','${name}.example.test',22,'fixture','\\x00','fixture-host-key','unreachable','ssh') RETURNING id`)).toBe(id);
      unavailableNodes.push(id);
      const reply = await page.request.post(panelPath("/api/workloads"), { headers, data: { name, node_id: node.id, kind: "service", compose: consumerCompose, project_id: project.id } });
      expect(reply.status(), await reply.text()).toBe(201);
      const resource = ((await reply.json()) as { id: string }).id;
      unavailableResources.push(resource);
      deferImageChecks(resource);
      expect(sql(`UPDATE workloads SET node_id='${id}' WHERE id='${resource}' RETURNING node_id`)).toBe(id);
      return { id, name };
    };
    type UnavailableNode = { id: string; name: string };
    const byId = (nodes: UnavailableNode[]) => [...nodes].sort((a, b) => (a.id < b.id ? -1 : a.id > b.id ? 1 : 0));
    const inventoryPath = panelPath(`/api/managed-registries/${registryId}/removal-inventory`);
    const baselineReply = await page.request.get(inventoryPath);
    const baselineText = await baselineReply.text();
    expect(baselineReply.status(), baselineText).toBe(200);
    const baseline = (JSON.parse(baselineText) as { unavailable_nodes: UnavailableNode[] }).unavailable_nodes;
    const first = await addUnavailableNode("a");
    const once = byId([...baseline, first]);
    await teamQuiet();
    const inventoryReply = respondedTo(inventoryPath, "GET");
    await item.getByRole("button", { name: "Delete retained data" }).click();
    const purgeDialog = page.getByRole("dialog", { name: `Delete the retained data of ${registryName}?` });
    const purgeButton = purgeDialog.getByRole("button", { name: "Delete data permanently" });
    const inventory = await inventoryReply;
    const inventoryText = await inventory.text();
    expect(inventory.status(), inventoryText).toBe(200);
    expect(JSON.parse(inventoryText)).toEqual({ unavailable_nodes: once });
    const unavailableList = purgeDialog.getByRole("list", { name: "Unavailable nodes" });
    await expect(unavailableList.getByRole("listitem")).toHaveText(once.map((entry) => entry.name));
    const acknowledge = purgeDialog.getByRole("checkbox", { name: "Delete the data even if releases or builds on these nodes become unrecoverable" });
    await expect(acknowledge).not.toBeChecked();
    await expect(purgeButton).toBeDisabled();
    await purgeDialog.getByLabel(`Type ${hostname} to confirm`, { exact: true }).fill(hostname);
    await expect(purgeButton).toBeDisabled();
    await acknowledge.check();
    await expect(purgeButton).toBeEnabled();
    await capture(page, testInfo, "managed-registry-unavailable-nodes");

    // A second unavailable node appears after the acknowledgement: Cove refuses, names it and needs a new acknowledgement.
    const second = await addUnavailableNode("b");
    let deletes = 0;
    const countDeletes = (request: { url: () => string; method: () => string }) => {
      if (request.method() === "DELETE" && new URL(request.url()).pathname === panelPath(`/api/managed-registries/${registryId}`)) deletes++;
    };
    page.on("request", countDeletes);
    const changedReply = respondedTo(panelPath(`/api/managed-registries/${registryId}`), "DELETE");
    await purgeButton.click();
    const changed = await changedReply;
    const changedText = await changed.text();
    expect(changed.status(), changedText).toBe(409);
    expect(changed.request().postDataJSON()).toEqual({ delete_data: true, acknowledge_unavailable_nodes: once.map((entry) => entry.id) });
    const both = byId([...once, second]);
    expect(JSON.parse(changedText)).toEqual({ error: "registry_inventory_changed", message: "Review and acknowledge the unavailable nodes before deleting registry data.", unavailable_nodes: both });
    await expect(purgeDialog.getByText("The unavailable nodes changed", { exact: true })).toBeVisible();
    await expect(unavailableList.getByRole("listitem")).toHaveText(both.map((entry) => entry.name));
    await expect(purgeDialog.getByLabel(`Type ${hostname} to confirm`, { exact: true })).toHaveValue(hostname);
    await expect(acknowledge).not.toBeChecked();
    await expect(purgeButton).toBeDisabled();
    expect((await current()).data_retained).toBe(true);
    await capture(page, testInfo, "managed-registry-inventory-changed");
    expect(deletes).toBe(1);
    page.off("request", countDeletes);

    await acknowledge.check();
    const purgeReply = respondedTo(panelPath(`/api/managed-registries/${registryId}`), "DELETE");
    await purgeButton.click();
    const purged = await purgeReply;
    expect(purged.status(), await purged.text()).toBe(202);
    expect(purged.request().postDataJSON()).toEqual({ delete_data: true, acknowledge_unavailable_nodes: both.map((entry) => entry.id) });
    await expect(purgeDialog).toHaveCount(0);
    const gone = await registryReaches("removed");
    expect(gone.data_retained).toBe(false);
    expect(docker("ps", "-aq", "--filter", `label=com.docker.compose.project=cove-${registryId}`)).toBe("");
    expect(docker("volume", "ls", "-q", "--filter", `label=dev.cove.managed-registry=${registryId}`)).toBe("");
    await expect(item.getByText("No retained data reported", { exact: true })).toBeVisible({ timeout: 30_000 });
    await expect(item.getByRole("button", { name: "Delete retained data" })).toHaveCount(0);
    await capture(page, testInfo, "managed-registry-purged");

    // Return the never-deployed resources to the ready worker, delete them and drop the fixture node rows.
    for (const resource of unavailableResources.splice(0)) {
      expect(sql(`UPDATE workloads SET node_id='${node.id}' WHERE id='${resource}' RETURNING id`)).toBe(resource);
      const removed = await page.request.delete(panelPath(`/api/workloads/${resource}`), { headers });
      expect(removed.status(), await removed.text()).toBe(200);
    }
    for (const id of unavailableNodes.splice(0)) expect(sql(`DELETE FROM nodes WHERE id='${id}' RETURNING id`)).toBe(id);

    // Delete the empty record from the dashboard; the generated resource and its operation history go with it.
    await teamQuiet();
    await item.getByRole("button", { name: "Delete registry record" }).click();
    const recordDialog = page.getByRole("dialog", { name: `Delete the record of ${registryName}?` });
    await expect(recordDialog).toContainText("This deletes the empty registry resource and its operation history from Cove.");
    const recordReply = respondedTo(panelPath(`/api/managed-registries/${registryId}/record`), "DELETE");
    await recordDialog.getByRole("button", { name: "Delete registry record" }).click();
    const recordDeleted = await recordReply;
    const recordText = await recordDeleted.text();
    expect(recordDeleted.status(), recordText).toBe(200);
    expect(JSON.parse(recordText)).toEqual({ ok: true, cleanup: { worker_files_retained: false } });
    expectNoBody(recordDeleted);
    await expect(item).toHaveCount(0, { timeout: 30_000 });
    expect((await registries()).map((entry) => entry.id)).not.toContain(registryId);
    const workloadsReply = await page.request.get(panelPath("/api/workloads"));
    const workloadsText = await workloadsReply.text();
    expect(workloadsReply.status(), workloadsText).toBe(200);
    expect((JSON.parse(workloadsText) as { id: string }[]).map((entry) => entry.id)).not.toContain(registryId);
  } catch (error) {
    primary = asError(error);
  }
  // Cleanup never replaces the first failure; its own failure fails the test only when the test otherwise passed.
  let cleanupFailure: Error | null = null;
  try {
    cleanupBrowserResources();
    if (readyNodeId) for (const resource of unavailableResources) sql(`UPDATE workloads SET node_id='${readyNodeId}' WHERE id='${resource}'`);
    for (const id of unavailableNodes) sql(`DELETE FROM nodes WHERE id='${id}' AND NOT EXISTS(SELECT 1 FROM workloads WHERE node_id='${id}')`);
    if (registryId) {
      const volumes = docker("volume", "ls", "-q", "--filter", `label=dev.cove.managed-registry=${registryId}`).split(/\s+/).filter(Boolean);
      for (const volume of volumes) if (volume === `cove-${registryId}_data`) docker("volume", "rm", volume);
    }
  } catch (error) {
    cleanupFailure = asError(error);
  }
  if (primary !== null) {
    if (cleanupFailure !== null) testInfo.annotations.push({ type: "cleanup-failure", description: cleanupFailure.message });
    throw primary;
  }
  if (cleanupFailure !== null) throw cleanupFailure;
});

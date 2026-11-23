// Real-backend UI acceptance for durable workload node moves. No API mocks: every request reaches the
// backend started by the Go move test through `python scripts/test-workload-move.py --browser`, which serves a
// frozen production dist and supplies the session. Not a Playwright test file; it never targets real hosts or staging.
//
// Env (all required):
//   COVE_MOVE_BROWSER_URL       local https origin of the test control plane (never printed)
//   COVE_MOVE_BROWSER_COOKIE    __Host-cove session value for a team owner (never printed or stored)
//   COVE_MOVE_BROWSER_RESOURCE  resource id to move
//   COVE_MOVE_BROWSER_TARGET    destination node id
//   COVE_MOVE_BROWSER_SHOTS     artifact directory outside the repository
//   COVE_MOVE_BROWSER_FRONTEND  worktree frontend directory, used only to resolve @playwright/test
// Optional: COVE_MOVE_BROWSER_TIMEOUT_MS (default 900000) for the move to reach Succeeded.
// Exit code 0 means every check passed; results.json in the shots directory records the evidence.
import { createRequire } from "node:module";
import { mkdirSync, writeFileSync } from "node:fs";
import { join } from "node:path";

function required(name) {
  const value = process.env[name] ?? "";
  if (value === "") throw new Error(`${name} is required`);
  return value;
}

const baseURL = required("COVE_MOVE_BROWSER_URL");
const cookie = required("COVE_MOVE_BROWSER_COOKIE");
const resource = required("COVE_MOVE_BROWSER_RESOURCE");
const target = required("COVE_MOVE_BROWSER_TARGET");
const shots = required("COVE_MOVE_BROWSER_SHOTS");
const frontend = required("COVE_MOVE_BROWSER_FRONTEND");
const moveTimeout = Number(process.env.COVE_MOVE_BROWSER_TIMEOUT_MS ?? 900_000);

const origin = new URL(baseURL);
if (origin.protocol !== "https:") throw new Error("COVE_MOVE_BROWSER_URL must be https");
// Certificate errors are ignored only for the harness's loopback fixture.
if (!["127.0.0.1", "localhost", "[::1]"].includes(origin.hostname)) throw new Error("COVE_MOVE_BROWSER_URL must be a loopback fixture");
if (!/^[A-Za-z0-9_-]{1,128}$/.test(resource) || !/^[A-Za-z0-9_-]{1,128}$/.test(target)) throw new Error("Unexpected resource or target id");

const { chromium } = createRequire(join(frontend, "package.json"))("@playwright/test");
mkdirSync(shots, { recursive: true });

const results = { started_at: new Date().toISOString(), resource, target, checks: [], phases: [], operation_id: null, passed: false };
function check(name, ok, detail) {
  results.checks.push({ name, ok: Boolean(ok), ...(detail === undefined ? {} : { detail }) });
  console.log(`${ok ? "PASS" : "FAIL"} ${name}${detail === undefined ? "" : ` ${JSON.stringify(detail)}`}`);
  if (!ok) throw new Error(`Check failed: ${name}`);
}

// Strips anything that could carry the origin or session before it reaches logs or results.json.
function scrub(text) {
  return String(text).split(baseURL).join("<base>").split(origin.host).join("<host>").split(cookie).join("<cookie>");
}

async function api(page, path) {
  return page.evaluate(async (path) => {
    const response = await fetch(path, { credentials: "same-origin", cache: "no-store", headers: { Accept: "application/json" } });
    return { status: response.status, cache: response.headers.get("cache-control"), body: response.ok ? await response.json() : null };
  }, path);
}

async function shoot(page, name, locator) {
  const widths = [];
  for (const width of [390, 768, 1440]) {
    await page.setViewportSize({ width, height: 900 });
    await page.waitForTimeout(200);
    const fits = await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth);
    widths.push({ width, fits });
    const path = join(shots, `${name}-${width}.png`);
    if (locator) {
      await locator.scrollIntoViewIfNeeded();
      await locator.screenshot({ path });
    } else {
      await page.screenshot({ path });
    }
  }
  await page.setViewportSize({ width: 1440, height: 900 });
  check(`${name}: no horizontal overflow at 390/768/1440`, widths.every((item) => item.fits), widths);
}

const browser = await chromium.launch();
const context = await browser.newContext({ ignoreHTTPSErrors: true, viewport: { width: 1440, height: 900 } });
await context.addCookies([{ name: "__Host-cove", value: cookie, url: `${origin.origin}/`, secure: true, httpOnly: true, sameSite: "Strict" }]);
const page = await context.newPage();
const consoleErrors = [];
page.on("pageerror", (error) => consoleErrors.push(scrub(error.message)));
page.on("console", (message) => {
  if (message.type() === "error") consoleErrors.push(scrub(message.text()));
});

let exitCode = 1;
try {
  await page.goto(`${origin.origin}/resources/${encodeURIComponent(resource)}/general`);
  await page.getByRole("heading", { name: "General", exact: true }).or(page.getByLabel("Password", { exact: true })).first().waitFor({ timeout: 60_000 });
  check("session accepted (no login redirect)", !/\/login(?:[?#]|$)/.test(new URL(page.url()).pathname));

  const session = await api(page, "/api/session");
  check("signed in as a team owner, not an instance administrator", session.status === 200 && session.body?.is_admin === false && session.body?.teams?.some((team) => team.id === session.body.team_id && team.role === "owner"));
  const before = await api(page, `/api/workloads/${encodeURIComponent(resource)}`);
  const source = before.body?.node_id;
  check("resource starts on another node than the target", before.status === 200 && typeof source === "string" && source !== target);
  const nodes = (await api(page, "/api/nodes")).body ?? [];
  const sourceName = nodes.find((node) => node.id === source)?.name;
  const targetName = nodes.find((node) => node.id === target)?.name;
  check("fixture node names", sourceName === "Source worker" && targetName === "Destination worker", { sourceName, targetName });
  const initialMove = await api(page, `/api/workloads/${encodeURIComponent(resource)}/node-move`);
  check("move status is no-store and has no unresolved move", initialMove.status === 200 && initialMove.cache === "no-store" && (!initialMove.body?.move || ["succeeded", "rolled_back"].includes(initialMove.body.move.phase)), { phase: initialMove.body?.move?.phase ?? null });

  const section = page.locator("section").filter({ has: page.getByRole("heading", { name: "Move to another node", exact: true }) });
  await section.waitFor({ timeout: 30_000 });
  if (initialMove.body?.move?.phase === "rolled_back") {
    check("earlier proven rollback is shown honestly", await section.getByText("Rolled back", { exact: true }).isVisible());
  }

  // Keyboard only: choose the destination and request the preview.
  const combobox = section.getByRole("combobox", { name: "Destination node" });
  await combobox.waitFor({ timeout: 30_000 });
  const optionsText = await section.locator("select option").allTextContents();
  check("destination list offers the target and not the source", optionsText.includes("Destination worker") && !optionsText.includes("Source worker"), optionsText);
  await combobox.focus();
  await page.keyboard.press("Enter");
  await page.getByRole("listbox").waitFor();
  const targetOption = await page.locator(`[role=option][data-value="${target}"]`).getAttribute("id");
  for (let step = 0; step <= optionsText.length && (await combobox.getAttribute("aria-activedescendant")) !== targetOption; step++) {
    await page.keyboard.press("ArrowDown");
  }
  await page.keyboard.press("Enter");
  check("keyboard selected the target node", (await section.locator("select").inputValue()) === target && (await combobox.textContent())?.trim() === "Destination worker");
  check("focus stays on the destination control", await combobox.evaluate((element) => element === document.activeElement));
  await page.keyboard.press("Tab");
  const previewButton = section.getByRole("button", { name: "Preview move", exact: true });
  check("Tab reaches Preview move", await previewButton.evaluate((element) => element === document.activeElement));
  const previewResponse = page.waitForResponse((response) => response.url().endsWith(`/api/workloads/${resource}/node-move/preview`) && response.request().method() === "POST", { timeout: 180_000 });
  await page.keyboard.press("Enter");
  const preview = await previewResponse;
  const previewBody = await preview.json();
  check("real preview succeeded and is no-store", preview.status() === 200 && preview.headers()["cache-control"] === "no-store");
  check("preview request named only the chosen destination", JSON.stringify(preview.request().postDataJSON()) === JSON.stringify({ destination_node_id: target }));
  const volumes = previewBody.source.volumes.map((volume) => volume.name);
  results.preview = { containers: previewBody.source.containers, running: previewBody.source.running, volumes, architecture: previewBody.source.architecture, max_bytes: previewBody.source.max_bytes, target_containers: previewBody.target.containers, target_volumes: previewBody.target.volumes.length };
  check("preview counts two containers with one running", previewBody.source.containers === 2 && previewBody.source.running === 1, results.preview);
  check("preview includes the application volume and the local backup repository", volumes.length >= 2, volumes);
  check("destination is empty", previewBody.target.containers === 0 && previewBody.target.volumes.length === 0);

  await section.getByText("Move preview", { exact: true }).waitFor();
  check("UI shows source and destination", (await section.getByText("Source worker", { exact: true }).count()) > 0 && (await section.getByText("Destination worker").count()) > 0);
  check("UI shows container counts", await section.getByText("2 containers, 1 running", { exact: true }).isVisible());
  for (const name of volumes) check(`UI lists volume ${name}`, await section.getByText(name, { exact: true }).isVisible());
  const sectionText = (await section.textContent()) ?? "";
  check("UI hides validation internals", [previewBody.source.daemon, previewBody.target.daemon, previewBody.source.root].every((value) => !value || !sectionText.includes(value)));
  await shoot(page, "01-preview", section);

  // Confirm downtime by keyboard and queue through the real API.
  const moveButton = section.getByRole("button", { name: "Move with downtime", exact: true });
  await moveButton.focus();
  await page.keyboard.press("Enter");
  const dialog = page.getByRole("dialog", { name: "Move " + before.body.name + " to Destination worker?" });
  await dialog.waitFor();
  check("confirmation names the resource and both nodes", (await dialog.getByText(/Running containers on Source worker are stopped/).isVisible()) && (await dialog.getByText(/copied to Destination worker/).isVisible()));
  check("confirmation explains health check before placement", await dialog.getByText(/placement switches only after that/).isVisible());
  check("focus moves into the confirmation", await dialog.evaluate((element) => element.contains(document.activeElement)));
  await shoot(page, "02-confirm");
  await page.keyboard.press("Escape");
  await dialog.waitFor({ state: "hidden" });
  check("Escape cancels without queueing", (await api(page, `/api/workloads/${encodeURIComponent(resource)}/node-move`)).body?.move?.phase !== "queued");
  await moveButton.focus();
  await page.keyboard.press("Enter");
  await dialog.waitFor();
  const stop = dialog.getByRole("button", { name: "Stop and move", exact: true });
  for (let step = 0; step < 4 && !(await stop.evaluate((element) => element === document.activeElement)); step++) await page.keyboard.press("Tab");
  check("Tab reaches Stop and move", await stop.evaluate((element) => element === document.activeElement));
  const queueResponse = page.waitForResponse((response) => response.url().endsWith(`/api/workloads/${resource}/node-move`) && response.request().method() === "POST", { timeout: 180_000 });
  await page.keyboard.press("Enter");
  const queued = await queueResponse;
  const queuedBody = await queued.json().catch(() => null);
  check("real queue accepted with 202", queued.status() === 202 && typeof queuedBody?.id === "string", { status: queued.status() });
  check("queue sent the exact reviewed revision with confirmation", JSON.stringify(queued.request().postDataJSON()) === JSON.stringify({ destination_node_id: target, revision: previewBody.revision, confirm: true }));
  results.operation_id = queuedBody.id;

  // Wait for the UI's own polling to reach Moved, recording each phase label it shows.
  const deadline = Date.now() + moveTimeout;
  const labels = ["Queued", "Stopping the source", "Exporting", "Transferring", "Starting on destination", "Committing placement", "Removing source leftovers", "Rolling back", "Discarding destination copy", "Moved", "Rolled back"];
  let shown = "";
  while (Date.now() < deadline) {
    for (const label of labels) {
      if (await section.getByText(label, { exact: true }).first().isVisible().catch(() => false)) {
        if (label !== shown) {
          shown = label;
          results.phases.push({ label, at: new Date().toISOString() });
          console.log(`phase ${label}`);
        }
        break;
      }
    }
    if (shown === "Moved" || shown === "Rolled back" || shown === "Rolling back") break;
    await page.waitForTimeout(1_000);
  }
  if (shown !== "Moved") {
    const error = await section.getByRole("alert").first().textContent().catch(() => null);
    check("move reached Moved in the UI", false, { last: shown, error: error ? scrub(error) : null });
  }
  check("move reached Moved in the UI", true, { phases: results.phases.map((item) => item.label) });

  const operation = await api(page, `/api/operations/${encodeURIComponent(results.operation_id)}`);
  check("move operation succeeded", operation.status === 200 && operation.body?.action === "move" && operation.body?.state === "succeeded");
  const after = await api(page, `/api/workloads/${encodeURIComponent(resource)}`);
  check("resource placement is the target", after.body?.node_id === target);
  const finalMove = await api(page, `/api/workloads/${encodeURIComponent(resource)}/node-move`);
  check("move status reports succeeded for this operation", finalMove.body?.move?.id === results.operation_id && finalMove.body?.move?.phase === "succeeded");

  // The UI reloads the resource once the move commits.
  const nodeRow = page.locator("xpath=//p[normalize-space()='Node']/../..");
  await nodeRow.getByText("Destination worker", { exact: true }).waitFor({ timeout: 30_000 });
  check("General shows the new node", true);
  await page.locator(`a[href="/nodes/${target}"]`, { hasText: "Destination worker" }).first().waitFor({ timeout: 30_000 });
  check("resource header links the new node", true);
  await section.getByRole("combobox", { name: "Destination node" }).waitFor({ timeout: 30_000 });
  const nextOptions = await section.locator("select option").allTextContents();
  check("planner now offers the old source, not the new node", nextOptions.includes("Source worker") && !nextOptions.includes("Destination worker"), nextOptions);
  await shoot(page, "03-final-general", section);

  await page.goto(`${origin.origin}/resources/${encodeURIComponent(resource)}/operations`);
  await page.getByText("Move to another node").first().waitFor({ timeout: 30_000 });
  check("operation history labels the move", true);
  await shoot(page, "04-final-operations");

  await page.goto(`${origin.origin}/operations/${encodeURIComponent(results.operation_id)}`);
  await page.getByText("Succeeded").first().waitFor({ timeout: 30_000 });
  check("operation page shows Succeeded", true);
  await shoot(page, "05-final-operation");

  const unexpected = consoleErrors.filter((message) => !message.includes("Failed to load resource"));
  check("no page or console errors", unexpected.length === 0, unexpected.slice(0, 5));
  results.passed = true;
  exitCode = 0;
} catch (error) {
  results.error = scrub(error instanceof Error ? error.message : error);
  console.error(`ERROR ${results.error}`);
  await page.screenshot({ path: join(shots, "failure.png"), fullPage: true }).catch(() => {});
} finally {
  results.finished_at = new Date().toISOString();
  results.console_errors = consoleErrors.slice(0, 20);
  writeFileSync(join(shots, "results.json"), JSON.stringify(results, null, 2));
  await context.clearCookies();
  await browser.close();
}
process.exit(exitCode);

import { mkdtempSync, readFileSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { createServer } from "node:net";
import { createHash, randomBytes } from "node:crypto";
import { expect, test, type Download, type Page, type TestInfo, type Locator } from "@playwright/test";
import { spawn, execFileSync } from "node:child_process";

const password = process.env.COVE_BROWSER_PASSWORD ?? "";
const compose = (marker: string) =>
  `services:\n  app:\n    image: busybox:1.37\n    command: [sh, -c, 'echo ${marker}; sleep 300']\n    healthcheck:\n      test: [CMD, 'true']\n      interval: 1s\n`;

test.afterEach(() => {
  const database = process.env.COVE_BROWSER_DATABASE;
  if (!database || !/^cove-browser-[a-f0-9]+-db$/.test(database)) throw new Error("Isolated browser database required for cleanup");
  const resources = execFileSync("docker", ["exec", database, "psql", "-U", "postgres", "-d", "cove", "-Atc", "UPDATE workloads w SET manually_stopped=true WHERE NOT EXISTS(SELECT 1 FROM operations o WHERE o.workload_id=w.id AND o.state IN ('queued','running')) RETURNING w.id"], { encoding: "utf8" }).split(/\s+/).filter(value => /^[a-f0-9]{32}$/.test(value));
  for (const resource of resources) {
    const ids = execFileSync("docker", ["ps", "-aq", "--filter", `label=com.docker.compose.project=cove-${resource}`], { encoding: "utf8" }).split(/\s+/).filter(Boolean);
    if (ids.length) execFileSync("docker", ["rm", "-f", "-v", ...ids]);
    const networks = execFileSync("docker", ["network", "ls", "-q", "--filter", `label=com.docker.compose.project=cove-${resource}`], { encoding: "utf8" }).split(/\s+/).filter(Boolean);
    if (networks.length) execFileSync("docker", ["network", "rm", ...networks]);
  }
});

async function chooseOption(control: Locator, selection: string | { label?: string; index?: number }) {
  await control.click();
  const list = control.page().getByRole("listbox");
  await expect(list).toBeVisible();
  const options = list.getByRole("option");
  if (typeof selection === "string") {
    const count = await options.count();
    for (let index = 0; index < count; index++) {
      if (await options.nth(index).getAttribute("data-value") === selection) { await options.nth(index).click(); return; }
    }
    throw new Error(`Select option not found: ${selection}`);
  }
  if (selection.index !== undefined) await options.nth(selection.index).click();
  else await list.getByRole("option", { name: selection.label, exact: true }).click();
}

// The Compose editor is CodeMirror, which renders one .cm-line element per document line.
function editorText(editor: Locator) {
  return editor.evaluate((element) => Array.from(element.querySelectorAll(".cm-line"), (line) => line.textContent).join("\n"));
}

async function signIn(page: Page, secret = password, email = "") {
	await expect(page).toHaveURL(/\/login(?:[?#]|$)/);
  if (email) await page.getByLabel("Email", { exact: true }).fill(email);
  await page.getByLabel("Password", { exact: true }).fill(secret);
  await page.getByRole("button", { name: "Sign in", exact: true }).click();
  // Navigating before the login request settles would cancel it.
  await expect(page).not.toHaveURL(/\/login(?:[?#]|$)/);
}

async function expectNoHorizontalScroll(page: Page) {
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBe(true);
}

async function capture(page: Page, testInfo: TestInfo, name: string) {
  for (const width of [390, 768, 1440]) {
    await page.setViewportSize({ width, height: 900 });
    await page.evaluate(() => window.scrollTo(0, 0));
    await expectNoHorizontalScroll(page);
    await page.screenshot({ path: testInfo.outputPath(`${name}-${width}.png`), fullPage: true });
  }
}

async function createProject(page: Page, name: string) {
  await page.goto("/projects");
  await page.locator("main header").getByRole("button", { name: "New project", exact: true }).click();
  await page.getByLabel("Project name", { exact: true }).fill(name);
  await page.getByRole("button", { name: "Create project", exact: true }).click();
  await expect(page.getByRole("heading", { name, exact: true })).toBeVisible();
  await expect(page.getByRole("link", { name: /production/ }).first()).toBeVisible();
}

// Reads the resource's application containers from the Docker host the real worker manages.
function containers(resourceURL: string) {
  const workload = new URL(resourceURL).pathname.split("/")[2] ?? "";
  if (!/^[0-9a-f]{32}$/.test(workload)) throw new Error("Unexpected resource URL");
  const ids = execFileSync("docker", ["ps", "-aq", "--filter", `label=com.docker.compose.project=cove-${workload}`], { encoding: "utf8" }).split(/\s+/).filter(Boolean);
  expect(ids.length).toBeGreaterThan(0);
  return execFileSync("docker", ["inspect", "--format", '{{.Id}}|{{.State.StartedAt}}|{{.State.Status}}|{{if index .NetworkSettings.Networks "cove-ingress"}}routed{{end}}', ...ids], { encoding: "utf8" }).trim();
}

function proxyRouters() {
  return execFileSync("docker", ["exec", "cove-ingress", "wget", "-qO-", "http://127.0.0.1:8080/api/http/routers"], { encoding: "utf8" });
}

// Manual deploys check capacity first. Passing checks queue at once; otherwise the dialog needs an explicit choice.
async function deployFromHeader(page: Page) {
  await page.getByRole("button", { name: "Deploy", exact: true }).click();
  const proceed = page.getByRole("dialog", { name: "Capacity before deploy" }).getByRole("button", { name: /^Deploy (anyway|without check)$/ });
  await expect(proceed.or(page.getByText("Deploy queued", { exact: true }))).toBeVisible({ timeout: 60_000 });
  if (await proceed.isVisible()) await proceed.click();
}

async function createResource(page: Page, name: string) {
  await page.getByRole("link", { name: "New resource", exact: true }).first().click();
  await page.getByRole("button", { name: "Custom Compose or Git" }).click();
  await page.getByLabel("Name", { exact: true }).fill(name);
  await chooseOption(page.getByLabel("Node", { exact: true }), { label: "ready-browser-worker" });
  await page.getByLabel("Compose file", { exact: true }).fill(compose(name));
  await page.getByRole("button", { name: "Create resource", exact: true }).click();
  await expect(page.getByRole("heading", { name, exact: true })).toBeVisible();
  return page.url();
}

test("first administrator setup and dashboard integration settings", async ({ page }, testInfo) => {
  const database = process.env.COVE_BROWSER_DATABASE;
  if (!database || !/^cove-browser-[a-f0-9]+-db$/.test(database)) throw new Error("Isolated browser database required");
  const sql = (query: string) => execFileSync("docker", ["exec", database, "psql", "-U", "postgres", "-d", "cove", "-Atc", query], { encoding: "utf8" }).trim();
  expect(sql("SELECT count(*) FROM users")).toBe("1");
  const previousHash = sql("SELECT encode(password_hash,'hex') FROM users WHERE id='bootstrap'");
  expect(previousHash).toMatch(/^[a-f0-9]+$/);
  const membership = sql("SELECT count(*) FROM team_members WHERE team_id='default' AND user_id='bootstrap'");
  try {
    sql("UPDATE users SET password_hash=NULL WHERE id='bootstrap'; UPDATE control_settings SET setup_completed=false,registration_enabled=false");
    await page.goto("/login");
    await expect(page).toHaveURL(/\/register$/);
    await expect(page.getByRole("heading", { name: "Set up Cove" })).toBeVisible();
    for (const width of [320, 390, 768, 1280, 1920]) {
      await page.setViewportSize({ width, height: 900 });
      await expectNoHorizontalScroll(page);
      await page.screenshot({ path: testInfo.outputPath(`first-setup-${width}.png`), fullPage: true });
    }
    await page.getByLabel("Name", { exact: true }).fill("Instance owner");
    await page.getByLabel("Email", { exact: true }).fill("owner@example.com");
    await page.getByLabel("Password", { exact: true }).fill(password);
    await page.getByLabel("Confirm password", { exact: true }).fill(password);
    await page.getByRole("button", { name: "Create administrator account" }).click();
    await expect(page).toHaveURL(/\/dashboard$/);
    await page.goto("/admin");
    await page.getByLabel("SMTP host", { exact: true }).fill("smtp.example.com");
    await page.getByLabel("SMTP port", { exact: true }).fill("465");
    await page.getByLabel("Sender email", { exact: true }).fill("cove@example.com");
    await page.getByLabel("SMTP username", { exact: false }).fill("fixture-mailer");
    await page.getByLabel("SMTP password", { exact: false }).fill("fixture-smtp-password");
    await page.locator("form").filter({ has: page.getByLabel("SMTP host", { exact: true }) }).getByRole("button", { name: "Save changes" }).click();
    await expect(page.getByText("Email settings saved", { exact: true })).toBeVisible();
    await expect(page.getByLabel("SMTP password", { exact: false })).toHaveValue("");
    await page.getByLabel("Client ID", { exact: true }).fill("fixture-oauth-client");
    await page.getByLabel("Client secret", { exact: true }).fill("fixture-oauth-secret");
    await page.locator("form").filter({ has: page.getByLabel("Client ID", { exact: true }) }).getByRole("button", { name: "Save changes" }).click();
    await expect(page.getByText("GitHub settings saved", { exact: true })).toBeVisible();
    await page.reload();
    await expect(page.getByLabel("SMTP host", { exact: true })).toHaveValue("smtp.example.com");
    await expect(page.getByLabel("Client ID", { exact: true })).toHaveValue("fixture-oauth-client");
    const settings = await (await page.request.get("/api/settings/integrations")).json() as { smtp: { password_set: boolean; password?: string }; github: { client_secret_set: boolean; client_secret?: string } };
    expect(settings.smtp.password_set).toBe(true);
    expect(settings.smtp.password).toBeUndefined();
    expect(settings.github.client_secret_set).toBe(true);
    expect(settings.github.client_secret).toBeUndefined();
    await page.getByRole("button", { name: "Disable email", exact: true }).click();
    const emailDialog = page.getByRole("dialog", { name: "Disable email delivery?" });
    await expect(emailDialog).toBeVisible();
    await emailDialog.press("Escape");
    await expect(emailDialog).not.toBeVisible();
    await expect(page.getByLabel("SMTP host", { exact: true })).toHaveValue("smtp.example.com");
    await page.getByRole("button", { name: "Disable email", exact: true }).click();
    await emailDialog.getByRole("button", { name: "Disable email", exact: true }).click();
    await expect(page.getByLabel("SMTP host", { exact: true })).toHaveValue("");
    await page.getByRole("button", { name: "Disable GitHub sign-in", exact: true }).click();
    await page.getByRole("dialog", { name: "Disable GitHub sign-in?" }).getByRole("button", { name: "Disable GitHub sign-in", exact: true }).click();
    await expect(page.getByLabel("Client ID", { exact: true })).toHaveValue("");
    await expectNoHorizontalScroll(page);
  } finally {
    sql(`UPDATE users SET email=NULL,name='Administrator',password_hash=decode('${previousHash}','hex') WHERE id='bootstrap'; DELETE FROM sessions WHERE user_id='bootstrap'; UPDATE control_settings SET setup_completed=true,registration_enabled=false,integration_settings=NULL`);
    if (membership === "0") sql("DELETE FROM team_members WHERE team_id='default' AND user_id='bootstrap'");
  }
});

test("panel recovery settings save locally and wait for the host timer", async ({ page }, testInfo) => {
  await page.goto("/admin");
  await signIn(page);
  const status = page.locator("section", { has: page.getByRole("heading", { name: "Panel recovery", exact: true }) });
  await expect(status.getByText("Host timer not connected", { exact: true })).toBeVisible();
  await expect(status.getByText("sudo sh scripts/install-panel-recovery.sh /opt/cove", { exact: true })).toBeVisible();
  await expect(status.getByRole("button", { name: "Back up now" })).toBeDisabled();
  await expect(page.getByText("Snapshots appear here once the host timer is installed and has taken a backup.")).toBeVisible();
  const refused = await page.request.post("/api/settings/panel-backup/actions", { data: { action: "backup" }, headers: { Origin: new URL(page.url()).origin } });
  expect(refused.status()).toBe(409);

  const form = page.locator("form", { has: page.getByRole("heading", { name: "Panel backup schedule", exact: true }) });
  await form.getByLabel("Keep at least (snapshots)").fill("101");
  await form.getByRole("button", { name: "Save changes" }).click();
  await expect(form.getByText("Enter a whole number from 1 to 100.")).toBeVisible();
  await expect(form.getByLabel("Keep at least (snapshots)")).toBeFocused();
  await form.getByLabel("Keep at least (snapshots)").fill("5");
  await form.getByLabel("Back up every (hours)").fill("12");
  await form.getByRole("switch", { name: "Scheduled backups" }).check({ force: true });
  await form.getByRole("button", { name: "Save changes" }).click();
  await expect(form.getByText("Saved. The host uses these settings on its next check-in.")).toBeVisible();
  await page.reload();
  await expect(form.getByLabel("Keep at least (snapshots)")).toHaveValue("5");
  await expect(form.getByLabel("Back up every (hours)")).toHaveValue("12");
  await expect(form.getByRole("switch", { name: "Scheduled backups" })).toBeChecked();
  const saved = await (await page.request.get("/api/settings/panel-backup")).json() as { config: Record<string, unknown>; password_set: boolean };
  expect(saved.config).toEqual({ enabled: true, interval_hours: 12, keep_last: 5, keep_days: 30, max_bytes: 8589934592, storage_id: "" });
  expect(saved.password_set).toBe(false);

  for (const width of [390, 768, 1440]) {
    await page.setViewportSize({ width, height: 900 });
    await expectNoHorizontalScroll(page);
    await status.scrollIntoViewIfNeeded();
    await page.screenshot({ path: testInfo.outputPath(`panel-recovery-${width}.png`), fullPage: true });
  }
  const restored = await page.request.put("/api/settings/panel-backup", { data: { enabled: false, interval_hours: 24, keep_last: 7, keep_days: 30, max_bytes: 8589934592, storage_id: "" }, headers: { Origin: new URL(page.url()).origin } });
  expect(restored.status()).toBe(200);
});

test("real session, enrollment, responsive navigation and logout", async ({ page }, testInfo) => {
  const errors: string[] = [];
  page.on("pageerror", (error) => errors.push(error.message));
  await page.goto("/nodes");
  await signIn(page);
  await expect(page.getByRole("heading", { name: "Nodes", exact: true })).toBeVisible();
  await page.getByRole("link", { name: "Add node", exact: true }).first().click();
  await expect(page.getByText("Shared nodes are not an isolation boundary")).toBeVisible();
  await page.getByLabel("Name", { exact: true }).fill("browser-test-worker");
  await page.getByLabel("SSH address").fill("192.0.2.10");
  await page.getByRole("button", { name: "Generate setup command" }).click();
  await expect(page.locator('pre[aria-label="Setup command"]')).toBeVisible();
  await page.getByRole("button", { name: "Close", exact: true }).click();
  await expect(page.getByRole("heading", { name: "browser-test-worker", exact: true })).toBeVisible();
  await expect(page.locator('pre[aria-label="Setup command"]')).toHaveCount(0);

  await page.goto("/nodes");
  await expect(page.getByText("browser-test-worker", { exact: true })).toBeVisible();
  await capture(page, testInfo, "nodes");
  await page.goto("/projects");
  await expect(page.getByRole("heading", { name: "Projects", exact: true })).toBeVisible();
  await capture(page, testInfo, "projects");

  await page.setViewportSize({ width: 390, height: 900 });
  const menu = page.getByRole("button", { name: "Menu", exact: true });
  await expect(page.getByRole("navigation", { name: "Primary" })).toHaveCount(0);
  await menu.click();
  await expect(menu).toHaveAttribute("aria-expanded", "true");
  await page.getByRole("link", { name: "Nodes", exact: true }).click();
  await expect(page.getByRole("heading", { name: "Nodes", exact: true })).toBeVisible();
  await expect(menu).toHaveAttribute("aria-expanded", "false");
  await page.setViewportSize({ width: 1440, height: 900 });

  await page.getByRole("link", { name: "browser-test-worker", exact: true }).click();
  await page.getByRole("button", { name: "Delete node", exact: true }).click();
  await page.getByRole("dialog").getByRole("button", { name: "Delete node", exact: true }).click();
  await expect(page.getByRole("heading", { name: "Nodes", exact: true })).toBeVisible();
  await expect(page.getByText("browser-test-worker", { exact: true })).toHaveCount(0);
  await page.getByRole("button", { name: "Sign out", exact: true }).click();
  await expect(page.getByLabel("Password", { exact: true })).toBeVisible();
  await page.goto("/nodes");
  await expect(page.getByLabel("Password", { exact: true })).toBeVisible();
  expect(errors).toEqual([]);
});

test("custom proxy configuration validates, applies and removes without a restart", async ({ page }, testInfo) => {
  test.setTimeout(120_000);
  await page.goto("/nodes");
  await signIn(page);
  await page.getByRole("link", { name: "ready-browser-worker", exact: true }).click();
  await page.getByRole("link", { name: "Manage proxy configuration", exact: true }).click();
  await expect(page.getByRole("heading", { name: "Proxy configuration", exact: true })).toBeVisible();
  const before = execFileSync("docker", ["inspect", "--format", "{{.Id}}|{{.State.StartedAt}}", "cove-ingress"], { encoding: "utf8" });
  await page.getByRole("button", { name: "New file", exact: true }).click();
  await page.getByLabel("File name", { exact: true }).fill("browser-fixture.yaml");
  await page.getByLabel("Configuration", { exact: true }).fill("http:\n  middlewares:\n    browser-fixture:\n      headers:\n        customRequestHeaders:\n          X-Cove-Fixture: acceptance\n");
  await page.getByRole("button", { name: "Check only", exact: true }).click();
  await expect(page.getByText("Check browser-fixture.yaml: Valid", { exact: true })).toBeVisible({ timeout: 60000 });
  await page.getByRole("button", { name: "Validate and apply", exact: true }).click();
  await expect(page.getByRole("button", { name: "Edit browser-fixture.yaml", exact: true })).toBeVisible({ timeout: 60000 });
  await expect(page.getByRole("button", { name: "Remove file", exact: true })).toBeVisible();
  await capture(page, testInfo, "proxy-config");
  await page.getByRole("button", { name: "Edit browser-fixture.yaml", exact: true }).click();
  await page.getByRole("button", { name: "Remove file", exact: true }).click();
  await page.getByRole("dialog").getByRole("button", { name: "Remove file", exact: true }).click();
  await expect(page.getByRole("button", { name: "Edit browser-fixture.yaml", exact: true })).toHaveCount(0, { timeout: 60000 });
  expect(execFileSync("docker", ["inspect", "--format", "{{.Id}}|{{.State.StartedAt}}", "cove-ingress"], { encoding: "utf8" })).toBe(before);
});

test("project, environment and resource lifecycle through the real worker", async ({ page }, testInfo) => {
  test.setTimeout(210_000);
  await page.goto("/projects");
  await signIn(page);
  await createProject(page, "Browser project");
  const resourceURL = await createResource(page, "browser-service");
  const breadcrumb = page.getByRole("navigation", { name: "Breadcrumb" });
  await expect(breadcrumb.getByRole("link", { name: "Browser project", exact: true })).toBeVisible();
  await expect(breadcrumb.getByRole("link", { name: "production", exact: true })).toBeVisible();

  const sections = page.getByRole("navigation", { name: "Resource sections" });
  await sections.getByRole("link", { name: "Resource limits", exact: true }).click();
  const limits = page.getByRole("region", { name: "Resource limits", exact: true });
  await limits.getByRole("button", { name: "Add service limit", exact: true }).click();
  await limits.getByLabel("Compose service", { exact: true }).fill("app");
  await limits.getByLabel("CPU cores", { exact: true }).fill("0.5");
  await limits.getByLabel("Memory (MiB)", { exact: true }).fill("64");
  await limits.getByRole("button", { name: "Save changes", exact: true }).click();
  await expect(limits.getByRole("button", { name: "Save changes", exact: true })).toBeDisabled();
  await page.reload();
  await expect(limits.getByLabel("CPU cores", { exact: true })).toHaveValue("0.5");
  await capture(page, testInfo, "resource-limits");
  await sections.getByRole("link", { name: "Networking", exact: true }).click();
  const networks = page.getByRole("region", { name: "Private networks" });
  await networks.getByLabel("New network name", { exact: true }).fill("Browser private network");
  await networks.getByRole("button", { name: "Create private network", exact: true }).click();
  await networks.getByRole("button", { name: "Add connection", exact: true }).click();
  await chooseOption(networks.getByLabel("Network", { exact: true }), { label: "Browser private network" });
  await networks.getByLabel("Compose service", { exact: true }).fill("app");
  await networks.getByLabel("DNS alias", { exact: true }).fill("browser-app");
  await networks.getByRole("button", { name: "Save connections", exact: true }).click();
  await expect(networks.getByText("Connections saved", { exact: true })).toBeVisible();
  await page.getByRole("link", { name: "Overview", exact: true }).click();

  await deployFromHeader(page);
  await expect(page.getByText("Succeeded", { exact: true }).first()).toBeVisible({ timeout: 60_000 });
  await expect(page.getByText("running", { exact: true }).first()).toBeVisible();
  await capture(page, testInfo, "resource");

  // Unsaved section edits are guarded and can be discarded or saved in place.
  await sections.getByRole("link", { name: "General", exact: true }).click();
  const general = page.getByRole("region", { name: "General" });
  await general.getByLabel("Name", { exact: true }).fill("browser-service-renamed");
  await expect(general.getByText("Unsaved changes", { exact: true })).toBeVisible();
  await sections.getByRole("link", { name: "Logs", exact: true }).click();
  const discard = page.getByRole("dialog", { name: "Discard unsaved changes?" });
  await expect(discard).toBeVisible();
  await discard.getByRole("button", { name: "Cancel", exact: true }).click();
  await expect(general.getByLabel("Name", { exact: true })).toHaveValue("browser-service-renamed");
  await general.getByRole("button", { name: "Discard changes", exact: true }).click();
  await expect(general.getByLabel("Name", { exact: true })).toHaveValue("browser-service");
  await expect(general.getByRole("button", { name: "Save changes", exact: true })).toBeDisabled();
  await capture(page, testInfo, "resource-general");

  await sections.getByRole("link", { name: "Preflight", exact: true }).click();
  const preflight = page.getByRole("region", { name: "Deployment preflight", exact: true });
  const beforePreflight = containers(resourceURL);
  await preflight.getByRole("button", { name: "Run preflight", exact: true }).click();
  await expect(preflight.getByText("Compose: Passed", { exact: true })).toBeVisible({ timeout: 60_000 });
  await expect(preflight.getByText("Runtime: Review", { exact: true })).toBeVisible({ timeout: 60_000 });
  expect(containers(resourceURL)).toBe(beforePreflight);
  await capture(page, testInfo, "preflight");
  await sections.getByRole("link", { name: "Change history", exact: true }).click();
  const history = page.getByRole("region", { name: "Change history", exact: true });
  await expect(history.getByText("Configuration updated", { exact: true }).first()).toBeVisible();
  await history.locator("summary").filter({ hasText: /^limits$/ }).first().click();
  await expect(history.getByText("After", { exact: true }).first()).toBeVisible();
  await capture(page, testInfo, "configuration-history");
  await sections.getByRole("link", { name: "Source", exact: true }).click();
  const source = page.getByRole("region", { name: "Source", exact: true });
  const originalCompose = await editorText(source.getByLabel("Compose file", { exact: true }));
  await source.getByRole("radio", { name: "Git repository" }).check();
  await source.getByLabel("Repository URL", { exact: true }).fill("https://github.com/example/app");
  await source.getByRole("textbox", { name: /^Watch paths/ }).fill("src/**\n!**/*.test.ts");
  await source.getByRole("button", { name: "Save changes", exact: true }).click();
  await expect(source.getByText("Unsaved changes", { exact: true })).toHaveCount(0);
  await page.reload();
  await expect(source.getByRole("textbox", { name: /^Watch paths/ })).toHaveValue("src/**\n!**/*.test.ts");
  await capture(page, testInfo, "watch-paths");
  await source.getByRole("radio", { name: "Manage in Cove" }).check();
  await source.getByLabel("Compose file", { exact: true }).fill(originalCompose);
  await source.getByRole("button", { name: "Save changes", exact: true }).click();
  await expect(source.getByText("Unsaved changes", { exact: true })).toHaveCount(0);
  await page.reload();
  await expect(source.getByRole("radio", { name: "Manage in Cove" })).toBeChecked();
  await expect.poll(() => editorText(source.getByLabel("Compose file", { exact: true }))).toBe(originalCompose);
  await expect(source.getByLabel("Compose path", { exact: true })).toHaveCount(0);
  await capture(page, testInfo, "git-managed-compose");

  await source.getByRole("textbox", { name: /^Watch paths/ }).fill("docs/**");
  await expect(source.getByText("Unsaved changes", { exact: true })).toBeVisible();
  await source.getByRole("button", { name: "Discard changes", exact: true }).click();
  await expect(source.getByRole("textbox", { name: /^Watch paths/ })).toHaveValue("src/**\n!**/*.test.ts");
  await source.getByRole("radio", { name: /^Compose file/ }).check();
  await source.getByLabel("Compose file", { exact: true }).fill(originalCompose);
  await source.getByRole("button", { name: "Save changes", exact: true }).click();
  await expect(source.getByText("Unsaved changes", { exact: true })).toHaveCount(0);

  // Adding a route to the running service applies it live without recreating or restarting containers.
  const running = containers(resourceURL);
  expect(running).toMatch(/\|running\|$/);
  await sections.getByRole("link", { name: "Domains", exact: true }).click();
  const domains = page.getByRole("region", { name: "Domains" });
  await domains.getByRole("button", { name: "Add route", exact: true }).click();
  await domains.getByLabel("Hostname", { exact: true }).fill("Bad host");
  await domains.getByLabel("Compose service", { exact: true }).fill("app");
  await domains.getByRole("button", { name: "Save and apply", exact: true }).click();
  await expect(domains.getByText(/Use a DNS hostname/)).toBeVisible();
  await expect(domains.getByLabel("Hostname", { exact: true })).toBeFocused();
  const hostname = `browser-${Date.now()}.cove.test`;
  await domains.getByLabel("Hostname", { exact: true }).fill(hostname);
  await domains.getByRole("button", { name: "Save and apply", exact: true }).click();
  await expect(domains.getByText("Routes applied", { exact: true })).toBeVisible({ timeout: 60_000 });
  await expect(domains.getByText("Unsaved changes", { exact: true })).toHaveCount(0);
  expect(containers(resourceURL)).toBe(running.replace(/\|$/gm, "|routed"));
  expect(proxyRouters()).toContain(hostname);
  await capture(page, testInfo, "resource-domains");
  await page.reload();
  await expect(domains.getByLabel("Hostname", { exact: true })).toHaveValue(hostname);

  await breadcrumb.getByRole("link", { name: "Browser project", exact: true }).click();
  await expect(page.getByRole("list", { name: "Environments in Browser project" }).getByRole("link", { name: /production/ })).toBeVisible();
  await capture(page, testInfo, "project-browser");
  const createEnvironment = async (environment: string) => {
    await page.getByRole("button", { name: "New environment", exact: true }).click();
    const dialog = page.getByRole("dialog", { name: "New environment in Browser project" });
    await dialog.getByLabel("Environment name", { exact: true }).fill(environment);
    await dialog.getByRole("button", { name: "Create environment", exact: true }).click();
    await expect(page.getByRole("heading", { name: `Resources in ${environment}` })).toBeVisible();
  };
  await createEnvironment("Disposable");
  // Environment actions live in a keyboard-operable menu.
  const environmentMenu = page.getByRole("button", { name: "Disposable environment actions", exact: true });
  for (const width of [320, 390, 768, 1440]) {
    await page.setViewportSize({ width, height: 700 });
    for (const trigger of [environmentMenu, page.getByRole("button", { name: "Project actions", exact: true })]) {
      await trigger.click();
      const menu = page.getByRole("menu");
      await expect(menu).toBeVisible();
      const assertInsideViewport = async () => {
        await expect.poll(async () => {
          const bounds = await menu.boundingBox();
          return bounds !== null && bounds.x >= 8 && bounds.y >= 8
            && bounds.x + bounds.width <= page.viewportSize()!.width - 8
            && bounds.y + bounds.height <= page.viewportSize()!.height - 8;
        }).toBe(true);
        const bounds = await menu.boundingBox();
        expect(bounds).not.toBeNull();
        expect(bounds!.x).toBeGreaterThanOrEqual(8);
        expect(bounds!.y).toBeGreaterThanOrEqual(8);
        expect(bounds!.x + bounds!.width).toBeLessThanOrEqual(page.viewportSize()!.width - 8);
        expect(bounds!.y + bounds!.height).toBeLessThanOrEqual(page.viewportSize()!.height - 8);
        await expectNoHorizontalScroll(page);
      };
      await assertInsideViewport();
      await page.setViewportSize({ width, height: 450 });
      await expect(menu).toBeVisible();
      await assertInsideViewport();
      await page.screenshot({ path: testInfo.outputPath(`action-menu-${width}-${await trigger.getAttribute("aria-label")}.png`) });
      await page.keyboard.press("Escape");
      await expect(trigger).toBeFocused();
      await page.setViewportSize({ width, height: 700 });
    }
  }
  await page.setViewportSize({ width: 1440, height: 900 });
  await environmentMenu.focus();
  await page.keyboard.press("ArrowDown");
  await expect(page.getByRole("menuitem", { name: "Clone environment", exact: true })).toBeFocused();
  await page.keyboard.press("Escape");
  await expect(page.getByRole("menu")).toHaveCount(0);
  await expect(environmentMenu).toBeFocused();
  await page.keyboard.press("ArrowUp");
  await expect(page.getByRole("menuitem", { name: "Delete environment", exact: true })).toBeFocused();
  await page.keyboard.press("Enter");
  await page.getByRole("dialog", { name: "Delete Disposable?" }).getByRole("button", { name: "Delete environment", exact: true }).click();
  await expect(page.getByRole("link", { name: /Disposable/ })).toHaveCount(0);
  await page.getByRole("button", { name: "Project actions", exact: true }).click();
  await expect(page.getByRole("menuitem", { name: /Delete project.*Move or delete its 1 resource first/ })).toHaveAttribute("aria-disabled", "true");
  await page.keyboard.press("Escape");
  await createEnvironment("Acceptance");
  await expect(page.getByText("No resources in Acceptance yet.")).toBeVisible();
  await capture(page, testInfo, "environment");

  await page.goto(resourceURL + "/settings");
  await chooseOption(page.getByLabel("Project and environment", { exact: true }), { label: "Browser project / Project resources" });
  await page.getByRole("button", { name: "Save placement", exact: true }).click();
  await expect(page.getByText("Placement saved", { exact: true })).toBeVisible();
  await expect(breadcrumb.getByRole("link", { name: "production", exact: true })).toHaveCount(0);
  await breadcrumb.getByRole("link", { name: "Browser project", exact: true }).click();
  await expect(page.getByRole("region", { name: "Project resources", exact: true }).getByRole("link", { name: "browser-service", exact: true })).toBeVisible();
  await capture(page, testInfo, "project-resources");
  await page.goto(resourceURL + "/settings");
  await chooseOption(page.getByLabel("Project and environment", { exact: true }), { label: "Browser project / Acceptance" });
  await page.getByRole("button", { name: "Save placement", exact: true }).click();
  await expect(page.getByText("Placement saved", { exact: true })).toBeVisible();
  await expect(breadcrumb.getByRole("link", { name: "Acceptance", exact: true })).toBeVisible();

  await page.getByRole("link", { name: "Backups", exact: true }).click();
  await page.getByRole("button", { name: "Configure backups", exact: true }).click();
  await page.getByLabel("Encryption password", { exact: true }).fill("browser-backup-password-fixture");
  await page.getByLabel("Interval in hours", { exact: true }).fill("0");
  await page.getByRole("button", { name: "Save policy", exact: true }).click();
  await expect(page.getByRole("button", { name: "Back up now", exact: true })).toBeVisible();
  await page.getByRole("button", { name: "Edit policy", exact: true }).click();
  await expect(page.getByLabel("Encryption password", { exact: true })).toHaveValue("");
  await page.getByRole("button", { name: "Cancel", exact: true }).click();

  await page.getByRole("link", { name: "Operations", exact: true }).filter({ visible: true }).last().click();
  await page.getByRole("link", { name: "Deploy", exact: true }).first().click();
  await page.getByRole("button", { name: "Roll back to this release" }).click();
  await page.getByRole("button", { name: "Roll back", exact: true }).click();
  await expect(page.getByRole("heading", { name: "Rollback browser-service", exact: true })).toBeVisible();
  await expect(page.getByText("Succeeded", { exact: true }).first()).toBeVisible({ timeout: 60_000 });

  await page.goto("/tokens");
  await page.getByLabel("Name", { exact: true }).fill("browser-reader");
  await chooseOption(page.getByLabel("Resource", { exact: true }), { label: "browser-service" });
  await page.getByRole("button", { name: "Create token", exact: true }).click();
  await expect(page.getByLabel("New API token")).toBeVisible();
  await page.getByRole("button", { name: "Close", exact: true }).click();
  await expect(page.getByLabel("New API token")).toHaveCount(0);
  await page.getByRole("button", { name: "Revoke browser-reader", exact: true }).click();
  await page.getByRole("button", { name: "Revoke token", exact: true }).click();
  await expect(page.getByText("No API tokens created.")).toBeVisible();

  await page.goto(resourceURL + "/settings");
  await page.getByRole("button", { name: "Delete resource", exact: true }).click();
  await page.getByRole("dialog").getByRole("button", { name: "Delete resource", exact: true }).click();
  await expect(page.getByText("Stop the workload and resolve worker errors before deleting", { exact: true })).toBeVisible();
  await page.getByRole("button", { name: "Stop", exact: true }).click();
  await page.getByRole("dialog").getByRole("button", { name: "Stop", exact: true }).click();
  await page.getByRole("link", { name: "Overview", exact: true }).click();
  await expect(page.getByText("exited", { exact: true }).first()).toBeVisible({ timeout: 60_000 });
  await expect(page.getByText("Manually stopped, automatic deployments paused", { exact: true })).toBeVisible();
  await expect(page.getByRole("button", { name: "Deploy", exact: true })).toBeEnabled();

  // Removing the route from the stopped service updates the proxy only; the service stays stopped.
  const stopped = containers(resourceURL);
  expect(stopped).toMatch(/\|exited\|routed$/);
  await sections.getByRole("link", { name: "Domains", exact: true }).click();
  await expect(domains.getByText("This resource is stopped", { exact: true })).toBeVisible();
  await domains.getByRole("button", { name: `Remove route ${hostname}`, exact: true }).click();
  await domains.getByRole("button", { name: "Save and apply", exact: true }).click();
  await expect(domains.getByText("Routes applied", { exact: true })).toBeVisible({ timeout: 60_000 });
  expect(containers(resourceURL)).toBe(stopped);
  expect(proxyRouters()).not.toContain(hostname);
  await expect(page.getByText("Manually stopped, automatic deployments paused", { exact: true })).toBeVisible();
  await page.getByRole("navigation", { name: "Resource sections" }).getByRole("link", { name: "Settings", exact: true }).click();
  await page.getByRole("button", { name: "Delete resource", exact: true }).click();
  await page.getByRole("dialog").getByRole("button", { name: "Delete resource", exact: true }).click();
  await expect(page.getByRole("heading", { name: "All resources", exact: true })).toBeVisible();
});

test("invited member signs up with scoped access while teams stay isolated", async ({ page, browser, baseURL }, testInfo) => {
  test.setTimeout(120_000);
  await page.goto("/projects");
  await signIn(page);
  await createProject(page, "Hidden project");
  const hiddenURL = await createResource(page, "hidden-service");
  await createProject(page, "Scoped project");
  await createResource(page, "scoped-service");

  await page.goto("/team");
  const invite = page.getByRole("region", { name: "Invite someone" });
  await invite.getByLabel("Email", { exact: true }).fill("member@example.test");
  await invite.getByRole("button", { name: "Add grant", exact: true }).click();
  await chooseOption(invite.getByLabel("Scope", { exact: true }), { label: "Scoped project / production" });
  await chooseOption(invite.getByLabel("Role", { exact: true }), { label: "Viewer" });
  await invite.getByRole("button", { name: "Create invitation", exact: true }).click();
  const link = await invite.getByLabel("Invitation link", { exact: true }).inputValue();
  expect(link).toMatch(/\/invite#.+/);
  await expect(invite.getByText("Email was not sent; share this link privately")).toBeVisible();
  // The link is a bearer credential: dismiss it before any screenshot is written.
  await invite.getByRole("button", { name: "Done", exact: true }).click();
  await expect(page.getByLabel("Invitation link", { exact: true })).toHaveCount(0);
  await capture(page, testInfo, "team");

  const switcher = page.getByLabel("Team", { exact: true }).filter({ visible: true });
  const original = await switcher.getAttribute("data-value") ?? "";
  await page.getByRole("region", { name: "Create another team" }).getByLabel("Team name", { exact: true }).fill("Browser team B");
  await page.getByRole("button", { name: "Create team", exact: true }).click();
  await expect(page.getByRole("heading", { name: "Projects", exact: true })).toBeVisible();
  await expect(switcher).toHaveText("Browser team B");
  await expect(page.getByText("Scoped project")).toHaveCount(0);
  await expect(page.getByText("No projects yet")).toBeVisible();
  await page.goto(hiddenURL);
  await expect(page.getByRole("heading", { name: "Resource not found", exact: true })).toBeVisible();
  await chooseOption(switcher, original);
  await expect(page.getByRole("link", { name: "Scoped project" }).first()).toBeVisible();
  await page.goto("/team");
  await expect(page.getByText("member@example.test").first()).toBeVisible();

  const visitor = await browser.newContext({ baseURL, ignoreHTTPSErrors: true });
  const member = await visitor.newPage();
  await member.goto("/register");
  await expect(member.getByText("Registration is closed")).toBeVisible();
  await member.goto(link.replace(/^https?:\/\/[^/]+/, ""));
  await expect(member).toHaveURL(/\/invite$/);
  await member.getByRole("button", { name: "Review invitation", exact: true }).click();
  await expect(member.getByText("member@example.test")).toBeVisible();
  await member.getByLabel("Name", { exact: true }).fill("Browser Member");
  await member.getByLabel("Password", { exact: true }).fill("browser-member-password");
  await member.getByLabel("Confirm password", { exact: true }).fill("browser-member-password");
  await member.getByRole("button", { name: "Create account and join", exact: true }).click();
  await expect(member.getByRole("heading", { name: "Projects", exact: true })).toBeVisible();
  await expect(member.getByRole("link", { name: "Scoped project" }).first()).toBeVisible();
  await expect(member.getByText("Hidden project")).toHaveCount(0);
  await expect(member.getByRole("button", { name: "New project", exact: true })).toHaveCount(0);
  await expect(member.getByRole("link", { name: "Settings", exact: true })).toHaveCount(0);
  await expect(member.getByRole("link", { name: "Audit log", exact: true })).toHaveCount(0);

  await member.getByRole("link", { name: /production/ }).first().click();
  await member.getByRole("link", { name: "scoped-service", exact: true }).click();
  await expect(member.getByRole("heading", { name: "scoped-service", exact: true })).toBeVisible();
  await expect(member.getByRole("button", { name: "Deploy", exact: true })).toHaveCount(0);
  const memberSections = member.getByRole("navigation", { name: "Resource sections" });
  await memberSections.getByRole("link", { name: "General", exact: true }).click();
  await expect(member.getByText("View only", { exact: true })).toBeVisible();
  await expect(member.getByRole("button", { name: "Save changes" })).toHaveCount(0);
  await memberSections.getByRole("link", { name: "Domains", exact: true }).click();
  await expect(member.getByRole("button", { name: "Add route" })).toHaveCount(0);
  await memberSections.getByRole("link", { name: "Settings", exact: true }).click();
  await expect(member.getByText("Settings need editor access")).toBeVisible();
  await capture(member, testInfo, "member-resource");
  await member.goto(hiddenURL);
  await expect(member.getByText("Permission denied", { exact: true })).toBeVisible();
  await expect(member.getByRole("heading", { name: "hidden-service", exact: true })).toHaveCount(0);
  await member.goto("/nodes");
  await expect(member.getByRole("link", { name: "Add node", exact: true })).toHaveCount(0);

  await page.goto("/admin");
  await expect(page.getByRole("button", { name: "Open registration", exact: true })).toBeDisabled();
  await expect(page.getByText("Registration needs email delivery")).toBeVisible();
  await page.goto("/integrations");
  await page.getByLabel("App name", { exact: true }).fill("Cove browser fixture");
  await page.getByLabel("Owning organization").fill("example");
  await page.getByRole("button", { name: "Prepare GitHub App", exact: true }).click();
  await expect(page.getByRole("button", { name: "Continue to GitHub", exact: true })).toBeVisible();
  await expect(page.locator('form[action^="https://github.com/organizations/example/settings/apps/new"]')).toHaveCount(1);
  await capture(page, testInfo, "github-app-setup");
  await page.goto("/users");
  await page.getByRole("button", { name: "Disable member@example.test", exact: true }).click();
  await page.getByRole("dialog").getByRole("button", { name: "Disable user", exact: true }).click();
  await expect(page.getByRole("button", { name: "Enable member@example.test", exact: true })).toBeVisible();
  await member.reload();
  await expect(member).toHaveURL(/\/login(?:[?#]|$)/);
  await visitor.close();
});

test("template catalog creates an editable database without deploying", async ({ page }, testInfo) => {
  await page.goto("/resources/new");
  await signIn(page);
  await page.goto("/resources/new");
  await expect(page.getByRole("heading", { name: "Resource templates" })).toBeVisible();
  await capture(page, testInfo, "template-catalog");
  await page.getByLabel("Search templates").fill("no-such-template");
  await expect(page.getByText("No templates match.", { exact: false })).toBeVisible();
  await page.getByRole("button", { name: "Clear filters" }).click();
  await chooseOption(page.getByLabel("Category", { exact: true }), "Databases");
  await page.getByRole("button", { name: /PostgreSQL/ }).click();
  await expect(page.getByLabel("Database password", { exact: true })).toHaveAttribute("type", "password");
  await page.getByRole("button", { name: "Review configuration" }).click();
  await page.getByLabel("Name", { exact: true }).fill("template-postgres");
  await expect(page.getByRole("button", { name: "Create resource", exact: true })).toBeDisabled();
  await chooseOption(page.getByLabel("Project and environment", { exact: true }), { label: "Scoped project / Project resources" });
  await chooseOption(page.getByLabel("Node", { exact: true }), { label: "ready-browser-worker" });
  await expect(page.getByLabel("Compose file", { exact: true })).toContainText("image: postgres:18-alpine");
  await page.getByRole("button", { name: "Create resource", exact: true }).click();
  await expect(page.getByRole("heading", { name: "template-postgres", exact: true })).toBeVisible();
  const sections = page.getByRole("navigation", { name: "Resource sections" });
  await sections.getByRole("link", { name: "Environment variables", exact: true }).click();
  await expect(page.getByText("POSTGRES_PASSWORD", { exact: true })).toBeVisible();
  await sections.getByRole("link", { name: "Domains", exact: true }).click();
  const domains = page.getByRole("region", { name: "Domains" });
  await expect(domains.getByText("Not deployed yet", { exact: true })).toBeVisible();
  await domains.getByRole("button", { name: "Add route", exact: true }).click();
  await domains.getByLabel("Hostname", { exact: true }).fill(`template-${Date.now()}.cove.test`);
  await domains.getByLabel("Compose service", { exact: true }).fill("postgres");
  await domains.getByLabel("Container port", { exact: true }).fill("5432");
  await domains.getByRole("button", { name: "Save routes", exact: true }).click();
  await expect(domains.getByText("Routes saved", { exact: true })).toBeVisible();
  await expect(domains.getByText("They apply when this resource is first deployed.")).toBeVisible();
  await sections.getByRole("link", { name: "Operations", exact: true }).click();
  await expect(page.getByText("No operations in the recent history.")).toBeVisible();
});

test("infrastructure pages, encrypted connection settings and real node terminal", async ({ page }, testInfo) => {
  const errors: string[] = [];
  page.on("pageerror", (error) => errors.push(error.message));
  page.on("console", (message) => { if (message.type() === "error" && message.text().includes("Content Security Policy")) errors.push(message.text()); });
  await page.goto("/dashboard"); await signIn(page);
  await expect(page.getByRole("heading", { name: "Dashboard", exact: true })).toBeVisible();
  await capture(page, testInfo, "dashboard");
  await page.goto("/registries");
  await page.getByRole("button", { name: "Add registry", exact: true }).click();
  await page.getByLabel("Name", { exact: true }).fill("Acceptance registry");
  await page.getByLabel("Registry host", { exact: true }).fill("ghcr.io");
  await page.getByLabel("Username", { exact: true }).fill("fixture");
  await page.getByLabel("Password or access token", { exact: true }).fill("fixture-secret");
  await page.getByRole("button", { name: "Save", exact: true }).click();
  await expect(page.getByRole("heading", { name: "Acceptance registry", exact: true })).toBeVisible();
  await page.getByRole("button", { name: "Edit", exact: true }).click();
  await expect(page.getByLabel("Password or access token", { exact: true })).toHaveValue("");
  await page.getByRole("button", { name: "Cancel", exact: true }).click();
  await capture(page, testInfo, "registries");
  await page.goto("/storage");
  await page.getByRole("button", { name: "Add storage location", exact: true }).click();
  await page.getByLabel("Name", { exact: true }).fill("Acceptance storage");
  await chooseOption(page.getByLabel("Protocol", { exact: true }), "webdav");
  await page.getByLabel("Endpoint", { exact: true }).fill("https://files.example.com/dav");
  await page.getByLabel("Directory", { exact: true }).fill("backups");
  await page.getByLabel("Username", { exact: true }).fill("fixture");
  await page.getByLabel("Password", { exact: true }).fill("fixture-secret");
  await capture(page, testInfo, "storage-form");
  await page.getByRole("button", { name: "Save", exact: true }).click();
  await expect(page.getByRole("heading", { name: "Acceptance storage", exact: true })).toBeVisible();
  await page.goto("/notifications");
  await page.getByRole("button", { name: "Add notification", exact: true }).click();
  await page.getByLabel("Name", { exact: true }).fill("Acceptance notifications");
  await page.getByLabel("Webhook URL", { exact: true }).fill("https://notify.example.com/fixture");
  await page.getByLabel("Enable notifications", { exact: true }).uncheck();
  await page.getByRole("button", { name: "Save", exact: true }).click();
  await expect(page.getByRole("heading", { name: "Acceptance notifications", exact: true })).toBeVisible();
  await capture(page, testInfo, "notifications");
  await page.goto("/terminal");
  await chooseOption(page.getByLabel("Node", { exact: true }), { label: "ready-browser-worker" });
  await page.getByRole("button", { name: "Connect", exact: true }).click();
  const input = page.locator(".xterm-helper-textarea");
  await expect(input).toBeVisible();
  await expect(page.locator(".xterm-rows")).toContainText("~ $");
  await input.pressSequentially("printf 'cove-terminal-%s\\n' accepted");
  await input.press("Enter");
  await expect(page.locator(".xterm-screen")).toContainText("cove-terminal-accepted");
  await capture(page, testInfo, "terminal");
  await page.getByRole("button", { name: "Disconnect", exact: true }).click();
  await expect(page.getByRole("button", { name: "Connect", exact: true })).toBeEnabled();
  expect(errors).toEqual([]);
});

test("private networking, logical backup policy and ordered migration deployment", async ({ page }, testInfo) => {
  test.setTimeout(180_000);
  await page.goto("/projects");
  await signIn(page);
  await createProject(page, "Feature acceptance");
  const projectURL = page.url();
  const resourceURL = await createResource(page, "ordered-app");
  const resourceId = new URL(resourceURL).pathname.split("/")[2];
  await page.getByRole("navigation", { name: "Resource sections" }).getByRole("link", { name: "Networking", exact: true }).click();
  const privateAccess = page.getByRole("region", { name: "Tailscale", exact: true });
  await privateAccess.getByLabel("Enable Tailscale private access").check();
  await privateAccess.getByLabel("Compose service", { exact: true }).fill("app");
  await privateAccess.getByLabel("Tailnet hostname").fill("acceptance-private");
  await privateAccess.getByLabel("Tailscale auth key", { exact: true }).fill("tskey-auth-fixture-not-a-real-key");
  await privateAccess.getByRole("button", { name: "Save private access" }).click();
  await expect(privateAccess.getByText(/Saved. Deploy/)).toBeVisible();
  await expect(privateAccess.getByLabel("Tailscale auth key", { exact: true })).toHaveValue("");
  const config = await page.request.get(`/api/workloads/${resourceId}/private-network`);
  expect(await config.text()).not.toContain("tskey");
  await capture(page, testInfo, "private-networking");
  await privateAccess.getByLabel("Enable Tailscale private access").uncheck();
  const disabled = page.waitForResponse(response => response.url().endsWith("/private-network") && response.request().method() === "PUT");
  await privateAccess.getByRole("button", { name: "Save private access" }).click();
  expect((await disabled).status()).toBe(200);
  await page.getByRole("navigation", { name: "Resource sections" }).getByRole("link", { name: "Backups", exact: true }).click();
  await page.getByRole("button", { name: "Configure backups" }).click();
  await chooseOption(page.getByLabel("Backup method"), "postgresql");
  await page.getByLabel("Database Compose service").fill("app");
  await page.getByLabel("Database name", { exact: true }).fill("acceptance");
  await page.getByLabel("Encryption password").fill("fixture-backup-encryption-only");
  await page.getByLabel("Interval in hours").fill("0");
  await page.getByRole("button", { name: "Save policy" }).click();
  await expect(page.getByRole("button", { name: "Edit policy" })).toBeVisible();
  const backup = await page.request.get(`/api/workloads/${resourceId}/backup-policy`);
  expect(await backup.json()).toMatchObject({ backup: { database: { engine: "postgresql" } } });
  await page.goto(projectURL);
  await page.getByRole("link", { name: "New resource", exact: true }).first().click();
  await page.getByRole("button", { name: "Custom Compose or Git" }).click();
  await page.getByLabel("Name", { exact: true }).fill("ordered-migration");
  await chooseOption(page.getByLabel("Node", { exact: true }), { label: "ready-browser-worker" });
  await page.getByRole("radio", { name: /^Job/ }).check();
  await page.getByLabel("Job service", { exact: true }).fill("migrate");
  await page.getByLabel("Compose file", { exact: true }).fill("services:\n  migrate:\n    image: busybox:1.37\n    command: [sh, -c, 'echo migration-complete']\n");
  await page.getByRole("button", { name: "Create resource", exact: true }).click();
  await expect(page.getByRole("heading", { name: "ordered-migration", exact: true })).toBeVisible();
  const jobId = new URL(page.url()).pathname.split("/")[2];
  await page.goto(projectURL);
  await page.getByRole("button", { name: "Deploy in order" }).click();
  const plan = page.getByRole("dialog", { name: "Deploy resources in order" });
  await chooseOption(plan.getByLabel("Step 1"), { label: "ordered-migration (job)" });
  await chooseOption(plan.getByLabel("Step 2"), { label: "ordered-app" });
  await plan.getByRole("button", { name: "Save sequence", exact: true }).click();
  await expect(plan.getByText("Sequence saved.", { exact: true })).toBeVisible();
  await page.reload();
  await page.getByRole("button", { name: "Deploy in order" }).click();
  await expect(plan.getByLabel("Step 1")).toHaveAttribute("data-value", jobId ?? "");
  await expect(plan.getByLabel("Step 2")).toHaveAttribute("data-value", resourceId ?? "");
  await capture(page, testInfo, "saved-deployment-sequence");
  const queued = page.waitForResponse(response => response.url().endsWith("/deployments") && response.request().method() === "POST");
  await plan.getByRole("button", { name: "Deploy selected resources" }).click();
  const response = await queued;
  expect(response.status()).toBe(202);
  const queuedBody = await response.json() as { operation_ids: string[] };
  const ids = queuedBody.operation_ids;
  for (const id of ids) {
    await expect.poll(async () => {
      const status = await page.request.get(`/api/operations/${id}`);
      const operation = await status.json() as { state: string };
      return operation.state;
    }, { timeout: 90_000 }).toBe("succeeded");
  }
  const removed = await page.request.delete(`/api/workloads/${jobId}`, { headers: { Origin: new URL(page.url()).origin } });
  expect(removed.status()).toBe(200);
  await page.goto("/integrations");
  await page.getByText("Advanced: connect a zone manually", { exact: true }).click();
  await page.getByRole("button", { name: "Connect Cloudflare zone" }).click();
  await expect(page.getByLabel("Cloudflare zone ID")).toBeVisible();
  await capture(page, testInfo, "cloudflare-setup");
});

test("shared variables stay hidden and resolve in a real deployment", async ({ page }, testInfo) => {
  test.setTimeout(120_000);
  await page.goto("/projects"); await signIn(page);
  await createProject(page, "Shared variables project");
  const projectURL = page.url();
  const shared = page.getByRole("region", { name: "Shared variables", exact: true });
  await shared.getByRole("button", { name: "Shared variables", exact: true }).click();
  await shared.getByRole("button", { name: "Add variable", exact: true }).click();
  await shared.getByLabel("Name of variable 1", { exact: true }).fill("CHECK");
  await shared.getByLabel("Value of CHECK", { exact: true }).fill("shared-fixture-value");
  await shared.getByRole("button", { name: "Save variables", exact: true }).click();
  await expect(shared.getByRole("button", { name: "Remove", exact: true })).toBeVisible();
  await page.reload();
  await shared.getByRole("button", { name: "Shared variables", exact: true }).click();
  await expect(shared.getByText("CHECK", { exact: true })).toBeVisible();
  await expect(shared.locator('input[value="shared-fixture-value"]')).toHaveCount(0);
  await capture(page, testInfo, "shared-variables");
  const url = await createResource(page, "shared-variable-app");
  const id = new URL(url).pathname.split("/")[2];
  const config = await (await page.request.get(`/api/workloads/${id}`)).json() as { name: string; node_id: string; compose: string };
  const saved = await page.request.put(`/api/workloads/${id}`, { headers: { Origin: new URL(url).origin }, data: { name: config.name, node_id: config.node_id, compose: config.compose + "    environment:\n      CHECK: ${CHECK}\n", kind: "service", environment: { CHECK: "{{environment.CHECK}}" } } });
  expect(saved.status()).toBe(200);
  await deployFromHeader(page);
  await expect(page.getByText("Succeeded", { exact: true }).first()).toBeVisible({ timeout: 60_000 });
  const ids = execFileSync("docker", ["ps", "-q", "--filter", `label=com.docker.compose.project=cove-${id}`], { encoding: "utf8" }).trim().split(/\s+/);
  expect(ids.length).toBe(1);
  expect(execFileSync("docker", ["exec", ids[0] ?? "", "printenv", "CHECK"], { encoding: "utf8" }).trim()).toBe("shared-fixture-value");
  await page.goto(projectURL);
  await shared.getByRole("button", { name: "Shared variables", exact: true }).click();
  await shared.getByRole("button", { name: "Remove", exact: true }).click();
  await shared.getByRole("button", { name: "Save variables", exact: true }).click();
  await expect(shared.getByText("CHECK", { exact: true })).toHaveCount(0);
});

test("restore drills verify copied volume data without restarting the resource", async ({ page }, testInfo) => {
  test.setTimeout(360_000);
  await page.goto("/projects"); await signIn(page);
  await createProject(page, "Restore drill project");
  const url = await createResource(page, "restore-drill-app");
  const id = new URL(url).pathname.split("/")[2];
  const config = await (await page.request.get(`/api/workloads/${id}`)).json() as { name: string; node_id: string };
  const yaml = "services:\n  app:\n    image: busybox:1.37\n    command: [sh, -c, 'echo original > /data/marker; sleep 3600']\n    volumes: [data:/data]\n    healthcheck:\n      test: [CMD, 'true']\n      interval: 1s\nvolumes:\n  data: {}\n";
  expect((await page.request.put(`/api/workloads/${id}`, { headers: { Origin: new URL(url).origin }, data: { name: config.name, node_id: config.node_id, kind: "service", compose: yaml } })).status()).toBe(200);
  await deployFromHeader(page);
  await expect(page.getByText("Succeeded", { exact: true }).first()).toBeVisible({ timeout: 180_000 });
  await page.getByRole("navigation", { name: "Resource sections" }).getByRole("link", { name: "Backups", exact: true }).click();
  await page.getByRole("button", { name: "Configure backups", exact: true }).click();
  await page.getByLabel("Encryption password", { exact: true }).fill("fixture-restore-drill-password");
  await page.getByLabel("Restore verification interval (hours)", { exact: true }).fill("24");
  await page.getByRole("button", { name: "Save policy", exact: true }).click();
  await page.getByRole("button", { name: "Back up now", exact: true }).click();
  let backup: { id: string; state: string } | undefined;
  await expect.poll(async () => {
    const operations = await (await page.request.get(`/api/operations?workload_id=${id}`)).json() as { id: string; action: string; state: string }[];
    backup = operations.find(item => item.action === "backup");
    return backup && !["queued", "running"].includes(backup.state);
  }, { timeout: 180_000 }).toBe(true);
  const backupLog = await (await page.request.get(`/api/operations/${backup?.id}/logs`)).text();
  expect(backup?.state, backupLog).toBe("succeeded");
  const verify = page.getByRole("button", { name: "Verify restore", exact: true });
  await expect(verify).toBeEnabled({ timeout: 180_000 });
  const before = containers(url);
  const container = before.split("|")[0] ?? "";
  execFileSync("docker", ["exec", container, "sh", "-c", "echo changed > /data/marker"]);
  await verify.click();
  await expect(page.getByText(/File restore verified/)).toBeVisible({ timeout: 180_000 });
  expect(containers(url)).toBe(before);
  expect(execFileSync("docker", ["exec", container, "cat", "/data/marker"], { encoding: "utf8" }).trim()).toBe("changed");
  await capture(page, testInfo, "restore-drill");
  await page.getByRole("button", { name: "Restore as new resource", exact: true }).click();
  await page.getByLabel("New resource name", { exact: true }).fill("Restored separate resource");
  await page.getByRole("button", { name: "Restore separate resource", exact: true }).click();
  const progress = page.getByRole("link", { name: "View restore progress", exact: true });
  await expect(progress).toBeVisible();
  const destination = await page.getByRole("link", { name: "Open new resource", exact: true }).getAttribute("href");
  const targetID = destination?.split("/")[2] ?? "";
  const operationURL = await progress.getAttribute("href");
  const operationID = operationURL?.split("/")[2];
  await progress.click();
  await expect.poll(async () => (await (await page.request.get(`/api/operations/${operationID}`)).json() as { state: string }).state, { timeout: 180_000 }).toMatch(/succeeded|failed/);
  const result = await (await page.request.get(`/api/operations/${operationID}`)).json() as { state: string };
  expect(result.state, await (await page.request.get(`/api/operations/${operationID}/logs`)).text()).toBe("succeeded");
  const volumes = execFileSync("docker", ["volume", "ls", "-q", "--filter", `label=dev.cove.restore=${targetID}`], { encoding: "utf8" }).trim().split(/\s+/);
  expect(volumes).toHaveLength(1);
  expect(execFileSync("docker", ["run", "--rm", "--network", "none", "--mount", `type=volume,source=${volumes[0]},target=/data,readonly`, "busybox:1.37", "cat", "/data/marker"], { encoding: "utf8" }).trim()).toBe("original");
  expect(execFileSync("docker", ["ps", "-aq", "--filter", `label=com.docker.compose.project=cove-${targetID}`], { encoding: "utf8" }).trim()).toBe("");
  expect(containers(url)).toBe(before);
  expect(execFileSync("docker", ["exec", container, "cat", "/data/marker"], { encoding: "utf8" }).trim()).toBe("changed");
  await capture(page, testInfo, "restore-copy");

});

// The harness worker's normal Docker volumes sit on a filesystem without snapshot support, so this covers the stopped fallback.
test("filesystem snapshot check and backup fall back to the stopped method on an unsupported volume", async ({ page }, testInfo) => {
  test.setTimeout(360_000);
  await page.goto("/projects"); await signIn(page);
  await createProject(page, "Snapshot fallback project");
  const url = await createResource(page, "snapshot-fallback-app");
  const id = new URL(url).pathname.split("/")[2];
  const origin = new URL(url).origin;
  const config = await (await page.request.get(`/api/workloads/${id}`)).json() as { name: string; node_id: string };
  const yaml = "services:\n  app:\n    image: busybox:1.37\n    command: [sh, -c, 'echo original > /data/marker; sleep 3600']\n    volumes: [data:/data]\n    healthcheck:\n      test: [CMD, 'true']\n      interval: 1s\nvolumes:\n  data: {}\n";
  expect((await page.request.put(`/api/workloads/${id}`, { headers: { Origin: origin }, data: { name: config.name, node_id: config.node_id, kind: "service", compose: yaml } })).status()).toBe(200);
  await deployFromHeader(page);
  await expect(page.getByText("Succeeded", { exact: true }).first()).toBeVisible({ timeout: 180_000 });

  await page.getByRole("navigation", { name: "Resource sections" }).getByRole("link", { name: "Backups", exact: true }).click();
  await page.getByRole("button", { name: "Configure backups", exact: true }).click();
  await page.getByRole("switch", { name: "Use filesystem snapshots" }).check({ force: true });
  const space = page.getByLabel("LVM snapshot space (MiB)", { exact: true });
  await expect(space).toHaveValue("1024");
  await space.fill("32");
  await page.getByRole("button", { name: "Check volumes", exact: true }).click();
  await expect(page.getByText("Enter a whole number from 64 to 65,536 MiB.", { exact: true })).toBeVisible();
  await expect(space).toBeFocused();
  await space.fill("512");

  const previewResponse = page.waitForResponse(response => response.url().includes(`/api/workloads/${id}/backups/snapshot-preview`));
  await page.keyboard.press("Tab");
  await page.keyboard.press("Enter");
  const previewed = await previewResponse;
  expect(previewed.status(), await previewed.text()).toBe(200);
  expect(previewed.headers()["cache-control"]).toBe("no-store");
  expect(new URL(previewed.url()).searchParams.get("max_bytes")).toBe(String(512 * 1024 * 1024));
  // The reason may be per mount (unsupported filesystem or missing host tools) or host-wide (no systemd manager); either is a 200 advisory result.
  const preview = await previewed.json() as { available: boolean; reason?: string; mounts: { mount: string; available: boolean; reason?: string }[] };
  expect(preview.available).toBe(false);
  expect(preview.reason).toBeTruthy();
  expect(preview.mounts.length).toBeGreaterThan(0);
  await expect(page.getByText("Backups will use the stopped method", { exact: true })).toBeVisible();
  await expect(page.getByText(preview.reason ?? "", { exact: true })).toBeVisible();
  const checked = page.getByRole("list", { name: "Volumes checked" });
  for (const mount of preview.mounts) {
    await expect(checked).toContainText(mount.mount);
    await expect(checked).toContainText(mount.available ? "Supported" : "Not supported");
    if (mount.reason) await expect(checked).toContainText(mount.reason);
  }
  await capture(page, testInfo, "snapshot-fallback-preview");

  await page.getByLabel("Encryption password", { exact: true }).fill("fixture-snapshot-fallback-password");
  await page.getByRole("button", { name: "Save policy", exact: true }).click();
  await expect(page.getByText(/Filesystem snapshot, stopped fallback/)).toBeVisible();
  expect(await (await page.request.get(`/api/workloads/${id}/backup-policy`)).json()).toMatchObject({ backup: { volume_snapshot: { max_bytes: 512 * 1024 * 1024 } } });

  await page.getByRole("button", { name: "Back up now", exact: true }).click();
  let backup: { id: string; state: string } | undefined;
  await expect.poll(async () => {
    const operations = await (await page.request.get(`/api/operations?workload_id=${id}`)).json() as { id: string; action: string; state: string }[];
    backup = operations.find(item => item.action === "backup");
    return backup && !["queued", "running"].includes(backup.state);
  }, { timeout: 180_000 }).toBe(true);
  expect(backup?.state, await (await page.request.get(`/api/operations/${backup?.id}/logs`)).text()).toBe("succeeded");
  const records = await (await page.request.get(`/api/workloads/${id}/backups`)).json() as { backups: { volume_method?: string; fallback_reason?: string; data_snapshot?: string }[] };
  expect(records.backups[0]).toMatchObject({ volume_method: "stopped" });
  expect(records.backups[0]?.fallback_reason).toBeTruthy();
  await expect(page.getByText("Stopped containers", { exact: true })).toBeVisible({ timeout: 60_000 });
  await expect(page.getByText(`Snapshots unavailable: ${records.backups[0]?.fallback_reason ?? ""}`, { exact: true })).toBeVisible();
  await capture(page, testInfo, "snapshot-fallback-history");
});

test("Redis complete-instance backups verify, restore in place and restore as a new resource", async ({ page }, testInfo) => {
  test.setTimeout(480_000);
  await page.goto("/projects"); await signIn(page);
  await createProject(page, "Redis backup project");
  const url = await createResource(page, "redis-backup-app");
  const id = new URL(url).pathname.split("/")[2];
  const origin = new URL(url).origin;
  const config = await (await page.request.get(`/api/workloads/${id}`)).json() as { name: string; node_id: string };
  // CodeMirror input is unreliable for exact YAML, so the Compose file is saved through the API.
  const yaml = "services:\n  cache:\n    image: redis:7.4-alpine\n    volumes: [data:/data]\n    healthcheck:\n      test: [CMD, redis-cli, ping]\n      interval: 1s\nvolumes:\n  data: {}\n";
  expect((await page.request.put(`/api/workloads/${id}`, { headers: { Origin: origin }, data: { name: config.name, node_id: config.node_id, kind: "service", compose: yaml } })).status()).toBe(200);
  await deployFromHeader(page);
  await expect(page.getByText("Succeeded", { exact: true }).first()).toBeVisible({ timeout: 180_000 });
  const container = containers(url).split("|")[0] ?? "";
  const redis = (...args: string[]) => execFileSync("docker", ["exec", container, "redis-cli", ...args], { encoding: "utf8" }).trim();
  expect(redis("SET", "marker", "original")).toBe("OK");
  expect(redis("-n", "3", "SET", "other-db", "kept")).toBe("OK");

  await page.getByRole("navigation", { name: "Resource sections" }).getByRole("link", { name: "Backups", exact: true }).click();
  await page.getByRole("button", { name: "Configure backups", exact: true }).click();
  await chooseOption(page.getByLabel("Backup method"), "redis");
  await expect(page.getByLabel("Database name", { exact: true })).toHaveCount(0);
  await expect(page.getByText(/Complete instance: every logical database/)).toBeVisible();
  await page.getByLabel("Database Compose service", { exact: true }).fill("cache");
  await page.getByLabel("Encryption password", { exact: true }).fill("fixture-redis-backup-password");
  await page.getByLabel("Interval in hours", { exact: true }).fill("0");
  await capture(page, testInfo, "redis-backup-policy");
  await page.getByRole("button", { name: "Save policy", exact: true }).click();
  await expect(page.getByRole("button", { name: "Edit policy", exact: true })).toBeVisible();
  expect(await (await page.request.get(`/api/workloads/${id}/backup-policy`)).json()).toMatchObject({ backup: { database: { engine: "redis", service: "cache", name: "all" } } });
  await expect(page.getByText(/Redis complete instance \(cache\)/)).toBeVisible();

  const finished = async (action: string, after: Set<string>) => {
    let operation: { id: string; action: string; state: string } | undefined;
    await expect.poll(async () => {
      const operations = await (await page.request.get(`/api/operations?workload_id=${id}`)).json() as { id: string; action: string; state: string }[];
      operation = operations.find(item => item.action === action && !after.has(item.id));
      return operation && !["queued", "running"].includes(operation.state);
    }, { timeout: 240_000 }).toBe(true);
    expect(operation?.state, await (await page.request.get(`/api/operations/${operation?.id}/logs`)).text()).toBe("succeeded");
  };
  const seen = async () => new Set(((await (await page.request.get(`/api/operations?workload_id=${id}`)).json()) as { id: string }[]).map(item => item.id));

  let before = await seen();
  await page.getByRole("button", { name: "Back up now", exact: true }).click();
  await finished("backup", before);
  await expect(page.getByText("Redis complete instance", { exact: true })).toBeVisible({ timeout: 60_000 });
  await expect(page.getByText(/Database restore verified/)).toBeVisible();
  const backups = await (await page.request.get(`/api/workloads/${id}/backups`)).json() as { backups: { verification?: string; database?: { engine: string; name: string } }[] };
  expect(backups.backups[0]).toMatchObject({ verification: "database", database: { engine: "redis", name: "all" } });
  await capture(page, testInfo, "redis-backups");

  expect(redis("SET", "marker", "changed")).toBe("OK");
  expect(redis("-n", "3", "DEL", "other-db")).toBe("1");
  await expect(page.getByRole("button", { name: "Restore", exact: true })).toBeEnabled({ timeout: 60_000 });
  await page.getByRole("button", { name: "Restore", exact: true }).click();
  const confirm = page.getByRole("dialog", { name: "Restore Redis complete instance?" });
  await expect(confirm.getByText(/pauses this resource's containers/)).toBeVisible();
  await capture(page, testInfo, "redis-restore-confirm");
  before = await seen();
  await confirm.getByRole("button", { name: "Restore data", exact: true }).click();
  await finished("restore", before);
  const restored = containers(url).split("|")[0] ?? "";
  const redisAfter = (...args: string[]) => execFileSync("docker", ["exec", restored, "redis-cli", ...args], { encoding: "utf8" }).trim();
  await expect.poll(() => redisAfter("GET", "marker"), { timeout: 60_000 }).toBe("original");
  expect(redisAfter("-n", "3", "GET", "other-db")).toBe("kept");

  await page.goto(url);
  await page.getByRole("navigation", { name: "Resource sections" }).getByRole("link", { name: "Backups", exact: true }).click();
  await page.getByRole("button", { name: "Restore as new resource", exact: true }).first().click();
  const copy = page.getByRole("dialog", { name: "Restore as a separate resource" });
  await expect(copy.getByText(/The complete Redis instance snapshot is restored into the database service/)).toBeVisible();
  await expect(copy.getByText(/PostgreSQL ownership and grants/)).toHaveCount(0);
  await copy.getByLabel("New resource name", { exact: true }).fill("Restored Redis copy");
  await capture(page, testInfo, "redis-restore-copy-form");
  await copy.getByRole("button", { name: "Restore separate resource", exact: true }).click();
  const progress = copy.getByRole("link", { name: "View restore progress", exact: true });
  await expect(progress).toBeVisible();
  const operationID = (await progress.getAttribute("href"))?.split("/")[2];
  await expect.poll(async () => (await (await page.request.get(`/api/operations/${operationID}`)).json() as { state: string }).state, { timeout: 240_000 }).toMatch(/succeeded|failed/);
  const result = await (await page.request.get(`/api/operations/${operationID}`)).json() as { state: string };
  expect(result.state, await (await page.request.get(`/api/operations/${operationID}/logs`)).text()).toBe("succeeded");
});

test("resource bindings, hooks and promotion keep target configuration", async ({ page }, testInfo) => {
  test.setTimeout(300_000);
  await page.goto("/projects"); await signIn(page);
  await createProject(page, "Promotion project");
  const projectURL = page.url();
  const sourceURL = await createResource(page, "promotion-source");
  const sourceID = new URL(sourceURL).pathname.split("/")[2];
  const source = await (await page.request.get(`/api/workloads/${sourceID}`)).json() as { name: string; node_id: string; compose: string; project_id: string };
  const headers = { Origin: new URL(sourceURL).origin };
  expect((await page.request.put(`/api/workloads/${sourceID}`, { headers, data: { name: source.name, node_id: source.node_id, kind: "service", compose: source.compose, environment: { MODE: "source-fixture" } } })).status()).toBe(200);
  await deployFromHeader(page);
  await expect(page.getByText("Succeeded", { exact: true }).first()).toBeVisible({ timeout: 60_000 });
  const sourceContainer = containers(sourceURL).split("|")[0] ?? "";
  const sourceImage = execFileSync("docker", ["inspect", "--format", "{{.Image}}", sourceContainer], { encoding: "utf8" }).trim();
  await page.goto(projectURL);
  const targetURL = await createResource(page, "promotion-target");
  const targetID = new URL(targetURL).pathname.split("/")[2];
  const target = await (await page.request.get(`/api/workloads/${targetID}`)).json() as { name: string; node_id: string };
  await page.getByRole("navigation", { name: "Resource sections" }).getByRole("link", { name: "Environment variables", exact: true }).click();
  await page.getByLabel("Binding name", { exact: true }).fill("SOURCE_MODE");
  await chooseOption(page.getByLabel("Source resource", { exact: true }), sourceID ?? "");
  await chooseOption(page.getByLabel("Source variable", { exact: true }), "MODE");
  await page.getByRole("button", { name: "Save binding", exact: true }).click();
  await expect(page.getByText("{{resource.SOURCE_MODE}}", { exact: true })).toBeVisible();
  await capture(page, testInfo, "resource-bindings");
  const yaml = "services:\n  app:\n    image: cove-invalid-image:never-pull-this\n    command: [sh, -c, 'sleep 3600']\n    environment:\n      MODE: ${MODE}\n      REFERENCE: ${REFERENCE}\n    volumes: [data:/data]\n    healthcheck:\n      test: [CMD, 'true']\n      interval: 1s\nvolumes:\n  data: {}\n";
  expect((await page.request.put(`/api/workloads/${targetID}`, { headers, data: { name: target.name, node_id: target.node_id, kind: "service", compose: yaml, environment: { MODE: "target-fixture", REFERENCE: "{{resource.SOURCE_MODE}}" }, hooks: { before: { service: "app", command: "echo pre > /data/hook-before" }, after: { service: "app", command: "echo post > /data/hook-after" } } } })).status()).toBe(200);
  await page.goto(`${targetURL}/hooks`);
  await expect(page.getByLabel("Pre-deployment command", { exact: true })).toHaveValue("echo pre > /data/hook-before");
  await capture(page, testInfo, "deployment-hooks");
  await page.getByRole("navigation", { name: "Resource sections" }).getByRole("link", { name: "Promote release", exact: true }).click();
  await chooseOption(page.getByLabel("Source resource", { exact: true }), sourceID ?? "");
  await page.getByLabel("Source release", { exact: true }).click();
  await expect(page.getByRole("listbox").getByRole("option")).toHaveCount(2);
  await page.getByLabel("Source release", { exact: true }).press("Escape");
  await chooseOption(page.getByLabel("Source release", { exact: true }), { index: 1 });
  await capture(page, testInfo, "promotion");
  await page.getByRole("button", { name: "Promote release", exact: true }).click();
  await page.getByRole("link", { name: "View deployment", exact: true }).click();
  await expect(page.getByText("Succeeded", { exact: true }).first()).toBeVisible({ timeout: 90_000 });
  const container = containers(targetURL).split("|")[0] ?? "";
  expect(execFileSync("docker", ["inspect", "--format", "{{.Image}}", container], { encoding: "utf8" }).trim()).toBe(sourceImage);
  expect(execFileSync("docker", ["exec", container, "printenv", "MODE", "REFERENCE"], { encoding: "utf8" }).trim()).toBe("target-fixture\nsource-fixture");
  expect(execFileSync("docker", ["exec", container, "cat", "/data/hook-before", "/data/hook-after"], { encoding: "utf8" }).trim()).toBe("pre\npost");
  await page.goto(`/projects/${source.project_id}`);
  await expect(page.getByRole("region", { name: "Dependencies", exact: true }).getByText("Uses SOURCE_MODE", { exact: true })).toBeVisible();
  await capture(page, testInfo, "dependencies");
  await page.goto(`${targetURL}/troubleshooting`);
  await expect(page.getByText("No manual stop is recorded.", { exact: true })).toBeVisible();
  await capture(page, testInfo, "troubleshooting");
});

test("file secrets, build rotation and deployment approval", async ({ page }, testInfo) => {
  test.setTimeout(360_000);
  await page.goto("/projects"); await signIn(page);
  await createProject(page, "Secret project");
  const projectURL = page.url();
  const url = await createResource(page, "file-secret-app");
  const id = new URL(url).pathname.split("/")[2];
  const resource = await (await page.request.get(`/api/workloads/${id}`)).json() as { name: string; node_id: string; project_id: string };
  const headers = { Origin: new URL(url).origin };
  const yaml = `services:
  app:
    build:
      context: .
      dockerfile_inline: |
        FROM busybox:1.37
        RUN --mount=type=secret,id=token test -s /run/secrets/token && sha256sum /run/secrets/token | cut -d ' ' -f 1 > /built-checksum
    command: [sh, -c, 'sleep 3600']
    healthcheck:
      test: [CMD, 'true']
      interval: 1s
`;
  const input = { name: resource.name, node_id: resource.node_id, kind: "service", compose: yaml, environment: { TOKEN: "fixture-secret-one" } };
  expect((await page.request.put(`/api/workloads/${id}`, { headers, data: input })).status()).toBe(200);
  await page.goto(`${url}/environment`);
  await page.getByRole("button", { name: "Add file secret", exact: true }).click();
  await page.getByLabel("Secret name", { exact: true }).fill("token");
  await chooseOption(page.getByLabel("Saved variable", { exact: true }), "TOKEN");
  await page.getByLabel("Runtime services", { exact: false }).fill("app");
  await page.getByLabel("Build services", { exact: false }).fill("app");
  await page.getByRole("region", { name: "File and build secrets", exact: true }).getByRole("button", { name: "Save changes", exact: true }).click();
  await expect(page.getByText("Saved. Changes apply on the next deploy.", { exact: true })).toBeVisible();
  await capture(page, testInfo, "file-secrets");
  await page.goto(projectURL);
  await page.getByRole("button", { name: "Deployment policy", exact: true }).click();
  await page.getByLabel("Require approval for deployments and rollbacks").check();
  await page.getByRole("button", { name: "Save deployment policy", exact: true }).click();
  await expect(page.getByText("Deployment policy saved.", { exact: true })).toBeVisible();
  const deploy = async () => {
    const response = await page.request.post(`/api/workloads/${id}/operations`, { headers, data: { action: "deploy" } });
    expect(response.status()).toBe(202);
    const operation = await response.json() as { id: string };
    await page.goto(`/operations/${operation.id}`);
    await expect(page.getByRole("button", { name: "Approve deployment", exact: true })).toBeVisible();
    await page.getByRole("button", { name: "Approve deployment", exact: true }).click();
    await expect.poll(async () => (await (await page.request.get(`/api/operations/${operation.id}`)).json() as { state: string }).state, { timeout: 150_000 }).toMatch(/succeeded|failed/);
    const result = await (await page.request.get(`/api/operations/${operation.id}`)).json() as { state: string };
    if (result.state !== "succeeded") throw new Error(await (await page.request.get(`/api/operations/${operation.id}/logs`)).text());
    const container = containers(url).split("|")[0] ?? "";
    const runtime = execFileSync("docker", ["exec", container, "cat", "/run/secrets/token"], { encoding: "utf8" });
    const checksum = execFileSync("docker", ["exec", container, "cat", "/built-checksum"], { encoding: "utf8" });
    const env = execFileSync("docker", ["inspect", "--format", "{{json .Config.Env}}", container], { encoding: "utf8" });
    expect(env).not.toContain("TOKEN=");
    return { runtime, checksum };
  };
  const first = await deploy();
  expect(first.runtime).toBe("fixture-secret-one");
  expect((await page.request.put(`/api/workloads/${id}`, { headers, data: { ...input, environment: { TOKEN: "fixture-secret-two" } } })).status()).toBe(200);
  const second = await deploy();
  expect(second.runtime).toBe("fixture-secret-two");
  expect(second.checksum).not.toBe(first.checksum);
});

test("temporary SSH access reaches one service and revocation closes it", async ({ page }, testInfo) => {
  test.setTimeout(180_000);
  await page.goto("/projects"); await signIn(page);
  await createProject(page, "Access project");
  const url = await createResource(page, "access-service");
  const id = new URL(url).pathname.split("/")[2];
  const resource = await (await page.request.get(`/api/workloads/${id}`)).json() as { name: string; node_id: string };
  const headers = { Origin: new URL(url).origin };
  const yaml = `services:
  app:
    image: busybox:1.37
    command: [sh, -c, 'mkdir -p /www; echo tunnel-fixture > /www/index.html; httpd -f -p 8080 -h /www']
    healthcheck:
      test: [CMD, 'true']
      interval: 1s
`;
  expect((await page.request.put(`/api/workloads/${id}`, { headers, data: { name: resource.name, node_id: resource.node_id, kind: "service", compose: yaml } })).status()).toBe(200);
  await page.reload();
  await deployFromHeader(page);
  await expect(page.getByText("Succeeded", { exact: true }).first()).toBeVisible({ timeout: 60_000 });
  // This fixture's worker is containerized; attach it to emulate the real host's bridge reachability.
  execFileSync("docker", ["network", "connect", `cove-${id}_default`, process.env.COVE_BROWSER_WORKER ?? ""]);
  const directory = mkdtempSync(join(tmpdir(), "cove-tunnel-"));
  const identity = join(directory, "key");
  let child: ReturnType<typeof spawn> | undefined;
  let diagnostics = "";
  try {
    execFileSync("ssh-keygen", ["-q", "-t", "ed25519", "-N", "", "-f", identity]);
    await page.goto(`${url}/networking`);
    await page.getByLabel("Database service", { exact: true }).fill("app");
    await page.getByLabel("Container port", { exact: true }).fill("8080");
    await page.getByLabel("Ed25519 public key", { exact: true }).fill(readFileSync(identity + ".pub", "utf8"));
    const responsePromise = page.waitForResponse(response => response.url().endsWith("/database-access") && response.request().method() === "POST");
    await page.getByRole("button", { name: "Open temporary access", exact: true }).click();
    const response = await responsePromise;
    expect(response.status()).toBe(201);
    const lease = await response.json() as { id: string; host_key: string; expires_at: string };
    await capture(page, testInfo, "database-access");
    const reservation = createServer();
    await new Promise<void>(resolve => reservation.listen(0, "127.0.0.1", resolve));
    const address = reservation.address();
    if (!address || typeof address === "string") throw new Error("No local fixture port");
    const localPort = address.port;
    await new Promise<void>((resolve, reject) => reservation.close(error => error ? reject(error) : resolve()));
    child = spawn(process.env.COVE_BROWSER_CLIENT ?? "", ["connect", "--host", "127.0.0.1", "--ssh-port", process.env.COVE_BROWSER_SSH_PORT ?? "", "--local-port", String(localPort), "--identity", identity, "--host-key", lease.host_key.trim(), "--expires", lease.expires_at]);
    child.stderr?.on("data", (data: Buffer) => { diagnostics += data.toString(); });
    await expect.poll(async () => {
      try { return await (await fetch(`http://127.0.0.1:${localPort}`, { signal: AbortSignal.timeout(2000) })).text(); } catch { return diagnostics; }
    }, { timeout: 20000 }).toContain("tunnel-fixture");
    await page.getByRole("button", { name: "Revoke access", exact: true }).click();
    await expect(page.getByRole("button", { name: "Revoke access", exact: true })).toHaveCount(0);
    await expect.poll(async () => {
      try { await fetch(`http://127.0.0.1:${localPort}`, { signal: AbortSignal.timeout(2000) }); return false; } catch { return true; }
    }).toBe(true);
  } finally {
    if (child && child.exitCode === null) {
      child.kill();
      await new Promise<void>(resolve => child?.once("exit", () => resolve()));
    }
    rmSync(directory, { recursive: true, force: true });
    execFileSync("docker", ["network", "disconnect", `cove-${id}_default`, process.env.COVE_BROWSER_WORKER ?? ""]);
  }
});

test("guarded deployment rejects an unhealthy candidate without replacing the app", async ({ page }, testInfo) => {
  test.setTimeout(180_000);
  await page.goto("/projects"); await signIn(page);
  await createProject(page, "Guarded project");
  const url = await createResource(page, "guarded-app");
  const id = new URL(url).pathname.split("/")[2];
  await deployFromHeader(page);
  await expect(page.getByText("Succeeded", { exact: true }).first()).toBeVisible({ timeout: 60000 });
  const original = containers(url);
  await page.goto(`${url}/deployment-safety`);
  await page.getByLabel("Enable guarded deployment", { exact: true }).check();
  await page.getByRole("button", { name: "Save changes", exact: true }).click();
  await expect(page.getByText("Saved. Changes apply on the next deploy.", { exact: true })).toBeVisible();
  await capture(page, testInfo, "deployment-safety");
  const resource = await (await page.request.get(`/api/workloads/${id}`)).json() as { name: string; node_id: string; compose: string };
  const headers = { Origin: new URL(url).origin };
  const deploy = async () => {
    const response = await page.request.post(`/api/workloads/${id}/operations`, { headers, data: { action: "deploy" } });
    expect(response.status()).toBe(202);
    const operation = await response.json() as { id: string };
    await expect.poll(async () => (await (await page.request.get(`/api/operations/${operation.id}`)).json() as { state: string }).state, { timeout: 90000 }).toMatch(/succeeded|failed/);
    return { result: await (await page.request.get(`/api/operations/${operation.id}`)).json() as { state: string }, logs: await (await page.request.get(`/api/operations/${operation.id}/logs`)).text() };
  };
  expect((await page.request.put(`/api/workloads/${id}`, { headers, data: { name: resource.name, node_id: resource.node_id, kind: "service", compose: resource.compose.replace("'true'", "'false'") } })).status()).toBe(200);
  const failed = await deploy();
  expect(failed.result.state, failed.logs).toBe("failed");
  expect(failed.logs).toContain("candidate failed");
  expect(containers(url)).toBe(original);
  expect((await page.request.put(`/api/workloads/${id}`, { headers, data: { name: resource.name, node_id: resource.node_id, kind: "service", compose: compose("guarded-success") } })).status()).toBe(200);
  const success = await deploy();
  expect(success.result.state, success.logs).toBe("succeeded");
  expect(containers(url).split("|")[0]).not.toBe(original.split("|")[0]);
});

test("preview settings keep preview variables separate and fit mobile", async ({ page }, testInfo) => {
  await page.goto("/projects"); await signIn(page);
  await createProject(page, "Preview settings project");
  const url = await createResource(page, "preview-source");
  const id = new URL(url).pathname.split("/")[2];
  const resource = await (await page.request.get(`/api/workloads/${id}`)).json() as { name: string; node_id: string };
  expect((await page.request.put(`/api/workloads/${id}`, { headers: { Origin: new URL(url).origin }, data: { name: resource.name, node_id: resource.node_id, kind: "service", source: { repository: "https://github.com/example/preview-fixture.git", branch: "main", compose_path: "compose.yaml" } } })).status()).toBe(200);
  await page.goto(`${url}/previews`);
  await page.getByLabel("Replace preview variables", { exact: true }).check();
  await page.getByLabel("Preview variables", { exact: true }).fill("PREVIEW_TOKEN=preview-fixture-only");
  await page.getByRole("button", { name: "Save preview settings", exact: true }).click();
  await expect(page.getByText("Preview settings saved.", { exact: true })).toBeVisible();
  await expect(page.getByText("Saved preview variables: PREVIEW_TOKEN", { exact: true })).toBeVisible();
  await expect(page.getByLabel("Preview variables", { exact: true })).toHaveCount(0);
  const data = await (await page.request.get(`/api/workloads/${id}/previews`)).text();
  expect(data).not.toContain("preview-fixture-only");
  expect((await (await page.request.get(`/api/workloads/${id}`)).json() as { environment_keys: string[] }).environment_keys).not.toContain("PREVIEW_TOKEN");
  await capture(page, testInfo, "preview-settings");
});

test("account sessions can revoke another browser without signing out this one", async ({ page, browser }, testInfo) => {
  await page.goto("/account");
  await signIn(page);
  const origin = new URL(page.url()).origin;
  const context = await browser.newContext({ ignoreHTTPSErrors: true, userAgent: "Cove session acceptance" });
  try {
    const other = await context.newPage();
    await other.goto(`${origin}/login`);
    await signIn(other);
    await page.reload();
    const sessions = page.getByRole("region", { name: "Active sessions", exact: true });
    await expect(sessions.getByText("This browser", { exact: true })).toBeVisible();
    const remote = sessions.getByRole("listitem").filter({ hasText: "Cove session acceptance" });
    await expect(remote).toBeVisible();
    await capture(page, testInfo, "account-sessions");
    await remote.getByRole("button", { name: "Revoke session", exact: true }).click();
    await expect(remote).toHaveCount(0);
    expect((await context.request.get(`${origin}/api/session`)).status()).toBe(401);
    expect((await page.request.get("/api/session")).status()).toBe(200);
  } finally { await context.close(); }
});

test("environment graph creates, applies and revokes a private connection on the real worker", async ({ page }, testInfo) => {
  test.setTimeout(300_000);
  await page.goto("/projects");
  await signIn(page);
  await createProject(page, "Connection graph project");
  await page.getByRole("navigation", { name: "Project sections" }).getByRole("link", { name: /production/ }).click();
  const environmentURL = page.url();
  await createResource(page, "Graph API");
  await page.goto(environmentURL);
  await createResource(page, "Graph DB");
  await page.setViewportSize({ width: 1280, height: 900 });
  await page.goto(environmentURL);
  const section = page.locator("section", { has: page.getByRole("heading", { name: "Resources in production" }) });
  await section.getByRole("button", { name: "Graph", exact: true }).click();
  await expect(section.getByRole("button", { name: "Graph", exact: true })).toHaveAttribute("aria-pressed", "true");
  // Keyboard alternative to dragging: choose the source handle, then the target node.
  await section.getByRole("button", { name: "Connect from Graph API" }).click();
  await expect(section.getByRole("status")).toContainText("Choose the target for Graph API");
  await section.getByRole("button", { name: "Connect to Graph DB" }).click();
  const dialog = page.getByRole("dialog", { name: "New private connection" });
  await expect(dialog.getByLabel("Private hostname", { exact: true })).toHaveValue("graph-db.internal");
  await chooseOption(dialog.getByLabel("Source service", { exact: true }), "app");
  await chooseOption(dialog.getByLabel("Target service", { exact: true }), "app");
  await dialog.getByLabel("Target port", { exact: true }).fill("8080");
  await dialog.getByRole("button", { name: "Create connection", exact: true }).click();
  await expect(dialog).toBeHidden();
  const edge = section.getByRole("button", { name: /Graph API · app to Graph DB · app, graph-db.internal port 8080, Active/ });
  await expect(edge).toBeVisible({ timeout: 120_000 });
  expect(execFileSync("docker", ["inspect", "--format", "{{index .Config.Labels \"cove.managed\"}}", "cove-link"], { encoding: "utf8" }).trim()).toBe("link");
  await edge.click();
  await expect(section.getByText("Both resources run on the same node; traffic stays on that node.", { exact: false })).toBeVisible();
  for (const width of [320, 390, 768, 1280, 1920]) {
    await page.setViewportSize({ width, height: 900 });
    for (const view of ["Graph", "List"]) {
      await section.getByRole("button", { name: view, exact: true }).click();
      await expectNoHorizontalScroll(page);
      await page.screenshot({ path: testInfo.outputPath(`connections-${view.toLowerCase()}-${width}.png`), fullPage: true });
    }
  }
  await page.setViewportSize({ width: 1280, height: 900 });
  await section.getByRole("button", { name: "List", exact: true }).click();
  const list = section.getByRole("list", { name: "Private connections" });
  await expect(list.getByText("graph-db.internal:8080 · TCP")).toBeVisible();
  await list.getByRole("button", { name: /Actions for Graph API/ }).click();
  await page.getByRole("menuitem", { name: "Remove connection" }).click();
  await page.getByRole("dialog", { name: "Remove this connection?" }).getByRole("button", { name: "Remove connection" }).click();
  await expect(list).toHaveCount(0, { timeout: 120_000 });
  await expect(section.getByText("No private connections.", { exact: false })).toBeVisible();
  await expect.poll(() => execFileSync("docker", ["ps", "-aq", "--filter", "name=^cove-link$"], { encoding: "utf8" }).trim(), { timeout: 90_000 }).toBe("");

  // Layout check for the documented matrix (A->DB, B->DB, B->C, D->C) with a restricted source,
  // an endpoint in another environment, a failed connection and a shared network.
  const graph = await (await page.request.get(`/api/projects/${new URL(environmentURL).pathname.split("/")[2]}/connections`)).json() as { resources: Record<string, unknown>[] };
  const base = graph.resources[0];
  const environmentID = new URL(environmentURL).pathname.split("/")[4];
  const resource = (id: string, name: string, environment = environmentID, node = "ready-browser-worker") => ({ ...base, id, name, environment_id: environment, environment: environment === environmentID ? "production" : "staging", node, services: ["app"] });
  const link = (id: string, source: string, target: string, alias: string, state = "applied", extra = {}) => ({ id, source: { id: source, name: source.toUpperCase(), service: "app", restricted: false }, target: { id: target, name: target.toUpperCase(), service: "app", restricted: false }, port: 5432, alias, protocol: "tcp", cross_node: true, state, sides: [], broader_access: [], can_edit: true, can_remove: true, can_retry: true, updated_at: new Date().toISOString(), ...extra });
  await page.route("**/api/projects/*/connections*", route => route.fulfill({ json: {
    resources: [resource("a", "A"), resource("b", "B"), resource("db", "DB", environmentID, "worker-two"), resource("d", "D", environmentID, "worker-two"), resource("c", "C", "staging-environment")],
    connections: [link("a-db", "a", "db", "db.internal"), link("b-db", "b", "db", "db.internal"), link("b-c", "b", "c", "c.internal", "pending"), link("d-c", "d", "c", "c.internal", "error", { detail: "The target has no containers yet.", broader_access: ["shared network legacy"] }),
      { ...link("x-db", "x", "db", ""), source: { restricted: true }, alias: undefined, can_edit: false }],
    shared_networks: [{ name: "legacy", resources: ["d", "c"] }],
  } }));
  for (const width of [768, 1280, 1920]) {
    await page.setViewportSize({ width, height: 900 });
    await page.goto(environmentURL);
    await expect(section.getByRole("button", { name: "Graph", exact: true })).toHaveAttribute("aria-pressed", "true");
    await section.getByRole("button", { name: /D · app to C · app, c.internal port 5432, Error/ }).click();
    await expect(section.getByText("Broader access exists")).toBeVisible();
    await expectNoHorizontalScroll(page);
    await page.screenshot({ path: testInfo.outputPath(`connections-matrix-${width}.png`), fullPage: true });
  }
  await page.unroute("**/api/projects/*/connections*");
});

test("Redis backups download as a verified RDB, import back through the panel and restore", async ({ page }, testInfo) => {
  test.setTimeout(600_000);
  await page.goto("/projects"); await signIn(page);
  await createProject(page, "Backup transfer project");
  const url = await createResource(page, "transfer-redis");
  const id = new URL(url).pathname.split("/")[2];
  const origin = new URL(url).origin;
  const config = await (await page.request.get(`/api/workloads/${id}`)).json() as { name: string; node_id: string };
  // CodeMirror input is unreliable for exact YAML, so the Compose file is saved through the API.
  const yaml = "services:\n  cache:\n    image: redis:7.4-alpine\n    volumes: [data:/data]\n    healthcheck:\n      test: [CMD, redis-cli, ping]\n      interval: 1s\nvolumes:\n  data: {}\n";
  expect((await page.request.put(`/api/workloads/${id}`, { headers: { Origin: origin }, data: { name: config.name, node_id: config.node_id, kind: "service", compose: yaml } })).status()).toBe(200);
  await page.reload();
  await deployFromHeader(page);
  await expect(page.getByText("Succeeded", { exact: true }).first()).toBeVisible({ timeout: 180_000 });
  const redis = (args: string[], input?: string) => execFileSync("docker", ["exec", "-i", containers(url).split("|")[0] ?? "", "redis-cli", ...args], { encoding: "utf8", input }).trim();
  const payload = randomBytes(24 * 1024).toString("hex");
  expect(redis(["-x", "SET", "payload"], payload)).toBe("OK");
  expect(redis(["SET", "marker", "original"])).toBe("OK");

  const finished = async (action: string, after: Set<string>) => {
    let operation: { id: string; action: string; state: string } | undefined;
    await expect.poll(async () => {
      const operations = await (await page.request.get(`/api/operations?workload_id=${id}`)).json() as { id: string; action: string; state: string }[];
      operation = operations.find(item => item.action === action && !after.has(item.id));
      return operation && !["queued", "running"].includes(operation.state);
    }, { timeout: 240_000 }).toBe(true);
    expect(operation?.state, await (await page.request.get(`/api/operations/${operation?.id}/logs`)).text()).toBe("succeeded");
  };
  const seen = async () => new Set(((await (await page.request.get(`/api/operations?workload_id=${id}`)).json()) as { id: string }[]).map(item => item.id));
  const backupIds = async () => ((await (await page.request.get(`/api/workloads/${id}/backups`)).json()) as { backups: { id: string }[] }).backups.map(item => item.id);

  // Administrators see the instance-wide transfer limit; saving sends an exact byte count.
  await page.goto("/admin");
  const transfers = page.locator("form").filter({ has: page.getByRole("heading", { name: "Backup transfers", exact: true }) });
  const limit = transfers.getByLabel("Largest backup (MiB)", { exact: true });
  await expect(limit).toHaveValue("1024");
  await limit.fill("2048");
  await transfers.getByRole("button", { name: "Save changes", exact: true }).click();
  await expect(page.getByText("Saved. New transfers use this limit.", { exact: true })).toBeVisible();
  expect(await (await page.request.get("/api/settings/transfer-limits")).json()).toMatchObject({ backup_bytes: 2048 * 1024 * 1024 });
  await page.getByRole("heading", { name: "Backup transfers", exact: true }).scrollIntoViewIfNeeded();
  await capture(page, testInfo, "backup-transfer-settings");

  await page.goto(url);
  await page.getByRole("navigation", { name: "Resource sections" }).getByRole("link", { name: "Backups", exact: true }).click();
  await page.getByRole("button", { name: "Configure backups", exact: true }).click();
  await chooseOption(page.getByLabel("Backup method"), "redis");
  await page.getByLabel("Database Compose service", { exact: true }).fill("cache");
  await page.getByLabel("Encryption password", { exact: true }).fill("fixture-redis-transfer-password");
  await page.getByLabel("Interval in hours", { exact: true }).fill("0");
  await page.getByRole("button", { name: "Save policy", exact: true }).click();
  await expect(page.getByRole("button", { name: "Import dump", exact: true })).toBeVisible();
  let before = await seen();
  await page.getByRole("button", { name: "Back up now", exact: true }).click();
  await finished("backup", before);
  const [original] = await backupIds();
  await expect(page.getByRole("button", { name: "Download", exact: true })).toBeEnabled({ timeout: 60_000 });

  // The download is a plain link: the browser streams it to disk from a new tab.
  await page.getByRole("button", { name: "Download", exact: true }).click();
  const warning = page.getByRole("dialog", { name: "Download unencrypted backup?" });
  await expect(warning.getByText(/Redis complete instance dump/)).toBeVisible();
  await expect(warning.getByText(`cove-backup-${original}.rdb`, { exact: true })).toBeVisible();
  await capture(page, testInfo, "backup-download-warning");
  // Chromium may attribute a download that opened in a new tab to either the tab or its opener.
  const downloaded = new Promise<Download>((resolve, reject) => {
    page.once("download", resolve);
    page.context().once("page", (popup) => popup.once("download", resolve));
    setTimeout(() => reject(new Error("No download started")), 60_000);
  });
  await warning.getByRole("link", { name: "Download", exact: true }).click();
  const download = await downloaded;
  expect(download.suggestedFilename()).toBe(`cove-backup-${original}.rdb`);
  const directory = mkdtempSync(join(tmpdir(), "cove-transfer-"));
  try {
    const file = join(directory, download.suggestedFilename());
    await download.saveAs(file);
    const bytes = readFileSync(file);
    expect(bytes.length).toBeGreaterThan(32 * 1024);
    expect(bytes.subarray(0, 5).toString()).toBe("REDIS");
    const direct = await page.request.get(`/api/workloads/${id}/backups/${original}/download`);
    expect(direct.headers()["cache-control"]).toBe("no-store");
    expect(createHash("sha256").update(bytes).digest("hex")).toBe(direct.headers()["x-cove-sha256"]);

    expect(redis(["SET", "marker", "changed"])).toBe("OK");
    expect(redis(["DEL", "payload"])).toBe("1");

    // An invalid dump publishes nothing and leaves the running database alone.
    await page.getByRole("button", { name: "Import dump", exact: true }).click();
    const dialog = page.getByRole("dialog", { name: "Import database dump" });
    await expect(dialog.getByText("Accepted file: Redis RDB snapshot", { exact: true })).toBeVisible();
    const input = dialog.getByLabel("Dump file", { exact: true });
    await expect(input).toBeEnabled();
    await input.setInputFiles({ name: "not-a-dump.rdb", mimeType: "application/octet-stream", buffer: Buffer.from("this is not an RDB snapshot\n".repeat(64)) });
    await dialog.getByRole("button", { name: "Import and verify", exact: true }).click();
    await expect(dialog.getByText("Import failed", { exact: true })).toBeVisible({ timeout: 240_000 });
    await capture(page, testInfo, "backup-import-failed");
    expect(await backupIds()).toEqual([original]);
    expect(redis(["GET", "marker"])).toBe("changed");

    await input.setInputFiles(file);
    await dialog.getByRole("button", { name: "Import and verify", exact: true }).click();
    await expect(dialog.getByText("Uploading and verifying", { exact: true })).toBeVisible();
    await expect(dialog.getByText("Verified backup stored", { exact: true })).toBeVisible({ timeout: 240_000 });
    await expect(dialog.getByText(createHash("sha256").update(bytes).digest("hex"), { exact: true })).toBeVisible();
    expect(redis(["GET", "marker"])).toBe("changed");
    await expect(dialog.getByRole("button", { name: "Restore this backup…", exact: true })).toBeVisible({ timeout: 60_000 });
    await capture(page, testInfo, "backup-import-verified");
    const ids = await backupIds();
    expect(ids).toHaveLength(2);
    const imported = ids.find(item => item !== original);

    await dialog.getByRole("button", { name: "Restore this backup…", exact: true }).click();
    const confirm = page.getByRole("dialog", { name: "Restore Redis complete instance?" });
    await expect(confirm.getByText(/saves a safety backup/)).toBeVisible();
    before = await seen();
    const restoreRequest = page.waitForRequest((request) => request.method() === "POST" && request.url().endsWith(`/api/workloads/${id}/operations`));
    await confirm.getByRole("button", { name: "Restore data", exact: true }).click();
    expect((await restoreRequest).postDataJSON()).toEqual({ action: "restore", backup_id: imported });
    await finished("restore", before);
    await expect.poll(() => redis(["GET", "marker"]), { timeout: 60_000 }).toBe("original");
    expect(redis(["GET", "payload"])).toBe(payload);
    await page.reload();
    await capture(page, testInfo, "backup-transfer-list");
  } finally {
    rmSync(directory, { recursive: true, force: true });
  }
});

test("administrators browse, edit, download and delete container files on the real worker", async ({ page }, testInfo) => {
  test.setTimeout(180_000);
  await page.goto("/projects");
  await signIn(page);
  await createProject(page, "Files project");
  const url = await createResource(page, "files-service");
  // This test covers Files, not Compose editor input, so store the exact Compose text before deploying.
  const id = new URL(url).pathname.split("/")[2];
  const resource = await (await page.request.get(`/api/workloads/${id}`)).json() as { name: string; node_id: string };
  expect((await page.request.put(`/api/workloads/${id}`, { headers: { Origin: new URL(url).origin }, data: { name: resource.name, node_id: resource.node_id, kind: "service", compose: compose("files-service") } })).status()).toBe(200);
  await page.reload();
  await deployFromHeader(page);
  await expect(page.getByText("Succeeded", { exact: true }).first()).toBeVisible({ timeout: 60_000 });

  await page.getByRole("navigation", { name: "Resource sections" }).getByRole("link", { name: "Files", exact: true }).click();
  const files = page.getByRole("region", { name: "Files", exact: true });
  await chooseOption(files.getByLabel("Container or volume", { exact: true }), { index: 1 });
  const root = files.getByRole("list", { name: "Contents of /" });
  await expect(root.getByRole("button", { name: "tmp", exact: true })).toBeVisible({ timeout: 30_000 });
  await expect(root.getByRole("button", { name: "proc", exact: true })).toHaveCount(0);
  await root.getByRole("button", { name: "tmp", exact: true }).click();
  await expect(files.getByRole("list", { name: "Contents of /tmp" }).or(files.getByText("This folder is empty.", { exact: true }))).toBeVisible();

  await files.getByRole("button", { name: "New file", exact: true }).click();
  const create = page.getByRole("dialog", { name: "New file" });
  await create.getByLabel("File name", { exact: true }).fill("cove-browser.txt");
  await create.getByRole("button", { name: "Create file", exact: true }).click();
  const editor = page.getByLabel("Contents of /tmp/cove-browser.txt", { exact: true });
  await expect(editor).toHaveValue("");
  await editor.fill("hello from the browser\n");
  await page.getByRole("button", { name: "Save file", exact: true }).click();
  await expect(page.getByText("Saved.", { exact: true })).toBeVisible({ timeout: 30_000 });
  await editor.fill("second revision\n");
  await page.getByRole("button", { name: "Save file", exact: true }).click();
  await expect(page.getByText("Saved.", { exact: true })).toBeVisible({ timeout: 30_000 });
  await capture(page, testInfo, "resource-files");

  const download = page.waitForEvent("download");
  await page.getByRole("button", { name: "Download", exact: true }).click();
  const saved = await download;
  expect(saved.suggestedFilename()).toBe("cove-browser.txt");
  expect(readFileSync(await saved.path(), "utf8")).toBe("second revision\n");
  expect(page.url()).not.toContain("cove-browser");
  expect(await page.evaluate(() => JSON.stringify({ ...window.localStorage }) + JSON.stringify({ ...window.sessionStorage }))).not.toContain("second revision");

  await page.getByRole("button", { name: "Delete cove-browser.txt", exact: true }).click();
  const remove = page.getByRole("dialog", { name: "Delete cove-browser.txt?" });
  await expect(remove).toContainText("/tmp/cove-browser.txt");
  await remove.getByRole("button", { name: "Delete file", exact: true }).click();
  await expect(page.getByText("Deleted cove-browser.txt.", { exact: true })).toBeVisible({ timeout: 30_000 });
  await expect(page.getByLabel("Contents of /tmp/cove-browser.txt", { exact: true })).toHaveCount(0);
});

test("alert observations inspect a real service and certificate and container rules target it", async ({ page }, testInfo) => {
  test.setTimeout(240_000);
  await page.goto("/projects");
  await signIn(page);
  await createProject(page, "Alerts project");
  const url = await createResource(page, "alerts-service");
  const id = new URL(url).pathname.split("/")[2];
  const hostname = `alerts-${Date.now()}.cove.test`;
  const resource = await (await page.request.get(`/api/workloads/${id}`)).json() as { name: string; node_id: string };
  // Store exact Compose text and a route through the API; this test covers alerting, not editor input.
  const restarting = "services:\n  app:\n    image: busybox:1.37\n    command: [sh, -c, 'echo alerts; sleep 300; echo stopped']\n    restart: unless-stopped\n    healthcheck:\n      test: [CMD, 'true']\n      interval: 1s\n";
  expect((await page.request.put(`/api/workloads/${id}`, { headers: { Origin: new URL(url).origin }, data: { name: resource.name, node_id: resource.node_id, kind: "service", compose: restarting, routes: [{ domain: hostname, service: "app", port: 8080 }] } })).status()).toBe(200);
  await page.reload();
  await deployFromHeader(page);
  await expect(page.getByText("Succeeded", { exact: true }).first()).toBeVisible({ timeout: 90_000 });
  const container = containers(url).split("|")[0] ?? "";
  // Ending the main process outside a Cove operation makes Docker's restart policy restart the container.
  execFileSync("docker", ["exec", container, "pkill", "sleep"]);
  await expect.poll(() => execFileSync("docker", ["inspect", "--format", "{{.RestartCount}}", container], { encoding: "utf8" }).trim(), { timeout: 30_000 }).toBe("1");

  await page.goto(`${url}/troubleshooting`);
  const observations = page.getByRole("region", { name: "Alert observations", exact: true });
  await expect(observations.getByText(/Not inspected yet/)).toBeVisible();
  await observations.getByRole("button", { name: "Inspect", exact: true }).click();
  await expect(observations.getByText(/^Inspected /)).toBeVisible({ timeout: 30_000 });
  await expect(observations.getByRole("list", { name: "Inspected containers" }).getByText("Docker restart count 1", { exact: true })).toBeVisible();
  await expect(observations.getByText("Automatic restarts", { exact: true })).toBeVisible();
  const certificate = observations.getByRole("list", { name: "Inspected certificates" }).getByRole("listitem").filter({ hasText: hostname });
  await expect(certificate.getByText(/^Unknown\./).or(certificate.getByText(/when inspected$/))).toBeVisible();
  await observations.getByRole("button", { name: "Inspect again", exact: true }).click();
  await expect(observations.getByRole("button", { name: "Inspect again", exact: true })).toBeEnabled({ timeout: 30_000 });
  await capture(page, testInfo, "alert-observations");

  await page.goto("/notifications");
  await page.getByRole("button", { name: "Add notification", exact: true }).click();
  await page.getByLabel("Name", { exact: true }).fill("Alert fixture webhook");
  await page.getByLabel("Webhook URL", { exact: true }).fill("https://notify.example.com/alerts-fixture");
  await page.getByLabel("Enable notifications", { exact: true }).uncheck();
  await page.getByRole("button", { name: "Save", exact: true }).click();
  await expect(page.getByRole("heading", { name: "Alert fixture webhook", exact: true })).toBeVisible();
  const rules = page.getByRole("region", { name: "Threshold rules", exact: true });
  await rules.getByRole("button", { name: "New rule", exact: true }).click();
  await rules.getByLabel("Name", { exact: true }).fill("Certificate expiring");
  await chooseOption(rules.getByLabel("Metric", { exact: true }), "certificate");
  await chooseOption(rules.getByLabel("Service", { exact: true }), id ?? "");
  await chooseOption(rules.getByLabel("Domain", { exact: true }), hostname);
  await rules.getByLabel("Threshold (days)", { exact: true }).fill("14");
  await rules.getByRole("checkbox", { name: /Alert fixture webhook/ }).check();
  await capture(page, testInfo, "certificate-rule-form");
  await rules.getByRole("button", { name: "Create rule", exact: true }).click();
  const list = rules.getByRole("list", { name: "Threshold rules" });
  await expect(list.getByText(new RegExp(String.raw`^Certificate expiry for ${hostname.replaceAll(".", String.raw`\.`)} at or below 14 days for 5 minutes on alerts-service \(Alerts project / [^)]+\) · repeats at most every 1 hour$`))).toBeVisible();
  await rules.getByRole("button", { name: "New rule", exact: true }).click();
  await rules.getByLabel("Name", { exact: true }).fill("Restart loop");
  await chooseOption(rules.getByLabel("Metric", { exact: true }), "restarts");
  await chooseOption(rules.getByLabel("Service", { exact: true }), id ?? "");
  await rules.getByLabel("Threshold (events)", { exact: true }).fill("2.5");
  await rules.getByRole("checkbox", { name: /Alert fixture webhook/ }).check();
  await rules.getByRole("button", { name: "Create rule", exact: true }).click();
  await expect(rules.getByText("Enter a whole number from 1 to 1000.", { exact: true })).toBeVisible();
  await rules.getByLabel("Threshold (events)", { exact: true }).fill("3");
  await rules.getByRole("button", { name: "Create rule", exact: true }).click();
  await expect(list.getByText(/^Container restarts at or above 3 in ten minutes for 5 minutes on alerts-service \(Alerts project \/ [^)]+\) · repeats at most every 1 hour$/)).toBeVisible();
  await capture(page, testInfo, "resource-threshold-rules");
  for (const name of ["Certificate expiring", "Restart loop"]) {
    await rules.getByRole("button", { name: `Delete ${name}`, exact: true }).click();
    await page.getByRole("dialog", { name: `Delete ${name}?` }).getByRole("button", { name: "Delete rule", exact: true }).click();
    await expect(rules.getByText(name, { exact: true })).toHaveCount(0);
  }
});

test("merged live logs, capacity before deploy, right-sizing and cost estimates on the real worker", async ({ page }, testInfo) => {
  test.setTimeout(240_000);
  await page.goto("/projects");
  await signIn(page);
  await createProject(page, "Operations project");
  const url = await createResource(page, "operations-service");
  const id = new URL(url).pathname.split("/")[2];
  const resource = await (await page.request.get(`/api/workloads/${id}`)).json() as { name: string; node_id: string; project_id: string };
  // Exact Compose text through the API; the sidecar has no limits, so capacity cannot be fully confirmed.
  const services = "services:\n  app:\n    image: busybox:1.37\n    command: [sh, -c, 'while true; do echo app-tick; sleep 1; done']\n  sidecar:\n    image: busybox:1.37\n    command: [sh, -c, 'while true; do echo sidecar-tick; sleep 1; done']\n";
  expect((await page.request.put(`/api/workloads/${id}`, { headers: { Origin: new URL(url).origin }, data: { name: resource.name, node_id: resource.node_id, kind: "service", compose: services, limits: { app: { cpus: 0.1, memory_mb: 32 } } } })).status()).toBe(200);
  await page.reload();

  await page.getByRole("button", { name: "Deploy", exact: true }).click();
  const capacity = page.getByRole("dialog", { name: "Capacity before deploy" });
  const proceed = capacity.getByRole("button", { name: /^Deploy (anyway|without check)$/ });
  await expect(proceed).toBeVisible({ timeout: 60_000 });
  await expect(capacity.getByRole("button", { name: "Cancel", exact: true })).toBeVisible();
  await capture(page, testInfo, "capacity-before-deploy");
  await proceed.click();
  await expect(page.getByText("Succeeded", { exact: true }).first()).toBeVisible({ timeout: 90_000 });

  await page.goto(`${url}/logs`);
  const live = page.getByRole("region", { name: "Live log lines", exact: true });
  await expect(live.getByText("app-tick", { exact: true }).first()).toBeVisible({ timeout: 30_000 });
  await expect(live.getByText("sidecar-tick", { exact: true }).first()).toBeVisible({ timeout: 30_000 });
  await page.getByRole("button", { name: "Pause", exact: true }).click();
  await expect(page.getByText(/^Paused\./)).toBeVisible();
  await chooseOption(page.getByRole("combobox", { name: "Container", exact: true }).first(), { index: 1 });
  await expect(live.getByText("app-tick", { exact: true }).first().or(live.getByText("sidecar-tick", { exact: true }).first())).toBeVisible();
  await capture(page, testInfo, "live-logs");
  await page.getByRole("button", { name: "Resume", exact: true }).click();

  await page.goto(`${url}/limits`);
  await expect(page.getByRole("heading", { name: "Right-sizing suggestions", exact: true })).toBeVisible();
  await expect(page.getByText(/No per-service usage history yet|distinct hours?/).first()).toBeVisible({ timeout: 30_000 });
  await capture(page, testInfo, "right-sizing");

  await page.goto(`${url}/preflight`);
  await page.getByRole("button", { name: "Check capacity", exact: true }).click();
  await expect(page.getByText(/^Inspection snapshot\. Checks compare/)).toBeVisible({ timeout: 60_000 });

  await page.goto(`/nodes/${resource.node_id}`);
  const price = page.getByRole("region", { name: "Node price", exact: true });
  await price.getByLabel("Currency", { exact: true }).fill("eur");
  await price.getByLabel("Monthly price", { exact: true }).fill("12.505");
  await price.getByRole("button", { name: "Save price", exact: true }).click();
  await expect(price.getByText("EUR uses at most 2 decimal places.", { exact: true })).toBeVisible();
  await price.getByLabel("Monthly price", { exact: true }).fill("12.50");
  await price.getByRole("button", { name: "Save price", exact: true }).click();
  await expect(price.getByText("Price saved", { exact: true })).toBeVisible();
  await expect(price.getByText(/EUR\s12\.50/)).toBeVisible();
  await capture(page, testInfo, "node-price");

  await page.goto(`/projects/${resource.project_id}`);
  const costs = page.getByRole("region", { name: "Estimated monthly cost", exact: true });
  await expect(costs.getByText(/^Project total:/)).toBeVisible();
  await expect(costs.getByText(/EUR\s\d/).first()).toBeVisible();
  await capture(page, testInfo, "project-costs");
});

test("every page adapts to phone tablet laptop and wide desktop", async ({ page, browser }, testInfo) => {
  test.setTimeout(300_000);
  await page.goto("/projects");
  await signIn(page);
  await createProject(page, "Responsive audit project");
  const projectURL = `/projects/${new URL(page.url()).pathname.split("/")[2]}`;
  const resourceURL = new URL(await createResource(page, "Responsive audit resource")).pathname.replace(/\/$/, "");
  const resource = await (await page.request.get(`/api/workloads/${resourceURL.split("/")[2]}`)).json() as { node_id: string; environment_id: string };
  const projects = await (await page.request.get("/api/projects")).json() as { id: string; environments: { id: string }[] }[];
  const environmentID = projects.find(project => project.id === projectURL.split("/")[2])!.environments[0].id;
  await page.route("**/api/github/app", route => route.fulfill({ json: { configured: true, user_auth_configured: true, slug: "sample-app", install_url: "https://github.com/apps/sample-app/installations/new", installations: Array.from({ length: 8 }, (_, i) => ({ id: i + 1, account: `Sample-organization-${i + 1}`, team_ids: ["default"], can_manage: true, shared_with_current_team: true })) } }));
  await page.route("**/api/cloudflare/connections", route => route.fulfill({ json: [{ id: "sample-connection", name: "Sample Cloudflare account", kind: "token" }] }));
  await page.route("**/api/cloudflare/connections/sample-connection/zones", route => route.fulfill({ json: [{ id: "a".repeat(32), name: "example.com" }, { id: "b".repeat(32), name: "a-long-domain-name-for-responsive-testing.example.net" }] }));
  await page.route("**/api/cloudflare", route => route.fulfill({ json: [{ zone_id: "a".repeat(32), domain: "example.com", can_manage: true, enabled: true, proxied: false, nodes: {}, shares: [{ team_id: "default", domains: ["app.example.com", "*.staging.example.com"] }] }] }));
  const paths = ["/dashboard", "/projects", projectURL, `${projectURL}/environments/${environmentID}`, "/resources", "/resources/new", "/nodes", "/nodes/new", `/nodes/${resource.node_id}`, `/nodes/${resource.node_id}/proxy`, "/operations", "/terminal", "/integrations", "/registries", "/storage", "/notifications", "/team", "/tokens", "/audit", "/account", "/users", "/admin",
    ...["", "logs", "operations", "general", "source", "domains", "environment", "networking", "registry", "backups", "files", "settings", "limits", "configuration-history", "preflight", "deployment-safety", "previews", "hooks", "schedule", "promotion", "troubleshooting"].map(section => `${resourceURL}/${section}`)];
  const operations = await (await page.request.get("/api/operations")).json() as { id: string }[];
  if (operations[0]) paths.push(`/operations/${operations[0].id}`);
  else {
    const operation = await page.request.post(`/api/workloads/${resourceURL.split("/")[2]}/operations`, { headers: { Origin: new URL(page.url()).origin }, data: { action: "stop" } });
    expect(operation.ok()).toBe(true);
    const created = await operation.json() as { id: string };
    paths.push(`/operations/${created.id}`);
  }
  for (const width of [320, 390, 768, 1280, 1920]) {
    await page.setViewportSize({ width, height: 900 });
    for (const path of paths) {
      await page.goto(path);
      await expect(page.locator("main h1"), `Page ${path} at ${width}`).toBeVisible();
      await expect(page.getByText("Something went wrong", { exact: true })).toHaveCount(0);
      await page.waitForLoadState("networkidle");
      expect(await page.evaluate(() => document.documentElement.scrollWidth), `${path} at ${width}`).toBeLessThanOrEqual(width);
      await page.screenshot({ path: testInfo.outputPath(`responsive-${path.replaceAll("/", "_")}-${width}.png`), fullPage: true });
      if (path === "/operations" || path === "/resources/new") {
        const control = page.locator("main").getByRole("combobox").first();
        await control.focus();
        await control.press("ArrowDown");
        const list = page.getByRole("listbox");
        await expect(list).toBeVisible();
        const bounds = await list.boundingBox();
        expect(bounds!.x).toBeGreaterThanOrEqual(8);
        expect(bounds!.x + bounds!.width).toBeLessThanOrEqual(width - 8);
        expect(bounds!.y).toBeGreaterThanOrEqual(8);
        expect(bounds!.y + bounds!.height).toBeLessThanOrEqual(892);
        await page.screenshot({ path: testInfo.outputPath(`select-${path.replaceAll("/", "_")}-${width}.png`) });
        await control.press("Escape");
        await expect(list).toHaveCount(0);
        await expect(control).toBeFocused();
      }
      if (path === "/integrations") {
        await expect(page.getByRole("button", { name: "Save changes", exact: true })).toHaveCount(1);
        await page.getByLabel("Sample-organization-1: Default team", { exact: true }).uncheck();
        await expect(page.getByText("Unsaved assignment", { exact: true })).toBeVisible();
        await page.getByRole("button", { name: "Discard changes", exact: true }).click();
        await expect(page.getByLabel("Sample-organization-1: Default team", { exact: true })).toBeChecked();
        const github = await page.getByRole("region", { name: "GitHub App", exact: true }).boundingBox();
        const cloudflare = await page.getByRole("region", { name: "Cloudflare DNS", exact: true }).boundingBox();
        if (width === 1920) {
          expect(cloudflare!.x).toBeGreaterThan(github!.x + github!.width);
          expect(Math.abs(cloudflare!.y - github!.y)).toBeLessThan(2);
        }
        if (width <= 768) {
          await expect(page.getByRole("button", { name: "Menu", exact: true })).toBeVisible();
          expect(cloudflare!.y).toBeGreaterThan(github!.y);
        }
        await page.getByRole("button", { name: "Choose domains", exact: true }).click();
        await expect(page.getByLabel("example.com", { exact: true })).toBeVisible();
        await expectNoHorizontalScroll(page);
        await page.screenshot({ path: testInfo.outputPath(`cloudflare-discovery-${width}.png`), fullPage: true });
        await page.getByRole("button", { name: "Cancel", exact: true }).click();
        await page.getByRole("button", { name: "Use API token", exact: true }).click();
        await expect(page.getByLabel("Connection name", { exact: true })).toBeVisible();
        await expectNoHorizontalScroll(page);
        await page.screenshot({ path: testInfo.outputPath(`cloudflare-token-${width}.png`), fullPage: true });
        await page.getByRole("button", { name: "Cancel", exact: true }).click();
        await page.getByRole("button", { name: "Share domains", exact: true }).click();
        await expect(page.getByRole("heading", { name: "Share domains from example.com" })).toBeVisible();
        await expect(page.getByLabel("Default team", { exact: true })).toHaveValue("app.example.com\n*.staging.example.com");
        await expectNoHorizontalScroll(page);
        await page.screenshot({ path: testInfo.outputPath(`cloudflare-sharing-${width}.png`), fullPage: true });
        await page.getByRole("button", { name: "Close", exact: true }).click();
        await page.getByText("Advanced: connect a zone manually", { exact: true }).click();
        await page.getByRole("button", { name: "Connect Cloudflare zone" }).click();
        await expect(page.getByLabel("Cloudflare zone ID")).toBeVisible();
        await expectNoHorizontalScroll(page);
        await page.screenshot({ path: testInfo.outputPath(`cloudflare-form-${width}.png`), fullPage: true });
      }
      const add = { "/registries": "Add registry", "/storage": "Add storage location", "/notifications": "Add notification" }[path];
      if (add) {
        await page.getByRole("button", { name: add, exact: true }).click();
        await expectNoHorizontalScroll(page);
        await page.screenshot({ path: testInfo.outputPath(`connection-${path.slice(1)}-${width}.png`), fullPage: true });
        if (path === "/storage") {
          for (const protocol of ["sftp", "ftps", "webdav"]) {
            await chooseOption(page.getByLabel("Protocol", { exact: true }), protocol);
            await expectNoHorizontalScroll(page);
            await page.screenshot({ path: testInfo.outputPath(`storage-${protocol}-${width}.png`), fullPage: true });
          }
        }
      }
    }
  }
  const publicPage = await browser.newPage({ ignoreHTTPSErrors: true });
  try {
    for (const width of [320, 390, 768, 1280, 1920]) {
      await publicPage.setViewportSize({ width, height: 900 });
      for (const path of ["/login", "/register", "/forgot-password", "/reset-password", "/verify", "/invite"]) {
        await publicPage.goto(new URL(path, page.url()).href);
        await expect(publicPage.locator("h1")).toBeVisible();
        await expectNoHorizontalScroll(publicPage);
        await publicPage.screenshot({ path: testInfo.outputPath(`public-${path.slice(1)}-${width}.png`), fullPage: true });
      }
    }
  } finally { await publicPage.close(); }
});

test("administrator password rotation signs out every session", async ({ page }) => {
  await page.goto("/account");
  await signIn(page);
  const replacement = password + "-rotated";
  const section = page.getByRole("region", { name: "Password" });
  await section.getByLabel("Current password", { exact: true }).fill(password);
  await section.getByLabel("New password", { exact: true }).fill(replacement);
  await section.getByLabel("Confirm new password", { exact: true }).fill(replacement);
  await section.getByRole("button", { name: "Change password and sign out" }).click();
  await expect(page.getByLabel("Password", { exact: true })).toBeVisible();
  await signIn(page, replacement);
  await expect(page.getByRole("heading", { name: "Dashboard", exact: true })).toBeVisible();
  // Later spec files sign in with the provisioned password, so rotate back.
  await page.goto("/account");
  const restore = page.getByRole("region", { name: "Password" });
  await restore.getByLabel("Current password", { exact: true }).fill(replacement);
  await restore.getByLabel("New password", { exact: true }).fill(password);
  await restore.getByLabel("Confirm new password", { exact: true }).fill(password);
  await restore.getByRole("button", { name: "Change password and sign out" }).click();
  await expect(page.getByLabel("Password", { exact: true })).toBeVisible();
  await signIn(page);
  await expect(page.getByRole("heading", { name: "Dashboard", exact: true })).toBeVisible();
});

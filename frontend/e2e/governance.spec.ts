import { execFileSync } from "node:child_process";
import { expect, test, type Locator, type Page, type TestInfo } from "@playwright/test";

const password = process.env.COVE_BROWSER_PASSWORD ?? "";
const compose = "services:\n  app:\n    image: busybox:1.37\n    command: [sh, -c, 'sleep 300']\n    healthcheck:\n      test: [CMD, 'true']\n      interval: 1s\n";
const secretValues = ["governance-value-one", "governance-value-two"];

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

// Spells out characters outside printable ASCII so look-alike whitespace shows up in assertion output.
function visible(text: string) {
  return text.replace(/[^\x20-\x7e\n]/g,(character) => `<U+${(character.codePointAt(0) ?? 0).toString(16).toUpperCase().padStart(4, "0")}>`);
}

async function chooseOption(control: Locator, index: number) {
  await control.click();
  const list = control.page().getByRole("listbox");
  await expect(list).toBeVisible();
  await list.getByRole("option").nth(index).click();
}

async function capture(page: Page, testInfo: TestInfo, name: string) {
  for (const width of [390, 768, 1440]) {
    await page.setViewportSize({ width, height: 900 });
    await page.evaluate(() => window.scrollTo(0, 0));
    expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBe(true);
    await page.screenshot({ path: testInfo.outputPath(`${name}-${width}.png`), fullPage: true });
  }
  await page.setViewportSize({ width: 1440, height: 900 });
}

async function expectNoSecretValues(page: Page) {
  const text = await page.locator("body").innerText();
  for (const value of secretValues) expect(text).not.toContain(value);
}

async function settle(page: Page, operationId: string) {
  await expect.poll(async () => (await (await page.request.get(`/api/operations/${operationId}`)).json() as { state: string }).state, { timeout: 120_000 }).toMatch(/succeeded|failed/);
  const result = await (await page.request.get(`/api/operations/${operationId}`)).json() as { state: string };
  if (result.state !== "succeeded") throw new Error(await (await page.request.get(`/api/operations/${operationId}/logs`)).text());
}

test("deployment reasons, release comparison, team schedule and emergency override", async ({ page }, testInfo) => {
  test.setTimeout(300_000);
  await page.setViewportSize({ width: 1440, height: 900 });
  await page.goto("/projects");
  await expect(page).toHaveURL(/\/login(?:[?#]|$)/);
  await page.getByLabel("Password", { exact: true }).fill(password);
  await page.getByRole("button", { name: "Sign in", exact: true }).click();
  await expect(page).not.toHaveURL(/\/login(?:[?#]|$)/);

  await page.locator("main header").getByRole("button", { name: "New project", exact: true }).click();
  await page.getByLabel("Project name", { exact: true }).fill("Governance project");
  await page.getByRole("button", { name: "Create project", exact: true }).click();
  await expect(page.getByRole("heading", { name: "Governance project", exact: true })).toBeVisible();
  await page.getByRole("link", { name: "New resource", exact: true }).first().click();
  await page.getByRole("button", { name: "Custom Compose or Git" }).click();
  await page.getByLabel("Name", { exact: true }).fill("governance-app");
  await page.getByLabel("Node", { exact: true }).click();
  await page.getByRole("listbox").getByRole("option", { name: "ready-browser-worker", exact: true }).click();
  const editor = page.getByLabel("Compose file", { exact: true });
  await editor.fill(compose);
  expect(visible(await editor.evaluate((element) => Array.from(element.querySelectorAll(".cm-line"), (line) => line.textContent).join("\n")))).toBe(visible(compose));
  await page.getByRole("button", { name: "Create resource", exact: true }).click();
  await expect(page.getByRole("heading", { name: "governance-app", exact: true })).toBeVisible();
  const url = page.url();
  const id = new URL(url).pathname.split("/")[2] ?? "";
  const headers = { Origin: new URL(url).origin };
  // The Compose editor must save exactly what was typed; whitespace swapped for look-alike characters would break Compose.
  const saved = await (await page.request.get(`/api/workloads/${id}`)).json() as { compose: string };
  expect(visible(saved.compose)).toBe(visible(compose));
  const setMode = async (value: string) => {
    await page.goto(`${url}/environment`);
    const variables = page.getByRole("region", { name: "Environment variables", exact: true });
    const savedMode = variables.getByRole("list", { name: "Saved variables" }).getByText("MODE", { exact: true });
    await variables.getByRole("button", { name: "Add variable", exact: true }).click();
    await variables.getByLabel("Name of variable 1", { exact: true }).fill("MODE");
    await variables.getByLabel("Value of MODE", { exact: true }).fill(value);
    await variables.getByRole("button", { name: "Save changes", exact: true }).click();
    await expect(variables.getByText(/^Saved\./)).toBeVisible();
    await expect(savedMode).toBeVisible();
    await page.goto(url);
  };

  // The deploy dialog is reachable and submittable from the keyboard; the reason is optional.
  await setMode(secretValues[0] ?? "");
  const deployButton = page.locator("main header").getByRole("button", { name: "Deploy", exact: true });
  await deployButton.focus();
  await page.keyboard.press("Enter");
  const deployDialog = page.getByRole("dialog", { name: "Deploy governance-app?" });
  await expect(deployDialog).toBeVisible();
  await deployDialog.getByLabel("Reason").fill("Initial governance deploy");
  await capture(page, testInfo, "governance-deploy-reason");
  // Deploy stays disabled until the pending configuration review loads.
  const confirmDeploy = deployDialog.getByRole("button", { name: "Deploy", exact: true });
  await expect(confirmDeploy).toBeEnabled();
  await confirmDeploy.focus();
  await page.keyboard.press("Enter");
  await expect(deployDialog).toBeHidden();
  const latest = page.getByRole("region", { name: "Latest operation" }).getByRole("link", { name: "Deploy", exact: true });
  await expect(latest).toBeVisible({ timeout: 30_000 });
  const first = (await latest.getAttribute("href"))?.split("/").pop() ?? "";
  expect((await (await page.request.get(`/api/operations/${first}`)).json() as { reason: string }).reason).toBe("Initial governance deploy");
  await settle(page, first);

  await setMode(secretValues[1] ?? "");
  const second = await page.request.post(`/api/workloads/${id}/operations`, { headers, data: { action: "deploy", reason: "Second governance deploy" } });
  expect(second.status()).toBe(202);
  const secondOperation = await second.json() as { id: string };
  await settle(page, secondOperation.id);

  await page.goto(`${url}/operations`);
  await expect(page.getByText("Reason: Second governance deploy", { exact: true }).first()).toBeVisible();
  await page.goto(`/operations/${secondOperation.id}`);
  await expect(page.getByText("Second governance deploy", { exact: true })).toBeVisible();
  await capture(page, testInfo, "governance-operation-reason");
  await page.goto("/audit");
  await expect(page.getByText("Reason: Initial governance deploy", { exact: true }).first()).toBeVisible();
  await capture(page, testInfo, "governance-audit-reason");

  // Instance settings render the fixture's real security and update state without changing it.
  await page.goto("/admin");
  await expect(page.getByRole("heading", { name: "Updates", exact: true })).toBeVisible();
  await expect(page.getByLabel("Update channel", { exact: true })).toBeVisible({ timeout: 30_000 });
  await expect(page.getByRole("status", { name: /^Loading/ })).toHaveCount(0);
  await expect(page.getByText("Loading settings", { exact: true })).toHaveCount(0);
  await capture(page, testInfo, "governance-instance-settings");

  // Release comparison shows which settings changed, never their values.
  await page.goto(`${url}/configuration-history`);
  const comparison = page.getByRole("region", { name: "Compare releases", exact: true });
  await chooseOption(comparison.getByLabel("From release", { exact: true }), 2);
  await chooseOption(comparison.getByLabel("To release", { exact: true }), 1);
  await comparison.getByRole("button", { name: "Compare", exact: true }).focus();
  await page.keyboard.press("Enter");
  await expect(comparison.getByRole("list", { name: "Changed settings" }).getByText("Variable MODE", { exact: true })).toBeVisible({ timeout: 60_000 });
  await expect(comparison.getByRole("table", { name: "Service images" })).toBeVisible();
  // Neither release came from Git, so there is no history that could be incomplete.
  await expect(comparison.getByText("Neither release is Git-backed; there are no Git commits to compare.", { exact: true })).toBeVisible();
  await expect(comparison.getByText("Commit history is incomplete", { exact: true })).toHaveCount(0);
  await expectNoSecretValues(page);
  await capture(page, testInfo, "governance-release-comparison");

  // Team schedule: invalid rows are reported in place and unsaved; a valid approval policy saves.
  await page.goto("/team");
  const policy = page.getByRole("form", { name: "team deployment policy" });
  await policy.getByRole("button", { name: "Deployment policy", exact: true }).click();
  await policy.getByLabel("Require approval for deployments and rollbacks").check();
  await policy.getByRole("button", { name: "Add window", exact: true }).click();
  await policy.getByRole("checkbox", { name: "Monday", exact: true }).check();
  await policy.getByLabel("Window 1 starts", { exact: true }).fill("09:00");
  await policy.getByLabel("Window 1 ends", { exact: true }).fill("09:00");
  await policy.getByRole("button", { name: "Save deployment policy", exact: true }).click();
  await expect(policy.getByText("Fix the highlighted fields", { exact: true })).toBeVisible();
  await expect(policy.getByLabel("Window 1 starts", { exact: true })).toHaveAttribute("aria-invalid", "true");
  await capture(page, testInfo, "governance-team-policy-invalid");
  await policy.getByRole("button", { name: "Remove window 1", exact: true }).click();
  await policy.getByRole("button", { name: "Save deployment policy", exact: true }).click();
  await expect(policy.getByText("Deployment policy saved.", { exact: true })).toBeVisible();
  await page.reload();
  await expect(policy.getByText(/^Approval required/)).toBeVisible();

  // An approval-gated deployment can be overridden once with a required, recorded reason.
  const gated = await page.request.post(`/api/workloads/${id}/operations`, { headers, data: { action: "deploy" } });
  expect(gated.status()).toBe(202);
  const gatedOperation = await gated.json() as { id: string };
  await page.goto(`/operations/${gatedOperation.id}`);
  const overrideButton = page.getByRole("button", { name: "Emergency override", exact: true });
  await expect(overrideButton).toBeVisible();
  await expect(page.getByText("Approval required", { exact: true })).toBeVisible();
  await overrideButton.focus();
  await page.keyboard.press("Enter");
  const overrideDialog = page.getByRole("dialog", { name: "Override the deployment schedule?" });
  await expect(overrideDialog).toBeVisible();
  await page.keyboard.press("Escape");
  await expect(overrideDialog).toBeHidden();
  await overrideButton.click();
  await overrideDialog.getByRole("button", { name: "Override and allow start", exact: true }).click();
  await expect(overrideDialog.getByLabel("Emergency reason")).toHaveAttribute("aria-invalid", "true");
  await overrideDialog.getByLabel("Emergency reason").fill("Browser outage drill");
  await capture(page, testInfo, "governance-emergency-override-dialog");
  await overrideDialog.getByRole("button", { name: "Override and allow start", exact: true }).click();
  await expect(overrideDialog).toBeHidden();
  await expect(page.getByText("Emergency override recorded", { exact: true })).toBeVisible();
  await expect(page.getByText("Browser outage drill", { exact: true })).toBeVisible();
  // Until the worker picks it up the operation stays queued, but approval is no longer needed or offered.
  await expect(page.getByText("Approval required", { exact: true })).toHaveCount(0);
  await expect(page.getByRole("button", { name: "Approve deployment", exact: true })).toHaveCount(0);
  await expect(overrideButton).toHaveCount(0);
  await settle(page, gatedOperation.id);
  await capture(page, testInfo, "governance-emergency-override");

  await page.goto("/team");
  await policy.getByRole("button", { name: "Deployment policy", exact: true }).click();
  await policy.getByLabel("Require approval for deployments and rollbacks").uncheck();
  await policy.getByRole("button", { name: "Save deployment policy", exact: true }).click();
  await expect(policy.getByText("Deployment policy saved.", { exact: true })).toBeVisible();
});

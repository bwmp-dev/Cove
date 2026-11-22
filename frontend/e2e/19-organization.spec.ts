import { execFileSync } from "node:child_process";
import { expect, test, type Locator, type Page, type TestInfo } from "@playwright/test";

const password = process.env.COVE_BROWSER_PASSWORD ?? "";
const widths = [390, 768, 1440];
const compose = (marker: string) =>
  `services:\n  app:\n    image: busybox:1.37\n    command: [sh, -c, 'echo ${marker}; sleep 300']\n    healthcheck:\n      test: [CMD, 'true']\n      interval: 1s\n`;

test.afterEach(() => {
  const database = process.env.COVE_BROWSER_DATABASE;
  if (!database || !/^cove-browser-[a-f0-9]+-db$/.test(database)) throw new Error("Isolated browser database required for cleanup");
  const resources = execFileSync("docker", ["exec", database, "psql", "-U", "postgres", "-d", "cove", "-Atc", "UPDATE workloads w SET manually_stopped=true WHERE NOT EXISTS(SELECT 1 FROM operations o WHERE o.workload_id=w.id AND o.state IN ('queued','running')) RETURNING w.id"], { encoding: "utf8" }).split(/\s+/).filter((value) => /^[a-f0-9]{32}$/.test(value));
  for (const resource of resources) {
    const ids = execFileSync("docker", ["ps", "-aq", "--filter", `label=com.docker.compose.project=cove-${resource}`], { encoding: "utf8" }).split(/\s+/).filter(Boolean);
    if (ids.length) execFileSync("docker", ["rm", "-f", "-v", ...ids]);
    const networks = execFileSync("docker", ["network", "ls", "-q", "--filter", `label=com.docker.compose.project=cove-${resource}`], { encoding: "utf8" }).split(/\s+/).filter(Boolean);
    if (networks.length) execFileSync("docker", ["network", "rm", ...networks]);
  }
});

async function chooseOption(control: Locator, label: string) {
  await control.click();
  const list = control.page().getByRole("listbox");
  await expect(list).toBeVisible();
  await list.getByRole("option", { name: label, exact: true }).click();
}

async function openProjectOverview(page: Page) {
  // Creating a project with its default production environment lands on that environment.
  await expect(page).toHaveURL(/\/projects\/[^/?#]+\/environments\/[^/?#]+$/);
  await page.getByRole("navigation", { name: "Project sections" }).getByRole("link", { name: "Overview", exact: true }).click();
  await expect(page).toHaveURL(/\/projects\/[^/?#]+$/);
}

async function signIn(page: Page) {
  await expect(page).toHaveURL(/\/login(?:[?#]|$)/);
  await page.getByLabel("Password", { exact: true }).fill(password);
  await page.getByRole("button", { name: "Sign in", exact: true }).click();
  await expect(page).not.toHaveURL(/\/login(?:[?#]|$)/);
}

async function capture(page: Page, testInfo: TestInfo, name: string) {
  for (const width of widths) {
    await page.setViewportSize({ width, height: 900 });
    await page.evaluate(() => window.scrollTo(0, 0));
    expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBe(true);
    await page.screenshot({ path: testInfo.outputPath(`${name}-${width}.png`), fullPage: true });
  }
  await page.setViewportSize({ width: 1440, height: 900 });
}

async function createResource(page: Page, name: string) {
  await page.getByRole("link", { name: "New resource", exact: true }).first().click();
  await page.getByRole("button", { name: "Custom Compose or Git" }).click();
  await page.getByLabel("Name", { exact: true }).fill(name);
  await chooseOption(page.getByLabel("Node", { exact: true }), "ready-browser-worker");
  await page.getByLabel("Compose file", { exact: true }).fill(compose(name));
  await page.getByRole("button", { name: "Create resource", exact: true }).click();
  await expect(page.getByRole("heading", { name, exact: true })).toBeVisible();
  // Playwright fill types into CodeMirror with non-breaking-space indentation, so the exact YAML is saved through the API before any deploy.
  const id = new URL(page.url()).pathname.split("/")[2] ?? "";
  if (!/^[0-9a-f]{32}$/.test(id)) throw new Error("Unexpected resource URL");
  const resource = (await (await page.request.get(`/api/workloads/${id}`)).json()) as { name: string; node_id: string };
  const saved = await page.request.put(`/api/workloads/${id}`, { headers: { Origin: new URL(page.url()).origin }, data: { name: resource.name, node_id: resource.node_id, kind: "service", compose: compose(name) } });
  expect(saved.status(), await saved.text()).toBe(200);
  expect(((await (await page.request.get(`/api/workloads/${id}`)).json()) as { compose: string }).compose).toBe(compose(name));
}

test("guided setup, personal pins, tags, server-paged browser, bulk actions and activity", async ({ page }, testInfo) => {
  test.setTimeout(240_000);
  await page.goto("/dashboard");
  await signIn(page);

  // The checklist follows real team state and can be skipped and reopened without changing anything else.
  const checklist = page.getByRole("region", { name: "Get started" });
  const showChecklist = page.getByRole("button", { name: "Show setup checklist", exact: true });
  // The checklist renders nothing while its query is pending, so wait for either state before choosing.
  await expect(checklist.or(showChecklist)).toBeVisible();
  if (await showChecklist.isVisible()) await showChecklist.click();
  await expect(checklist.getByRole("list", { name: "Setup steps" })).toBeVisible();
  await expect(page.getByRole("region", { name: "Activity", exact: true })).toBeVisible();
  await capture(page, testInfo, "organization-dashboard");
  await checklist.getByRole("button", { name: /^(Skip for now|Hide)$/ }).click();
  await expect(checklist).not.toBeVisible();
  const progress = (await (await page.request.get("/api/onboarding")).json()) as { dismissed: boolean; complete: boolean };
  expect(progress.dismissed).toBe(true);
  if (!progress.complete) {
    await showChecklist.click();
    await expect(checklist).toBeVisible();
  }

  // A project with two resources to organize.
  await page.goto("/projects");
  await page.locator("main header").getByRole("button", { name: "New project", exact: true }).click();
  await page.getByLabel("Project name", { exact: true }).fill("Organization project");
  await page.getByRole("button", { name: "Create project", exact: true }).click();
  await expect(page.getByRole("heading", { name: "Organization project", exact: true })).toBeVisible();
  await openProjectOverview(page);
  await page.locator("main header").getByRole("button", { name: "Pin Organization project", exact: true }).click();
  await expect(page.locator("main header").getByRole("button", { name: "Unpin Organization project", exact: true })).toBeVisible();
  await createResource(page, "organize-web");
  await page.goto("/projects");
  await page.getByRole("link", { name: "Organization project" }).first().click();
  await createResource(page, "organize-api");
  await page.locator("main header").getByRole("button", { name: "Pin organize-api", exact: true }).click();
  await expect(page.locator("main header").getByRole("button", { name: "Unpin organize-api", exact: true })).toBeVisible();

  // The organize page is linked from All resources and keeps the existing table intact.
  await page.goto("/resources");
  await expect(page.getByRole("table", { name: "Resources" })).toBeVisible();
  await page.getByRole("link", { name: "Organize", exact: true }).click();
  await expect(page.getByRole("heading", { name: "Organize resources", exact: true })).toBeVisible();
  const table = page.getByRole("table", { name: /Resources, page 1/ });
  await expect(table.getByRole("link", { name: "organize-api", exact: true })).toBeVisible();
  // Pinned resources are listed first.
  await expect(table.getByRole("row").nth(1)).toContainText("organize-api");

  await table.getByRole("button", { name: "Edit tags for organize-web", exact: true }).click();
  const tagDialog = page.getByRole("dialog", { name: "Tags for organize-web" });
  await tagDialog.getByLabel("Tags", { exact: true }).fill("Bad Tag!");
  await expect(tagDialog.getByText(/is not a valid tag/)).toBeVisible();
  await tagDialog.getByLabel("Tags", { exact: true }).fill("Frontend, edge");
  await tagDialog.getByRole("button", { name: "Save tags", exact: true }).click();
  await expect(tagDialog).not.toBeVisible();
  await expect(table.getByRole("list", { name: "Tags for organize-web" })).toContainText("frontend");

  await page.getByLabel("Tag", { exact: true }).fill("frontend");
  await expect(table.getByRole("link", { name: "organize-api", exact: true })).not.toBeVisible();
  await expect(table.getByRole("link", { name: "organize-web", exact: true })).toBeVisible();
  await page.getByRole("button", { name: "Reset filters" }).first().click();
  await page.getByRole("checkbox", { name: "Pinned only" }).check();
  await expect(table.getByRole("link", { name: "organize-web", exact: true })).not.toBeVisible();
  await page.getByRole("button", { name: "Reset filters" }).first().click();
  await expect(page.getByRole("navigation", { name: "Resource pages" })).toBeVisible();

  // Selection is visible, cleared on filter change, and confirmed before a bulk deploy.
  await table.getByRole("checkbox", { name: "Select organize-web", exact: true }).check();
  await table.getByRole("checkbox", { name: "Select organize-api", exact: true }).check();
  await expect(page.getByText("2 resources selected", { exact: true })).toBeVisible();
  await capture(page, testInfo, "organization-browser");
  await page.getByRole("button", { name: "Review deploy", exact: true }).click();
  const confirm = page.getByRole("dialog", { name: "Deploy 2 resources?" });
  await expect(confirm.getByText("Deploy without a per-resource review", { exact: true })).toBeVisible();
  await confirm.getByRole("button", { name: "Deploy 2 resources", exact: true }).click();
  const results = page.getByRole("list", { name: "Bulk action results" });
  await expect(results.getByRole("link", { name: "View operation" })).toHaveCount(2);
  await expect(page.getByText("2 resources selected", { exact: true })).not.toBeVisible();
  await capture(page, testInfo, "organization-bulk-results");
  await results.getByRole("link", { name: "View operation" }).first().click();
  await expect(page.getByText("Succeeded", { exact: true }).first()).toBeVisible({ timeout: 90_000 });

  // The dashboard activity shows the real operations without actors or values.
  await page.goto("/dashboard");
  const activity = page.getByRole("list", { name: "Activity" });
  await expect(activity.getByRole("link", { name: "organize-web" }).first()).toBeVisible();
  await expect(activity).not.toContainText("Administrator");
  await capture(page, testInfo, "organization-activity");
});

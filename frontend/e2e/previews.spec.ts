import { execFileSync } from "node:child_process";
import { expect, test, type Locator, type Page } from "@playwright/test";

const password = process.env.COVE_BROWSER_PASSWORD ?? "";
const widths = [320, 390, 768, 1280, 1920];

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

async function expectNoHorizontalScroll(page: Page) {
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBe(true);
}

async function captureWidths(page: Page, path: (width: number) => string, fullPage = true) {
  for (const width of widths) {
    await page.setViewportSize({ width, height: 900 });
    await page.evaluate(() => window.scrollTo(0, 0));
    await expectNoHorizontalScroll(page);
    await page.screenshot({ path: path(width), fullPage });
  }
}

test("environment previews: recipe from a real deployment, preview states and confirmations", async ({ page }, testInfo) => {
  test.setTimeout(300_000);
  await page.goto("/projects");
  await expect(page).toHaveURL(/\/login(?:[?#]|$)/);
  await page.getByLabel("Password", { exact: true }).fill(password);
  await page.getByRole("button", { name: "Sign in", exact: true }).click();
  await expect(page).not.toHaveURL(/\/login(?:[?#]|$)/);

  // A real project, environment and deployed resource on the real worker.
  await page.goto("/projects");
  await page.locator("main header").getByRole("button", { name: "New project", exact: true }).click();
  await page.getByLabel("Project name", { exact: true }).fill("Preview recipe project");
  await page.getByRole("button", { name: "Create project", exact: true }).click();
  await expect(page.getByRole("heading", { name: "Preview recipe project", exact: true })).toBeVisible();
  await page.getByRole("navigation", { name: "Project sections" }).getByRole("link", { name: /production/ }).click();
  await page.getByRole("link", { name: "New resource", exact: true }).first().click();
  await page.getByRole("button", { name: "Custom Compose or Git" }).click();
  await page.getByLabel("Name", { exact: true }).fill("preview-api");
  await chooseOption(page.getByLabel("Node", { exact: true }), "ready-browser-worker");
  await page.getByLabel("Compose file", { exact: true }).fill("services:\n  app:\n    image: busybox:1.37\n    command: [sh, -c, 'sleep 300']\n    healthcheck:\n      test: [CMD, 'true']\n      interval: 1s\n");
  await page.getByRole("button", { name: "Create resource", exact: true }).click();
  await expect(page.getByRole("heading", { name: "preview-api", exact: true })).toBeVisible();
  const url = page.url();
  const origin = new URL(url).origin;
  const id = new URL(url).pathname.split("/")[2];
  const resource = (await (await page.request.get(`/api/workloads/${id}`)).json()) as { name: string; node_id: string; compose: string; project_id: string };
  const projectId = resource.project_id;
  // A credential-like variable must be decided before previews can copy it.
  expect((await page.request.put(`/api/workloads/${id}`, { headers: { Origin: origin }, data: { name: resource.name, node_id: resource.node_id, kind: "service", compose: resource.compose, environment: { API_TOKEN: "production-token-fixture", LOG_LEVEL: "info" } } })).status()).toBe(200);
  const deploy = (await (await page.request.post(`/api/workloads/${id}/operations`, { headers: { Origin: origin }, data: { action: "deploy" } })).json()) as { id: string };
  await expect.poll(async () => ((await (await page.request.get(`/api/operations/${deploy.id}`)).json()) as { state: string }).state, { timeout: 180_000 }).toBe("succeeded");

  // The recipe plan is read from the deployed release on the worker.
  await page.goto(`/projects/${projectId}/previews`);
  await expect(page.getByRole("heading", { name: "Environment previews" })).toBeVisible();
  await chooseOption(page.getByLabel("Production environment", { exact: true }), "production");
  const resources = page.locator("section", { has: page.getByRole("heading", { name: "Resources", exact: true }) });
  await expect(resources.getByText("Decide how previews use API_TOKEN.")).toBeVisible({ timeout: 60_000 });
  await expect(resources.getByText("LOG_LEVEL", { exact: true })).toBeVisible();
  await chooseOption(page.getByRole("combobox", { name: "API_TOKEN in previews" }), "Use a preview value");
  await expect(page.getByRole("button", { name: "Save recipe" })).toBeDisabled();
  await page.getByLabel("Preview value for API_TOKEN").fill("preview-token-fixture");
  await page.getByRole("button", { name: "Save recipe" }).click();
  await expect(resources.getByText("Decide how previews use API_TOKEN.")).toHaveCount(0, { timeout: 60_000 });
  const recipe = await (await page.request.get(`/api/projects/${projectId}/preview-recipe`)).text();
  expect(recipe).not.toContain("preview-token-fixture");
  expect(recipe).not.toContain("production-token-fixture");
  await captureWidths(page, (width) => testInfo.outputPath(`previews-recipe-${width}.png`));

  // Preview lifecycle states and their actions, rendered from intercepted list and detail responses.
  const now = Date.now();
  const iso = (offset: number) => new Date(now + offset).toISOString();
  const branch = (repository: string, head: string) => ({ repository_id: repository === "acme/web" ? 1 : 2, repository, branch: "staging/checkout-redesign", head, checked_at: iso(-60_000) });
  const preview = (previewId: string, slug: string, state: string, stage: string, extra = {}) => ({ id: previewId, project_id: projectId, environment_id: `${previewId}-env`, base_environment_id: "prod", suffix: slug, slug, state, stage, detail: "", pinned: false, access: "private", delete_reason: "", last_activity_at: iso(-3_600_000), expires_at: iso(68 * 3_600_000), created_at: iso(-7_200_000), branches: [branch("acme/web", "4f1c2e9a7b0d3c5e6f8091a2b3c4d5e6f7081920"), branch("acme/api", "")], permissions: ["read", "operate", "edit"], ...extra });
  const previews = [
    preview("p-awaiting", "invoice-export", "awaiting_approval", "approval", { environment_id: "" }),
    preview("p-ready", "checkout-redesign", "ready", "ready", { pinned: true, expires_at: null }),
    preview("p-failed", "search-index-rebuild-with-a-very-long-branch-name-e3f1a2", "failed", "deploy", { detail: "Deploying api failed. Its data was kept. Check the operation log, push a fix or retry." }),
  ];
  const calls: { method: string; path: string; body: string | null }[] = [];
  await page.route("**/api/projects/*/environment-previews", (route) => route.fulfill({ json: { previews, waiting: [{ repository_id: 1, branch: "staging/onboarding", reason: "Waiting: 3 of 3 previews are in use.", received_at: iso(-120_000) }] } }));
  await page.route("**/api/environment-previews/**", (route) => {
    const request = route.request();
    calls.push({ method: request.method(), path: new URL(request.url()).pathname, body: request.postData() });
    if (request.method() !== "GET") return route.fulfill({ status: 202, json: { ok: true } });
    return route.fulfill({
      json: {
        ...previews[1],
        resources: [
          { base_id: "b1", resource_id: id, name: "web", kind: "service", source: "branch", revision: "4f1c2e9a7b0d3c5e6f8091a2b3c4d5e6f7081920", baseline_revision: "9a8b7c6d5e4f30211203a4b5c6d7e8f901234567", baseline_release: "r1", baseline_deployed_at: iso(-86_400_000), deployed: true, data_state: "none", snapshot_at: null, data_report: null, routes: ["web-checkout-redesign.preview.example.com"], published: true, operation: "o1" },
          { base_id: "b2", resource_id: id, name: "api", kind: "service", source: "baseline", revision: "9a8b7c6d5e4f30211203a4b5c6d7e8f901234567", baseline_revision: "9a8b7c6d5e4f30211203a4b5c6d7e8f901234567", baseline_release: "r2", baseline_deployed_at: iso(-86_400_000), deployed: true, data_state: "ready", snapshot_at: iso(-3_000_000), data_report: { seeded: { engine: "postgresql", snapshot_at: iso(-3_000_000), tables: 14, rows: 182340, verified: true }, volumes: [{ target: "v", mode: "copy", files: 42, bytes: 1_024_000, digest: "sha256:x" }] }, routes: [], published: false, operation: "o2" },
          { base_id: "b3", resource_id: id, name: "nightly-report", kind: "job", source: "disabled", revision: "", baseline_revision: "", baseline_release: "", baseline_deployed_at: null, deployed: false, data_state: "none", snapshot_at: null, data_report: null, routes: [], published: false, operation: "" },
        ],
      },
    });
  });
  await page.goto(`/projects/${projectId}/previews`);
  const list = page.getByRole("list", { name: "Previews in Preview recipe project" });
  await expect(list.getByText("Awaiting approval")).toBeVisible();
  await expect(page.getByText("Waiting: 3 of 3 previews are in use.", { exact: false })).toBeVisible();
  await expect(list.getByText("branch deleted; runs the pinned production revision").first()).toBeVisible();
  await list.getByRole("button", { name: "Approve" }).click();
  await expect.poll(() => calls.some((call) => call.method === "POST" && call.path === "/api/environment-previews/p-awaiting/approve")).toBe(true);
  await captureWidths(page, (width) => testInfo.outputPath(`previews-list-${width}.png`));
  await page.setViewportSize({ width: 1280, height: 900 });
  await list.getByRole("button", { name: "Actions for preview checkout-redesign" }).click();
  await page.getByRole("menuitem", { name: "Delete preview" }).click();
  const dialog = page.getByRole("dialog", { name: "Delete checkout-redesign?" });
  await expect(dialog.getByRole("button", { name: "Delete preview" })).toBeDisabled();
  await dialog.getByLabel("Type checkout-redesign to confirm").fill("checkout-redesign");
  await expect(dialog.getByRole("button", { name: "Delete preview" })).toBeEnabled();
  await captureWidths(page, (width) => testInfo.outputPath(`previews-delete-dialog-${width}.png`), false);
  await dialog.getByRole("button", { name: "Delete preview" }).click();
  await expect(dialog).toBeHidden();
  expect(calls.find((call) => call.method === "DELETE")?.body).toBe(JSON.stringify({ confirm: "checkout-redesign" }));

  // A preview environment's page: status, data provenance and protected access.
  await page.route("**/api/projects", async (route) => {
    const response = await route.fetch();
    const projects = (await response.json()) as { id: string; environments: unknown[] }[];
    for (const project of projects) if (project.id === projectId) project.environments.push({ id: "p-ready-env", name: "Preview checkout-redesign", preview_id: "p-ready", permissions: ["read", "operate", "edit"] });
    await route.fulfill({ response, json: projects });
  });
  await page.goto(`/projects/${projectId}/environments/p-ready-env`);
  await expect(page.getByRole("heading", { name: "Preview checkout-redesign" })).toBeVisible();
  await expect(page.getByText("14 tables and 182340 rows verified", { exact: false }).first()).toBeAttached();
  await expect(page.getByRole("link", { name: "web-checkout-redesign.preview.example.com" })).toBeVisible();
  await expect(page.getByRole("navigation", { name: "Breadcrumb" }).getByRole("link", { name: "Previews" })).toBeVisible();
  await expect(page.getByRole("navigation", { name: "Project sections" }).getByRole("link", { name: /Previews/ })).toHaveAttribute("aria-current", "page");
  await expect(page.getByRole("button", { name: "New connection" })).toHaveCount(0);
  await captureWidths(page, (width) => testInfo.outputPath(`preview-environment-${width}.png`));
  await page.unrouteAll({ behavior: "ignoreErrors" });
});

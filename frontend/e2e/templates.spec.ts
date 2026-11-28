import { expect, test, type Locator, type Page, type TestInfo } from "@playwright/test";

const password = process.env.COVE_BROWSER_PASSWORD ?? "";

interface Catalogue {
  templates: { origin: string; template: { id: string; name: string; category: string; capabilities: string[] | null } }[];
}

async function signIn(page: Page) {
  await page.goto("/resources/new");
  await expect(page).toHaveURL(/\/login(?:[?#]|$)/);
  await page.getByLabel("Password", { exact: true }).fill(password);
  await page.getByRole("button", { name: "Sign in", exact: true }).click();
  await expect(page).not.toHaveURL(/\/login(?:[?#]|$)/);
}

async function chooseOption(control: Locator, label: string) {
  await control.click();
  const list = control.page().getByRole("listbox");
  await expect(list).toBeVisible();
  await list.getByRole("option", { name: label, exact: true }).click();
}

async function capture(page: Page, testInfo: TestInfo, name: string) {
  for (const width of [390, 768, 1440]) {
    await page.setViewportSize({ width, height: 900 });
    await page.evaluate(() => window.scrollTo(0, 0));
    expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth), `${name} at ${width}`).toBe(true);
    await page.screenshot({ path: testInfo.outputPath(`${name}-${width}.png`), fullPage: true });
  }
  await page.setViewportSize({ width: 1440, height: 900 });
}

async function catalogue(page: Page) {
  return (await (await page.request.get("/api/templates")).json()) as Catalogue;
}

test("server catalogue prepares an undeployed resource and a team template round-trips", async ({ page }, testInfo) => {
  test.setTimeout(120_000);
  await page.setViewportSize({ width: 1440, height: 900 });
  await signIn(page);
  const project = `Templates ${Date.now()}`;
  await page.goto("/projects");
  await page.locator("main header").getByRole("button", { name: "New project", exact: true }).click();
  await page.getByLabel("Project name", { exact: true }).fill(project);
  await page.getByRole("button", { name: "Create project", exact: true }).click();
  await expect(page.getByRole("heading", { name: project, exact: true })).toBeVisible();
  await page.goto("/resources/new");
  const served = await catalogue(page);
  expect(served.templates.length).toBeGreaterThan(5);
  await expect(page.getByRole("heading", { name: "Resource templates" })).toBeVisible();
  await expect(page.getByText(`${served.templates.length} templates`, { exact: true })).toBeVisible();
  await capture(page, testInfo, "templates-catalogue");

  await page.getByPlaceholder("Search templates").fill("no-such-template");
  await expect(page.getByText("No matching templates")).toBeVisible();
  await page.getByRole("button", { name: "Reset filters" }).first().click();
  const databases = served.templates.filter((entry) => entry.template.category === "Databases").length;
  await chooseOption(page.getByRole("combobox", { name: "Category" }), "Databases");
  await expect(page.getByText(`${databases} of ${served.templates.length} templates`)).toBeVisible();

  // Host access needs an explicit acknowledgement, given here with the keyboard.
  const socket = served.templates.find((entry) => (entry.template.capabilities ?? []).includes("docker_socket"));
  if (socket) {
    await page.getByRole("button", { name: "Reset filters" }).first().click();
    await page.getByPlaceholder("Search templates").fill(socket.template.name);
    await page.getByRole("button", { name: new RegExp(socket.template.name) }).click();
    await expect(page.getByText("This template gets access to the worker host")).toBeVisible();
    await expect(page.getByText(/No public route/)).toBeVisible();
    await page.getByRole("button", { name: /Review configuration/ }).click();
    await expect(page.getByText(/Confirm that you reviewed the host access/)).toBeVisible();
    await capture(page, testInfo, "templates-host-access");
    const acknowledgement = page.getByRole("checkbox", { name: /I reviewed this host access/ });
    await acknowledgement.focus();
    await page.keyboard.press("Space");
    await expect(acknowledgement).toBeChecked();
    await page.getByRole("button", { name: "Back to templates" }).click();
  }

  await page.getByRole("button", { name: "Reset filters" }).first().click();
  await page.getByPlaceholder("Search templates").fill("PostgreSQL");
  await page.getByRole("button", { name: /^PostgreSQL/ }).click();
  await expect(page.getByText("Started healthy in Cove's local check").or(page.getByText("Not started in a runtime check"))).toBeVisible();
  await expect(page.getByText(/does not guarantee the template runs on your worker|Run preflight/).first()).toBeVisible();
  const secret = page.getByLabel(/Postgres Password/);
  await expect(secret).toHaveAttribute("type", "password");
  await expect(secret).toHaveValue("");
  await page.getByText("Review Compose file").click();
  await expect(page.locator("pre").filter({ hasText: "image: postgres:" })).toBeVisible();
  await capture(page, testInfo, "templates-detail");
  const draftRequest = page.waitForRequest((request) => request.url().endsWith("/api/templates/draft"));
  await page.getByRole("button", { name: /Review configuration/ }).click();
  const draftBody = (await draftRequest).postDataJSON() as { values: Record<string, string> };
  expect(draftBody.values).not.toHaveProperty("POSTGRES_PASSWORD");

  const name = `template-postgres-${Date.now()}`;
  await page.getByLabel("Name", { exact: true }).fill(name);
  await chooseOption(page.getByLabel("Project and environment", { exact: true }), `${project} / Project resources`);
  await chooseOption(page.getByLabel("Node", { exact: true }), "ready-browser-worker");
  await page.getByRole("button", { name: "Create resource", exact: true }).click();
  await expect(page.getByRole("heading", { name, exact: true })).toBeVisible();
  const sections = page.getByRole("navigation", { name: "Resource sections" });
  await sections.getByRole("link", { name: "Environment variables", exact: true }).click();
  await expect(page.getByText("POSTGRES_PASSWORD", { exact: true })).toBeVisible();
  await sections.getByRole("link", { name: "Operations", exact: true }).click();
  await expect(page.getByText("No operations in the recent history.")).toBeVisible();

  // Save the resource as a team template: preview, confirm, then delete it from the catalogue.
  await sections.getByRole("link", { name: "Settings", exact: true }).click();
  await page.getByRole("button", { name: "Save as team template" }).click();
  const templateId = `browser-${Date.now()}`;
  await page.getByLabel(/^Template ID/).fill(templateId);
  await page.getByRole("button", { name: "Preview template" }).click();
  await expect(page.getByText(/Enter a description/)).toBeVisible();
  await page.getByLabel("Description", { exact: true }).fill("PostgreSQL saved from a browser acceptance resource.");
  await page.getByLabel(/^Category/).fill("Team databases");
  await page.getByLabel(/^Source link/).fill("https://hub.docker.com/_/postgres");
  await page.getByLabel(/^Licence link/).fill("https://www.postgresql.org/about/licence/");
  await page.getByLabel(/^Documentation link/).fill("https://hub.docker.com/_/postgres");
  await page.getByLabel("CPUs", { exact: true }).fill("0.25");
  await page.getByLabel("Memory (MiB)", { exact: true }).fill("256");
  await page.getByLabel("Disk (MiB)", { exact: true }).fill("1024");
  await page.getByRole("button", { name: "Preview template" }).click();
  await expect(page.getByText("Transformed Compose file")).toBeVisible();
  const transformed = await page.locator("pre").filter({ hasText: "services:" }).innerText();
  expect(transformed).toContain("SETTING_");
  expect(transformed).not.toContain("POSTGRES_PASSWORD: ${POSTGRES_PASSWORD");
  await capture(page, testInfo, "templates-save-preview");
  await page.getByLabel(/^Category/).fill("Databases (team)");
  await expect(page.getByRole("button", { name: "Save to team" })).toHaveCount(0);
  await page.getByRole("button", { name: "Preview template" }).click();
  await page.getByRole("button", { name: "Save to team" }).click();
  const confirm = page.getByRole("dialog", { name: /^Save .+ to your team\?$/ });
  await expect(confirm).toBeVisible();
  await confirm.getByRole("button", { name: "Save template" }).focus();
  await page.keyboard.press("Enter");
  await expect(page.getByText("Template saved")).toBeVisible();

  await page.goto("/resources/new");
  await chooseOption(page.getByRole("combobox", { name: "Origin" }), "Team template");
  await page.getByRole("button", { name: new RegExp(name) }).click();
  await expect(page.getByText(/validated against the Compose schema only/)).toBeVisible();
  await capture(page, testInfo, "templates-team-detail");
  await page.getByRole("button", { name: "Delete template" }).click();
  const remove = page.getByRole("dialog", { name: /^Delete .+\?$/ });
  await remove.getByRole("button", { name: "Delete template" }).click();
  await expect(page.getByRole("heading", { name: "Resource templates" })).toBeVisible();
  expect((await catalogue(page)).templates.some((entry) => entry.origin === "team" && entry.template.id === templateId)).toBe(false);
});

test("template sources validate, report failed refreshes and remove after confirmation", async ({ page }, testInfo) => {
  test.setTimeout(120_000);
  await page.setViewportSize({ width: 1440, height: 900 });
  await signIn(page);
  for (const path of ["/team", "/admin"]) {
    await page.goto(path);
    const section = page.getByRole("region", { name: "Template sources" });
    await expect(section).toBeVisible();
    await expect(section.getByText("No template sources")).toBeVisible();
    await section.getByRole("button", { name: "Add source" }).click();
    await section.getByLabel("Name", { exact: true }).fill("Private source");
    await section.getByLabel("URL", { exact: true }).fill("https://user:token@templates.invalid/repo.git");
    await section.getByRole("button", { name: "Add source" }).click();
    await expect(section.getByText(/Template sources must be public/)).toBeVisible();
    await expect(section.getByText(/Enter a branch, tag or commit/)).toBeVisible();
    // The server refuses private network destinations the browser cannot judge.
    await section.getByLabel("URL", { exact: true }).fill("https://10.0.0.1/repo.git");
    await section.getByLabel("Reference", { exact: true }).fill("main");
    await section.getByRole("button", { name: "Add source" }).click();
    await expect(section.getByText("Could not add the source")).toBeVisible();
    await chooseOption(section.getByRole("combobox", { name: "Type" }), "HTTPS index file");
    await expect(section.getByLabel("Reference", { exact: true })).toHaveCount(0);
    await section.getByLabel("Name", { exact: true }).fill("Unreachable index");
    await section.getByLabel("URL", { exact: true }).fill("https://templates.invalid/cove-templates.json");
    await section.getByRole("button", { name: "Add source" }).click();
    await expect(section.getByText("Unreachable index")).toBeVisible();
    await section.getByRole("button", { name: "Refresh Unreachable index" }).click();
    await expect(section.getByText("Refresh failed", { exact: true })).toBeVisible({ timeout: 90_000 });
    await expect(section.getByText("Refresh failed, no valid revision")).toBeVisible();
    await expect(section.getByText("0 templates")).toBeVisible();
    await capture(page, testInfo, `templates-sources${path.replace("/", "-")}`);
    await section.getByRole("button", { name: "Remove Unreachable index" }).click();
    const dialog = page.getByRole("dialog", { name: "Remove Unreachable index?" });
    await dialog.getByRole("button", { name: "Remove source" }).click();
    await expect(section.getByText("No template sources")).toBeVisible();
  }
});

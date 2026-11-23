import { expect, test, type Locator, type Page, type TestInfo } from "@playwright/test";

const password = process.env.COVE_BROWSER_PASSWORD ?? "";

async function signIn(page: Page) {
  await expect(page).toHaveURL(/\/login(?:[?#]|$)/);
  await page.getByLabel("Password", { exact: true }).fill(password);
  await page.getByRole("button", { name: "Sign in", exact: true }).click();
  await expect(page).not.toHaveURL(/\/login(?:[?#]|$)/);
}

async function openProjectOverview(page: Page) {
  // Creating a project with its default production environment lands on that environment.
  await expect(page).toHaveURL(/\/projects\/[^/?#]+\/environments\/[^/?#]+$/);
  await page.getByRole("navigation", { name: "Project sections" }).getByRole("link", { name: "Overview", exact: true }).click();
  await expect(page).toHaveURL(/\/projects\/[^/?#]+$/);
}

async function chooseValue(control: Locator, value: string) {
  await control.click();
  const list = control.page().getByRole("listbox");
  await expect(list).toBeVisible();
  await list.locator(`[role="option"][data-value="${value}"]`).click();
}

async function capture(page: Page, testInfo: TestInfo, name: string) {
  for (const width of [390, 768, 1440]) {
    await page.setViewportSize({ width, height: 900 });
    expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBe(true);
    await page.screenshot({ path: testInfo.outputPath(`${name}-${width}.png`), fullPage: true });
  }
}

test("encrypted notes, revocable public status page and display time zones", async ({ page, browser, baseURL }, testInfo) => {
  test.setTimeout(120_000);
  const origin = new URL(baseURL ?? page.url()).origin;
  await page.goto("/projects");
  await signIn(page);

  await page.locator("main header").getByRole("button", { name: "New project", exact: true }).click();
  await page.getByLabel("Project name", { exact: true }).fill("Notes project");
  await page.getByRole("button", { name: "Create project", exact: true }).click();
  await expect(page.getByRole("heading", { name: "Notes project", exact: true })).toBeVisible();
  await openProjectOverview(page);

  // Notes render only the server's sanitized CommonMark: no raw HTML and no image requests.
  const imageRequests: string[] = [];
  page.on("request", (request) => {
    if (request.resourceType() === "image" && !request.url().startsWith(origin)) imageRequests.push(request.url());
  });
  const notes = page.getByRole("region", { name: "Notes", exact: true });
  await notes.getByRole("button", { name: "Add notes", exact: true }).click();
  await notes.getByLabel("Markdown", { exact: true }).fill("# On-call runbook\n\n<script>window.injected = true</script>\n\n![tracker](https://example.invalid/pixel.png)\n\n- Restart the worker");
  await notes.getByRole("button", { name: "Save notes", exact: true }).click();
  await expect(notes.getByRole("heading", { name: "On-call runbook" })).toBeVisible();
  await expect(notes.getByText("Restart the worker")).toBeVisible();
  await expect(notes.locator("script, img")).toHaveCount(0);
  expect(await page.evaluate(() => (window as { injected?: boolean }).injected)).toBeUndefined();
  expect(imageRequests).toEqual([]);
  await capture(page, testInfo, "project-notes");

  // Public status page: opt-in, shown once, no badge request from the admin page.
  const statusRequests: string[] = [];
  page.on("request", (request) => {
    if (/\/status\/|\/api\/public\//.test(request.url())) statusRequests.push(request.url());
  });
  await page.getByRole("link", { name: "Configuration", exact: true }).click();
  const sharing = page.getByRole("region", { name: "Public status page", exact: true });
  await sharing.getByLabel("Public title", { exact: true }).fill("Public demo");
  await sharing.getByRole("checkbox", { name: /anyone who has the link/ }).check();
  await sharing.getByRole("button", { name: "Create public link", exact: true }).click();
  const link = await sharing.getByLabel("Status page link", { exact: true }).inputValue();
  expect(link).toMatch(/\/shared\/[A-Za-z0-9_-]{32,}$/);
  expect(statusRequests).toEqual([]);
  await capture(page, testInfo, "status-share-created");
  await sharing.getByRole("button", { name: "I have copied the links", exact: true }).click();
  await expect(sharing.getByText(/Sharing as/)).toBeVisible();
  await expect(sharing.getByText(link)).toHaveCount(0);

  const visitor = await browser.newContext({ ignoreHTTPSErrors: true });
  try {
    const publicPage = await visitor.newPage();
    const thirdParty: string[] = [];
    publicPage.on("request", (request) => {
      if (!request.url().startsWith(origin)) thirdParty.push(request.url());
    });
    await publicPage.goto(new URL(new URL(link).pathname, origin).toString());
    await expect(publicPage.getByRole("heading", { name: "Public demo" })).toBeVisible();
    await expect(publicPage.getByText("Latest deployment results, not live availability.").first()).toBeVisible();
    await expect(publicPage.locator('meta[name="referrer"]')).toHaveAttribute("content", "no-referrer");
    await expect(publicPage.getByText("Notes project")).toHaveCount(0);
    expect(thirdParty).toEqual([]);
    await capture(publicPage, testInfo, "public-status");

    const revoke = page.getByRole("region", { name: "Public status page", exact: true });
    await revoke.getByRole("button", { name: "Turn off sharing", exact: true }).click();
    await page.getByRole("dialog", { name: "Turn off the public status page?" }).getByRole("button", { name: "Turn off sharing", exact: true }).click();
    await expect(revoke.getByRole("button", { name: "Create public link", exact: true })).toBeVisible();
    await publicPage.reload();
    await expect(publicPage.getByText("This status page is not available")).toBeVisible();
  } finally {
    await visitor.close();
  }

  // Team default and personal override change displayed times; schedules stay UTC.
  await page.goto("/team");
  const team = page.getByRole("region", { name: "Team time zone", exact: true });
  await chooseValue(team.getByLabel("Default time zone", { exact: true }), "America/New_York");
  await team.getByRole("button", { name: "Save time zone", exact: true }).click();
  await expect(team.getByText("Time zone saved")).toBeVisible();

  await page.goto("/account");
  const account = page.getByRole("region", { name: "Time zone", exact: true });
  await expect(account.getByText("Times currently show in America/New_York.", { exact: false })).toBeVisible();
  await expect(account.getByText("Scheduled backups run in UTC. Scheduled jobs and deployment windows use the time zone saved with each one.", { exact: false })).toBeVisible();
  await expect(account.getByText("UTC", { exact: true }).filter({ visible: true })).toBeVisible();
  await chooseValue(account.getByLabel("Display time zone", { exact: true }), "Asia/Tokyo");
  await account.getByRole("button", { name: "Save time zone", exact: true }).click();
  await expect(account.getByText("Times currently show in Asia/Tokyo.", { exact: false })).toBeVisible();
  await capture(page, testInfo, "account-timezone");
  await chooseValue(account.getByLabel("Display time zone", { exact: true }), "");
  await account.getByRole("button", { name: "Save time zone", exact: true }).click();
  await expect(account.getByText("Times currently show in America/New_York.", { exact: false })).toBeVisible();
});

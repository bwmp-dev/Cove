import { expect, test, type Page } from "@playwright/test";

const password = process.env.COVE_BROWSER_PASSWORD ?? "";

async function signIn(page: Page) {
  await page.goto("/projects");
  await expect(page).toHaveURL(/\/login(?:[?#]|$)/);
  await page.getByLabel("Password", { exact: true }).fill(password);
  await page.getByRole("button", { name: "Sign in", exact: true }).click();
  // Navigating before the login request settles would cancel it.
  await expect(page).not.toHaveURL(/\/login(?:[?#]|$)/);
}

async function openProjectOverview(page: Page) {
  // Creating a project with its default production environment lands on that environment.
  await expect(page).toHaveURL(/\/projects\/[^/?#]+\/environments\/[^/?#]+$/);
  await page.getByRole("navigation", { name: "Project sections" }).getByRole("link", { name: "Overview", exact: true }).click();
  await expect(page).toHaveURL(/\/projects\/[^/?#]+$/);
}

async function expectNoHorizontalScroll(page: Page) {
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBe(true);
}

function searchButton(page: Page) {
  return page.getByRole("button", { name: /^Search/ });
}

test("command palette: search, keyboard navigation, shortcut help and responsive layout", async ({ page }, testInfo) => {
  test.setTimeout(120_000);
  await signIn(page);
  const projectName = `Palette project ${Date.now().toString(36)}`;
  await page.goto("/projects");
  await page.locator("main header").getByRole("button", { name: "New project", exact: true }).click();
  await page.getByLabel("Project name", { exact: true }).fill(projectName);
  await page.getByRole("button", { name: "Create project", exact: true }).click();
  await expect(page.getByRole("heading", { name: projectName, exact: true })).toBeVisible();
  await openProjectOverview(page);
  const projectPath = new URL(page.url()).pathname;

  // Shortcuts are off until opted in; the labelled help button turns them on.
  await page.goto("/dashboard");
  await page.locator("main").click({ position: { x: 4, y: 4 } });
  await page.keyboard.press("Control+K");
  await expect(page.getByRole("combobox", { name: "Search projects, resources, nodes and pages" })).toBeHidden();
  const help = page.getByRole("dialog", { name: "Keyboard shortcuts" });
  await page.getByRole("button", { name: "Keyboard shortcuts", exact: true }).click();
  await help.getByRole("switch", { name: "Use keyboard shortcuts" }).click();
  await help.getByRole("button", { name: "Close", exact: true }).click();

  // Results come from the real /api/search and open through the router.
  await page.locator("main").click({ position: { x: 4, y: 4 } });
  await page.keyboard.press("Control+K");
  const input = page.getByRole("combobox", { name: "Search projects, resources, nodes and pages" });
  await expect(input).toBeFocused();
  await expect(page.getByRole("listbox", { name: "Search results" }).getByRole("option").first()).toBeVisible();
  await input.fill(projectName.toUpperCase());
  const option = page.getByRole("option", { name: projectName, exact: true });
  await expect(option).toBeVisible();
  await expect(option).toHaveAttribute("aria-selected", "true");
  await page.keyboard.press("Enter");
  await expect(page).toHaveURL(new RegExp(`${projectPath}$`));
  await expect(input).toBeHidden();

  // Escape closes, discards the text and returns focus to the Search button.
  await searchButton(page).click();
  await input.fill("no-such-thing-in-this-panel");
  await expect(page.getByText("No matches for “no-such-thing-in-this-panel”.")).toBeVisible();
  await page.keyboard.press("Escape");
  await expect(input).toBeHidden();
  await expect(searchButton(page)).toBeFocused();
  await searchButton(page).click();
  await expect(input).toHaveValue("");
  await input.fill("é".repeat(101));
  await expect(page.getByText("Search text can be at most 200 bytes.")).toBeVisible();
  await page.keyboard.press("Escape");

  // g chords navigate only outside text fields.
  await page.locator("main").click({ position: { x: 4, y: 4 } });
  await page.keyboard.press("g");
  await page.keyboard.press("r");
  await expect(page).toHaveURL(/\/resources$/);
  await page.goto("/projects");
  await page.locator("main header").getByRole("button", { name: "New project", exact: true }).click();
  await page.getByLabel("Project name", { exact: true }).click();
  await page.keyboard.type("gd?");
  await expect(page.getByLabel("Project name", { exact: true })).toHaveValue("gd?");
  await expect(page).toHaveURL(/\/projects$/);
  await expect(page.getByRole("dialog", { name: "Keyboard shortcuts" })).toBeHidden();
  await page.keyboard.press("Escape");
  await page.goto("/resources");

  // r r refreshes in place and n r opens the existing New resource form.
  await page.locator("main").click({ position: { x: 4, y: 4 } });
  await page.keyboard.press("r");
  await page.keyboard.press("r");
  await expect(page).toHaveURL(/\/resources$/);
  await page.keyboard.press("n");
  await page.keyboard.press("r");
  await expect(page).toHaveURL(/\/resources\/new/);
  await page.goto("/resources");

  // ? opens the help, which can turn shortcuts off again; the Search button keeps working.
  await page.locator("main").click({ position: { x: 4, y: 4 } });
  await page.keyboard.press("?");
  await expect(help).toBeVisible();
  await help.getByRole("switch", { name: "Use keyboard shortcuts" }).click();
  await help.getByRole("button", { name: "Close", exact: true }).click();
  await page.locator("main").click({ position: { x: 4, y: 4 } });
  await page.keyboard.press("Control+K");
  await expect(input).toBeHidden();
  await page.keyboard.press("g");
  await page.keyboard.press("d");
  await expect(page).toHaveURL(/\/resources$/);
  await searchButton(page).click();
  await input.fill("shortcuts");
  await page.getByRole("option", { name: /Keyboard shortcuts/ }).click();
  await expect(help.getByRole("switch", { name: "Use keyboard shortcuts" })).not.toBeChecked();
  await help.getByRole("button", { name: "Close", exact: true }).click();

  for (const width of [390, 768, 1440]) {
    await page.setViewportSize({ width, height: 900 });
    await searchButton(page).click();
    await expect(input).toBeFocused();
    await input.fill(projectName);
    await expect(page.getByRole("option", { name: projectName, exact: true })).toBeVisible();
    await expectNoHorizontalScroll(page);
    await page.screenshot({ path: testInfo.outputPath(`command-palette-${width}.png`) });
    await page.keyboard.press("Escape");
    await expect(input).toBeHidden();
  }
});

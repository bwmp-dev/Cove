import { expect, test, type Locator, type Page, type TestInfo } from "@playwright/test";

const password = process.env.COVE_BROWSER_PASSWORD ?? "";
// The harness serves the panel under this prefix (COVE_PUBLIC_URL = origin + prefix) only for this run.
const base = process.env.COVE_BROWSER_BASE_PATH ?? "";

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
  await page.setViewportSize({ width: 1440, height: 900 });
}

// Every same-origin request the dashboard makes must stay under the prefix, and no asset may fail to load.
function watchRequests(page: Page, origin: string) {
  const outside: string[] = [];
  const failed: string[] = [];
  const assets: string[] = [];
  page.on("request", (request) => {
    const url = new URL(request.url());
    if (url.origin !== origin) return;
    if (url.pathname !== base && !url.pathname.startsWith(`${base}/`)) outside.push(url.pathname);
    if (url.pathname.startsWith(`${base}/assets/`) && url.pathname.endsWith(".js")) assets.push(url.pathname);
  });
  page.on("response", (response) => {
    if (["script", "stylesheet", "image", "font"].includes(response.request().resourceType()) && response.status() >= 400) failed.push(`${response.status()} ${response.url()}`);
  });
  page.on("requestfailed", (request) => {
    if (["script", "stylesheet", "image", "font"].includes(request.resourceType())) failed.push(`failed ${request.url()}`);
  });
  return { outside, failed, assets };
}

// Nothing outside the prefix serves the panel. The root API path itself lies inside a mount such as /api, where the
// panel answers the unknown route /session, so there it is only required not to expose the session API.
async function expectNoRootAPI(page: Page, origin: string) {
  expect((await page.request.get(`${origin}${base}-outside/api/session`)).status()).toBe(404);
  const rootSession = await page.request.get(`${origin}/api/session`);
  if ("/api/session" === base || "/api/session".startsWith(`${base}/`)) {
    expect(rootSession.headers()["content-type"] ?? "").not.toContain("application/json");
  } else {
    expect(rootSession.status()).toBe(404);
  }
}

test.describe("19 base path", () => {
  test.skip(!base, "Runs only when COVE_BROWSER_BASE_PATH serves the panel under a path prefix");

  test("19 base path: sign-in, navigation, API, shares, callback metadata and logout under a path prefix", async ({ page, browser, baseURL }, testInfo) => {
    test.setTimeout(120_000);
    const origin = new URL(baseURL ?? "").origin;
    const panel = `${origin}${base}`;
    const errors: string[] = [];
    page.on("pageerror", (error) => errors.push(error.message));
    const requests = watchRequests(page, origin);

    // Only the liveness check stays at the root; the dashboard and its APIs exist only under the prefix.
    await expectNoRootAPI(page, origin);
    expect((await page.request.get(`${origin}/healthz`)).status()).toBe(200);
    const bare = await page.request.get(panel, { maxRedirects: 0 });
    expect(bare.status()).toBe(308);
    expect(bare.headers()["location"]).toBe(`${base}/`);

    await page.goto(`${panel}/projects`);
    await page.waitForURL((url) => url.pathname === `${base}/login` && url.searchParams.get("redirect") === "/projects");
    await expect(page.locator('meta[name="cove-base-path"]')).toHaveAttribute("content", base);
    await expect(page.locator("base")).toHaveAttribute("href", `${base}/`);
    await capture(page, testInfo, "base-path-login");
    await page.getByLabel("Password", { exact: true }).fill(password);
    await page.getByRole("button", { name: "Sign in", exact: true }).click();
    await expect(page).toHaveURL(`${panel}/projects`);
    await expect(page.getByRole("heading", { name: "Projects", exact: true })).toBeVisible();

    const session = (await page.context().cookies(panel)).find((cookie) => cookie.name === "__Host-cove");
    expect(session).toMatchObject({ path: "/", secure: true, httpOnly: true, sameSite: "Strict" });

    // State-changing requests are accepted only from the panel origin, which never includes the prefix. Session routes
    // refuse a cookie sent from another origin as unauthenticated.
    expect((await page.request.put(`${panel}/api/preferences/timezone`, { data: { timezone: "" }, headers: { Origin: "https://evil.example" } })).status()).toBe(401);
    expect((await page.request.put(`${panel}/api/preferences/timezone`, { data: { timezone: "" }, headers: { Origin: `${origin}${base}` } })).status()).toBe(401);
    expect((await page.request.put(`${panel}/api/preferences/timezone`, { data: { timezone: "" }, headers: { Origin: origin } })).ok()).toBe(true);

    // Router links carry the prefix; navigation and deep-link refreshes keep it.
    const nodesLink = page.getByRole("navigation", { name: "Primary" }).getByRole("link", { name: "Nodes", exact: true });
    await expect(nodesLink).toHaveAttribute("href", `${base}/nodes`);
    await nodesLink.click();
    await expect(page).toHaveURL(`${panel}/nodes`);
    await expect(page.getByRole("heading", { name: "Nodes", exact: true })).toBeVisible();
    await page.reload();
    await expect(page.getByRole("heading", { name: "Nodes", exact: true })).toBeVisible();

    // A user preference mutation through the shared API client.
    await page.goto(`${panel}/account`);
    const account = page.getByRole("region", { name: "Time zone", exact: true });
    await chooseValue(account.getByLabel("Display time zone", { exact: true }), "Asia/Tokyo");
    await account.getByRole("button", { name: "Save time zone", exact: true }).click();
    await expect(account.getByText("Times currently show in Asia/Tokyo.", { exact: false })).toBeVisible();
    await page.reload();
    await expect(account.getByText("Times currently show in Asia/Tokyo.", { exact: false })).toBeVisible();
    await chooseValue(account.getByLabel("Display time zone", { exact: true }), "");
    await account.getByRole("button", { name: "Save time zone", exact: true }).click();
    await expect(account.getByText("Times currently show in Asia/Tokyo.", { exact: false })).toHaveCount(0);

    // The API reference loads its renderer chunk and offers the prefixed description; Try it out uses the spec's server.
    await page.goto(`${panel}/api-docs`);
    await expect(page.getByRole("link", { name: "Download OpenAPI JSON", exact: true })).toHaveAttribute("href", `${base}/api/v1/openapi.json`);
    await expect(page.locator(".swagger-ui").first()).toBeVisible();
    const spec = (await (await page.request.get(`${panel}/api/v1/openapi.json`)).json()) as { servers: { url: string }[] };
    expect(spec.servers[0]?.url).toBe(`${base}/api/v1`);

    // Server-generated GitHub App callbacks include the prefix. Nothing is sent to GitHub.
    const setup = await page.request.post(`${panel}/api/github/app/setup`, { data: { name: "Cove base path test", description: "", organization: "" }, headers: { Origin: origin } });
    expect(setup.ok()).toBe(true);
    const manifest = JSON.parse(((await setup.json()) as { manifest: string }).manifest) as { url: string; redirect_url: string; setup_url: string; callback_urls: string[]; hook_attributes: { url: string } };
    expect(manifest.url).toBe(panel);
    expect(manifest.redirect_url).toBe(`${panel}/api/github/app/callback`);
    expect(manifest.setup_url).toBe(`${panel}/integrations`);
    expect(manifest.callback_urls).toEqual([`${panel}/api/github/app/connect/callback`]);
    expect(manifest.hook_attributes.url).toBe(`${panel}/hooks/github-app`);

    // A public status link and badge under the prefix, opened by a visitor without a session.
    await page.goto(`${panel}/projects`);
    await page.locator("main header").getByRole("button", { name: "New project", exact: true }).click();
    await page.getByLabel("Project name", { exact: true }).fill("Base path project");
    await page.getByRole("button", { name: "Create project", exact: true }).click();
    await expect(page.getByRole("heading", { name: "Base path project", exact: true })).toBeVisible();
    await page.waitForURL((url) => url.pathname.startsWith(`${base}/projects/`));
    await capture(page, testInfo, "base-path-project");
    await page.getByRole("link", { name: "Configuration", exact: true }).click();
    const sharing = page.getByRole("region", { name: "Public status page", exact: true });
    await sharing.getByLabel("Public title", { exact: true }).fill("Base path status");
    await sharing.getByRole("checkbox", { name: /anyone who has the link/ }).check();
    await sharing.getByRole("button", { name: "Create public link", exact: true }).click();
    const link = await sharing.getByLabel("Status page link", { exact: true }).inputValue();
    const badge = await sharing.getByLabel("Badge image URL", { exact: true }).inputValue();
    expect(link.startsWith(`${panel}/shared/`)).toBe(true);
    expect(badge.startsWith(`${panel}/status/`)).toBe(true);
    expect(badge.endsWith("/badge.svg")).toBe(true);
    await sharing.getByRole("button", { name: "I have copied the links", exact: true }).click();

    const visitor = await browser.newContext({ ignoreHTTPSErrors: true });
    try {
      const publicPage = await visitor.newPage();
      const visitorRequests = watchRequests(publicPage, origin);
      const publicAPI = publicPage.waitForRequest((request) => request.url().startsWith(`${panel}/api/public/projects/`));
      await publicPage.goto(link);
      const apiRequest = await publicAPI;
      expect(apiRequest.headers()["x-cove-team"]).toBeUndefined();
      await expect(publicPage.getByRole("heading", { name: "Base path status" })).toBeVisible();
      await capture(publicPage, testInfo, "base-path-public-status");
      const svg = await visitor.request.get(badge);
      expect(svg.status()).toBe(200);
      expect(svg.headers()["content-type"]).toContain("image/svg+xml");
      expect(await svg.text()).toContain("<svg");
      expect(visitorRequests.outside).toEqual([]);
      expect(visitorRequests.failed).toEqual([]);
    } finally {
      await visitor.close();
    }

    // Lazy route chunks resolved against the injected <base href>.
    expect(new Set(requests.assets).size).toBeGreaterThan(1);

    await page.getByRole("button", { name: "Sign out", exact: true }).click();
    await page.waitForURL((url) => url.pathname === `${base}/login`);
    await expect(page.getByLabel("Password", { exact: true })).toBeVisible();
    expect((await page.request.get(`${panel}/api/session`)).status()).toBe(401);
    await expectNoRootAPI(page, origin);
    await page.goto(`${panel}/nodes`);
    await page.waitForURL((url) => url.pathname === `${base}/login` && url.searchParams.get("redirect") === "/nodes");

    expect(requests.outside).toEqual([]);
    expect(requests.failed).toEqual([]);
    expect(errors).toEqual([]);
  });
});

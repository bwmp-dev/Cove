import { expect, test, type Page } from "@playwright/test";

// Default and negative acceptance only. The standard scripts/test-browser.py worker mounts the host Docker socket, so
// it can read its local endpoint and offers verification, but no dedicated bounded filesystem is provided: the status
// must stay "no hard cap", a bogus path must be refused through the real form, and the refusal must leave the saved
// state unchanged. Positive and exhaustion acceptance needs a real dedicated filesystem and runs as a separate native
// test. No request here is mocked.
const password = process.env.COVE_BROWSER_PASSWORD ?? "";

test.use({ actionTimeout: 30_000, navigationTimeout: 60_000 });

interface Status {
  supported: boolean;
  can_configure: boolean;
  configured: boolean;
  verified: boolean;
  cleanup_pending: boolean;
  path: string;
  image: string;
  reason: string;
  active_operation_id: string;
  capacity_bytes: number;
  available_bytes: number;
  can_manage: boolean;
}

const defaultReason = /no dedicated bounded build-cache filesystem is configured/;
const bogusPath = "/tmp/bogus";

async function expectNoHorizontalScroll(page: Page) {
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBe(true);
}

test("build cache: the mounted-socket browser worker reports no hard cap and refuses a bogus path from the form", async ({ page }, testInfo) => {
  await page.goto("/nodes");
  await expect(page).toHaveURL(/\/login(?:[?#]|$)/);
  await page.getByLabel("Password", { exact: true }).fill(password);
  await page.getByRole("button", { name: "Sign in", exact: true }).click();
  await expect(page).not.toHaveURL(/\/login(?:[?#]|$)/);

  const nodes = (await (await page.request.get("/api/nodes")).json()) as { id: string; name: string; state: string; capabilities: { build_cache_limits?: boolean } | null }[];
  const node = nodes.find((item) => item.name === "ready-browser-worker");
  if (!node) throw new Error("The browser worker is missing");
  expect(node.state).toBe("ready");
  expect(node.capabilities?.build_cache_limits).toBe(true);
  const statusURL = `/api/nodes/${node.id}/build-cache`;
  const readStatus = async () => {
    const response = await page.request.get(statusURL);
    expect(response.status(), await response.text()).toBe(200);
    return (await response.json()) as Status;
  };
  const unconfigured = { supported: true, can_configure: true, configured: false, verified: false, cleanup_pending: false, path: "", active_operation_id: "", capacity_bytes: 0, available_bytes: 0, can_manage: true };

  const initial = await readStatus();
  expect(initial).toMatchObject(unconfigured);
  expect(initial.reason).toMatch(defaultReason);
  expect(initial.image).toMatch(/@sha256:[a-f0-9]{64}$/);

  const section = page.getByRole("region", { name: "Build cache" });
  const expectDefault = async () => {
    await expect(section.getByText("No hard cap", { exact: true })).toBeVisible();
    await expect(section.getByText(/do not limit it, so this is not a hard cap/)).toBeVisible();
    await expect(section.getByText(defaultReason)).toBeVisible();
    await expect(section.getByText("Hard cap enforced")).toHaveCount(0);
    await expect(section.getByText("Saved filesystem not verified")).toHaveCount(0);
    await expect(section.getByRole("meter")).toHaveCount(0);
    await expect(section.getByRole("button", { name: "Disable hard cap" })).toHaveCount(0);
    await expect(section.getByRole("button", { name: "Retry cleanup" })).toHaveCount(0);
  };

  await page.goto(`/nodes/${node.id}`);
  await expectDefault();
  await expect(section.getByText(/registry storage are outside this cap/)).toBeVisible();
  await expect(section.getByText(initial.image)).toBeVisible();
  await expect(section.getByText("This worker cannot verify a dedicated filesystem.")).toHaveCount(0);

  const field = section.getByRole("textbox", { name: "Filesystem mount path" });
  const verify = section.getByRole("button", { name: "Verify and use filesystem" });
  const dialog = page.getByRole("dialog", { name: "Use this filesystem for the build cache?" });
  await expect(field).toHaveValue("");
  await expect(verify).toBeEnabled();

  const puts: { method: string; body: string | null }[] = [];
  page.on("request", (request) => {
    if (new URL(request.url()).pathname === statusURL && request.method() !== "GET") puts.push({ method: request.method(), body: request.postData() });
  });

  await verify.click();
  await expect(section.getByText("Enter the filesystem's mount path.", { exact: true })).toBeVisible();
  await expect(field).toHaveAttribute("aria-invalid", "true");
  await expect(dialog).toHaveCount(0);
  expect(puts).toEqual([]);

  await field.focus();
  await expect(field).toBeFocused();
  await page.keyboard.press("Shift+Tab");
  await expect(field).not.toBeFocused();
  await page.keyboard.press("Tab");
  await expect(field).toBeFocused();
  await field.fill(bogusPath);
  await expect(section.getByText("Enter the filesystem's mount path.", { exact: true })).toHaveCount(0);
  await page.keyboard.press("Tab");
  await expect(verify).toBeFocused();
  await page.keyboard.press("Enter");
  await expect(dialog).toBeVisible();
  await expect(dialog.getByText(bogusPath, { exact: true })).toBeVisible();

  const refusal = page.waitForResponse((response) => new URL(response.url()).pathname === statusURL && response.request().method() === "PUT");
  await dialog.getByRole("button", { name: "Verify and use filesystem" }).click();
  const refused = await refusal;
  expect(refused.status(), await refused.text()).toBe(409);
  expect(puts).toHaveLength(1);
  expect(puts[0].method).toBe("PUT");
  expect(JSON.parse(puts[0].body ?? "null")).toEqual({ path: bogusPath });

  await expect(dialog).toHaveCount(0);
  await expect(section.getByText("Verifying the filesystem did not complete", { exact: true })).toBeVisible();
  await expect(section.getByText(/Your path is kept\. Check the refreshed status/)).toBeVisible();
  await expect(field).toHaveValue(bogusPath);
  await expectDefault();
  await expect(section.getByText(/Hard cap verified|was saved but is not verified/)).toHaveCount(0);

  const after = await readStatus();
  expect(after).toMatchObject(unconfigured);
  expect(after.reason).toMatch(defaultReason);

  await page.reload();
  await expectDefault();
  await expect(field).toHaveValue("");
  await expect(section.getByText("Verifying the filesystem did not complete")).toHaveCount(0);
  expect(puts).toHaveLength(1);

  await section.scrollIntoViewIfNeeded();
  for (const width of [320, 390, 768, 1280, 1440, 1920]) {
    await page.setViewportSize({ width, height: 900 });
    await expect(section.getByText("No hard cap", { exact: true })).toBeVisible();
    await expectNoHorizontalScroll(page);
    await page.screenshot({ path: testInfo.outputPath(`build-cache-${width}.png`), fullPage: true });
  }
});

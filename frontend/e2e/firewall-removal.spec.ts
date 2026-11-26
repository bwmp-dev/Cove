// Mocked contract test for stopping Cove's firewall management. The panel, session and worker node are real, and the
// worker's reported capabilities are checked as they are. Every firewall inspection, preview and change below is
// answered by a route mock, so no firewall request ever reaches the worker or changes a host.
import { expect, test, type Page, type TestInfo } from "@playwright/test";

const password = process.env.COVE_BROWSER_PASSWORD ?? "";
const base = process.env.COVE_BROWSER_BASE_PATH ?? "";
const panelPath = (route: string) => `${base}${route}`;
const firewallPath = /\/api\/nodes\/[^/]+\/firewall(?:\/preview)?$/;

async function capture(page: Page, testInfo: TestInfo, name: string) {
  for (const width of [320, 390, 768, 1280, 1440, 1920]) {
    await page.setViewportSize({ width, height: 900 });
    expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth), `${name} at ${width}`).toBe(true);
    await page.screenshot({ path: testInfo.outputPath(`${name}-${width}.png`), fullPage: true });
  }
  await page.setViewportSize({ width: 1280, height: 900 });
}

test("firewall removal: mocked residue preview, cancellation, confirmation, ambiguous pending removal and old worker", async ({ page }, testInfo) => {
  test.setTimeout(180_000);
  const errors: string[] = [];
  page.on("pageerror", (error) => errors.push(error.message));
  await page.goto(panelPath("/nodes"));
  await expect(page).toHaveURL(/\/login(?:[?#]|$)/);
  await page.getByLabel("Password", { exact: true }).fill(password);
  await page.getByRole("button", { name: "Sign in", exact: true }).click();
  await expect(page).not.toHaveURL(/\/login(?:[?#]|$)/);

  const nodes = (await (await page.request.get(panelPath("/api/nodes"))).json()) as { id: string; name: string; capabilities: { firewall?: boolean; firewall_remove?: boolean } | null }[];
  const node = nodes.find((item) => item.name === "ready-browser-worker");
  if (!node) throw new Error("Fixture worker node missing");
  // Real probe result: the upgraded worker reports both capabilities.
  expect(node.capabilities?.firewall).toBe(true);
  expect(node.capabilities?.firewall_remove).toBe(true);

  // Mocked host states. Residue is an empty managed policy whose table is gone but whose boot and recovery files remain.
  const residue = {
    supported: true, active: false, managed: true, remove_supported: true, revision: "", limited: false, notice: "", last_error: "",
    rules: [], available: [{ protocol: "tcp", port: 8080 }], reserved: [22, 80, 443],
    ufw: "Status: active", nftables: "table inet filter {\n chain input {\n  type filter hook input priority filter; policy accept;\n }\n}",
  };
  const removed = { ...residue, managed: false };
  const revisions = ["e".repeat(64), "f".repeat(64), "1".repeat(64)];
  let current: Record<string, unknown> = residue;
  const posts: { path: string; body: unknown }[] = [];
  let releaseApply: (() => void) | undefined;
  let applyResponse: { status: number; body: unknown } = { status: 200, body: removed };
  await page.route(firewallPath, async (route) => {
    const request = route.request();
    if (request.method() === "GET") return route.fulfill({ json: current, headers: { "Cache-Control": "no-store" } });
    const path = new URL(request.url()).pathname;
    posts.push({ path, body: request.postDataJSON() });
    if (path.endsWith("/preview")) return route.fulfill({ json: { ...current, revision: revisions.shift() } });
    await new Promise<void>((resolve) => { releaseApply = resolve; });
    if (applyResponse.status !== 200) return route.fulfill({ status: applyResponse.status, contentType: "text/plain", body: String(applyResponse.body) });
    return route.fulfill({ json: applyResponse.body });
  });
  const firewall = panelPath(`/api/nodes/${node.id}/firewall`);
  const applies = () => posts.filter((post) => post.path === firewall);

  await page.goto(panelPath(`/nodes/${node.id}`));
  const section = page.getByRole("region", { name: "Host firewall" });
  await section.getByRole("button", { name: "Inspect", exact: true }).click();
  await expect(section.getByText("Stop managing the firewall", { exact: true })).toBeVisible();
  await expect(section.getByText(/its table, boot restoration or recovery files remain/)).toBeVisible();
  await section.scrollIntoViewIfNeeded();
  await capture(page, testInfo, "firewall-removal-residue");

  // A fresh removal preview opens the confirmation; cancelling sends nothing else.
  const stop = section.getByRole("button", { name: "Stop managing firewall", exact: true });
  await stop.click();
  const dialog = page.getByRole("dialog", { name: "Stop managing this host's firewall?" });
  await expect(dialog).toContainText("Cove's empty table, boot restoration and recovery files are still removed");
  await expect(dialog).toContainText("UFW, other nftables tables and default policies are not changed and still apply");
  expect(posts).toEqual([{ path: `${firewall}/preview`, body: { remove: true } }]);
  await capture(page, testInfo, "firewall-removal-preview");
  await page.keyboard.press("Escape");
  await expect(dialog).toBeHidden();
  expect(applies()).toHaveLength(0);

  // The next attempt previews again and submits only that preview's revision, after the node name is typed.
  await stop.click();
  const confirm = dialog.getByLabel(`Type ${node.name} to confirm`);
  await confirm.fill(node.name.slice(0, -1));
  await confirm.press("Enter");
  await expect(dialog.getByRole("button", { name: "Stop managing firewall", exact: true })).toBeDisabled();
  await confirm.fill(node.name);
  await confirm.press("Enter");
  await expect(section.getByText("Stopping firewall management", { exact: true })).toBeVisible();
  await expect(section.getByRole("button", { name: "Inspect again", exact: true })).toBeDisabled();
  await capture(page, testInfo, "firewall-removal-stopping");
  expect(posts.map((post) => post.path)).toEqual([`${firewall}/preview`, `${firewall}/preview`, firewall]);
  expect(applies()[0].body).toEqual({ remove: true, revision: "f".repeat(64), confirm: true });
  current = removed;
  releaseApply?.();
  await expect(section.getByText("Cove stopped managing this host's firewall", { exact: true })).toBeVisible();
  await expect(section.getByText("Stop managing the firewall", { exact: true })).toBeHidden();
  await capture(page, testInfo, "firewall-removal-stopped");

  // An ambiguous result is not reported as success: the status is read again once and shows the pending removal.
  current = residue;
  await section.getByRole("button", { name: "Inspect again", exact: true }).click();
  await stop.click();
  await dialog.getByLabel(`Type ${node.name} to confirm`).fill(node.name);
  applyResponse = { status: 502, body: "firewall confirmation could not be verified; refresh before retrying and inspect the recovery deadline" };
  await dialog.getByRole("button", { name: "Stop managing firewall", exact: true }).click();
  await expect(section.getByText("Stopping firewall management", { exact: true })).toBeVisible();
  current = { ...residue, pending: { id: "c".repeat(32), deadline: new Date(Date.now() + 120_000).toISOString(), remove: true } };
  releaseApply?.();
  await expect(section.getByText("The request to stop managing the firewall could not be confirmed", { exact: true })).toBeVisible();
  await expect(section.getByText("Unconfirmed stop of firewall management", { exact: true })).toBeVisible();
  await expect(section.getByText("Cove stopped managing this host's firewall", { exact: true })).toBeHidden();
  await expect(stop).toBeDisabled();
  await expect(section.getByRole("button", { name: "Edit rules", exact: true })).toBeDisabled();
  await capture(page, testInfo, "firewall-removal-pending");
  expect(applies()).toHaveLength(2);

  // A worker that predates removal reports no remove_supported field and is not offered removal.
  const old: Record<string, unknown> = { ...residue };
  delete old.remove_supported;
  current = old;
  await section.getByRole("button", { name: "Inspect again", exact: true }).click();
  await expect(section.getByText(/needs an upgraded worker/)).toBeVisible();
  await expect(stop).toBeHidden();
  await capture(page, testInfo, "firewall-removal-old-worker");
  expect(applies()).toHaveLength(2);
  expect(errors).toEqual([]);
});

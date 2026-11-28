// Cloud provider tokens and paid server creation against the disposable packaged panel.
// Real backend: sign-in, provider token create/edit/delete through the UI, the empty list, the invalid-kind
// request, every no-store header and the sealed token at rest. Route-mocked: catalogue, creation (a lost
// acknowledgement), reconcile and abandon, so no provider is called. COVE_G14_SINKHOLE names a container
// that the three provider API hosts resolve to; it must log no connection during the case.
import { createHash, randomBytes } from "node:crypto";
import { execFileSync } from "node:child_process";
import { expect, test, type Locator, type Page, type TestInfo } from "@playwright/test";

const password = process.env.COVE_BROWSER_PASSWORD ?? "";
const database = process.env.COVE_BROWSER_DATABASE ?? "";
const sinkhole = process.env.COVE_G14_SINKHOLE ?? "";
const fixture = /^(cove-browser-[a-f0-9]{12})-db$/.exec(database)?.[1];

function docker(...args: string[]) {
  return execFileSync("docker", args, { encoding: "utf8" }).trim();
}

function sinkholeHits() {
  return docker("logs", sinkhole).split("\n").filter((line) => line.startsWith("CONNECT")).length;
}

async function chooseOption(control: Locator, selection: string | { label: string }) {
  await control.click();
  const list = control.page().getByRole("listbox");
  await expect(list).toBeVisible();
  if (typeof selection !== "string") {
    await list.getByRole("option", { name: selection.label, exact: true }).click();
    return;
  }
  const options = list.getByRole("option");
  const count = await options.count();
  for (let index = 0; index < count; index++) {
    if ((await options.nth(index).getAttribute("data-value")) === selection) {
      await options.nth(index).click();
      return;
    }
  }
  throw new Error(`Select option not found: ${selection}`);
}

async function signIn(page: Page) {
  await expect(page).toHaveURL(/\/login(?:[?#]|$)/);
  await page.getByLabel("Password", { exact: true }).fill(password);
  await page.getByRole("button", { name: "Sign in", exact: true }).click();
  await expect(page).not.toHaveURL(/\/login(?:[?#]|$)/);
}

async function expectNoHorizontalScroll(page: Page) {
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBe(true);
}

async function capture(page: Page, testInfo: TestInfo, name: string, fullPage = true) {
  for (const width of [390, 768, 1440]) {
    await page.setViewportSize({ width, height: 900 });
    await expectNoHorizontalScroll(page);
    await page.screenshot({ path: testInfo.outputPath(`${name}-${width}.png`), fullPage });
  }
  await page.setViewportSize({ width: 1440, height: 900 });
}

// Same-origin requests from the signed-in page, so the panel's CSRF origin check applies as in the dashboard.
async function api(page: Page, method: string, path: string, body?: unknown) {
  return page.evaluate(async ({ method, path, body }) => {
    const response = await fetch(path, { method, headers: body === undefined ? {} : { "Content-Type": "application/json" }, body: body === undefined ? undefined : JSON.stringify(body), credentials: "same-origin", cache: "no-store" });
    return { status: response.status, cache: response.headers.get("cache-control"), text: await response.text() };
  }, { method, path, body });
}

test.skip(sinkhole === "", "Requires the isolated provider sinkhole fixture: set COVE_G14_SINKHOLE to the container the provider API hosts resolve to.");

test("cloud provider tokens and paid server creation stay explicit and never expose secrets", async ({ page }, testInfo) => {
  test.setTimeout(240_000);
  expect(fixture, "isolated fixture database name").toBeDefined();
  expect(sinkhole).toMatch(/^g14-sink-[a-f0-9]{12}$/);

  // Preflight: the running containers are the unique images built for this run.
  for (const role of ["panel", "worker"]) {
    expect(docker("inspect", "--format", "{{.Image}}", `${fixture}-${role}`), `${role} container image`).toBe(docker("image", "inspect", "--format", "{{.Id}}", `${fixture}-${role}:development`));
  }
  const hostKey = docker("exec", `${fixture}-worker`, "cat", "/etc/ssh/ssh_host_ed25519_key.pub").split(" ").slice(0, 2).join(" ");
  const pinned = docker("exec", database, "psql", "-U", "postgres", "-d", "cove", "-Atc", "SELECT host_key FROM nodes WHERE name='ready-browser-worker'");
  expect(pinned.split(" ").slice(0, 2).join(" "), "worker host key matches the pinned key").toBe(hostKey);
  const fingerprint = `SHA256:${createHash("sha256").update(Buffer.from(hostKey.split(" ")[1] ?? "", "base64")).digest("base64").replace(/=+$/, "")}`;
  testInfo.annotations.push({ type: "worker-fingerprint", description: fingerprint });

  await page.goto("/nodes");
  await signIn(page);
  await page.goto("/nodes");

  const empty = await api(page, "GET", "/api/node-providers");
  expect(empty).toEqual({ status: 200, cache: "no-store", text: expect.stringMatching(/^\[\]\s*$/) });
  const invalid = await api(page, "POST", "/api/node-providers", { kind: "aws", name: "Invalid", token: "x".repeat(32) });
  expect(invalid.status).toBe(400);
  expect(invalid.cache).toBe("no-store");
  expect((await api(page, "GET", "/api/node-provisions")).cache).toBe("no-store");
  const hitsBefore = sinkholeHits();

  // Real backend token CRUD.
  const token = `fake${randomBytes(24).toString("hex")}`;
  const name = `Browser provider ${randomBytes(3).toString("hex")}`;
  const tokens = page.locator("section").filter({ has: page.getByRole("heading", { name: "Cloud provider tokens", exact: true }) });
  await expect(tokens.getByText(/No provider tokens/)).toBeVisible();
  await tokens.getByRole("button", { name: "Add token", exact: true }).click();
  const add = page.getByRole("dialog", { name: "Add provider token" });
  await expect(add.getByText(/A Read & Write API token from the Hetzner Cloud project/)).toBeVisible();
  await add.getByLabel("Name", { exact: true }).fill(name);
  await expect(add.getByLabel("API token", { exact: true })).toHaveAttribute("type", "password");
  await add.getByLabel("API token", { exact: true }).fill(token);
  await capture(page, testInfo, "g14-token-dialog", false);
  await add.getByLabel("API token", { exact: true }).press("Enter");
  await expect(add).toBeHidden();
  await expect(tokens.getByText(name, { exact: true })).toBeVisible();
  const listed = await api(page, "GET", "/api/node-providers");
  expect(listed.cache).toBe("no-store");
  expect(listed.text).not.toContain(token);
  const provider = (JSON.parse(listed.text) as { id: string; name: string; kind: string; has_token: boolean }[]).find((item) => item.name === name);
  expect(provider).toMatchObject({ kind: "hetzner", has_token: true });
  const stored = docker("exec", database, "psql", "-U", "postgres", "-d", "cove", "-Atc", `SELECT encode(secret,'escape') FROM node_providers WHERE id='${provider!.id}'`);
  expect(stored, "token is sealed at rest").not.toContain(token);
  expect(await page.evaluate((secret) => [location.href, JSON.stringify({ ...localStorage }), JSON.stringify({ ...sessionStorage }), document.documentElement.outerHTML, ...Array.from(document.querySelectorAll("input"), (input) => input.value)].some((value) => value.includes(secret)), token)).toBe(false);
  expect(sinkholeHits(), "saving a token made no provider connection").toBe(hitsBefore);

  await tokens.getByRole("button", { name: `Edit ${name}`, exact: true }).click();
  const edit = page.getByRole("dialog", { name: `Edit ${name}` });
  await expect(edit.getByLabel(/Replacement API token/)).toHaveValue("");
  await expect(edit.getByRole("combobox", { name: "Provider" })).toBeDisabled();
  await edit.getByRole("button", { name: "Save changes", exact: true }).click();
  await expect(edit).toBeHidden();
  expect(sinkholeHits(), "editing a token made no provider connection").toBe(hitsBefore);

  // Mocked provider-facing calls. Evidence for these comes from the routes, not the backend.
  const now = Date.now();
  const iso = (offset: number) => new Date(now + offset).toISOString();
  const job = { provider_id: provider!.id, provider_kind: "hetzner", provider_name: name, node_id: null, server_id: "", last_error: "", created_at: iso(-60_000), updated_at: iso(-30_000), deadline: iso(1_140_000) };
  const jobs = [
    { ...job, id: "a".repeat(32), server_name: "browser-uncertain", phase: "uncertain", last_error: "The provider did not confirm the server creation." },
    { ...job, id: "b".repeat(32), server_name: "browser-failed", phase: "failed", server_id: "77", last_error: "The worker did not enroll before the deadline." },
  ];
  const posts: unknown[] = [];
  const mutations: string[] = [];
  let catalogCalls = 0;
  await page.route(`**/api/node-providers/${provider!.id}/catalog`, (route) => {
    catalogCalls++;
    return route.fulfill({ json: {
      regions: [{ id: "fsn1", name: "Falkenstein" }],
      plans: [{ id: "cx22", name: "CX22", cpus: 2, memory_mb: 4096, architecture: "x86", regions: ["fsn1"] }],
      images: [{ id: "debian-12", name: "Debian 12", architecture: "x86" }],
      limited: false,
      checked_at: iso(0),
    } });
  });
  await page.route("**/api/node-provisions", (route) => {
    if (route.request().method() === "POST") {
      posts.push(route.request().postDataJSON());
      return route.fulfill({ status: 503, contentType: "text/plain", body: "control plane unavailable" });
    }
    return route.fulfill({ json: jobs });
  });
  await page.route("**/api/node-provisions/*/reconcile", (route) => { mutations.push(`${route.request().method()} reconcile ${route.request().postData()}`); return route.fulfill({ status: 202, json: { id: "a".repeat(32) } }); });
  await page.route(`**/api/node-provisions/${"a".repeat(32)}`, (route) => { mutations.push(`${route.request().method()} abandon ${route.request().postData()}`); return route.fulfill({ json: { ok: true } }); });
  await page.reload();

  const servers = page.locator("section").filter({ has: page.getByRole("heading", { name: "Cloud servers", exact: true }) });
  await expect(servers.getByText("Outcome unknown", { exact: true })).toBeVisible();
  await expect(servers.getByRole("button", { name: "Reconcile browser-failed" })).toHaveCount(0);
  await capture(page, testInfo, "g14-cloud-servers");

  // Keyboard path: open the form, review and confirm without the mouse where it matters.
  const create = servers.getByRole("button", { name: "Create server", exact: true });
  await create.focus();
  await page.keyboard.press("Enter");
  await chooseOption(page.getByRole("combobox", { name: "Provider token" }), { label: `${name} (Hetzner Cloud)` });
  await page.getByLabel("Server name", { exact: true }).fill("browser-worker");
  expect(catalogCalls, "catalogue is not fetched until requested").toBe(0);
  await page.getByRole("button", { name: "Load catalogue", exact: true }).click();
  await chooseOption(page.getByRole("combobox", { name: "Region" }), "fsn1");
  await chooseOption(page.getByRole("combobox", { name: "Plan" }), "cx22");
  await chooseOption(page.getByRole("combobox", { name: "Image" }), "debian-12");
  await expect(page.getByRole("combobox", { name: "Plan" })).toContainText("Price unknown");
  await capture(page, testInfo, "g14-cloud-server-create");
  await page.getByRole("button", { name: "Review paid server", exact: true }).focus();
  await page.keyboard.press("Enter");
  const confirm = page.getByRole("dialog", { name: "Create a paid server?" });
  await expect(confirm.getByRole("button", { name: "Create paid server", exact: true })).toBeDisabled();
  await confirm.getByLabel(/I understand/).focus();
  await page.keyboard.press("Space");
  await expect(confirm.getByRole("button", { name: "Create paid server", exact: true })).toBeDisabled();
  await page.keyboard.press("Tab");
  await expect(confirm.getByLabel(/trusted Cosign v3 or later/)).toBeFocused();
  await page.keyboard.press("Space");
  await page.keyboard.press("Tab");
  await page.keyboard.press("Tab");
  await expect(confirm.getByRole("button", { name: "Create paid server", exact: true })).toBeFocused();
  await capture(page, testInfo, "g14-cloud-server-confirm", false);
  await page.keyboard.press("Enter");
  await expect(confirm.getByText("The creation may have been queued", { exact: true })).toBeVisible();
  await expect(confirm.getByRole("button", { name: "Create paid server", exact: true })).toBeDisabled();
  expect(posts).toEqual([{ provider_id: provider!.id, name: "browser-worker", region: "fsn1", plan: "cx22", image: "debian-12", confirm: true, confirm_verifier: true }]);
  await capture(page, testInfo, "g14-cloud-server-ambiguous", false);
  await page.keyboard.press("Escape");
  await expect(confirm).toBeHidden();
  expect(posts, "no automatic retry").toHaveLength(1);

  await servers.getByRole("button", { name: "Reconcile browser-uncertain", exact: true }).click();
  const reconcile = page.getByRole("dialog", { name: "Reconcile browser-uncertain?" });
  await expect(reconcile.getByText(/never creates another server/)).toBeVisible();
  await reconcile.getByRole("button", { name: "Look up server", exact: true }).click();
  await expect(reconcile).toBeHidden();
  await servers.getByRole("button", { name: "Abandon tracking browser-uncertain", exact: true }).click();
  const abandon = page.getByRole("dialog", { name: "Abandon tracking browser-uncertain?" });
  await expect(abandon.getByText("The server may still be billed", { exact: true })).toBeVisible();
  await capture(page, testInfo, "g14-abandon", false);
  await abandon.getByRole("button", { name: "Abandon tracking", exact: true }).click();
  await expect(abandon).toBeHidden();
  expect(mutations).toEqual(['POST reconcile {"confirm":true}', 'DELETE abandon {"confirm":true}']);
  await page.unrouteAll({ behavior: "wait" });

  // Real backend delete, then the list is empty again.
  await page.reload();
  await tokens.getByRole("button", { name: `Delete ${name}`, exact: true }).click();
  const remove = page.getByRole("dialog", { name: `Delete ${name}?` });
  await expect(remove.getByText(/keep running and stay billed/)).toBeVisible();
  await remove.getByRole("button", { name: "Delete token", exact: true }).click();
  await expect(remove).toBeHidden();
  await expect(tokens.getByText(name, { exact: true })).toHaveCount(0);
  expect((await api(page, "GET", "/api/node-providers")).text).toMatch(/^\[\]\s*$/);
  expect(sinkholeHits(), "no provider connection during the whole case").toBe(hitsBefore);
  testInfo.annotations.push({ type: "evidence", description: `real: preflight, token create/edit/delete, sealed-at-rest, no-store; mocked: catalogue (${catalogCalls} call), create (503 lost ack, ${posts.length} POST), reconcile, abandon; sinkhole hits ${sinkholeHits()}` });
});

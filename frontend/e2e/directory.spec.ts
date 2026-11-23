import { execFileSync } from "node:child_process";
import { createHmac } from "node:crypto";
import { expect, request as playwrightRequest, test, type APIRequestContext, type BrowserContext, type Locator, type Page, type TestInfo } from "@playwright/test";

// Identity fixtures (OpenLDAP, the GitHub provider and the fixture control server) exist only when the harness
// is run for this spec or with --identity. Ordinary full-suite runs skip it.
const fixtureURL = process.env.COVE_IDENTITY_FIXTURE_URL ?? "";
const fixtureToken = process.env.COVE_IDENTITY_FIXTURE_TOKEN ?? "";
const adminEmail = process.env.COVE_BROWSER_EMAIL ?? "";
const adminPassword = process.env.COVE_BROWSER_PASSWORD ?? "";
const ldap = {
  host: process.env.COVE_IDENTITY_LDAP_HOST ?? "",
  port: process.env.COVE_IDENTITY_LDAP_PORT ?? "",
  ca: process.env.COVE_IDENTITY_LDAP_CA ?? "",
  password: process.env.COVE_IDENTITY_LDAP_PASSWORD ?? "",
  user: process.env.COVE_IDENTITY_LDAP_USER ?? "",
  email: process.env.COVE_IDENTITY_LDAP_EMAIL ?? "",
};
const githubMemberId = process.env.COVE_IDENTITY_MEMBER_ID ?? "";
const base = process.env.COVE_BROWSER_BASE_PATH ?? "";
const panelPath = (route: string) => `${base}${route}`;

const directoryName = "Corp directory";
const groupDN = "cn=engineering,ou=groups,dc=example,dc=invalid";
const sourceName = "Acme engineering";
const scimEmail = "scim-person@example.invalid";
const scimGroupName = "Provisioned engineers";
const scimUserSchema = "urn:ietf:params:scim:schemas:core:2.0:User";
const scimGroupSchema = "urn:ietf:params:scim:schemas:core:2.0:Group";
const scimPatchSchema = "urn:ietf:params:scim:api:messages:2.0:PatchOp";

interface Member {
  user_id: string;
  email: string;
  role: string;
  managed_by: { source_kind: string; source_id: string; display_name?: string }[] | null;
}

function sql(query: string) {
  const database = process.env.COVE_BROWSER_DATABASE;
  if (!database || !/^cove-browser-[a-f0-9]+-db$/.test(database)) throw new Error("Isolated browser database required");
  return execFileSync("docker", ["exec", database, "psql", "-U", "postgres", "-d", "cove", "-Atc", query], { encoding: "utf8" }).trim();
}

// RFC 6238 with the parameters Cove issues: SHA-1, 6 digits, 30-second steps.
function totp(secret: string, step: number) {
  const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZ234567";
  let bits = "";
  for (const character of secret.replace(/=+$/, "").toUpperCase()) bits += alphabet.indexOf(character).toString(2).padStart(5, "0");
  const key = Buffer.from((bits.match(/.{8}/g) ?? []).map((byte) => parseInt(byte, 2)));
  const counter = Buffer.alloc(8);
  counter.writeBigUInt64BE(BigInt(step));
  const digest = createHmac("sha1", key).update(counter).digest();
  const offset = (digest[digest.length - 1] ?? 0) & 0xf;
  const value = (digest.readUInt32BE(offset) & 0x7fffffff) % 1_000_000;
  return value.toString().padStart(6, "0");
}

const currentStep = () => Math.floor(Date.now() / 30_000);

// Cove refuses a TOTP step that was already used, so wait for the next one.
async function nextStep(page: Page, after: number) {
  while (currentStep() <= after) await page.waitForTimeout(Math.max(250, 30_000 - (Date.now() % 30_000) + 250));
  return currentStep();
}

async function expectNoHorizontalScroll(page: Page) {
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBe(true);
}

// Only called while no token, bind password or directory password is visible on the page.
async function capture(page: Page, testInfo: TestInfo, name: string, target?: Locator) {
  for (const width of [390, 768, 1440]) {
    await page.setViewportSize({ width, height: 900 });
    await expectNoHorizontalScroll(page);
    if (target) await target.scrollIntoViewIfNeeded();
    await page.screenshot({ path: testInfo.outputPath(`${name}-${width}.png`), fullPage: !target });
  }
  await page.setViewportSize({ width: 1440, height: 900 });
}

async function choose(page: Page, combobox: Locator, option: string) {
  await combobox.click();
  const list = page.getByRole("listbox");
  await expect(list).toBeVisible();
  await list.getByRole("option", { name: option, exact: true }).click();
  await expect(list).toBeHidden();
}

async function signInAdmin(page: Page) {
  await page.goto(panelPath("/login"));
  await page.getByLabel("Email", { exact: true }).fill(adminEmail);
  await page.getByLabel("Password", { exact: true }).fill(adminPassword);
  await page.getByRole("button", { name: "Sign in", exact: true }).click();
  await expect(page).not.toHaveURL(/\/login(?:[?#]|$)/);
}

async function directorySignIn(page: Page, password: string) {
  await page.goto(panelPath("/login"));
  await page.getByRole("button", { name: `Continue with ${directoryName}`, exact: true }).click();
  await expect(page.getByRole("heading", { name: `Sign in with ${directoryName}`, exact: true })).toBeVisible();
  await page.getByLabel("Directory username", { exact: true }).fill(ldap.user);
  const field = page.getByLabel("Password", { exact: true });
  await expect(field).toHaveAttribute("type", "password");
  await field.fill(password);
  await field.press("Enter");
}

async function members(api: APIRequestContext, team: string) {
  const response = await api.get(panelPath(`/api/teams/${team}/members`));
  expect(response.status()).toBe(200);
  return (await response.json()) as Member[];
}

async function setRoster(ids: { id: number; login: string }[]) {
  const control = await playwrightRequest.newContext({ baseURL: fixtureURL });
  try {
    const response = await control.post("/fixture/roster", { headers: { Authorization: `Bearer ${fixtureToken}` }, data: { members: ids } });
    expect(response.ok()).toBe(true);
  } finally {
    await control.dispose();
  }
}

const fixtureMember = [{ id: 101, login: "fixture-member" }];

test.describe("directory identity integrations", () => {
  test.skip(!fixtureURL, "identity fixtures not provisioned");

  test("LDAP sign-in, SCIM provisioning, GitHub team sync and member provenance", async ({ page, browser }, testInfo) => {
    test.setTimeout(900_000);
    for (const [name, value] of Object.entries({ COVE_BROWSER_EMAIL: adminEmail, COVE_IDENTITY_FIXTURE_TOKEN: fixtureToken, COVE_IDENTITY_MEMBER_ID: githubMemberId, ...Object.fromEntries(Object.entries(ldap).map(([key, item]) => [`ldap.${key}`, item])) })) {
      if (value === "") throw new Error(`${name} is required by the identity fixture`);
    }
    await page.setViewportSize({ width: 1440, height: 900 });
    await signInAdmin(page);
    const origin = new URL(page.url()).origin;
    const headers = { Origin: origin };
    const contexts: BrowserContext[] = [];
    const isolated: APIRequestContext[] = [];
    const team = "default";
    const teams = (await (await page.request.get(panelPath("/api/settings/directory/teams"))).json()) as { teams: { id: string; name: string }[] };
    const teamName = teams.teams.find((item) => item.id === team)?.name ?? "";
    expect(teamName).not.toBe("");

    try {
      let providerId = "";
      await test.step("configure an LDAP directory and test the saved connection", async () => {
        await page.goto(panelPath("/admin"));
        const section = page.getByRole("region", { name: "LDAP directories", exact: true });
        await section.getByRole("button", { name: "Add directory", exact: true }).click();
        const form = section.getByRole("form", { name: "New LDAP directory", exact: true });
        await form.getByLabel("Display name", { exact: true }).fill(directoryName);
        await form.getByLabel("Host name", { exact: true }).fill(ldap.host);
        await form.getByLabel("Port", { exact: true }).fill(ldap.port);
        await form.getByRole("radio", { name: /^LDAPS/ }).check();
        // The CA label carries an "Optional" marker, so it is matched by its leading text.
        await form.getByLabel("CA certificates").fill(ldap.ca);
        await form.getByLabel("Bind DN", { exact: true }).fill("cn=service,dc=example,dc=invalid");
        await form.getByLabel("Bind password", { exact: true }).fill(ldap.password);
        await form.getByLabel("User search base", { exact: true }).fill("ou=users,dc=example,dc=invalid");
        await form.getByLabel("User filter", { exact: true }).fill("(&(objectClass=inetOrgPerson)(uid={username}))");
        await expect(form.getByRole("combobox", { name: "Immutable ID attribute", exact: true })).toContainText("entryUUID");
        await form.getByLabel("Email attribute", { exact: true }).fill("mail");
        await form.getByLabel("Name attribute", { exact: true }).fill("cn");
        await form.getByLabel("Group search base", { exact: true }).fill("ou=groups,dc=example,dc=invalid");
        await form.getByLabel("Group filter", { exact: true }).fill("(&(objectClass=groupOfNames)(member={dn}))");
        await expect(form.getByRole("switch", { name: "Create accounts for new directory users", exact: true })).not.toBeChecked();
        await form.getByRole("switch", { name: "Create accounts for new directory users", exact: true }).check();
        await form.getByRole("switch", { name: "Allow sign-in with this directory", exact: true }).check();
        await form.getByRole("button", { name: "Add mapping", exact: true }).click();
        await form.getByLabel("Group DN", { exact: true }).fill(groupDN);
        await choose(page, form.getByRole("combobox", { name: "Cove team", exact: true }), teamName);
        await choose(page, form.getByRole("combobox", { name: "Role", exact: true }), "Editor (team-wide)");
        await form.getByRole("button", { name: "Add directory", exact: true }).click();
        await expect(section.getByText("Directory saved", { exact: true })).toBeVisible();
        await expect(form).toHaveCount(0);

        const saved = (await (await page.request.get(panelPath("/api/settings/ldap/providers"))).json()) as { providers: { id: string; enabled: boolean; password_configured: boolean; configuration: { display_name: string; bind_password?: string } }[] };
        const provider = saved.providers.find((item) => item.configuration.display_name === directoryName);
        expect(provider?.enabled).toBe(true);
        expect(provider?.password_configured).toBe(true);
        expect(provider?.configuration.bind_password).toBeUndefined();
        providerId = provider?.id ?? "";
        expect(await page.locator("body").innerText()).not.toContain(ldap.password);

        await section.getByRole("button", { name: `Test connection to ${directoryName}`, exact: true }).click();
        await expect(section.getByText("Connected", { exact: true })).toBeVisible({ timeout: 20_000 });
        await expect(section.getByText(/This does not check the user or group filters/)).toBeVisible();
        await capture(page, testInfo, "directory-ldap-settings", section);
      });

      const aliceContext = await browser.newContext({ ignoreHTTPSErrors: true });
      contexts.push(aliceContext);
      const alice = await aliceContext.newPage();
      await alice.setViewportSize({ width: 1440, height: 900 });
      let aliceId = "";
      await test.step("directory sign-in, then Cove's own second factor", async () => {
        await directorySignIn(alice, ldap.password);
        await expect(alice).not.toHaveURL(/\/login(?:[?#]|$)/);
        const session = (await (await alice.request.get(panelPath("/api/session"))).json()) as { user_id: string; email: string; teams: { id: string }[] };
        expect(session.email).toBe(ldap.email);
        expect(session.teams.map((item) => item.id)).toContain(team);
        aliceId = session.user_id;
        expect((await alice.request.get(panelPath("/api/projects"))).status()).toBe(200);
        const granted = (await members(page.request, team)).find((item) => item.user_id === aliceId);
        expect(granted?.managed_by?.map((source) => source.source_kind)).toContain("ldap");

        const begin = await alice.request.post(panelPath("/api/account/totp"), { headers, data: { current_password: "" } });
        expect(begin.status()).toBe(201);
        const secret = new URL(((await begin.json()) as { provisioning_uri: string }).provisioning_uri).searchParams.get("secret") ?? "";
        expect(secret).not.toBe("");
        const enrolled = currentStep();
        expect((await alice.request.post(panelPath("/api/account/totp/confirm"), { headers, data: { code: totp(secret, enrolled) } })).status()).toBe(200);
        expect((await alice.request.post(panelPath("/auth/logout"), { headers })).ok()).toBe(true);

        await directorySignIn(alice, ldap.password);
        const code = alice.getByLabel("Authenticator or recovery code", { exact: true });
        await expect(code).toBeVisible();
        expect((await alice.request.get(panelPath("/api/session"))).status()).toBe(401);
        expect((await alice.request.get(panelPath("/api/projects"))).status()).toBe(401);
        await capture(alice, testInfo, "directory-second-factor");
        const step = await nextStep(alice, enrolled);
        await code.fill(totp(secret, step));
        await alice.getByRole("button", { name: "Verify and sign in", exact: true }).click();
        await expect(alice).not.toHaveURL(/\/login(?:[?#]|$)/);
        expect((await alice.request.get(panelPath("/api/session"))).status()).toBe(200);
      });

      await test.step("lockout after repeated wrong directory passwords", async () => {
        const attacker = await playwrightRequest.newContext({ baseURL: origin, ignoreHTTPSErrors: true });
        isolated.push(attacker);
        for (let attempt = 0; attempt < 10; attempt += 1) {
          const response = await attacker.post(panelPath(`/auth/ldap/${providerId}/sign-in`), { headers, data: { email: ldap.user, password: `wrong-${attempt}` } });
          expect(response.status()).toBe(401);
        }
        const lockedContext = await browser.newContext({ ignoreHTTPSErrors: true });
        contexts.push(lockedContext);
        const locked = await lockedContext.newPage();
        await locked.setViewportSize({ width: 1440, height: 900 });
        await directorySignIn(locked, ldap.password);
        await expect(locked.getByRole("alert")).toHaveText("Sign-in is paused after repeated failed attempts. Try again in 15 minutes.");
        await expect(locked.getByLabel("Password", { exact: true })).toHaveValue("");
        await capture(locked, testInfo, "directory-lockout");
        // Clears only this fixture account's lockout, so later directory sign-ins stay deterministic.
        sql(`DELETE FROM authentication_failures WHERE key='account:${aliceId}'`);
      });

      const scimBase = `${origin}${base}/scim/v2`;
      const scim = await playwrightRequest.newContext({ ignoreHTTPSErrors: true });
      isolated.push(scim);
      let scimUserId = "";
      let scimGroupId = "";
      await test.step("SCIM token is shown once, provisioning, group mapping with a stale save, rotation and deprovisioning", async () => {
        await page.goto(panelPath("/admin"));
        const section = page.getByRole("region", { name: "SCIM provisioning", exact: true });
        await section.getByRole("button", { name: "Generate token", exact: true }).click();
        const tokenField = section.getByLabel("New SCIM bearer token", { exact: true });
        await expect(tokenField).toBeVisible();
        const firstToken = await tokenField.inputValue();
        expect(firstToken.length).toBeGreaterThan(20);
        await section.getByRole("button", { name: "I have saved the token", exact: true }).click();
        await expect(tokenField).toHaveCount(0);
        await page.reload();
        await expect(section.getByText("Configured", { exact: true })).toBeVisible();
        await expect(section.getByLabel("New SCIM bearer token", { exact: true })).toHaveCount(0);
        expect(await page.locator("body").innerText()).not.toContain(firstToken);
        expect(JSON.stringify(await (await page.request.get(panelPath("/api/settings/scim"))).json())).not.toContain(firstToken);

        await section.getByRole("switch", { name: "Accept provisioning requests", exact: true }).check();
        await section.getByRole("button", { name: "Save changes", exact: true }).click();
        await expect(section.getByText("Provisioning settings saved", { exact: true })).toBeVisible();

        const bearer = (token: string) => ({ Authorization: `Bearer ${token}`, "Content-Type": "application/scim+json" });
        const user = await scim.post(`${scimBase}/Users`, {
          headers: bearer(firstToken),
          data: { schemas: [scimUserSchema], userName: scimEmail, externalId: "scim-browser-person", active: true, displayName: "SCIM browser person", emails: [{ value: scimEmail, primary: true, type: "work" }] },
        });
        expect(user.status()).toBe(201);
        scimUserId = ((await user.json()) as { id: string }).id;
        const group = await scim.post(`${scimBase}/Groups`, { headers: bearer(firstToken), data: { schemas: [scimGroupSchema], displayName: scimGroupName, externalId: "scim-browser-group", members: [{ value: scimUserId }] } });
        expect(group.status()).toBe(201);
        scimGroupId = ((await group.json()) as { id: string }).id;

        await page.reload();
        const groups = section.getByRole("list", { name: "Provisioned groups", exact: true });
        await groups.getByRole("button", { name: `Edit mappings for ${scimGroupName}`, exact: true }).click();
        const editor = section.getByRole("form", { name: `Mappings for ${scimGroupName}`, exact: true });
        await editor.getByRole("button", { name: "Add mapping", exact: true }).click();
        await choose(page, editor.getByRole("combobox", { name: "Cove team", exact: true }), teamName);
        await choose(page, editor.getByRole("combobox", { name: "Role", exact: true }), "Editor (team-wide)");
        await editor.getByRole("button", { name: "Review and save", exact: true }).click();
        const review = page.getByRole("dialog", { name: `Save mappings for ${scimGroupName}?`, exact: true });
        await expect(review).toBeVisible();
        // The identity provider changes the Group while the administrator is reviewing.
        const concurrent = await scim.patch(`${scimBase}/Groups/${scimGroupId}`, { headers: bearer(firstToken), data: { schemas: [scimPatchSchema], Operations: [{ op: "replace", path: "members", value: [{ value: scimUserId }] }] } });
        expect(concurrent.status()).toBe(204);
        await review.getByRole("button", { name: "Save mappings", exact: true }).click();
        await expect(editor.getByText("This group changed", { exact: true })).toBeVisible();
        expect((await members(page.request, team)).some((item) => item.user_id === scimUserId)).toBe(false);
        await editor.getByRole("button", { name: "Cancel", exact: true }).click();
        await expect(editor).toHaveCount(0);

        // Review the Group's current revision before saving again.
        await page.reload();
        await groups.getByRole("button", { name: `Edit mappings for ${scimGroupName}`, exact: true }).click();
        const retry = section.getByRole("form", { name: `Mappings for ${scimGroupName}`, exact: true });
        await retry.getByRole("button", { name: "Add mapping", exact: true }).click();
        await choose(page, retry.getByRole("combobox", { name: "Cove team", exact: true }), teamName);
        await choose(page, retry.getByRole("combobox", { name: "Role", exact: true }), "Editor (team-wide)");
        await retry.getByRole("button", { name: "Review and save", exact: true }).click();
        const confirm = page.getByRole("dialog", { name: `Save mappings for ${scimGroupName}?`, exact: true });
        await confirm.getByRole("button", { name: "Save mappings", exact: true }).click();
        await expect(retry).toHaveCount(0);
        await expect(groups.getByText(`${teamName} · Editor (team-wide)`, { exact: true })).toBeVisible();
        const provisioned = (await members(page.request, team)).find((item) => item.user_id === scimUserId);
        expect(provisioned?.managed_by?.map((source) => source.source_kind)).toContain("scim");
        expect(provisioned?.managed_by?.find((source) => source.source_kind === "scim")?.display_name).toBe("SCIM provisioning");

        // Provenance is checked while the provisioned person is active: deactivation removes their derived team access.
        await page.goto(panelPath("/team"));
        const teamMembers = page.getByRole("region", { name: "Members", exact: true });
        await expect(teamMembers.getByRole("listitem").filter({ hasText: scimEmail }).getByText("Managed by SCIM provisioning", { exact: true })).toBeVisible();
        await page.goto(panelPath("/admin"));

        // Escape cancels the rotation; nothing is rotated until the dialog is confirmed.
        await section.getByRole("button", { name: "Rotate token", exact: true }).click();
        const rotate = page.getByRole("dialog", { name: "Rotate the SCIM token?", exact: true });
        await expect(rotate).toBeVisible();
        await page.keyboard.press("Escape");
        await expect(rotate).toBeHidden();
        expect((await scim.get(`${scimBase}/Users`, { headers: bearer(firstToken) })).status()).toBe(200);
        await section.getByRole("button", { name: "Rotate token", exact: true }).click();
        await expect(rotate).toBeVisible();
        await rotate.getByRole("button", { name: "Rotate token", exact: true }).focus();
        await page.keyboard.press("Enter");
        await expect(tokenField).toBeVisible();
        const secondToken = await tokenField.inputValue();
        expect(secondToken).not.toBe(firstToken);
        await section.getByRole("button", { name: "I have saved the token", exact: true }).click();
        await expect(tokenField).toHaveCount(0);
        expect((await scim.get(`${scimBase}/Users`, { headers: bearer(firstToken) })).status()).toBe(401);
        expect((await scim.get(`${scimBase}/Users`, { headers: bearer(secondToken) })).status()).toBe(200);

        // A stale If-Match is refused and recorded as a failed provisioning change.
        const stale = await scim.patch(`${scimBase}/Groups/${scimGroupId}`, { headers: { ...bearer(secondToken), "If-Match": 'W/"1"' }, data: { schemas: [scimPatchSchema], Operations: [{ op: "replace", path: "displayName", value: "Stale rename" }] } });
        expect(stale.status()).toBe(412);
        const deactivate = await scim.patch(`${scimBase}/Users/${scimUserId}`, { headers: bearer(secondToken), data: { schemas: [scimPatchSchema], Operations: [{ op: "replace", path: "active", value: false }] } });
        expect(deactivate.status()).toBe(200);

        await page.reload();
        const accounts = section.getByRole("list", { name: "Provisioned accounts", exact: true });
        await expect(accounts.getByRole("listitem").filter({ hasText: scimEmail }).getByText("Disabled, sessions and tokens revoked", { exact: true })).toBeVisible();
        const activity = section.getByRole("list", { name: "Provisioning activity", exact: true });
        await expect(activity.getByRole("listitem").filter({ hasText: "Updated user" }).first().getByText("Succeeded", { exact: true })).toBeVisible();
        await expect(activity.getByRole("listitem").filter({ hasText: "Updated group" }).getByText("Failed (HTTP 412)", { exact: true })).toBeVisible();
        await expect(activity.getByRole("listitem").filter({ hasText: "Created group" }).first()).toContainText(scimGroupName);
        await capture(page, testInfo, "directory-scim", section);

        await page.goto(panelPath("/users"));
        const row = page.getByRole("listitem").filter({ hasText: scimEmail });
        await expect(row.getByText("Deactivated by the identity provider: signed out everywhere and API tokens revoked.", { exact: true })).toBeVisible();
        await expect(row).toContainText("Provisioned by SCIM");
        await expect(row).toContainText("Disabled");

        // The disabled account and its SCIM ownership stay, but its directory-only team membership is removed.
        expect((await members(page.request, team)).some((item) => item.user_id === scimUserId)).toBe(false);
        await page.goto(panelPath("/team"));
        const remaining = page.getByRole("region", { name: "Members", exact: true });
        await expect(remaining.getByRole("listitem").filter({ hasText: ldap.email })).toBeVisible();
        await expect(remaining.getByRole("listitem").filter({ hasText: scimEmail })).toHaveCount(0);
      });

      await test.step("GitHub team sync: owner consent, preview, apply, stale refusal and removal", async () => {
        await setRoster([]);
        await page.goto(panelPath("/admin"));
        const section = page.getByRole("region", { name: "GitHub team sync", exact: true });
        await section.getByRole("button", { name: "Add source", exact: true }).click();
        const form = section.getByRole("form", { name: "New GitHub team sync source", exact: true });
        await form.getByLabel("Name", { exact: true }).fill(sourceName);
        await form.getByLabel("GitHub organization", { exact: true }).fill("acme");
        await form.getByRole("radio", { name: "Your GitHub account (OAuth)", exact: true }).check();
        // Paused, so the scheduler cannot apply changes between the explicit preview and apply.
        await form.getByRole("switch", { name: "Refresh automatically", exact: true }).uncheck();
        await form.getByRole("button", { name: "Add mapping", exact: true }).click();
        await form.getByLabel("GitHub team slug", { exact: true }).fill("engineering");
        await choose(page, form.getByRole("combobox", { name: "Cove team", exact: true }), teamName);
        await choose(page, form.getByRole("combobox", { name: "Role", exact: true }), "Editor (team-wide)");
        await form.getByRole("button", { name: "Add source", exact: true }).click();
        await expect(section.getByText("Source saved", { exact: true })).toBeVisible();
        await expect(section.getByText("Authorization needed", { exact: true })).toBeVisible();

        // Only GitHub's authorization page is stood in for; the callback performs the real exchange against the fixture provider.
        await page.route("https://github.com/login/oauth/authorize**", async (route) => {
          const state = new URL(route.request().url()).searchParams.get("state") ?? "";
          await route.fulfill({ status: 302, headers: { Location: `${origin}${base}/auth/github/callback?state=${encodeURIComponent(state)}&code=fixture` } });
        });
        await section.getByRole("button", { name: `Authorize organization access for ${sourceName}`, exact: true }).click();
        await section.getByLabel("Current password", { exact: true }).fill(adminPassword);
        await section.getByRole("button", { name: "Continue to GitHub", exact: true }).click();
        await expect(page.getByText("GitHub organization access authorized", { exact: true })).toBeVisible({ timeout: 20_000 });
        await expect(page.getByTestId("github-team-sync-consent")).toBeFocused();
        await expect.poll(() => new URL(page.url()).search).toBe("");
        expect(new URL(page.url()).hash).toBe("#github-team-sync");
        await page.unroute("https://github.com/login/oauth/authorize**");
        const sources = (await (await page.request.get(panelPath("/api/settings/github-team-sync"))).json()) as { sources: { configuration: { name: string }; credential_configured: boolean }[] };
        expect(sources.sources.find((item) => item.configuration.name === sourceName)?.credential_configured).toBe(true);

        await setRoster(fixtureMember);
        await section.getByRole("button", { name: `Preview changes for ${sourceName}`, exact: true }).click();
        const table = section.getByRole("table", { name: "Membership changes", exact: true });
        const joining = table.getByRole("row").filter({ hasText: "github-member@example.invalid" });
        await expect(joining).toContainText("GitHub fixture member");
        await expect(joining.getByText("Joins", { exact: true })).toBeVisible();
        await capture(page, testInfo, "directory-github-preview", section);
        await section.getByRole("button", { name: "Apply 1 change", exact: true }).click();
        const apply = page.getByRole("dialog", { name: `Apply changes from ${sourceName}?`, exact: true });
        await apply.getByRole("button", { name: "Apply changes", exact: true }).click();
        await expect(section.getByText("Changes applied", { exact: true })).toBeVisible();
        expect((await members(page.request, team)).find((item) => item.user_id === githubMemberId)?.managed_by?.map((source) => source.source_kind)).toContain("github");

        await setRoster([]);
        await section.getByRole("button", { name: "Preview again", exact: true }).click();
        const leaving = section.getByRole("table", { name: "Membership changes", exact: true }).getByRole("row").filter({ hasText: "github-member@example.invalid" });
        await expect(leaving.getByText("Loses access", { exact: true })).toBeVisible();
        await section.getByRole("button", { name: "Apply 1 change", exact: true }).click();
        const removal = page.getByRole("dialog", { name: `Apply changes from ${sourceName}?`, exact: true });
        await expect(removal).toContainText("lose the access this source granted");
        // The roster changes back after the preview was reviewed, so applying it must be refused.
        await setRoster(fixtureMember);
        await removal.getByRole("button", { name: "Apply and remove 1 membership", exact: true }).click();
        await expect(section.getByText("Preview out of date", { exact: true })).toBeVisible();
        await expect(section.getByText(/Nothing was changed. Preview again/)).toBeVisible();
        expect((await members(page.request, team)).some((item) => item.user_id === githubMemberId)).toBe(true);

        await setRoster([]);
        await section.getByRole("button", { name: "Preview", exact: true }).click();
        await expect(section.getByRole("table", { name: "Membership changes", exact: true }).getByText("Loses access", { exact: true })).toBeVisible();
        await section.getByRole("button", { name: "Apply 1 change", exact: true }).click();
        await page.getByRole("dialog", { name: `Apply changes from ${sourceName}?`, exact: true }).getByRole("button", { name: "Apply and remove 1 membership", exact: true }).click();
        await expect(section.getByText("Changes applied", { exact: true })).toBeVisible();
        expect((await members(page.request, team)).some((item) => item.user_id === githubMemberId && (item.managed_by ?? []).some((source) => source.source_kind === "github"))).toBe(false);
      });

      await test.step("member provenance and local owner recovery", async () => {
        await page.goto(panelPath("/team"));
        const list = page.getByRole("region", { name: "Members", exact: true });
        const aliceRow = list.getByRole("listitem").filter({ hasText: ldap.email });
        await expect(aliceRow.getByText(`Managed by LDAP — ${directoryName}`, { exact: true })).toBeVisible();
        await expect(aliceRow.getByRole("button", { name: `Edit ${ldap.email}`, exact: true })).toHaveCount(0);
        await capture(page, testInfo, "directory-team-provenance", list);

        await aliceRow.getByRole("button", { name: `Make local owner ${ldap.email}`, exact: true }).click();
        const promote = page.getByRole("dialog", { name: `Make ${ldap.email} a local owner?`, exact: true });
        await expect(promote).toContainText(`LDAP — ${directoryName}`);
        await promote.getByRole("button", { name: "Make local owner", exact: true }).click();
        await expect(promote).toBeHidden();
        await expect.poll(async () => (await members(page.request, team)).find((item) => item.user_id === aliceId)?.role).toBe("owner");
        await expect(aliceRow.getByText("Owner", { exact: true })).toBeVisible();

        // Ordinary members never receive administrator source names.
        const asMember = await members(alice.request, team);
        expect(asMember.some((item) => (item.managed_by ?? []).length > 0)).toBe(true);
        for (const item of asMember) for (const source of item.managed_by ?? []) expect(source.display_name).toBeUndefined();
        const asAdmin = await members(page.request, team);
        expect(asAdmin.find((item) => item.user_id === aliceId)?.managed_by?.find((source) => source.source_kind === "ldap")?.display_name).toBe(directoryName);

        // Removing the directory's mapping and reconciling on the next directory sign-in keeps the local owner.
        await page.goto(panelPath("/admin"));
        const section = page.getByRole("region", { name: "LDAP directories", exact: true });
        await section.getByRole("button", { name: `Edit ${directoryName}`, exact: true }).click();
        const form = section.getByRole("form", { name: `Edit ${directoryName}`, exact: true });
        await expect(form.getByLabel("Bind password")).toHaveValue("");
        await form.getByRole("button", { name: "Remove mapping 1", exact: true }).click();
        await form.getByRole("button", { name: "Save directory", exact: true }).click();
        await expect(section.getByText("Directory saved", { exact: true })).toBeVisible();
        const reconcile = await playwrightRequest.newContext({ baseURL: origin, ignoreHTTPSErrors: true });
        isolated.push(reconcile);
        const signedIn = await reconcile.post(panelPath(`/auth/ldap/${providerId}/sign-in`), { headers, data: { email: ldap.user, password: ldap.password } });
        expect(signedIn.status()).toBe(200);
        expect(((await signedIn.json()) as { second_factor_required: boolean }).second_factor_required).toBe(true);
        expect((await members(page.request, team)).find((item) => item.user_id === aliceId)?.role).toBe("owner");
      });
    } finally {
      for (const context of isolated) await context.dispose();
      for (const context of contexts) await context.close();
    }
  });
});

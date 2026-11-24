// Packaged ACME DNS-01 acceptance. Runs only in `python scripts/test-browser.py --certificate-dns`, which gives the panel
// a fixed validating Pebble directory, a local verified-TLS stand-in for api.cloudflare.com and a local TXT resolver on
// its own Docker network. Nothing here contacts Cloudflare, public DNS or a public certificate authority, and TLS is
// verified against the fixture's own roots throughout.
import { execFileSync } from "node:child_process";
import { readFileSync } from "node:fs";
import { request as httpsRequest } from "node:https";
import { expect, test, type Locator, type Page, type Request } from "@playwright/test";
import { capture, cleanupBrowserResources, servedFingerprint, verified, type Verified } from "./route-tls";

const password = process.env.COVE_BROWSER_PASSWORD ?? "";
const provider = process.env.COVE_BROWSER_CERTIFICATE_PROVIDER ?? "";
const providerToken = process.env.COVE_BROWSER_CERTIFICATE_TOKEN ?? "";
const fixtureRoot = process.env.COVE_BROWSER_CERTIFICATE_ROOT ?? "";
const issuanceRoot = process.env.COVE_BROWSER_CERTIFICATE_ISSUANCE_ROOT ?? "";
const fixtureZone = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa";

test.skip(!provider, "Needs the --certificate-dns browser fixture");
test.afterEach(cleanupBrowserResources);

const web = "mkdir -p /www; echo dns-certificate-marker > /www/index.html; exec httpd -f -p 8080 -h /www";
const compose = [
  "services:",
  "  web:",
  "    image: busybox:1.37",
  "    pull_policy: never",
  "    init: true",
  "    restart: unless-stopped",
  `    command: ${JSON.stringify(["sh", "-ec", web])}`,
  "    healthcheck:",
  "      test: [CMD, wget, -q, -O, /dev/null, 'http://127.0.0.1:8080/']",
  "      interval: 1s",
  "      timeout: 2s",
  "      retries: 3",
  "",
].join("\n");

interface Operation {
  id: string;
  action: string;
  state: string;
  result: { error?: string } | null;
}

interface Issuer {
  id: string;
  name: string;
  provider: string;
  phase: string;
  certificate_id: string;
  operation_id: string;
  last_error: string;
  directory?: string;
  terms_url?: string;
  terms_accepted_at?: string;
}

interface Certificate {
  id: string;
  name: string;
  source?: string;
  fingerprint: string;
  dns_names: string[];
}

interface SavedRoute {
  domain: string;
  service: string;
  port: number;
  policy?: { certificate_source?: string; certificate_id?: string | null };
}

interface Challenges {
  items: Record<string, unknown>[];
  more: boolean;
}

// A verified HTTPS GET to a fixture endpoint published on the host, trusting only the fixture's own root.
function fixtureGet(url: string, headers: Record<string, string> = {}): Promise<string> {
  const ca = readFileSync(fixtureRoot, "utf8");
  return new Promise((resolve, reject) => {
    const req = httpsRequest(url, { method: "GET", ca, rejectUnauthorized: true, headers, timeout: 15_000 }, (response) => {
      let body = "";
      response.setEncoding("utf8");
      response.on("data", (chunk: string) => (body += chunk));
      response.on("end", () => ((response.statusCode ?? 0) === 200 ? resolve(body) : reject(new Error(`${url} answered ${response.statusCode}: ${body}`))));
      response.on("error", reject);
    });
    req.on("timeout", () => req.destroy(new Error(`Timed out requesting ${url}`)));
    req.on("error", reject);
    req.end();
  });
}

interface ProviderState {
  records: number;
  creates: number;
  deletes: number;
}

const providerState = async () => (JSON.parse(await fixtureGet(`${provider}/fixture/state`, { Authorization: `Bearer ${providerToken}` })) as { result: ProviderState }).result;

async function choose(page: Page, scope: Locator, name: string, option: string) {
  await scope.getByRole("combobox", { name, exact: true }).click();
  await page.getByRole("option", { name: option, exact: true }).click();
}

const marker = "dns-certificate-marker\n";

// Bounded polling that, unlike expect.poll, stops at once when guard throws, so a recorded failure is never retried away.
async function waitUntil(what: string, timeout: number, check: () => Promise<boolean>, guard: () => Promise<void> | void, detail?: () => Promise<string>) {
  const deadline = Date.now() + timeout;
  for (;;) {
    await guard();
    if (await check()) return;
    if (Date.now() >= deadline) throw new Error(`Timed out after ${timeout / 1000}s waiting for ${what}${detail ? `\n${await detail()}` : ""}`);
    await new Promise<void>((resolve) => setTimeout(resolve, 2_000));
  }
}

// Every verified request from the first applied route until the renewed version is served counts toward availability.
// The first TLS, status or body failure is kept, so a brief outage fails the test even when later requests recover.
function availabilityMonitor(hostname: string, ca: string) {
  const started = Date.now();
  let failure: string | undefined;
  let requests = 0;
  let stopped = false;
  let wake = () => {};
  const probe = async (): Promise<Verified | undefined> => {
    requests += 1;
    const at = `${((Date.now() - started) / 1000).toFixed(1)}s`;
    try {
      const result = await verified(hostname, ca);
      if (result.status === 200 && result.body === marker) return result;
      failure ??= `${at}: answered ${result.status} ${JSON.stringify(result.body.slice(0, 200))} with certificate ${result.fingerprint}`;
    } catch (error) {
      failure ??= `${at}: ${error instanceof Error ? error.message : String(error)}`;
    }
    return undefined;
  };
  const check = () => {
    if (failure) throw new Error(`Verified HTTPS to ${hostname} failed after the route was applied; first failure at ${failure} (${requests} requests)`);
  };
  const loop = (async () => {
    while (!stopped) {
      await probe();
      if (stopped) break;
      await new Promise<void>((resolve) => {
        const timer = setTimeout(resolve, 2_000);
        wake = () => {
          clearTimeout(timer);
          resolve();
        };
      });
    }
  })();
  const stop = async () => {
    stopped = true;
    wake();
    await loop;
  };
  return { probe, check, stop };
}

const forbidden = /PRIVATE KEY|CERTIFICATE REQUEST|"(csr|private_key|private_key_pem|order_uri|content|comment|token|txt|value)"/i;

test("ACME DNS-01: request with explicit consent in the dashboard, validate through the fixture DNS provider, route, renew and clean up on the real ingress", async ({ page }, testInfo) => {
  test.setTimeout(900_000);
  const root = await fixtureGet(issuanceRoot);
  expect(root).toContain("-----BEGIN CERTIFICATE-----");
  const initial = await providerState();

  await page.goto("/projects");
  await expect(page).toHaveURL(/\/login(?:[?#]|$)/);
  await page.getByLabel("Password", { exact: true }).fill(password);
  await page.getByRole("button", { name: "Sign in", exact: true }).click();
  await expect(page).not.toHaveURL(/\/login(?:[?#]|$)/);
  const headers = { Origin: new URL(page.url()).origin };
  const json = async <T>(path: string) => {
    const response = await page.request.get(path);
    expect(response.status(), `${path}: ${await response.text()}`).toBe(200);
    return (await response.json()) as T;
  };

  // Zone setup goes through the real API: connect the fixture token, import its zone and share wildcards with the team.
  const session = await json<{ team_id: string }>("/api/session");
  const connected = await page.request.post("/api/cloudflare/connections", { headers, data: { name: "DNS-01 fixture", token: providerToken } });
  expect(connected.status(), await connected.text()).toBe(200);
  const connection = (await connected.json()) as { id: string; zones: { id: string; name: string }[] };
  expect(connection.zones.map((zone) => zone.id)).toContain(fixtureZone);
  const imported = await page.request.post(`/api/cloudflare/connections/${connection.id}/zones`, { headers, data: { zones: [fixtureZone] } });
  expect(imported.ok(), await imported.text()).toBe(true);
  const shared = await page.request.put(`/api/cloudflare/${fixtureZone}/shares`, { headers, data: { shares: [{ team_id: session.team_id, domains: ["*.example.test"] }] } });
  expect(shared.ok(), await shared.text()).toBe(true);

  const nodes = await json<{ id: string; name: string; capabilities: { route_protocol?: number } | null }[]>("/api/nodes");
  const node = nodes.find((item) => item.name === "ready-browser-worker");
  if (!node) throw new Error("The browser worker is missing");
  expect(node.capabilities?.route_protocol ?? 0).toBeGreaterThanOrEqual(4);
  const project = (await (await page.request.post("/api/projects", { headers, data: { name: "DNS-01 certificates project" } })).json()) as { id: string };
  const created = await page.request.post("/api/workloads", { headers, data: { name: "certificate-dns", node_id: node.id, kind: "service", compose, project_id: project.id } });
  expect(created.status(), await created.text()).toBe(201);
  const { id } = (await created.json()) as { id: string };
  // Keeps the scheduled image check away from external registries for this busybox fixture.
  const database = process.env.COVE_BROWSER_DATABASE ?? "";
  if (!/^cove-browser-[a-f0-9]+-db$/.test(database) || !/^[a-f0-9]{32}$/.test(id)) throw new Error("Isolated browser database required");
  execFileSync("docker", ["exec", database, "psql", "-U", "postgres", "-d", "cove", "-Atc", `UPDATE workloads SET next_image_check_at=now()+interval '1 day' WHERE id='${id}'`]);

  const unique = Date.now().toString(36);
  const base = `dns-${unique}.example.test`;
  const wildcard = `*.${base}`;
  const child = `one.${base}`;
  const issuerName = `DNS wildcard ${unique}`;

  const operation = (operationId: string) => json<Operation>(`/api/operations/${operationId}`);
  const settled = async (operationId: string) => {
    await expect.poll(async () => (await operation(operationId)).state, { timeout: 180_000 }).toMatch(/^(succeeded|failed|cancelled|interrupted)$/);
    const result = await operation(operationId);
    if (result.state !== "succeeded") throw new Error(`${result.action} ${operationId} ended ${result.state}: ${result.result?.error ?? ""}\n${await (await page.request.get(`/api/operations/${operationId}/logs`)).text()}`);
  };
  const quiet = () => expect.poll(async () => (await json<Operation[]>(`/api/operations?workload_id=${id}`)).filter((item) => item.state === "queued" || item.state === "running").map((item) => item.action), { timeout: 120_000 }).toEqual([]);
  const issuers = () => json<Issuer[]>(`/api/workloads/${id}/certificate-issuers`);
  const certificates = () => json<Certificate[]>(`/api/workloads/${id}/certificates`);
  const savedRoutes = async () => (await json<{ routes?: SavedRoute[] }>(`/api/workloads/${id}`)).routes ?? [];
  const challenges = async () => {
    const response = await page.request.get(`/api/workloads/${id}/certificate-challenges`);
    expect(response.status()).toBe(200);
    const text = await response.text();
    expect(text).not.toMatch(forbidden);
    return JSON.parse(text) as Challenges;
  };
  const currentIssuer = async () => (await issuers()).find((item) => item.name === issuerName);
  const issuerReport = async () => {
    const current = await currentIssuer();
    if (!current) return `Issuer ${issuerName} is missing`;
    const lines = [`Issuer ${issuerName} is ${current.phase}${current.last_error ? `: ${current.last_error}` : ""}`];
    if (current.operation_id) {
      const latestOperation = await operation(current.operation_id);
      lines.push(`${latestOperation.action} ${latestOperation.id} ${latestOperation.state}${latestOperation.result?.error ? `: ${latestOperation.result.error}` : ""}`, await (await page.request.get(`/api/operations/${latestOperation.id}/logs`)).text());
    }
    lines.push(`Challenges: ${JSON.stringify((await challenges()).items)}`);
    return lines.join("\n");
  };
  // A failed issuer does not recover on its own, so waiting on it would only hold the shared fixture until the timeout.
  const issuerHealthy = async () => {
    const current = await currentIssuer();
    if (!current || current.phase === "failed") throw new Error(`The certificate issuer failed\n${await issuerReport()}`);
  };
  const issuerPhase = (phase: string, timeout: number) => waitUntil(`issuer ${issuerName} to be ${phase}`, timeout, async () => (await currentIssuer())?.phase === phase, issuerHealthy, issuerReport);

  const deploy = await page.request.post(`/api/workloads/${id}/operations`, { headers, data: { action: "deploy" } });
  expect(deploy.status(), await deploy.text()).toBe(202);
  await settled(((await deploy.json()) as { id: string }).id);

  const authorityRequests: Request[] = [];
  const issuerPosts: Request[] = [];
  page.on("request", (request) => {
    const path = new URL(request.url()).pathname;
    if (path === `/api/workloads/${id}/certificate-authority`) authorityRequests.push(request);
    if (path === `/api/workloads/${id}/certificate-issuers` && request.method() === "POST") issuerPosts.push(request);
  });

  // The default Origin CA view never contacts the (possibly public) authority.
  await page.goto(`/resources/${id}/domains`);
  const form = page.getByRole("form", { name: "Request a certificate" });
  await expect(page.getByText("No certificates have been requested for this resource.")).toBeVisible({ timeout: 30_000 });
  await expect(form.getByRole("radio", { name: /Cloudflare Origin CA/ })).toBeChecked();
  await expect(page.getByText("Trusted by Cloudflare, not by browsers")).toBeVisible();
  expect(authorityRequests).toHaveLength(0);

  const authorityResponse = page.waitForResponse((response) => new URL(response.url()).pathname === `/api/workloads/${id}/certificate-authority`);
  await form.getByRole("radio", { name: /ACME DNS-01/ }).check();
  const authority = (await (await authorityResponse).json()) as { directory: string; terms_url: string; terms_text: string };
  expect(authority.directory).toBe("https://pebble:14000/dir");
  await expect(form.getByText(authority.directory, { exact: true })).toBeVisible();
  if (authority.terms_text) {
    await expect(form.getByRole("region", { name: "Terms of service" })).toHaveText(authority.terms_text);
  }
  expect(await page.locator('a[href^="data:"]').count()).toBe(0);
  const consent = form.getByRole("checkbox", { name: "I accept this certificate authority's terms of service" });
  await expect(consent).not.toBeChecked();

  await form.getByLabel("Name", { exact: true }).fill(issuerName);
  await choose(page, form, "Cloudflare zone", "example.test");
  await form.getByLabel("Hostnames", { exact: true }).fill(wildcard);
  await form.getByRole("button", { name: "Request certificate" }).click();
  await expect(form.getByText("Accept the certificate authority's terms to request a certificate.")).toBeVisible();
  await expect(consent).toBeFocused();
  expect(issuerPosts).toHaveLength(0);
  await capture(page, testInfo, "certificate-dns-consent");

  await consent.check();
  const posted = page.waitForResponse((response) => new URL(response.url()).pathname === `/api/workloads/${id}/certificate-issuers` && response.request().method() === "POST");
  await form.getByRole("button", { name: "Request certificate" }).click();
  const postResponse = await posted;
  expect(postResponse.status(), await postResponse.text()).toBe(202);
  expect(postResponse.request().postDataJSON()).toEqual({ name: issuerName, provider: "acme_dns", zone_id: fixtureZone, domains: [wildcard], validity_days: 0, auto_renew: true, accept_terms: true, terms_url: authority.terms_url });
  await expect(form.getByText("Certificate requested")).toBeVisible();
  await expect(consent).not.toBeChecked();

  // The panel publishes the TXT record through the fixture provider, waits for it in DNS and Pebble validates it.
  await expect(page.getByText("DNS challenge records", { exact: true })).toBeVisible({ timeout: 30_000 });
  await capture(page, testInfo, "certificate-dns-requesting");
  await issuerPhase("issued", 240_000);
  const issuer = await currentIssuer();
  if (!issuer?.certificate_id) throw new Error("The issued certificate is missing");
  expect(issuer).toMatchObject({ provider: "acme_dns", directory: authority.directory, terms_url: authority.terms_url });
  expect(issuer.terms_accepted_at).toBeTruthy();
  expect(JSON.stringify(await issuers())).not.toMatch(forbidden);
  const issuerRow = page.getByRole("list", { name: "Certificate issuers" }).getByRole("listitem").filter({ hasText: issuerName });
  await expect(issuerRow.getByText("Issued", { exact: true })).toBeVisible({ timeout: 30_000 });
  await expect(issuerRow.getByText(authority.directory)).toBeVisible();
  expect((await providerState()).creates).toBeGreaterThan(initial.creates);

  // Select the issuer's latest version on a wildcard route in the dashboard; renewal may already have replaced it.
  const latest = async () => {
    const current = (await issuers()).find((item) => item.id === issuer.id);
    const version = (await certificates()).find((item) => item.id === current?.certificate_id);
    if (!version) throw new Error("The issuer's latest version is not stored");
    return version;
  };
  const first = await latest();
  expect(first).toMatchObject({ source: "acme_dns", dns_names: [wildcard] });
  expect(JSON.stringify(await certificates())).not.toMatch(forbidden);
  await page.reload();
  const routes = page.getByRole("list", { name: "Routes" }).getByRole("listitem").filter({ has: page.getByLabel("Hostname", { exact: true }) });
  await expect(page.getByRole("list", { name: "Stored certificates" }).getByText(first.name, { exact: true })).toBeVisible({ timeout: 30_000 });
  if ((await routes.count()) === 0) await page.getByRole("button", { name: "Add route", exact: true }).click();
  const row = routes.first();
  await row.getByLabel("Hostname", { exact: true }).fill(wildcard);
  await row.getByLabel("Compose service", { exact: true }).fill("web");
  await row.getByLabel("Container port", { exact: true }).fill("8080");
  const toggle = row.getByRole("button", { name: /^Advanced routing/ });
  if ((await toggle.getAttribute("aria-expanded")) !== "true") await toggle.click();
  await choose(page, row, "Certificate", "Stored certificate");
  const selected = await latest();
  await choose(page, row, "Certificate version", `${selected.name} · ACME DNS-01`);
  await expect(row.getByText(/Trusted by clients that trust the certificate authority the panel operator configured/)).toBeVisible();

  await quiet();
  const save = page.getByRole("button", { name: "Save and apply", exact: true });
  await expect(save).toBeEnabled({ timeout: 30_000 });
  const pending = page.waitForResponse((response) => new URL(response.url()).pathname === `/api/workloads/${id}/routes` && response.request().method() === "PUT");
  await save.click();
  const routeResponse = await pending;
  expect(routeResponse.status(), await routeResponse.text()).toBe(202);
  expect((routeResponse.request().postDataJSON() as { routes: SavedRoute[] }).routes).toEqual([{ domain: wildcard, service: "web", port: 8080, policy: { certificate_source: "uploaded", certificate_id: selected.id } }]);
  await settled(((await routeResponse.json()) as { id: string }).id);

  // The real ingress serves the selected leaf for a name one label below the wildcard, verified against Pebble's root.
  const routedFingerprint = async () => {
    const routed = (await savedRoutes())[0]?.policy?.certificate_id;
    return (await certificates()).find((item) => item.id === routed)?.fingerprint;
  };
  await expect.poll(async () => (await servedFingerprint(child, root)) === (await routedFingerprint()), { timeout: 90_000 }).toBe(true);
  expect(await verified(child, root)).toMatchObject({ status: 200, body: marker });

  // Normal short-lived renewal: a new version is issued and applied to the route through a route operation, while
  // verified HTTPS keeps answering from the first applied route until the renewed version is served.
  const availability = availabilityMonitor(child, root);
  try {
    await waitUntil("a renewed version to be issued and routed", 300_000, async () => {
      const current = await currentIssuer();
      const routed = (await savedRoutes())[0]?.policy?.certificate_id;
      return current?.phase === "issued" && routed !== undefined && routed !== first.id && routed === current.certificate_id;
    }, async () => {
      availability.check();
      await issuerHealthy();
    }, issuerReport);
    const renewed = await currentIssuer();
    if (renewed?.operation_id) await settled(renewed.operation_id);
    availability.check();
    await waitUntil("the renewed version to be served", 120_000, async () => {
      const served = (await availability.probe())?.fingerprint;
      return served !== undefined && served !== first.fingerprint && served === (await routedFingerprint());
    }, availability.check);
  } finally {
    await availability.stop();
  }
  availability.check();
  expect(await verified(child, root)).toMatchObject({ status: 200, body: marker });
  expect((await certificates()).some((item) => item.id === first.id)).toBe(true);

  // Stop renewing through the dashboard so later steps are not raced by the next cycle.
  await page.reload();
  await issuerRow.getByRole("button", { name: `Actions for issuer ${issuerName}` }).click();
  await page.getByRole("menuitem", { name: "Turn off automatic renewal" }).click();
  await expect(issuerRow.getByText(/Automatic renewal off/)).toBeVisible({ timeout: 30_000 });
  await issuerPhase("issued", 240_000);
  await quiet();
  await expect.poll(async () => (await challenges()).items.length, { timeout: 180_000 }).toBe(0);
  await capture(page, testInfo, "certificate-dns-issued");

  // Remove the issuer in the dashboard. The routed version keeps serving; DNS cleanup is not claimed instantly.
  const servedBefore = await servedFingerprint(child, root);
  await issuerRow.getByRole("button", { name: `Actions for issuer ${issuerName}` }).click();
  await page.getByRole("menuitem", { name: "Remove issuer" }).click();
  const dialog = page.getByRole("dialog", { name: `Remove issuer ${issuerName}?` });
  await expect(dialog).toContainText("does not clear them at once");
  await dialog.getByRole("button", { name: "Remove issuer" }).click();
  await expect(dialog).toBeHidden({ timeout: 30_000 });
  await expect(page.getByText("No certificates have been requested for this resource.")).toBeVisible({ timeout: 30_000 });
  expect(await servedFingerprint(child, root)).toBe(servedBefore);
  expect(await verified(child, root)).toMatchObject({ status: 200, body: marker });

  // The first version is no longer routed or an issuer's latest, so it can be deleted in the dashboard.
  await quiet();
  const stored = page.getByRole("list", { name: "Stored certificates" });
  const deleteFirst = stored.getByRole("button", { name: `Delete certificate ${first.name}` });
  await expect(deleteFirst).toBeEnabled({ timeout: 30_000 });
  await deleteFirst.click();
  const confirm = page.getByRole("dialog", { name: `Delete certificate ${first.name}?` });
  await confirm.getByRole("button", { name: "Delete certificate" }).click();
  await expect(confirm).toBeHidden({ timeout: 30_000 });
  await expect(stored.getByText(first.name, { exact: true })).toBeHidden();
  expect(await verified(child, root)).toMatchObject({ status: 200, body: marker });

  // Every owned fixture record is confirmed removed, and nothing remains outstanding for the resource or the zone.
  await expect.poll(async () => (await providerState()).records, { timeout: 180_000 }).toBe(0);
  const final = await providerState();
  expect(final.deletes - initial.deletes).toBe(final.creates - initial.creates);
  expect((await challenges()).items).toEqual([]);
  const zoneChallenges = await page.request.get(`/api/cloudflare/${fixtureZone}/certificate-challenges`);
  expect(zoneChallenges.status()).toBe(200);
  expect(await zoneChallenges.text()).not.toMatch(forbidden);

  await page.goto("/integrations");
  await page.getByRole("button", { name: "DNS challenges for example.test" }).click();
  await expect(page.getByText("DNS challenge records in example.test")).toBeVisible();
  await expect(page.getByText("No outstanding DNS challenge records.")).toBeVisible({ timeout: 30_000 });
  await capture(page, testInfo, "certificate-dns-zone-owner");
  expect(authorityRequests.length).toBeGreaterThan(0);
});

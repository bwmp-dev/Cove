// Mocked regression for pending public certificate retries on Domains and the operation DNS publication warning. The panel,
// session, node and resource are real; only the retry status, the resource's saved routes and one operation are mocked, so
// no route is applied and no certificate authority or DNS provider is contacted.
import { expect, test, type Page, type Route as Intercepted, type TestInfo } from "@playwright/test";
import { capture, expectNoHorizontalScroll } from "./route-tls";

const password = process.env.COVE_BROWSER_PASSWORD ?? "";
const base = process.env.COVE_BROWSER_BASE_PATH ?? "";
const panelPath = (route: string) => `${base}${route}`;

interface SavedRoute {
  protocol?: string;
  domain: string;
  service: string;
  port: number;
  policy?: Record<string, unknown>;
}

type Reply = Parameters<Intercepted["fulfill"]>[0];

// The shared capture covers 320, 390, 768 and 1440; the dashboard conventions also check 1280 and 1920.
async function captureAll(page: Page, testInfo: TestInfo, name: string) {
  await capture(page, testInfo, name);
  for (const width of [1280, 1920]) {
    await page.setViewportSize({ width, height: 900 });
    await expectNoHorizontalScroll(page);
    await page.screenshot({ path: testInfo.outputPath(`${name}-${width}.png`), fullPage: true });
  }
  await page.setViewportSize({ width: 1280, height: 900 });
}

const compose = "services:\n  web:\n    image: busybox:1.37\n    pull_policy: never\n    command: [sleep, infinity]\n";

test("certificate retries: pending public hostname, failed status read and operation DNS warning across widths", async ({ page }, testInfo) => {
  test.setTimeout(120_000);
  const errors: string[] = [];
  page.on("pageerror", (error) => errors.push(error.message));
  await page.goto(panelPath("/projects"));
  await expect(page).toHaveURL(/\/login(?:[?#]|$)/);
  await page.getByLabel("Password", { exact: true }).fill(password);
  await page.getByRole("button", { name: "Sign in", exact: true }).click();
  await expect(page).not.toHaveURL(/\/login(?:[?#]|$)/);
  const headers = { Origin: new URL(page.url()).origin };

  const nodes = (await (await page.request.get(panelPath("/api/nodes"))).json()) as { id: string; name: string }[];
  const node = nodes.find((item) => item.name === "ready-browser-worker");
  if (!node) throw new Error("The browser worker is missing");
  const project = (await (await page.request.post(panelPath("/api/projects"), { headers, data: { name: "Certificate retries project" } })).json()) as { id: string };
  const created = await page.request.post(panelPath("/api/workloads"), { headers, data: { name: "certificate-retries", node_id: node.id, kind: "service", compose, project_id: project.id } });
  expect(created.status(), await created.text()).toBe(201);
  const { id } = (await created.json()) as { id: string };
  // The real controller, worker RPC and base path, before any mock: an undeployed resource has no pending retries.
  const real = await page.request.get(panelPath(`/api/workloads/${id}/certificate-retries`));
  expect(real.status(), await real.text()).toBe(200);
  expect(await real.json()).toEqual([]);

  const stamp = Date.now();
  const exact = `pending-${stamp}.example.test`;
  const stale = `removed-${stamp}.example.test`;
  const privateHost = `private-${stamp}.example.test`;
  const defaultHost = `default-${stamp}.example.test`;
  const storedHost = `stored-${stamp}.example.test`;
  const wildcard = `*.wild-${stamp}.example.test`;
  const routes: SavedRoute[] = [
    { domain: exact, service: "web", port: 8080 },
    { domain: privateHost, service: "web", port: 8080, policy: { visibility: "lan", listener: "lan" } },
    { domain: defaultHost, service: "web", port: 8080, policy: { certificate_source: "default" } },
    { domain: storedHost, service: "web", port: 8080, policy: { certificate_source: "uploaded", certificate_id: "c".repeat(32) } },
    { domain: wildcard, service: "web", port: 8080 },
    { protocol: "tcp", domain: "", service: "web", port: 5432, policy: { visibility: "lan", listener: "lan" } },
  ];
  const workloadPath = `**/api/workloads/${id}`;
  await page.route(workloadPath, async (route: Intercepted) => {
    if (route.request().method() !== "GET") return route.continue();
    const response = await route.fetch();
    const workload = (await response.json()) as Record<string, unknown>;
    return route.fulfill({ response, json: { ...workload, routes } });
  });

  const message = "Certificate issuance is still pending. Cove retries automatically; check this hostname's DNS and public reachability on ports 80 and 443. Repeated failures slow retries to once every six hours.";
  const retry = (hostname: string) => ({ hostname, attempts: 2, next_attempt_at: new Date(Date.now() + 60 * 60_000).toISOString(), message });
  let reply: () => Reply = () => ({ json: [retry(exact), retry(stale), retry(privateHost), retry(defaultHost), retry(storedHost), retry(wildcard)] });
  let requests = 0;
  await page.route(`**/api/workloads/${id}/certificate-retries`, (route: Intercepted) => {
    requests += 1;
    return route.fulfill(reply());
  });

  // Only the current exact public Let's Encrypt hostname is listed, with no manual retry and no per-host attempt claim.
  await page.goto(panelPath(`/resources/${id}/domains`));
  const domains = page.getByRole("region", { name: "Domains", exact: true });
  const pending = domains.getByRole("list", { name: "Pending certificate retries" });
  await expect(pending).toBeVisible({ timeout: 30_000 });
  await expect(domains.getByText("Certificate issuance pending", { exact: true })).toBeVisible();
  await expect(pending.getByRole("listitem")).toHaveCount(1);
  await expect(pending.getByText(exact, { exact: true })).toBeVisible();
  await expect(pending.getByText(/^Next automatic retry due/)).toBeVisible();
  await expect(pending.getByText(message, { exact: true })).toBeVisible();
  for (const excluded of [stale, privateHost, defaultHost, storedHost, wildcard]) await expect(pending.getByText(excluded)).toHaveCount(0);
  await expect(pending.getByText(/configuration retr/)).toHaveCount(0);
  await expect(pending.getByRole("button")).toHaveCount(0);
  await captureAll(page, testInfo, "certificate-retries-pending");

  // A failed status read replaces the cached list with an error and Retry; the route controls stay available.
  reply = () => ({ status: 503, contentType: "text/plain", body: "certificate retry status unavailable" });
  await page.reload();
  const failure = domains.getByRole("alert").filter({ hasText: "Could not load certificate retry status" });
  await expect(failure).toBeVisible({ timeout: 30_000 });
  await expect(pending).toHaveCount(0);
  await expect(domains.getByText("Certificate issuance pending", { exact: true })).toHaveCount(0);
  // The resource is never deployed, so the form saves routes for the first deploy instead of applying them.
  await expect(domains.getByText("Not deployed yet", { exact: true })).toBeVisible();
  await expect(domains.getByRole("button", { name: "Save routes", exact: true })).toBeVisible();
  await expect(domains.getByLabel("Hostname", { exact: true }).first()).toBeVisible();
  await captureAll(page, testInfo, "certificate-retries-error");

  // An empty list after Retry shows no pending notice and never reads as an issued certificate.
  reply = () => ({ json: [] });
  const before = requests;
  await failure.getByRole("button", { name: "Retry", exact: true }).click();
  await expect.poll(() => requests).toBeGreaterThan(before);
  await expect(failure).toHaveCount(0);
  await expect(pending).toHaveCount(0);
  await expect(domains.getByText("Certificate issuance pending", { exact: true })).toHaveCount(0);
  await expect(domains.getByText(/certificate (is |was )?(issued|valid)/i)).toHaveCount(0);
  await page.unroute(workloadPath);

  // A succeeded deployment keeps the control plane's DNS publication warning as a warning, not an error.
  const operationId = "d".repeat(32);
  const createdAt = new Date(Date.now() - 5 * 60_000).toISOString();
  const warning = `DNS publication for ${exact} could not be confirmed within two minutes. Deployment continued; Cove will keep reconciling DNS and retrying certificate issuance. Check the Cloudflare connection and hostname configuration.`;
  await page.route(`**/api/operations/${operationId}`, (route: Intercepted) =>
    route.fulfill({
      json: {
        id: operationId,
        workload_id: id,
        action: "deploy",
        state: "succeeded",
        created_at: createdAt,
        can_override: false,
        can_approve: false,
        reason: "",
        result: { id: operationId, workload: id, action: "deploy", state: "succeeded", created_at: createdAt, finished_at: createdAt, dns_warning: warning },
      },
    }),
  );
  await page.route(`**/api/operations/${operationId}/logs`, (route: Intercepted) => route.fulfill({ json: { logs: "" } }));
  await page.goto(panelPath(`/operations/${operationId}`));
  await expect(page.getByText("DNS publication was not confirmed", { exact: true })).toBeVisible({ timeout: 30_000 });
  await expect(page.getByText(warning, { exact: true })).toBeVisible();
  await expect(page.getByText("Succeeded", { exact: true }).first()).toBeVisible();
  await expect(page.getByRole("alert").filter({ hasText: warning })).toHaveCount(0);
  await expect(page.getByText(/DNS (has )?propagated|certificate (was )?issued/i)).toHaveCount(0);
  await captureAll(page, testInfo, "certificate-retries-operation-dns-warning");
  expect(errors).toEqual([]);
});

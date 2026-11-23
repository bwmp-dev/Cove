import { expect, test, type Locator, type Page } from "@playwright/test";
import { capture, certificateAuthority, cleanupBrowserResources, servedFingerprint, verified } from "./route-tls";

const password = process.env.COVE_BROWSER_PASSWORD ?? "";

test.afterEach(cleanupBrowserResources);

// Two services that each answer / and /deep/ with their own marker, so responses show which route matched.
const site = (marker: string) => `mkdir -p /www/deep; echo ${marker} > /www/index.html; echo ${marker}-deep > /www/deep/index.html; exec httpd -f -p 8080 -h /www`;
const service = (name: string, marker: string) => [
  `  ${name}:`,
  "    image: busybox:1.37",
  "    pull_policy: never",
  "    init: true",
  "    restart: unless-stopped",
  `    command: ${JSON.stringify(["sh", "-ec", site(marker)])}`,
  "    healthcheck:",
  "      test: [CMD, wget, -q, -O, /dev/null, 'http://127.0.0.1:8080/']",
  "      interval: 1s",
  "      timeout: 2s",
  "      retries: 3",
];
const compose = ["services:", ...service("wild", "wildcard-marker"), ...service("exact", "exact-marker"), ""].join("\n");

interface Operation {
  id: string;
  action: string;
  state: string;
  result: { error?: string } | null;
}

interface SavedRoute {
  domain: string;
  service: string;
  port: number;
  policy?: Record<string, unknown>;
}

async function choose(page: Page, scope: Locator, name: string, option: string) {
  await scope.getByRole("combobox", { name, exact: true }).click();
  await page.getByRole("option", { name: option, exact: true }).click();
}

test("wildcard routes: upload a wildcard certificate, save one-label wildcard routes in the dashboard and verify SNI, scope and exact-host priority on the real ingress", async ({ page }, testInfo) => {
  test.setTimeout(420_000);
  const authority = certificateAuthority();
  try {
    await page.goto("/projects");
    await expect(page).toHaveURL(/\/login(?:[?#]|$)/);
    await page.getByLabel("Password", { exact: true }).fill(password);
    await page.getByRole("button", { name: "Sign in", exact: true }).click();
    await expect(page).not.toHaveURL(/\/login(?:[?#]|$)/);
    const headers = { Origin: new URL(page.url()).origin };

    const nodes = (await (await page.request.get("/api/nodes")).json()) as { id: string; name: string; capabilities: { route_protocol?: number } | null }[];
    const node = nodes.find((item) => item.name === "ready-browser-worker");
    if (!node) throw new Error("The browser worker is missing");
    expect(node.capabilities?.route_protocol ?? 0).toBeGreaterThanOrEqual(4);
    const project = (await (await page.request.post("/api/projects", { headers, data: { name: "Wildcard routes project" } })).json()) as { id: string };
    const created = await page.request.post("/api/workloads", { headers, data: { name: "route-wildcard", node_id: node.id, kind: "service", compose, project_id: project.id } });
    expect(created.status(), await created.text()).toBe(201);
    const { id } = (await created.json()) as { id: string };
    const base = `wild-${Date.now()}.cove.test`;
    const wildcard = `*.${base}`;
    const child = `one.${base}`;
    const priority = `priority.${base}`;
    const issued = authority.issue("wildcard", wildcard, [wildcard]);

    const operation = async (operationId: string) => (await (await page.request.get(`/api/operations/${operationId}`)).json()) as Operation;
    const settled = async (operationId: string) => {
      await expect.poll(async () => (await operation(operationId)).state, { timeout: 180_000 }).toMatch(/^(succeeded|failed|cancelled|interrupted)$/);
      const result = await operation(operationId);
      if (result.state !== "succeeded") throw new Error(`${result.action} ${operationId} ended ${result.state}: ${result.result?.error ?? ""}\n${await (await page.request.get(`/api/operations/${operationId}/logs`)).text()}`);
    };
    const quiet = () => expect.poll(async () => ((await (await page.request.get(`/api/operations?workload_id=${id}`)).json()) as Operation[]).filter((item) => item.state === "queued" || item.state === "running").map((item) => item.action), { timeout: 120_000 }).toEqual([]);

    const deploy = await page.request.post(`/api/workloads/${id}/operations`, { headers, data: { action: "deploy" } });
    expect(deploy.status(), await deploy.text()).toBe(202);
    await settled(((await deploy.json()) as { id: string }).id);

    // Upload the wildcard certificate in the dashboard.
    await page.goto(`/resources/${id}/domains`);
    const uploadForm = page.getByRole("form", { name: "Upload a certificate" });
    await expect(page.getByText("No certificates are stored for this resource.")).toBeVisible({ timeout: 30_000 });
    await uploadForm.getByLabel("Name", { exact: true }).fill("Browser CA wildcard");
    await uploadForm.getByLabel("Certificate chain", { exact: true }).fill(issued.chain);
    await uploadForm.getByLabel("Private key", { exact: true }).fill(issued.key);
    const uploaded = page.waitForResponse((response) => new URL(response.url()).pathname === `/api/workloads/${id}/certificates` && response.request().method() === "POST");
    await uploadForm.getByRole("button", { name: "Upload certificate" }).click();
    const uploadResponse = await uploaded;
    expect(uploadResponse.status(), await uploadResponse.text()).toBe(201);
    const metadata = (await uploadResponse.json()) as { id: string; fingerprint: string; dns_names: string[]; source?: string };
    expect(metadata.dns_names).toEqual([wildcard]);
    expect(metadata.source).toBe("uploaded");
    await expect(page.getByRole("list", { name: "Stored certificates" }).getByText("Browser CA wildcard", { exact: true })).toBeVisible();

    // Routes: the wildcard on / and on a longer /deep prefix, and an exact child that must also select a stored certificate.
    const routes = page.getByRole("list", { name: "Routes" }).getByRole("listitem").filter({ has: page.getByLabel("Hostname", { exact: true }) });
    const fill = async (index: number, domain: string, name: string, path = "") => {
      if ((await routes.count()) <= index) await page.getByRole("button", { name: "Add route", exact: true }).click();
      const row = routes.nth(index);
      await row.getByLabel("Hostname", { exact: true }).fill(domain);
      await row.getByLabel("Compose service", { exact: true }).fill(name);
      await row.getByLabel("Container port", { exact: true }).fill("8080");
      const toggle = row.getByRole("button", { name: /^Advanced routing/ });
      if ((await toggle.getAttribute("aria-expanded")) !== "true") await toggle.click();
      if (path) await row.getByLabel("Path prefix").fill(path);
      await choose(page, row, "Certificate", "Stored certificate");
      await choose(page, row, "Certificate version", "Browser CA wildcard · Uploaded");
    };
    await fill(0, wildcard, "wild");
    await expect(page.getByText(/Matches exactly one label below/).first()).toBeVisible();
    await fill(1, wildcard, "wild", "/deep");
    await fill(2, priority, "exact");
    await capture(page, testInfo, "route-wildcard-editor");

    await quiet();
    const save = page.getByRole("button", { name: "Save and apply", exact: true });
    await expect(save).toBeEnabled({ timeout: 30_000 });
    const pending = page.waitForResponse((response) => new URL(response.url()).pathname === `/api/workloads/${id}/routes` && response.request().method() === "PUT");
    await save.click();
    const response = await pending;
    expect(response.status(), await response.text()).toBe(202);
    const sent = response.request().postDataJSON() as { routes: SavedRoute[] };
    const stored = { certificate_source: "uploaded", certificate_id: metadata.id };
    expect(sent.routes).toEqual([
      { domain: wildcard, service: "wild", port: 8080, policy: stored },
      { domain: wildcard, service: "wild", port: 8080, policy: { path_prefix: "/deep", ...stored } },
      { domain: priority, service: "exact", port: 8080, policy: stored },
    ]);
    await settled(((await response.json()) as { id: string }).id);
    await expect(page.getByText("Routes applied", { exact: true })).toBeVisible({ timeout: 30_000 });
    expect(((await (await page.request.get(`/api/workloads/${id}`)).json()) as { routes?: SavedRoute[] }).routes).toEqual(sent.routes);

    // One label below the wildcard is served the uploaded wildcard over verified SNI and reaches the wildcard routes.
    await expect.poll(() => servedFingerprint(child, authority.ca), { timeout: 60_000 }).toBe(metadata.fingerprint);
    expect(await verified(child, authority.ca)).toMatchObject({ status: 200, body: "wildcard-marker\n" });
    expect(await verified(child, authority.ca, "/deep/")).toMatchObject({ status: 200, body: "wildcard-marker-deep\n" });

    // The explicit hostname wins on every path, including the longer wildcard /deep prefix.
    expect(await verified(priority, authority.ca)).toMatchObject({ fingerprint: metadata.fingerprint, status: 200, body: "exact-marker\n" });
    expect(await verified(priority, authority.ca, "/deep/")).toMatchObject({ status: 200, body: "exact-marker-deep\n" });

    // The apex and deeper names are outside the wildcard: the ingress does not present a certificate the CA issued for them.
    expect(await servedFingerprint(base, authority.ca)).toMatch(/^rejected: /);
    expect(await servedFingerprint(`a.b.${base}`, authority.ca)).toMatch(/^rejected: /);

    // The certificate cannot be deleted while saved routes select it.
    await expect(page.getByRole("list", { name: "Stored certificates" }).getByRole("button", { name: "Delete certificate Browser CA wildcard" })).toBeDisabled();
    await capture(page, testInfo, "route-wildcard-applied");
  } finally {
    authority.dispose();
  }
});

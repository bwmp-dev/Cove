import { execFileSync, spawn } from "node:child_process";
import { createHash } from "node:crypto";
import { existsSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { request as httpsRequest } from "node:https";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { Duplex } from "node:stream";
import { connect, type TLSSocket } from "node:tls";
import { expect, test, type APIResponse, type Page, type TestInfo } from "@playwright/test";

const password = process.env.COVE_BROWSER_PASSWORD ?? "";

test.afterEach(() => {
  const database = process.env.COVE_BROWSER_DATABASE;
  if (!database || !/^cove-browser-[a-f0-9]+-db$/.test(database)) throw new Error("Isolated browser database required for cleanup");
  const resources = execFileSync("docker", ["exec", database, "psql", "-U", "postgres", "-d", "cove", "-Atc", "UPDATE workloads w SET manually_stopped=true WHERE NOT EXISTS(SELECT 1 FROM operations o WHERE o.workload_id=w.id AND o.state IN ('queued','running')) RETURNING w.id"], { encoding: "utf8" }).split(/\s+/).filter((value) => /^[a-f0-9]{32}$/.test(value));
  for (const resource of resources) {
    const ids = execFileSync("docker", ["ps", "-aq", "--filter", `label=com.docker.compose.project=cove-${resource}`], { encoding: "utf8" }).split(/\s+/).filter(Boolean);
    if (ids.length) execFileSync("docker", ["rm", "-f", "-v", ...ids]);
    const networks = execFileSync("docker", ["network", "ls", "-q", "--filter", `label=com.docker.compose.project=cove-${resource}`], { encoding: "utf8" }).split(/\s+/).filter(Boolean);
    if (networks.length) execFileSync("docker", ["network", "rm", ...networks]);
  }
});

// The same OpenSSL CLI scripts/test-browser.py uses for the panel's test TLS. Node cannot create X.509 certificates.
const openssl = (() => {
  try {
    execFileSync("openssl", ["version"], { stdio: "ignore" });
    return "openssl";
  } catch {
    const git = "C:/Program Files/Git/usr/bin/openssl.exe";
    if (process.platform === "win32" && existsSync(git)) return git;
    throw new Error("The OpenSSL CLI is required to create disposable route certificates");
  }
})();

interface Issued {
  chain: string;
  key: string;
}

// A disposable private CA and ECDSA P-256 server certificates it signs. Clients verify the routed certificate against
// this CA, so certificate verification is never skipped. Everything lives in a temporary directory removed afterwards.
function certificateAuthority() {
  const dir = mkdtempSync(join(tmpdir(), "cove-route-certificates-"));
  const path = (name: string) => join(dir, name);
  writeFileSync(path("openssl.cnf"), "[req]\ndistinguished_name=dn\n[dn]\n");
  const run = (...args: string[]) => execFileSync(openssl, args, { stdio: ["ignore", "ignore", "pipe"] });
  run("req", "-x509", "-newkey", "ec", "-pkeyopt", "ec_paramgen_curve:prime256v1", "-nodes", "-days", "2", "-config", path("openssl.cnf"), "-subj", "/CN=Cove browser test CA", "-addext", "basicConstraints=critical,CA:TRUE", "-addext", "keyUsage=critical,keyCertSign,cRLSign", "-keyout", path("ca.key"), "-out", path("ca.pem"));
  let serial = 1;
  const issue = (label: string, commonName: string, names: string[]): Issued => {
    run("req", "-new", "-newkey", "ec", "-pkeyopt", "ec_paramgen_curve:prime256v1", "-nodes", "-config", path("openssl.cnf"), "-subj", `/CN=${commonName}`, "-keyout", path(`${label}.key`), "-out", path(`${label}.csr`));
    writeFileSync(path(`${label}.ext`), `basicConstraints=critical,CA:FALSE\nkeyUsage=critical,digitalSignature\nextendedKeyUsage=serverAuth\nsubjectAltName=${names.map((name) => `DNS:${name}`).join(",")}\n`);
    run("x509", "-req", "-in", path(`${label}.csr`), "-CA", path("ca.pem"), "-CAkey", path("ca.key"), "-set_serial", String(serial++), "-days", "2", "-extfile", path(`${label}.ext`), "-out", path(`${label}.pem`));
    return { chain: readFileSync(path(`${label}.pem`), "utf8"), key: readFileSync(path(`${label}.key`), "utf8") };
  };
  return { ca: readFileSync(path("ca.pem"), "utf8"), issue, dispose: () => rmSync(dir, { recursive: true, force: true }) };
}

async function expectNoHorizontalScroll(page: Page) {
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBe(true);
}

async function capture(page: Page, testInfo: TestInfo, name: string) {
  for (const width of [320, 390, 768, 1440]) {
    await page.setViewportSize({ width, height: 900 });
    await expectNoHorizontalScroll(page);
    await page.screenshot({ path: testInfo.outputPath(`${name}-${width}.png`), fullPage: true });
  }
  await page.setViewportSize({ width: 1280, height: 900 });
}

// A TCP stream to the real Traefik ingress, which publishes no host ports: `nc` inside the ingress container.
function ingressTunnel() {
  const tunnel = spawn("docker", ["exec", "-i", "cove-ingress", "nc", "127.0.0.1", "443"], { stdio: ["pipe", "pipe", "inherit"] });
  const pipe = new Duplex({
    read() {
      tunnel.stdout.resume();
    },
    write(chunk: Buffer, _encoding, callback) {
      tunnel.stdin.write(chunk, callback);
    },
    final(callback) {
      tunnel.stdin.end(callback);
    },
  });
  tunnel.stdout.on("data", (chunk: Buffer) => {
    if (!pipe.push(chunk)) tunnel.stdout.pause();
  });
  tunnel.stdout.on("end", () => pipe.push(null));
  return { pipe, close: () => tunnel.kill() };
}

interface Verified {
  fingerprint: string;
  status: number;
  body: string;
}

// An HTTPS request with SNI for hostname, verified against the test CA. Rejects when the ingress serves any
// certificate the CA did not issue for that hostname.
function verified(hostname: string, ca: string): Promise<Verified> {
  const { pipe, close } = ingressTunnel();
  const socket = connect({ socket: pipe, servername: hostname, ca, rejectUnauthorized: true });
  return new Promise<Verified>((resolve, reject) => {
    const request = httpsRequest({ createConnection: () => socket, host: hostname, path: "/", method: "GET", headers: { Host: hostname, Connection: "close" }, timeout: 30_000 }, (response) => {
      const peer = (response.socket as TLSSocket).getPeerCertificate();
      const fingerprint = createHash("sha256").update(peer.raw).digest("hex");
      let body = "";
      response.setEncoding("utf8");
      response.on("data", (chunk: string) => (body += chunk));
      response.on("end", () => resolve({ fingerprint, status: response.statusCode ?? 0, body }));
      response.on("error", reject);
    });
    request.on("timeout", () => request.destroy(new Error(`Timed out requesting ${hostname}`)));
    request.on("error", reject);
    request.end();
  }).finally(close);
}

const servedFingerprint = (hostname: string, ca: string) => verified(hostname, ca).then((result) => result.fingerprint, (error: Error) => `rejected: ${error.message}`);

const web = "mkdir -p /www; echo certificate-marker > /www/index.html; exec httpd -f -p 8080 -h /www";
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

interface SavedRoute {
  domain: string;
  service: string;
  port: number;
  policy?: Record<string, unknown>;
}

interface CertificateMetadata {
  id: string;
  name: string;
  fingerprint: string;
  dns_names: string[];
}

test("uploaded route certificates: upload, select, rotate, switch away and delete in the dashboard, served by the real ingress", async ({ page }, testInfo) => {
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
    expect(node.capabilities?.route_protocol ?? 0).toBeGreaterThanOrEqual(3);
    const project = (await (await page.request.post("/api/projects", { headers, data: { name: "Route certificates project" } })).json()) as { id: string };
    const created = await page.request.post("/api/workloads", { headers, data: { name: "route-certificates", node_id: node.id, kind: "service", compose, project_id: project.id } });
    expect(created.status(), await created.text()).toBe(201);
    const { id } = (await created.json()) as { id: string };
    const hostname = `certificates-${Date.now()}.cove.test`;
    const first = authority.issue("first", hostname, [hostname]);
    const rotated = authority.issue("rotated", hostname, [hostname]);
    const wide = authority.issue("wide", hostname, [hostname, `unowned-${Date.now()}.cove.test`]);

    const operation = async (operationId: string) => (await (await page.request.get(`/api/operations/${operationId}`)).json()) as Operation;
    const settled = async (operationId: string) => {
      await expect.poll(async () => (await operation(operationId)).state, { timeout: 180_000 }).toMatch(/^(succeeded|failed|cancelled|interrupted)$/);
      const result = await operation(operationId);
      if (result.state !== "succeeded") throw new Error(`${result.action} ${operationId} ended ${result.state}: ${result.result?.error ?? ""}\n${await (await page.request.get(`/api/operations/${operationId}/logs`)).text()}`);
    };
    const activeOperations = async () => ((await (await page.request.get(`/api/operations?workload_id=${id}`)).json()) as Operation[]).filter((item) => item.state === "queued" || item.state === "running");
    const quiet = () => expect.poll(async () => (await activeOperations()).map((item) => item.action), { timeout: 120_000 }).toEqual([]);
    const send = async (request: () => Promise<APIResponse>, expected: number[]) => {
      for (let attempt = 1; ; attempt++) {
        await quiet();
        const response = await request();
        if (attempt < 3 && response.status() === 409 && (await response.text()).includes("active operation")) continue;
        expect(expected, await response.text()).toContain(response.status());
        return response;
      }
    };
    const savedRoutes = async () => ((await (await page.request.get(`/api/workloads/${id}`)).json()) as { routes?: SavedRoute[] }).routes ?? [];
    const listed = async () => {
      const response = await page.request.get(`/api/workloads/${id}/certificates`);
      const text = await response.text();
      expect(text).not.toContain("PRIVATE KEY");
      expect(text).not.toContain("BEGIN CERTIFICATE");
      return JSON.parse(text) as CertificateMetadata[];
    };

    const deploy = (await (await send(() => page.request.post(`/api/workloads/${id}/operations`, { headers, data: { action: "deploy" } }), [202])).json()) as { id: string };
    await settled(deploy.id);
    // The isolated ingress cannot reach a certificate authority, so the route starts on the proxy's default certificate.
    const base = await send(() => page.request.put(`/api/workloads/${id}/routes`, { headers, data: { routes: [{ domain: hostname, service: "web", port: 8080, policy: { certificate_source: "default" } }] } }), [200, 202]);
    const baseUpdate = (await base.json()) as { id?: string };
    if (baseUpdate.id) await settled(baseUpdate.id);
    await expect.poll(() => servedFingerprint(hostname, authority.ca), { timeout: 60_000 }).toMatch(/^rejected: /);

    await page.goto(`/resources/${id}/domains`);
    const certificatesList = page.getByRole("list", { name: "Uploaded certificates" });
    const uploadForm = page.getByRole("form", { name: "Upload a certificate" });
    await expect(page.getByText("No certificates are uploaded to this resource.")).toBeVisible({ timeout: 30_000 });

    // Uploads through the dashboard; the response carries metadata only and the form forgets the key material.
    const upload = async (name: string, issued: Issued) => {
      await uploadForm.getByLabel("Name", { exact: true }).fill(name);
      await uploadForm.getByLabel("Certificate chain", { exact: true }).fill(issued.chain);
      await uploadForm.getByLabel("Private key", { exact: true }).fill(issued.key);
      const pending = page.waitForResponse((response) => new URL(response.url()).pathname === `/api/workloads/${id}/certificates` && response.request().method() === "POST");
      await uploadForm.getByRole("button", { name: "Upload certificate" }).click();
      const response = await pending;
      const text = await response.text();
      expect(response.status(), text).toBe(201);
      expect(text).not.toContain("PRIVATE KEY");
      await expect(uploadForm.getByText("Certificate uploaded")).toBeVisible();
      await expect(uploadForm.getByLabel("Certificate chain", { exact: true })).toHaveValue("");
      await expect(uploadForm.getByLabel("Private key", { exact: true })).toHaveValue("");
      await expect(certificatesList.getByText(name, { exact: true })).toBeVisible();
      return JSON.parse(text) as CertificateMetadata;
    };
    const firstMetadata = await upload("Browser CA first", first);
    expect(firstMetadata.dns_names).toEqual([hostname]);
    const wideMetadata = await upload("Browser CA wide", wide);
    expect(await page.evaluate(() => JSON.stringify({ ...localStorage }) + JSON.stringify({ ...sessionStorage }) + location.href)).not.toContain("PRIVATE KEY");
    await capture(page, testInfo, "route-certificates-list");

    // A rejected upload keeps what was typed and shows the API's reason.
    await uploadForm.getByLabel("Name", { exact: true }).fill("Mismatched key");
    await uploadForm.getByLabel("Certificate chain", { exact: true }).fill(first.chain);
    await uploadForm.getByLabel("Private key", { exact: true }).fill(rotated.key);
    await uploadForm.getByRole("button", { name: "Upload certificate" }).click();
    await expect(uploadForm.getByText("Could not upload the certificate")).toBeVisible();
    await expect(uploadForm.getByLabel("Private key", { exact: true })).toHaveValue(rotated.key);
    await uploadForm.getByRole("button", { name: "Clear" }).click();
    await expect(uploadForm.getByLabel("Private key", { exact: true })).toHaveValue("");

    const saveRoutes = async () => {
      const save = page.getByRole("button", { name: "Save and apply", exact: true });
      for (let attempt = 1; ; attempt++) {
        await quiet();
        await expect(save).toBeEnabled({ timeout: 30_000 });
        const pending = page.waitForResponse((response) => new URL(response.url()).pathname === `/api/workloads/${id}/routes` && response.request().method() === "PUT");
        await save.click();
        const response = await pending;
        if (attempt < 3 && response.status() === 409 && (await response.text()).includes("active operation")) continue;
        expect(response.status(), await response.text()).toBe(202);
        await settled(((await response.json()) as { id: string }).id);
        await expect(page.getByText("Routes applied", { exact: true })).toBeVisible({ timeout: 30_000 });
        return response.request().postDataJSON() as { routes: SavedRoute[] };
      }
    };
    const openAdvanced = async () => {
      const toggle = page.getByRole("button", { name: /^Advanced routing/ });
      await expect(toggle).toBeVisible({ timeout: 30_000 });
      if ((await toggle.getAttribute("aria-expanded")) !== "true") await toggle.click();
    };
    const choose = async (name: string, option: string) => {
      await page.getByRole("combobox", { name, exact: true }).click();
      await page.getByRole("option", { name: option, exact: true }).click();
    };

    // The certificate naming a hostname this resource does not route is refused before anything is sent.
    await openAdvanced();
    await choose("Certificate", "Uploaded certificate");
    await choose("Uploaded certificate", "Browser CA wide");
    let routePuts = 0;
    const countPuts = (request: { url: () => string; method: () => string }) => {
      if (new URL(request.url()).pathname === `/api/workloads/${id}/routes` && request.method() === "PUT") routePuts++;
    };
    page.on("request", countPuts);
    await page.getByRole("button", { name: "Save and apply", exact: true }).click();
    await expect(page.getByText(/also names unowned-/)).toBeVisible();
    expect(routePuts).toBe(0);
    page.off("request", countPuts);

    // Selecting the first certificate from the keyboard. The list is newest first, so it is the last option.
    const picker = page.getByRole("combobox", { name: "Uploaded certificate", exact: true });
    await picker.focus();
    await page.keyboard.press("Enter");
    await expect(page.getByRole("listbox")).toBeVisible();
    await page.keyboard.press("End");
    await page.keyboard.press("Enter");
    await expect(picker).toHaveText(/Browser CA first/);
    await expect(page.getByText(/also names unowned-/)).toHaveCount(0);
    await expect(picker).not.toHaveAttribute("aria-invalid");
    await expect(page.getByText("Saving may briefly restart the shared proxy", { exact: true })).toBeVisible();
    await capture(page, testInfo, "route-certificates-picker");
    const selected = await saveRoutes();
    expect(selected.routes).toEqual([{ domain: hostname, service: "web", port: 8080, policy: { certificate_source: "uploaded", certificate_id: firstMetadata.id } }]);
    expect(await savedRoutes()).toEqual(selected.routes);
    await expect.poll(() => servedFingerprint(hostname, authority.ca), { timeout: 60_000 }).toBe(firstMetadata.fingerprint);
    expect((await verified(hostname, authority.ca)).body).toBe("certificate-marker\n");

    // Saved routes block deleting the certificate they use, in the dashboard and at the API.
    await expect(certificatesList.getByRole("button", { name: "Delete certificate Browser CA first" })).toBeDisabled();
    await send(() => page.request.delete(`/api/workloads/${id}/certificates/${firstMetadata.id}`, { headers }), [409]);
    const remove = async (name: string, certificateId: string) => {
      await certificatesList.getByRole("button", { name: `Delete certificate ${name}` }).click();
      const dialog = page.getByRole("dialog", { name: `Delete certificate ${name}?` });
      await expect(dialog).toContainText("does not revoke the certificate");
      const pending = page.waitForResponse((response) => new URL(response.url()).pathname === `/api/workloads/${id}/certificates/${certificateId}` && response.request().method() === "DELETE");
      await dialog.getByRole("button", { name: "Delete certificate" }).click();
      expect((await pending).status()).toBe(200);
      await expect(certificatesList.getByText(name, { exact: true })).toHaveCount(0);
    };
    await quiet();
    await remove("Browser CA wide", wideMetadata.id);

    // Rotation to a new key needs no change to the route other than the selection.
    const rotatedMetadata = await upload("Browser CA rotated", rotated);
    await choose("Uploaded certificate", "Browser CA rotated");
    await expect(page.getByText("Saving may briefly restart the shared proxy", { exact: true })).toHaveCount(0);
    const rotation = await saveRoutes();
    expect(rotation.routes[0]?.policy).toEqual({ certificate_source: "uploaded", certificate_id: rotatedMetadata.id });
    await expect.poll(() => servedFingerprint(hostname, authority.ca), { timeout: 60_000 }).toBe(rotatedMetadata.fingerprint);
    await quiet();
    await remove("Browser CA first", firstMetadata.id);

    // Switching away names the new source explicitly, so the saved selection is not kept.
    await page.reload();
    await openAdvanced();
    await expect(page.getByRole("combobox", { name: "Uploaded certificate", exact: true })).toHaveText(/Browser CA rotated/, { timeout: 30_000 });
    await choose("Certificate", "Proxy default certificate");
    const switched = await saveRoutes();
    expect(switched.routes).toEqual([{ domain: hostname, service: "web", port: 8080, policy: { certificate_source: "default" } }]);
    expect(await savedRoutes()).toEqual(switched.routes);
    await expect.poll(() => servedFingerprint(hostname, authority.ca), { timeout: 60_000 }).toMatch(/^rejected: /);
    await quiet();
    await remove("Browser CA rotated", rotatedMetadata.id);
    expect(await listed()).toEqual([]);
    await capture(page, testInfo, "route-certificates-empty");
  } finally {
    authority.dispose();
  }
});

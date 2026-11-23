// Test-only TLS helpers shared by the packaged route certificate browser specs. Certificates come from a disposable
// private CA and are always verified against it; certificate verification is never skipped.
import { execFileSync, spawn } from "node:child_process";
import { createHash } from "node:crypto";
import { existsSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { request as httpsRequest } from "node:https";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { Duplex } from "node:stream";
import { connect, type TLSSocket } from "node:tls";
import { expect, type Page, type TestInfo } from "@playwright/test";

// Stops and removes the Compose resources the browser tests created, in the isolated browser fixture only.
export function cleanupBrowserResources() {
  const database = process.env.COVE_BROWSER_DATABASE;
  if (!database || !/^cove-browser-[a-f0-9]+-db$/.test(database)) throw new Error("Isolated browser database required for cleanup");
  const resources = execFileSync("docker", ["exec", database, "psql", "-U", "postgres", "-d", "cove", "-Atc", "UPDATE workloads w SET manually_stopped=true WHERE NOT EXISTS(SELECT 1 FROM operations o WHERE o.workload_id=w.id AND o.state IN ('queued','running')) RETURNING w.id"], { encoding: "utf8" }).split(/\s+/).filter((value) => /^[a-f0-9]{32}$/.test(value));
  for (const resource of resources) {
    const ids = execFileSync("docker", ["ps", "-aq", "--filter", `label=com.docker.compose.project=cove-${resource}`], { encoding: "utf8" }).split(/\s+/).filter(Boolean);
    if (ids.length) execFileSync("docker", ["rm", "-f", "-v", ...ids]);
    const networks = execFileSync("docker", ["network", "ls", "-q", "--filter", `label=com.docker.compose.project=cove-${resource}`], { encoding: "utf8" }).split(/\s+/).filter(Boolean);
    if (networks.length) execFileSync("docker", ["network", "rm", ...networks]);
  }
}

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

export interface Issued {
  chain: string;
  key: string;
}

// A disposable private CA and ECDSA P-256 server certificates it signs. Clients verify the routed certificate against
// this CA, so certificate verification is never skipped. Everything lives in a temporary directory removed afterwards.
export function certificateAuthority() {
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

export async function expectNoHorizontalScroll(page: Page) {
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBe(true);
}

export async function capture(page: Page, testInfo: TestInfo, name: string) {
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

export interface Verified {
  fingerprint: string;
  status: number;
  body: string;
}

// An HTTPS request with SNI for hostname, verified against the test CA. Rejects when the ingress serves any
// certificate the CA did not issue for that hostname.
export function verified(hostname: string, ca: string, path = "/"): Promise<Verified> {
  const { pipe, close } = ingressTunnel();
  const socket = connect({ socket: pipe, servername: hostname, ca, rejectUnauthorized: true });
  return new Promise<Verified>((resolve, reject) => {
    const request = httpsRequest({ createConnection: () => socket, host: hostname, path, method: "GET", headers: { Host: hostname, Connection: "close" }, timeout: 30_000 }, (response) => {
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

export const servedFingerprint = (hostname: string, ca: string) => verified(hostname, ca).then((result) => result.fingerprint, (error: Error) => `rejected: ${error.message}`);

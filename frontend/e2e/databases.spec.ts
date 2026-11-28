import { mkdtempSync, readFileSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { createServer } from "node:net";
import { execFileSync, spawn, spawnSync } from "node:child_process";
import { randomBytes } from "node:crypto";
import { expect, test, type Locator, type Page, type TestInfo } from "@playwright/test";

// Real-engine acceptance for managed databases and the Garage add-on. Every resource is created through the
// normal API or interface, so scripts/test-browser.py can find and remove what these tests start.
// Generated credentials are only passed to native clients through environment variables, never printed.

const password = process.env.COVE_BROWSER_PASSWORD ?? "";
const worker = process.env.COVE_BROWSER_WORKER ?? "";
const fixtureDatabase = process.env.COVE_BROWSER_DATABASE ?? "";

test.afterEach(() => {
  if (!/^cove-browser-[a-f0-9]+-db$/.test(fixtureDatabase)) throw new Error("Isolated browser database required for cleanup");
  const resources = execFileSync("docker", ["exec", fixtureDatabase, "psql", "-U", "postgres", "-d", "cove", "-Atc", "UPDATE workloads w SET manually_stopped=true WHERE NOT EXISTS(SELECT 1 FROM operations o WHERE o.workload_id=w.id AND o.state IN ('queued','running')) RETURNING w.id"], { encoding: "utf8" }).split(/\s+/).filter((value) => /^[a-f0-9]{32}$/.test(value));
  for (const resource of resources) {
    for (const filter of [`label=com.docker.compose.project=cove-${resource}`, `label=cove.database-tool.workload=${resource}`]) {
      const ids = execFileSync("docker", ["ps", "-aq", "--filter", filter], { encoding: "utf8" }).split(/\s+/).filter(Boolean);
      if (ids.length) execFileSync("docker", ["rm", "-f", "-v", ...ids]);
    }
    // The harness removes any network still in use, such as one a private link gateway joined.
    for (const network of networks(resource)) {
      detachWorker(network);
      spawnSync("docker", ["network", "rm", network], { stdio: "ignore" });
    }
  }
});

interface Credentials { host: string; port: number; user: string; password: string; database: string; url: string }
interface Task { id: string; kind: string; resource_id: string; source_id: string; state: string; operation_state: string; error: string; cutover_confirmed?: boolean }
interface OperationState { id: string; action: string; state: string; result?: { error?: string } | null }

async function chooseOption(control: Locator, label: string | RegExp) {
  await control.click();
  const list = control.page().getByRole("listbox");
  await expect(list).toBeVisible();
  await list.getByRole("option", { name: label, exact: typeof label === "string" }).click();
}

async function signIn(page: Page) {
  await page.goto("/projects");
  await expect(page).toHaveURL(/\/login(?:[?#]|$)/);
  await page.getByLabel("Password", { exact: true }).fill(password);
  await page.getByRole("button", { name: "Sign in", exact: true }).click();
  await expect(page).not.toHaveURL(/\/login(?:[?#]|$)/);
}

async function capture(page: Page, testInfo: TestInfo, name: string) {
  for (const width of [390, 768, 1440]) {
    await page.setViewportSize({ width, height: 900 });
    await page.evaluate(() => window.scrollTo(0, 0));
    expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth), `${name} at ${width}`).toBe(true);
    await page.screenshot({ path: testInfo.outputPath(`${name}-${width}.png`), fullPage: true });
  }
  await page.setViewportSize({ width: 1440, height: 900 });
}

async function createProject(page: Page, name: string) {
  await page.goto("/projects");
  await page.locator("main header").getByRole("button", { name: "New project", exact: true }).click();
  await page.getByLabel("Project name", { exact: true }).fill(name);
  await page.getByRole("button", { name: "Create project", exact: true }).click();
  await expect(page.getByRole("heading", { name, exact: true })).toBeVisible();
  await page.getByRole("navigation", { name: "Project sections" }).getByRole("link", { name: /production/ }).click();
  return page.url();
}

function idFrom(url: string) {
  const id = new URL(url).pathname.split("/")[2] ?? "";
  if (!/^[0-9a-f]{32}$/.test(id)) throw new Error("Unexpected resource URL");
  return id;
}

async function api<T>(page: Page, method: "GET" | "POST" | "PUT" | "DELETE", path: string, data?: unknown): Promise<T> {
  const origin = new URL(process.env.COVE_BROWSER_URL ?? "").origin;
  const response = await page.request.fetch(path, { method, data, headers: method === "GET" ? {} : { Origin: origin } });
  if (!response.ok()) throw new Error(`${method} ${path} returned ${response.status()}: ${(await response.text()).slice(0, 200)}`);
  return (await response.json()) as T;
}

async function createDatabase(page: Page, environmentURL: string, name: string, engine: RegExp, version?: string) {
  await page.goto(environmentURL);
  await page.getByRole("link", { name: "New resource", exact: true }).first().click();
  await page.getByRole("button", { name: "Managed database", exact: true }).click();
  await page.getByLabel("Name", { exact: true }).fill(name);
  await chooseOption(page.getByRole("combobox", { name: "Node" }), "ready-browser-worker");
  await page.getByRole("radio", { name: engine }).check();
  if (version) await page.getByRole("group", { name: "Version" }).getByRole("radio", { name: new RegExp(`^${version.replaceAll(".", "\\.")}`) }).check();
  await page.getByRole("button", { name: "Create database", exact: true }).click();
  await expect(page.getByRole("heading", { name, exact: true })).toBeVisible();
  return idFrom(page.url());
}

async function createService(page: Page, project: string, environment: string, name: string, compose: string, environmentValues: Record<string, string> = {}) {
  const node = (await api<{ id: string; name: string }[]>(page, "GET", "/api/nodes")).find((item) => item.name === "ready-browser-worker");
  if (!node) throw new Error("Fixture worker missing");
  const created = await api<{ id: string }>(page, "POST", "/api/workloads", { name, node_id: node.id, project_id: project, environment_id: environment, kind: "service", compose, source: null, environment: environmentValues });
  return created.id;
}

async function waitOperation(page: Page, id: string, timeout = 300_000) {
  let last: OperationState | undefined;
  await expect
    .poll(async () => {
      last = await api<OperationState>(page, "GET", `/api/operations/${id}`);
      return last.state;
    }, { timeout, intervals: [2000] })
    .toMatch(/^(succeeded|failed|interrupted|cancelled)$/);
  expect(last?.state, last?.result?.error ?? "operation did not succeed").toBe("succeeded");
}

// Bucket provisioning refuses while either resource has queued or running work. Besides explicit deploys, the
// scheduler queues an image digest check on a service soon after its first successful deploy when the worker
// supports image lifecycle checks, so that check must have been scheduled and finished as well.
async function waitIdle(page: Page, workloads: string[], imageChecked: string[] = [], timeout = 180_000) {
  const node = (await api<{ name: string; capabilities: { image_lifecycle?: boolean } | null }[]>(page, "GET", "/api/nodes")).find((item) => item.name === "ready-browser-worker");
  const expectImageCheck = node?.capabilities?.image_lifecycle === true;
  await expect
    .poll(
      async () => {
        const blocking: string[] = [];
        for (const workload of workloads) {
          const operations = await api<OperationState[]>(page, "GET", `/api/operations?workload_id=${workload}`);
          for (const operation of operations) if (operation.state === "queued" || operation.state === "running") blocking.push(`${workload} ${operation.action} ${operation.state} ${operation.id}`);
          if (expectImageCheck && imageChecked.includes(workload) && !operations.some((operation) => operation.action === "image_check")) blocking.push(`${workload} image_check not scheduled`);
        }
        return blocking;
      },
      { timeout, intervals: [2000], message: "operations still blocking bucket provisioning" },
    )
    .toEqual([]);
}

async function deploy(page: Page, id: string) {
  const queued = await api<{ id: string }>(page, "POST", `/api/workloads/${id}/operations`, { action: "deploy" });
  await waitOperation(page, queued.id);
}

// Manual deploys check capacity first: passing checks open the deployment review at once, otherwise the capacity
// dialog needs an explicit choice before the review.
async function deployFromHeader(page: Page, id: string) {
  await page.goto(`/resources/${id}`);
  await page.locator("main header").getByRole("button", { name: "Deploy", exact: true }).click();
  const proceed = page.getByRole("dialog", { name: "Capacity before deploy" }).getByRole("button", { name: /^Deploy (anyway|without check)$/ });
  const dialog = page.getByRole("dialog", { name: /^Deploy .+\?$/ });
  await expect(proceed.or(dialog)).toBeVisible({ timeout: 60_000 });
  if (await proceed.isVisible()) await proceed.click();
  const queued = page.waitForResponse((response) => response.url().endsWith(`/api/workloads/${id}/operations`) && response.request().method() === "POST");
  await dialog.getByRole("button", { name: "Deploy", exact: true }).click();
  const operation = (await (await queued).json()) as { id: string };
  await waitOperation(page, operation.id);
}

async function waitTask(page: Page, workload: string, kind: string) {
  let task: Task | undefined;
  await expect
    .poll(async () => {
      task = (await api<Task[]>(page, "GET", `/api/workloads/${workload}/database/maintenance`)).find((item) => item.kind === kind);
      return task ? `${task.state}:${task.operation_state}` : "missing";
    }, { timeout: 300_000, intervals: [2000] })
    .toMatch(/^(succeeded|failed):/);
  expect(task?.state, task?.error ?? "maintenance did not succeed").toBe("succeeded");
  return task as Task;
}

function networks(workload: string) {
  return execFileSync("docker", ["network", "ls", "-q", "--filter", `label=com.docker.compose.project=cove-${workload}`], { encoding: "utf8" }).split(/\s+/).filter(Boolean);
}

function network(workload: string) {
  const found = networks(workload);
  expect(found.length).toBeGreaterThan(0);
  return found[0] ?? "";
}

// The fixture worker is containerized; attaching it emulates the bridge reachability a real host has.
function attachWorker(id: string) {
  execFileSync("docker", ["network", "connect", id, worker]);
}

function detachWorker(id: string) {
  spawnSync("docker", ["network", "disconnect", id, worker], { stdio: "ignore" });
}

// Runs a native client in a throwaway container on the database's private network, so authentication is
// exercised over TCP rather than through the image's local trust rules. SECRET reaches only that container.
function native(workload: string, image: string, script: string, secret: string) {
  const result = spawnSync("docker", ["run", "--rm", "--network", network(workload), "--entrypoint", "sh", "-e", "SECRET", image, "-c", script], { encoding: "utf8", env: { ...process.env, SECRET: secret }, timeout: 120_000 });
  return { ok: result.status === 0, output: `${result.stdout}` };
}

async function expectNative(workload: string, image: string, script: string, secret: string, expected: string) {
  await expect.poll(() => { const result = native(workload, image, script, secret); return result.ok && result.output.includes(expected); }, { timeout: 120_000, intervals: [3000] }).toBe(true);
}

async function credentials(page: Page, workload: string) {
  return api<Credentials>(page, "GET", `/api/workloads/${workload}/database/credentials`);
}

async function freePort() {
  const reservation = createServer();
  await new Promise<void>((resolve) => reservation.listen(0, "127.0.0.1", resolve));
  const address = reservation.address();
  if (!address || typeof address === "string") throw new Error("No local fixture port");
  await new Promise<void>((resolve, reject) => reservation.close((error) => (error ? reject(error) : resolve())));
  return address.port;
}

const engines = [
  { name: "PostgreSQL", radio: /^PostgreSQL/, image: "postgres:18.7-alpine", script: 'PGPASSWORD="$SECRET" psql -h postgres -U app -d app -tAc "create table if not exists probe(v int); insert into probe values (7); select sum(v) from probe"' },
  { name: "MySQL", radio: /^MySQL/, image: "mysql:8.4.13", script: 'MYSQL_PWD="$SECRET" mysql -h mysql -u app app -N -e "create table if not exists probe(v int); insert into probe values (7); select sum(v) from probe"' },
  { name: "MariaDB", radio: /^MariaDB/, image: "mariadb:11.8.9", script: 'MYSQL_PWD="$SECRET" mariadb -h mariadb -u app app -N -e "create table if not exists probe(v int); insert into probe values (7); select sum(v) from probe"' },
  { name: "MongoDB", radio: /^MongoDB/, image: "mongo:8.0.38-noble", script: "mongosh --quiet \"mongodb://app:$SECRET@mongo:27017/app?authSource=admin\" --eval 'db.probe.insertOne({v:7}); print(\"sum=\" + db.probe.countDocuments() * 7)'" },
  { name: "Redis", radio: /^Redis/, image: "redis:8.10.3-alpine", script: 'redis-cli -h redis --user default -a "$SECRET" --no-auth-warning set probe 7 >/dev/null && redis-cli -h redis --user default -a "$SECRET" --no-auth-warning get probe' },
  { name: "Valkey", radio: /^Valkey/, image: "valkey/valkey:9.2.0-alpine", script: 'valkey-cli -h valkey --user default -a "$SECRET" --no-auth-warning set probe 7 >/dev/null && valkey-cli -h valkey --user default -a "$SECRET" --no-auth-warning get probe' },
  { name: "ClickHouse", radio: /^ClickHouse/, image: "clickhouse/clickhouse-server:26.9.36.3", script: 'clickhouse-client --host clickhouse --user app --password "$SECRET" --multiquery --query "create table if not exists app.probe (v UInt8) engine=Memory; insert into app.probe values (7); select sum(v) from app.probe"' },
];

for (const engine of engines) {
  test(`${engine.name} database is created, deployed and answers an authenticated native query`, async ({ page }, testInfo) => {
    test.setTimeout(420_000);
    await page.setViewportSize({ width: 1440, height: 900 });
    await signIn(page);
    const environmentURL = await createProject(page, `${engine.name} database project`);
    if (engine.name === "PostgreSQL") {
      await page.getByRole("link", { name: "New resource", exact: true }).first().click();
      await page.getByRole("button", { name: "Managed database", exact: true }).click();
      // Keyboard: the engine choices are native radios, so arrow keys move the selection.
      await page.getByRole("radio", { name: /^PostgreSQL/ }).focus();
      await page.keyboard.press("Space");
      await page.keyboard.press("ArrowRight");
      await expect(page.getByRole("radio", { name: /^MySQL/ })).toBeChecked();
      await capture(page, testInfo, "database-create");
    }
    const id = await createDatabase(page, environmentURL, `${engine.name.toLowerCase()}-db`, engine.radio);
    const workload = await api<{ kind: string; database: { engine: string; host: string } }>(page, "GET", `/api/workloads/${id}`);
    expect(workload.kind).toBe("database");
    expect(workload.database.host).toBe(`db-${id}.internal`);
    await deployFromHeader(page, id);
    const secret = (await credentials(page, id)).password;
    expect(secret).toMatch(/^[0-9a-f]{32,}$/);
    await expectNative(id, engine.image, engine.script, secret, "7");
    expect(native(id, engine.image, engine.script, "wrong-password").output).not.toContain("7");
    await page.goto(`/resources/${id}/database`);
    await expect(page.getByRole("button", { name: "Reveal credentials" })).toBeVisible();
    await expect(page.getByText(secret)).toHaveCount(0);
    if (engine.name === "PostgreSQL") {
      await capture(page, testInfo, "database-connection");
      await page.goto(`/resources/${id}/environment`);
      await expect(page.getByRole("list", { name: "Exported variables" })).toContainText("DB_URL");
      await capture(page, testInfo, "database-variables");
      await page.goto(`/resources/${id}/backups`);
      await expect(page.getByText("Restoring a managed database")).toBeVisible();
      await capture(page, testInfo, "database-backups");
      await page.goto("/resources");
      await expect(page.getByRole("row", { name: /postgresql-db/ })).toContainText("Database");
    }
  });
}

test("bindings and a private connection deliver DB_URL, and rotation refuses the old password", async ({ page }, testInfo) => {
  test.setTimeout(600_000);
  await page.setViewportSize({ width: 1440, height: 900 });
  await signIn(page);
  const environmentURL = await createProject(page, "Database binding project");
  const [, , projectId, , environmentId] = new URL(environmentURL).pathname.split("/");
  const db = await createDatabase(page, environmentURL, "orders-db", /^PostgreSQL/);
  await deployFromHeader(page, db);
  const consumerCompose = `services:
  app:
    image: postgres:18.7-alpine
    entrypoint: [sh, -c]
    command: ['until psql "\`printenv DATABASE_URL\`" -tAc "select 40+2" > /tmp/result 2>/dev/null; do sleep 1; done; echo "result=\`cat /tmp/result\`"; sleep 600']
    environment:
      DATABASE_URL: \${DATABASE_URL:?}
    healthcheck:
      test: [CMD, 'true']
      interval: 1s
`;
  const consumer = await createService(page, projectId ?? "", environmentId ?? "", "orders-api", consumerCompose, { DATABASE_URL: "{{resource.ORDERS_URL}}" });

  await page.goto(`/resources/${consumer}/environment`);
  await page.getByLabel("Binding name", { exact: true }).fill("ORDERS_URL");
  await chooseOption(page.getByRole("combobox", { name: "Source resource" }), /orders-db/);
  await chooseOption(page.getByRole("combobox", { name: "Source variable" }), "DB_URL (Connection URL, secret)");
  await expect(page.getByText("A binding grants no network access")).toBeVisible();
  await page.getByRole("button", { name: "Save binding", exact: true }).click();
  await expect(page.getByText("{{resource.ORDERS_URL}}")).toBeVisible();
  await capture(page, testInfo, "database-binding");
  await page.goto("/resources");
  await chooseOption(page.getByRole("combobox", { name: "Type" }), "Database");
  await expect(page.getByRole("link", { name: "orders-db" })).toBeVisible();
  await expect(page.getByRole("link", { name: "orders-api" })).toHaveCount(0);

  await page.goto(environmentURL);
  const section = page.locator("section", { has: page.getByRole("heading", { name: "Resources in production" }) });
  await section.getByRole("button", { name: "Graph", exact: true }).click();
  await section.getByRole("button", { name: "Connect from orders-api" }).click();
  await section.getByRole("button", { name: "Connect to orders-db" }).click();
  const dialog = page.getByRole("dialog", { name: "New private connection" });
  await expect(dialog.getByLabel("Private hostname", { exact: true })).toHaveValue(`db-${db}.internal`);
  await expect(dialog.getByLabel("Target port", { exact: true })).toHaveValue("5432");
  await expect(dialog.getByLabel("Target service", { exact: true })).toHaveValue("postgres");
  await chooseOption(dialog.getByRole("combobox", { name: "Source service" }), "app");
  await dialog.getByRole("button", { name: "Create connection", exact: true }).click();
  await expect(dialog).toBeHidden();
  await expect(section.getByRole("button", { name: new RegExp(`db-${db}.internal port 5432, Active`) })).toBeVisible({ timeout: 120_000 });

  const consumerLogs = () => {
    const ids = execFileSync("docker", ["ps", "-q", "--filter", `label=com.docker.compose.project=cove-${consumer}`], { encoding: "utf8" }).split(/\s+/).filter(Boolean);
    return ids.length ? execFileSync("docker", ["logs", ids[0] ?? ""], { encoding: "utf8" }) : "";
  };
  await deploy(page, consumer);
  await expect.poll(consumerLogs, { timeout: 120_000, intervals: [2000] }).toContain("result=42");

  const before = (await credentials(page, db)).password;
  await page.goto(`/resources/${db}/database`);
  await page.getByRole("button", { name: "Rotate password", exact: true }).click();
  const rotate = page.getByRole("dialog", { name: "Rotate the PostgreSQL password?" });
  await expect(rotate.getByRole("button", { name: "Rotate password" })).toBeDisabled();
  await rotate.getByRole("checkbox").check();
  await capture(page, testInfo, "database-rotate-dialog");
  await rotate.getByRole("button", { name: "Rotate password" }).click();
  await expect(rotate.getByText("Maintenance queued")).toBeVisible();
  await rotate.getByRole("button", { name: "Close" }).click();
  await waitTask(page, db, "rotate");
  const after = (await credentials(page, db)).password;
  expect(after).not.toBe(before);
  const probe = 'PGPASSWORD="$SECRET" psql -h postgres -U app -d app -tAc "select 7"';
  await expectNative(db, "postgres:18.7-alpine", probe, after, "7");
  expect(native(db, "postgres:18.7-alpine", probe, before).ok).toBe(false);
  await page.reload();
  await expect(page.getByRole("list", { name: "Maintenance tasks" })).toContainText("Succeeded");
  await capture(page, testInfo, "database-maintenance");

  await deploy(page, consumer);
  await expect.poll(consumerLogs, { timeout: 120_000, intervals: [2000] }).toContain("result=42");
});

test("PostgreSQL 17 enables an extension and copies its data to a separate PostgreSQL 18 resource", async ({ page }, testInfo) => {
  test.setTimeout(600_000);
  await page.setViewportSize({ width: 1440, height: 900 });
  await signIn(page);
  const environmentURL = await createProject(page, "Database upgrade project");
  const source = await createDatabase(page, environmentURL, "legacy-db", /^PostgreSQL/, "17.12-alpine");
  await deployFromHeader(page, source);
  const secret = (await credentials(page, source)).password;
  await expectNative(source, "postgres:17.12-alpine", 'PGPASSWORD="$SECRET" psql -h postgres -U app -d app -tAc "create table carried(v text); insert into carried values (\'kept-row\'); select v from carried"', secret, "kept-row");

  await page.goto(`/resources/${source}/database`);
  await page.getByRole("button", { name: "Enable pg_trgm", exact: true }).click();
  const extension = page.getByRole("dialog", { name: "Enable pg_trgm?" });
  await extension.getByRole("checkbox").check();
  await extension.getByRole("button", { name: "Enable extension" }).click();
  await expect(extension.getByText("Maintenance queued")).toBeVisible();
  await extension.getByRole("button", { name: "Close" }).click();
  await waitTask(page, source, "extension");
  await expectNative(source, "postgres:17.12-alpine", 'PGPASSWORD="$SECRET" psql -h postgres -U app -d app -tAc "select extname from pg_extension where extname=\'pg_trgm\'"', secret, "pg_trgm");

  await page.reload();
  const extensions = page.getByRole("list", { name: "Extensions" });
  await expect(extensions.getByRole("listitem").filter({ hasText: "pg_trgm" })).toContainText("Managed by Cove, not inspected");
  await page.getByRole("button", { name: "Inspect runtime", exact: true }).click();
  await expect(page.getByText("Installed extensions")).toBeVisible();
  await expect(extensions.getByRole("listitem").filter({ hasText: "pg_trgm" })).toContainText("Installed, managed by Cove");
  await page.getByRole("button", { name: "Create upgrade copy", exact: true }).click();
  const copy = page.getByRole("dialog", { name: "Create an upgrade copy?" });
  await copy.getByRole("radio", { name: /^PostgreSQL 18\.7-alpine/ }).check();
  await copy.getByLabel("Name of the new resource", { exact: true }).fill("legacy-db-18");
  await copy.getByRole("button", { name: "Check compatibility" }).click();
  await expect(copy.getByText(/^Compatible with/)).toBeVisible({ timeout: 60_000 });
  await copy.getByRole("checkbox", { name: /one-time snapshot copy/ }).check();
  await copy.getByRole("checkbox", { name: /external writers to legacy-db are stopped/ }).check();
  await capture(page, testInfo, "database-upgrade-dialog");
  await copy.getByRole("button", { name: "Create copy" }).click();
  await expect(copy.getByText("Copy queued")).toBeVisible();
  await copy.getByRole("button", { name: "Close" }).click();
  const task = await waitTask(page, source, "upgrade");
  expect(task.source_id).toBe(source);
  const target = task.resource_id;
  expect(target).not.toBe(source);
  const targetSecret = (await credentials(page, target)).password;
  expect(targetSecret).not.toBe(secret);
  await expectNative(target, "postgres:18.7-alpine", 'PGPASSWORD="$SECRET" psql -h postgres -U app -d app -tAc "select v || \':\' || current_setting(\'server_version_num\')::int / 10000 from carried; select extname from pg_extension where extname=\'pg_trgm\'"', targetSecret, "kept-row:18");
  expect(native(target, "postgres:18.7-alpine", 'PGPASSWORD="$SECRET" psql -h postgres -U app -d app -tAc "select extname from pg_extension"', targetSecret).output).toContain("pg_trgm");
  await expectNative(source, "postgres:17.12-alpine", 'PGPASSWORD="$SECRET" psql -h postgres -U app -d app -tAc "select v || \':\' || current_setting(\'server_version_num\')::int / 10000 from carried"', secret, "kept-row:17");
  await page.goto(`/resources/${target}/database`);
  await expect(page.getByText("PostgreSQL 18.7-alpine").first()).toBeVisible();

  // Cut-over switches bindings to the verified copy without redeploying consumers or removing the source.
  await page.goto(`/resources/${source}/database`);
  await page.getByRole("button", { name: "Cut over", exact: true }).click();
  const cutover = page.getByRole("dialog", { name: "Cut over to the copy?" });
  for (const box of await cutover.getByRole("checkbox").all()) await box.check();
  await capture(page, testInfo, "database-cutover-dialog");
  await cutover.getByRole("button", { name: "Switch to copy" }).click();
  await expect(cutover.getByText("Cut-over recorded")).toBeVisible();
  await cutover.getByRole("button", { name: "Close" }).click();
  expect((await api<Task[]>(page, "GET", `/api/workloads/${source}/database/maintenance`)).find((item) => item.kind === "upgrade")?.cutover_confirmed).toBe(true);
  await expectNative(source, "postgres:17.12-alpine", 'PGPASSWORD="$SECRET" psql -h postgres -U app -d app -tAc "select v from carried"', secret, "kept-row");
});

test("PostgreSQL pooler is enabled through its confirmation and exports pooled bindings", async ({ page }, testInfo) => {
  test.setTimeout(420_000);
  await page.setViewportSize({ width: 390, height: 844 });
  await signIn(page);
  const environmentURL = await createProject(page, "Database pooler project");
  const id = await createDatabase(page, environmentURL, "pooled-db", /^PostgreSQL/);
  await deployFromHeader(page, id);
  await page.goto(`/resources/${id}/database`);
  await page.getByRole("button", { name: "Enable pooler", exact: true }).click();
  const dialog = page.getByRole("dialog", { name: "Enable the connection pooler?" });
  await dialog.getByRole("checkbox").check();
  const queued = page.waitForResponse((response) => response.url().endsWith(`/api/workloads/${id}/database/pooler`) && response.request().method() === "POST");
  await dialog.getByRole("button", { name: "Enable pooler" }).click();
  await waitOperation(page, ((await (await queued).json()) as { id: string }).id);
  await dialog.getByRole("button", { name: "Close" }).click();
  await page.reload();
  await expect(page.getByRole("list", { name: "Exported variables" })).toContainText("DB_POOL_URL");
  await expect(page.getByText(`pool-${id}.internal`)).toBeVisible();
  await capture(page, testInfo, "database-pooler-390");
});

test("Adminer runs through the SSH tunnel and closes on revocation and on expiry", async ({ page }, testInfo) => {
  test.setTimeout(420_000);
  await page.setViewportSize({ width: 1440, height: 900 });
  await signIn(page);
  const environmentURL = await createProject(page, "Database Adminer project");
  const db = await createDatabase(page, environmentURL, "admin-db", /^MariaDB/);
  await deployFromHeader(page, db);
  const bridge = network(db);
  attachWorker(bridge);
  const directory = mkdtempSync(join(tmpdir(), "cove-adminer-"));
  const children: ReturnType<typeof spawn>[] = [];
  let diagnostics = "";
  const helpers = () => execFileSync("docker", ["ps", "-aq", "--filter", `label=cove.database-tool.workload=${db}`], { encoding: "utf8" }).trim();
  const connect = (identity: string, hostKey: string, expires: string, localPort: number) => {
    const child = spawn(process.env.COVE_BROWSER_CLIENT ?? "", ["connect", "--host", "127.0.0.1", "--ssh-port", process.env.COVE_BROWSER_SSH_PORT ?? "", "--local-port", String(localPort), "--identity", identity, "--host-key", hostKey.trim(), "--expires", expires]);
    child.stderr?.on("data", (data: Buffer) => { diagnostics += data.toString(); });
    children.push(child);
  };
  const networksOf = (container: string) => (container ? spawnSync("docker", ["inspect", "--format", "{{range $name, $net := .NetworkSettings.Networks}}{{$name}}={{$net.IPAddress}} {{end}}{{.State.Status}}", container], { encoding: "utf8" }).stdout.trim() : "none");
  const state = () => `${diagnostics}\nhelper: ${networksOf(helpers().split(/\s+/)[0] ?? "")}\nworker: ${networksOf(worker)}`;
  const adminer = async (localPort: number) => {
    try {
      return await (await fetch(`http://127.0.0.1:${localPort}/`, { signal: AbortSignal.timeout(3000) })).text();
    } catch {
      return "";
    }
  };
  try {
    const identity = join(directory, "key");
    execFileSync("ssh-keygen", ["-q", "-t", "ed25519", "-N", "", "-C", "cove-fixture", "-f", identity]);
    await page.goto(`/resources/${db}/networking`);
    await page.getByRole("radio", { name: "Adminer in the browser" }).check();
    await page.getByLabel("Ed25519 public key", { exact: true }).fill(readFileSync(`${identity}.pub`, "utf8"));
    const opened = page.waitForResponse((response) => response.url().endsWith("/database-access") && response.request().method() === "POST");
    await page.getByRole("button", { name: "Open Adminer access", exact: true }).click();
    const response = await opened;
    expect(response.status()).toBe(201);
    const lease = (await response.json()) as { host_key: string; expires_at: string };
    await expect(page.getByText("http://127.0.0.1:18080")).toBeVisible();
    await expect(page.getByRole("list", { name: "Active access leases" })).toContainText("Adminer");
    await capture(page, testInfo, "database-adminer");
    const localPort = await freePort();
    connect(identity, lease.host_key, lease.expires_at, localPort);
    await expect.poll(() => adminer(localPort), { timeout: 60_000, intervals: [2000] }).toContain("Adminer").catch((error: unknown) => { throw new Error(`${String(error)}\n${state()}`); });
    expect(helpers()).not.toBe("");
    expect(execFileSync("docker", ["inspect", "--format", "{{json .NetworkSettings.Ports}}", helpers().split(/\s+/)[0] ?? ""], { encoding: "utf8" })).not.toMatch(/HostPort/);
    await page.getByRole("button", { name: "Revoke access", exact: true }).click();
    await expect(page.getByRole("button", { name: "Revoke access", exact: true })).toHaveCount(0);
    await expect.poll(() => adminer(localPort), { timeout: 30_000 }).toBe("");
    await expect.poll(helpers, { timeout: 60_000 }).toBe("");

    // Expiry: the public API always grants 15 minutes, so ask the fixture worker directly for a short lease.
    const shortIdentity = join(directory, "short");
    execFileSync("ssh-keygen", ["-q", "-t", "ed25519", "-N", "", "-C", "cove-fixture", "-f", shortIdentity]);
    const expires = new Date(Date.now() + 45_000).toISOString();
    const request = { method: "tunnel_open", tunnel: { tool: "adminer", id: randomBytes(16).toString("hex"), workload: db, service: "mariadb", port: 8080, public_key: readFileSync(`${shortIdentity}.pub`, "utf8").trim(), expires_at: expires } };
    const reply = execFileSync("docker", ["exec", "-i", "-u", "cove", worker, "cove-worker", "rpc"], { input: JSON.stringify(request), encoding: "utf8" });
    expect(reply).not.toMatch(/"error"/);
    const hostKey = execFileSync("docker", ["exec", worker, "cat", "/etc/ssh/ssh_host_ed25519_key.pub"], { encoding: "utf8" });
    const shortPort = await freePort();
    connect(shortIdentity, hostKey, expires, shortPort);
    await expect.poll(() => adminer(shortPort), { timeout: 30_000, intervals: [2000] }).toContain("Adminer");
    await expect.poll(() => adminer(shortPort), { timeout: 90_000, intervals: [3000] }).toBe("");
    await expect.poll(helpers, { timeout: 90_000, intervals: [3000] }).toBe("");
  } finally {
    for (const child of children) {
      if (child.exitCode === null) {
        child.kill();
        await new Promise<void>((resolve) => child.once("exit", () => resolve()));
      }
    }
    rmSync(directory, { recursive: true, force: true });
    detachWorker(bridge);
  }
});

test("Garage add-on provisions a scoped bucket and backup destination, and revocation keeps the data", async ({ page }, testInfo) => {
  test.setTimeout(600_000);
  await page.setViewportSize({ width: 1440, height: 900 });
  await signIn(page);
  const environmentURL = await createProject(page, "Object storage project");
  const [, , projectId, , environmentId] = new URL(environmentURL).pathname.split("/");
  const domain = `s3-${randomBytes(4).toString("hex")}.cove-fixture.test`;

  await page.getByRole("link", { name: "New resource", exact: true }).first().click();
  await page.getByPlaceholder("Search templates").fill("Garage");
  await page.getByRole("button", { name: /Garage/ }).first().click();
  await page.getByRole("button", { name: /Review configuration/ }).click();
  await chooseOption(page.getByRole("combobox", { name: "Node" }), "ready-browser-worker");
  await page.getByRole("button", { name: "Create resource", exact: true }).click();
  await expect(page.getByRole("heading", { name: /garage/i })).toBeVisible();
  const garage = idFrom(page.url());
  await page.goto(`/resources/${garage}/domains`);
  await page.getByRole("button", { name: "Add route", exact: true }).click();
  await page.getByLabel("Hostname", { exact: true }).last().fill(domain);
  await page.getByLabel("Compose service", { exact: true }).last().fill("garage");
  await page.getByLabel("Container port", { exact: true }).last().fill("3900");
  await page.getByRole("button", { name: /Save routes|Save and apply/ }).click();
  await expect(page.getByText(/Routes saved|Routes applied/)).toBeVisible({ timeout: 60_000 });
  await deploy(page, garage);
  const garageBridge = network(garage);
  attachWorker(garageBridge);

  const consumerCompose = `services:
  app:
    image: cove-backup-remote:restic0.19.1-rclone1.76.0
    entrypoint: [sh, -c]
    command:
      - |
        echo "endpoint=\`printenv S3_ENDPOINT\` bucket=\`printenv S3_BUCKET\`"
        printenv S3_BUCKET > /tmp/bucket
        until rclone lsf "g:\`cat /tmp/bucket\`" >/dev/null 2>&1; do sleep 2; done
        echo scoped-object > /tmp/probe && rclone copyto /tmp/probe "g:\`cat /tmp/bucket\`/probe.txt" && rclone cat "g:\`cat /tmp/bucket\`/probe.txt" > /tmp/read && echo "read=\`cat /tmp/read\`"
        if rclone lsf g:data >/dev/null 2>&1; then echo other=allowed; else echo other=denied; fi
        if rclone mkdir g:cove-unscoped-probe >/dev/null 2>&1; then echo create=allowed; else echo create=denied; fi
        sleep 600
    environment:
      RCLONE_CONFIG_G_TYPE: s3
      RCLONE_CONFIG_G_PROVIDER: Other
      RCLONE_CONFIG_G_ENDPOINT: http://garage-s3.internal:3900
      RCLONE_CONFIG_G_FORCE_PATH_STYLE: "true"
      RCLONE_CONFIG_G_ACCESS_KEY_ID: \${S3_ACCESS_KEY_ID:?}
      RCLONE_CONFIG_G_SECRET_ACCESS_KEY: \${S3_SECRET_ACCESS_KEY:?}
      RCLONE_CONFIG_G_REGION: \${S3_REGION:?}
      S3_ENDPOINT: \${S3_ENDPOINT:?}
      S3_BUCKET: \${S3_BUCKET:?}
      S3_ACCESS_KEY_ID: \${S3_ACCESS_KEY_ID:?}
      S3_SECRET_ACCESS_KEY: \${S3_SECRET_ACCESS_KEY:?}
      S3_REGION: \${S3_REGION:?}
    healthcheck:
      test: [CMD, 'true']
      interval: 1s
`;
  const consumer = await createService(page, projectId ?? "", environmentId ?? "", "uploads-api", consumerCompose);
  await page.goto(`/resources/${consumer}/object-storage`);
  await chooseOption(page.getByRole("combobox", { name: "Garage resource" }), /garage/i);
  await expect(page.getByRole("combobox", { name: "S3 endpoint" })).toContainText(domain);
  await expect(page.getByRole("button", { name: "Provision bucket" })).toBeDisabled();
  await page.getByRole("checkbox", { name: "I understand this storage is not offsite." }).check();
  await page.getByRole("checkbox", { name: /Also add the bucket as a team backup destination/ }).check();
  await capture(page, testInfo, "object-storage-provision");
  await waitIdle(page, [garage, consumer], [garage]);
  const provisioned = page.waitForResponse((response) => response.url().endsWith(`/api/workloads/${consumer}/object-buckets`) && response.request().method() === "POST");
  await page.getByRole("button", { name: "Provision bucket" }).click();
  expect((await provisioned).status()).toBe(201);
  await expect(page.getByRole("region", { name: "Object storage", exact: true }).getByText("Ready", { exact: true })).toBeVisible({ timeout: 120_000 });
  await expect(page.getByRole("list", { name: "Bound S3 variables" })).toContainText("S3_SECRET_ACCESS_KEY");
  await capture(page, testInfo, "object-storage-ready");
  const [bucket] = await api<{ id: string; bucket: string; endpoint: string; storage_id: string }[]>(page, "GET", `/api/workloads/${consumer}/object-buckets`);
  expect(bucket?.endpoint).toBe(`https://${domain}`);
  expect(bucket?.storage_id).toBeTruthy();

  await api(page, "POST", "/api/private-connections", { source_id: consumer, source_service: "app", target_id: garage, target_service: "garage", port: 3900, alias: "garage-s3.internal" });
  await deploy(page, consumer);
  const logs = () => {
    const ids = execFileSync("docker", ["ps", "-q", "--filter", `label=com.docker.compose.project=cove-${consumer}`], { encoding: "utf8" }).split(/\s+/).filter(Boolean);
    return ids.length ? execFileSync("docker", ["logs", ids[0] ?? ""], { encoding: "utf8" }) : "";
  };
  await expect.poll(logs, { timeout: 180_000, intervals: [3000] }).toContain("create=");
  const output = logs();
  expect(output).toContain(`endpoint=https://${domain} bucket=${bucket?.bucket}`);
  expect(output).toContain("read=scoped-object");
  expect(output).toContain("other=denied");
  expect(output).toContain("create=denied");

  await page.goto("/storage");
  await expect(page.getByText("Storage on the same node is not offsite", { exact: false }).first()).toBeVisible();
  await capture(page, testInfo, "object-storage-destination");
  await page.goto(`/resources/${consumer}/backups`);
  await page.getByRole("button", { name: "Configure backups" }).click();
  await chooseOption(page.getByRole("combobox", { name: "Storage location" }), /same node, not offsite/);
  await expect(page.getByText("Not an offsite backup")).toBeVisible();
  await capture(page, testInfo, "object-storage-backup-policy");
  await page.getByRole("button", { name: "Cancel", exact: true }).click();

  await page.goto(`/resources/${consumer}/object-storage`);
  await page.getByRole("button", { name: "Revoke access" }).click();
  const revoke = page.getByRole("dialog", { name: "Revoke bucket access?" });
  await revoke.getByRole("button", { name: "Revoke access" }).click();
  await expect(page.getByRole("button", { name: "Provision bucket" })).toBeVisible({ timeout: 120_000 });
  // The data stays in Garage: its built-in administrative key can still read the object.
  const garageContainer = execFileSync("docker", ["ps", "-q", "--filter", `label=com.docker.compose.project=cove-${garage}`], { encoding: "utf8" }).trim();
  const info = execFileSync("docker", ["exec", garageContainer, "/garage", "bucket", "info", bucket?.bucket ?? ""], { encoding: "utf8" });
  expect(info).toMatch(/Objects:\s*1|objects:\s*1|1 object/i);
  detachWorker(garageBridge);
});

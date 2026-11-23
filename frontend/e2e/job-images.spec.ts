import { execFileSync } from "node:child_process";
import { expect, test, type Page } from "@playwright/test";
import { capture, cleanupBrowserResources } from "./route-tls";

const password = process.env.COVE_BROWSER_PASSWORD ?? "";

test.afterEach(cleanupBrowserResources);

// One-shot job: each run appends a line to a volume, so a check that started the job would be visible. pull_policy
// never keeps it on the busybox image the harness already loaded.
const jobCompose = (exit: number) =>
  [
    "services:",
    "  task:",
    "    image: busybox:1.37",
    "    pull_policy: never",
    `    command: [sh, -c, 'echo run >> /runs/count; exit ${exit}']`,
    "    volumes:",
    "      - runs:/runs",
    "volumes:",
    "  runs: {}",
    "",
  ].join("\n");

interface Operation {
  id: string;
  action: string;
  state: string;
  result: { error?: string } | null;
}

interface WorkloadImages {
  checked_at: string | null;
  operation_id: string;
  stale: boolean;
  images: { service: string; reference: string; image_id: string; status: string }[] | null;
}

const docker = (...args: string[]) => execFileSync("docker", args, { encoding: "utf8" }).trim();

// The isolated browser database only; refuses any other container.
function sql(query: string) {
  const database = process.env.COVE_BROWSER_DATABASE;
  if (!database || !/^cove-browser-[a-f0-9]+-db$/.test(database)) throw new Error("Isolated browser database required");
  return docker("exec", database, "psql", "-U", "postgres", "-d", "cove", "-Atc", query);
}

// Every container of the job's Compose project, including stopped ones retained from the last run.
function retained(id: string) {
  const ids = docker("ps", "-aq", "--filter", `label=com.docker.compose.project=cove-${id}`).split(/\s+/).filter(Boolean);
  if (ids.length === 0) return [];
  return docker("inspect", "--format", "{{.Id}}|{{.State.Status}}|{{.State.StartedAt}}|{{.State.FinishedAt}}|{{.State.ExitCode}}", ...ids).split(/\r?\n/).sort();
}

const running = (id: string) => docker("ps", "-q", "--filter", `label=com.docker.compose.project=cove-${id}`);

// Read from the job's volume in a throwaway container, so the job's own container is never started.
function runCount(id: string) {
  const volume = docker("volume", "ls", "-q", "--filter", `label=com.docker.compose.project=cove-${id}`, "--filter", "label=com.docker.compose.volume=runs");
  if (!volume) throw new Error("The job's runs volume is missing");
  return docker("run", "--rm", "--network", "none", "--mount", `type=volume,source=${volume},target=/runs,readonly`, "busybox:1.37", "cat", "/runs/count").split(/\r?\n/).length;
}

async function signIn(page: Page) {
  await page.goto("/projects");
  await expect(page).toHaveURL(/\/login(?:[?#]|$)/);
  await page.getByLabel("Password", { exact: true }).fill(password);
  await page.getByRole("button", { name: "Sign in", exact: true }).click();
  await expect(page).not.toHaveURL(/\/login(?:[?#]|$)/);
  return { Origin: new URL(page.url()).origin };
}

test("job images: a check reads the retained containers of the last run without running the job", async ({ page }, testInfo) => {
  test.setTimeout(420_000);
  const headers = await signIn(page);

  const nodes = (await (await page.request.get("/api/nodes")).json()) as { id: string; name: string; capabilities: { image_lifecycle?: boolean; job_image_lifecycle?: boolean; job_runs?: boolean } | null }[];
  const node = nodes.find((item) => item.name === "ready-browser-worker");
  if (!node) throw new Error("The browser worker is missing");
  expect(node.capabilities).toMatchObject({ image_lifecycle: true, job_image_lifecycle: true, job_runs: true });

  const project = (await (await page.request.post("/api/projects", { headers, data: { name: "Job images project" } })).json()) as { id: string };
  const created = await page.request.post("/api/workloads", { headers, data: { name: "job-images", node_id: node.id, kind: "job", job_service: "task", compose: jobCompose(0), project_id: project.id } });
  expect(created.status(), await created.text()).toBe(201);
  const { id } = (await created.json()) as { id: string };
  // Background hourly checks start due; defer this job's so the only new operation is the check queued below.
  expect(id).toMatch(/^[a-f0-9]{32}$/);
  // psql also prints the command tag, so only the returned IDs are compared.
  const deferred = sql(`UPDATE workloads SET next_image_check_at=now()+interval '1 day' WHERE id='${id}' AND kind='job' RETURNING id`).split(/\s+/).filter((value) => /^[a-f0-9]{32}$/.test(value));
  expect(deferred).toEqual([id]);
  const resource = (await (await page.request.get(`/api/workloads/${id}`)).json()) as { name: string; node_id: string };

  const operations = async () => (await (await page.request.get(`/api/operations?workload_id=${id}`)).json()) as Operation[];
  const idle = () => expect.poll(async () => (await operations()).filter((item) => item.state === "queued" || item.state === "running").map((item) => item.action), { timeout: 120_000 }).toEqual([]);
  const finished = async (operationId: string) => {
    const read = async () => (await (await page.request.get(`/api/operations/${operationId}`)).json()) as Operation;
    await expect.poll(async () => (await read()).state, { timeout: 180_000 }).toMatch(/^(succeeded|failed|cancelled|interrupted)$/);
    return read();
  };
  const run = async () => {
    await idle();
    const response = await page.request.post(`/api/workloads/${id}/operations`, { headers, data: { action: "deploy" } });
    expect(response.status(), await response.text()).toBe(202);
    return finished(((await response.json()) as { id: string }).id);
  };
  const images = async () => (await (await page.request.get(`/api/workloads/${id}/images`)).json()) as WorkloadImages;
  const schedule = async () => (await (await page.request.get(`/api/workloads/${id}/schedule`)).json()) as Record<string, unknown>;

  // Never run yet: the page explains that the job must run before there is anything to check.
  await page.goto(`/resources/${id}/registry`);
  const section = page.getByRole("region", { name: "Images", exact: true });
  await expect(section.getByText(/If the job has never run, run it first/)).toBeVisible({ timeout: 30_000 });
  await expect(page.getByRole("button", { name: "Pull images and redeploy" })).toHaveCount(0);

  // A yearly schedule that cannot fire during the test, so its saved fields can be compared after the check.
  const saved = await page.request.put(`/api/workloads/${id}/schedule`, { headers, data: { cron: "0 0 1 1 *", timezone: "UTC", overlap_policy: "skip", timeout_seconds: 60 } });
  expect(saved.status(), await saved.text()).toBe(200);

  const first = await run();
  expect(first.state, first.result?.error).toBe("succeeded");
  expect(running(id)).toBe("");
  const before = retained(id);
  expect(before.length).toBeGreaterThan(0);
  for (const container of before) expect(container).toMatch(/^[a-f0-9]{64}\|exited\|[^|]+\|[^|]+\|0$/);
  expect(runCount(id)).toBe(1);
  const scheduleBefore = await schedule();
  const operationsBefore = (await operations()).map((item) => item.id);

  // The check from the dashboard: exactly one image_check request, and no pull or redeploy control.
  await page.reload();
  const check = section.getByRole("button", { name: "Check images", exact: true });
  await expect(check).toBeEnabled({ timeout: 60_000 });
  await expect(page.getByRole("button", { name: "Pull images and redeploy" })).toHaveCount(0);
  await expect(section.getByText(/never run the job, pull images or start containers/)).toBeVisible();
  await capture(page, testInfo, "job-images-before-check");
  const queued = page.waitForResponse((response) => new URL(response.url()).pathname === `/api/workloads/${id}/operations` && response.request().method() === "POST");
  await check.focus();
  await page.keyboard.press("Enter");
  const response = await queued;
  expect(response.status(), await response.text()).toBe(202);
  expect(response.request().postDataJSON()).toEqual({ action: "image_check" });
  await expect(section.getByText("Image check queued", { exact: true })).toBeVisible();
  const checked = await finished(((await response.json()) as { id: string }).id);
  expect(checked.state, checked.result?.error).toBe("succeeded");

  // The check started nothing: same retained containers, timestamps and exit codes, one run in the volume.
  expect(retained(id)).toEqual(before);
  expect(running(id)).toBe("");
  expect(runCount(id)).toBe(1);
  expect(await schedule()).toEqual(scheduleBefore);
  const after = await operations();
  expect(after.filter((item) => !operationsBefore.includes(item.id)).map((item) => item.action)).toEqual(["image_check"]);

  const result = await images();
  expect(result).toMatchObject({ operation_id: checked.id, stale: false });
  expect(result.checked_at).not.toBeNull();
  expect((result.images ?? []).map((image) => [image.service, image.reference])).toEqual([["task", "busybox:1.37"]]);
  await expect(section.getByRole("list", { name: "Image status", exact: true })).toContainText("busybox:1.37", { timeout: 30_000 });
  await expect(section.getByText("Result is from before the latest run")).toHaveCount(0);
  await expect(page.getByRole("button", { name: "Pull images and redeploy" })).toHaveCount(0);
  await capture(page, testInfo, "job-images-checked");

  // A newer run that fails makes the cached result stale, because the job's images may have changed.
  await idle();
  const updated = await page.request.put(`/api/workloads/${id}`, { headers, data: { name: resource.name, node_id: resource.node_id, kind: "job", job_service: "task", compose: jobCompose(7) } });
  expect([200, 202], await updated.text()).toContain(updated.status());
  const failed = await run();
  expect(failed.state).toBe("failed");
  await expect.poll(async () => (await images()).stale, { timeout: 30_000 }).toBe(true);
  await expect(section.getByText("Result is from before the latest run")).toBeVisible({ timeout: 60_000 });
  await expect(section.getByRole("list", { name: "Image status at the last check", exact: true })).toContainText("busybox:1.37");
  await expect(page.getByRole("button", { name: "Pull images and redeploy" })).toHaveCount(0);
  await capture(page, testInfo, "job-images-stale");
});

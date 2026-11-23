import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { ReactNode } from "react";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { ApiError, request } from "../api/client";
import type { JobSchedule as Schedule, Node, Operation, Workload } from "../api/types";
import { JobSchedule } from "./job-schedule";

const base: Workload = { id: "job-1", name: "nightly", kind: "job", node_id: "n1", manually_stopped: false, deployed: true, permissions: ["read", "operate", "edit"], project_id: "p1", compose: "", environment_keys: [], source: null, has_source_token: false };
let resource: Workload = base;

vi.mock("@tanstack/react-router", () => ({ Link: ({ children }: { children: ReactNode }) => <a href="/">{children}</a> }));
vi.mock("../api/client", async importOriginal => ({ ...await importOriginal<typeof import("../api/client")>(), request: vi.fn() }));
vi.mock("./workload-detail", () => ({ useResource: () => resource }));
// jsdom does not implement modal dialogs.
HTMLDialogElement.prototype.showModal = function (this: HTMLDialogElement) { this.open = true; };
HTMLDialogElement.prototype.close = function (this: HTMLDialogElement) { this.open = false; };
beforeEach(() => {
  resource = base;
  vi.stubGlobal("ResizeObserver", class { observe = vi.fn(); disconnect = vi.fn(); });
});
afterEach(() => {
  vi.clearAllMocks();
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
});

const saved: Schedule = { cron: "0 9 * * *", timezone: "America/Los_Angeles", next_run_at: "2026-11-23T17:00:00Z", overlap_policy: "skip", timeout_seconds: 1800, pending_since: null };

function node(jobRuns = true, state: Node["state"] = "ready"): Node {
  return { id: "n1", name: "worker-1", address: "", port: 22, state, mode: "ssh", last_checked_at: null, capabilities: { protocol: 1, os: "linux", arch: "amd64", docker: "27", compose: "2", ready: true, job_runs: jobRuns } };
}

function serve({ schedule = saved, put, nodes = [node()], operations = [], post }: { schedule?: Schedule; put?: (input: unknown) => Promise<unknown>; nodes?: Node[] | Error; operations?: Operation[] | Error; post?: () => Promise<unknown> } = {}) {
  vi.mocked(request).mockImplementation((method: string, path: string, body?: unknown) => {
    if (path === "/api/workloads/job-1/schedule") return method === "GET" ? Promise.resolve(schedule) : (put?.(body) ?? Promise.resolve({ ...schedule, ...(body as object), pending_since: null }));
    if (path === "/api/nodes") return nodes instanceof Error ? Promise.reject(nodes) : Promise.resolve(nodes);
    if (path.startsWith("/api/operations")) return operations instanceof Error ? Promise.reject(operations) : Promise.resolve(operations);
    if (path === "/api/environments/e1/suspension") return Promise.resolve({ paused: true, phase: "paused", resources: [{ id: "job-1", name: "nightly", kind: "job", node_id: "n1", supported: true, phase: "paused", resume_wanted: false }] });
    if (method === "POST" && path === "/api/workloads/job-1/operations") return post?.() ?? Promise.resolve({ id: "op-run", state: "queued" });
    if (path === "/api/workloads/job-1/capacity") return Promise.resolve({ checks: [{ name: "Memory", state: "pass", message: "Enough memory" }] });
    if (path === "/api/workloads/job-1/configuration-diff") return Promise.resolve({ has_baseline: true, operation_id: "op0", revision: "", fields: [], diffs: [] });
    return Promise.reject(new Error(`unexpected ${method} ${path}`));
  });
}

function renderSchedule() {
  render(<QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}><JobSchedule /></QueryClientProvider>);
}

const saveButton = () => screen.getByRole("button", { name: "Save schedule" });
const runButton = () => screen.getByRole("button", { name: "Run now" });
// A passed capacity check opens the run's configuration review.
const runDialog = () => screen.findByRole("dialog", { name: /^Run .+ now\?$/ });
async function confirmRun(dialog: HTMLElement) {
  const confirm = within(dialog).getByRole("button", { name: "Run now" });
  await waitFor(() => expect(confirm).toBeEnabled());
  fireEvent.click(confirm);
}

function operation(overrides: Partial<Operation>): Operation {
  return { id: "op-1", workload_id: "job-1", action: "deploy", state: "succeeded", result: null, created_at: "2026-11-22T10:00:00Z", ...overrides };
}

it("explains a suspended schedule without an owner and claims it only when saved", async () => {
  serve({ schedule: { ...saved, ownership_required: true }, put: () => Promise.resolve({ ...saved, ownership_required: false }) });
  renderSchedule();
  expect(await screen.findByText("Scheduled runs are suspended")).toBeInTheDocument();
  expect(screen.getByText(/Save the schedule to make your account its owner and resume future runs/)).toBeInTheDocument();
  expect(screen.getByText("No run is scheduled while the schedule has no owner.")).toBeInTheDocument();
  expect(screen.queryByText(/Next run:/)).not.toBeInTheDocument();
  expect(vi.mocked(request).mock.calls.filter(([method]) => method === "PUT")).toHaveLength(0);

  await waitFor(() => expect(saveButton()).toBeEnabled());
  fireEvent.click(saveButton());
  await waitFor(() => expect(request).toHaveBeenCalledWith("PUT", "/api/workloads/job-1/schedule", { cron: "0 9 * * *", timezone: "America/Los_Angeles", overlap_policy: "skip", timeout_seconds: 1800 }));
  await waitFor(() => expect(screen.queryByText("Scheduled runs are suspended")).not.toBeInTheDocument());
  expect(screen.getByText(/Next run:/)).toBeInTheDocument();
});

it("tells viewers without configuration access who can resume a suspended schedule", async () => {
  resource = { ...base, permissions: ["read", "operate"] };
  serve({ schedule: { ...saved, ownership_required: true } });
  renderSchedule();
  expect(await screen.findByText(/no currently authorised owner/)).toBeInTheDocument();
  expect(screen.getByText(/Someone with Edit configuration and Operate access must save the schedule/)).toBeInTheDocument();
  expect(screen.queryByRole("button", { name: "Save schedule" })).not.toBeInTheDocument();
});

it("lets configuration editors without Operate clear a schedule but not claim one", async () => {
  resource = { ...base, permissions: ["read", "edit", "edit_configuration"] };
  serve({ schedule: { ...saved, ownership_required: true } });
  renderSchedule();
  expect(await screen.findByText(/Resuming it needs Operate access as well as Edit configuration/)).toBeInTheDocument();
  expect(screen.getByText("You can disable scheduling. Saving a cron expression also needs Operate access.")).toBeInTheDocument();

  await waitFor(() => expect(saveButton()).toBeEnabled());
  fireEvent.click(saveButton());
  expect(await screen.findByText(/Saving a cron schedule also needs Operate access to this job/)).toBeInTheDocument();
  expect(screen.getByLabelText("Cron expression")).toHaveAttribute("aria-invalid", "true");
  expect(vi.mocked(request).mock.calls.filter(([method]) => method === "PUT")).toHaveLength(0);

  fireEvent.change(screen.getByLabelText("Cron expression"), { target: { value: "" } });
  fireEvent.click(saveButton());
  await waitFor(() => expect(request).toHaveBeenCalledWith("PUT", "/api/workloads/job-1/schedule", { cron: "", timezone: "America/Los_Angeles", overlap_policy: "skip", timeout_seconds: 1800 }));
});

it("shows the saved time zone and saves it with the execution settings", async () => {
  serve({ put: () => Promise.resolve({ ...saved, cron: "0 9 * * 1", timezone: "America/New_York", next_run_at: "2026-11-23T14:00:00Z" }) });
  renderSchedule();
  const timezone = await screen.findByLabelText("Time zone");
  expect(timezone).toHaveValue("America/Los_Angeles");
  expect(screen.getByText(/Nov 23, 2026, 9:00:00 AM PST/)).toBeInTheDocument();
  expect(screen.getByRole("combobox", { name: "When a run overlaps" })).toHaveTextContent("Skip");
  expect(screen.getByLabelText("Timeout (seconds)")).toHaveValue(1800);

  fireEvent.change(screen.getByLabelText("Cron expression"), { target: { value: " 0 9 * * 1 " } });
  fireEvent.change(timezone, { target: { value: "America/New_York" } });
  await waitFor(() => expect(saveButton()).toBeEnabled());
  fireEvent.click(saveButton());
  await waitFor(() => expect(request).toHaveBeenCalledWith("PUT", "/api/workloads/job-1/schedule", { cron: "0 9 * * 1", timezone: "America/New_York", overlap_policy: "skip", timeout_seconds: 1800 }));
  expect(await screen.findByText(/Nov 23, 2026, 9:00:00 AM EST/)).toBeInTheDocument();
  expect(screen.getByText("Schedule saved")).toBeInTheDocument();
});

it("saves a chosen overlap policy and timeout", async () => {
  const user = userEvent.setup();
  serve();
  renderSchedule();
  const overlap = await screen.findByRole("combobox", { name: "When a run overlaps" });
  await waitFor(() => expect(overlap).toBeEnabled());
  await user.click(overlap);
  await user.click(screen.getByRole("option", { name: "Replace" }));
  const timeout = screen.getByLabelText("Timeout (seconds)");
  await user.clear(timeout);
  await user.type(timeout, "90");
  await user.click(saveButton());
  await waitFor(() => expect(request).toHaveBeenCalledWith("PUT", "/api/workloads/job-1/schedule", { cron: "0 9 * * *", timezone: "America/Los_Angeles", overlap_policy: "replace", timeout_seconds: 90 }));
  expect(await screen.findByRole("combobox", { name: "When a run overlaps" })).toHaveTextContent("Replace");
  expect(screen.getByLabelText("Timeout (seconds)")).toHaveValue(90);
});

it("keeps the saved time zone and execution settings when the schedule is disabled", async () => {
  serve({ schedule: { ...saved, cron: "*/15 * * * *", timezone: "Europe/Berlin", overlap_policy: "queue_one", timeout_seconds: 600 }, put: () => Promise.resolve({ ...saved, cron: "", timezone: "Europe/Berlin", next_run_at: null, overlap_policy: "queue_one", timeout_seconds: 600 }) });
  renderSchedule();
  fireEvent.change(await screen.findByLabelText("Cron expression"), { target: { value: "" } });
  await waitFor(() => expect(saveButton()).toBeEnabled());
  fireEvent.click(saveButton());
  await waitFor(() => expect(request).toHaveBeenCalledWith("PUT", "/api/workloads/job-1/schedule", { cron: "", timezone: "Europe/Berlin", overlap_policy: "queue_one", timeout_seconds: 600 }));
  expect(await screen.findByText("Scheduling is disabled.")).toBeInTheDocument();
  expect(screen.getByLabelText("Time zone")).toHaveValue("Europe/Berlin");
  expect(screen.getByRole("combobox", { name: "When a run overlaps" })).toHaveTextContent("Queue one");
});

it("rejects timeouts outside the limits next to the field without sending", async () => {
  serve();
  renderSchedule();
  const timeout = await screen.findByLabelText("Timeout (seconds)");
  const cases: [string, string][] = [["4", "Use 5 to 86,400 seconds."], ["86401", "Use 5 to 86,400 seconds."], ["", "Enter a timeout in seconds."], ["1.5", "Use a whole number of seconds."]];
  for (const [value, message] of cases) {
    fireEvent.change(timeout, { target: { value } });
    fireEvent.click(saveButton());
    expect(await screen.findByText(message)).toBeInTheDocument();
    expect(timeout).toHaveAttribute("aria-invalid", "true");
    expect(timeout.getAttribute("aria-describedby")).toContain("job-timeout-error");
  }
  expect(request).not.toHaveBeenCalledWith("PUT", expect.anything(), expect.anything());
  fireEvent.change(timeout, { target: { value: "86400" } });
  fireEvent.click(saveButton());
  await waitFor(() => expect(request).toHaveBeenCalledWith("PUT", "/api/workloads/job-1/schedule", expect.objectContaining({ timeout_seconds: 86400 })));
});

it("keeps the typed values and shows the server message when saving fails", async () => {
  serve({ put: () => Promise.reject(new ApiError(400, "use skip, queue_one or replace and a timeout from 5 to 86400 seconds")) });
  renderSchedule();
  const cron = await screen.findByLabelText("Cron expression");
  fireEvent.change(cron, { target: { value: "*/5 * * * *" } });
  fireEvent.change(screen.getByLabelText("Timeout (seconds)"), { target: { value: "120" } });
  fireEvent.click(saveButton());
  expect(await screen.findByText("use skip, queue_one or replace and a timeout from 5 to 86400 seconds")).toBeInTheDocument();
  expect(screen.getByText("Could not save the schedule")).toBeInTheDocument();
  expect(cron).toHaveValue("*/5 * * * *");
  expect(screen.getByLabelText("Timeout (seconds)")).toHaveValue(120);
});

it("explains a worker upgrade conflict", async () => {
  serve({ put: () => Promise.reject(new ApiError(409, "upgrade and verify the worker before configuring job execution")) });
  renderSchedule();
  fireEvent.change(await screen.findByLabelText("Timeout (seconds)"), { target: { value: "60" } });
  await waitFor(() => expect(saveButton()).toBeEnabled());
  fireEvent.click(saveButton());
  expect(await screen.findByText("upgrade and verify the worker before configuring job execution")).toBeInTheDocument();
});

it("lets an older worker save cron and time zone with the default execution settings omitted", async () => {
  serve({ nodes: [{ ...node(), capabilities: { protocol: 1, os: "linux", arch: "amd64", docker: "27", compose: "2", ready: true } }] });
  renderSchedule();
  expect(await screen.findByText("Worker upgrade needed")).toBeInTheDocument();
  expect(screen.getByText(/Cron, time zone and Run now still work/)).toBeInTheDocument();
  expect(screen.getByRole("combobox", { name: "When a run overlaps" })).toBeDisabled();
  expect(screen.getByLabelText("Timeout (seconds)")).toBeDisabled();
  fireEvent.change(screen.getByLabelText("Cron expression"), { target: { value: "0 1 * * *" } });
  fireEvent.click(saveButton());
  await waitFor(() => expect(request).toHaveBeenCalledWith("PUT", "/api/workloads/job-1/schedule", { cron: "0 1 * * *", timezone: "America/Los_Angeles" }));
  await waitFor(() => expect(runButton()).toBeEnabled());
});

it("resets saved advanced settings on an older worker and blocks runs that need the timeout", async () => {
  serve({ nodes: [node(false)], schedule: { ...saved, overlap_policy: "replace", timeout_seconds: 60 } });
  renderSchedule();
  expect(await screen.findByText(/so schedule changes and runs are rejected/)).toBeInTheDocument();
  expect(screen.getByLabelText("Timeout (seconds)")).toBeDisabled();
  expect(runButton()).toBeDisabled();
  expect(screen.getByText(/cannot apply the saved timeout/)).toBeInTheDocument();
  fireEvent.click(screen.getByRole("button", { name: "Reset to defaults" }));
  await waitFor(() => expect(request).toHaveBeenCalledWith("PUT", "/api/workloads/job-1/schedule", { cron: "0 9 * * *", timezone: "America/Los_Angeles", overlap_policy: "skip", timeout_seconds: 1800 }));
  expect(await screen.findByText(/Cron, time zone and Run now still work/)).toBeInTheDocument();
  expect(screen.queryByRole("button", { name: "Reset to defaults" })).not.toBeInTheDocument();
  await waitFor(() => expect(runButton()).toBeEnabled());
});

it("blocks running when the job's node cannot be found", async () => {
  serve({ nodes: [] });
  renderSchedule();
  expect(await screen.findByText(/cannot find this job's node/)).toBeInTheDocument();
  expect(runButton()).toBeDisabled();
});

it("is read-only without edit access", async () => {
  resource = { ...base, permissions: ["read"] };
  serve();
  renderSchedule();
  expect(await screen.findByLabelText("Cron expression")).toBeDisabled();
  expect(screen.getByLabelText("Timeout (seconds)")).toBeDisabled();
  expect(screen.queryByRole("button", { name: "Save schedule" })).not.toBeInTheDocument();
  expect(screen.getByText("Changing the schedule needs edit access.")).toBeInTheDocument();
  expect(screen.queryByRole("button", { name: "Run now" })).not.toBeInTheDocument();
  expect(screen.getByText("Running the job needs operator access.")).toBeInTheDocument();
});

it("disables saving and running while offline", async () => {
  vi.spyOn(navigator, "onLine", "get").mockReturnValue(false);
  serve();
  renderSchedule();
  expect(await screen.findByRole("button", { name: "Save schedule" })).toBeDisabled();
  expect(runButton()).toBeDisabled();
  expect(screen.getByText("You are offline. Run now is available when the connection returns.")).toBeInTheDocument();
});

it("shows a waiting scheduled run", async () => {
  serve({ schedule: { ...saved, overlap_policy: "queue_one", pending_since: "2026-11-22T09:30:00Z" } });
  renderSchedule();
  expect(await screen.findByText("A scheduled run is waiting")).toBeInTheDocument();
  expect(screen.getByText(/Only one run waits; missed ticks are not replayed/)).toBeInTheDocument();
});

it("queues a run after confirmation and links to the operation", async () => {
  serve();
  renderSchedule();
  await waitFor(() => expect(runButton()).toBeEnabled());
  expect(screen.getByText("This job has not run yet.")).toBeInTheDocument();
  fireEvent.click(runButton());
  const dialog = await runDialog();
  expect(request).toHaveBeenCalledWith("GET", "/api/workloads/job-1/capacity");
  expect(within(dialog).getByText(/still going after 30 minutes/)).toBeInTheDocument();
  expect(within(dialog).queryByText(/resumes its schedule/)).not.toBeInTheDocument();
  fireEvent.change(within(dialog).getByRole("textbox", { name: /^Reason/ }), { target: { value: "backfill" } });
  await confirmRun(dialog);
  await waitFor(() => expect(request).toHaveBeenCalledWith("POST", "/api/workloads/job-1/operations", { action: "deploy", reason: "backfill" }));
  expect(await screen.findByText("Run queued")).toBeInTheDocument();
});

it("explains that running a manually stopped job resumes its schedule", async () => {
  resource = { ...base, manually_stopped: true };
  serve();
  renderSchedule();
  await waitFor(() => expect(runButton()).toBeEnabled());
  fireEvent.click(runButton());
  expect(within(await runDialog()).getByText(/Running it now also resumes its schedule/)).toBeInTheDocument();
});

it("reports a rejected run", async () => {
  serve({ post: () => Promise.reject(new ApiError(409, "deployments are frozen for this environment")) });
  renderSchedule();
  await waitFor(() => expect(runButton()).toBeEnabled());
  fireEvent.click(runButton());
  await confirmRun(await runDialog());
  expect(await screen.findByText("deployments are frozen for this environment")).toBeInTheDocument();
  expect(screen.getByText("Could not queue the run")).toBeInTheDocument();
});

it("blocks running while another operation is active", async () => {
  serve({ operations: [operation({ action: "build", state: "running" })] });
  renderSchedule();
  expect(await screen.findByText(/Build and publish running. Run now is available when it finishes./)).toBeInTheDocument();
  expect(runButton()).toBeDisabled();
});

it("blocks running when operations cannot be read", async () => {
  serve({ operations: new ApiError(503, "unavailable") });
  renderSchedule();
  expect(await screen.findByText(/Cove cannot tell whether another operation is running/)).toBeInTheDocument();
  expect(runButton()).toBeDisabled();
});

it("blocks running while the node is not ready", async () => {
  serve({ nodes: [node(true, "unreachable")] });
  renderSchedule();
  expect(await screen.findByText("worker-1 is not ready. Run now is available once the node is ready.")).toBeInTheDocument();
  expect(runButton()).toBeDisabled();
});

it("shows the reported job result of the latest run without inferring success", async () => {
  serve({ operations: [operation({ state: "failed", result: { id: "op-1", workload: "job-1", action: "deploy", state: "failed", created_at: "2026-11-22T10:00:00Z", job: { timed_out: true, containers_stopped: false, logs_unavailable: true } } })] });
  renderSchedule();
  expect(await screen.findByText("Execution timed out.")).toBeInTheDocument();
  expect(screen.getByText(/Cleanup could not be confirmed/)).toBeInTheDocument();
  expect(screen.getByText("Output could not be collected from the worker.")).toBeInTheDocument();
});

it("explains that only jobs can be scheduled", async () => {
  resource = { ...base, kind: "service" };
  serve();
  renderSchedule();
  expect(await screen.findByText(/Create a one-shot job resource/)).toBeInTheDocument();
  expect(screen.queryByRole("button", { name: "Run now" })).not.toBeInTheDocument();
});

it("blocks Run now while the job's environment is paused", async () => {
  resource = { ...base, environment_id: "e1", environment_paused: true };
  serve();
  renderSchedule();
  expect(await screen.findByText("The environment is paused. Running the job is available after it resumes.")).toBeInTheDocument();
  expect(runButton()).toBeDisabled();
});

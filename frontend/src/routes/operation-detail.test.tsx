import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, expect, it, vi } from "vitest";
import { ApiError, request } from "../api/client";
import type { Operation } from "../api/types";
import { OperationPage } from "./operation-detail";

vi.mock("../api/client", async (original) => ({ ...(await original<typeof import("../api/client")>()), request: vi.fn() }));
vi.mock("@tanstack/react-router", () => ({
  getRouteApi: () => ({ useParams: () => ({ operationId: "op-1" }) }),
  useNavigate: () => vi.fn(),
  Link: ({ children }: { children: React.ReactNode }) => <span>{children}</span>,
}));
vi.mock("../components/LogViewer", () => ({ LogViewer: () => null }));

// jsdom does not implement modal dialogs.
HTMLDialogElement.prototype.showModal = function (this: HTMLDialogElement) {
  this.open = true;
};
HTMLDialogElement.prototype.close = function (this: HTMLDialogElement) {
  this.open = false;
};

afterEach(() => vi.clearAllMocks());

const workload = { id: "w1", name: "web", kind: "service", node_id: "n1", node: "node-a", project_id: "p1", permissions: ["read", "operate", "edit", "view", "read_secrets", "edit_configuration", "manage_backups"] };

function operation(overrides: Partial<Operation>): Operation {
  return { id: "op-1", workload_id: "w1", action: "deploy", state: "queued", result: null, created_at: "2026-11-22T09:00:00Z", can_approve: true, can_override: true, reason: "", emergency_override_at: null, emergency_override_by: null, emergency_override_reason: "", ...overrides };
}

function mockApi(detail: Operation, override?: () => Promise<unknown>) {
  vi.mocked(request).mockImplementation((method, path) => {
    if (path === "/api/operations/op-1") return Promise.resolve(detail);
    if (path === "/api/workloads") return Promise.resolve([workload]);
    if (path === "/api/projects") return Promise.resolve([]);
    if (path === "/api/workloads/w1/releases") return Promise.resolve({ releases: [{ id: "op-1", created_at: detail.created_at }] });
    if (method === "POST" && path === "/api/operations/op-1/emergency-override") return override ? override() : Promise.resolve({ ok: true });
    if (method === "POST" && path === "/api/workloads/w1/operations") return Promise.resolve({ id: "op-2", state: "queued" });
    return Promise.reject(new Error(`unexpected ${method} ${path}`));
  });
}

function renderPage() {
  render(<QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}><OperationPage /></QueryClientProvider>);
}

it("shows the reason recorded with a deployment", async () => {
  mockApi(operation({ state: "succeeded", reason: "Ship the checkout fix", can_override: false }));
  renderPage();
  expect(await screen.findByText("Ship the checkout fix")).toBeInTheDocument();
  expect(screen.getByText("Reason")).toBeInTheDocument();
});

it("requires a reason before an emergency override and then submits it", async () => {
  mockApi(operation({}));
  renderPage();
  fireEvent.click(await screen.findByRole("button", { name: "Emergency override" }));
  const dialog = await screen.findByRole("dialog", { name: "Override the deployment schedule?" });
  fireEvent.click(within(dialog).getByRole("button", { name: "Override and allow start" }));
  expect(await within(dialog).findByText("Enter a reason.")).toBeInTheDocument();
  expect(request).not.toHaveBeenCalledWith("POST", "/api/operations/op-1/emergency-override", expect.anything());

  fireEvent.change(within(dialog).getByLabelText("Emergency reason"), { target: { value: "  Payment outage INC-42  " } });
  fireEvent.click(within(dialog).getByRole("button", { name: "Override and allow start" }));
  await waitFor(() => expect(request).toHaveBeenCalledWith("POST", "/api/operations/op-1/emergency-override", { reason: "Payment outage INC-42" }));
});

it("explains when an operation is no longer eligible for an override", async () => {
  mockApi(operation({}), () => Promise.reject(new ApiError(409, "operation is no longer eligible")));
  renderPage();
  fireEvent.click(await screen.findByRole("button", { name: "Emergency override" }));
  const dialog = await screen.findByRole("dialog", { name: "Override the deployment schedule?" });
  fireEvent.change(within(dialog).getByLabelText("Emergency reason"), { target: { value: "Outage" } });
  fireEvent.click(within(dialog).getByRole("button", { name: "Override and allow start" }));
  expect(await within(dialog).findByText(/no longer eligible/)).toBeInTheDocument();
});

it("hides the override from people who cannot use it and for ineligible operations", async () => {
  mockApi(operation({ can_override: false }));
  renderPage();
  expect(await screen.findByText("Summary")).toBeInTheDocument();
  expect(screen.queryByRole("button", { name: "Emergency override" })).not.toBeInTheDocument();
});

it("hides the override for operations that are not deploys or rollbacks", async () => {
  mockApi(operation({ action: "restart" }));
  renderPage();
  expect(await screen.findByText("Summary")).toBeInTheDocument();
  expect(screen.queryByRole("button", { name: "Emergency override" })).not.toBeInTheDocument();
});

it("shows a recorded override instead of offering another", async () => {
  mockApi(operation({ emergency_override_at: "2026-11-22T09:05:00Z", emergency_override_by: "user-7", emergency_override_reason: "Payment outage INC-42" }));
  renderPage();
  expect(await screen.findByText("Emergency override recorded")).toBeInTheDocument();
  expect(screen.getByText("Payment outage INC-42")).toBeInTheDocument();
  expect(screen.getByText("user-7")).toBeInTheDocument();
  expect(screen.queryByRole("button", { name: "Emergency override" })).not.toBeInTheDocument();
});

it("stops asking for approval once an emergency override is recorded on a queued deployment", async () => {
  mockApi(operation({ approval_required: true, approved_at: null, can_override: false }));
  renderPage();
  expect(await screen.findByText("Approval required")).toBeInTheDocument();
  expect(screen.getByRole("button", { name: "Approve deployment" })).toBeInTheDocument();
  cleanup();

  mockApi(operation({ approval_required: true, approved_at: null, emergency_override_at: "2026-11-22T09:05:00Z", emergency_override_by: "user-7", emergency_override_reason: "Payment outage INC-42" }));
  renderPage();
  expect(await screen.findByText("Emergency override recorded")).toBeInTheDocument();
  expect(screen.queryByText("Approval required")).not.toBeInTheDocument();
  expect(screen.queryByRole("button", { name: "Approve deployment" })).not.toBeInTheDocument();
  expect(screen.queryByRole("button", { name: "Emergency override" })).not.toBeInTheDocument();
});

it("sends an optional reason with a rollback", async () => {
  mockApi(operation({ state: "succeeded", can_override: false }));
  renderPage();
  const rollback = await screen.findByRole("button", { name: "Roll back to this release" });
  await waitFor(() => expect(rollback).toBeEnabled());
  fireEvent.click(rollback);
  const dialog = await screen.findByRole("dialog", { name: "Roll back to this release?" });
  expect(within(dialog).getByText(/Requesting a rollback turns off this resource's image update policy if it is set to deploy automatically, even if the rollback waits for approval or fails\./)).toHaveTextContent("Turn the policy on and save it to resume automatic updates.");
  fireEvent.change(within(dialog).getByLabelText(/Reason/), { target: { value: "Bad migration" } });
  fireEvent.click(within(dialog).getByRole("button", { name: "Roll back" }));
  await waitFor(() => expect(request).toHaveBeenCalledWith("POST", "/api/workloads/w1/operations", { action: "rollback", release: "op-1", reason: "Bad migration" }));
});

it("leaves the reason out of a rollback without one", async () => {
  mockApi(operation({ state: "succeeded", can_override: false }));
  renderPage();
  const rollback = await screen.findByRole("button", { name: "Roll back to this release" });
  await waitFor(() => expect(rollback).toBeEnabled());
  fireEvent.click(rollback);
  fireEvent.click(within(await screen.findByRole("dialog", { name: "Roll back to this release?" })).getByRole("button", { name: "Roll back" }));
  await waitFor(() => expect(request).toHaveBeenCalledWith("POST", "/api/workloads/w1/operations", { action: "rollback", release: "op-1" }));
});

it("shows a policy hold as a held deployment rather than a worker error", async () => {
  mockApi(operation({ can_override: false, result: { error: "Deployments are frozen until 18:00 UTC.", hold_reason: "Deployments are frozen until 18:00 UTC." } }));
  renderPage();
  expect(await screen.findByText("Deployment held")).toBeInTheDocument();
  expect(screen.getByText("Deployments are frozen until 18:00 UTC.")).toBeInTheDocument();
  expect(screen.queryByText("Worker error")).not.toBeInTheDocument();
  expect(screen.queryByRole("alert")).not.toBeInTheDocument();
  expect(screen.getByText("Not yet acknowledged by the worker")).toBeInTheDocument();
});

it("reports a control-plane failure without attributing it to the worker", async () => {
  mockApi(operation({ state: "cancelled", can_override: false, result: { error: "Deployment skipped because its prerequisite did not succeed." } }));
  renderPage();
  const alert = await screen.findByRole("alert");
  expect(within(alert).getByText("Operation error")).toBeInTheDocument();
  expect(within(alert).getByText("Deployment skipped because its prerequisite did not succeed.")).toBeInTheDocument();
});

it("still reports a real execution failure as a worker error", async () => {
  mockApi(operation({ state: "failed", can_override: false, result: { id: "op-1", workload: "w1", action: "deploy", state: "failed", created_at: "2026-11-22T09:00:00Z", error: "compose up exited with status 1" } }));
  renderPage();
  const alert = await screen.findByRole("alert");
  expect(within(alert).getByText("Worker error")).toBeInTheDocument();
  expect(within(alert).getByText("compose up exited with status 1")).toBeInTheDocument();
  expect(screen.queryByText("Deployment held")).not.toBeInTheDocument();
});

const dnsWarning = "DNS publication for app.example.com could not be confirmed within two minutes. Deployment continued; Cove will keep reconciling DNS and retrying certificate issuance. Check the Cloudflare connection and hostname configuration.";

it("keeps the DNS publication warning on a successful deployment without reporting an error", async () => {
  mockApi(operation({ state: "succeeded", can_override: false, result: { id: "op-1", workload: "w1", action: "deploy", state: "succeeded", created_at: "2026-11-22T09:00:00Z", dns_warning: dnsWarning } }));
  renderPage();
  expect(await screen.findByText("DNS publication was not confirmed")).toBeInTheDocument();
  expect(screen.getByText(dnsWarning)).toBeInTheDocument();
  expect(screen.getByText("DNS propagation and certificate issuance may still be pending.")).toBeInTheDocument();
  expect(screen.queryByRole("alert")).not.toBeInTheDocument();
  expect(screen.queryByText(/DNS (has )?propagated|certificate (was )?issued/i)).not.toBeInTheDocument();
});

it("shows the DNS publication warning beside a control-plane error", async () => {
  mockApi(operation({ state: "failed", can_override: false, result: { error: "Deployment failed after the routes were exposed.", dns_warning: dnsWarning } }));
  renderPage();
  expect(await screen.findByText(dnsWarning)).toBeInTheDocument();
  const alert = screen.getByRole("alert");
  expect(within(alert).getByText("Operation error")).toBeInTheDocument();
  expect(within(alert).queryByText(dnsWarning)).not.toBeInTheDocument();
});

it("keeps the DNS publication warning visible beside a deployment hold", async () => {
  mockApi(operation({ can_override: false, result: { error: "Deployments are frozen until 18:00 UTC.", hold_reason: "Deployments are frozen until 18:00 UTC.", dns_warning: dnsWarning } }));
  renderPage();
  expect(await screen.findByText("Deployment held")).toBeInTheDocument();
  expect(screen.getByText("Deployments are frozen until 18:00 UTC.")).toBeInTheDocument();
  expect(screen.getByText("DNS publication was not confirmed")).toBeInTheDocument();
  expect(screen.getByText(dnsWarning)).toBeInTheDocument();
  expect(screen.queryByText("Operation error")).not.toBeInTheDocument();
  expect(screen.queryByRole("alert")).not.toBeInTheDocument();
});

it("ignores a DNS warning that is not text", async () => {
  // Malformed server data the type cannot express.
  const malformed: unknown = { error: "", dns_warning: { raw: "unexpected" } };
  mockApi(operation({ state: "succeeded", can_override: false, result: malformed as Operation["result"] }));
  renderPage();
  expect(await screen.findByText("Summary")).toBeInTheDocument();
  expect(screen.queryByText("DNS publication was not confirmed")).not.toBeInTheDocument();
  expect(screen.queryByText(/unexpected/)).not.toBeInTheDocument();
});

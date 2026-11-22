import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { request } from "../api/client";
import { ThresholdRules } from "./ThresholdRules";

const manager = { is_admin: true, team_id: "one", teams: [{ id: "one", name: "Team", role: "owner", grants: [] }] };
const session = vi.hoisted((): { current: unknown } => ({ current: null }));
vi.mock("../api/client", () => ({ request: vi.fn(), errorMessage: (error: Error) => error.message }));
vi.mock("../lib/session", () => ({ useSession: () => session.current }));
beforeEach(() => {
  session.current = manager;
  vi.stubGlobal("ResizeObserver", class { observe = vi.fn(); disconnect = vi.fn(); });
  HTMLDialogElement.prototype.showModal = vi.fn(function (this: HTMLDialogElement) { this.open = true; });
  HTMLDialogElement.prototype.close = vi.fn(function (this: HTMLDialogElement) { this.open = false; });
});
afterEach(() => { vi.clearAllMocks(); vi.unstubAllGlobals(); });

const nodes = [{ id: "n1", name: "edge-1", capabilities: { alert_observations: true } }, { id: "n2", name: "other-team-node" }, { id: "n3", name: "old-worker", capabilities: {} }];
const workloads = [
  { id: "w1", name: "api", kind: "service", node_id: "n1", project: "Shop", environment: "production" },
  { id: "w2", name: "nightly", kind: "job", node_id: "n1", project: "Shop", environment: "production" },
  { id: "w3", name: "legacy", kind: "service", node_id: "n3", project: "Shop" },
];
const channels = [
  { id: "c1", name: "Ops chat", notification: { provider: "slack", enabled: true, success: false, failure: true, health: true } },
  { id: "c2", name: "Backups only", notification: { provider: "discord", enabled: true, success: false, failure: false, events: ["backup.failed"] } },
  { id: "c3", name: "Owners", notification: { provider: "email", enabled: true, success: false, failure: true, health: true, owner: true } },
];
const rule = { id: "r1", name: "Hot CPU", node_id: "n1", workload_id: "", domain: "", metric: "cpu", threshold: 90, confirmation_seconds: 300, repeat_seconds: 3600, channel_ids: ["c1"] };

function renderRules(rules: unknown[]) {
  vi.mocked(request).mockImplementation((method, path) => {
    if (method !== "GET") return Promise.resolve(path === "/api/notifications/rules" ? { id: "r2" } : { ok: true });
    if (path === "/api/notifications/rules") return Promise.resolve(rules);
    if (path === "/api/notifications") return Promise.resolve(channels);
    if (path === "/api/nodes") return Promise.resolve(nodes);
    if (path === "/api/teams/one/nodes") return Promise.resolve(["n1", "n3"]);
    if (path === "/api/workloads") return Promise.resolve(workloads);
    if (path === "/api/workloads/w1") return Promise.resolve({ id: "w1", routes: [{ domain: "b.example.test", service: "web", port: 80 }, { domain: "a.example.test", service: "web", port: 80 }, { domain: "a.example.test", service: "admin", port: 81 }] });
    return Promise.reject(new Error(`unexpected ${path}`));
  });
  render(<QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}><ThresholdRules /></QueryClientProvider>);
}

function choose(label: string, option: string) {
  fireEvent.click(screen.getByRole("combobox", { name: label }));
  fireEvent.click(screen.getByRole("option", { name: option }));
}

it("creates a rule on a team node with the selected channels", async () => {
  renderRules([]);
  expect(await screen.findByText(/No threshold rules yet/)).toBeInTheDocument();
  fireEvent.click(screen.getByRole("button", { name: "New rule" }));
  fireEvent.click(screen.getByRole("combobox", { name: "Node" }));
  expect(screen.queryByRole("option", { name: "other-team-node" })).not.toBeInTheDocument();
  fireEvent.click(screen.getByRole("option", { name: "edge-1" }));
  fireEvent.click(screen.getByRole("combobox", { name: "Metric" }));
  expect(screen.getAllByRole("option").map((option) => option.textContent)).toEqual(["CPU usage", "Memory used", "Data disk used", "Certificate expiry", "Container restarts", "Out-of-memory kills", "Container crashes"]);
  fireEvent.click(screen.getByRole("option", { name: "Data disk used" }));
  expect(screen.getByText(/Only the filesystem that holds \/var\/lib\/cove/)).toBeInTheDocument();
  expect(screen.getByText("This channel does not select threshold events.")).toBeInTheDocument();
  expect(screen.getByText(/Resource owner emails skip node events/)).toBeInTheDocument();
  fireEvent.change(screen.getByLabelText("Name"), { target: { value: "Disk filling" } });
  fireEvent.change(screen.getByLabelText("Threshold (%)"), { target: { value: "85" } });
  choose("Lasting at least", "10 minutes");
  fireEvent.click(screen.getByRole("button", { name: "Create rule" }));
  expect(await screen.findByText("Select at least one channel.")).toBeInTheDocument();
  expect(request).not.toHaveBeenCalledWith("POST", expect.anything(), expect.anything());
  fireEvent.click(screen.getByRole("checkbox", { name: /Ops chat/ }));
  fireEvent.click(screen.getByRole("button", { name: "Create rule" }));
  await waitFor(() => expect(screen.queryByRole("button", { name: "Create rule" })).not.toBeInTheDocument());
  expect(request).toHaveBeenCalledWith("POST", "/api/notifications/rules", { name: "Disk filling", node_id: "n1", workload_id: "", domain: "", metric: "disk", threshold: 85, confirmation_seconds: 600, repeat_seconds: 3600, channel_ids: ["c1"] });
});

it("shows API errors when creating a rule", async () => {
  renderRules([]);
  fireEvent.click(await screen.findByRole("button", { name: "New rule" }));
  choose("Node", "edge-1");
  fireEvent.change(screen.getByLabelText("Name"), { target: { value: "Busy" } });
  fireEvent.change(screen.getByLabelText("Threshold (%)"), { target: { value: "95" } });
  fireEvent.click(screen.getByRole("checkbox", { name: /Ops chat/ }));
  vi.mocked(request).mockImplementationOnce(() => Promise.reject(new Error("a team may have at most 100 threshold rules")));
  fireEvent.click(screen.getByRole("button", { name: "Create rule" }));
  expect(await screen.findByText("a team may have at most 100 threshold rules")).toBeInTheDocument();
});

it("lists rules and warns that deleting does not close external incidents", async () => {
  renderRules([rule, { ...rule, id: "r9", name: "Moved", node_id: "n2", channel_ids: ["gone"] }]);
  const list = await screen.findByRole("list", { name: "Threshold rules" });
  expect(within(list).getByText("CPU usage at or above 90% for 5 minutes on edge-1 · repeats at most every 1 hour")).toBeInTheDocument();
  expect(within(list).getByText("Channels: Ops chat")).toBeInTheDocument();
  expect(within(list).getByText("Channels: Deleted channel")).toBeInTheDocument();
  expect(within(list).getByText(/no longer assigned to this team/)).toBeInTheDocument();
  fireEvent.click(screen.getByRole("button", { name: "Delete Hot CPU" }));
  const dialog = screen.getByRole("dialog", { name: "Delete Hot CPU?" });
  expect(within(dialog).getByText(/PagerDuty or Opsgenie incident for it stays open/)).toBeInTheDocument();
  fireEvent.click(within(dialog).getByRole("button", { name: "Delete rule" }));
  await waitFor(() => expect(request).toHaveBeenCalledWith("DELETE", "/api/notifications/rules/r1"));
});

it("does not load rules for team members who cannot manage them", () => {
  session.current = { is_admin: false, team_id: "one", teams: [{ id: "one", name: "Team", role: "member", grants: [] }] };
  renderRules([]);
  expect(screen.getByText("Team owners and admins manage threshold rules")).toBeInTheDocument();
  expect(request).not.toHaveBeenCalled();
});

it("creates a certificate rule for a configured domain of a team service", async () => {
  renderRules([]);
  fireEvent.click(await screen.findByRole("button", { name: "New rule" }));
  choose("Metric", "Certificate expiry");
  expect(screen.queryByRole("combobox", { name: "Node" })).not.toBeInTheDocument();
  expect(screen.queryByText(/Resource owner emails skip node events/)).not.toBeInTheDocument();
  expect(screen.getByRole("combobox", { name: "Domain" })).toBeDisabled();
  fireEvent.click(screen.getByRole("combobox", { name: "Service" }));
  expect(screen.getAllByRole("option").map((option) => option.textContent)).toEqual(["Select a service", "api", "legacy"]);
  fireEvent.click(screen.getByRole("option", { name: "api" }));
  await waitFor(() => expect(screen.getByRole("combobox", { name: "Domain" })).toBeEnabled());
  fireEvent.click(screen.getByRole("combobox", { name: "Domain" }));
  expect(screen.getAllByRole("option").map((option) => option.textContent)).toEqual(["Select a domain", "a.example.test", "b.example.test"]);
  fireEvent.click(screen.getByRole("option", { name: "b.example.test" }));
  fireEvent.change(screen.getByLabelText("Name"), { target: { value: "Cert expiring" } });
  fireEvent.change(screen.getByLabelText("Threshold (days)"), { target: { value: "4000" } });
  fireEvent.click(screen.getByRole("checkbox", { name: /Owners/ }));
  fireEvent.click(screen.getByRole("button", { name: "Create rule" }));
  expect(await screen.findByText("Enter a number of days above 0 and at most 3650.")).toBeInTheDocument();
  fireEvent.change(screen.getByLabelText("Threshold (days)"), { target: { value: "14" } });
  fireEvent.click(screen.getByRole("button", { name: "Create rule" }));
  await waitFor(() => expect(request).toHaveBeenCalledWith("POST", "/api/notifications/rules", { name: "Cert expiring", node_id: "", workload_id: "w1", domain: "b.example.test", metric: "certificate", threshold: 14, confirmation_seconds: 300, repeat_seconds: 3600, channel_ids: ["c3"] }));
});

it("requires a service and blocks rules on workers without observation support", async () => {
  renderRules([]);
  fireEvent.click(await screen.findByRole("button", { name: "New rule" }));
  choose("Metric", "Container restarts");
  expect(screen.queryByRole("combobox", { name: "Domain" })).not.toBeInTheDocument();
  fireEvent.change(screen.getByLabelText("Name"), { target: { value: "Restart loop" } });
  fireEvent.change(screen.getByLabelText("Threshold (events)"), { target: { value: "3" } });
  fireEvent.click(screen.getByRole("checkbox", { name: /Ops chat/ }));
  fireEvent.click(screen.getByRole("button", { name: "Create rule" }));
  expect(await screen.findByText("Select a service.")).toBeInTheDocument();
  choose("Service", "legacy");
  expect(screen.getByText("Upgrade this service's worker first")).toBeInTheDocument();
  expect(screen.getByRole("button", { name: "Create rule" })).toBeDisabled();
  choose("Service", "api");
  expect(screen.queryByText("Upgrade this service's worker first")).not.toBeInTheDocument();
  fireEvent.click(screen.getByRole("button", { name: "Create rule" }));
  await waitFor(() => expect(request).toHaveBeenCalledWith("POST", "/api/notifications/rules", expect.objectContaining({ metric: "restarts", workload_id: "w1", domain: "", node_id: "", threshold: 3 })));
});

it("lists resource rules with their service, route and unit", async () => {
  renderRules([
    { ...rule, id: "r2", name: "Cert", node_id: "n1", workload_id: "w1", domain: "a.example.test", metric: "certificate", threshold: 14 },
    { ...rule, id: "r3", name: "Crashing", node_id: "n1", workload_id: "w1", metric: "crashes", threshold: 2 },
    { ...rule, id: "r4", name: "Gone", node_id: "n1", workload_id: "deleted", metric: "oom", threshold: 1 },
  ]);
  const list = await screen.findByRole("list", { name: "Threshold rules" });
  expect(within(list).getByText("Certificate expiry for a.example.test at or below 14 days for 5 minutes on api (Shop / production) · repeats at most every 1 hour")).toBeInTheDocument();
  expect(within(list).getByText("Container crashes at or above 2 in ten minutes for 5 minutes on api (Shop / production) · repeats at most every 1 hour")).toBeInTheDocument();
  expect(within(list).getByText(/on an unavailable service/)).toBeInTheDocument();
  expect(within(list).getByText(/service is no longer available to this team/)).toBeInTheDocument();
  expect(within(list).queryByText(/%/)).not.toBeInTheDocument();
});

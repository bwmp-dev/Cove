import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { ApiError, request } from "../api/client";
import { LogForwarders } from "./LogForwarders";

const session = vi.hoisted((): { current: unknown } => ({ current: null }));
vi.mock("../api/client", async (importActual) => ({ ...(await importActual<typeof import("../api/client")>()), request: vi.fn() }));
vi.mock("../lib/session", () => ({ useSession: () => session.current }));

const owner = { is_admin: false, team_id: "t1", teams: [{ id: "t1", name: "Team", role: "owner", grants: [] }], permissions: ["view", "read_secrets", "operate", "edit_configuration", "manage_backups", "manage_members"] };
const saved = { id: "f1", name: "Grafana", kind: "loki", enabled: true, endpoint_set: true, token_set: true, username_set: true, last_success: "2026-11-22T11:58:00Z", last_error: "", sent_entries: 1200, dropped_entries: 0, pending_batches: 1 };

beforeEach(() => {
  session.current = owner;
  vi.stubGlobal("ResizeObserver", class { observe = vi.fn(); disconnect = vi.fn(); });
  HTMLDialogElement.prototype.showModal = vi.fn(function (this: HTMLDialogElement) { this.open = true; });
  HTMLDialogElement.prototype.close = vi.fn(function (this: HTMLDialogElement) { this.open = false; });
});
afterEach(() => { vi.resetAllMocks(); vi.unstubAllGlobals(); });

function renderForwarders(list: unknown[]) {
  vi.mocked(request).mockImplementation((method) => (method === "GET" ? Promise.resolve(list) : Promise.resolve({ id: "f2", ok: true })));
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(<QueryClientProvider client={client}><LogForwarders /></QueryClientProvider>);
  return client;
}

function choose(label: string, option: string) {
  fireEvent.click(screen.getByRole("combobox", { name: label }));
  fireEvent.click(screen.getByRole("option", { name: option }));
}

it("creates a Datadog forwarder and never keeps the key in the query cache", async () => {
  const client = renderForwarders([]);
  expect(await screen.findByText(/No log forwarders/)).toBeInTheDocument();
  fireEvent.click(screen.getByRole("button", { name: "New forwarder" }));
  choose("Receiver", "Datadog");
  expect(screen.queryByLabelText(/Username/)).not.toBeInTheDocument();
  fireEvent.change(screen.getByLabelText("Name"), { target: { value: "DD" } });
  fireEvent.change(screen.getByLabelText("Logs intake URL"), { target: { value: "https://http-intake.logs.example.test/api/v2/logs" } });
  fireEvent.click(screen.getByRole("button", { name: "Create forwarder" }));
  expect(await screen.findByText("Enter the API key.")).toBeInTheDocument();
  fireEvent.change(screen.getByLabelText("API key"), { target: { value: "secret-key" } });
  fireEvent.click(screen.getByRole("button", { name: "Create forwarder" }));
  await waitFor(() => expect(request).toHaveBeenCalledWith("POST", "/api/notifications/forwarders", { name: "DD", kind: "datadog", endpoint: "https://http-intake.logs.example.test/api/v2/logs", token: "secret-key", username: "", enabled: true }));
  await waitFor(() => expect(screen.queryByRole("button", { name: "Create forwarder" })).not.toBeInTheDocument());
  expect(JSON.stringify(client.getMutationCache().getAll().map((mutation) => mutation.state.variables))).not.toContain("secret-key");
});

it("edits with hidden saved values, sending blanks to keep them, and requires new ones after a kind change", async () => {
  renderForwarders([saved]);
  fireEvent.click(await screen.findByRole("button", { name: "Edit Grafana" }));
  const endpoint = screen.getByLabelText(/Push URL/);
  expect(endpoint).toHaveValue("");
  expect(screen.getByText(/saved push url is hidden/i)).toBeInTheDocument();
  expect(screen.getByLabelText(/Token or password/)).toHaveValue("");
  choose("Receiver", "Syslog over TLS");
  expect(screen.getByText(/needs a new endpoint and credentials/)).toBeInTheDocument();
  fireEvent.click(screen.getByRole("button", { name: "Save forwarder" }));
  expect(await screen.findByText("Enter the receiver address.")).toBeInTheDocument();
  fireEvent.change(screen.getByLabelText(/Receiver address/), { target: { value: "tls://syslog.example.test" } });
  fireEvent.click(screen.getByRole("button", { name: "Save forwarder" }));
  expect(await screen.findByText(/port from 1 to 65535/)).toBeInTheDocument();
  choose("Receiver", "Grafana Loki");
  fireEvent.click(screen.getByRole("button", { name: "Save forwarder" }));
  await waitFor(() => expect(request).toHaveBeenCalledWith("PUT", "/api/notifications/forwarders/f1", { name: "Grafana", kind: "loki", endpoint: "", token: "", username: "", enabled: true }));
});

it("shows delivery counters and test results without exposing configuration", async () => {
  renderForwarders([{ ...saved, last_error: "receiver returned 401", dropped_entries: 12 }]);
  const row = (await screen.findByText("Grafana")).closest("li") as HTMLElement;
  expect(within(row).getByText("Failing")).toBeInTheDocument();
  expect(within(row).getByText(/1200 sent · 1 batch queued/)).toBeInTheDocument();
  expect(within(row).getByText(/12 dropped/)).toBeInTheDocument();
  vi.mocked(request).mockImplementation((method) => (method === "GET" ? Promise.resolve([saved]) : Promise.reject(new ApiError(502, "Log forwarding test failed; verify the endpoint, credentials and public TLS reachability"))));
  fireEvent.click(within(row).getByRole("button", { name: "Send test to Grafana" }));
  expect(await screen.findByText(/Log forwarding test failed/)).toBeInTheDocument();
  expect(request).toHaveBeenCalledWith("POST", "/api/notifications/forwarders/f1/test");
});

it("does not load forwarders for team members", () => {
  session.current = { ...owner, teams: [{ id: "t1", name: "Team", role: "member", grants: [] }] };
  renderForwarders([]);
  expect(screen.getByText("Team owners and admins manage log forwarding")).toBeInTheDocument();
  expect(request).not.toHaveBeenCalled();
});

it.each([
  ["Edit configuration without Read secrets", ["view", "operate", "edit_configuration"]],
  ["Read secrets without Edit configuration", ["view", "read_secrets"]],
])("does not load forwarders for a team admin with %s", (_label, permissions) => {
  session.current = { ...owner, teams: [{ id: "t1", name: "Team", role: "admin", grants: [] }], permissions };
  renderForwarders([]);
  expect(screen.getByText("Team owners and admins manage log forwarding")).toBeInTheDocument();
  expect(request).not.toHaveBeenCalled();
});

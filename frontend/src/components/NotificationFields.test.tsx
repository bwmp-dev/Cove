import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { request } from "../api/client";
import { Connections } from "./Connections";

vi.mock("../api/client", () => ({ request: vi.fn(), errorMessage: (error: Error) => error.message }));
vi.mock("../lib/session", () => ({ useSession: () => ({ is_admin: true, team_id: "one", teams: [{ id: "one", name: "Team", role: "owner", grants: [] }] }) }));
vi.mock("@tanstack/react-router", () => ({ useBlocker: () => ({ status: "idle" }) }));
beforeEach(() => vi.stubGlobal("ResizeObserver", class { observe = vi.fn(); disconnect = vi.fn(); }));
afterEach(() => { vi.clearAllMocks(); vi.unstubAllGlobals(); });

const signed = { id: "n1", name: "Ops hook", notification: { provider: "webhook", url: "", enabled: true, success: false, failure: false, health: false, events: ["backup.failed"], signing_secret_set: true } };

function renderPage(channels: unknown[]) {
  vi.mocked(request).mockImplementation((method, path) => Promise.resolve(path === "/api/notifications/events" ? ["backup.succeeded", "backup.failed", "disk_low.alert"] : method === "GET" ? channels : { id: "n1" }));
  render(<QueryClientProvider client={new QueryClient()}><Connections kind="notifications" /></QueryClientProvider>);
}

it("edits exact events and keeps the hidden URL and signing secret", async () => {
  renderPage([signed]);
  expect(await screen.findByText("JSON webhook · Enabled · 1 selected event · Signed")).toBeInTheDocument();
  fireEvent.click(screen.getByRole("button", { name: "Edit" }));
  expect(screen.getByText("A signing secret is saved and hidden. Leave this blank to keep it, or enter a new one to replace it.")).toBeInTheDocument();
  expect(screen.getByLabelText(/Webhook URL/)).not.toBeRequired();
  expect(await screen.findByRole("checkbox", { name: "Failed" })).toBeChecked();
  fireEvent.click(screen.getByRole("checkbox", { name: "Low disk space: alert" }));
  fireEvent.click(screen.getByRole("button", { name: "Save" }));
  await screen.findByRole("button", { name: "Add notification" });
  expect(request).toHaveBeenCalledWith("PUT", "/api/notifications/n1", { name: "Ops hook", notification: { provider: "webhook", url: "", enabled: true, success: false, failure: false, health: false, events: ["backup.failed", "disk_low.alert"] } });
});

it("adds an email channel and shows API errors", async () => {
  renderPage([]);
  fireEvent.click(await screen.findByRole("button", { name: "Add notification" }));
  fireEvent.click(screen.getByRole("combobox", { name: "Provider" }));
  fireEvent.click(screen.getByRole("option", { name: "Email" }));
  expect(screen.queryByLabelText(/Webhook URL/)).not.toBeInTheDocument();
  expect(screen.queryByLabelText(/Signing secret/)).not.toBeInTheDocument();
  fireEvent.change(screen.getByLabelText("Name"), { target: { value: "On call" } });
  fireEvent.change(screen.getByLabelText("Recipient email"), { target: { value: "ops@example.com" } });
  vi.mocked(request).mockImplementationOnce(() => Promise.reject(new Error("a valid recipient email is required")));
  fireEvent.click(screen.getByRole("button", { name: "Save" }));
  expect(await screen.findByText("a valid recipient email is required")).toBeInTheDocument();
  expect(request).toHaveBeenCalledWith("POST", "/api/notifications", { name: "On call", notification: { provider: "email", url: "", email: "ops@example.com", enabled: true, success: false, failure: true, health: false, events: [] } });
});

const catalogue = ["backup.failed", "node_unreachable.alert", "node_unreachable.recovered", "disk_low.alert", "disk_low.recovered"];

function renderWithCatalogue(channels: unknown[]) {
  vi.mocked(request).mockImplementation((method, path) => Promise.resolve(path === "/api/notifications/events" ? catalogue : method === "GET" ? channels : { id: "n1" }));
  render(<QueryClientProvider client={new QueryClient()}><Connections kind="notifications" /></QueryClientProvider>);
}

function chooseProvider(label: string) {
  fireEvent.click(screen.getByRole("combobox", { name: "Provider" }));
  fireEvent.click(screen.getByRole("option", { name: label }));
}

it("edits a Pushover channel without resending saved credentials", async () => {
  renderWithCatalogue([{ id: "p1", name: "Phones", notification: { provider: "pushover", url: "", enabled: true, success: false, failure: true, health: false, token_set: true, user_key_set: true } }]);
  expect(await screen.findByText("Pushover · Enabled · Failed operations")).toBeInTheDocument();
  fireEvent.click(screen.getByRole("button", { name: "Edit" }));
  expect(screen.queryByLabelText(/Webhook URL/)).not.toBeInTheDocument();
  expect(screen.getByLabelText(/Application token/)).toHaveValue("");
  expect(screen.getByLabelText(/Application token/)).not.toBeRequired();
  expect(screen.getByText("The saved application token is hidden. Leave this blank to keep it, or enter a new one to replace it.")).toBeInTheDocument();
  expect(screen.getByLabelText(/User key/)).not.toBeRequired();
  fireEvent.click(screen.getByRole("button", { name: "Save" }));
  await screen.findByRole("button", { name: "Add notification" });
  expect(request).toHaveBeenCalledWith("PUT", "/api/notifications/p1", { name: "Phones", notification: { provider: "pushover", url: "", enabled: true, success: false, failure: true, health: false, events: [] } });
});

it("shows field errors for Matrix and prefills the saved room", async () => {
  renderWithCatalogue([{ id: "m1", name: "Ops room", notification: { provider: "matrix", url: "", enabled: true, success: false, failure: true, health: false, token_set: true, target: "!ops:example.org" } }]);
  expect(await screen.findByText("Matrix · Enabled · Failed operations · !ops:example.org")).toBeInTheDocument();
  fireEvent.click(screen.getByRole("button", { name: "Edit" }));
  expect(screen.getByLabelText("Room ID")).toHaveValue("!ops:example.org");
  fireEvent.change(screen.getByLabelText(/Homeserver URL/), { target: { value: "https://matrix.example.org/?a=b" } });
  fireEvent.click(screen.getByRole("button", { name: "Save" }));
  expect(await screen.findByText("Enter the base URL without a query string.")).toBeInTheDocument();
  expect(screen.getByLabelText(/Homeserver URL/)).toHaveAttribute("aria-invalid", "true");
  expect(screen.getByLabelText(/Homeserver URL/)).toHaveFocus();
  expect(request).not.toHaveBeenCalledWith("PUT", expect.anything(), expect.anything());
});

it("adds a PagerDuty channel with paired health events and warns before testing", async () => {
  renderWithCatalogue([]);
  fireEvent.click(await screen.findByRole("button", { name: "Add notification" }));
  chooseProvider("PagerDuty");
  expect(screen.queryByRole("radio", { name: /By result/ })).not.toBeInTheDocument();
  fireEvent.change(screen.getByLabelText("Name"), { target: { value: "On call" } });
  fireEvent.change(screen.getByLabelText("Integration key"), { target: { value: "routing-key" } });
  fireEvent.click(await screen.findByRole("checkbox", { name: "Low disk space" }));
  fireEvent.click(screen.getByRole("button", { name: "Save" }));
  await screen.findByRole("button", { name: "Add notification" });
  expect(request).toHaveBeenCalledWith("POST", "/api/notifications", { name: "On call", notification: { provider: "pagerduty", url: "", token: "routing-key", enabled: true, success: false, failure: false, health: false, events: ["disk_low.alert", "disk_low.recovered"] } });
});

it("explains that testing an incident channel opens an incident", async () => {
  renderWithCatalogue([{ id: "o1", name: "Genie", notification: { provider: "opsgenie", url: "", enabled: true, success: false, failure: false, health: false, token_set: true, events: ["node_unreachable.alert", "node_unreachable.recovered"] } }]);
  expect(await screen.findByText("Opsgenie · Enabled · 1 health condition")).toBeInTheDocument();
  fireEvent.click(screen.getByRole("button", { name: "Test" }));
  expect(screen.getByText("Testing creates a real Opsgenie incident")).toBeInTheDocument();
  expect(screen.getByText(/resolve or close it in Opsgenie afterwards/)).toBeInTheDocument();
});

it("saves an email channel for the resource owner and explains its test", async () => {
  renderWithCatalogue([{ id: "e1", name: "Owners", notification: { provider: "email", url: "", email: "old@example.com", enabled: true, success: false, failure: true, health: false } }]);
  expect(await screen.findByText("Email · old@example.com · Enabled · Failed operations")).toBeInTheDocument();
  fireEvent.click(screen.getByRole("button", { name: "Edit" }));
  expect(screen.getByRole("radio", { name: /Fixed address/ })).toBeChecked();
  fireEvent.click(screen.getByRole("radio", { name: /Resource owner/ }));
  expect(screen.queryByLabelText("Recipient email")).not.toBeInTheDocument();
  expect(screen.getByText(/for node events, which have no resource owner/)).toBeInTheDocument();
  fireEvent.click(screen.getByRole("button", { name: "Save" }));
  await screen.findByRole("button", { name: "Add notification" });
  expect(request).toHaveBeenCalledWith("PUT", "/api/notifications/e1", { name: "Owners", notification: { provider: "email", url: "", email: "", owner: true, enabled: true, success: false, failure: true, health: false, events: [] } });
});

it("tells the tester that an owner channel test goes to their own address", async () => {
  renderWithCatalogue([{ id: "e2", name: "Owners", notification: { provider: "email", url: "", email: "", owner: true, enabled: true, success: false, failure: true, health: false } }]);
  expect(await screen.findByText("Email · Resource owner · Enabled · Failed operations")).toBeInTheDocument();
  fireEvent.click(screen.getByRole("button", { name: "Test" }));
  expect(screen.getByText("The test goes to your own email address")).toBeInTheDocument();
  vi.mocked(request).mockImplementationOnce(() => Promise.reject(new Error("your account has no verified email address")));
  fireEvent.click(screen.getByRole("button", { name: "Run test" }));
  expect(await screen.findByText("your account has no verified email address")).toBeInTheDocument();
  expect(request).toHaveBeenCalledWith("POST", "/api/notifications/e2/test", {});
});

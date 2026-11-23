import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { ApiError, request } from "../api/client";
import type { Workload } from "../api/types";
import { UptimeChecks } from "./UptimeChecks";

vi.mock("../api/client", async (importActual) => ({ ...(await importActual<typeof import("../api/client")>()), request: vi.fn() }));

beforeEach(() => {
  vi.useFakeTimers({ toFake: ["Date"], now: Date.parse("2026-11-22T12:00:00Z") });
  vi.stubGlobal("ResizeObserver", class { observe = vi.fn(); disconnect = vi.fn(); });
  HTMLDialogElement.prototype.showModal = vi.fn(function (this: HTMLDialogElement) { this.open = true; });
  HTMLDialogElement.prototype.close = vi.fn(function (this: HTMLDialogElement) { this.open = false; });
});
afterEach(() => { vi.useRealTimers(); vi.resetAllMocks(); vi.unstubAllGlobals(); });

const routes = [{ domain: "api.example.test", service: "web", port: 80 }, { domain: "www.example.test", service: "web", port: 80 }];
const editor = { id: "w1", name: "shop", routes, permissions: ["read", "operate", "edit"] } as unknown as Workload;
const check = { id: "u1", domain: "api.example.test", path: "/healthz", enabled: true, interval_seconds: 60, timeout_seconds: 5, expected_min: 200, expected_max: 399, public_label: "API", checked_at: "2026-11-22T11:50:00Z", up: true, status: 200, latency_ms: 42, error: "", consecutive_failures: 0 };

function renderChecks(workload: Workload, checks: unknown[]) {
  vi.mocked(request).mockImplementation((method) => (method === "GET" ? Promise.resolve(checks) : Promise.resolve({ id: "u2" })));
  render(<QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}><UptimeChecks workload={workload} /></QueryClientProvider>);
}

it("treats an old result as unknown instead of up", async () => {
  renderChecks(editor, [check]);
  const row = (await screen.findByText("api.example.test")).closest("li") as HTMLElement;
  expect(within(row).getByText("Unknown (stale)")).toBeInTheDocument();
  expect(within(row).queryByText("Up")).not.toBeInTheDocument();
  expect(within(row).getByText(/Public as “API”/)).toBeInTheDocument();
});

it("creates a check for a domain that has none and validates the path first", async () => {
  renderChecks(editor, [check]);
  fireEvent.click(await screen.findByRole("button", { name: "New check" }));
  fireEvent.click(screen.getByRole("combobox", { name: "Domain" }));
  expect(screen.getAllByRole("option").map((option) => option.textContent)).toEqual(["www.example.test"]);
  fireEvent.click(screen.getByRole("option", { name: "www.example.test" }));
  fireEvent.change(screen.getByLabelText("Path"), { target: { value: "/status?full=1" } });
  fireEvent.click(screen.getByRole("button", { name: "Create check" }));
  expect(await screen.findByText(/without spaces, a query or a fragment/)).toBeInTheDocument();
  expect(request).not.toHaveBeenCalledWith("POST", expect.anything(), expect.anything());
  fireEvent.change(screen.getByLabelText("Path"), { target: { value: "/status" } });
  fireEvent.change(screen.getByLabelText(/Public label/), { target: { value: "Website" } });
  fireEvent.click(screen.getByRole("button", { name: "Create check" }));
  await waitFor(() => expect(request).toHaveBeenCalledWith("POST", "/api/workloads/w1/uptime", { domain: "www.example.test", path: "/status", enabled: true, interval_seconds: 300, timeout_seconds: 5, expected_min: 200, expected_max: 399, public_label: "Website" }));
});

it("shows the schedule budget conflict from the API", async () => {
  renderChecks(editor, []);
  vi.mocked(request).mockImplementation((method) => (method === "GET" ? Promise.resolve([]) : Promise.reject(new ApiError(409, "Uptime check schedule budget exceeded"))));
  fireEvent.click(await screen.findByRole("button", { name: "New check" }));
  fireEvent.click(screen.getByRole("button", { name: "Create check" }));
  expect(await screen.findByText("Uptime check schedule budget exceeded")).toBeInTheDocument();
});

it("edits with PUT and deletes after confirmation", async () => {
  renderChecks(editor, [{ ...check, checked_at: "2026-11-22T11:59:30Z", up: false, status: 503, error: "unexpected status 503", consecutive_failures: 3 }]);
  const row = (await screen.findByText("api.example.test")).closest("li") as HTMLElement;
  expect(within(row).getByText("Down")).toBeInTheDocument();
  expect(within(row).getByText(/3 consecutive failures/)).toBeInTheDocument();
  fireEvent.click(within(row).getByRole("button", { name: "Edit check for api.example.test" }));
  expect(screen.getByLabelText("Domain")).toHaveAttribute("readonly");
  expect(screen.queryByRole("combobox", { name: "Domain" })).not.toBeInTheDocument();
  fireEvent.change(screen.getByLabelText("Interval (seconds)"), { target: { value: "120" } });
  expect(screen.queryByText("This clears the check's history")).not.toBeInTheDocument();
  fireEvent.change(screen.getByLabelText("Path"), { target: { value: "/ready" } });
  expect(screen.getByText("This clears the check's history")).toBeInTheDocument();
  fireEvent.change(screen.getByLabelText("Path"), { target: { value: "/healthz" } });
  expect(screen.queryByText("This clears the check's history")).not.toBeInTheDocument();
  fireEvent.change(screen.getByLabelText("Highest healthy status"), { target: { value: "299" } });
  expect(screen.getByText("This clears the check's history")).toBeInTheDocument();
  fireEvent.click(screen.getByRole("button", { name: "Save check" }));
  await waitFor(() => expect(request).toHaveBeenCalledWith("PUT", "/api/workloads/w1/uptime/u1", expect.objectContaining({ domain: "api.example.test", path: "/healthz", interval_seconds: 120, expected_max: 299 })));
  fireEvent.click(await screen.findByRole("button", { name: "Delete check for api.example.test" }));
  fireEvent.click(within(screen.getByRole("dialog")).getByRole("button", { name: "Delete check" }));
  await waitFor(() => expect(request).toHaveBeenCalledWith("DELETE", "/api/workloads/w1/uptime/u1"));
});

it("is read-only for viewers", async () => {
  renderChecks({ ...editor, permissions: ["read"] } as unknown as Workload, [check]);
  expect(await screen.findByText("api.example.test")).toBeInTheDocument();
  expect(screen.queryByRole("button", { name: "New check" })).not.toBeInTheDocument();
  expect(screen.queryByRole("button", { name: /Edit/ })).not.toBeInTheDocument();
});

it("checks one concrete hostname under a wildcard domain and never sends the wildcard", async () => {
  const wildcard = { ...editor, routes: [...routes, { domain: "*.shops.example.test", service: "web", port: 80 }] } as Workload;
  const shop = { ...check, id: "u2", domain: "acme.shops.example.test", public_label: "" };
  renderChecks(wildcard, [check, shop]);
  // A check under the wildcard is still routed, so it is editable and not reported as unconfigured.
  const row = (await screen.findByText("acme.shops.example.test")).closest("li") as HTMLElement;
  expect(within(row).queryByText(/no longer configured/)).not.toBeInTheDocument();
  expect(within(row).getByRole("button", { name: "Edit check for acme.shops.example.test" })).toBeEnabled();

  fireEvent.click(screen.getByRole("button", { name: "New check" }));
  fireEvent.click(screen.getByRole("combobox", { name: "Domain" }));
  expect(screen.getAllByRole("option").map((option) => option.textContent)).toEqual(["*.shops.example.test", "www.example.test"]);
  fireEvent.click(screen.getByRole("option", { name: "www.example.test" }));
  expect(screen.queryByLabelText("Hostname to check")).not.toBeInTheDocument();
  fireEvent.click(screen.getByRole("combobox", { name: "Domain" }));
  fireEvent.click(screen.getByRole("option", { name: "*.shops.example.test" }));
  const host = screen.getByLabelText("Hostname to check");
  expect(host).toHaveAccessibleDescription(/The wildcard itself is never requested/);
  for (const value of ["", "a.b", "*", "-bad"]) {
    fireEvent.change(host, { target: { value } });
    fireEvent.click(screen.getByRole("button", { name: "Create check" }));
    expect(await screen.findByText("Enter one DNS label, such as shop, to check under *.shops.example.test.")).toBeInTheDocument();
  }
  expect(request).not.toHaveBeenCalledWith("POST", expect.anything(), expect.anything());
  fireEvent.change(host, { target: { value: "Beta" } });
  fireEvent.click(screen.getByRole("button", { name: "Create check" }));
  await waitFor(() => expect(request).toHaveBeenCalledWith("POST", "/api/workloads/w1/uptime", expect.objectContaining({ domain: "beta.shops.example.test", path: "/" })));
  expect(vi.mocked(request).mock.calls.filter(([method]) => method === "POST").map(([, , body]) => (body as { domain: string }).domain)).toEqual(["beta.shops.example.test"]);
});

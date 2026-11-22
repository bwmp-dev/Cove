import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { ApiError, request } from "../api/client";
import { DeploymentPolicy } from "./DeploymentPolicy";

const team = vi.hoisted(() => ({ current: { id: "t1", name: "Team", role: "owner", grants: [] } }));
vi.mock("../api/client", async (original) => ({ ...(await original<typeof import("../api/client")>()), request: vi.fn() }));
vi.mock("../lib/session", () => ({ useCurrentTeam: () => team.current }));
vi.mock("@tanstack/react-router", () => ({ useBlocker: () => ({ status: "idle" }) }));

beforeEach(() => vi.stubGlobal("ResizeObserver", class { observe = vi.fn(); disconnect = vi.fn(); }));
afterEach(() => { vi.clearAllMocks(); vi.unstubAllGlobals(); team.current.role = "owner"; });

const saved = {
  approval: false,
  freeze_from: "2026-12-24T00:00:00Z",
  freeze_until: "2026-12-27T00:00:00Z",
  window_start: "08:00",
  window_end: "18:00",
  timezone: "UTC",
  recurring_windows: [],
  freezes: [],
  outside_window: "block",
};

function mockApi(put: (body: unknown) => Promise<unknown> = (body) => Promise.resolve(body)) {
  vi.mocked(request).mockImplementation((method, path, body) => {
    if (path === "/api/teams/t1/deployment-policy") return method === "PUT" ? put(body) : Promise.resolve(saved);
    return Promise.reject(new Error(`unexpected ${method} ${path}`));
  });
}

async function openPolicy() {
  render(<QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}><DeploymentPolicy scope="team" id="t1" /></QueryClientProvider>);
  expect(await screen.findByText(/Daily 08:00–18:00 UTC/)).toBeInTheDocument();
  fireEvent.click(screen.getByRole("button", { name: "Deployment policy" }));
}

it("saves weekly windows in the chosen time zone and keeps the existing freeze and daily times", async () => {
  mockApi();
  await openPolicy();
  fireEvent.change(screen.getByLabelText("Time zone"), { target: { value: "Europe/Berlin" } });
  fireEvent.click(screen.getByRole("button", { name: "Add window" }));
  const windows = screen.getByRole("list", { name: "Weekly windows" });
  fireEvent.click(within(windows).getByRole("checkbox", { name: "Friday" }));
  fireEvent.change(within(windows).getByLabelText("Window 1 ends"), { target: { value: "15:00" } });
  expect(screen.getByText("Unsaved changes")).toBeInTheDocument();
  fireEvent.click(screen.getByRole("button", { name: "Save deployment policy" }));
  await waitFor(() => expect(request).toHaveBeenCalledWith("PUT", "/api/teams/t1/deployment-policy", {
    ...saved,
    timezone: "Europe/Berlin",
    recurring_windows: [{ days: [1, 2, 3, 4], start: "09:00", end: "15:00" }],
  }));
  expect(await screen.findByText("Deployment policy saved.")).toBeInTheDocument();
});

it("sends one-off freezes as explicit UTC instants with their reason", async () => {
  mockApi();
  await openPolicy();
  fireEvent.click(screen.getByRole("button", { name: "Add freeze" }));
  const freezes = screen.getByRole("list", { name: "Freezes" });
  fireEvent.change(within(freezes).getByLabelText("Freeze 1 starts (UTC)"), { target: { value: "2027-01-10T22:00" } });
  fireEvent.change(within(freezes).getByLabelText("Freeze 1 ends (UTC)"), { target: { value: "2027-01-11T06:00" } });
  fireEvent.change(within(freezes).getByLabelText(/Reason/), { target: { value: "Data centre move" } });
  fireEvent.click(screen.getByRole("button", { name: "Save deployment policy" }));
  await waitFor(() => expect(request).toHaveBeenCalledWith("PUT", "/api/teams/t1/deployment-policy", expect.objectContaining({
    freeze_from: "2026-12-24T00:00:00Z",
    freezes: [{ from: "2027-01-10T22:00:00Z", until: "2027-01-11T06:00:00Z", reason: "Data centre move" }],
  })));
});

it("shows validation errors beside the fields and does not save", async () => {
  mockApi();
  await openPolicy();
  fireEvent.change(screen.getByLabelText("Time zone"), { target: { value: "Mars/Olympus" } });
  fireEvent.click(screen.getByRole("button", { name: "Add window" }));
  const windows = screen.getByRole("list", { name: "Weekly windows" });
  for (const day of ["Monday", "Tuesday", "Wednesday", "Thursday", "Friday"]) fireEvent.click(within(windows).getByRole("checkbox", { name: day }));
  fireEvent.click(screen.getByRole("button", { name: "Add freeze" }));
  fireEvent.change(screen.getByLabelText("Freeze 1 starts (UTC)"), { target: { value: "2027-01-11T06:00" } });
  fireEvent.change(screen.getByLabelText("Freeze 1 ends (UTC)"), { target: { value: "2027-01-10T06:00" } });
  fireEvent.click(screen.getByRole("button", { name: "Save deployment policy" }));
  expect(await screen.findByText("Enter a valid IANA time zone, such as UTC or America/New_York.")).toBeInTheDocument();
  expect(screen.getByText("Choose at least one day.")).toBeInTheDocument();
  expect(screen.getByText("The end must be after the start.")).toBeInTheDocument();
  expect(screen.getByText("Fix the highlighted fields")).toBeInTheDocument();
  expect(request).not.toHaveBeenCalledWith("PUT", expect.anything(), expect.anything());
});

it("keeps unsaved edits when the save is rejected and shows the reason", async () => {
  mockApi(() => Promise.reject(new ApiError(400, "valid IANA timezone required")));
  await openPolicy();
  fireEvent.click(screen.getByRole("switch", { name: "Require approval for deployments and rollbacks" }));
  fireEvent.click(screen.getByRole("button", { name: "Save deployment policy" }));
  expect(await screen.findByText("valid IANA timezone required")).toBeInTheDocument();
  expect(screen.getByRole("switch", { name: "Require approval for deployments and rollbacks" })).toBeChecked();
  expect(screen.getByText("Unsaved changes")).toBeInTheDocument();
});

it("is read-only for people who are not team owners or admins", async () => {
  team.current.role = "member";
  mockApi();
  await openPolicy();
  expect(screen.getByText("Only team owners and admins can change this policy.")).toBeInTheDocument();
  expect(screen.getByLabelText("Time zone")).toBeDisabled();
  expect(screen.queryByRole("button", { name: "Save deployment policy" })).not.toBeInTheDocument();
  expect(screen.queryByRole("button", { name: "Add window" })).not.toBeInTheDocument();
});

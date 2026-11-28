import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, fireEvent, render, screen, waitFor } from "@testing-library/react";
import type { ReactNode } from "react";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { request } from "../api/client";
import { ResourceDefaultsNote, ResourceDefaultsSection } from "./ResourceDefaults";

vi.mock("../api/client", () => ({ request: vi.fn(), errorMessage: (error: Error) => error.message, ApiError: class extends Error {} }));
vi.mock("@tanstack/react-router", () => ({ useBlocker: () => ({ status: "idle" }) }));
beforeEach(() => vi.stubGlobal("ResizeObserver", class { observe = vi.fn(); disconnect = vi.fn(); }));
afterEach(() => {
  vi.resetAllMocks();
  vi.unstubAllGlobals();
});

const node = (name: string, resourceDefaults: boolean) => ({ id: name, name, state: "ready", capabilities: { resource_defaults: resourceDefaults } });

const member = { user: "u1", user_id: "u1", email: "a@example.test", name: "A", is_admin: false, has_password: true, github_linked: false, team_id: "t1", teams: [{ id: "t1", name: "Team", role: "owner", grants: [] }] };

function mockApi({ configured = { cpus: 0, memory_mb: 0 }, team = { cpus: 2, memory_mb: 512 }, nodes = [node("alpha", true)], session = member, assigned = [] as string[] } = {}) {
  vi.mocked(request).mockImplementation((method: string, path: string, body?: unknown) => {
    if (path === "/api/nodes") return Promise.resolve(nodes);
    if (path === "/api/session") return Promise.resolve(session);
    if (path === "/api/teams/t1/nodes") return Promise.resolve(assigned);
    if (method === "PUT") {
      const saved = body as { cpus: number; memory_mb: number };
      const teamValues = path === "/api/resource-defaults" ? saved : team;
      return Promise.resolve({ configured: saved, team: teamValues, effective: { cpus: saved.cpus || teamValues.cpus, memory_mb: saved.memory_mb || teamValues.memory_mb } });
    }
    const local = path === "/api/resource-defaults" ? team : configured;
    return Promise.resolve({ configured: local, team, effective: { cpus: local.cpus || team.cpus, memory_mb: local.memory_mb || team.memory_mb } });
  });
}

function wrap(children: ReactNode) {
  return render(<QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}>{children}</QueryClientProvider>);
}

function puts() {
  return vi.mocked(request).mock.calls.filter(([method]) => method === "PUT");
}

it("explains inherited team values and saves environment overrides", async () => {
  mockApi();
  wrap(<ResourceDefaultsSection target={{ scope: "environment", id: "env-1" }} canEdit />);
  const cpu = await screen.findByLabelText(/CPU cores/);
  expect(cpu).toHaveValue(null);
  expect(screen.getByText("Blank inherits the team default of 2 cores.")).toBeInTheDocument();
  expect(screen.getByText("Blank inherits the team default of 512 MiB.")).toBeInTheDocument();
  expect(screen.getByText("2 cores CPU, 512 MiB memory")).toBeInTheDocument();

  fireEvent.change(cpu, { target: { value: "0.5" } });
  expect(screen.getByText("Unsaved changes")).toBeInTheDocument();
  expect(screen.getByText("0.5 cores CPU, 512 MiB memory")).toBeInTheDocument();
  fireEvent.click(screen.getByRole("button", { name: "Save changes" }));
  await waitFor(() => expect(request).toHaveBeenCalledWith("PUT", "/api/environments/env-1/resource-defaults", { cpus: 0.5, memory_mb: 0 }));
  expect(await screen.findByText("Saved. Applies at each resource's next deploy.")).toBeInTheDocument();
});

it("validates each field next to it without saving", async () => {
  mockApi();
  wrap(<ResourceDefaultsSection target={{ scope: "team" }} canEdit />);
  const memory = await screen.findByLabelText(/Memory \(MiB\)/);
  fireEvent.change(memory, { target: { value: "4" } });
  fireEvent.click(screen.getByRole("button", { name: "Save changes" }));
  expect(await screen.findByText("Enter whole MiB from 6 to 16,777,216, or leave blank.")).toBeInTheDocument();
  expect(memory).toHaveAttribute("aria-invalid", "true");
  expect(memory).toHaveFocus();
  expect(puts()).toHaveLength(0);
});

it("clears a team default by saving blank fields", async () => {
  mockApi();
  wrap(<ResourceDefaultsSection target={{ scope: "team" }} canEdit />);
  const cpu = await screen.findByLabelText(/CPU cores/);
  expect(cpu).toHaveValue(2);
  fireEvent.change(cpu, { target: { value: "" } });
  fireEvent.change(screen.getByLabelText(/Memory \(MiB\)/), { target: { value: "" } });
  fireEvent.click(screen.getByRole("button", { name: "Save changes" }));
  await waitFor(() => expect(request).toHaveBeenCalledWith("PUT", "/api/resource-defaults", { cpus: 0, memory_mb: 0 }));
});

it("warns about workers without resource defaults support", async () => {
  mockApi({ nodes: [node("alpha", true), node("legacy", false)] });
  wrap(<ResourceDefaultsSection target={{ scope: "team" }} canEdit />);
  expect(await screen.findByText(/legacy does not report resource defaults support/)).toBeInTheDocument();
});

it("warns instance administrators only about old workers assigned to the current team", async () => {
  const admin = { ...member, is_admin: true, teams: [] };
  mockApi({ session: admin, nodes: [node("alpha", true), node("other-team-legacy", false)], assigned: ["alpha"] });
  const { unmount } = wrap(<ResourceDefaultsSection target={{ scope: "team" }} canEdit />);
  await screen.findByLabelText(/CPU cores/);
  await waitFor(() => expect(request).toHaveBeenCalledWith("GET", "/api/teams/t1/nodes", undefined, expect.anything()));
  // Let the assigned-node response settle so the absence below is not just a pending query.
  await act(() => new Promise((resolve) => setTimeout(resolve, 50)));
  expect(screen.queryByText(/do(es)? not report resource defaults support/)).not.toBeInTheDocument();
  unmount();

  mockApi({ session: admin, nodes: [node("alpha", true), node("assigned-legacy", false)], assigned: ["alpha", "assigned-legacy"] });
  wrap(<ResourceDefaultsSection target={{ scope: "team" }} canEdit />);
  expect(await screen.findByText(/assigned-legacy does not report resource defaults support/)).toBeInTheDocument();
});

it("is read-only for members who cannot change team defaults", async () => {
  mockApi();
  wrap(<ResourceDefaultsSection target={{ scope: "team" }} canEdit={false} />);
  expect(await screen.findByLabelText(/CPU cores/)).toBeDisabled();
  expect(screen.getByText("Team owners and admins can change these defaults.")).toBeInTheDocument();
  expect(screen.queryByRole("button", { name: "Save changes" })).not.toBeInTheDocument();
});

it("shows a retryable error when defaults cannot be loaded", async () => {
  vi.mocked(request).mockRejectedValue(new Error("resource defaults unavailable"));
  wrap(<ResourceDefaultsSection target={{ scope: "team" }} canEdit />);
  expect(await screen.findByText("Could not load resource defaults")).toBeInTheDocument();
  expect(screen.getByRole("button", { name: "Retry" })).toBeInTheDocument();
});

it("summarises effective defaults on the limits page and falls back when they cannot be read", async () => {
  mockApi({ configured: { cpus: 1, memory_mb: 0 } });
  const { unmount } = wrap(<ResourceDefaultsNote environmentId="env-1" />);
  expect(await screen.findByText(/Current environment and team defaults: 1 core CPU, 512 MiB memory/)).toBeInTheDocument();
  unmount();

  vi.mocked(request).mockRejectedValue(new Error("forbidden"));
  wrap(<ResourceDefaultsNote environmentId="env-1" />);
  expect(screen.getByText(/use the environment and team resource defaults at their next deploy/)).toBeInTheDocument();
  expect(screen.queryByText(/Current/)).not.toBeInTheDocument();
});
